"""The target state, up and running on this machine, for the web app to be pointed at.

  the proof cluster   the workspace in its partition, every spike flag on, the door included
  lambda              Jobli's whole bundle in the stock runtime, LAMBDA_DQL_AS_CALLER=true
                      (spike_lambda.py: recording proxy, local backend sink, no name server)
  a stand-in gateway  on this machine, in place of api.dev.jobli.io

The stand-in does the two things the web app needs of the gateway, the way
gateway/jobli/middleware/main.go does them:

  POST /oauth/token   password, refresh_token and client_credentials grants. It gives an
                      opaque key and keeps who it is for. The Firebase idToken is read
                      for its email and NOT verified: this is for one person on their
                      own machine.
  POST /graphql       finds the session of the key in `Authorization`, signs the JWT
                      Dgraph verifies with the workspace the `Jobli-Workspace` header
                      names (`*` when there is none), and passes the request on.
                      Server-sent events are passed through as they come.

  python3 target_state.py up      # runs until Ctrl-C, then puts the cluster back
  python3 target_state.py check   # brings it up, tries it with SPIKE_SUB / SPIKE_EMAIL, takes it down

With it up, start the web app in another terminal (its .env.local is not touched):

  cd nextgorillaapp && GRAPHQL_API=http://localhost:8790/graphql \\
      TOKEN_API=http://localhost:8790/oauth/token npm run dev

Whatever is added while it is up is removed from both namespaces when it stops.
"""
import base64
import hashlib
import hmac
import http.client
import http.server
import json
import os
import secrets
import sys
import threading
import time
import urllib.parse
import urllib.request

import proof_real as P
import probe_cross_workspace as X
import probe_phase1_writes as W
import probe_subscriptions as S
import probe_target_lambda as T
import spike_lambda as L

GATEWAY_PORT = 8790
SESSIONS = {}                 # opaque key -> {"sub", "email", "azp"}
REFRESH = {}                  # refresh token -> the same


def sign(claims):
    signing = P.b64(json.dumps({"alg": "HS256", "typ": "JWT"}).encode()) + "." + P.b64(json.dumps(claims).encode())
    return signing + "." + P.b64(hmac.new(P.SECRET.encode(), signing.encode(), hashlib.sha256).digest())


def graph_jwt(session, workspace):
    """The claims AddGraphJWT builds. A session here has no list of workspaces, as a
    password-grant session has none, so the workspace named is taken as it is."""
    now = int(time.time())
    return sign({"aud": [P.AUDIENCE], "iss": "urn://api.jobli.io", "iat": now, "exp": now + 3600, "scope": ["_all"],
                 "azp": session["azp"], "sub": session["sub"] or "anonymous", "email": session["email"],
                 "ws": workspace or "*"})


