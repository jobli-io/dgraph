"""Subscriptions: do they work, what does one change cost, and how far does it reach
(spike probe, not a proof: it prints and asserts nothing).

The web app subscribes to its list pages with the list query itself, sent as a
subscription over server-sent events (utils/graphqlSubscription.ts). The alpha answers
with the current result and then runs the whole query again each time a GraphQL
mutation touches a type the query mentions, sending a frame when the result differs.

For each setup this opens the candidate list page as a subscription, with the app's own
document (SUBSCRIPTION_CANDIDATE_FOR_LIST), as the workspace owner, and reports:

  - the first frame, against the same query asked plainly
  - how long after a candidate is added the next frame arrives
  - the same with nine list subscriptions open, as nine people would have
  - how many store queries one add sets off, and whether an add in ANOTHER workspace
    sets off the same work here

  today      the root namespace, shared by every workspace, rules as written
  partition  the workspace's own partition, rules as written
  fast       the partition, with the fast path on for queries and mutations
  two-ns     the partition and the root namespace on one alpha, each named by header:
             the subscribers are in one and the other workspace's add in the other

What it adds is committed, and removed again.

  SPIKE_SUB=... SPIKE_EMAIL=... python3 probe_subscriptions.py
"""
import http.client
import http.server
import json
import os
import re
import statistics
import threading
import time
import urllib.parse
import urllib.request

import proof_real as P
import probe_phase1_writes as W
import probe_cross_workspace as X
from spikelib import NS_HEADER

WSM = W.WSM
OTHER = W.OTHER_WS
MUTATIONS = "; partition-mutation-grants=true"
# SPIKE_ROUTING=1 adds subscription-scope-routing to every setup, and the @referenceScope
# lines of graph/.graphql to the schema: in a shared namespace they are what says whose
# record a mutation changed.
ROUTING = "; subscription-scope-routing=true" if os.environ.get("SPIKE_ROUTING") else ""
# (name, flags, the namespace the workspace's data is in, whether requests name it by header)
ARMS = [("today", P.OFF, None, False), ("partition", P.ON, 1, False),
        ("fast", P.FULL + P.INLINE + P.TOGETHER + MUTATIONS, 1, False),
        # The subscribers in the partition and the other workspace's add in the root namespace, each
        # named by header: the only way here to have two namespaces written to on one alpha.
        ("two-ns", P.HEADER_MODE, 1, True)]
ARMS = [(n, f + ROUTING, ns, h) for n, f, ns, h in ARMS
        if not os.environ.get("SPIKE_ARMS") or n in os.environ["SPIKE_ARMS"].split(",")]
OTHERS = 8
WAIT = 45


class Stream:
    """One subscription over server-sent events. Frames are kept with the time they came."""

    def __init__(self, c, tok, document, variables, ns=None):
        u = urllib.parse.urlparse(c.alpha_http(0))
        self.conn = http.client.HTTPConnection(u.hostname, u.port, timeout=600)
        self.frames, self.error, self.opened = [], None, time.perf_counter()
        body = json.dumps({"query": document, "variables": variables})
        headers = {"Content-Type": "application/json", "Accept": "text/event-stream", P.HEADER: tok}
        if ns is not None:
            headers[NS_HEADER] = str(ns)
        self.conn.request("POST", "/graphql", body, headers)
        threading.Thread(target=self._read, daemon=True).start()

    def _read(self):
        try:
            resp = self.conn.getresponse()
            if "event-stream" not in (resp.getheader("Content-Type") or ""):
                self.error = "not a stream: %s %s" % (resp.status, resp.read(300))
                return
            while True:
                line = resp.readline()
                if not line:
                    return
                if line.startswith(b"data: ") and len(line) > 8:
                    try:
                        self.frames.append((time.perf_counter(), json.loads(line[6:])))
                    except ValueError:
                        pass
        except Exception as e:                                   # closed from our side, or the alpha went away
            self.error = self.error or repr(e)[:120]

    def wait(self, n, timeout=WAIT):
        """The time the n-th frame came, or None."""
        end = time.time() + timeout
        while time.time() < end and len(self.frames) < n and not (self.error and len(self.frames) < n and not self.conn.sock):
            time.sleep(0.005)
        return self.frames[n - 1][0] if len(self.frames) >= n else None

    def close(self):
        try:
            self.conn.close()
        except Exception:
            pass


