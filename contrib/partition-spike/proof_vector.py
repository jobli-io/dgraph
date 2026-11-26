#!/usr/bin/env python3
"""Proof point for partition namespaces (spike): HNSW vector search per partition.

Jobli's schema has float32vector predicates with HNSW indexes. An HNSW index
keeps its graph in supporting predicates that Zero never lists as tablets, so
it is a separate path from the ordinary indexes. This checks that each
partition gets its own graph: a similarity search returns only the caller's
vectors, whichever namespace's vectors the probe is closest to, and an index
added after the data exists is built in every partition.

usage: proof_vector.py
"""

import random
import sys

from spikelib import Checks, Cluster

random.seed(3)
AXES = {"root": [1, 0, 0, 0], "A": [0, 1, 0, 0], "B": [0, 0, 1, 0]}


def vec(base):
    return "[" + ", ".join("%.4f" % (b + random.uniform(-0.02, 0.02)) for b in base) + "]"


def nearest(c, pred, probe, ns, k=3):
    r = c.dql('{ q(func: similar_to(%s, %d, "%s")) { spikeLabel } }' % (pred, k, vec(probe)), ns)
    if r.get("errors"):
        return "error: %s" % r["errors"][0]["message"][:120]
    return [x["spikeLabel"] for x in (r.get("data") or {}).get("q") or []]


def main():
    t = Checks()
    with Cluster("vec", 460) as c:
        c.alter('spikeVec: float32vector @index(hnsw(metric:"cosine")) .\n'
                "spikeVecLate: float32vector .\nspikeLabel: string @index(exact) .")
        spaces = {"root": None, "A": c.add_partition(), "B": c.add_partition()}
        empty = c.add_partition()
        for tag, ns in spaces.items():
            lines = []
            for i in range(200):
                lines.append('_:n%d <spikeVec> "%s" .' % (i, vec(AXES[tag])))
                lines.append('_:n%d <spikeVecLate> "%s" .' % (i, vec(AXES[tag])))
                lines.append('_:n%d <spikeLabel> "%s-%d" .' % (i, tag, i))
            c.mutate("\n".join(lines), ns)

        for tag, ns in spaces.items():
            for like, probe in AXES.items():
                got = nearest(c, "spikeVec", probe, ns)
                ok = isinstance(got, list) and len(got) == 3 and all(x.startswith(tag + "-") for x in got)
                t.check("namespace %s, probe nearest to %s's vectors: only %s's come back" % (tag, like, tag), ok, got)
        t.check("a partition with no vectors finds none", nearest(c, "spikeVec", AXES["A"], empty) == [])

        r = c.alter('spikeVecLate: float32vector @index(hnsw(metric:"cosine")) .')
        c.wait_idle(ops=("opIndexing",))
        t.check("HNSW index added after the data exists", not r.get("errors"), r.get("errors", ""))
        for tag, ns in spaces.items():
            got = nearest(c, "spikeVecLate", AXES["A"], ns)
            ok = isinstance(got, list) and len(got) == 3 and all(x.startswith(tag + "-") for x in got)
            t.check("  namespace %s is searchable through the late index" % tag, ok, got)

        _, tablets, _ = c.state()
        t.check("no tablets outside the root namespace", all(x.startswith("0-") for x in tablets),
                "%d tablets" % len(tablets))
    sys.exit(t.done())


if __name__ == "__main__":
    main()
