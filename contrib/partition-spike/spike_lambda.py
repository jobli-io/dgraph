"""Jobli's real lambda for the probes, in place of the stub that says yes to everything.

The whole bundle (graph/lambda, as `npm run build:dev` builds it) is built outside the
repository and run in the stock dgraph/dgraph-lambda image on the port the proof
cluster's alpha calls. Three things keep it on this machine:

  - its alpha is a recording proxy on this host (`Tap`), which passes every request on
    to the proof cluster and keeps what was asked and whether it carried a token
  - its backend (BACKEND_API: notifications, storage, billing) is a sink on this host
    that answers 200 and keeps what it was sent, so the events lambda would publish
    can be read
  - the container is given no name server: no host name but host.docker.internal
    resolves in it

The dev lambda container and graph/lambda/dist are not touched.
"""
import base64
import http.client
import http.server
import json
import os
import subprocess
import threading
import time
import urllib.parse
import urllib.request

import proof_real as P

LAMBDA_SRC = os.path.join(P.JOBLI, "graph", "lambda")
WORK = os.path.join(P.ROOT, "run", "lambda-full")
IMAGE = "dgraph/dgraph-lambda:latest"
NAME = "spike-lambda"
TAP_PORT, SINK_PORT = 8788, 8789
# the value graph/docker-compose.yml gives the dev lambda
TOKEN_KEY = "0123456789abcdef" * 4

CONFIG = """
const config = require(%(repo)s);
config.output = Object.assign({}, config.output, { path: %(out)s });
module.exports = config;
"""


def build():
    """The whole lambda bundle, as `npm run build:dev` makes it, written outside the repository."""
    os.makedirs(WORK, exist_ok=True)
    out = os.path.join(WORK, "dist")
    conf = os.path.join(WORK, "webpack.full.js")
    open(conf, "w").write(CONFIG % {"repo": json.dumps(os.path.join(LAMBDA_SRC, "webpack.config.js")), "out": json.dumps(out)})
    env = dict(os.environ, NODE_OPTIONS="--openssl-legacy-provider")
    r = subprocess.run(["npx", "webpack", "--mode", "development", "--target=webworker", "--config", conf],
                       cwd=LAMBDA_SRC, env=env, capture_output=True, text=True)
    bundle = os.path.join(out, "main.js")
    if r.returncode or not os.path.exists(bundle):
        raise SystemExit("the bundle did not build:\n%s\n%s" % (r.stdout[-1500:], r.stderr[-1500:]))
    return bundle


def claims(token):
    try:
        body = token.split(".")[1]
        return json.loads(base64.urlsafe_b64decode(body + "=" * (-len(body) % 4)))
    except (ValueError, IndexError, AttributeError):
        return {}