def shape(frame):
    """Rows and count of a list frame, and its first error."""
    d = (frame or {}).get("data") or {}
    rows = next((len(v) for v in d.values() if isinstance(v, list)), None)
    count = next((v.get("count") for v in d.values() if isinstance(v, dict) and "count" in v), None)
    errs = (frame or {}).get("errors") or []
    return rows, count, (errs[0].get("message", "")[:140] if errs else "")


def gql(c, tok, query, variables, ns=None):
    return c.post("/graphql", json.dumps({"query": query, "variables": variables}), ns=ns, headers={P.HEADER: tok})


def store_queries(c):
    text = urllib.request.urlopen(c.alpha_http(0) + "/debug/prometheus_metrics", timeout=30).read().decode()
    m = re.search(r'^dgraph_num_queries_total\{method="Server\.Query"[^}]*\} (\d+)', text, re.M)
    return int(m.group(1)) if m else 0


def nsrows(c, q, ns):
    r = c.rows(q, ns=ns)
    if isinstance(r, str):
        raise SystemExit("%s\n%s" % (r, q))
    return r


def remove_new(c, marker, ns):
    """W.remove_new, in a namespace: every node made after the marker, and every edge to one."""
    new, after = [], marker
    while True:
        page = W.uids(nsrows(c, "{ q(func: has(dgraph.type), first: 10000, after: %s) { uid } }" % after, ns))
        new += page
        if len(page) < 10000:
            break
        after = page[-1]
    rdf = []
    for u in new:
        rdf += ["<%s> * * ." % u, "<%s> <dgraph.type> * ." % u]
    rdf.append("<%s> <dgraph.type> * ." % marker)
    c.mutate("\n".join(rdf), ns=ns, delete=True)
    # a partition takes its schema from the root namespace
    schema = c.dql("schema {}", ns=0 if ns else None)["data"]["schema"]
    preds = sorted(x["predicate"] for x in schema if x.get("type") == "uid" and not x["predicate"].startswith("dgraph."))
    edges = []
    for i in range(0, len(preds), 30):
        chunk = preds[i:i + 30]
        r = c.dql("{ %s }" % " ".join("p%d(func: has(<%s>)) @cascade { uid x: <%s> (after: %s) { uid } }" % (j, x, x, marker)
                                      for j, x in enumerate(chunk)), ns=ns)
        for j, x in enumerate(chunk):
            for row in (r.get("data") or {}).get("p%d" % j) or []:
                edges += [(row["uid"], x, u) for u in W.refs(row.get("x"))]
    if edges:
        c.mutate("\n".join("<%s> <%s> <%s> ." % e for e in edges), ns=ns, delete=True)
    return len(new), len(edges)


