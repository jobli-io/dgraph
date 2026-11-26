#!/usr/bin/env python3
"""Proof point for partition namespaces (spike): index changes reach every partition.

The schema lives in the root namespace, but each partition keeps its own index
keys. In the first spike, adding an index rebuilt it for the root namespace
only, and data a partition already held stayed unindexed without any error.

This loads un-indexed data into the root namespace and several partitions,
then changes the schema in the root and checks every namespace:

  1. add term, exact, int, reverse and count indexes
  2. change a tokenizer, which drops one index and builds another
  3. drop an index, change data while it is gone, add it back

usage: proof_index_rebuild.py [partitions, default 5] [nodes per partition, default 2000]
"""

import sys
import time

from spikelib import Checks, Cluster

PARTS = int(sys.argv[1]) if len(sys.argv) > 1 else 5
NODES = int(sys.argv[2]) if len(sys.argv) > 2 else 2000

PLAIN = """
spikeTitle: string .
spikeScore: int .
spikeLink: [uid] .
"""
INDEXED = """
spikeTitle: string @index(term, exact) .
spikeScore: int @index(int) .
spikeLink: [uid] @reverse @count .
"""


def load(c, tag, ns, n):
    for base in range(0, n, 20000):
        lines = []
        for i in range(base, min(base + 20000, n)):
            # "tag" is a word only this namespace uses; "shared" is in every namespace.
            lines.append('_:n%d <spikeTitle> "%s shared item%d" .' % (i, tag, i))
            lines.append('_:n%d <spikeScore> "%d"^^<xs:int> .' % (i, i % 100))
            if i > 0:
                lines.append("_:n%d <spikeLink> _:n0 ." % i)
        c.mutate("\n".join(lines), ns)


def count(c, func, ns):
    rows = c.rows("{ q(func: %s) { count(uid) } }" % func, ns)
    return rows[0]["count"] if isinstance(rows, list) and rows else rows


def alter(c, schema):
    start = time.time()
    r = c.alter(schema)
    if r.get("errors"):
        raise SystemExit("alter failed: %s" % r["errors"])
    c.wait_idle(ops=("opIndexing",))
    return time.time() - start


def main():
    t = Checks()
    with Cluster("ix", 440) as c:
        alter(c, PLAIN)
        spaces = {"root": (None, "tagroot")}
        for k in range(1, PARTS + 1):
            spaces["partition %d" % k] = (c.add_partition(), "tag%d" % k)
        empty = c.add_partition()  # a partition with no data for these predicates
        for name, (ns, tag) in spaces.items():
            load(c, tag, ns, NODES)
        own = list(spaces.values())
        print("loaded %d nodes into the root namespace and each of %d partitions, no indexes" % (NODES, PARTS))

        r = c.rows('{ q(func: anyofterms(spikeTitle, "shared")) { count(uid) } }', own[1][0])
        t.check("before: the term lookup is refused, the predicate has no index", isinstance(r, str), r)

        took = alter(c, INDEXED)
        print("added term, exact, int, reverse and count indexes in %.1fs" % took)
        over90 = sum(1 for i in range(NODES) if i % 100 >= 90)
        for name, (ns, tag) in spaces.items():
            other = own[1][1] if tag != own[1][1] else own[2][1]
            got = [
                count(c, 'anyofterms(spikeTitle, "shared")', ns),
                count(c, 'anyofterms(spikeTitle, "%s")' % tag, ns),
                count(c, 'anyofterms(spikeTitle, "%s")' % other, ns),
                count(c, 'eq(spikeTitle, "%s shared item7")' % tag, ns),
                count(c, "ge(spikeScore, 90)", ns),
                count(c, "has(~spikeLink)", ns),
                count(c, "eq(count(spikeLink), 1)", ns),
                count(c, "eq(count(~spikeLink), %d)" % (NODES - 1), ns),
            ]
            want = [NODES, NODES, 0, 1, over90, 1, NODES - 1, 1]
            t.check("%s: term, exact, int, reverse and count indexes cover existing data" % name,
                    got == want, "%s, want %s" % (got, want))
        t.check("the partition without data stays empty", count(c, 'anyofterms(spikeTitle, "shared")', empty) == 0)

        # A write after the rebuild is indexed as usual.
        p1 = own[1][0]
        c.mutate('_:x <spikeTitle> "%s shared late" .\n_:x <spikeScore> "95"^^<xs:int> .' % own[1][1], p1)
        t.check("a write after the rebuild is indexed in its partition only",
                [count(c, 'anyofterms(spikeTitle, "late")', ns) for ns, _ in own[:3]] == [0, 1, 0])

        # 2. Swap the exact tokenizer for hash: one index dropped, one built.
        took = alter(c, "spikeTitle: string @index(term, hash) .")
        print("changed tokenizer exact -> hash in %.1fs" % took)
        for name, (ns, tag) in list(spaces.items())[:3]:
            got = [count(c, 'eq(spikeTitle, "%s shared item7")' % tag, ns),
                   count(c, 'anyofterms(spikeTitle, "%s")' % tag, ns)]
            want = [1, NODES + (1 if ns == p1 else 0)]
            t.check("%s: lookups work through the new hash index" % name, got == want, "%s, want %s" % (got, want))

        # 3. Drop the index, change data while it is gone, add it back.
        took = alter(c, "spikeTitle: string .")
        print("dropped the index in %.1fs" % took)
        r = c.rows('{ q(func: anyofterms(spikeTitle, "shared")) { uid } }', p1)
        t.check("after the drop the lookup is refused in a partition", isinstance(r, str), r)
        uid = c.rows('{ q(func: has(spikeTitle), first: 1) { uid } }', p1)[0]["uid"]
        c.mutate('<%s> <spikeTitle> "renamed while unindexed" .' % uid, p1)
        took = alter(c, "spikeTitle: string @index(term) .")
        print("added the term index back in %.1fs" % took)
        t.check("the value changed while unindexed is found by its new term",
                count(c, 'anyofterms(spikeTitle, "renamed")', p1) == 1)
        t.check("and no longer by its old terms",
                count(c, 'anyofterms(spikeTitle, "shared")', p1) == NODES)  # NODES + late - renamed
        t.check("other partitions are unchanged",
                count(c, 'anyofterms(spikeTitle, "shared")', own[2][0]) == NODES)

        _, tablets, _ = c.state()
        t.check("no tablets outside the root namespace", all(x.startswith("0-") for x in tablets),
                "%d tablets" % len(tablets))
    sys.exit(t.done())


if __name__ == "__main__":
    main()
