"""Subscriptions: which of a workspace's open pages one change runs again, and which of
them it had to (spike probe, not a proof: it prints and asserts nothing).

A change is sent to every subscription that mentions the changed type, and each runs its
whole query again. This opens what a recruiter's team would have open in ONE workspace,
with the app's own documents, as the workspace owner:

  - the candidate list page, the same page with a text search, and the candidate count
  - the notes panel of eight candidates, and the note count of each

and then makes seven changes for real, one at a time: a note added on the first candidate,
edited, and removed as the app removes it (deleted: true); a candidate added, edited
(a field no page shows), renamed (a field the notes panels show of a candidate), and
deleted. For each change it reports

  - how many subscriptions ran again, and how many of them had a different answer to give
  - how long after the change the last page that had to change got its frame
  - whether any page was left showing something other than what the same query answers now

  today      the root namespace, shared by every workspace, rules as written
  partition  the workspace's own partition, rules as written
  fast       the partition, with the fast path on for queries and mutations

SPIKE_NARROW=1 turns on subscription-scope-routing and subscription-change-narrowing in
every setup, with the @referenceScope lines in the schema. What it adds is removed again.

  SPIKE_SUB=... SPIKE_EMAIL=... python3 probe_subscription_reach.py
"""
import http.server
import json
import os
import re
import threading
import time
import urllib.request

import proof_real as P
import probe_phase1_writes as W
import probe_cross_workspace as X
import probe_subscriptions as S

NARROW = "; subscription-scope-routing=true; subscription-change-narrowing=true" if os.environ.get("SPIKE_NARROW") else ""
ARMS = [("today", P.OFF, None), ("partition", P.ON, 1),
        ("fast", P.FULL + P.INLINE + P.TOGETHER + S.MUTATIONS, 1)]
ARMS = [(n, f + NARROW, ns) for n, f, ns in ARMS
        if not os.environ.get("SPIKE_ARMS") or n in os.environ["SPIKE_ARMS"].split(",")]
PANELS = 8
# SPIKE_MORE=1 makes five more changes, chosen to be easy to get wrong: the note restored,
# moved to the second candidate, that candidate renamed, and a note added on two candidates
MORE = bool(os.environ.get("SPIKE_MORE"))
# SPIKE_WHY=1 has the alpha log what it decided for each page, and prints it
WHY = bool(os.environ.get("SPIKE_WHY"))
if WHY:
    P.ALPHA_ARGS = P.ALPHA_ARGS + ["--v", "2"]
LOG = os.path.join(P.ROOT, "run", "rj", "alpha0.log")


def decisions(since):
    """The narrowing lines the alpha logged after byte offset since, with bucket ids replaced by page order."""
    with open(LOG, "rb") as f:
        f.seek(since)
        text = f.read().decode("utf-8", "replace")
    return [m.group(1) for m in re.finditer(r"subscription narrowing: (.*)", text)]
SETTLE = 120


def wakes(c):
    """What became of the changes that reached a subscription: {rerun, scoped, skipped, checked}."""
    text = urllib.request.urlopen(c.alpha_http(0) + "/debug/prometheus_metrics", timeout=30).read().decode()
    out = {}
    for what, n in re.findall(r'^dgraph_subscription_wakes_total\{method="(\w+)"[^}]*\} (\d+)', text, re.M):
        out[what] = out.get(what, 0) + int(n)
    return out


def as_query(document):
    query, n = re.subn(r"(?m)^(\s*)subscription(\s+\w+\s*[({])", r"\1query\2", document, count=1)
    if n != 1:
        raise SystemExit("no subscription operation found in a document")
    return query


def scalars(c, uid, ns):
    """The record's own values, by predicate."""
    row = S.nsrows(c, "{ q(func: uid(%s)) { expand(_all_) } }" % uid, ns)
    return {k: v for k, v in (row[0] if row else {}).items()
            if not isinstance(v, dict) and not (isinstance(v, list) and v and isinstance(v[0], dict))}