def main():
    if not (P.SUB and P.EMAIL):
        raise SystemExit("set SPIKE_SUB and SPIKE_EMAIL to the userId and email of the owner of %s" % P.WS)
    server = http.server.ThreadingHTTPServer(("127.0.0.1", P.LAMBDA_PORT), W.Stub)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    original = open(os.path.join(P.OUT, "schema_today.graphql")).read()
    params = P.load("params")
    _, _, variables = P.suite(params)[0]
    # the document the app's list page subscribes with, and the same selection asked as a query
    document = P.app_query("api/candidate.ts", "SUBSCRIPTION_CANDIDATE_FOR_LIST")
    query, n = re.subn(r"(?m)^(\s*)subscription(\s+\w+\s*[({])", r"\1query\2", document, count=1)
    if n != 1:
        raise SystemExit("no subscription operation found in the app's list document")
    add = "mutation($in: [AddCandidateInput!]!) { m: addCandidate(input: $in) { candidate { id } } }"
    out = {}
    with P.cluster(P.OFF) as c:
        c.wait_ready(0, timeout=900)
        schema = original
        if ROUTING:
            schema = dict(X.arms(original))["scope"]
        if P.get_schema(c) != schema:
            r = P.set_schema(c, schema)
            if r.get("errors"):
                raise SystemExit("the schema was refused: %s" % json.dumps(r["errors"])[:400])
        ws = nsrows(c, '{ q(func: eq(Workspace.name, "%s")) { uid } }' % P.WS, None)[0]["uid"]
        ows = nsrows(c, '{ q(func: eq(Workspace.name, "%s")) { uid } }' % OTHER, None)[0]["uid"]
        t = W.pick_targets(c, ws)
        other = nsrows(c, '{ q(func: uid(%s)) { u: Workspace.hasUser @filter(eq(User.status, "ACTIVE")) { User.userId User.email } } }' % ows, None)[0]["u"]
        here = {u.get("User.userId") for u in nsrows(c, "{ q(func: uid(%s)) { u: Workspace.hasUser { User.userId } } }" % ws, None)[0]["u"]}
        out_user = next(u for u in other if u.get("User.userId") and u["User.userId"] not in here)
        OG = nsrows(c, "{ q(func: type(Group)) @filter(uid_in(%s, %s) AND %s) { uid } }" % (WSM, ows, W.LIVE), None)[0]["uid"]
        toks = {"owner": P.token(), "outsider": P.token(ws=OTHER, sub=out_user["User.userId"], email=out_user.get("User.email", ""))}
        mine = {"in": [{"firstName": "Spike", "lastName": "Subscription", "inGroup": [{"id": t["G"]}]}]}
        theirs = {"in": [{"firstName": "Spike", "lastName": "Subscription", "inGroup": [{"id": OG}]}]}

        for arm, flags, ns, by_header in ARMS:
            mine_ns, theirs_ns = (ns, 0) if by_header else (None, None)
            res = out[arm] = {}
            c.flags = P.HEADER_MODE if ns else P.OFF
            P.restart(c)
            while True:
                stale = sorted(W.uids(nsrows(c, "{ q(func: type(SpikeMarker)) { uid } }", ns)), key=lambda u: int(u, 16))
                if not stale:
                    break
                print("an earlier run left its records behind: removed %s" % (remove_new(c, stale[0], ns),))
            marker = c.mutate('_:m <dgraph.type> "SpikeMarker" .', ns=ns)["data"]["uids"]["m"]
            # with two namespaces named by header, the other workspace's adds land in the root one
            marker0 = c.mutate('_:m <dgraph.type> "SpikeMarker" .', ns=0)["data"]["uids"]["m"] if by_header else None
            streams = []
            try:
                c.flags = flags
                P.restart(c)
                print("\n== %s" % arm, flush=True)
                # the same query, asked plainly
                wall = []
                for i in range(6):
                    s = time.perf_counter()
                    r = gql(c, toks["owner"], query, variables, mine_ns)
                    wall.append((time.perf_counter() - s) * 1000)
                res["query"] = statistics.median(wall[1:])
                plain = shape(r)
                first = Stream(c, toks["owner"], document, variables, mine_ns)
                streams.append(first)
                at = first.wait(1)
                if at is None:
                    print("  no first frame in %ds: %s" % (WAIT, first.error))
                    res["works"] = False
                    continue
                got = shape(first.frames[0][1])
                res["first"] = (at - first.opened) * 1000
                res["works"] = got[:2] == plain[:2] and got[0] is not None
                print("  the query asked plainly: %s rows, count %s, %.0f ms" % (plain[0], plain[1], res["query"]))
                print("  the subscription's first frame: %s rows, count %s, after %.0f ms %s" % (got[0], got[1], res["first"], got[2]))
                if not res["works"]:
                    print("  the subscription does not answer as the query does: nothing more is measured here")
                    continue

                def added(who, v, watch):
                    """Adds a candidate; returns the mutation's time and, for each stream watched, the time to its next frame."""
                    have = [len(s.frames) for s in watch]
                    s0 = time.perf_counter()
                    r = gql(c, toks[who], add, v, mine_ns)
                    done = time.perf_counter()
                    if not (r.get("data") or {}).get("m"):
                        return None, [], json.dumps(r.get("errors"))[:200]
                    lat = []
                    for s, n in zip(watch, have):
                        at = s.wait(n + 1, timeout=WAIT)
                        lat.append(None if at is None else (at - done) * 1000)
                    return (done - s0) * 1000, lat, ""

                one = []
                for _ in range(3):
                    took, lat, err = added("owner", mine, [first])
                    if err:
                        print("  the add failed: %s" % err)
                        break
                    one.append((took, lat[0]))
                    time.sleep(1.5)
                if one and all(l is not None for _, l in one):
                    res["add"] = statistics.median(x for x, _ in one)
                    res["frame"] = statistics.median(l for _, l in one)
                    print("  one subscription: the add took %.0f ms; the next frame came %.0f ms after it (median of %d)" % (
                        res["add"], res["frame"], len(one)))
                else:
                    print("  one subscription: no frame followed an add: %s" % one)

                # eight more people on the same page, each their own bucket
                for i in range(OTHERS):
                    streams.append(Stream(c, toks["owner"], document, dict(variables, first=variables["first"] + 1 + i), mine_ns))
                opened = [s.wait(1, timeout=120) for s in streams[1:]]
                if all(opened):
                    time.sleep(1)
                    q0 = store_queries(c)
                    took, lat, err = added("owner", mine, streams)
                    time.sleep(2.5)
                    res["set_off"] = store_queries(c) - q0
                    ok = [l for l in lat if l is not None]
                    if ok:
                        res["nine_first"], res["nine_last"], res["nine_got"] = min(ok), max(ok), len(ok)
                        print("  %d subscriptions: the add took %.0f ms; frames came %.0f to %.0f ms after it (%d of %d); "
                              "the add set off %d store requests" % (len(streams), took, min(ok), max(ok), len(ok), len(streams), res["set_off"]))
                    time.sleep(1)
                    # an add in another workspace
                    q0 = store_queries(c)
                    s0 = time.perf_counter()
                    r = gql(c, toks["outsider"], add, theirs, theirs_ns)
                    took = (time.perf_counter() - s0) * 1000
                    time.sleep(3.5)
                    there = store_queries(c) - q0
                    if (r.get("data") or {}).get("m"):
                        res["other_set_off"] = there
                        print("  with those %d open, an add in %s took %.0f ms and set off %d store requests" % (len(streams), OTHER, took, there))
                    else:
                        print("  an add in %s could not be made here: %s" % (OTHER, json.dumps(r.get("errors"))[:160]))
                else:
                    print("  the other %d subscriptions did not all open" % OTHERS)
                for s in streams:
                    s.close()
                streams = []
                time.sleep(3)
                # the same two adds with nobody subscribed
                q0 = store_queries(c)
                gql(c, toks["owner"], add, mine, mine_ns)
                time.sleep(2.5)
                res["alone"] = store_queries(c) - q0
                q0 = store_queries(c)
                r = gql(c, toks["outsider"], add, theirs, theirs_ns)
                time.sleep(2.5)
                if (r.get("data") or {}).get("m"):
                    res["other_alone"] = store_queries(c) - q0
                print("  with nobody subscribed: an add here sets off %s store requests, an add in %s %s" % (
                    res.get("alone"), OTHER, res.get("other_alone", "n/a")))
            finally:
                for s in streams:
                    s.close()
                c.flags = P.HEADER_MODE if ns else P.OFF
                P.restart(c)
                print("  removed %d nodes and %d edges to them in the %s" % (remove_new(c, marker, ns) + ("partition" if ns else "root namespace",)))
                if marker0:
                    print("  and %d nodes and %d edges to them in the root namespace" % remove_new(c, marker0, 0))
        c.flags = P.OFF
        P.restart(c)
        if P.get_schema(c) != original:
            P.set_schema(c, original)

    print("\n%-58s %s" % ("", "  ".join("%-10s" % a[0] for a in ARMS)))
    for key, text in (("works", "the subscription answers as the query does"), ("query", "the list query, asked plainly (ms)"),
                      ("first", "first frame (ms)"), ("add", "adding a candidate (ms)"), ("frame", "next frame after the add, one subscription (ms)"),
                      ("nine_first", "first of nine frames after an add (ms)"), ("nine_last", "last of nine frames after an add (ms)"),
                      ("set_off", "store requests one add sets off, nine subscribed"), ("alone", "the same, nobody subscribed"),
                      ("other_set_off", "store requests an add in another workspace sets off"), ("other_alone", "the same, nobody subscribed")):
        cells = []
        for a in (x[0] for x in ARMS):
            v = out.get(a, {}).get(key)
            cells.append("%-10s" % ("-" if v is None else v if isinstance(v, bool) else "%.0f" % v))
        print("%-58s %s" % (text, "  ".join(cells)))


if __name__ == "__main__":
    main()
