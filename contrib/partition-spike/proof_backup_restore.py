#!/usr/bin/env python3
"""Proof point for partition namespaces (spike): backup and restore round trip.

A backup picks keys by Zero's tablet list, and partitions have no tablets of
their own, so the first spike's backups silently left partition data out. This
takes a full and an incremental backup of a cluster with data in the root
namespace and in three partitions, then restores it three ways and compares
every namespace with what was backed up:

  1. online, into the same cluster after its data has been damaged
  2. online, into a second empty cluster
  3. offline (dgraph restore), into a third cluster's data directory

It also checks that a cluster without partition namespaces refuses the backup.

usage: proof_backup_restore.py
"""

import glob
import json
import os
import shutil
import subprocess
import sys

from spikelib import BIN, ROOT, Checks, Cluster

SCHEMA = """
spikeName: string @index(exact, term) .
spikeAge: int @index(int) .
spikeFriend: [uid] @reverse @count .
type SpikePerson {
  spikeName
  spikeAge
  spikeFriend
}
"""


def people(tag, n, start=0):
    lines = []
    for i in range(start, start + n):
        lines.append('_:p%d <spikeName> "%s person %d" .' % (i, tag, i))
        lines.append('_:p%d <spikeAge> "%d"^^<xs:int> .' % (i, 18 + i % 60))
        lines.append('_:p%d <dgraph.type> "SpikePerson" .' % i)
        if i > start:
            lines.append("_:p%d <spikeFriend> _:p%d ." % (i, i - 1))
    return "\n".join(lines)


def fingerprint(c, ns):
    """Everything a namespace holds, read through data keys and through each index."""
    data = c.rows("{ q(func: has(spikeName), orderasc: spikeName, first: 100000) "
                  "{ uid spikeName spikeAge spikeFriend { uid } } }", ns)
    return {
        "data": data,
        "term": c.rows('{ q(func: anyofterms(spikeName, "person")) { count(uid) } }', ns),
        "int": c.rows("{ q(func: ge(spikeAge, 70)) { count(uid) } }", ns),
        "reverse": c.rows("{ q(func: has(~spikeFriend)) { count(uid) } }", ns),
        "count": c.rows("{ q(func: eq(count(spikeFriend), 1)) { count(uid) } }", ns),
        "type": c.rows("{ q(func: type(SpikePerson)) { count(uid) } }", ns),
    }


def summary(fp):
    return {k: (len(v) if k == "data" and isinstance(v, list) else v) for k, v in fp.items()}


def same(got, want):
    """Equal, and neither side is an error: two identical errors are not a match."""
    return got == want and not any(isinstance(v, str) for v in got.values())


def restore(c, location):
    r = c.admin('mutation { restore(input: {location: "%s"}) { code message } }' % location, check=False)
    if r.get("errors"):
        return r["errors"][0]["message"]
    c.wait_idle(ops=("opRestore",))
    c.wait_ready()
    return None


