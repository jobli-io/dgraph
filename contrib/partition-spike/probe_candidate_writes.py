"""Who can read and write a Candidate today (spike probe, not a proof: it prints and
asserts nothing).

Runs in the partition proof_real.py built, with the rules as written. Adds four test
users (a role holding _CANDIDATE on the Workspace and on one group, the editor role
and the viewer role on that group), sends update, delete and add as each with @dryRun
so nothing is committed, and removes the users again.

  SPIKE_SUB=... SPIKE_EMAIL=... python3 probe_candidate_writes.py
"""
import json
import proof_real as P

ROLE = "spike-candidate-admin"
WHO = {"gadmin": "spike-group-admin", "gviewer": "spike-group-viewer", "wadmin": "spike-workspace-admin",
       "geditor": "spike-group-editor"}


def uids(v):
    return [x["uid"] for x in (v if isinstance(v, list) else [v] if v else [])]


def cleanup(c):
    P.CALLERS = dict(WHO)
    n = P.remove_callers(c)
    rdf = []
    for r in c.rows('{ q(func: eq(IAMRole.name, "%s")) { uid w: WorkspaceMember.inWorkspace { uid } } }' % ROLE, ns=1):
        rdf += ["<%s> <Workspace.hasIAMRole> <%s> ." % (w, r["uid"]) for w in uids(r.get("w"))]
        rdf.append("<%s> * * ." % r["uid"])
    if rdf:
        c.mutate("\n".join(rdf), ns=1, delete=True)
    return n


