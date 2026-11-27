"""Mutations: who is allowed what, and what it costs, today and inside a partition
(spike probe, not a proof: it prints and asserts nothing).

The fast path of 10.4 to 10.8 settles a caller's grants once and leaves the cascades
they settle out of a query. partition-mutation-grants does the same for the rules a
mutation asks. This sends the same dry-run mutations as three callers under four
setups and prints the decision and the time for each:

  today      the root namespace, shared by every workspace, rules as written
  partition  the workspace's own partition, rules as written
  fast       the partition, with the fast path on for queries and mutations
  asking     the same, with nothing remembered between requests

  owner    the workspace's owner
  editor   a user bound to the workspace's editor role on one group
  reader   a user bound to its viewer role on the Workspace: may write nothing

The partition is the one proof_real.py made of the dev workspace (namespace 1). The
records keep their IDs there, so the same mutations name the same records in both.

  SPIKE_SUB=... SPIKE_EMAIL=... python3 probe_partition_mutations.py
"""
import http.server
import json
import os
import re
import statistics
import threading
import time

import proof_real as P
import probe_phase1_writes as W

NS = 1
WSM = W.WSM
LIVE = W.LIVE
WRITERS = {"editor": "spike-mut-group-editor", "reader": "spike-mut-workspace-reader"}
RUNS = int(os.environ.get("SPIKE_MUT_RUNS", "9"))
MUTATIONS = "; partition-mutation-grants=true"
ARMS = [("today", P.OFF), ("partition", P.ON), ("fast", P.FULL + P.INLINE + P.TOGETHER + MUTATIONS),
        ("asking", P.FULL_ASKING + P.INLINE + P.TOGETHER + MUTATIONS)]
ARMS = [a for a in ARMS if not os.environ.get("SPIKE_ARMS") or a[0] in os.environ["SPIKE_ARMS"].split(",")]


def rows(c, q, ns=NS):
    r = c.rows(q, ns=ns)
    if isinstance(r, str):
        raise SystemExit("%s\n%s" % (r, q))
    return r


def remove_writers(c, ns=NS):
    found = rows(c, "{ q(func: eq(User.userId, %s)) @filter(type(User)) { uid b: User.hasIAMBinding { uid "
                    "r: IAMBinding.forRole { uid } x: IAMBinding.forResource { uid } w: %s { uid } } w: %s { uid } } }"
                 % (json.dumps(sorted(WRITERS.values())), WSM, WSM), ns)
    rdf = []
    for u in found:
        for b in u.get("b", []):
            rdf += ["<%s> <IAMRole.hasBinding> <%s> ." % (r, b["uid"]) for r in P.edges(b.get("r"))]
            rdf += ["<%s> <IAMResource.hasIAMBinding> <%s> ." % (x, b["uid"]) for x in P.edges(b.get("x"))]
            rdf += ["<%s> <Workspace.hasIAMRoleBinding> <%s> ." % (w, b["uid"]) for w in P.edges(b.get("w"))]
            rdf.append("<%s> * * ." % b["uid"])
        rdf += ["<%s> <Workspace.hasUser> <%s> ." % (w, u["uid"]) for w in P.edges(u.get("w"))]
        rdf.append("<%s> * * ." % u["uid"])
    if rdf:
        c.mutate("\n".join(rdf), ns=ns, delete=True)
    return len(found)


