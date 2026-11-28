"""Jobli's app catalogue in the control plane, on the proof cluster, through Jobli's
real lambda (spike probe, not a proof: it prints and asserts nothing).

The schema is the one the other probes run on, with the catalogue placed
(catalogue.place): IntegrationApp and IntegrationAppManifest in the root namespace,
their reverse lists gone, an app owned by name. The data is migrated first
(catalogue.migrate_root, catalogue.strip_partition), which is one way: the workspace's
partition no longer holds a copy of any app or manifest.

As the owner of the workspace, in its partition, with every spike flag on:

  - read the catalogue: the marketplace, the workspace's own apps, its plugins with the
    app, the manifest and the icon under each
  - records by the app that made them (a filter through fromSource)
  - add a note: which app made it, and whether anything of the catalogue was made
  - make an app from the workspace, change it, change somebody else's
  - what hand-written DQL sees of the catalogue from inside the partition

and the same reads as a member with a workspace role that is not _ALL, if the cluster
has one. What it adds is removed afterwards and the schema is put back.

  SPIKE_SUB=... SPIKE_EMAIL=... python3 probe_catalogue.py
"""
import json
import os
import time

import catalogue as K
import proof_real as P
import probe_phase1_writes as W
import probe_cross_workspace as X
import probe_subscriptions as S
import probe_target_lambda as T
import spike_lambda as L

MARKET = """query {
  data: queryIntegrationAppManifest(filter: { isLatest: true }) {
    id version status tags
    hasIcon { id fileName }
    forApp { id clientId displayName ownerWorkspace hasPhoto { id fileName ownerWorkspace } }
  }
}"""
PLUGINS = """query($id: ID) {
  data: getWorkspace(id: $id) {
    hasPlugin {
      clientId
      installedManifest { version status forApp { clientId } hasIcon { id } }
      hasApp { clientId displayName ownerWorkspace }
    }
  }
}"""


def short(r, n=260):
    if r.get("errors"):
        return "error: %s" % r["errors"][0].get("message", "")[:n]
    d = r.get("data") or {}
    return json.dumps(d.get("data", d))[:n]


def rows(r):
    d = (r.get("data") or {})
    d = d.get("data", d)
    return d if isinstance(d, list) else next(iter(d.values()), []) if isinstance(d, dict) else []


def dql(c, q, ns=None, tok=None):
    r = c.post("/query", q, ns=ns, ctype="application/dql", headers={P.HEADER: tok} if tok else None)
    if r.get("errors"):
        return "error: %s" % r["errors"][0].get("message", "")[:160]
    return r.get("data") or {}


