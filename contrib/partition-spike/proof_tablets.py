#!/usr/bin/env python3
"""Proof point for partition namespaces (spike): Zero's tablet list per tenant.

Creates N namespaces on a cluster that already has a schema in the root
namespace and reports how Zero's membership state grows.

  partition mode  alpha runs with --feature-flags "partition-namespaces=true";
                  a namespace is only an ID and inherits the root schema.
  stock mode      alpha runs with ACL; each namespace gets its own copy of the
                  schema, which is how stock Dgraph multi-tenancy works.

usage: proof_tablets.py <partition|stock> <alpha http addr> <count> <schema.dql>
"""

import json
import sys
import time
import urllib.error
import urllib.request

MODE, ADDR, COUNT, SCHEMA_FILE = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]
ALPHA = "http://" + ADDR


def post(path, body, token=None, ctype="application/json", timeout=600):
    req = urllib.request.Request(ALPHA + path, data=body.encode(), method="POST")
    req.add_header("Content-Type", ctype)
    if token:
        req.add_header("X-Dgraph-AccessToken", token)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read() or b"{}")
    except urllib.error.HTTPError as e:
        return json.loads(e.read() or b"{}")


def admin(query, token=None):
    r = post("/admin", json.dumps({"query": query}), token)
    if r.get("errors"):
        raise SystemExit("admin error: %s" % json.dumps(r["errors"])[:300])
    return r["data"]


def login(ns):
    q = 'mutation { login(userId: "groot", password: "password", namespace: %d) { response { accessJWT } } }' % ns
    return admin(q)["login"]["response"]["accessJWT"]


STATE_TOKEN = None


def state():
    req = urllib.request.Request(ALPHA + "/state")
    if STATE_TOKEN:
        req.add_header("X-Dgraph-AccessToken", STATE_TOKEN)
    with urllib.request.urlopen(req, timeout=120) as resp:
        raw = resp.read()
    s = json.loads(raw)
    tablets = sum(len(g.get("tablets", {})) for g in s.get("groups", {}).values())
    return tablets, len(raw)


def alter(schema, token=None):
    r = post("/alter", schema, token, "text/plain")
    if r.get("errors"):
        raise SystemExit("alter error: %s" % json.dumps(r["errors"])[:400])


def settle(expect_at_least):
    for _ in range(60):
        t, size = state()
        if t >= expect_at_least:
            return t, size
        time.sleep(0.5)
    return state()


def main():
    schema = open(SCHEMA_FILE).read()
    preds = sum(1 for line in schema.splitlines() if line.rstrip().endswith(" ."))
    global STATE_TOKEN
    root_token = login(0) if MODE == "stock" else None
    STATE_TOKEN = root_token

    t0 = time.time()
    alter(schema, root_token)
    base_tablets, base_size = settle(preds)
    print("mode: %s" % MODE)
    print("schema: %d predicates, applied to the root namespace in %.1fs" % (preds, time.time() - t0))
    print("%-12s %10s %16s %14s" % ("namespaces", "tablets", "state (bytes)", "elapsed (s)"))
    print("%-12d %10d %16d %14s" % (0, base_tablets, base_size, "-"))

    marks = sorted(set([1, 10, COUNT // 4, COUNT // 2, COUNT]) - {0})
    start = time.time()
    for i in range(1, COUNT + 1):
        ns = admin("mutation { addNamespace { namespaceId } }", root_token)["addNamespace"]["namespaceId"]
        if MODE == "stock":
            alter(schema, login(ns))
        if i in marks:
            tablets, size = settle(base_tablets + (preds * i if MODE == "stock" else 0))
            print("%-12d %10d %16d %14.1f" % (i, tablets, size, time.time() - start))

    tablets, size = state()
    per_ns = (tablets - base_tablets) / float(COUNT)
    print("\ntablets added per namespace: %.1f" % per_ns)
    print("membership state added per namespace: %.0f bytes" % ((size - base_size) / float(COUNT)))
    print("time per namespace: %.3fs" % ((time.time() - start) / COUNT))


if __name__ == "__main__":
    main()
