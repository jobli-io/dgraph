"""The app's flows through Jobli's real lambda, today and in the target state (spike
probe, not a proof: it prints and asserts nothing).

Every probe before this one ran against a stub that says yes to each check and does
nothing on a change. Here the alpha calls Jobli's whole lambda bundle (spike_lambda.py),
so each flow runs its real validation, its real resolvers and its real webhooks, and
every request lambda makes of the alpha passes a recording proxy.

Three arms, the same flows as the workspace owner in each:

  today       everything in the root namespace, flags off, lambda as it is
  as it is    the workspace in its partition with every spike flag on, lambda as it is
  target      the same, with LAMBDA_DQL_AS_CALLER=true

For each flow it reports whether it worked, how long it took, what lambda asked of the
alpha (GraphQL, DQL reads, DQL writes) and how much of that carried no caller's token,
the events lambda sent to its backend, the errors lambda logged, and the records made
in each namespace. A request with no token lands in the root namespace, so in a
partition that is a read of nothing or a write to the wrong place.

What the flows add is removed from both namespaces afterwards.

  SPIKE_SUB=... SPIKE_EMAIL=... python3 probe_target_lambda.py
"""
import collections
import json
import os
import re
import time

import proof_real as P
import probe_phase1_writes as W
import probe_cross_workspace as X
import probe_subscriptions as S
import spike_lambda as L

TARGET = (P.FULL + P.INLINE + P.TOGETHER + S.MUTATIONS
          + "; subscription-scope-routing=true; subscription-change-narrowing=true")
ARMS = [("today", P.OFF, None, False), ("as it is", TARGET, 1, False), ("target", TARGET, 1, True)]
ARMS = [a for a in ARMS if not os.environ.get("SPIKE_ARMS") or a[0] in os.environ["SPIKE_ARMS"].split(",")]
STRANGER = "non-member: "
DETAIL = os.path.join(P.ROOT, "run", "target-lambda-calls.txt")


def masked(v):
    """An answer with what differs from run to run taken out: IDs, dates, generated keys."""
    if isinstance(v, dict):
        return {k: ("*" if k in ("id", "xId", "sId", "createdAt", "updatedAt", "periodStart", "periodEnd") and x is not None
                    else masked(x)) for k, x in sorted(v.items())}
    if isinstance(v, list):
        return [masked(x) for x in v]
    if isinstance(v, str) and (re.fullmatch(r"0x[0-9a-f]+", v) or re.match(r"\d{4}-\d\d-\d\dT", v)):
        return "*"
    return v


def differ(a, b, path=""):
    """Where two masked answers differ."""
    if type(a) != type(b):
        return [path or "."]
    if isinstance(a, dict):
        return [p for k in sorted(set(a) | set(b)) for p in differ(a.get(k), b.get(k), "%s.%s" % (path, k))]
    if isinstance(a, list):
        if len(a) != len(b):
            return ["%s (%d and %d)" % (path, len(a), len(b))]
        return [p for i, (x, y) in enumerate(zip(a, b)) for p in differ(x, y, "%s[%d]" % (path, i))]
    return [] if a == b else [path or "."]


def without(document, field):
    """A document with one field, and what it selects, taken out."""
    m = re.search(r"\b%s\s*\{" % re.escape(field), document)
    if not m:
        return document
    depth, i = 1, m.end()
    while depth and i < len(document):
        depth += {"{": 1, "}": -1}.get(document[i], 0)
        i += 1
    return document[:m.start()] + document[i:]


def got(d):
    """What an answer holds, in a few words."""
    if not isinstance(d, dict) or not d:
        return "nothing" if not d else str(d)[:60]
    parts = []
    for k, v in sorted(d.items()):
        if isinstance(v, list):
            parts.append("%d %s" % (len(v), k))
        elif isinstance(v, dict):
            parts.append("%s: %s" % (k, got(v)) if any(isinstance(x, (list, dict)) for x in v.values()) and len(v) < 6 else "a %s" % k)
        elif v is not None and k in ("numUids", "success", "errors", "count"):
            parts.append("%s %s" % (k, v))
        elif v is None:
            parts.append("no %s" % k)
    return ", ".join(parts)[:150] or "an answer"


