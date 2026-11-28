"""The door of a partition: who gets in, by every kind of request (spike probe, not a
proof: it prints and asserts nothing).

The claim that selects a partition is whatever the request named (the gateway checks
it against nothing). GraphQL has rules that keep a non-member out. DQL has none, and a
lambda resolver that works by DQL acts wherever the caller's claim points.

`partition-door` makes the tenant resolver ask the partition, before it lets a request
in, whether its rules show the caller any record that can hold a grant. This runs the
same requests with the door off and on, in the target state with Jobli's real lambda:

  callers    the owner, a viewer of the workspace, a viewer of one group only, the
             apps installed in the workspace (a token with no user), a member of another
             workspace naming this one (through the test client and through the web
             app's client), and a user nobody has heard of
  requests   a GraphQL count, a DQL read, a DQL write, and a lambda resolver that
             writes by DQL (setClientData, on the owner's User record)

and times a small query as the owner with the door off, on, and asking every time.

  SPIKE_SUB=... SPIKE_EMAIL=... python3 probe_partition_door.py
"""
import json
import os
import time

import proof_real as P
import probe_cross_workspace as X
import probe_phase1_writes as W
import probe_subscriptions as S
import probe_target_lambda as T
import spike_lambda as L

DOOR = T.TARGET + "; partition-door=true; partition-door-ttl=5s"
ASKING = T.TARGET + "; partition-door=true; partition-door-ttl=0"
ARMS = [("door off", T.TARGET), ("door on", DOOR)]
COUNT = "{ c(func: type(Candidate)) { count(uid) } }"
SMALL = "{ q: queryNoteType(first: 1) { id } }"
RUNS = 300


def short(r):
    errs = r.get("errors") or []
    return errs[0].get("message", "")[:70] if errs else None


def ask(c, tok, me):
    """What one caller gets from each kind of request."""
    out = {}
    r = P.gql(c, tok, "{ c: aggregateCandidate { count } }", {})
    out["graphql"] = short(r) or "%s candidates" % ((r.get("data") or {}).get("c") or {}).get("count")
    r = c.dql(COUNT, headers={P.HEADER: tok})
    out["dql read"] = short(r) or "%s candidates" % r["data"]["c"][0]["count"]
    r = c.post("/mutate?commitNow=true", '{ set { _:x <dgraph.type> "SpikeDoorProbe" . } }', ctype="application/rdf",
               headers={P.HEADER: tok})
    out["dql write"] = short(r) or "written"
    if me:
        info = "door %d" % time.time_ns()
        r = P.gql(c, tok, P.app_query("api/clientData.ts", "SET_CLIENTDATA"),
                  {"forResource": me, "xId": "spike-door", "info": info})
        d = (r.get("data") or {}).get("data") or {}
        # what the owner's record holds afterwards says whether the write went through
        held = c.dql('{ q(func: uid(%s)) { d: ClientDataOwner.hasClientData @filter(eq(ClientData.clientId, "integration-test")) '
                     '{ i: ClientData.info } } }' % me, headers={P.HEADER: P.token()})
        rows = ((held.get("data") or {}).get("q") or [{}])[0].get("d") or []
        wrote = any(x.get("i") == info for x in (rows if isinstance(rows, list) else [rows]))
        out["lambda write"] = "%s; the owner's record %s" % (
            short(r) or "answered success %s" % d.get("success"), "CHANGED" if wrote else "not changed")
    return out


def timed(c, tok):
    first = time.perf_counter()
    P.gql(c, tok, SMALL, {})
    first = (time.perf_counter() - first) * 1000
    ms = []
    for _ in range(RUNS):
        s0 = time.perf_counter()
        P.gql(c, tok, SMALL, {})
        ms.append((time.perf_counter() - s0) * 1000)
    return first, P.pct(ms, 0.5), P.pct(ms, 0.95)


