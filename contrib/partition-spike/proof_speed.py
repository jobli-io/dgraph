#!/usr/bin/env python3
"""Proof point for partition namespaces (spike): query cost per tenant.

Loads the same synthetic candidates two ways on one cluster:

  shared     every tenant's candidates in the root namespace, each with an edge
             to its workspace node; a query filters by workspace (today's model,
             without the cost of evaluating @auth rules on top)
  partition  one tenant's candidates alone in a partition namespace; a query
             needs no workspace filter

and times the same three queries against both. Synthetic data, so the numbers
show the mechanism and its scaling, not production latency.

usage: proof_speed.py [alpha http addr] [tenants, default 200] [candidates per tenant, default 5000]
"""

import json
import random
import statistics
import sys
import time
import urllib.error
import urllib.request

ALPHA = "http://" + (sys.argv[1] if len(sys.argv) > 1 else "localhost:8280")
TENANTS = int(sys.argv[2]) if len(sys.argv) > 2 else 200
PER_TENANT = int(sys.argv[3]) if len(sys.argv) > 3 else 5000
NS_HEADER = "x-dgraph-partition-ns"
RUNS = 30

SCHEMA = """
spikeName: string @index(term) .
spikeStatus: string @index(hash) .
spikeCreatedAt: datetime @index(hour) .
spikeWs: uid .
spikeWsName: string @index(hash) .
"""

FIRST = ["Ada", "Alan", "Grace", "Edsger", "Linus", "Barbara", "Donald", "Margaret", "Ken", "Dennis",
         "Radia", "Tim", "Guido", "Anita", "John", "Frances", "Niklaus", "Leslie", "Shafi", "Tony"]
LAST = ["Smith", "Jones", "Taylor", "Brown", "Wilson", "Evans", "Thomas", "Roberts", "Johnson", "Lewis",
        "Walker", "Robinson", "Wood", "Thompson", "White", "Watson", "Jackson", "Wright", "Green", "Harris",
        "Cooper", "King", "Lee", "Martin", "Clarke", "James", "Morgan", "Hughes", "Edwards", "Hill",
        "Moore", "Clark", "Harrison", "Scott", "Young", "Morris", "Hall", "Ward", "Turner", "Carter",
        "Phillips", "Mitchell", "Patel", "Adams", "Campbell", "Anderson", "Allen", "Cook", "Bailey", "Parker"]
STATUS = ["NEW", "SCREENING", "INTERVIEW", "OFFER", "HIRED", "REJECTED"]


def post(path, body, ns=None, ctype="application/json"):
    req = urllib.request.Request(ALPHA + path, data=body.encode(), method="POST")
    req.add_header("Content-Type", ctype)
    if ns is not None:
        req.add_header(NS_HEADER, str(ns))
    try:
        with urllib.request.urlopen(req, timeout=600) as resp:
            return json.loads(resp.read() or b"{}")
    except urllib.error.HTTPError as e:
        return json.loads(e.read() or b"{}")


def must(r):
    if r.get("errors"):
        raise SystemExit(json.dumps(r["errors"])[:400])
    return r


def candidate_lines(rnd, prefix, ws_ref):
    lines = []
    for i in range(PER_TENANT):
        n = "_:%s%d" % (prefix, i)
        ts = "2026-%02d-%02dT%02d:%02d:00Z" % (rnd.randint(1, 11), rnd.randint(1, 28), rnd.randint(0, 23), rnd.randint(0, 59))
        lines.append('%s <spikeName> "%s %s" .' % (n, rnd.choice(FIRST), rnd.choice(LAST)))
        lines.append('%s <spikeStatus> "%s" .' % (n, rnd.choice(STATUS)))
        lines.append('%s <spikeCreatedAt> "%s" .' % (n, ts))
        if ws_ref:
            lines.append("%s <spikeWs> %s ." % (n, ws_ref))
    return lines


def timed(query, ns):
    lat, wall, result = [], [], None
    for _ in range(RUNS):
        t = time.perf_counter()
        r = must(post("/query", query, ns, "application/dql"))
        wall.append((time.perf_counter() - t) * 1000)
        lat.append(r["extensions"]["server_latency"]["processing_ns"] / 1e6)
        result = r["data"]
    return statistics.median(lat), statistics.median(wall), result


def main():
    must(post("/alter", SCHEMA, None, "text/plain"))
    ns = must(post("/admin", json.dumps({"query": "mutation { addNamespace { namespaceId } }"})))["data"]["addNamespace"]["namespaceId"]

    start = time.time()
    ws0 = None
    for t in range(TENANTS):
        rnd = random.Random(t)
        lines = ['_:ws <spikeWsName> "ws-%d" .' % t] + candidate_lines(rnd, "c", "_:ws")
        r = must(post("/mutate?commitNow=true", "{ set {\n%s\n} }" % "\n".join(lines), None, "application/rdf"))
        if t == 0:
            ws0 = r["data"]["uids"]["ws"]
    # Tenant 0 again, alone in its partition, from the same seed.
    lines = candidate_lines(random.Random(0), "c", None)
    must(post("/mutate?commitNow=true", "{ set {\n%s\n} }" % "\n".join(lines), ns, "application/rdf"))
    print("loaded %d tenants x %d candidates into the root namespace and tenant 0 into partition %d (%.0fs)" % (
        TENANTS, PER_TENANT, ns, time.time() - start))

    f = "@filter(uid_in(spikeWs, %s))" % ws0
    queries = [
        ("term search, newest 20",
         '{ q(func: anyofterms(spikeName, "smith"), orderdesc: spikeCreatedAt, first: 20) %s { uid spikeName spikeCreatedAt } }'),
        ("list page, newest 20",
         '{ q(func: has(spikeStatus), orderdesc: spikeCreatedAt, first: 20) %s { uid spikeName spikeCreatedAt } }'),
        ("count by status",
         '{ q(func: eq(spikeStatus, "INTERVIEW")) %s { count(uid) } }'),
        ("term search, count",
         '{ q(func: anyofterms(spikeName, "smith jones")) %s { count(uid) } }'),
    ]

    print("\nmedian of %d runs, server processing time in ms (wall time in brackets)" % RUNS)
    print("%-26s %20s %20s %10s  %s" % ("query", "shared + ws filter", "partition", "ratio", "same result"))
    for name, q in queries:
        s_lat, s_wall, s_res = timed(q % f, None)
        p_lat, p_wall, p_res = timed(q % "", ns)

        def shape(res):
            rows = res["q"]
            if rows and "count" in rows[0]:
                return rows[0]["count"]
            return [(r.get("spikeName"), r.get("spikeCreatedAt")) for r in rows]

        same = shape(s_res) == shape(p_res)
        print("%-26s %11.2f (%6.2f) %11.2f (%6.2f) %9.1fx  %s" % (
            name, s_lat, s_wall, p_lat, p_wall, s_lat / p_lat if p_lat else 0, "yes" if same else "NO"))


if __name__ == "__main__":
    main()
