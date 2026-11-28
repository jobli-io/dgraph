"""An alpha whose GraphQL schema will not load: where does a request with a token go?
(spike probe, not a proof: it prints and asserts nothing)

Jobli's schema calls lambda while it loads, so an alpha that starts while lambda is down
has a schema stored and none loaded. The tenant resolver takes the name of the header
that carries the token from the loaded schema. This starts the proof cluster under the
claim-based resolver with nothing answering for lambda, and asks by DQL:

  as the workspace owner     should be the partition: 16,163 candidates, 1 workspace
  with no token              the root namespace: 29,955 candidates, 173 workspaces
  with a token signed wrong  should be refused

then starts the lambda stub and asks by GraphQL, to see the alpha recover by itself.

  SPIKE_SUB=... SPIKE_EMAIL=... python3 probe_unloaded_schema.py
  SPIKE_BIN=<an earlier build> ...     to see what that build did
"""
import http.server
import threading
import time

import proof_real as P
import probe_phase1_writes as W
import probe_subscriptions as S
import probe_target_lambda as T

COUNT = "{ c(func: type(Candidate)) { count(uid) } w(func: type(Workspace)) { count(uid) } }"


def where(r):
    if r.get("errors"):
        return "refused: %s" % r["errors"][0].get("message", "")[:90]
    d = r["data"]
    return "%s candidates, %s workspaces" % (d["c"][0]["count"], d["w"][0]["count"])


def main():
    if not (P.SUB and P.EMAIL):
        raise SystemExit("set SPIKE_SUB and SPIKE_EMAIL to the userId and email of the owner of %s" % P.WS)
    tok = P.token()
    forged = tok[:-6] + ("AAAAAA" if not tok.endswith("AAAAAA") else "BBBBBB")
    c = P.cluster(P.ON)
    made = {}
    with c:
        c.wait_ready(0, timeout=900)
        try:
            for arm, flags in (("claim-based resolver", P.ON), ("the target state, door on", T.TARGET + T.DOOR)):
                c.flags = flags
                P.restart(c)
                print("\n== %s, lambda down" % arm, flush=True)
                r = P.gql(c, tok, "{ aggregateCandidate { count } }", {})
                print("  GraphQL as the owner:        %s" % ((r.get("errors") or [{}])[0].get("message", "")[:100] or r.get("data")))
                print("  DQL as the owner:            %s" % where(c.dql(COUNT, headers={P.HEADER: tok})))
                print("  DQL with no token:           %s" % where(c.dql(COUNT)))
                print("  DQL, token signed wrong:     %s" % where(c.dql(COUNT, headers={P.HEADER: forged})))
                r = c.post("/mutate?commitNow=true", '{ set { _:x <dgraph.type> "SpikeUnloadedProbe" . } }', ctype="application/rdf",
                           headers={P.HEADER: tok})
                made[arm] = ((r.get("data") or {}).get("uids") or {}).get("x") or "refused: %s" % (r.get("errors") or [{}])[0].get("message", "")[:80]
                print("  DQL write as the owner:      %s" % made[arm])
            # lambda comes back: nothing is restarted
            server = http.server.ThreadingHTTPServer(("127.0.0.1", P.LAMBDA_PORT), W.Stub)
            threading.Thread(target=server.serve_forever, daemon=True).start()
            end, r = time.time() + 60, {}
            while time.time() < end:
                r = P.gql(c, tok, "{ aggregateCandidate { count } }", {})
                if r.get("data"):
                    break
                time.sleep(1)
            print("\nlambda back, no restart: GraphQL as the owner: %s" % (r.get("data") or (r.get("errors") or [{}])[0].get("message", "")[:100]))
        finally:
            try:
                server
            except NameError:
                server = http.server.ThreadingHTTPServer(("127.0.0.1", P.LAMBDA_PORT), W.Stub)
                threading.Thread(target=server.serve_forever, daemon=True).start()
            c.flags = P.HEADER_MODE
            P.restart(c)
            print()
            for ns, name in ((0, "the root namespace"), (1, "the partition")):
                rows = S.nsrows(c, "{ q(func: type(SpikeUnloadedProbe)) { uid } }", ns)
                print("records the owner's writes left in %s: %d" % (name, len(rows)))
                if rows:
                    c.mutate("\n".join('<%s> <dgraph.type> "SpikeUnloadedProbe" .' % r["uid"] for r in rows), ns=ns, delete=True)
            c.flags = P.OFF
            P.restart(c)


if __name__ == "__main__":
    main()