def main():
    if not (P.SUB and P.EMAIL):
        raise SystemExit("set SPIKE_SUB and SPIKE_EMAIL to the userId and email of the owner of %s" % P.WS)
    original = open(os.path.join(P.OUT, "schema_today.graphql")).read()
    scope = dict(X.arms(original))["scope"]
    placed = K.place(scope)
    bundle = L.build()
    c = P.cluster(P.HEADER_MODE)
    tap = L.Tap(c.alpha_http(0)).start()
    L.start(bundle, True)
    marker = None
    try:
        c.start()
        c.wait_ready(0, timeout=900)
        print("data, the partition:  %s" % json.dumps(K.strip_partition(c, 1)))
        if P.get_schema(c) != placed:
            r = P.set_schema(c, placed)
            if r.get("errors"):
                raise SystemExit("the placed schema was refused: %s" % json.dumps(r["errors"])[:600])
        print("the schema with the catalogue placed is loaded")
        print("data, root namespace: %s" % json.dumps(K.migrate_root(c)))
        marker = c.mutate('_:m <dgraph.type> "SpikeMarker" .', ns=0)["data"]["uids"]["m"]
        c.flags = T.TARGET
        P.restart(c)
        tok = P.token()
        end = time.time() + 180
        while time.time() < end and not P.gql(c, tok, "{ aggregateCandidate { count } }", {}).get("data"):
            time.sleep(0.5)
        root = lambda q: dql(c, q)          # no token: the root namespace
        mine = lambda q: dql(c, q, tok=tok)  # the caller's token: the partition

        want = root('{ m(func: type(IntegrationAppManifest)) @filter(eq(IntegrationAppManifest.isLatest, true) AND '
                    'eq(IntegrationAppManifest.status, "PUBLISHED")) { count(uid) } '
                    'own(func: eq(IntegrationApp.ownerWorkspace, "%s")) { count(uid) } }' % P.WS)
        print("\nin the root namespace: %d manifests are latest and published; %d apps are owned by %s" % (
            want["m"][0]["count"], want["own"][0]["count"], P.WS))

        print("\nas the owner of %s, in its partition:" % P.WS)
        t0 = time.perf_counter()
        r = P.gql(c, tok, MARKET, {})
        took = (time.perf_counter() - t0) * 1000
        m = rows(r)
        print("  the marketplace:            %s" % (short(r) if r.get("errors") else
              "%d manifests in %.0f ms; %d with an icon; apps owned by %s; %d apps with a photo" % (
                  len(m), took, sum(1 for x in m if x.get("hasIcon")),
                  sorted({(x.get("forApp") or {}).get("ownerWorkspace") or "(none)" for x in m}),
                  sum(1 for x in m if (x.get("forApp") or {}).get("hasPhoto")))))
        # a plugin is shown to its own app: the system app is the caller here, as lambda's system calls are
        ws = mine('{ q(func: eq(Workspace.name, "%s")) { uid } }' % P.WS)["q"][0]["uid"]
        own = mine("{ q(func: type(Plugin)) @filter(has(Plugin.hasApp)) { count(uid) } }")["q"][0]["count"]
        r = P.gql(c, P.token(azp="jobli-system"), PLUGINS, {"id": ws})
        p = ((r.get("data") or {}).get("data") or {}).get("hasPlugin") or []
        print("  its plugins, as the system app: %s" % (short(r) if r.get("errors") else
              "%d of the %d that have an app; with the app read: %d; with the manifest: %d; whose manifest names the same app: %d" % (
                  len(p), own, sum(1 for x in p if x.get("hasApp")), sum(1 for x in p if x.get("installedManifest")),
                  sum(1 for x in p if x.get("installedManifest") and
                      (x["installedManifest"].get("forApp") or {}).get("clientId") == x.get("clientId")))))
        exp = root('{ var(func: eq(IntegrationApp.ownerWorkspace, "%s")) { own as IntegrationApp.hasManifest } '
                   'q(func: type(IntegrationAppManifest)) @filter(eq(IntegrationAppManifest.status, "PUBLISHED") OR uid(own)) { count(uid) } '
                   'mine(func: uid(own)) @filter(NOT eq(IntegrationAppManifest.status, "PUBLISHED")) { count(uid) } }' % P.WS)
        r = P.gql(c, tok, "{ data: queryIntegrationAppManifest { id status } }", {})
        print("  every manifest it may read: %s (published, or of an app it owns: %d; %d of its own are not published)" % (
            short(r) if r.get("errors") else len(rows(r)), exp["q"][0]["count"], exp["mine"][0]["count"]))
        r = P.gql(c, tok, "{ data: queryIntegrationApp { clientId displayName ownerWorkspace } }", {})
        print("  the apps it may maintain:   %s" % (short(r) if r.get("errors") else
              "%d, owned by %s" % (len(rows(r)), sorted({x.get("ownerWorkspace") for x in rows(r)}))))
        r = P.gql(c, tok, '{ data: getIntegrationApp(clientId: "jobli-system") { clientId displayName } }', {})
        print("  an app of another workspace, asked for directly: %s" % short(r))
        r = P.gql(c, P.token(azp="jobli-system"), 'query($id: ID) { data: getWorkspace(id: $id) { hasPlugin(filter: {clientId: {eq: "jobli-system"}}) '
                  "{ hasApp { clientId displayName } } } }", {"id": ws})
        print("  the same app, under the workspace's plugin of it: %s" % short(r))

        # records by the app that made them
        sysapp = root('{ q(func: eq(IntegrationApp.clientId, "jobli-system")) { uid } }')["q"][0]["uid"]
        n = mine("{ q(func: type(NoteType)) @filter(uid_in(Recordable.fromSource, %s)) { count(uid) } "
                 "all(func: type(NoteType)) { count(uid) } }" % sysapp)
        r = P.gql(c, tok, '{ data: queryNoteType(filter: {fromSource: {clientId: {eq: "jobli-system"}}}) { name } }', {})
        print("  note types made by jobli-system: %s (by its edges: %d of %d)" % (
            short(r) if r.get("errors") else len(rows(r)), n["q"][0]["count"], n["all"][0]["count"]))
        r = P.gql(c, tok, 'query($id: ID) { data: getWorkspace(id: $id) { hasNoteType(filter: {name: {regexp: "/^Generic Note$/"}, '
                          'fromSource: {clientId: {eq: "jobli-system"}}}) { id name } } }', {"id": ws})
        print("  the web app's own filter (the workspace's generic note type): %s" % short(r))

        # a write: the note names the app that made it, and nothing of the catalogue is made
        before = (mine("{ q(func: has(IntegrationApp.clientId)) { count(uid) } }")["q"][0]["count"],
                  root("{ q(func: type(IntegrationApp)) { count(uid) } }")["q"][0]["count"])
        t = W.pick_targets(c, S.nsrows(c, '{ q(func: eq(Workspace.name, "%s")) { uid } }' % P.WS, None)[0]["uid"])
        types = [x["uid"] for x in mine("{ q(func: type(NoteType), first: 1) @filter(uid_in(%s, %s)) { uid } }" % (W.WSM, ws))["q"]]
        tap.quiet(0.8, 20)
        calls0 = len(tap.calls)
        r = P.gql(c, tok, P.app_query("api/note.ts", "ADD_NOTE"),
                  {"input": [{"text": "spike catalogue", "forResource": [{"id": t["N"]}], "hasType": {"id": types[0]}}]})
        tap.quiet()
        note = (rows(r) or [{}])[0].get("id") if not r.get("errors") else None
        print("  add a note:                 %s" % (short(r, 200) if r.get("errors") else "ok, %s" % note))
        if note:
            r = P.gql(c, tok, "query($id: ID!) { data: getNote(id: $id) { text fromSource { clientId displayName } } }", {"id": note})
            print("    the app that made it:     %s" % short(r))
        after = (mine("{ q(func: has(IntegrationApp.clientId)) { count(uid) } }")["q"][0]["count"],
                 root("{ q(func: type(IntegrationApp)) { count(uid) } }")["q"][0]["count"])
        print("    app records, partition / root: before %s, after %s" % (before, after))
        asked = tap.calls[calls0:]
        print("    lambda asked the alpha %d times, %d with no caller's token, %d refused or failed" % (
            len(asked), sum(1 for x in asked if not x["token"]), sum(1 for x in asked if T.failed(x))))

        # maintaining apps
        r = P.gql(c, tok, 'mutation { data: addIntegrationApp(input: [{displayName: "Spike catalogue app", description: "spike"}]) '
                          "{ integrationApp { id clientId ownerWorkspace } } }", {})
        made = ((r.get("data") or {}).get("data") or {}).get("integrationApp") or [{}]
        print("  make an app from the workspace: %s" % short(r, 300))
        if made[0].get("id"):
            where = (root('{ q(func: uid(%s)) { c: IntegrationApp.clientId o: IntegrationApp.ownerWorkspace } }' % made[0]["id"])["q"],
                     mine('{ q(func: uid(%s)) { c: IntegrationApp.clientId } }' % made[0]["id"])["q"])
            print("    in the root namespace %s; in the partition %s" % where)
            r = P.gql(c, tok, 'mutation($id: [ID!]) { data: updateIntegrationApp(input: {filter: {id: $id}, set: {description: "changed"}}) '
                              "{ numUids } }", {"id": [made[0]["id"]]})
            print("    change it:                %s" % short(r))
        # photos of control records: who may change the record may change its photo
        photo = 'mutation($in: [AddGlobalPhotoInput!]!) { data: addGlobalPhoto(input: $in) { globalPhoto { id ownerWorkspace forResource { id } } } }'
        if made[0].get("id"):
            r = P.gql(c, tok, photo, {"in": [{"forResource": {"id": made[0]["id"]}}]})
            print("  add a photo of the new app:  %s" % short(r))
            r = P.gql(c, tok, "query($id: ID!) { data: getIntegrationApp(id: $id) { clientId hasPhoto { id ownerWorkspace } } }", {"id": made[0]["id"]})
            print("    the app with its photo:    %s" % short(r))
        r = P.gql(c, tok, photo, {"in": [{"forResource": {"id": sysapp}}]})
        print("  add a photo of an app of another workspace: %s" % short(r))
        r = P.gql(c, tok, photo, {"in": [{}]})
        print("  add a photo that is of no record (an icon): %s" % short(r))
        r = P.gql(c, tok, 'mutation { data: updateIntegrationApp(input: {filter: {clientId: {eq: "jobli-system"}}, '
                          'set: {description: "changed from %s"}}) { numUids } }' % P.WS, {})
        print("  change an app of another workspace: %s" % short(r))

        # a release of the new app, and installing it: lambda's manifest hooks read the app in
        # the catalogue, and its installer writes the plugin in the partition with its edges to
        # the app and the manifest, which it has to find in the catalogue
        if made[0].get("id"):
            print("\na release of the new app, and installing it, as the web app does:")
            # a release that was accepted before, of an app a developer made, under the new app's ID
            donors = [m for m in root('{ q(func: type(IntegrationAppManifest)) @filter(eq(IntegrationAppManifest.status, "PUBLISHED", "PRIVATE_PUBLISHED")) '
                                      "{ t: IntegrationAppManifest.manifest a: IntegrationAppManifest.forApp { c: IntegrationApp.clientId } } }")["q"]
                      if m.get("t") and m.get("a")]
            ident = lambda m: (m["a"][0] if isinstance(m["a"], list) else m["a"])["c"]
            donor = sorted(donors, key=lambda m: (not ident(m).startswith("app_"), len(m["t"])))[0]
            donor_id = ident(donor)
            release = "1.0.0"
            try:
                doc = json.loads(donor["t"])
                doc["appId"] = made[0]["clientId"]
                release = doc.get("version") or release
                text = json.dumps(doc)
            except ValueError:
                text = donor["t"].replace(donor_id, made[0]["clientId"])
            calls0, log0 = len(tap.calls), len(L.logs())
            r = P.gql(c, tok, "mutation($in: [AddIntegrationAppManifestInput!]!) { data: addIntegrationAppManifest(input: $in) "
                              "{ integrationAppManifest { id sId status manifestDigest tags forApp { clientId } } } }",
                      {"in": [{"forApp": {"id": made[0]["id"]}, "version": release, "manifest": text}]})
            man = (((r.get("data") or {}).get("data") or {}).get("integrationAppManifest") or [{}])[0]
            print("  add a release (a copy of %s's manifest, %d bytes): %s" % (donor_id, len(text), short(r, 260)))
            if man.get("id"):
                r = P.gql(c, tok, "mutation($id: [ID!]) { data: updateIntegrationAppManifest(input: {filter: {id: $id}, set: {status: PRIVATE_PUBLISHED}}) "
                                  "{ integrationAppManifest { status publishedAt isLatest } } }", {"id": [man["id"]]})
                print("  publish it privately:   %s" % short(r))
                roots0 = root("{ q(func: has(dgraph.type)) { count(uid) } }")["q"][0]["count"]
                tap.quiet(0.8, 20)
                t0 = time.perf_counter()
                r = P.gql(c, tok, P.app_query("api/plugin.ts", "ADD_PLUGIN"), {"input": [{"installedManifest": {"id": man["id"]}, "deleted": False}]})
                took = (time.perf_counter() - t0) * 1000
                tap.quiet()
                print("  install it:             %s in %.0f ms" % (short(r, 220) if r.get("errors") else "numUids %s" % (
                    ((r.get("data") or {}).get("data") or {}).get("numUids")), took))
                got = mine('{ q(func: type(Plugin)) @filter(eq(Plugin.clientId, "%s")) { uid s: Plugin.lifecycleState w: count(WorkspaceMember.inWorkspace) '
                           "a: Plugin.hasApp { uid } m: Plugin.installedManifest { uid } b: count(Plugin.hasIAMBinding) r: count(Plugin.managesResource) } }"
                           % made[0]["clientId"])["q"]
                one = lambda v: (v[0] if isinstance(v, list) else v) if v else {}
                for g in got:
                    print("    in the partition: state %s; its app edge is the catalogue's app: %s; its manifest edge is the release: %s; "
                          "%s bindings, %s resources it manages" % (g.get("s"), one(g.get("a")).get("uid") == made[0]["id"],
                                                                    one(g.get("m")).get("uid") == man["id"], g.get("b", 0), g.get("r", 0)))
                if not got:
                    print("    no plugin of the new app is in the partition")
                rr = P.gql(c, P.token(azp=made[0]["clientId"]), 'query($id: ID) { data: getWorkspace(id: $id) { hasPlugin(filter: {clientId: {eq: "%s"}}) '
                           "{ lifecycleState hasApp { clientId displayName } installedManifest { version status forApp { clientId } } } } }"
                           % made[0]["clientId"], {"id": ws})
                print("    read back as that app:  %s" % short(rr, 260))
                print("    records in the root namespace: %d before the install, %d after" % (
                    roots0, root("{ q(func: has(dgraph.type)) { count(uid) } }")["q"][0]["count"]))
            asked = tap.calls[calls0:]
            print("  for the three steps lambda asked the alpha %d times: %d as the caller, %d of the catalogue (no caller's token), %d refused or failed" % (
                len(asked), sum(1 for x in asked if x["token"]), sum(1 for x in asked if not x["token"]), sum(1 for x in asked if T.failed(x))))
            for l in [l for l in L.logs()[log0:].splitlines() if ('"severity":"ERROR"' in l or "[ERROR]" in l) and "GEMINI_API_KEY" not in l][:5]:
                print("    lambda logged: %s" % l[:260])

        # the account: the person's, in the control plane
        print("\nthe caller's account, from inside the partition:")
        acct = root('{ q(func: eq(UserAccount.userId, "%s")) { uid f: UserAccount.firstName t: count(UserAccount.hasDeviceToken) '
                    'p: UserAccount.hasNotificationPreference { uid e: NotificationPreference.emailEnabled c: count(NotificationPreference.categorySettings) } } '
                    'n(func: type(UserAccount)) { count(uid) } }' % P.SUB)
        accounts, a = acct["n"][0]["count"], (acct["q"] or [{}])[0]
        pref = a.get("p") or {}
        pref = pref[0] if isinstance(pref, list) else pref
        r = P.gql(c, tok, P.app_query("api/notification.ts", "QUERY_MY_NOTIFICATION_ACCOUNT"), {"userId": P.SUB})
        got = (rows(r) or [{}])[0] if not r.get("errors") else {}
        print("  the web app's account query: %s" % (short(r) if r.get("errors") else
              "%d account; its preference %s; category rows %d (root: %d); device tokens %d (root: %d)" % (
                  len(rows(r)), "read" if got.get("hasNotificationPreference") else "MISSING",
                  len((got.get("hasNotificationPreference") or {}).get("categorySettings") or []), pref.get("c", 0),
                  len(got.get("hasDeviceToken") or []), a.get("t", 0))))
        r = P.gql(c, tok, "{ data: queryUserAccount { userId } }", {})
        print("  every account it may read:   %s of %d" % (short(r) if r.get("errors") else len(rows(r)), accounts))
        r = P.gql(c, tok, 'query($u: String) { data: queryUser(filter: {userId: {eq: $u}}) { userId userAccount { userId displayName } } }', {"u": P.SUB})
        me = rows(r)
        print("  its user profile in this workspace, with the account under it: %s" % (short(r) if r.get("errors") else
              "%d record(s) of the caller in the partition; account read under %d, the caller's own in each: %s" % (
                  len(me), sum(1 for x in me if x.get("userAccount")),
                  all(x["userAccount"].get("userId") == P.SUB for x in me if x.get("userAccount")))))
        if a.get("uid"):
            was = a.get("f")
            r = P.gql(c, tok, 'mutation($u: String) { data: updateUserAccount(input: {filter: {userId: {eq: $u}}, set: {firstName: "Spike"}}) { numUids } }', {"u": P.SUB})
            print("  change its own account:      %s" % short(r))
            back = ('set: {firstName: "%s"}' % was) if was else 'remove: {firstName: "Spike"}'
            P.gql(c, tok, "mutation($u: String) { data: updateUserAccount(input: {filter: {userId: {eq: $u}}, %s}) { numUids } }" % back, {"u": P.SUB})
            if pref.get("uid"):
                flip = lambda v: P.gql(c, tok, P.app_query("api/notification.ts", "UPDATE_NOTIFICATION_PREFERENCE"),
                                       {"input": {"filter": {"id": [pref["uid"]]}, "set": {"emailEnabled": v}}})
                r = flip(not pref.get("e", True))
                print("  the web app's preference update: %s" % short(r, 120))
                flip(pref.get("e", True))
            r = P.gql(c, tok, "mutation { data: addUserAccount(input: [{}], upsert: true) { userAccount { id } } }", {})
            again = (((r.get("data") or {}).get("data") or {}).get("userAccount") or [{}])[0].get("id")
            print("  find-or-make the account, as lambda does when it links a profile: %s; accounts in the root namespace: %d" % (
                short(r) if r.get("errors") else ("the same account" if again == a["uid"] else "ANOTHER: %s" % again),
                root("{ n(func: type(UserAccount)) { count(uid) } }")["n"][0]["count"]))
            r = P.gql(c, tok, photo, {"in": [{"forResource": {"id": a["uid"]}}]})
            print("  add a photo of its own account: %s" % short(r))
            someone = root('{ q(func: type(UserAccount), first: 1) @filter(NOT eq(UserAccount.userId, "%s") AND NOT has(GlobalPhotoOwner.hasPhoto)) '
                           "{ uid u: UserAccount.userId } }" % P.SUB)["q"]
            if someone:
                r = P.gql(c, tok, photo, {"in": [{"forResource": {"id": someone[0]["uid"]}}]})
                print("  add a photo of another person's account: %s" % short(r))
                r = P.gql(c, tok, 'mutation($u: String) { data: updateUserAccount(input: {filter: {userId: {eq: $u}}, set: {firstName: "Taken"}}) { numUids } }',
                          {"u": someone[0]["u"]})
                print("  change another person's account: %s" % short(r))
        d = mine("{ a(func: type(UserAccount)) { count(uid) } u(func: has(User.userAccount)) { count(uid) } }")
        print("  account records in the partition: %d; users there that name an account: %d" % (d["a"][0]["count"], d["u"][0]["count"]))

        # hand-written DQL from inside the partition
        d = mine('{ apps(func: type(IntegrationApp)) { count(uid) } byid(func: eq(IntegrationApp.clientId, "jobli-system")) { uid } '
                 "p(func: has(Plugin.hasApp), first: 1) { Plugin.clientId Plugin.hasApp { IntegrationApp.clientId } "
                 "Plugin.installedManifest { IntegrationAppManifest.version } } }")
        print("\nhand-written DQL with the caller's token, in the partition:")
        print("  apps by type: %s; the app jobli-system by its client ID: %s" % (d["apps"][0]["count"], d["byid"]))
        print("  a plugin with its app and manifest: %s" % json.dumps(d["p"]))

        reader = P.token(sub=P.CALLERS["reader"], email="reader@spike.invalid")
        if P.gql(c, reader, "{ aggregateCandidate { count } }", {}).get("data"):
            print("\nas a member with a workspace role that is not _ALL:")
            r = P.gql(c, reader, MARKET, {})
            print("  the marketplace:           %s" % (short(r) if r.get("errors") else "%d manifests" % len(rows(r))))
            r = P.gql(c, reader, "{ data: queryIntegrationApp { clientId } }", {})
            print("  the apps it may maintain:  %s" % (short(r) if r.get("errors") else len(rows(r))))
            if made[0].get("id"):
                r = P.gql(c, reader, 'mutation($id: [ID!]) { data: updateIntegrationApp(input: {filter: {id: $id}, set: {description: "reader"}}) '
                                     "{ numUids } }", {"id": [made[0]["id"]]})
                print("  change the workspace's new app: %s" % short(r))
            bare = root('{ q(func: eq(IntegrationApp.ownerWorkspace, "%s"), first: 1) @filter(NOT has(GlobalPhotoOwner.hasPhoto)) { uid } }' % P.WS)["q"]
            if bare:
                r = P.gql(c, reader, photo, {"in": [{"forResource": {"id": bare[0]["uid"]}}]})
                print("  add a photo of an app of the workspace that has none: %s" % short(r))
            r = P.gql(c, reader, photo, {"in": [{}]})
            print("  add a photo that is of no record: %s" % short(r))
        logged = [l for l in L.logs().splitlines() if ('"severity":"ERROR"' in l or "[ERROR]" in l) and "GEMINI_API_KEY" not in l]
        print("\nlambda logged %d errors%s" % (len(logged), "".join("\n  " + l[:260] for l in logged[:8])))
    finally:
        L.stop()
        if c.procs:
            c.flags = P.HEADER_MODE
            P.restart(c)
            if marker:
                for n in (0, 1):
                    print("removed %d nodes and %d edges to them in %s" % (
                        S.remove_new(c, marker, n) + ("the partition" if n else "the root namespace",)))
            L.start(bundle, False)
            try:
                r = P.set_schema(c, original)
                print("the schema is put back%s" % (": REFUSED %s" % json.dumps(r["errors"])[:300] if r.get("errors") else ""))
            finally:
                L.stop()
                c.stop()


if __name__ == "__main__":
    main()