class Tap:
    """What lambda asked of the alpha, and what it sent to its backend."""

    def __init__(self, upstream):
        u = urllib.parse.urlparse(upstream)
        self.host, self.port = u.hostname, u.port
        self.calls, self.sent = [], []
        self.servers = []

    def start(self):
        tap = self

        class Forward(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def pass_on(self, method):
                body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
                token = self.headers.get(P.HEADER)
                rec = {"t": time.time(), "path": self.path.split("?")[0], "args": self.path.partition("?")[2],
                       "token": bool(token), "ws": claims(token).get("ws") if token else None,
                       "body": body[:4000].decode("utf-8", "replace")}
                headers = {k: v for k, v in self.headers.items()
                           if k.lower() not in ("host", "content-length", "connection", "accept-encoding", "transfer-encoding")}
                try:
                    conn = http.client.HTTPConnection(tap.host, tap.port, timeout=900)
                    conn.request(method, self.path, body or None, headers)
                    resp = conn.getresponse()
                    raw, status, ctype = resp.read(), resp.status, resp.getheader("Content-Type") or "application/json"
                    conn.close()
                except OSError as e:
                    raw, status, ctype = json.dumps({"errors": [{"message": "tap: %s" % e}]}).encode(), 502, "application/json"
                rec.update(status=status, ms=(time.time() - rec["t"]) * 1000, answer=raw[:1500].decode("utf-8", "replace"))
                tap.calls.append(rec)
                self.send_response(status)
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_POST(self):
                self.pass_on("POST")

            def do_GET(self):
                self.pass_on("GET")

            def log_message(self, *args):
                pass

        class Sink(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def take(self):
                body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
                tap.sent.append({"t": time.time(), "path": self.path, "type": self.headers.get("Ce-Type"),
                                 "workspace": self.headers.get("Ce-Workspacename"), "body": body[:20000].decode("utf-8", "replace")})
                out = b"{}"
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(out)))
                self.end_headers()
                self.wfile.write(out)

            do_POST = do_GET = do_PUT = do_DELETE = take

            def log_message(self, *args):
                pass

        for port, handler in ((TAP_PORT, Forward), (SINK_PORT, Sink)):
            server = http.server.ThreadingHTTPServer(("127.0.0.1", port), handler)
            server.daemon_threads = True
            threading.Thread(target=server.serve_forever, daemon=True).start()
            self.servers.append(server)
        return self

    def quiet(self, for_s=1.5, timeout=45):
        """Waits until lambda has asked nothing for a while, counted from now: webhooks are
        sent after the answer, so the first request of one can come some time after it."""
        since = time.time()
        end = since + timeout
        while time.time() < end:
            last = max([c["t"] + c["ms"] / 1000 for c in self.calls[-5:]] + [s["t"] for s in self.sent[-5:]] + [since])
            if time.time() - last >= for_s:
                return True
            time.sleep(0.2)
        return False


def stop():
    subprocess.run(["docker", "rm", "-f", NAME], capture_output=True)


def start(bundle, as_caller, notify=True):
    """Runs the bundle in the stock runtime, on the port the proof cluster's alpha calls."""
    stop()
    env = {"DGRAPH_URL": "http://host.docker.internal:%d" % TAP_PORT, "NODE_ENV": "development",
           "LAMBDA_DQL_AS_CALLER": "true" if as_caller else "false", "TOKEN_ENCRYPTION_KEY": TOKEN_KEY}
    if notify:
        env.update(NOTIFY_ENABLED="true", BACKEND_API="http://host.docker.internal:%d" % SINK_PORT, BACKEND_API_KEY="spike")
    cmd = ["docker", "run", "-d", "--name", NAME, "-p", "127.0.0.1:%d:8686" % P.LAMBDA_PORT,
           "--add-host", "host.docker.internal:host-gateway", "--dns", "127.0.0.1",
           "-v", "%s:/app/script/script.js:ro" % bundle,
           "-v", "%s:/app/ecosystem.config.js:ro" % os.path.join(LAMBDA_SRC, "ecosystem.config.js")]
    for k, v in env.items():
        cmd += ["-e", "%s=%s" % (k, v)]
    r = subprocess.run(cmd + [IMAGE], capture_output=True, text=True)
    if r.returncode:
        raise SystemExit("the lambda container did not start: %s" % r.stderr[-600:])
    end = time.time() + 120
    while time.time() < end:
        try:
            req = urllib.request.Request("http://127.0.0.1:%d/graphql-worker" % P.LAMBDA_PORT, method="POST",
                                         data=b'{"resolver": "Spike.none"}', headers={"Content-Type": "application/json"})
            urllib.request.urlopen(req, timeout=10).read()
            return
        except urllib.error.HTTPError:
            return                                   # it answered: the script is loaded
        except (urllib.error.URLError, ConnectionError, OSError):
            time.sleep(1)
    raise SystemExit("the lambda container did not answer:\n%s" % logs()[-2000:])


def logs():
    r = subprocess.run(["docker", "logs", NAME], capture_output=True, text=True)
    return r.stdout + r.stderr


def resolves(name):
    """Whether a host name resolves inside the container."""
    r = subprocess.run(["docker", "exec", NAME, "node", "-e",
                        "require('dns').lookup(%s, (e, a) => console.log(e ? 'no' : a))" % json.dumps(name)],
                       capture_output=True, text=True, timeout=60)
    return r.stdout.strip()