def main():
    if not (P.SUB and P.EMAIL):
        raise SystemExit("set SPIKE_SUB and SPIKE_EMAIL to the userId and email of the owner of %s" % P.WS)
    original = open(os.path.join(P.OUT, "schema_today.graphql")).read()
    bundle = L.build()
    c = P.cluster(P.OFF)
    L.Tap(c.alpha_http(0)).start()
    out, marker = {}, None
    try:
        L.start(bundle, True)
        c.start()
        c.wait_ready(0, timeout=900)
        schema = dict(X.arms(original))["scope"]
        if P.get_schema(c) != schema:
            P.set_schema(c, schema)
        ws = S.nsrows(c, '{ q(func: eq(Workspace.name, "%s")) { uid } }' % P.WS, None)[0]["uid"]
        me = W.uids(S.nsrows(c, '{ q(func: eq(User.userId, "%s")) @filter(uid_in(%s, %s)) { uid } }' % (P.SUB, W.WSM, ws), None))[0]
        here = {u.get("User.userId") for u in W.rows(c, "{ q(func: uid(%s)) { u: Workspace.hasUser { User.userId } } }" % ws)[0]["u"]}
        other = W.rows(c, '{ q(func: eq(Workspace.name, "%s")) { u: Workspace.hasUser @filter(eq(User.status, "ACTIVE")) '
                          "{ uid User.userId User.email } } }" % W.OTHER_WS)[0]["u"]
        stranger = next(u for u in other if u.get("User.userId") and u["User.userId"] not in here)
        c.flags = P.HEADER_MODE
        P.restart(c)
        marker = c.mutate('_:m <dgraph.type> "SpikeMarker" .', ns=0)["data"]["uids"]["m"]
        P.add_callers(c, ws)
        apps = S.nsrows(c, "{ q(func: uid(%s)) { p: Workspace.hasPlugin @filter(NOT eq(Recordable.deleted, true)) { c: Plugin.clientId "
                           "b: count(Plugin.hasIAMBinding) a: Plugin.hasApp { c: IntegrationApp.clientId } } } }" % ws, 1)
        apps = sorted({p.get("c") for p in (apps[0].get("p") if apps else []) or [] if p.get("c")})
        print("apps installed in the workspace: %s" % apps)
        s_sub, s_email = stranger["User.userId"], stranger.get("User.email", "")
        callers = [("the owner", P.token(), True),
                   ("a viewer of the workspace", P.token(sub=P.CALLERS["reader"], email="reader@spike.invalid"), True),
                   ("a viewer of one group only", P.token(sub=P.CALLERS["group"], email="group@spike.invalid"), True)]
        callers += [("app %s, no user" % a, P.token(sub="anonymous", email="", azp=a), False) for a in apps]
        callers += [("a member of %s" % W.OTHER_WS, P.token(sub=s_sub, email=s_email), True),
                    ("the same, through the web app", P.token(sub=s_sub, email=s_email, azp="jobli-web"), False),
                    ("a user nobody has heard of", P.token(sub="spike-nobody", email="nobody@spike.invalid"), True)]
        for arm, flags in ARMS:
            c.flags = flags
            P.restart(c)
            end = time.time() + 180
            while time.time() < end and not P.gql(c, callers[0][1], "{ aggregateCandidate { count } }", {}).get("data"):
                time.sleep(0.5)
            print("\n== %s" % arm, flush=True)
            res = out[arm] = {}
            # the owner saves client data first, so that the others have a record to overwrite
            for name, tok, lam in callers:
                res[name] = ask(c, tok, me if lam else None)
                print("  %-34s %s" % (name, "; ".join("%s: %s" % kv for kv in res[name].items())), flush=True)
        print("\n%-34s %-10s %s" % ("a small query as the owner", "first", "then %d more: median, 95th" % RUNS))
        for arm, flags in ARMS + [("door on, asking every time", ASKING)]:
            c.flags = flags
            P.restart(c)
            end = time.time() + 180
            while time.time() < end and "data" not in P.gql(c, P.token(sub="warm", email=""), "{ __typename }", {}):
                time.sleep(0.5)
            first, p50, p95 = timed(c, callers[0][1])
            out.setdefault("timing", {})[arm] = [first, p50, p95]
            print("%-34s %6.1f ms  %6.2f ms, %6.2f ms" % (arm, first, p50, p95), flush=True)
    finally:
        L.stop()
        if c.procs:
            try:
                c.flags = P.HEADER_MODE
                P.restart(c)
                P.remove_callers(c)
                if marker:
                    for n in (0, 1):
                        print("removed %d nodes and %d edges to them in %s" % (
                            S.remove_new(c, marker, n) + ("the partition" if n else "the root namespace",)))
                c.flags = P.OFF
                P.restart(c)
                if P.get_schema(c) != original:
                    P.set_schema(c, original)
            finally:
                c.stop()
    P.save("partition_door", out)


if __name__ == "__main__":
    main()