def add_writers(c, ws, group, ns=NS):
    """Written as the app's own users and bindings are. Needs the header resolver."""
    remove_writers(c, ns)
    role = {}
    for r in rows(c, "{ q(func: type(IAMRole)) @filter(uid_in(%s, %s)) { uid n: IAMRole.name p: IAMRole.permission } }" % (WSM, ws), ns):
        role[r.get("n")] = r
    viewer = next(r for r in role.values() if r.get("p") == ["READ"])
    like = rows(c, "{ q(func: uid(%s)) { u: Ownable.ownedBy { t: dgraph.type } "
                   "b: Workspace.hasIAMRoleBinding (first: 1) { t: dgraph.type } } }" % ws, ns)
    user_types, binding_types = P.edges_types(like[0]["u"]), P.edges_types(like[0]["b"])
    rdf = []
    for who, r, target in (("editor", role["editor"], group), ("reader", viewer, ws)):
        u, b = "_:u_" + who, "_:b_" + who
        rdf += ['%s <dgraph.type> "%s" .' % (u, t) for t in user_types]
        rdf += ['%s <dgraph.type> "%s" .' % (b, t) for t in binding_types]
        rdf += ['%s <User.userId> "%s" .' % (u, WRITERS[who]), '%s <User.status> "ACTIVE" .' % u,
                '%s <User.displayName> "Spike %s" .' % (u, who), '%s <User.email> "%s@spike.invalid" .' % (u, who),
                '%s <Identifiable.sId> "jobli::user::%s" .' % (u, WRITERS[who]),
                '%s <Recordable.deleted> "false"^^<xs:boolean> .' % u,
                "%s <%s> <%s> ." % (u, WSM, ws), "%s <User.hasIAMBinding> %s ." % (u, b),
                "%s <IAMBinding.forUserMember> %s ." % (b, u), "%s <IAMBinding.forRole> <%s> ." % (b, r["uid"]),
                "%s <IAMBinding.forResource> <%s> ." % (b, target), "%s <%s> <%s> ." % (b, WSM, ws),
                "<%s> <IAMRole.hasBinding> %s ." % (r["uid"], b), "<%s> <IAMResource.hasIAMBinding> %s ." % (target, b),
                "<%s> <Workspace.hasIAMRoleBinding> %s ." % (ws, b), "<%s> <Workspace.hasUser> %s ." % (ws, u)]
    c.mutate("\n".join(rdf), ns=ns)
    return sorted(role["editor"].get("p") or [])


def targets(c, ws):
    groups = [g for g in rows(c, "{ q(func: type(Group)) @filter(uid_in(%s, %s) AND %s) { uid n: Group.name "
                                 "jobs: count(Group.hasJob) cands: count(Group.hasCandidate) } }" % (WSM, ws, LIVE))
              if g.get("jobs") and g.get("cands")]
    G = min(groups, key=lambda g: g["jobs"])
    H = max([g for g in groups if g["uid"] != G["uid"]], key=lambda g: g["cands"])
    shape = "{ uid g: Groupable.inGroup { uid } att: count(AttachmentOwner.hasAttachment) }"
    live = "@filter(%s AND has(%s))" % (LIVE, WSM)
    in_g = rows(c, "{ q(func: uid(%s)) { c: Group.hasCandidate %s %s } }" % (G["uid"], live, shape))[0]["c"]
    in_h = rows(c, "{ q(func: uid(%s)) { c: Group.hasCandidate (first: 2000) %s %s } }" % (H["uid"], live, shape))[0]["c"]
    N = next(x for x in in_g if set(W.uids(x.get("g"))) == {G["uid"]} and not x.get("att"))
    B = next(x for x in in_h if G["uid"] not in W.uids(x.get("g")))
    job = rows(c, "{ q(func: uid(%s)) { j: Group.hasJob (first: 1) @filter(%s) { uid } } }" % (G["uid"], LIVE))[0]["j"][0]["uid"]
    cat = rows(c, "{ q(func: type(JobCategory), first: 1) @filter(uid_in(%s, %s)) { uid } }" % (WSM, ws))[0]["uid"]
    comp = rows(c, "{ q(func: uid(%s)) { o: Group.hasCompany (first: 1) @filter(%s) { uid } } }" % (G["uid"], LIVE))
    comp = W.uids((comp or [{}])[0].get("o"))
    t = {"G": G["uid"], "H": H["uid"], "N": N["uid"], "B": B["uid"], "job": job, "category": cat, "company": comp[0] if comp else None}
    print("group G = %s %s (the editor's), other group H = %s %s" % (G["uid"], G.get("n"), H["uid"], H.get("n")))
    print("targets:", json.dumps(t))
    return t


