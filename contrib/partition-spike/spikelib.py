"""Shared helpers for the partition-namespace proof scripts (spike).

Starts throwaway dgraph clusters from a spike binary on offset ports and talks
to them over HTTP. Nothing here is specific to one proof.

Ports for offset o: zero grpc 5080+o, zero http 6080+o, alpha internal 7080+o,
alpha http 8080+o, alpha grpc 9080+o.
"""

import json
import os
import shutil
import signal
import subprocess
import time
import urllib.error
import urllib.request

ROOT = os.environ.get("SPIKE_ROOT", "/Users/idowuayoola/Documents/jobli/.spike-cache")
BIN = os.environ.get("SPIKE_BIN", os.path.join(ROOT, "bin", "dgraph-spike"))
NS_HEADER = "x-dgraph-partition-ns"
TELEMETRY = "reports=false; sentry=false;"


class Cluster:
    """One Zero and one or more alphas of a single group."""

    def __init__(self, name, offset, alphas=1, flags="partition-namespaces=true", env=None,
                 extra_alpha_args=None, fresh=True):
        self.name = name
        self.offset = offset
        self.n = alphas
        self.flags = flags
        self.env = dict(os.environ, DGRAPH_TABLET_SIZE_INTERVAL="20s", **(env or {}))
        self.extra = extra_alpha_args or []
        self.dir = os.path.join(ROOT, "run", name)
        self.procs = {}
        if fresh:
            shutil.rmtree(self.dir, ignore_errors=True)
        os.makedirs(self.dir, exist_ok=True)

    # -- addresses
    def zero_grpc(self):
        return "localhost:%d" % (5080 + self.offset)

    def zero_http(self):
        return "http://localhost:%d" % (6080 + self.offset)

    def alpha_http(self, i=0):
        return "http://localhost:%d" % (8080 + self.offset + i)

    def alpha_dir(self, i=0):
        return os.path.join(self.dir, "alpha%d" % i)

    # -- lifecycle
    def _spawn(self, key, args, cwd):
        os.makedirs(cwd, exist_ok=True)
        log = open(os.path.join(self.dir, key + ".log"), "ab")
        self.procs[key] = subprocess.Popen([BIN] + args, cwd=cwd, stdout=log, stderr=log, env=self.env)

    def start_zero(self):
        self._spawn("zero", ["zero", "--my=" + self.zero_grpc(), "-o", str(self.offset),
                             "--replicas", str(self.n), "--telemetry", TELEMETRY,
                             "--logtostderr", "-v=2"], os.path.join(self.dir, "zero"))

    def start_alpha(self, i=0):
        o = self.offset + i
        self._spawn("alpha%d" % i, ["alpha", "--my=localhost:%d" % (7080 + o), "-o", str(o),
                                    "--zero=" + self.zero_grpc(), "--telemetry", TELEMETRY,
                                    "--security", "whitelist=127.0.0.1",
                                    "--feature-flags", self.flags, "--cache", "size-mb=512",
                                    "--logtostderr", "-v=2"] + self.extra, self.alpha_dir(i))

    def stop_alpha(self, i=0, sig=signal.SIGTERM):
        p = self.procs.pop("alpha%d" % i, None)
        if p:
            p.send_signal(sig)
            try:
                p.wait(600)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait(30)

    def start(self, wait=True):
        self.start_zero()
        time.sleep(2)
        for i in range(self.n):
            self.start_alpha(i)
        if wait:
            for i in range(self.n):
                self.wait_ready(i)
        return self

    def stop(self):
        """Stops the alphas, then Zero, and waits for each: a data directory stays locked
        until its alpha has really gone, and after a large load that takes a while."""
        keys = sorted(self.procs, key=lambda k: k == "zero")
        for key in keys:
            p = self.procs.pop(key)
            p.send_signal(signal.SIGTERM)
            try:
                p.wait(600)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait(30)
        time.sleep(1)

    def __enter__(self):
        return self.start()

    def __exit__(self, *exc):
        self.stop()

    def wait_ready(self, i=0, timeout=180):
        """Waits until alpha i serves both DQL and the GraphQL admin API."""
        end = time.time() + timeout
        while time.time() < end:
            try:
                r = self.admin("{ health { status } }", i=i, check=False)
                if r.get("data") and not r.get("errors"):
                    q = self.dql("{ q(func: has(dgraph.type), first: 1) { uid } }", i=i)
                    if "data" in q and not q.get("errors"):
                        return
            except (urllib.error.URLError, ConnectionError, OSError):
                pass
            time.sleep(1)
        raise SystemExit("alpha %d of %s not ready after %ds" % (i, self.name, timeout))

    # -- requests
    def post(self, path, body, ns=None, ctype="application/json", i=0, headers=None, timeout=900):
        data = body if isinstance(body, bytes) else body.encode()
        req = urllib.request.Request(self.alpha_http(i) + path, data=data, method="POST")
        req.add_header("Content-Type", ctype)
        if ns is not None:
            req.add_header(NS_HEADER, str(ns))
        for k, v in (headers or {}).items():
            req.add_header(k, v)
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                return json.loads(resp.read() or b"{}")
        except urllib.error.HTTPError as e:
            raw = e.read() or b"{}"
            try:
                return json.loads(raw)
            except ValueError:
                return {"errors": [{"message": raw.decode(errors="replace")[:300]}]}

    def dql(self, query, ns=None, i=0, headers=None):
        return self.post("/query", query, ns, "application/dql", i, headers)

    def rows(self, query, ns=None, i=0, block="q"):
        r = self.dql(query, ns, i)
        if r.get("errors"):
            return "error: %s" % json.dumps(r["errors"])[:200]
        return (r.get("data") or {}).get(block) or []

    def mutate(self, rdf, ns=None, i=0, delete=False):
        body = "{ %s {\n%s\n} }" % ("delete" if delete else "set", rdf)
        r = self.post("/mutate?commitNow=true", body, ns, "application/rdf", i)
        if r.get("errors"):
            raise SystemExit("mutate error (ns %s): %s" % (ns, json.dumps(r["errors"])[:300]))
        return r

    def alter(self, schema, ns=None, i=0):
        return self.post("/alter", schema, ns, "text/plain", i)

    def admin(self, query, i=0, check=True, variables=None):
        r = self.post("/admin", json.dumps({"query": query, "variables": variables or {}}), i=i)
        if check and r.get("errors"):
            raise SystemExit("admin error: %s" % json.dumps(r["errors"])[:400])
        return r if not check else r["data"]

    def graphql(self, query, ns=None, i=0, headers=None, variables=None):
        return self.post("/graphql", json.dumps({"query": query, "variables": variables or {}}),
                         ns, i=i, headers=headers)

    def add_partition(self):
        return self.admin("mutation { addNamespace { namespaceId } }")["addNamespace"]["namespaceId"]

    def state(self, i=0):
        with urllib.request.urlopen(self.alpha_http(i) + "/state", timeout=300) as resp:
            raw = resp.read()
        s = json.loads(raw)
        tablets = [t for g in s.get("groups", {}).values() for t in g.get("tablets", {})]
        return s, tablets, len(raw)

    def health(self, i=0):
        with urllib.request.urlopen(self.alpha_http(i) + "/health", timeout=30) as resp:
            return json.loads(resp.read())[0]

    def wait_idle(self, i=0, ops=("opIndexing", "opRestore", "opBackup"), timeout=3600):
        """Waits until none of the named background operations is running on alpha i."""
        end = time.time() + timeout
        time.sleep(1)
        while time.time() < end:
            try:
                ongoing = self.health(i).get("ongoing") or []
            except (urllib.error.URLError, ConnectionError, OSError):
                ongoing = ["unreachable"]
            if not any(o in ongoing for o in ops) and "unreachable" not in ongoing:
                return
            time.sleep(1)
        raise SystemExit("alpha %d still busy after %ds" % (i, timeout))

    def rss_mb(self, key):
        p = self.procs.get(key)
        if not p:
            return 0
        out = subprocess.run(["ps", "-o", "rss=", "-p", str(p.pid)], capture_output=True, text=True).stdout
        return int(out.strip() or 0) // 1024

    def heap_mb(self, i=0):
        """Go heap in use on alpha i, which unlike RSS does not count freed memory not yet returned."""
        try:
            with urllib.request.urlopen(self.alpha_http(i) + "/debug/vars", timeout=30) as resp:
                return json.loads(resp.read())["memstats"]["HeapInuse"] >> 20
        except (urllib.error.URLError, ConnectionError, OSError, KeyError, ValueError):
            return 0

    def zero_heap_mb(self):
        """Go heap in use on Zero. Its transaction oracle is the part that grows with writes."""
        try:
            with urllib.request.urlopen(self.zero_http() + "/debug/vars", timeout=30) as resp:
                return json.loads(resp.read())["memstats"]["HeapInuse"] >> 20
        except (urllib.error.URLError, ConnectionError, OSError, KeyError, ValueError):
            return 0

    def log_count(self, key, needle):
        n = 0
        with open(os.path.join(self.dir, key + ".log"), "rb") as f:
            for line in f:
                if needle.encode() in line:
                    n += 1
        return n


class Checks:
    def __init__(self):
        self.results = []

    def check(self, name, ok, detail=""):
        self.results.append(bool(ok))
        d = (" -> %s" % detail) if detail != "" else ""
        print("%s  %s%s" % ("PASS" if ok else "FAIL", name, d[:230]), flush=True)
        return ok

    def done(self):
        passed = sum(self.results)
        print("\n%d/%d passed" % (passed, len(self.results)))
        return 0 if passed == len(self.results) else 1
