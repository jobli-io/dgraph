#!/usr/bin/env python3
"""Proof point for partition namespaces (spike): many partitions with data, replicated.

Runs one Zero and three alphas in a single group (three replicas), loads
Jobli's DQL schema into the root namespace, then grows the cluster in steps to
the target number of partitions. Each holds a few nodes written through
Jobli's real Candidate predicates, with their hash, trigram and fulltext
indexes, and one partition in 500 holds 2,000, the way a few workspaces are
much larger than the rest. At each step it records what the
design said must stay flat (Zero's tablet list, the size of the membership
state, memory and idle CPU of Zero and the alphas) and what a tenant sees
(write and query latency, and that a sample of partitions holds exactly its
own data on every replica).

Before the last step one alpha is stopped, so that when it comes back it has to
catch up from the leader. Afterwards the script adds an index, takes a backup,
restores it, and deletes some partitions, checking a sample each time.

usage: proof_scale.py [steps, default 1000,10000,50000] [nodes per partition, default 20]
       [schema.dql, default the dumped Jobli schema]
"""

import concurrent.futures
import glob
import os
import random
import shutil
import statistics
import subprocess
import sys
import time

from spikelib import ROOT, Checks, Cluster

STEPS = [int(x) for x in (sys.argv[1] if len(sys.argv) > 1 else "1000,10000,50000").split(",")]
K = int(sys.argv[2]) if len(sys.argv) > 2 else 20
SCHEMA_FILE = sys.argv[3] if len(sys.argv) > 3 else os.path.join(ROOT, "jobli_schema.dql")
ALPHAS = 3
# SPIKE_CONTROL=1 writes the same nodes into the root namespace of a cluster with
# partition namespaces off, to tell what the partitions cost from what the writes cost.
CONTROL = os.environ.get("SPIKE_CONTROL") == "1"
HEAVY_EVERY, HEAVY = 500, 2000
size = {}  # namespace -> number of candidates it holds
THREADS = 8
SAMPLE = 150
rnd = random.Random(11)


def rdf(ns, lo=0, hi=None):
    lines = []
    for i in range(lo, K if hi is None else hi):
        n = "_:c%d" % i
        lines.append('%s <dgraph.type> "Candidate" .' % n)
        lines.append('%s <Candidate.firstName> "First%d" .' % (n, i))
        lines.append('%s <Candidate.lastName> "Tenant%d" .' % (n, ns))
        lines.append('%s <Candidate.email> "c%d@ws%d.example" .' % (n, i, ns))
        lines.append('%s <Candidate.summary> "candidate %d of workspace %d likes graphs" .' % (n, i, ns))
        lines.append('%s <spikeTag> "tag%d" .' % (n, ns))
    return "\n".join(lines)


def cpu_seconds(pid):
    out = subprocess.run(["ps", "-o", "time=", "-p", str(pid)], capture_output=True, text=True).stdout.strip()
    if not out:
        return 0.0
    parts = out.replace("-", ":").split(":")
    secs = 0.0
    for p in parts:
        secs = secs * 60 + float(p)
    return secs


def idle_cpu(c, keys, window=15):
    before = {k: cpu_seconds(c.procs[k].pid) for k in keys if k in c.procs}
    time.sleep(window)
    return {k: 100.0 * (cpu_seconds(c.procs[k].pid) - v) / window for k, v in before.items()}


def pct(values, p):
    values = sorted(values)
    return values[min(len(values) - 1, int(len(values) * p))] if values else 0


def verify(c, sample, alpha, all_ns):
    """Each sampled partition holds exactly its own K candidates, seen through data keys and indexes."""
    bad, lat = [], []
    for ns in sample:
        other = all_ns[(all_ns.index(ns) + 1) % len(all_ns)]
        q = ('{ n(func: type(Candidate)) { count(uid) } '
             'own(func: eq(Candidate.email, "c3@ws%d.example")) { count(uid) } '
             'other(func: eq(Candidate.email, "c3@ws%d.example")) { count(uid) } '
             'text(func: alloftext(Candidate.summary, "workspace %d")) { count(uid) } }' % (ns, other, ns))
        r = c.dql(q, ns, i=alpha)
        d = r.get("data") or {}
        got = [(d.get(b) or [{}])[0].get("count") for b in ("n", "own", "other", "text")]
        if got != [size[ns], 1, 0, size[ns]]:
            bad.append((ns, got if not r.get("errors") else r["errors"][0]["message"][:80]))
        else:
            lat.append(r["extensions"]["server_latency"]["processing_ns"] / 1000.0)
    return bad, lat


