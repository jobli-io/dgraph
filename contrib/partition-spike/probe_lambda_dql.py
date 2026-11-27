"""Lambda's direct database calls: which namespace they land in, from the real lambda
runtime (spike probe, not a proof: it prints and asserts nothing).

A lambda resolver reaches the alpha two ways. `graphql()` carries the caller's token.
`dql.query()` and `dql.mutate()` are sent by the runtime with a fixed token from its
environment, whoever the caller is, so under the claim-based resolver they land in the
root namespace and not in the caller's partition.

graph/lambda/src/_dql.js puts a `dql` that carries the caller's token in place of the
runtime's, for every resolver, when LAMBDA_DQL_AS_CALLER=true. This runs the stock
dgraph/dgraph-lambda image with a bundle of Jobli's own _setup.js (which installs it)
and two resolvers written for this probe, one that counts candidates and workspaces by
DQL and one that writes a record, and calls them as the alpha would:

  - with the switch off and on
  - as the workspace owner, and with no token

Nothing of Jobli's lambda other than _setup.js is in the bundle, so nothing is sent
anywhere but the proof cluster. What it writes is removed.

  SPIKE_SUB=... SPIKE_EMAIL=... python3 probe_lambda_dql.py
"""
import http.server
import json
import os
import subprocess
import threading
import time
import urllib.request

import proof_real as P
import probe_phase1_writes as W

LAMBDA_SRC = os.path.join(P.JOBLI, "graph", "lambda")
WORK = os.path.join(P.ROOT, "run", "lambda-dql")
IMAGE = "dgraph/dgraph-lambda:latest"
NAME = "spike-lambda-dql"
PORT = 8787
COUNT = "{ c(func: type(Candidate)) { count(uid) } w(func: type(Workspace)) { count(uid) } }"

ENTRY = """
self.addGraphQLResolvers({
    "Query.spikeWhere": async ({ dql }) => {
        const r = await dql.query(%s);
        return JSON.stringify(r.data || r);
    },
    "Mutation.spikeWrite": async ({ dql }) => {
        const r = await dql.mutate('{ set { _:x <dgraph.type> "SpikeDqlProbe" . } }');
        return JSON.stringify((r.data && r.data.uids) || r);
    },
});
""" % json.dumps(COUNT)

CONFIG = """
const config = require(%(repo)s);
config.entry = { main: [%(setup)s, %(entry)s] };
config.output = Object.assign({}, config.output, { path: %(out)s });
config.resolve = Object.assign({}, config.resolve, { modules: [%(modules)s] });
config.resolveLoader = { modules: [%(modules)s] };
module.exports = config;
"""


def build():
    """Bundles Jobli's _setup.js with the two probe resolvers, outside the repository."""
    os.makedirs(WORK, exist_ok=True)
    entry = os.path.join(WORK, "entry.js")
    open(entry, "w").write(ENTRY)
    q = json.dumps
    open(os.path.join(WORK, "webpack.probe.js"), "w").write(CONFIG % {
        "repo": q(os.path.join(LAMBDA_SRC, "webpack.config.js")), "setup": q(os.path.join(LAMBDA_SRC, "src", "_setup.js")),
        "entry": q(entry), "out": q(os.path.join(WORK, "dist")), "modules": q(os.path.join(LAMBDA_SRC, "node_modules"))})
    env = dict(os.environ, NODE_OPTIONS="--openssl-legacy-provider")
    r = subprocess.run(["npx", "webpack", "--mode", "development", "--target=webworker", "--config", os.path.join(WORK, "webpack.probe.js")],
                       cwd=LAMBDA_SRC, env=env, capture_output=True, text=True)
    out = os.path.join(WORK, "dist", "main.js")
    if r.returncode or not os.path.exists(out):
        raise SystemExit("the bundle did not build:\n%s\n%s" % (r.stdout[-1500:], r.stderr[-1500:]))
    return out


def stop():
    subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)


