"""Does @referenceScope refuse a write the product makes for itself (spike probe, not a
proof: it prints and asserts nothing).

probe_cross_workspace.py asks what a member of another workspace can no longer do. This
one asks the opposite of the callers that are not a plain member:

  system     the token lambda signs for itself when it sets a workspace up: the caller's
             own claims, with azp "jobli-system" and ws the workspace (workspace.js,
             quota.js). The mutations are the ones workspace.js sends.
  discovery  a token whose ws is "*": a signed-in user who has not picked a workspace
  no token   a request with no token at all

Root namespace of the cluster proof_real.py built, partition namespaces off. Every write
is a dry run, sent under today's schema and under today's schema with the
@referenceScope lines of graph/.graphql.

  SPIKE_SUB=... SPIKE_EMAIL=... python3 probe_reference_scope_flows.py
"""
import hashlib
import hmac
import http.server
import json
import os
import re
import threading
import time

import proof_real as P
import probe_phase1_writes as W
import probe_cross_workspace as X

WSM = W.WSM
OTHER = W.OTHER_WS
SYSTEM = "jobli-system"


def token(ws, sub, email, azp="integration-test"):
    now = int(time.time())
    body = {"aud": [P.AUDIENCE], "azp": azp, "email": email, "exp": now + 24 * 3600, "iat": now,
            "iss": "urn://api.jobli.io", "scope": ["_all"], "sub": sub, "ws": ws}
    signing = P.b64(json.dumps({"alg": "HS256", "typ": "JWT"}).encode()) + "." + P.b64(json.dumps(body).encode())
    return signing + "." + P.b64(hmac.new(P.SECRET.encode(), signing.encode(), hashlib.sha256).digest())


def run(c, tok, query, variables):
    body = json.dumps({"query": query, "variables": variables})
    r = c.post("/graphql", body, headers={P.HEADER: tok}) if tok else c.post("/graphql", body)
    d = r.get("data") or {}
    d = next(iter(d.values())) if d else None
    errs = [e.get("message", "") for e in r.get("errors") or []]
    if errs and not d:
        return ("refused" if "uthoriz" in errs[0] else "error"), errs[0]
    return str((d or {}).get("numUids")), (errs[0] if errs else "")


NEW, NEW_SUB, NEW_EMAIL = "spike-scope-new", "spike-scope-new-owner", "scope-new@spike.invalid"


def new_workspace(c, arm):
    """createWorkspace, as workspace.js carries it out for a workspace that does not exist:
    committed, then removed with every edge to what it made."""
    marker = c.mutate('_:m <dgraph.type> "SpikeMarker" .')["data"]["uids"]["m"]
    system, owner = token(NEW, NEW_SUB, NEW_EMAIL, SYSTEM), token(NEW, NEW_SUB, NEW_EMAIL)
    owner_x = "%s::%s" % (NEW, NEW_EMAIL)
    said = []

    def step(label, tok, query, variables):
        d, e = W.ask(c, tok, query, variables)
        said.append("%s: %s%s" % (label, "ok" if d and not e else "no", (" " + e[0][:150]) if e else ""))
        return d or {}
    try:
        d = step("add the workspace and its owner", system,
                 "mutation($in: [AddWorkspaceInput!]!) { m: addWorkspace(input: $in, upsert: true) { workspace { id ownedBy { id } } } }",
                 {"in": [{"name": NEW, "displayName": "Spike scope new",
                          "ownedBy": {"xId": owner_x, "email": NEW_EMAIL, "userId": NEW_SUB, "status": "ACTIVE"}}]})
        w = (d.get("workspace") or [{}])[0]
        ws, user = w.get("id"), (w.get("ownedBy") or {}).get("id")
        if not ws:
            return said
        step("tie the owner's profile to a user account", system,
             "mutation($in: [AddUserAccountInput!]!) { m: addUserAccount(input: $in, upsert: true) { userAccount { id } } }",
             {"in": [{"hasUserProfile": [{"id": user}]}]})
        # the installer writes the workspace's roles in DQL
        types = W.rows(c, '{ q(func: eq(IAMRole.name, "admin"), first: 1) { t: dgraph.type } }')[0]["t"]
        c.mutate("\n".join(['_:r <dgraph.type> "%s" .' % x for x in types] + [
            '_:r <IAMRole.name> "admin" .', '_:r <IAMRole.permission> "_ALL" .',
            '_:r <IAMRole.xId> "jobli::iamrole::%s::admin" .' % NEW, "_:r <%s> <%s> ." % (WSM, ws)]))
        step("bind the admin role to the owner on the workspace", system,
             "mutation($in: [AddIAMBindingInput!]!) { m: addIAMBinding(input: $in, upsert: true) { numUids } }",
             {"in": [{"xId": "jobli::iambinding::%s::admin-user" % NEW, "forRole": {"xId": "jobli::iamrole::%s::admin" % NEW},
                      "forUserMember": {"xId": owner_x}, "forResource": {"id": ws}}]})
        d = step("the owner adds a group", owner, "mutation($in: [AddGroupInput!]!) { m: addGroup(input: $in) { group { id } } }",
                 {"in": [{"name": "Spike scope group"}]})
        group = (d.get("group") or [{}])[0].get("id")
        if group:
            d = step("the owner adds a candidate into it", owner,
                     "mutation($in: [AddCandidateInput!]!) { m: addCandidate(input: $in) { candidate { id } } }",
                     {"in": [{"firstName": "Spike", "lastName": "Scope", "inGroup": [{"id": group}]}]})
            cand = (d.get("candidate") or [{}])[0].get("id")
            if cand:
                step("the owner tags the candidate", owner, "mutation($in: [AddTagInput!]!) { m: addTag(input: $in) { numUids } }",
                     {"in": [{"name": "spike-scope", "tagable": [{"id": cand}]}]})
        return said
    finally:
        n, edges, left = W.remove_new(c, marker)
        said.append("removed %d nodes and %d edges to them; still there: %s" % (n, edges, left))