def put_back(c, uid, before, ns):
    """Writes back the values a change gave the record."""
    now = scalars(c, uid, ns)
    changed = [k for k in set(before) | set(now) if before.get(k) != now.get(k)]
    if not changed:
        return 0
    c.mutate("\n".join("<%s> <%s> * ." % (uid, k) for k in changed), ns=ns, delete=True)
    rdf = []
    for k in changed:
        for v in (before[k] if isinstance(before.get(k), list) else [before[k]] if k in before else []):
            rdf.append("<%s> <%s> %s ." % (uid, k, json.dumps(str(v).lower() if isinstance(v, bool) else str(v))))
    if rdf:
        c.mutate("\n".join(rdf), ns=ns)
    return len(changed)


def settle(c, quiet=2.0):
    """Waits until the alpha has stopped asking the store anything for a while."""
    last, since, end = None, time.time(), time.time() + SETTLE
    while time.time() < end:
        now = (S.store_queries(c), wakes(c).get("rerun", 0))
        if now != last:
            last, since = now, time.time()
        elif time.time() - since >= quiet:
            return True
        time.sleep(0.2)
    return False


def main():
    if not (P.SUB and P.EMAIL):
        raise SystemExit("set SPIKE_SUB and SPIKE_EMAIL to the userId and email of the owner of %s" % P.WS)
    server = http.server.ThreadingHTTPServer(("127.0.0.1", P.LAMBDA_PORT), W.Stub)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    original = open(os.path.join(P.OUT, "schema_today.graphql")).read()
    params = P.load("params")
    suite = P.suite(params)
    cand_list = P.app_query("api/candidate.ts", "SUBSCRIPTION_CANDIDATE_FOR_LIST")
    cand_count = P.app_query("api/aggregation.ts", "AGGREGATE_CANDIDATE_SUBSCRIPTION")
    note_list = P.app_query("api/note.ts", "NOTE_SUBSCRIPTION")
    note_count = P.app_query("api/note.ts", "NOTE_AGGREGATE_SUBSCRIPTION")
    add_note = P.app_query("api/note.ts", "ADD_NOTE")
    upd_note = P.app_query("api/note.ts", "UPDATE_NOTE")
    del_note = P.app_query("api/note.ts", "DELETE_NOTE")
    add_cand = "mutation($in: [AddCandidateInput!]!) { data: addCandidate(input: $in) { candidate { id } } }"
    upd_cand = "mutation($in: UpdateCandidateInput!) { data: updateCandidate(input: $in) { candidate { id } } }"
    del_cand = "mutation($id: [ID!]!) { data: deleteCandidate(filter: {id: $id}) { numUids } }"
    out = {}
    with P.cluster(P.OFF) as c:
        c.wait_ready(0, timeout=900)
        schema = dict(X.arms(original))["scope"] if NARROW else original
        if P.get_schema(c) != schema:
            r = P.set_schema(c, schema)
            if r.get("errors"):
                raise SystemExit("the schema was refused: %s" % json.dumps(r["errors"])[:400])
        ws = S.nsrows(c, '{ q(func: eq(Workspace.name, "%s")) { uid } }' % P.WS, None)[0]["uid"]
        t = W.pick_targets(c, ws)
        cands = W.uids(S.nsrows(c, "{ q(func: uid(%s)) { c: Group.hasCandidate (first: %d) @filter(%s AND has(%s)) { uid } } }" % (
            t["G"], PANELS, W.LIVE, W.WSM), None)[0]["c"])
        if len(cands) < PANELS:
            raise SystemExit("group %s has only %d candidates" % (t["G"], len(cands)))
        types = W.uids(S.nsrows(c, "{ q(func: type(NoteType), first: 1) @filter(uid_in(%s, %s)) { uid } }" % (W.WSM, ws), None))
        if not types:
            raise SystemExit("the workspace has no note type to give a note")
        tok = P.token()
        # the pages: (name, document, variables)
        pages = [("candidate list", cand_list, suite[0][2]), ("candidate list, text search", cand_list, suite[2][2]),
                 ("candidate count", cand_count, {"filter": suite[0][2]["filter"]})]
        for i, u in enumerate(cands):
            f = {"and": [{"deleted": False}, {"forResource": {"id": [u]}}]}
            pages.append(("notes of candidate %d" % (i + 1), note_list,
                          {"filter": f, "cascade": ["inWorkspace", "forResource"], "first": 20, "offset": 0}))
            pages.append(("note count of candidate %d" % (i + 1), note_count, {"filter": f}))

        for arm, flags, ns in ARMS:
            res = out[arm] = {}
            c.flags = P.HEADER_MODE if ns else P.OFF
            P.restart(c)
            while True:
                stale = sorted(W.uids(S.nsrows(c, "{ q(func: type(SpikeMarker)) { uid } }", ns)), key=lambda u: int(u, 16))
                if not stale:
                    break
                print("an earlier run left its records behind: removed %s" % (S.remove_new(c, stale[0], ns),))
            marker = c.mutate('_:m <dgraph.type> "SpikeMarker" .', ns=ns)["data"]["uids"]["m"]
            second = scalars(c, cands[1], ns)
            streams = []
            try:
                c.flags = flags
                P.restart(c)
                print("\n== %s" % arm, flush=True)

                def answers():
                    """What each page's query answers now."""
                    return [P.canonical((S.gql(c, tok, as_query(d), v).get("data"))) for _, d, v in pages]

                answers()                                           # warm
                for _, d, v in pages:
                    streams.append(S.Stream(c, tok, d, v))
                if not all(s.wait(1, timeout=180) for s in streams):
                    print("  not every page opened: %s" % [s.error for s in streams if not s.frames])
                    continue
                settle(c)
                made = {}

                def change(name, doc, variables):
                    before = answers()
                    settle(c)
                    have = [len(s.frames) for s in streams]
                    w0, q0 = wakes(c), S.store_queries(c)
                    log0 = os.path.getsize(LOG) if WHY else 0
                    s0 = time.perf_counter()
                    r = S.gql(c, tok, doc, variables)
                    done = time.perf_counter()
                    d = (r.get("data") or {}).get("data")
                    if not d or r.get("errors"):
                        print("  %-22s failed: %s" % (name, json.dumps(r.get("errors"))[:300]))
                        return None
                    quiet = settle(c)
                    w1, q1 = wakes(c), S.store_queries(c)
                    after = answers()
                    due = [i for i in range(len(pages)) if before[i] != after[i]]
                    got = [i for i in range(len(pages)) if len(streams[i].frames) > have[i]]
                    stale = [pages[i][0] for i in range(len(pages))
                             if P.canonical(streams[i].frames[-1][1].get("data")) != after[i]]
                    last = max([streams[i].frames[have[i]][0] - done for i in due if i in got] or [0]) * 1000
                    row = res[name] = {
                        "took": (done - s0) * 1000, "reruns": w1.get("rerun", 0) - w0.get("rerun", 0),
                        "skipped": w1.get("skipped", 0) - w0.get("skipped", 0),
                        "checked": w1.get("checked", 0) - w0.get("checked", 0),
                        "store": q1 - q0, "due": len(due), "got": len(got), "missed": [pages[i][0] for i in due if i not in got],
                        "stale": stale, "last": last, "quiet": quiet, "due_pages": [pages[i][0] for i in due]}
                    print("  %-22s took %4.0f ms; %2d of %d pages ran again (%d skipped, %d asked the store); %d had a new answer "
                          "and %d got a frame; last needed frame after %5.0f ms; %d store requests%s%s" % (
                              name, row["took"], row["reruns"], len(pages), row["skipped"], row["checked"], row["due"], row["got"],
                              last, row["store"], "" if quiet else "; NOT quiet in %ds" % SETTLE,
                              "; STALE: %s" % stale if stale else ""), flush=True)
                    if WHY:
                        seen = {}
                        for line in decisions(log0):
                            line = re.sub(r"bucket \d+: ", "", line)
                            seen[line] = seen.get(line, 0) + 1
                        for line, n in sorted(seen.items(), key=lambda kv: -kv[1]):
                            print("      %2d x %s" % (n, line[:700]))
                    return d

                d = change("add a note", add_note, {"input": [{"text": "spike reach", "forResource": [{"id": cands[0]}],
                                                                     "hasType": {"id": types[0]}}]})
                note = d and d["note"] and d["note"][0]["id"]
                if note:
                    change("edit the note", upd_note, {"input": {"filter": {"id": [note]}, "set": {"text": "spike reach, edited"}}})
                    change("remove the note", del_note, {"id": note})
                if note and MORE:
                    for name, v in (
                            ("restore the note", {"filter": {"id": [note]}, "set": {"deleted": False}}),
                            ("move the note", {"filter": {"id": [note]}, "set": {"forResource": [{"id": cands[1]}]},
                                               "remove": {"forResource": [{"id": cands[0]}]}})):
                        change(name, upd_note, {"input": v})
                        print("      due: %s" % res.get(name, {}).get("due_pages"))
                    change("rename a candidate shown", upd_cand, {"in": {"filter": {"id": [cands[1]]}, "set": {"firstName": "Spiked"}}})
                    print("      due: %s" % res.get("rename a candidate shown", {}).get("due_pages"))
                    change("add a note on two", add_note, {"input": [{"text": "spike reach, two", "hasType": {"id": types[0]},
                                                                      "forResource": [{"id": cands[2]}, {"id": cands[3]}]}]})
                    print("      due: %s" % res.get("add a note on two", {}).get("due_pages"))
                d = change("add a candidate", add_cand, {"in": [{"firstName": "Spike", "lastName": "Reach", "inGroup": [{"id": t["G"]}]}]})
                cand = d and d["candidate"] and d["candidate"][0]["id"]
                if cand:
                    change("edit the candidate", upd_cand, {"in": {"filter": {"id": [cand]}, "set": {"specialty": "spike"}}})
                    change("rename the candidate", upd_cand, {"in": {"filter": {"id": [cand]}, "set": {"firstName": "Spiked"}}})
                    change("delete the candidate", del_cand, {"id": [cand]})
            finally:
                for s in streams:
                    s.close()
                c.flags = P.HEADER_MODE if ns else P.OFF
                P.restart(c)
                print("  removed %d nodes and %d edges to them in the %s" % (S.remove_new(c, marker, ns) + ("partition" if ns else "root namespace",)))
                back = put_back(c, cands[1], second, ns)
                if back:
                    print("  wrote back %d values of the second candidate" % back)
        c.flags = P.OFF
        P.restart(c)
        if P.get_schema(c) != original:
            P.set_schema(c, original)

    names = ["add a note", "edit the note", "remove the note"] + (
        ["restore the note", "move the note", "rename a candidate shown", "add a note on two"] if MORE else []) + ["add a candidate", "edit the candidate", "rename the candidate",
             "delete the candidate"]
    for key, text, fmt in (("reruns", "pages run again, of %d" % len(pages), "%d"), ("due", "pages with a new answer", "%d"),
                           ("last", "last needed frame, ms after the change", "%.0f"), ("store", "store requests set off", "%d")):
        print("\n%-24s %s" % (text, "  ".join("%-10s" % a[0] for a in ARMS)))
        for n in names:
            print("  %-22s %s" % (n, "  ".join("%-10s" % ("-" if n not in out.get(a[0], {}) else fmt % out[a[0]][n][key]) for a in ARMS)))
    bad = [(a, n, r["stale"], r["missed"]) for a, rows in out.items() for n, r in rows.items() if r["stale"] or r["missed"]]
    print("\npages left stale, or that never got the frame they were due: %s" % (bad or "none"))
    P.save("subscription_reach%s%s" % ("_narrow" if NARROW else "", "_more" if MORE else ""), out)


if __name__ == "__main__":
    main()