def cases(t):
    g = lambda *ids: [{"id": i} for i in ids]
    cand = "mutation($in: [AddCandidateInput!]!) @dryRun { m: addCandidate(input: $in) { numUids } }"
    updc = "mutation($in: UpdateCandidateInput!) @dryRun { m: updateCandidate(input: $in) { numUids } }"
    delc = "mutation($id: [ID!]!) @dryRun { m: deleteCandidate(filter: {id: $id}) { numUids } }"
    delj = "mutation($id: [ID!]!) @dryRun { m: deleteJob(filter: {id: $id}) { numUids } }"
    updo = "mutation($in: UpdateCompanyInput!) @dryRun { m: updateCompany(input: $in) { numUids } }"
    updj = "mutation($in: UpdateJobInput!) @dryRun { m: updateJob(input: $in) { numUids } }"
    tag = "mutation($in: [AddTagInput!]!) @dryRun { m: addTag(input: $in) { numUids } }"
    note = "mutation($in: [AddNoteInput!]!) @dryRun { m: addNote(input: $in) { numUids } }"
    att = "mutation($in: [AddAttachmentInput!]!) @dryRun { m: addAttachment(input: $in) { numUids } }"
    C = lambda **k: [dict({"firstName": "Spike", "lastName": "Mutation"}, **k)]
    return [
        ("update a candidate of G: a plain field", updc, {"in": {"filter": {"id": [t["N"]]}, "set": {"specialty": "spike"}}}),
        ("update a candidate outside G: a plain field", updc, {"in": {"filter": {"id": [t["B"]]}, "set": {"specialty": "spike"}}}),
        ("update a candidate of G: add the group H", updc, {"in": {"filter": {"id": [t["N"]]}, "set": {"inGroup": g(t["H"])}}}),
        ("add a candidate into G", cand, {"in": C(inGroup=g(t["G"]))}),
        ("add a candidate into H", cand, {"in": C(inGroup=g(t["H"]))}),
        ("add a candidate, no group given", cand, {"in": C()}),
        ("add a candidate into G with a new attachment", cand, {"in": C(inGroup=g(t["G"]), hasAttachment=[{"category": "RESUME"}])}),
        ("add a tag on a candidate of G", tag, {"in": [{"name": "spike-mutation", "tagable": g(t["N"])}]}),
        ("add a note on a candidate of G", note, {"in": [{"text": "spike", "forResource": g(t["N"])}]}),
        ("add an attachment on a candidate of G", att, {"in": [{"category": "OTHER", "forResource": g(t["N"])}]}),
        ("class a job of G under a category", updj, {"in": {"filter": {"id": [t["job"]]}, "set": {"class": {"id": t["category"]}}}}),
        ("update a job of G: a plain field", updj, {"in": {"filter": {"id": [t["job"]]}, "set": {"summary": "spike"}}}),
        ("update a company of G: a plain field", updo, {"in": {"filter": {"id": [t["company"]]}, "set": {"name": "Spike Mutation Co"}}}),
        ("delete a job of G", delj, {"id": [t["job"]]}),
        ("delete a candidate of G", delc, {"id": [t["N"]]}),
        ("delete a candidate outside G", delc, {"id": [t["B"]]}),
    ]


def ask(c, tok, query, variables, debug=False):
    r = P.gql(c, tok, query, variables, debug=debug)
    d = r.get("data") or {}
    d = next(iter(d.values())) if d else None
    errs = [e.get("message", "") for e in r.get("errors") or []]
    if errs and not d:
        out = "refused" if "uthoriz" in errs[0] else "error"
    else:
        out = str((d or {}).get("numUids"))
    return out, (errs[0] if errs else ""), r.get("extensions") or {}