def main():
    if not (P.SUB and P.EMAIL):
        raise SystemExit("set SPIKE_SUB and SPIKE_EMAIL to the userId and email of the owner of %s" % P.WS)
    server = http.server.ThreadingHTTPServer(("127.0.0.1", P.LAMBDA_PORT), W.Stub)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    original = open(os.path.join(P.OUT, "schema_today.graphql")).read()
    with P.cluster(P.OFF) as c:
        c.wait_ready(0, timeout=900)
        if P.get_schema(c) != original:
            P.set_schema(c, original)
        rows = lambda q: W.rows(c, q)
        one = lambda q: (rows(q) or [{}])[0]
        while True:
            stale = sorted(W.uids(rows("{ q(func: type(SpikeMarker)) { uid } }")), key=lambda u: int(u, 16))
            if not stale:
                break
            print("an earlier run left its records behind: removed %s" % (W.remove_new(c, stale[0]),))

        def workspace(name):
            w = one('{ q(func: eq(Workspace.name, "%s")) { uid n: Workspace.displayName o: Ownable.ownedBy { uid x: User.xId '
                    "e: User.email s: User.userId } } }" % name)
            o = W.one(w.get("o")) or {}
            role = one('{ q(func: eq(IAMRole.name, "admin")) @filter(uid_in(%s, %s)) { uid x: IAMRole.xId } }' % (WSM, w["uid"]))
            role_ref = {"xId": role["x"]} if role.get("x") else {"id": role.get("uid")}
            bind = one('{ q(func: eq(IAMBinding.xId, "jobli::iambinding::%s::admin-user")) { uid } }' % name)
            cat = one("{ q(func: type(JobCategory), first: 1) @filter(uid_in(%s, %s)) { uid } }" % (WSM, w["uid"]))
            job = one("{ q(func: type(Job), first: 1) @filter(%s) @cascade { uid Groupable.inGroup @filter(uid_in(%s, %s)) { uid } } }"
                      % (W.LIVE, WSM, w["uid"]))
            out = {"name": name, "uid": w["uid"], "display": w.get("n") or name, "owner": o.get("uid"), "owner_x": o.get("x"),
                   "email": o.get("e"), "sub": o.get("s"), "role": role_ref, "has_binding": bool(bind),
                   "category": cat.get("uid"), "job": job.get("uid")}
            print("%s: %s" % (name, json.dumps({k: v for k, v in out.items() if k not in ("email", "sub", "owner_x")})))
            return out

        here, there = workspace(P.WS), workspace(OTHER)
        # A workspace set up since roles were given an xId: its binding names the role as workspace.js does.
        keyed = [r for r in rows("{ q(func: has(IAMRole.xId)) @filter(eq(IAMRole.name, \"admin\")) { x: IAMRole.xId w: %s { n: Workspace.name } } }" % WSM)
                 if (W.one(r.get("w")) or {}).get("n") and r["x"] == "jobli::iamrole::%s::admin" % W.one(r["w"])["n"]]
        keyed = [r for r in keyed if W.one(r["w"])["n"] not in (P.WS, OTHER)]
        third = workspace(W.one(keyed[0]["w"])["n"]) if keyed else None
        manifest = one('{ q(func: type(IntegrationAppManifest), first: 1) @filter(eq(IntegrationAppManifest.status, "PUBLISHED")) { uid } }').get("uid")
        t = W.pick_targets(c, here["uid"])
        adrift = one("{ q(func: type(Company), first: 1) @filter(NOT has(Groupable.inGroup) AND NOT has(%s) AND %s) { uid } }" % (WSM, W.LIVE)).get("uid")
        print("a published manifest: %s; a company no path places in a workspace: %s" % (manifest, adrift))

        count = ("{ w(func: type(Workspace)) { count(uid) } a(func: type(UserAccount)) { count(uid) } p(func: type(Plugin)) { count(uid) } "
                 "b(func: type(IAMBinding)) { count(uid) } t(func: type(Tag)) { count(uid) } u(func: type(User)) { count(uid) } "
                 "o(func: uid(%s, %s)) { n: Workspace.displayName c: count(Workspace.hasIAMRoleBinding) } }" % (here["uid"], there["uid"]))
        before = c.dql(count)["data"]

        toks = {
            "system": token(here["name"], here["sub"] or P.SUB, here["email"] or P.EMAIL, SYSTEM),
            "system/other": token(there["name"], there["sub"], there["email"], SYSTEM),
            "system/third": token(third["name"], third["sub"], third["email"], SYSTEM) if third else None,
            "owner": P.token(),
            "discovery": token("*", P.SUB, P.EMAIL),
            "no token": None,
        }
        addw = "mutation($in: [AddWorkspaceInput!]!) @dryRun { m: addWorkspace(input: $in, upsert: true) { numUids } }"
        acct = "mutation($in: [AddUserAccountInput!]!) @dryRun { m: addUserAccount(input: $in, upsert: true) { numUids } }"
        plug = "mutation($in: [AddPluginInput!]!) @dryRun { m: addPlugin(input: $in) { numUids } }"
        bind = "mutation($in: [AddIAMBindingInput!]!) @dryRun { m: addIAMBinding(input: $in, upsert: true) { numUids } }"
        upda = "mutation($in: UpdateJobInput!) @dryRun { m: updateJob(input: $in) { numUids } }"
        tag = "mutation($in: [AddTagInput!]!) @dryRun { m: addTag(input: $in) { numUids } }"
        cand = "mutation($in: [AddCandidateInput!]!) @dryRun { m: addCandidate(input: $in) { numUids } }"
        g = lambda *ids: [{"id": i} for i in ids]

        def setup(w):
            """What workspace.js sends when a workspace is added, for a workspace that is already there."""
            return [
                ("add the workspace again, naming its owner by xId", addw, {"in": [{
                    "name": w["name"], "displayName": w["display"],
                    "ownedBy": {"xId": w["owner_x"], "email": w["email"], "userId": w["sub"], "status": "ACTIVE"}}]}),
                ("tie the owner's profile to a user account", acct, {"in": [{"hasUserProfile": g(w["owner"])}]}),
                ("install a plugin from a published manifest", plug, {"in": [{"installedManifest": {"id": manifest}}]}),
                ("bind the admin role to the owner on the workspace", bind, {"in": [{
                    "xId": "jobli::iambinding::%s::admin-user" % w["name"], "forRole": w["role"],
                    "forUserMember": {"xId": w["owner_x"]}, "forResource": {"id": w["uid"]}}]}),
            ]

        cases = [
            ("the product's own writes: should not change", (
                [("system", "%s: %s" % (here["name"], l), q, v) for l, q, v in setup(here)] +
                [("system/other", "%s: %s" % (there["name"], l), q, v) for l, q, v in setup(there)] +
                [("system/third", "%s: %s" % (third["name"], l), q, v) for l, q, v in (setup(third) if third else [])] + [
                    ("system", "class a job under a category of its workspace", upda,
                     {"in": {"filter": {"id": [here["job"]]}, "set": {"class": {"id": here["category"]}}}}),
                    ("owner", "class a job under a category of its workspace", upda,
                     {"in": {"filter": {"id": [here["job"]]}, "set": {"class": {"id": here["category"]}}}}),
                ])),
            ("across workspaces, or with no workspace: should be refused", [
                ("system/other", "bind own admin role to own owner, on the workspace %s" % here["name"], bind, {"in": [{
                    "xId": "jobli::iambinding::spike-scope::cross", "forRole": there["role"],
                    "forUserMember": {"xId": there["owner_x"]}, "forResource": {"id": here["uid"]}}]}),
                ("system/other", "bind the admin role of %s to own owner" % here["name"], bind, {"in": [{
                    "xId": "jobli::iambinding::spike-scope::cross", "forRole": here["role"],
                    "forUserMember": {"xId": there["owner_x"]}, "forResource": {"id": there["uid"]}}]}),
                ("system/other", "tie the owner of %s to a user account" % here["name"], acct, {"in": [{"hasUserProfile": g(here["owner"])}]}),
                ("system/other", "class own job under a category of %s" % here["name"], upda,
                 {"in": {"filter": {"id": [there["job"]]}, "set": {"class": {"id": here["category"]}}}}),
                ("discovery", "add a candidate into a group of %s" % here["name"], cand,
                 {"in": [{"firstName": "Spike", "lastName": "Scope", "inGroup": g(t["G"])}]}),
                ("discovery", "add a tag on a candidate of %s" % here["name"], tag, {"in": [{"name": "spike-scope", "tagable": g(t["N"])}]}),
                ("discovery", "class a job of %s under its own category" % here["name"], upda,
                 {"in": {"filter": {"id": [here["job"]]}, "set": {"class": {"id": here["category"]}}}}),
                ("no token", "class a job of %s under its own category" % here["name"], upda,
                 {"in": {"filter": {"id": [here["job"]]}, "set": {"class": {"id": here["category"]}}}}),
                ("no token", "add a tag on a candidate of %s" % here["name"], tag, {"in": [{"name": "spike-scope", "tagable": g(t["N"])}]}),
            ]),
            ("a record no path places in a workspace", [
                ("owner", "add a tag on a company with no group and no workspace", tag,
                 {"in": [{"name": "spike-scope", "tagable": g(adrift)}]} if adrift else None),
                ("system", "add a tag on a company with no group and no workspace", tag,
                 {"in": [{"name": "spike-scope", "tagable": g(adrift)}]} if adrift else None),
            ]),
        ]
        schemas = [(n, s) for n, s in X.arms(original) if n in ("today", "scope")]
        results, messages, flows = {}, {}, {}
        try:
            for name, schema in schemas:
                r = P.set_schema(c, schema)
                if r.get("errors"):
                    print("\n%s: the schema was refused: %s" % (name, json.dumps(r["errors"])[:600]))
                    continue
                flows[name] = new_workspace(c, name)
                for _, group in cases:
                    for who, label, q, v in group:
                        if v is None:
                            continue
                        got, m = run(c, toks[who], q, v)
                        results[(name, who, label)] = got
                        if m:
                            m = re.sub(r"0x[0-9a-f]+", "<uid>", m).replace(P.EMAIL, "<email>")
                            messages.setdefault(m[:300], []).append("%s / %s: %s" % (name, who, label))
            names = [n for n, _ in schemas if any(k[0] == n for k in results)]
            for title, group in cases:
                print("\n%s (numUids of a dry run)" % title)
                print("  %-13s %-62s %s" % ("", "", " ".join("%-8s" % n for n in names)))
                for who, label, _, v in group:
                    if v is not None:
                        print("  %-13s %-62s %s" % (who, label[:62], " ".join("%-8s" % results.get((n, who, label), "-") for n in names)))
            print("\na workspace that is new, set up as workspace.js does it and then used by its owner (committed, then removed)")
            for name in names:
                for line in flows.get(name) or []:
                    print("  %-6s %s" % (name, line))
            print()
            for m, where in messages.items():
                print("  message: %s\n      on: %s%s" % (m, "; ".join(where[:3]), " and %d more" % (len(where) - 3) if len(where) > 3 else ""))
        finally:
            if P.get_schema(c) != original:
                P.set_schema(c, original)
            after = c.dql(count)["data"]
            print("\nnothing was written: %s" % (json.dumps(before, sort_keys=True) == json.dumps(after, sort_keys=True)))
            if before != after:
                print("  before %s\n  after  %s" % (json.dumps(before, sort_keys=True), json.dumps(after, sort_keys=True)))


if __name__ == "__main__":
    main()
