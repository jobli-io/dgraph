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
        r = P.gql(c, tok, 'mutation { data: updateIntegrationApp(input: {filter: {clientId: {eq: "jobli-system"}}, '
                          'set: {description: "changed from %s"}}) { numUids } }' % P.WS, {})
        print("  change an app of another workspace: %s" % short(r))

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