def main():
    if not (P.SUB and P.EMAIL):
        raise SystemExit("set SPIKE_SUB and SPIKE_EMAIL to the userId and email of the owner of %s" % P.WS)
    server = http.server.ThreadingHTTPServer(("127.0.0.1", P.LAMBDA_PORT), W.Stub)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    original = open(os.path.join(P.OUT, "schema_today.graphql")).read()
    with P.cluster(P.ON) as c:
        try:
            c.flags = P.HEADER_MODE
            P.restart(c)
            ws = rows(c, '{ q(func: eq(Workspace.name, "%s")) { uid } }' % P.WS)[0]["uid"]
            t = targets(c, ws)
            perms = add_writers(c, ws, t["G"])
            add_writers(c, ws, t["G"], ns=0)
            print("the editor role's permissions: %s" % perms)
            if P.get_schema(c) != original:
                P.set_schema(c, original)
            # Read with the header resolver, as the partition is only named by header then.
            count = ("{ c(func: type(Candidate)) { count(uid) } t(func: type(Tag)) { count(uid) } n(func: type(Note)) { count(uid) } "
                     "a(func: type(Attachment)) { count(uid) } d(func: type(Address)) { count(uid) } r(func: type(CreateRecord)) { count(uid) } }")
            before = [c.dql(count, ns=n)["data"] for n in (NS, 0)]
            toks = {"owner": P.token(), "editor": P.token(sub=WRITERS["editor"], email="editor@spike.invalid"),
                    "reader": P.token(sub=WRITERS["reader"], email="reader@spike.invalid")}
            todo = [x for x in cases(t) if not os.environ.get("SPIKE_CASES") or any(k in x[0] for k in os.environ["SPIKE_CASES"].split(","))]
            toks = {k: v for k, v in toks.items() if not os.environ.get("SPIKE_WHO") or k in os.environ["SPIKE_WHO"].split(",")}
            res, messages = {}, {}
            for arm, flags in ARMS:
                c.flags = flags
                P.restart(c)
                for who, tok in toks.items():
                    for label, q, v in todo:
                        out, msg, _ = ask(c, tok, q, v)
                        res[arm, who, label] = {"out": out}
                        if msg:
                            messages.setdefault(re.sub(r"0x[0-9a-f]+", "<uid>", msg)[:230], []).append("%s / %s: %s" % (arm, who, label))
                        wall = []
                        for _ in range(RUNS):
                            s = time.perf_counter()
                            ask(c, tok, q, v)
                            wall.append((time.perf_counter() - s) * 1000)
                        res[arm, who, label]["ms"] = statistics.median(wall)
            names = [a for a, _ in ARMS]
            for who in toks:
                print("\n%s (numUids of a dry run; median ms of %d)" % (who, RUNS))
                print("  %-46s %s" % ("", "  ".join("%-18s" % n for n in names)))
                for label, _, _ in todo:
                    cells = ["%-7s %5.0f ms   " % (res[a, who, label]["out"], res[a, who, label]["ms"]) for a in names]
                    same = len({res[a, who, label]["out"] for a in names}) == 1
                    print("  %-46s %s%s" % (label[:46], "  ".join(cells), "" if same else "   DIFFERENT DECISION"))
            print()
            for m, where in messages.items():
                print("  message: %s\n      on: %s%s" % (m, "; ".join(where[:3]), " and %d more" % (len(where) - 3) if len(where) > 3 else ""))
        finally:
            c.flags = P.HEADER_MODE
            P.restart(c)
            after = [c.dql(count, ns=n)["data"] for n in (NS, 0)]
            print("\nnothing was written: %s%s" % (before == after, "" if before == after else "\n  before %s\n  after  %s" % (
                json.dumps(before, sort_keys=True), json.dumps(after, sort_keys=True))))
            print("removed %d test users" % (remove_writers(c) + remove_writers(c, ns=0)))
            c.flags = P.ON
            P.restart(c)
            if P.get_schema(c) != original:
                P.set_schema(c, original)


if __name__ == "__main__":
    main()