def kind_of(call):
    if call["path"] == "/graphql":
        return "graphql"
    if call["path"] == "/query":
        return "dql read"
    if call["path"] == "/mutate":
        return "dql write"
    return call["path"]


def failed(call):
    if call["status"] != 200:
        return True
    try:
        return bool(json.loads(call["answer"]).get("errors"))
    except ValueError:
        return '"errors":[{' in call["answer"]


def census(c, marker, tok):
    """The records made since the marker, by namespace and type. Under the claim-based
    resolver a request with no token reads the root namespace and the owner's reads the partition."""
    out = {}
    q = "{ q(func: has(dgraph.type), after: %s, first: 10000) { uid t: dgraph.type } }" % marker
    for name, headers in (("root", None), ("partition", {P.HEADER: tok})) if tok else (("root", None),):
        r = c.dql(q, headers=headers)
        out[name] = {x["uid"]: ",".join(sorted(x.get("t") or [])) for x in (r.get("data") or {}).get("q") or []}
    return out


def main():
    if not (P.SUB and P.EMAIL):
        raise SystemExit("set SPIKE_SUB and SPIKE_EMAIL to the userId and email of the owner of %s" % P.WS)
    original = open(os.path.join(P.OUT, "schema_today.graphql")).read()
    bundle = L.build()
    print("lambda bundle: %s (%d KB)" % (bundle, os.path.getsize(bundle) // 1024))
    c = P.cluster(P.OFF)
    tap = L.Tap(c.alpha_http(0)).start()
    L.start(bundle, False)
    print("inside the lambda container, example.com resolves: %s; host.docker.internal: %s" % (
        L.resolves("example.com"), L.resolves("host.docker.internal")))
    out, detail = {}, []
    try:
        c.start()
        c.wait_ready(0, timeout=900)
        schema = dict(X.arms(original))["scope"]
        if P.get_schema(c) != schema:
            r = P.set_schema(c, schema)
            if r.get("errors"):
                raise SystemExit("the schema was refused: %s" % json.dumps(r["errors"])[:400])
        ws = S.nsrows(c, '{ q(func: eq(Workspace.name, "%s")) { uid } }' % P.WS, None)[0]["uid"]
        t = W.pick_targets(c, ws)
        types = W.uids(S.nsrows(c, "{ q(func: type(NoteType), first: 1) @filter(uid_in(%s, %s)) { uid } }" % (W.WSM, ws), None))
        me = W.uids(S.nsrows(c, '{ q(func: eq(User.userId, "%s")) @filter(uid_in(%s, %s)) { uid } }' % (P.SUB, W.WSM, ws), None))
        if not (types and me):
            raise SystemExit("the workspace has no note type, or the caller is not one of its users")
        tok = P.token()
        G = [{"id": t["G"]}]
        # a member of another workspace, with a token that names this one: the gateway puts
        # the workspace a request names in the claim and does not check it against anything
        here = {u.get("User.userId") for u in W.rows(c, "{ q(func: uid(%s)) { u: Workspace.hasUser { User.userId } } }" % ws)[0]["u"]}
        other = W.rows(c, '{ q(func: eq(Workspace.name, "%s")) { u: Workspace.hasUser @filter(eq(User.status, "ACTIVE")) '
                          "{ uid User.userId User.email } } }" % W.OTHER_WS)[0]["u"]
        stranger = next(u for u in other if u.get("User.userId") and u["User.userId"] not in here)
        stranger_tok = P.token(sub=stranger["User.userId"], email=stranger.get("User.email", ""))
        listing = P.suite(P.load("params"))[0]

        # the flows: (name, document, variables as a function of what earlier flows made, what to keep)
        def first(key):
            return lambda d: ((d or {}).get(key) or [{}])[0].get("id")

        flows = [
            ("app start: user, groups, roles", P.app_query("api/workspace.ts", "RESTORE_CRITICAL"),
             lambda m: {"workspaceId": ws, "email": P.EMAIL}, None),
            ("app start: the workspace", without(P.app_query("api/workspace.ts", "RESTORE_APP_CONTEXT"), "hasEmailService"),
             lambda m: {"workspaceId": ws, "email": P.EMAIL}, None),
            ("app start: job boards and plugins", P.app_query("api/workspace.ts", "RESTORE_INTEGRATIONS"),
             lambda m: {"workspaceId": ws}, None),
            ("app start: workspace extras", without(P.app_query("api/workspace.ts", "RESTORE_WORKSPACE_EXTRAS"), "hasEmailService"),
             lambda m: {"id": ws}, None),
            ("who am I (lambda query)", "{ data: me { id userId email firstName lastName } }", lambda m: {}, None),
            ("job ad quotas and their usage", P.app_query("api/jobAdQuota.ts", "QUERY_JOBAD_QUOTA_BY_WORKSPACE"),
             lambda m: {"workspaceId": [ws]}, None),
            ("save client data (lambda mutation)", P.app_query("api/clientData.ts", "SET_CLIENTDATA"),
             lambda m: {"forResource": me[0], "xId": "spike-target-lambda", "info": json.dumps({"spike": True})}, None),
            ("add a note", P.app_query("api/note.ts", "ADD_NOTE"),
             lambda m: {"input": [{"text": "spike target", "forResource": [{"id": t["N"]}], "hasType": {"id": types[0]}}]},
             ("note", first("note"))),
            ("edit the note", P.app_query("api/note.ts", "UPDATE_NOTE"),
             lambda m: {"input": {"filter": {"id": [m["note"]]}, "set": {"text": "spike target, edited"}}}, None),
            ("remove the note", P.app_query("api/note.ts", "DELETE_NOTE"), lambda m: {"id": m["note"]}, None),
            ("add a candidate", P.app_query("api/candidate.ts", "ADD_CANDIDATE"),
             lambda m: {"input": [{"firstName": "Spike", "lastName": "Target", "email": "spike.target@example.invalid",
                                   "inGroup": G, "hasAddress": {"city": "Spike", "country": "Australia"}}]},
             ("cand", first("candidate"))),
            ("edit the candidate", P.app_query("api/candidate.ts", "UPDATE_CANDIDATE"),
             lambda m: {"input": {"filter": {"id": [m["cand"]]}, "set": {"specialty": "spike", "currentEmployer": "Spike Co"}}}, None),
            ("add a task on the candidate", P.app_query("api/task.ts", "ADD_TASK"),
             lambda m: {"input": [{"title": "spike target task", "forResource": [{"id": m["cand"]}],
                                   "date": "2030-01-01T00:00:00Z"}]}, ("task", first("task"))),
            ("complete the task", P.app_query("api/task.ts", "UPDATE_TASK"),
             lambda m: {"input": {"filter": {"id": [m["task"]]}, "set": {"status": "COMPLETED"}}}, None),
            ("add an attachment to the candidate", P.app_query("api/attachment.ts", "ADD_ATTACHMENT"),
             lambda m: {"input": [{"category": "RESUME", "fileName": "spike.pdf", "forResource": [{"id": m["cand"]}]}]},
             ("att", first("attachment"))),
            ("add a company", P.app_query("api/company.ts", "ADD_COMPANY"),
             lambda m: {"input": [{"name": "Spike Target Co", "inGroup": G}]}, ("co", first("company"))),
            ("add a contact at the company", P.app_query("api/contact.ts", "ADD_CONTACT"),
             lambda m: {"input": [{"firstName": "Spike", "lastName": "Contact", "inGroup": G, "forCompany": {"id": m["co"]}}]},
             ("contact", first("contact"))),
            ("add a job for the company", P.app_query("api/job.ts", "ADD_JOB"),
             lambda m: {"input": [{"title": "Spike target job", "inGroup": G, "forCompany": {"id": m["co"]}}]},
             ("job", first("job"))),
            ("add an application", P.app_query("api/application.ts", "ADD_APPLICATION"),
             lambda m: {"input": [{"inGroup": G, "hasCandidate": {"id": m["cand"]}, "forJob": {"id": m["job"]}}]},
             ("app", first("application"))),
            ("delete the candidate", P.app_query("api/candidate.ts", "DELETE_CANDIDATE"), lambda m: {"id": [m["cand"]]}, None),
        ]
        flows = [f + (tok,) for f in flows]
        flows += [(STRANGER + name, document, variables, None, stranger_tok) for name, document, variables in (
            ("app start: user, groups, roles", P.app_query("api/workspace.ts", "RESTORE_CRITICAL"),
             lambda m: {"workspaceId": ws, "email": stranger.get("User.email", "")}),
            ("who am I (lambda query)", "{ data: me { id userId email } }", lambda m: {}),
            ("the candidate list", listing[1], lambda m: listing[2]),
            ("job ad quotas and their usage", P.app_query("api/jobAdQuota.ts", "QUERY_JOBAD_QUOTA_BY_WORKSPACE"),
             lambda m: {"workspaceId": [ws]}),
            ("save client data on the owner's user", P.app_query("api/clientData.ts", "SET_CLIENTDATA"),
             lambda m: {"forResource": me[0], "xId": "spike-target-stranger", "info": json.dumps({"stranger": True})}),
            ("add a note on a candidate", P.app_query("api/note.ts", "ADD_NOTE"),
             lambda m: {"input": [{"text": "spike stranger", "forResource": [{"id": t["N"]}], "hasType": {"id": types[0]}}]}),
            ("add a candidate", P.app_query("api/candidate.ts", "ADD_CANDIDATE"),
             lambda m: {"input": [{"firstName": "Spike", "lastName": "Stranger", "inGroup": G}]}))]
        if os.environ.get("SPIKE_FLOWS"):
            keep = os.environ["SPIKE_FLOWS"].split(",")
            flows = [f for f in flows if any(k in f[0] for k in keep)]

        for arm, flags, ns, as_caller in ARMS:
            res = out[arm] = collections.OrderedDict()
            c.flags = P.HEADER_MODE if ns else P.OFF
            P.restart(c)
            for n in ((0, 1) if ns else (None,)):
                stale = sorted(W.uids(S.nsrows(c, "{ q(func: type(SpikeMarker)) { uid } }", n)), key=lambda u: int(u, 16))
                if stale:
                    print("an earlier run left its records behind: removed %s" % (S.remove_new(c, stale[0], n),))
            marker = c.mutate('_:m <dgraph.type> "SpikeMarker" .', ns=0 if ns else None)["data"]["uids"]["m"]
            try:
                L.start(bundle, as_caller)
                c.flags = flags
                P.restart(c)
                end = time.time() + 180
                while time.time() < end and not P.gql(c, tok, "{ aggregateCandidate { count } }", {}).get("data"):
                    time.sleep(0.5)
                print("\n== %s (LAMBDA_DQL_AS_CALLER=%s)" % (arm, "true" if as_caller else "false"), flush=True)
                made, seen = {}, census(c, marker, tok if ns else None)
                for name, document, variables, keep, who in flows:
                    try:
                        v = variables(made)
                    except KeyError as e:
                        res[name] = {"ok": False, "error": "not run: an earlier flow made no %s" % e}
                        print("  %-36s %s" % (name, res[name]["error"]))
                        continue
                    tap.quiet(0.8, 20)
                    calls0, sent0, log0 = len(tap.calls), len(tap.sent), len(L.logs())
                    s0 = time.perf_counter()
                    r = P.gql(c, who, document, v)
                    took = (time.perf_counter() - s0) * 1000
                    quiet = tap.quiet()
                    d = r.get("data") or {}
                    d = d["data"] if "data" in d else d      # the app names most answers `data`
                    errs = [e.get("message", "") for e in r.get("errors") or []]
                    calls, sent = tap.calls[calls0:], tap.sent[sent0:]
                    logged = [l for l in L.logs()[log0:].splitlines() if ('"severity":"ERROR"' in l or "[ERROR]" in l)
                              and "GEMINI_API_KEY" not in l]          # embeddings are off here, as in dev
                    now = census(c, marker, tok if ns else None)
                    new = {k: collections.Counter(tp for u, tp in now[k].items() if u not in seen.get(k, {})) for k in now}
                    seen = now
                    if keep and d:
                        made[keep[0]] = keep[1](d)
                        if not made[keep[0]]:
                            del made[keep[0]]
                    kinds = collections.Counter(kind_of(x) for x in calls)
                    bare = collections.Counter(kind_of(x) for x in calls if not x["token"])
                    bad = [x for x in calls if failed(x)]
                    row = res[name] = {
                        "ok": bool(d) and not errs, "error": errs[0][:300] if errs else "", "took": took, "answer": masked(d),
                        "got": got(d),
                        "calls": dict(kinds), "bare": dict(bare), "bad": len(bad), "quiet": quiet,
                        "sent": [x["type"] or x["path"] for x in sent],
                        "events": [x["workspace"] for x in sent if x["type"]],   # the workspace each event names
                        "logged": [l[:300] for l in logged], "new": {k: dict(n) for k, n in new.items()}}
                    print("  %-36s %s in %5.0f ms; lambda asked: %s; with no token: %s; refused or failed: %d; "
                          "sent to its backend: %s; new records: %s%s%s" % (
                              name, "ok    " if row["ok"] else "FAILED", took,
                              ", ".join("%d %s" % (n, k) for k, n in sorted(kinds.items())) or "nothing",
                              ", ".join("%d %s" % (n, k) for k, n in sorted(bare.items())) or "none", len(bad),
                              ", ".join(row["sent"]) or "nothing",
                              "; ".join("%s %d" % (k, sum(n.values())) for k, n in new.items()),
                              "" if quiet else "; lambda NOT quiet",
                              "; lambda logged %d errors" % len(logged) if logged else ""), flush=True)
                    if errs:
                        print("      %s" % errs[0][:400])
                    if name.startswith(STRANGER):
                        print("      got: %s" % row["got"])
                    detail.append("\n==== %s / %s: %s\n" % (arm, name, "ok" if row["ok"] else "FAILED: %s" % errs[:1]))
                    for x in calls:
                        detail.append("  %s %s%s %s ws=%s -> %s %.0f ms\n      asked: %s\n      answer: %s\n" % (
                            x["path"], x["args"] and "?" + x["args"], "", "token" if x["token"] else "NO TOKEN", x["ws"],
                            x["status"], x["ms"], " ".join(x["body"].split())[:1500], " ".join(x["answer"].split())[:500]))
                    for x in sent:
                        detail.append("  sent to backend %s %s workspace=%s\n      %s\n" % (
                            x["path"], x["type"], x["workspace"], " ".join(x["body"].split())[:1200]))
                    for l in logged:
                        detail.append("  lambda logged: %s\n" % l[:600])
                    for k, n in new.items():
                        if n:
                            detail.append("  new in %s: %s\n" % (k, dict(n)))
            finally:
                L.stop()
                c.flags = P.HEADER_MODE if ns else P.OFF
                P.restart(c)
                for n in ((0, 1) if ns else (None,)):
                    print("  removed %d nodes and %d edges to them in %s" % (
                        S.remove_new(c, marker, n) + ("the partition" if n else "the root namespace",)))
    finally:
        L.stop()
        if c.procs:
            try:
                c.flags = P.OFF
                P.restart(c)
                if P.get_schema(c) != original:
                    P.set_schema(c, original)
            finally:
                c.stop()
        with open(DETAIL, "w") as f:
            f.writelines(detail)

    arms = [a[0] for a in ARMS]
    everything = list(next(iter(out.values()), {}))
    names = [n for n in everything if not n.startswith(STRANGER)]
    print("\n%-36s %s" % ("flow", "  ".join("%-26s" % a for a in arms)))
    for n in names:
        cells = []
        for a in arms:
            r = out.get(a, {}).get(n)
            if not r or "took" not in r:
                cells.append("not run")
                continue
            base = out.get("today", {}).get(n, {})
            same = "" if a == "today" or not r["ok"] or "answer" not in base else (
                ", same answer" if not differ(base["answer"], r["answer"]) else ", answer differs")
            cells.append("%s %4.0f ms%s" % ("ok" if r["ok"] else "FAILED", r["took"], same))
        print("%-36s %s" % (n, "  ".join("%-26s" % x for x in cells)))
    for key, text in (("bare", "requests from lambda with no caller's token"), ("bad", "requests from lambda refused or failed"),
                      ("logged", "errors lambda logged")):
        print("\n%-36s %s" % (text, "  ".join("%-26s" % a for a in arms)))
        for n in names:
            cells = []
            for a in arms:
                r = out.get(a, {}).get(n, {})
                v = r.get(key)
                cells.append("-" if v is None else str(sum(v.values()) if isinstance(v, dict) else len(v) if isinstance(v, list) else v))
            print("%-36s %s" % (n, "  ".join("%-26s" % x for x in cells)))
    print("\n%-36s %s" % ("events sent / naming no workspace", "  ".join("%-26s" % a for a in arms)))
    for n in names:
        cells = []
        for a in arms:
            r = out.get(a, {}).get(n, {})
            ev = [x for x in r.get("events") or []]
            cells.append("-" if "events" not in r else "%d / %d" % (len(ev), sum(1 for x in ev if not x)))
        print("%-36s %s" % (n, "  ".join("%-26s" % x for x in cells)))
    print("\n%-36s %s" % ("new records, root / partition", "  ".join("%-26s" % a for a in arms)))
    for n in names:
        cells = []
        for a in arms:
            new = out.get(a, {}).get(n, {}).get("new")
            cells.append("-" if new is None else " / ".join(str(sum(new.get(k, {}).values())) if k in new else "-"
                                                             for k in ("root", "partition")))
        print("%-36s %s" % (n, "  ".join("%-26s" % x for x in cells)))
    print("\nwhat a member of %s got with a token naming %s, and the records it made (root / partition)" % (W.OTHER_WS, P.WS))
    for n in everything:
        if n.startswith(STRANGER):
            print("  %s" % n[len(STRANGER):])
            for a in arms:
                r = out.get(a, {}).get(n, {})
                new = r.get("new") or {}
                print("      %-10s %s%s; made %s" % (a, r.get("got"), "; %s" % r["error"][:110] if r.get("error") else "",
                                                 " / ".join(str(sum(new.get(k, {}).values())) if k in new else "-"
                                                            for k in ("root", "partition"))))
    for a in arms[1:]:
        for n in names:
            r, base = out.get(a, {}).get(n, {}), out.get("today", {}).get(n, {})
            if r.get("ok") and base.get("ok"):
                d = differ(base["answer"], r["answer"])
                if d:
                    print("\n%s, %s: the answer differs from today's at %s" % (a, n, d[:8]))
    print("\nevery request lambda made, by flow: %s" % DETAIL)
    P.save("target_lambda", out)


if __name__ == "__main__":
    main()