def main():
    t = Checks()
    # Snapshot as soon as the log is long enough. The default also waits 30 minutes
    # between snapshots, which would let the stopped replica catch up from the log alone.
    extra = ["--raft", "snapshot-after-entries=2000; snapshot-after-duration=0"]
    flags = "partition-namespaces=false" if CONTROL else "partition-namespaces=true"
    c = Cluster("zc" if CONTROL else "sc", 550 if CONTROL else 500, alphas=ALPHAS, flags=flags,
                extra_alpha_args=extra).start()
    rows = []
    try:
        r = c.alter(open(SCHEMA_FILE).read() + "\nspikeTag: string .\n")
        t.check("Jobli DQL schema loaded into the root namespace", not r.get("errors"), str(r.get("errors", ""))[:200])
        c.mutate(rdf(0))
        for i in range(ALPHAS):
            c.wait_ready(i)
        _, tablets0, state0 = c.state()

        def measure(n, extra_cols):
            _, tablets, state_bytes = c.state()
            idle = idle_cpu(c, ["zero"] + ["alpha%d" % i for i in range(ALPHAS)])
            alphas_rss = [c.rss_mb("alpha%d" % i) for i in range(ALPHAS) if "alpha%d" % i in c.procs]
            alphas_idle = [v for k, v in idle.items() if k != "zero"]
            row = dict(partitions=n, tablets=len(tablets), state_kb=state_bytes / 1024.0,
                       zero_rss=c.rss_mb("zero"), zero_heap=c.zero_heap_mb(), alpha_rss=max(alphas_rss),
                       zero_idle=idle["zero"],
                       alpha_idle=max(alphas_idle), **extra_cols)
            rows.append(row)
            print("  %s" % {k: (round(v, 2) if isinstance(v, float) else v) for k, v in row.items()}, flush=True)
            return row

        print("baseline, no partitions:", flush=True)
        measure(0, {})

        all_ns = []
        for step_no, target in enumerate(STEPS):
            last = step_no == len(STEPS) - 1
            if last and len(STEPS) > 1:
                c.stop_alpha(ALPHAS - 1)
                print("stopped alpha %d before the last step" % (ALPHAS - 1), flush=True)

            start = time.time()
            new = []
            while len(all_ns) + len(new) < target:
                # The control writes the same nodes into the root namespace of a stock cluster.
                new.append(len(all_ns) + len(new) + 1 if CONTROL else c.add_partition())
            create_ms = (time.time() - start) * 1000 / max(1, len(new))

            def write(ns):
                size[ns] = K
                s = time.time()
                c.mutate(rdf(ns), None if CONTROL else ns)
                took = (time.time() - s) * 1000
                if ns % HEAVY_EVERY == 0:
                    for lo in range(K, HEAVY, 500):
                        c.mutate(rdf(ns, lo, min(lo + 500, HEAVY)), None if CONTROL else ns)
                    size[ns] = HEAVY
                return took

            start = time.time()
            with concurrent.futures.ThreadPoolExecutor(THREADS) as pool:
                write_ms = list(pool.map(write, new))
            load_s = time.time() - start
            all_ns.extend(new)
            heavy = [ns for ns in new if size[ns] == HEAVY]
            print("%d partitions: created %d in %.2f ms each, loaded %d nodes each (%d of them %d) in %.0fs, "
                  "%d writers; %d nodes in all" % (len(all_ns), len(new), create_ms, K, len(heavy), HEAVY,
                                                  load_s, THREADS, sum(size.values())), flush=True)

            if CONTROL:
                measure(len(all_ns), dict(create_ms=0.0, write_p50_ms=statistics.median(write_ms),
                                          write_p95_ms=pct(write_ms, 0.95), query_p50_us=0.0, query_p95_us=0.0))
                continue
            sample = rnd.sample(all_ns, min(SAMPLE, len(all_ns))) + heavy[:5]
            lat_all = []
            for i in range(ALPHAS):
                if "alpha%d" % i not in c.procs:
                    continue
                bad, lat = verify(c, sample, i, all_ns)
                lat_all.extend(lat)
                t.check("%d partitions: %d sampled hold exactly their own data, read from alpha %d"
                        % (len(all_ns), len(sample), i), not bad, bad[:3])
            measure(len(all_ns), dict(create_ms=create_ms, write_p50_ms=statistics.median(write_ms),
                                      write_p95_ms=pct(write_ms, 0.95), query_p50_us=statistics.median(lat_all),
                                      query_p95_us=pct(lat_all, 0.95)))

            if last and len(STEPS) > 1:
                # The stopped replica missed every partition created in this step. Wait for
                # the group to snapshot and trim its log first, so the replica cannot simply
                # replay the log: it has to be sent the data. The check runs once a minute.
                lagging = ALPHAS - 1
                running = ["alpha%d" % i for i in range(ALPHAS - 1)]
                seen = sum(c.log_count(k, "Proposing snapshot") for k in running)
                deadline = time.time() + 240
                while time.time() < deadline and sum(c.log_count(k, "Proposing snapshot") for k in running) == seen:
                    time.sleep(5)
                start = time.time()
                c.start_alpha(lagging)
                c.wait_ready(lagging, timeout=1800)
                missed = rnd.sample(new, min(SAMPLE, len(new)))
                deadline = time.time() + 1800
                bad = [None]
                while time.time() < deadline:
                    bad, _ = verify(c, missed, lagging, all_ns)
                    if not bad:
                        break
                    time.sleep(5)
                snaps = c.log_count("alpha%d" % lagging, "Populated snapshot")
                t.check("the replica that was down has the partitions it missed (%.0fs after restart, "
                        "%d snapshot(s) streamed from the leader)" % (time.time() - start, snaps), not bad, bad[:3])
                t.check("it caught up through a snapshot, not only the log", snaps > 0, "%d snapshots" % snaps)

        if CONTROL:
            print("control: %d nodes written to the root namespace of a cluster with partition namespaces off"
                  % sum(size.values()))
            for r in rows:
                print("  %-8d tenants' worth of writes: Zero RSS %5d MB, Zero heap %5d MB, alpha RSS %5d MB" % (
                    r["partitions"], r["zero_rss"], r["zero_heap"], r["alpha_rss"]))
            return
        n = len(all_ns)
        sample = rnd.sample([ns for ns in all_ns if size[ns] == K], min(SAMPLE, n))
        big = [ns for ns in all_ns if size[ns] == HEAVY][:5]

        def mem(label):
            print("  %s: alpha RSS %s MB, Go heap in use %s MB" % (
                label, [c.rss_mb("alpha%d" % i) for i in range(ALPHAS)],
                [c.heap_mb(i) for i in range(ALPHAS)]), flush=True)
        mem("after the load")
        t.check("Zero's tablet list did not grow", len(c.state()[1]) == len(tablets0),
                "%d -> %d" % (len(tablets0), len(c.state()[1])))
        growth = c.state()[2] - state0
        t.check("the membership state grew by less than 1 byte per partition",
                growth < n, "%+d bytes for %d partitions" % (growth, n))

        # -- an index added in the root namespace has to reach every partition
        start = time.time()
        r = c.alter("spikeTag: string @index(exact) .")
        for i in range(ALPHAS):
            c.wait_idle(i, ops=("opIndexing",), timeout=7200)
        took = time.time() - start
        ok = True
        for i in range(ALPHAS):
            for ns in sample[:50] + big:
                other = all_ns[(all_ns.index(ns) + 1) % n]
                got = [c.rows('{ q(func: eq(spikeTag, "tag%d")) { count(uid) } }' % x, ns, i=i) for x in (ns, other)]
                if got != [[{"count": size[ns]}], [{"count": 0}]]:
                    ok = False
                    print("    index mismatch alpha %d ns %d: %s" % (i, ns, str(got)[:150]))
                    break
        t.check("index added over %d partitions in %.0fs covers a sample on every replica" % (n, took),
                ok and not r.get("errors"), r.get("errors", ""))
        mem("after the index build")

        # -- and so does dropping it, and building it a second time over the deletions
        start = time.time()
        c.alter("spikeTag: string .")
        for i in range(ALPHAS):
            c.wait_idle(i, ops=("opIndexing",), timeout=7200)
        drop_s = time.time() - start
        r = c.rows('{ q(func: eq(spikeTag, "tag%d")) { count(uid) } }' % sample[0], sample[0])
        t.check("index dropped over %d partitions in %.0fs; the lookup is refused" % (n, drop_s),
                isinstance(r, str), r)
        s0 = time.time()
        c.mutate('_:x <spikeTag> "added-while-unindexed" .', sample[0])
        write_after_drop = (time.time() - s0) * 1000
        start = time.time()
        c.alter("spikeTag: string @index(exact) .")
        for i in range(ALPHAS):
            c.wait_idle(i, ops=("opIndexing",), timeout=7200)
        again_s = time.time() - start
        got = [c.rows('{ q(func: eq(spikeTag, "%s")) { count(uid) } }' % v, sample[0], i=ALPHAS - 1)
               for v in ("tag%d" % sample[0], "added-while-unindexed", "tag%d" % sample[1])]
        t.check("index built again in %.0fs: old values, the value added meanwhile, and nothing else" % again_s,
                got == [[{"count": K}], [{"count": 1}], [{"count": 0}]], got)
        c.mutate('<%s> <spikeTag> * .' % c.rows('{ q(func: eq(spikeTag, "added-while-unindexed")) { uid } }',
                                               sample[0])[0]["uid"], sample[0], delete=True)
        print("  first write after the drop took %.0f ms" % write_after_drop, flush=True)

        # -- backup, damage, restore
        bdir = os.path.join(ROOT, "run", "scale-backup")
        shutil.rmtree(bdir, ignore_errors=True)
        os.makedirs(bdir)
        start = time.time()
        c.admin('mutation { backup(input: {destination: "%s", forceFull: true}) { response { code } } }' % bdir)
        c.wait_idle(ops=("opBackup",), timeout=7200)
        backup_s = time.time() - start
        bsize = sum(os.path.getsize(f) for f in glob.glob(os.path.join(bdir, "**", "*.backup"), recursive=True))
        t.check("backup of %d partitions in %.0fs" % (n, backup_s), bsize > n * K * 50, "%d MB" % (bsize >> 20))
        mem("after the backup")
        victims = sample[:20]
        for ns in victims:
            c.alter('{"drop_op": "DATA"}', ns)
        bad, _ = verify(c, victims, 0, all_ns)
        t.check("damage: 20 partitions emptied", len(bad) == len(victims))
        mem("after emptying 20 partitions")
        start = time.time()
        c.admin('mutation { restore(input: {location: "%s"}) { code message } }' % bdir)
        for i in range(ALPHAS):
            c.wait_idle(i, ops=("opRestore",), timeout=7200)
            c.wait_ready(i, timeout=600)
        restore_s = time.time() - start
        for i in range(ALPHAS):
            bad, _ = verify(c, sample, i, all_ns)
            t.check("restore in %.0fs: %d sampled partitions match again on alpha %d" % (restore_s, len(sample), i),
                    not bad, bad[:3])

        mem("after the restore")
        bad, _ = verify(c, big, 2, all_ns)
        t.check("the %d large partitions sampled match after the restore" % len(big), not bad, bad[:3])

        # -- per-tenant delete
        gone, kept = sample[:30], sample[30:]
        start = time.time()
        for ns in gone:
            c.admin("mutation { deleteNamespace(input: {namespaceId: %d}) { namespaceId } }" % ns)
        delete_ms = (time.time() - start) * 1000 / len(gone)
        left = [c.rows("{ q(func: type(Candidate)) { count(uid) } }", ns) for ns in gone]
        t.check("deleted %d partitions in %.0f ms each; their data is gone" % (len(gone), delete_ms),
                all(x == [{"count": 0}] or isinstance(x, str) for x in left), str(left[:2]))
        bad, _ = verify(c, kept, 1, all_ns)
        t.check("the other sampled partitions are untouched", not bad, bad[:3])

        time.sleep(45)  # let two tablet-size reports go by with the full data set
        bad, _ = verify(c, kept, 0, all_ns)
        t.check("data intact after further tablet-size reports", not bad, bad[:3])
        t.check("Zero never told a group to delete a predicate",
                c.log_count("zero", "Sending delete instruction") == 0)
        _, tablets, _ = c.state()
        t.check("no tablets outside the root namespace", all(x.startswith("0-") for x in tablets),
                "%d tablets" % len(tablets))
        mem("at the end")
        measure(n, {})
        # Tables only: Badger preallocates its memtable and value-log files while it runs.
        disk = sum(os.path.getsize(f) for f in glob.glob(os.path.join(c.alpha_dir(0), "p", "*.sst")))
        print("alpha 0 tables on disk: %d MB" % (disk >> 20))
    finally:
        c.stop()

    print("\n%-10s %8s %10s %9s %10s %10s %10s %11s %10s %10s %10s" % (
        "partitions", "tablets", "/state KB", "zero MB", "zero heap", "alpha MB", "zero CPU%", "alpha CPU%",
        "create ms", "write p50", "query p50"))
    for r in rows:
        print("%-10d %8d %10.0f %9d %10d %10d %10.1f %11.1f %10s %10s %10s" % (
            r["partitions"], r["tablets"], r["state_kb"], r["zero_rss"], r["zero_heap"], r["alpha_rss"], r["zero_idle"],
            r["alpha_idle"], "%.2f" % r["create_ms"] if "create_ms" in r else "-",
            "%.1f ms" % r["write_p50_ms"] if "write_p50_ms" in r else "-",
            "%.0f us" % r["query_p50_us"] if "query_p50_us" in r else "-"))
    sys.exit(t.done())


if __name__ == "__main__":
    main()