def start(bundle, alpha, as_caller):
    stop()
    cmd = ["docker", "run", "-d", "--name", NAME, "-p", "127.0.0.1:%d:8686" % PORT,
           "--add-host", "host.docker.internal:host-gateway",
           "-e", "DGRAPH_URL=%s" % alpha, "-e", "LAMBDA_DQL_AS_CALLER=%s" % ("true" if as_caller else "false"),
           "-v", "%s:/app/script/script.js:ro" % bundle, IMAGE]
    r = subprocess.run(cmd, capture_output=True, text=True)
    if r.returncode:
        raise SystemExit("the lambda container did not start: %s" % r.stderr[-600:])
    end = time.time() + 90
    while time.time() < end:
        try:
            call("Query.spikeWhere", None)
            return
        except Exception:
            time.sleep(1)
    logs = subprocess.run(["docker", "logs", "--tail", "30", NAME], capture_output=True, text=True)
    raise SystemExit("the lambda container did not answer:\n%s\n%s" % (logs.stdout[-1500:], logs.stderr[-1500:]))


def call(resolver, token):
    """Calls a resolver the way the alpha does: the caller's token goes in the body."""
    body = {"resolver": resolver, "args": {}}
    if token:
        body["authHeader"] = {"key": P.HEADER, "value": token}
    req = urllib.request.Request("http://127.0.0.1:%d/graphql-worker" % PORT, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            raw = resp.read().decode()
    except urllib.error.HTTPError as e:
        return "refused or failed: %s %s" % (e.code, e.read().decode()[:200])
    try:
        return json.loads(json.loads(raw))
    except (ValueError, TypeError):
        return raw[:300]


def counts(answer):
    if not isinstance(answer, dict) or "c" not in answer:
        return str(answer)[:200]
    return "%s candidates, %s workspaces" % (answer["c"][0]["count"], answer["w"][0]["count"])


def main():
    if not (P.SUB and P.EMAIL):
        raise SystemExit("set SPIKE_SUB and SPIKE_EMAIL to the userId and email of the owner of %s" % P.WS)
    server = http.server.ThreadingHTTPServer(("127.0.0.1", P.LAMBDA_PORT), W.Stub)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    bundle = build()
    print("bundle: %s (%d KB)" % (bundle, os.path.getsize(bundle) // 1024))
    with P.cluster(P.ON) as c:
        c.wait_ready(0, timeout=900)
        try:
            tok = P.token()
            # the alpha only knows the token's header once its GraphQL schema is loaded
            end = time.time() + 120
            while time.time() < end:
                if P.gql(c, tok, "{ aggregateCandidate { count } }", {}).get("data"):
                    break
                time.sleep(0.5)
            alpha = c.alpha_http(0).replace("localhost", "host.docker.internal")
            for as_caller in (False, True):
                start(bundle, alpha, as_caller)
                print("\nLAMBDA_DQL_AS_CALLER=%s" % ("true" if as_caller else "false (the runtime as it is)"))
                for who, token in (("the workspace owner", tok), ("no token", None)):
                    print("  %-20s read:  %s" % (who, counts(call("Query.spikeWhere", token))))
                    print("  %-20s write: %s" % (who, call("Mutation.spikeWrite", token)))
        finally:
            stop()
            c.flags = P.HEADER_MODE
            P.restart(c)
            print()
            for ns, name in ((0, "root namespace"), (1, "the partition")):
                d = c.dql(COUNT, ns=ns)["data"]
                rows = c.rows("{ q(func: type(SpikeDqlProbe)) { uid } }", ns=ns)
                print("%-15s holds %s candidates and %s workspaces, and %d records the probe wrote" % (
                    name, d["c"][0]["count"], d["w"][0]["count"], len(rows)))
                if rows:
                    c.mutate("\n".join("<%s> <dgraph.type> * ." % r["uid"] for r in rows), ns=ns, delete=True)
                    left = c.rows("{ q(func: type(SpikeDqlProbe)) { uid } }", ns=ns)
                    if left:
                        # seen twice on 28 Nov and not reproduced since: the type index kept the record
                        # after its type was deleted. Writing the value back and deleting it by value clears it.
                        print("  STILL LISTED after removal: %s; clearing by value" % [r["uid"] for r in left])
                        for r in left:
                            c.mutate('<%s> <dgraph.type> "SpikeDqlProbe" .' % r["uid"], ns=ns)
                            c.mutate('<%s> <dgraph.type> "SpikeDqlProbe" .' % r["uid"], ns=ns, delete=True)
                        print("  left now: %d" % len(c.rows("{ q(func: type(SpikeDqlProbe)) { uid } }", ns=ns)))
            c.flags = P.OFF
            P.restart(c)


if __name__ == "__main__":
    main()
