#!/usr/bin/env python3
"""Proof point for partition namespaces (spike): the tablet-size report.

Every few minutes the group leader tells Zero the size of each predicate it
finds in Badger tables owned by a single predicate. Zero answers a predicate it
has no tablet for with an instruction to delete it. A partition's predicates
have no tablets, so an unpatched alpha would lose a partition's data as soon as
one of its predicates fills a whole table.

This loads enough data into one predicate of one partition to fill several
tables, waits for size reports, and checks the data is still there.

Start the alpha with DGRAPH_TABLET_SIZE_INTERVAL=20s (run-spike.sh does).

usage: proof_tablet_sizes.py [alpha http addr, default localhost:8280] [nodes, default 1000000]
"""

import json
import random
import string
import sys
import time
import urllib.error
import urllib.request

ALPHA = "http://" + (sys.argv[1] if len(sys.argv) > 1 else "localhost:8280")
NODES = int(sys.argv[2]) if len(sys.argv) > 2 else 1000000
BATCH = 50000
NS_HEADER = "x-dgraph-partition-ns"
PRED = "spikeBlob"


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


def count(ns):
    r = post("/query", "{ q(func: has(%s)) { count(uid) } }" % PRED, ns, "application/dql")
    if r.get("errors"):
        return "error: %s" % json.dumps(r["errors"])[:120]
    return ((r.get("data") or {}).get("q") or [{}])[0].get("count", 0)


def tablet_names():
    with urllib.request.urlopen(ALPHA + "/state", timeout=60) as resp:
        s = json.loads(resp.read())
    return [t for g in s.get("groups", {}).values() for t in g.get("tablets", {})]


def main():
    r = post("/alter", "%s: string ." % PRED, None, "text/plain")
    if r.get("errors"):
        raise SystemExit(json.dumps(r["errors"]))
    r = post("/admin", json.dumps({"query": "mutation { addNamespace { namespaceId } }"}))
    ns = r["data"]["addNamespace"]["namespaceId"]
    print("partition namespace: %d, predicate: %s" % (ns, PRED))

    rnd = random.Random(7)
    alphabet = string.ascii_letters + string.digits
    start = time.time()
    for base in range(0, NODES, BATCH):
        lines = []
        for i in range(base, min(base + BATCH, NODES)):
            val = "".join(rnd.choices(alphabet, k=200))
            lines.append('_:n%d <%s> "%s" .' % (i, PRED, val))
        r = post("/mutate?commitNow=true", "{ set {\n%s\n} }" % "\n".join(lines), ns, "application/rdf")
        if r.get("errors"):
            raise SystemExit("mutate error: %s" % json.dumps(r["errors"])[:300])
    loaded = count(ns)
    print("loaded %s nodes (about %d MB of values) in %.0fs" % (loaded, NODES * 200 // (1 << 20), time.time() - start))

    print("%-10s %12s %12s" % ("t (s)", "partition", "root"))
    t0 = time.time()
    last = loaded
    for _ in range(9):
        time.sleep(10)
        last = count(ns)
        print("%-10d %12s %12s" % (time.time() - t0, last, count(None)))

    stray = [t for t in tablet_names() if not t.startswith("0-")]
    ok = last == loaded and not stray
    print("\ntablets outside the root namespace: %s" % (stray or "none"))
    print("%s  partition data %s after %d tablet-size intervals" % (
        "PASS" if ok else "FAIL", "intact" if last == loaded else "LOST (%s of %s left)" % (last, loaded), 4))
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
