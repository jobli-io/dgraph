"""Does a delete the caller is refused still remove what cascades from the record
(spike probe, not a proof: it prints and asserts nothing).

A delete by ID names its record in the query. The resolver reads that ID to find the
records that @cascadeDelete hangs off it, and adds each of them to the delete, before
the record's own delete rule has been asked. If the rule then refuses the record, are
its attachments, address and history removed all the same?

Root namespace of the cluster proof_real.py built, partition namespaces off, today's
schema. The owner adds candidates of their own, each with an attachment and an
address. Then each is deleted, for real, by one caller:

  editor     a member of the same workspace whose role has no delete permission
  outsider   a member of another workspace
  owner      the workspace's owner: the delete that should work

Everything the probe adds is removed again.

  SPIKE_SUB=... SPIKE_EMAIL=... python3 probe_refused_delete.py
"""
import http.server
import json
import os
import threading

import proof_real as P
import probe_phase1_writes as W

WSM = W.WSM
OTHER = W.OTHER_WS
PARTS = (("attachment", "AttachmentOwner.hasAttachment"), ("address", "Candidate.hasAddress"),
         ("create record", "Recordable.hasCreateRecord"))


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
        while True:
            stale = sorted(W.uids(rows("{ q(func: type(SpikeMarker)) { uid } }")), key=lambda u: int(u, 16))
            if not stale:
                break
            print("an earlier run left its records behind: removed %s" % (W.remove_new(c, stale[0]),))
        ws = rows('{ q(func: eq(Workspace.name, "%s")) { uid } }' % P.WS)[0]["uid"]
        ows = rows('{ q(func: eq(Workspace.name, "%s")) { uid } }' % OTHER)[0]["uid"]
        marker = c.mutate('_:m <dgraph.type> "SpikeMarker" .')["data"]["uids"]["m"]
        try:
            t = W.pick_targets(c, ws)
            perms = W.add_editor(c, ws, t["G"])
            other = rows('{ q(func: uid(%s)) { u: Workspace.hasUser @filter(eq(User.status, "ACTIVE")) { User.userId User.email } } }' % ows)[0]["u"]
            here = {u.get("User.userId") for u in rows("{ q(func: uid(%s)) { u: Workspace.hasUser { User.userId } } }" % ws)[0]["u"]}
            out_user = next(u for u in other if u.get("User.userId") and u["User.userId"] not in here)
            toks = {"owner": P.token(), "editor": P.token(sub=W.EDITOR, email="editor@spike.invalid"),
                    "outsider": P.token(ws=OTHER, sub=out_user["User.userId"], email=out_user.get("User.email", ""))}
            print("the editor's role in %s allows: %s" % (P.WS, perms))

            add = "mutation($in: [AddCandidateInput!]!) { m: addCandidate(input: $in) { candidate { id } } }"
            delete = "mutation($id: [ID!]!) { m: deleteCandidate(filter: {id: $id}) { numUids msg } }"
            shape = "{ q(func: uid(%s)) { uid t: dgraph.type n: Candidate.lastName " + " ".join(
                "p%d: %s { uid t: dgraph.type }" % (i, pred) for i, (_, pred) in enumerate(PARTS)) + " } }"

            def state(uid, parts=None):
                """The record and what hangs off it, as the store has them now."""
                r = (rows(shape % uid) or [{}])[0]
                found = {name: W.uids(r.get("p%d" % i)) for i, (name, _) in enumerate(PARTS)}
                if parts is None:
                    return bool(r.get("t")), found
                # A part that was there before: is it still a record, and still tied to the candidate?
                out = {}
                for name, was in parts.items():
                    for u in was:
                        alive = bool((rows("{ q(func: uid(%s)) { t: dgraph.type } }" % u) or [{}])[0].get("t"))
                        out[name] = "kept" if alive and u in found[name] else ("untied" if alive else "GONE")
                return bool(r.get("t")), out

            print("\n%-9s %-9s %-9s %s" % ("caller", "answer", "candidate", "what hung off it"))
            for who in ("editor", "outsider", "owner"):
                d, e = W.ask(c, toks["owner"], add, {"in": [{"firstName": "Spike", "lastName": "Refused delete " + who,
                                                             "inGroup": [{"id": t["G"]}],
                                                             "hasAttachment": [{"category": "RESUME"}],
                                                             "hasAddress": {"city": "Spike"}}]})
                if not d:
                    print("%-9s could not add a candidate to try: %s" % (who, e[:1]))
                    continue
                uid = d["candidate"][0]["id"]
                _, parts = state(uid)
                d, e = W.ask(c, toks[who], delete, {"id": [uid]})
                n = (d or {}).get("numUids")
                there, after = state(uid, parts)
                print("%-9s %-9s %-9s %s%s" % (who, "numUids %s" % n if d else "refused", "kept" if there else "deleted",
                                             ", ".join("%s: %s" % (k, after.get(k, "none to start with")) for k, _ in PARTS),
                                             ("   " + e[0][:110]) if e else ""))
        finally:
            if P.get_schema(c) != original:
                P.set_schema(c, original)
            n, edges, left = W.remove_new(c, marker)
            print("\nremoved %d nodes the probe added and %d edges to them; still there: %s" % (n, edges, left))


if __name__ == "__main__":
    main()