class Gateway(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    upstream = None           # (host, port) of the alpha

    def cors(self):
        self.send_header("Access-Control-Allow-Origin", self.headers.get("Origin") or "*")
        self.send_header("Access-Control-Allow-Credentials", "true")
        self.send_header("Access-Control-Allow-Headers", self.headers.get("Access-Control-Request-Headers") or "*")
        self.send_header("Access-Control-Allow-Methods", "POST, GET, OPTIONS")

    def answer(self, status, obj):
        raw = json.dumps(obj).encode()
        self.send_response(status)
        self.cors()
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_OPTIONS(self):
        self.send_response(204)
        self.cors()
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        path = self.path.split("?")[0].rstrip("/")
        if path.endswith("/oauth/token"):
            return self.token(body)
        if path.endswith("/graphql"):
            return self.graphql(body)
        self.answer(404, {"error": "the stand-in gateway serves /oauth/token and /graphql"})

    def token(self, body):
        args = dict(urllib.parse.parse_qsl(self.path.partition("?")[2]))
        try:
            args.update(json.loads(body or b"{}"))
        except ValueError:
            args.update(urllib.parse.parse_qsl(body.decode("utf-8", "replace")))
        try:
            azp = base64.b64decode(self.headers.get("Authorization", "").split()[-1]).decode().split(":")[0]
        except (ValueError, IndexError):
            azp = ""
        grant = args.get("grant_type")
        if grant == "password":
            session = {"sub": args.get("username", ""), "email": L.claims(args.get("password", "")).get("email", ""), "azp": azp}
        elif grant == "refresh_token" and args.get("refresh_token") in REFRESH:
            session = REFRESH[args["refresh_token"]]
        elif grant == "client_credentials":
            session = {"sub": "", "email": "", "azp": azp}
        else:
            return self.answer(400, {"error": "invalid_grant"})
        key, refresh = secrets.token_hex(24), secrets.token_hex(24)
        SESSIONS[key] = REFRESH[refresh] = session
        self.answer(200, {"access_token": key, "refresh_token": refresh, "token_type": "bearer", "expires_in": 3600,
                          "scope": "_all"})

    def graphql(self, body):
        key = (self.headers.get("Authorization") or "").split()[-1:] or [""]
        session = SESSIONS.get(key[0])
        if not session:
            return self.answer(401, {"error": "Key not authorised"})
        headers = {k: v for k, v in self.headers.items() if k.lower() not in (
            "host", "content-length", "connection", "accept-encoding", "authorization", "origin", "referer", "cookie")}
        headers[P.HEADER] = graph_jwt(session, self.headers.get("Jobli-Workspace"))
        conn = http.client.HTTPConnection(*self.upstream, timeout=3600)
        try:
            conn.request("POST", "/graphql", body, headers)
            resp = conn.getresponse()
            ctype = resp.getheader("Content-Type") or "application/json"
            self.send_response(resp.status)
            self.cors()
            self.send_header("Content-Type", ctype)
            if "event-stream" not in ctype:
                raw = resp.read()
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
                return
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            while True:
                chunk = resp.read1(65536)
                if not chunk:
                    break
                self.wfile.write(b"%x\r\n%s\r\n" % (len(chunk), chunk))
                self.wfile.flush()
            self.wfile.write(b"0\r\n\r\n")
        except (OSError, http.client.HTTPException):
            pass                              # the browser closed the stream, or the alpha went away
        finally:
            conn.close()

    def log_message(self, *args):
        pass


def start_gateway(alpha):
    u = urllib.parse.urlparse(alpha)
    Gateway.upstream = (u.hostname, u.port)
    server = http.server.ThreadingHTTPServer(("127.0.0.1", GATEWAY_PORT), Gateway)
    server.daemon_threads = True
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return "http://localhost:%d" % GATEWAY_PORT


def post(url, body, headers):
    req = urllib.request.Request(url, data=body, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            return json.loads(resp.read() or b"{}")
    except urllib.error.HTTPError as e:
        return {"status": e.code, "body": e.read().decode("utf-8", "replace")[:300]}


def check(c, gateway):
    """Signs in the way the web app does and asks through the stand-in gateway."""
    if not (P.SUB and P.EMAIL):
        raise SystemExit("set SPIKE_SUB and SPIKE_EMAIL to the userId and email of a member of %s" % P.WS)
    id_token = "x.%s.x" % P.b64(json.dumps({"email": P.EMAIL, "user_id": P.SUB}).encode())   # stands for Firebase's
    basic = "Basic " + base64.b64encode(b"jobli-web:unused").decode()
    tok = post(gateway + "/oauth/token?" + urllib.parse.urlencode(
        {"grant_type": "password", "username": P.SUB, "password": id_token, "scope": "_all"}), b"", {"Authorization": basic})
    print("password grant: %s" % sorted(tok))
    again = post(gateway + "/oauth/token", json.dumps({"grant_type": "refresh_token", "refresh_token": tok.get("refresh_token")}).encode(),
                 {"Authorization": basic, "Content-Type": "application/json"})
    print("refresh grant: %s" % ("a new key" if again.get("access_token") not in (None, tok.get("access_token")) else again))
    count = json.dumps({"query": "{ c: aggregateCandidate { count } w: aggregateWorkspace { count } }"}).encode()
    for name, headers in (("naming %s" % P.WS, {"Jobli-Workspace": P.WS}), ("naming *", {"Jobli-Workspace": "*"}), ("naming none", {})):
        r = post(gateway + "/graphql", count, dict(headers, Authorization=again["access_token"], **{"Content-Type": "application/json"}))
        d = r.get("data") or {}
        print("  %-18s %s candidates, %s workspaces %s" % (name, (d.get("c") or {}).get("count"), (d.get("w") or {}).get("count"),
                                                        [e["message"][:120] for e in r.get("errors") or []] or ""))
    r = post(gateway + "/graphql", count, {"Authorization": "nobody", "Content-Type": "application/json"})
    print("  %-18s %s" % ("an unknown key", r))
    r = post(gateway + "/graphql", json.dumps({"query": "{ me { id email } }"}).encode(),
             {"Authorization": "Bearer " + again["access_token"], "Jobli-Workspace": P.WS, "Content-Type": "application/json"})
    print("  me, through lambda: %d rows %s" % (len((r.get("data") or {}).get("me") or []), [e["message"][:120] for e in r.get("errors") or []] or ""))
    # a subscription over server-sent events, as the web app opens them
    u = urllib.parse.urlparse(gateway)
    conn = http.client.HTTPConnection(u.hostname, u.port, timeout=120)
    conn.request("POST", "/graphql", json.dumps({"query": "subscription { c: aggregateCandidate { count } }"}),
                 {"Authorization": again["access_token"], "Jobli-Workspace": P.WS, "Content-Type": "application/json",
                  "Accept": "text/event-stream"})
    resp = conn.getresponse()
    first = None
    if "event-stream" in (resp.getheader("Content-Type") or ""):
        end = time.time() + 60
        while time.time() < end and first is None:
            line = resp.readline()
            if line.startswith(b"data: ") and len(line) > 8:
                first = json.loads(line[6:])
    conn.close()
    print("  subscription over SSE: first frame %s" % (json.dumps(first)[:160] if first else "NONE (%s %s)" % (
        resp.status, resp.getheader("Content-Type"))))


def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else "up"
    if mode not in ("up", "check"):
        raise SystemExit(__doc__)
    original = open(os.path.join(P.OUT, "schema_today.graphql")).read()
    bundle = L.build()
    c = P.cluster(P.OFF)
    tap = L.Tap(c.alpha_http(0)).start()
    gateway = start_gateway(c.alpha_http(0))
    marker = None
    try:
        L.start(bundle, True)
        c.start()
        c.wait_ready(0, timeout=900)
        schema = dict(X.arms(original))["scope"]
        if P.get_schema(c) != schema:
            r = P.set_schema(c, schema)
            if r.get("errors"):
                raise SystemExit("the schema was refused: %s" % json.dumps(r["errors"])[:400])
        c.flags = P.HEADER_MODE
        P.restart(c)
        marker = c.mutate('_:m <dgraph.type> "SpikeMarker" .', ns=0)["data"]["uids"]["m"]
        c.flags = T.TARGET + T.DOOR
        P.restart(c)
        probe = P.token(sub=P.SUB or "nobody", email=P.EMAIL)
        end = time.time() + 180
        while time.time() < end and "data" not in P.gql(c, probe, "{ __typename }", {}):
            time.sleep(0.5)
        print("the target state is up: alpha %s, lambda on %d, stand-in gateway %s" % (c.alpha_http(0), P.LAMBDA_PORT, gateway))
        print("inside the lambda container, example.com resolves: %s" % L.resolves("example.com"))
        if mode == "check":
            check(c, gateway)
            return
        print("\nstart the web app in another terminal:\n  cd %s && GRAPHQL_API=%s/graphql TOKEN_API=%s/oauth/token npm run dev\n"
              "\nCtrl-C here stops it and removes what was added." % (P.APP, gateway, gateway), flush=True)
        seen = 0
        while True:
            time.sleep(5)
            if len(tap.calls) > seen:
                bare = sum(1 for x in tap.calls[seen:] if not x["token"])
                print("lambda asked the alpha %d more times, %d with no caller's token" % (len(tap.calls) - seen, bare), flush=True)
                seen = len(tap.calls)
    except KeyboardInterrupt:
        print("\nstopping")
    finally:
        L.stop()
        if c.procs:
            try:
                if marker:
                    c.flags = P.HEADER_MODE
                    P.restart(c)
                    for n in (0, 1):
                        print("removed %d nodes and %d edges to them in %s" % (
                            S.remove_new(c, marker, n) + ("the partition" if n else "the root namespace",)))
                c.flags = P.OFF
                P.restart(c)
                if P.get_schema(c) != original:
                    P.set_schema(c, original)
            finally:
                c.stop()


if __name__ == "__main__":
    main()