def main():
    P.start_lambda_stub()
    params = P.load("params")
    ws = params["ws_id"]
    original = open(P.os.path.join(P.OUT, "schema_today.graphql")).read()
    with P.cluster(P.HEADER_MODE) as c:
        c.wait_ready(0, timeout=900)
        try:
            cleanup(c)
            roles = c.rows("{ q(func: type(IAMRole)) { uid n: IAMRole.name p: IAMRole.permission t: dgraph.type w: WorkspaceMember.inWorkspace { uid } } }", ns=1)
            print("roles:", sorted((r.get("n"), sorted(r.get("p") or [])) for r in roles))
            viewer = [r for r in roles if r.get("p") == ["READ"]][0]
            editor = [r for r in roles if r.get("n") == "editor" and ws in uids(r.get("w"))][0]
            print("READ role picked:", viewer.get("n"), uids(viewer.get("w")), " editor:", editor["uid"], sorted(editor["p"]))
            groups = [g for g in c.rows("{ q(func: type(Group)) @filter(NOT eq(Recordable.deleted, true)) { uid n: Group.name "
                                        "jobs: count(Group.hasJob) cands: count(Group.hasCandidate) "
                                        "apps: count(Group.hasApplication) } }", ns=1)]
            print("groups:", [(g["uid"], g.get("n"), g.get("jobs"), g.get("cands"), g.get("apps")) for g in groups])
            G = min([g for g in groups if g.get("jobs")], key=lambda g: g["jobs"])
            H = max([g for g in groups if g["uid"] != G["uid"]], key=lambda g: g.get("cands") or 0)
            print("G =", G["uid"], G.get("n"), " H =", H["uid"], H.get("n"))

            live = "@filter(NOT eq(Recordable.deleted, true) AND has(WorkspaceMember.inWorkspace))"
            shape = "{ uid g: Groupable.inGroup { uid } a: Candidate.hasApplication { uid g: Groupable.inGroup { uid } } }"
            in_g = c.rows("{ q(func: uid(%s)) { c: Group.hasCandidate (first: 500) %s %s } }" % (G["uid"], live, shape), ns=1)
            in_h = c.rows("{ q(func: uid(%s)) { c: Group.hasCandidate (first: 2000) %s %s } }" % (H["uid"], live, shape), ns=1)
            via = c.rows("{ q(func: uid(%s)) { a: Group.hasApplication @filter(NOT eq(Recordable.deleted, true)) "
                         "{ uid c: Application.hasCandidate %s %s } } }" % (G["uid"], live, shape), ns=1)

            def groups_of(x):
                return set(uids(x.get("g")))

            def app_groups(x):
                return {u for a in x.get("a") or [] for u in uids(a.get("g"))}

            cand_g = (in_g[0].get("c") if in_g else None) or []
            cand_h = (in_h[0].get("c") if in_h else None) or []
            cand_via = [x for a in ((via[0].get("a") if via else None) or []) for x in
                        (a["c"] if isinstance(a.get("c"), list) else [a["c"]] if a.get("c") else [])]
            print("candidates in G: %d sampled; in H: %d sampled; reached by an application in G: %d" % (
                len(cand_g), len(cand_h), len(cand_via)))
            A = next((x for x in cand_g if groups_of(x) == {G["uid"]}), None) or (cand_g[0] if cand_g else None)
            B = next((x for x in cand_h if G["uid"] not in groups_of(x) and G["uid"] not in app_groups(x)), None)
            C = next((x for x in cand_via if G["uid"] not in groups_of(x)), None)
            targets = {"A: in G": A, "B: not in G, no application in G": B, "C: not in G, has an application in G": C}
            for k, v in targets.items():
                print("  %-40s %s groups %s application groups %s" % (
                    k, v and v["uid"], v and sorted(groups_of(v)), v and sorted(app_groups(v))))

            like = c.rows('{ q(func: uid(%s)) { u: Ownable.ownedBy { t: dgraph.type } '
                          'b: Workspace.hasIAMRoleBinding (first: 1) { t: dgraph.type } } }' % ws, ns=1)
            user_types, binding_types = P.edges_types(like[0]["u"]), P.edges_types(like[0]["b"])
            rdf = ['_:role <dgraph.type> "%s" .' % t for t in viewer["t"]]
            rdf += ['_:role <IAMRole.name> "%s" .' % ROLE, '_:role <IAMRole.permission> "_CANDIDATE" .',
                    "_:role <WorkspaceMember.inWorkspace> <%s> ." % ws, "<%s> <Workspace.hasIAMRole> _:role ." % ws]
            for who, role, target in (("gadmin", "_:role", G["uid"]), ("gviewer", "<%s>" % viewer["uid"], G["uid"]),
                                      ("wadmin", "_:role", ws), ("geditor", "<%s>" % editor["uid"], G["uid"])):
                u, b = "_:u_" + who, "_:b_" + who
                rdf += ['%s <dgraph.type> "%s" .' % (u, t) for t in user_types]
                rdf += ['%s <dgraph.type> "%s" .' % (b, t) for t in binding_types]
                rdf += ['%s <User.userId> "%s" .' % (u, WHO[who]), '%s <User.status> "ACTIVE" .' % u,
                        '%s <User.displayName> "Spike %s" .' % (u, who), '%s <User.email> "%s@spike.invalid" .' % (u, who),
                        '%s <Identifiable.sId> "jobli::user::%s" .' % (u, WHO[who]),
                        '%s <Recordable.deleted> "false"^^<xs:boolean> .' % u,
                        "%s <WorkspaceMember.inWorkspace> <%s> ." % (u, ws), "%s <User.hasIAMBinding> %s ." % (u, b),
                        "%s <IAMBinding.forUserMember> %s ." % (b, u), "%s <IAMBinding.forRole> %s ." % (b, role),
                        "%s <IAMBinding.forResource> <%s> ." % (b, target),
                        "%s <WorkspaceMember.inWorkspace> <%s> ." % (b, ws),
                        "%s <IAMRole.hasBinding> %s ." % (role, b), "<%s> <IAMResource.hasIAMBinding> %s ." % (target, b),
                        "<%s> <Workspace.hasIAMRoleBinding> %s ." % (ws, b)]
            c.mutate("\n".join(rdf), ns=1)

            before = c.rows("{ q(func: uid(%s)) { uid s: Candidate.specialty d: Recordable.deleted } }" % ", ".join(
                v["uid"] for v in targets.values() if v), ns=1)
            n_before = c.rows("{ q(func: type(Candidate)) { n: count(uid) } }", ns=1)

            c.flags = P.ON
            P.restart(c)
            if P.get_schema(c) != original:
                print("schema differs from schema_today; setting it")
                P.set_schema(c, original)
            toks = {"owner": P.token()}
            for who in WHO:
                toks[who] = P.token(sub=WHO[who], email="%s@spike.invalid" % who)

            def run(who, q, v=None):
                r = P.gql(c, toks[who], q, v or {})
                d = r.get("data") or {}
                d = next(iter(d.values())) if d else None
                errs = [e.get("message", "")[:160] for e in r.get("errors") or []]
                return (d or {}).get("numUids"), errs

            out = {}
            order = ["owner", "wadmin", "gadmin", "geditor", "gviewer"]
            print("\nread: candidates each caller can list")
            for who in order:
                r = P.gql(c, toks[who], "query { a: aggregateCandidate(filter: {deleted: false}) { count } }", {})
                print("  %-8s %s %s" % (who, (r.get("data") or {}).get("a"), [e["message"][:120] for e in r.get("errors") or []]))
            for op, q in (("update", "mutation($id: [ID!]) @dryRun { updateCandidate(input: {filter: {id: $id}, set: "
                                     "{specialty: \"spike dry run\"}}) { numUids } }"),
                          ("delete", "mutation($id: [ID!]!) @dryRun { deleteCandidate(filter: {id: $id}) { numUids } }")):
                print("\n%s, dry run (numUids)" % op)
                for k, v in targets.items():
                    if not v:
                        continue
                    row = {who: run(who, q, {"id": [v["uid"]]}) for who in order}
                    out[op + " " + k] = row
                    print("  %-40s %s" % (k, "   ".join("%s=%s%s" % (w, n, " " + str(e) if e else "") for w, (n, e) in row.items())))
            print("\nadd, dry run (numUids)")
            add = "mutation($in: [AddCandidateInput!]!) @dryRun { addCandidate(input: $in) { numUids } }"
            for k, groups_in in (("into G", [{"id": G["uid"]}]), ("into H", [{"id": H["uid"]}]), ("no group given", None)):
                item = {"firstName": "Spike", "lastName": "Dryrun"}
                if groups_in:
                    item["inGroup"] = groups_in
                row = {who: run(who, add, {"in": [item]}) for who in order}
                out["add " + k] = row
                print("  %-40s %s" % (k, "   ".join("%s=%s%s" % (w, n, " " + str(e) if e else "") for w, (n, e) in row.items())))
            P.save("writes_today", {"G": G, "H": H, "targets": {k: v and v["uid"] for k, v in targets.items()},
                                    "results": out})
        finally:
            c.flags = P.HEADER_MODE
            P.restart(c)
            try:
                after = c.rows("{ q(func: uid(%s)) { uid s: Candidate.specialty d: Recordable.deleted } }" % ", ".join(
                    v["uid"] for v in targets.values() if v), ns=1)
                n_after = c.rows("{ q(func: type(Candidate)) { n: count(uid) } }", ns=1)
                left = c.rows('{ q(func: eq(Candidate.lastName, "Dryrun")) { uid } }', ns=1)
                print("\nnothing committed: targets unchanged %s, candidate count %s -> %s, dry-run adds left behind %s" % (
                    json.dumps(before, sort_keys=True) == json.dumps(after, sort_keys=True), n_before, n_after, left))
            except NameError:
                pass
            print("removed %d test users" % cleanup(c))
            c.flags = P.ON
            P.restart(c)


main()