def main():
    t = Checks()
    bdir = os.path.join(ROOT, "run", "backup-proof-data")
    shutil.rmtree(bdir, ignore_errors=True)
    os.makedirs(bdir)

    a = Cluster("bk-a", 400).start()
    try:
        r = a.alter(SCHEMA)
        t.check("schema applied to the root namespace", not r.get("errors"), r.get("errors", ""))
        p1, p2, p3 = a.add_partition(), a.add_partition(), a.add_partition()
        a.mutate(people("root", 20))
        a.mutate(people("one", 5000), p1)
        a.mutate(people("two", 50), p2)
        _, tablets_before, _ = a.state()

        a.admin('mutation { backup(input: {destination: "%s", forceFull: true}) { response { code } } }' % bdir)
        a.wait_idle(ops=("opBackup",))
        manifest = json.load(open(os.path.join(bdir, "manifest.json")))["Manifests"]
        t.check("full backup records that namespaces are partitions",
                manifest[-1].get("partition_namespaces") is True, manifest[-1].get("type"))
        groups = manifest[-1]["groups"]
        listed = [p for g in groups.values() for p in g]
        t.check("the manifest still lists only root-namespace predicates",
                all(p.startswith("0-") for p in listed), "%d predicates" % len(listed))

        # Changes after the full backup, so the incremental one has work in every namespace.
        a.mutate(people("three", 30), p3)
        a.mutate(people("one-late", 10, start=9000), p1)
        r = a.alter('{"drop_op": "DATA"}', p2)
        t.check("drop data in partition 2 between backups", not r.get("errors"), r.get("errors", ""))
        a.mutate(people("two-again", 7), p2)
        a.mutate(people("root-late", 3, start=500))
        a.admin('mutation { backup(input: {destination: "%s"}) { response { code } } }' % bdir)
        a.wait_idle(ops=("opBackup",))
        manifest = json.load(open(os.path.join(bdir, "manifest.json")))["Manifests"]
        t.check("incremental backup taken", manifest[-1]["type"] == "incremental", manifest[-1]["type"])
        size = sum(os.path.getsize(f) for f in glob.glob(os.path.join(bdir, "**", "*.backup"), recursive=True))
        t.check("the backup files hold the partitions' data", size > 150000, "%d KB" % (size // 1024))

        spaces = {"root": None, "partition 1": p1, "partition 2": p2, "partition 3": p3}
        want = {name: fingerprint(a, ns) for name, ns in spaces.items()}
        print("backed up: %s" % {k: summary(v)["data"] for k, v in want.items()})
        t.check("partition 1 has 5,010 people before the damage", len(want["partition 1"]["data"]) == 5010)
        t.check("partition 2 has only the 7 written after its drop", len(want["partition 2"]["data"]) == 7)

        # 1. Damage the cluster, then restore in place.
        a.alter('{"drop_op": "DATA"}', p1)
        a.mutate(people("junk", 40, start=20000), p3)
        a.mutate(people("junk", 5, start=20000))
        t.check("damage: partition 1 emptied", len(fingerprint(a, p1)["data"]) == 0)
        err = restore(a, bdir)
        t.check("online restore into the same cluster", err is None, err or "")
        for name, ns in spaces.items():
            got = fingerprint(a, ns)
            t.check("  %s matches the backup" % name, same(got, want[name]), summary(got))
        _, tablets_after, _ = a.state()
        t.check("Zero's tablet list is the same as before the backup",
                sorted(tablets_after) == sorted(tablets_before),
                "%d -> %d" % (len(tablets_before), len(tablets_after)))
        newp = a.add_partition()
        t.check("a partition created after the restore gets a fresh ID", newp > p3, "%d > %d" % (newp, p3))
        a.mutate(people("after", 2, start=30000), p1)
        t.check("a restored partition accepts indexed writes",
                a.rows('{ q(func: anyofterms(spikeName, "after")) { count(uid) } }', p1) == [{"count": 2}])
    finally:
        a.stop()

    # 2. A second, empty cluster.
    b = Cluster("bk-b", 410).start()
    try:
        err = restore(b, bdir)
        t.check("online restore into an empty second cluster", err is None, err or "")
        for name, ns in spaces.items():
            got = fingerprint(b, ns)
            t.check("  %s matches the backup" % name, same(got, want[name]), summary(got))
        _, tablets_b, _ = b.state()
        t.check("  no tablets outside the root namespace",
                all(x.startswith("0-") for x in tablets_b), "%d tablets" % len(tablets_b))
    finally:
        b.stop()

    # 3. Offline restore into a new data directory, then start an alpha on it.
    c = Cluster("bk-c", 420)
    c.start_zero()
    try:
        import time
        time.sleep(5)
        out = subprocess.run([BIN, "restore", "-p", os.path.join(c.alpha_dir(0), "p"), "-l", bdir,
                              "-z", c.zero_grpc()], capture_output=True, text=True, cwd=c.dir)
        # The offline tool writes one directory per group: p/p1 for group 1.
        p_root = os.path.join(c.alpha_dir(0), "p")
        if os.path.isdir(os.path.join(p_root, "p1")):
            tmp = p_root + ".restored"
            os.rename(os.path.join(p_root, "p1"), tmp)
            shutil.rmtree(p_root)
            os.rename(tmp, p_root)
        t.check("offline restore ran", out.returncode == 0, (out.stderr or out.stdout)[-200:] if out.returncode else "")
        c.start_alpha(0)
        c.wait_ready(0)
        for name, ns in spaces.items():
            got = fingerprint(c, ns)
            t.check("  %s matches the backup" % name, same(got, want[name]), summary(got))
    finally:
        c.stop()

    # 4. A cluster that does not treat namespaces as partitions must not take this backup:
    #    it would report the partitions' tables to Zero and be told to delete them.
    d = Cluster("bk-d", 430, flags="partition-namespaces=false").start()
    try:
        err = restore(d, bdir)
        t.check("a cluster without partition namespaces refuses the backup",
                err is not None and "partition" in err, err or "accepted")
    finally:
        d.stop()

    sys.exit(t.done())


if __name__ == "__main__":
    main()
