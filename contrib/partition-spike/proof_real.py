#!/usr/bin/env python3
"""Proof point for partition namespaces (spike): Jobli's own queries, with their
auth rules, on the restored dev data.

The earlier speed test compared a plain workspace filter with a partition on
synthetic data. This one runs the queries the Jobli web app sends, read from its
source, as a real member of a real workspace, against the dev backup:

  today        everything in the root namespace, partition namespaces off, the
               schema and auth rules exactly as restored
  today again  the root namespace once more, on the store that by then also
               holds the partition: the control for the arms below
  partition    the same workspace alone in a partition, selected by the ws claim
               of the same token, auth rules unchanged
  ws rules     the partition again, with the WorkspaceMember rule replaced by a
   off         check that the ws claim is set (role and group rules stay)
  no rules     the partition again with every auth rule removed: not a design,
               only the floor that shows what the remaining rules cost

All run on one cluster and one data directory, from the same binary. The
partition is filled from an RDF export of the root namespace, keeping every
UID, so the same query must return the same bytes in each arm. Where it does
not, the script names the records that differ and why.

usage: SPIKE_SUB=<a member's userId> SPIKE_EMAIL=<their email> \
       proof_real.py [restore] [today] [extract] [load] [relayout] [partition] [report]
       With no stage named, all of them run in that order.
       SPIKE_WS names the workspace (default dc98a028).
       SPIKE_ARMS="ws_rules_off,foreign" reruns only those parts of the partition stage.
"""

import base64
import concurrent.futures
import glob
import gzip
import hashlib
import hmac
import http.server
import json
import os
import re
import shutil
import statistics
import sys
import threading
import time

from spikelib import ROOT, Checks, Cluster

JOBLI = os.environ.get("JOBLI_ROOT", "/Users/idowuayoola/Documents/jobli")
BACKUP = os.environ.get("SPIKE_BACKUP", os.path.join(JOBLI, "graph/dgraph/alpha/data/backups/dgraph_20261123"))
APP = os.path.join(JOBLI, "nextgorillaapp")
OUT = os.path.join(ROOT, "run", "real-out")

WS = os.environ.get("SPIKE_WS", "dc98a028")
# The member of WS the token is signed for. From the environment, so that nobody's
# identity is kept in the repository.
SUB = os.environ.get("SPIKE_SUB", "")
EMAIL = os.environ.get("SPIKE_EMAIL", "")
# The verification key the dev schema declares in Dgraph.Authorization.
SECRET, HEADER, AUDIENCE = "mysecret", "X-Auth-Token", "urn://api.jobli.io/graphql"

OFF = "partition-namespaces=false"
ON = "partition-namespaces=true; partition-resolver=jwt; partition-claim=ws; partition-dir-ttl=30s"
HEADER_MODE = "partition-namespaces=true; partition-resolver=header"
ALPHA_ARGS = ["--cache", "size-mb=1024"]
# Jobli's schema has @lambda fields and validation expressions that call a lambda,
# and the alpha tries those out when it loads the schema. A stub answers them. None
# of the queries below reaches a lambda field.
LAMBDA_PORT = 8786
LAMBDA = "lambda-url=http://localhost:%d/graphql-worker" % LAMBDA_PORT

RUNS, WARMUP, BUDGET_S = 30, 5, 90
PAGE = 25
CLIENTS, LOAD_S = 8, 15
# SPIKE_ONLY="Company,Job" measures only the queries whose label contains one of
# these, and merges them into the results already saved for the arm.
ONLY = [x for x in os.environ.get("SPIKE_ONLY", "").split(",") if x]

DIRECTORY = """
WorkspaceEntry.name: string @index(exact) @upsert .
WorkspaceEntry.namespace: int .
"""


# ---------------------------------------------------------------- lambda stub

class LambdaStub(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        body = b'{"code": 200}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


def start_lambda_stub():
    server = http.server.ThreadingHTTPServer(("127.0.0.1", LAMBDA_PORT), LambdaStub)
    threading.Thread(target=server.serve_forever, daemon=True).start()


# ---------------------------------------------------------------- tokens

def b64(raw):
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def token(ws=WS, sub=SUB, email=EMAIL):
    """The shape graph/scripts/generate_token.py signs for local testing."""
    now = int(time.time())
    body = {"aud": [AUDIENCE], "azp": "integration-test", "email": email, "exp": now + 24 * 3600, "iat": now,
            "iss": "urn://api.jobli.io", "scope": ["_all"], "sub": sub, "ws": ws}
    signing = b64(json.dumps({"alg": "HS256", "typ": "JWT"}).encode()) + "." + b64(json.dumps(body).encode())
    return signing + "." + b64(hmac.new(SECRET.encode(), signing.encode(), hashlib.sha256).digest())


# ---------------------------------------------------------------- the app's queries

def app_query(path, name, _seen=None):
    """The text of an exported gql document in the web app, with its fragments."""
    src = open(os.path.join(APP, path)).read()
    m = re.search(r"export const %s = gql`(.*?)`" % re.escape(name), src, re.S)
    if not m:
        raise SystemExit("%s not found in %s" % (name, path))
    text = m.group(1)
    for frag in re.findall(r"\$\{(\w+)\}", text):
        text = text.replace("${%s}" % frag, app_query(path, frag))
    return text


def suite(params):
    """(label, document, variables). Variables follow what the app's hooks build."""
    ws_id = params["ws_id"]
    live = {"and": [{"and": [{"deleted": False}]}]}                        # useCollectionQuery
    order = {"desc": "updatedAt", "then": {"desc": "createdAt", "then": {"asc": "xId"}}}  # buildStableCollectionOrder
    scope = {"and": [{"deleted": False}, {"inGroup": {"inWorkspace": {"id": [ws_id]}}}]}  # workspaceResourceFilter
    rx = "/%s/i" % params["term"]
    person = [{"displayName": {"regexp": rx}}, {"firstName": {"regexp": rx}}, {"lastName": {"regexp": rx}},
              {"email": {"regexp": rx}}, {"otherEmails": {"regexp": rx}}]

    def with_text(clauses):
        return {"and": [scope, {"or": clauses}]}

    kinds = ["candidate", "contact", "company", "job", "jobAd", "application", "placement"]
    text = {
        "candidate": with_text(person),
        "contact": with_text(person + [{"forCompany": {"name": {"regexp": rx}}}]),
        "company": with_text([{"name": {"regexp": rx}}, {"website": {"regexp": rx}}, {"email": {"regexp": rx}}]),
        "job": with_text([{"title": {"regexp": rx}}, {"forCompany": {"name": {"regexp": rx}}}]),
        "jobAd": with_text([{"title": {"regexp": rx}}, {"contactEmail": {"regexp": rx}}]),
        "application": with_text([{"hasCandidate": {"or": person}}, {"forJob": {"title": {"regexp": rx}}}]),
        "placement": with_text([{"forApplication": {"hasCandidate": {"or": person}}},
                                {"forApplication": {"forJob": {"title": {"regexp": rx}}}}]),
    }
    cand_list = app_query("api/candidate.ts", "QUERY_CANDIDATE_FOR_LIST")
    return [
        ("Candidate list, page 1, with count", cand_list,
         {"offset": 0, "first": PAGE, "filter": live, "order": order}),
        ("Candidate list, page 20, with count", cand_list,
         {"offset": 19 * PAGE, "first": PAGE, "filter": live, "order": order}),
        ("Candidate list, text search, with count", cand_list,
         {"offset": 0, "first": PAGE, "filter": {"and": [{"and": [{"deleted": False}]}, {"or": person}]},
          "order": order}),
        ("Candidate count", app_query("api/candidate.ts", "QUERY_CANDIDATE_COUNT"), {"filter": live}),
        ("Candidate detail", app_query("api/candidate.ts", "GET_CANDIDATE"), {"id": params["candidate_id"]}),
        ("Candidate by email", app_query("api/candidate.ts", "QUERY_CANDIDATE_BY_EMAIL"),
         {"email": params["candidate_email"], "workspaceId": ws_id}),
        ("Candidate picker, typed text", app_query("api/candidate.ts", "QUERY_CANDIDATE_FOR_DROPDOWN"),
         {"workspaceId": ws_id, "offset": 0, "first": 50, "order": {"desc": "updatedAt"}, "nameRegex": rx,
          "firstRegex": rx, "lastRegex": rx, "emailRegex": rx}),
        ("Job list, page 1, with count", app_query("api/job.ts", "QUERY_JOB_FOR_LIST"),
         {"offset": 0, "first": PAGE, "filter": live, "order": order}),
        ("Application list, page 1, with count", app_query("api/application.ts", "QUERY_APPLICATION_FOR_LIST"),
         {"offset": 0, "first": PAGE, "filter": live, "order": order}),
        ("Company list, page 1, with count", app_query("api/company.ts", "QUERY_COMPANY_FOR_LIST"),
         {"offset": 0, "first": PAGE, "filter": live, "order": order}),
        ("Chat # menu, 7 record kinds, newest 5", app_query("api/chatTagSearch.ts", "QUERY_CHAT_TAG_SEARCH"),
         dict({k: scope for k in kinds}, first=5)),
        ("Chat # menu, 7 record kinds, typed text", app_query("api/chatTagSearch.ts", "QUERY_CHAT_TAG_SEARCH"),
         dict(text, first=5)),
    ]


# ---------------------------------------------------------------- measuring

def gql(c, tok, query, variables, debug=False):
    return c.post("/graphql" + ("?debug=true" if debug else ""),
                  json.dumps({"query": query, "variables": variables}), headers={HEADER: tok})


def answer(r):
    """What the client gets: the data, and the messages of any errors beside it. The dev
    data has records that break the schema's non-null rules, and the app sees those too."""
    return canonical({"data": r.get("data"), "errors": sorted(e.get("message", "") for e in r.get("errors") or [])})


def canonical(data):
    return json.dumps(data, sort_keys=True, separators=(",", ":"))


def rows_in(data):
    n = 0
    for v in (data or {}).values():
        n += len(v) if isinstance(v, list) else (1 if v else 0)
    return n


def pct(values, p):
    values = sorted(values)
    return values[min(len(values) - 1, int(len(values) * p))]


def measure(c, tok, arm, params):
    """Runs the suite one request at a time, then one query under concurrent load."""
    out = load(arm, {}) if ONLY else {}
    os.makedirs(os.path.join(OUT, arm), exist_ok=True)
    for label, query, variables in suite(params):
        if ONLY and not any(o in label for o in ONLY):
            continue
        r = gql(c, tok, query, variables)
        if not r.get("data"):
            print("  %-42s ERROR %s" % (label, json.dumps(r.get("errors"))[:300]), flush=True)
            out[label] = {"error": (r.get("errors") or [{}])[0].get("message", "")[:300]}
            continue
        for _ in range(WARMUP - 1):
            gql(c, tok, query, variables)
        wall, start = [], time.time()
        while len(wall) < RUNS and (len(wall) < 5 or time.time() - start < BUDGET_S):
            t = time.perf_counter()
            r = gql(c, tok, query, variables)
            wall.append((time.perf_counter() - t) * 1000)
        data = r.get("data")
        with open(os.path.join(OUT, arm, re.sub(r"\W+", "_", label) + ".json"), "w") as f:
            f.write(answer(r))
        ext = r.get("extensions") or {}
        out[label] = {"p50": statistics.median(wall), "p95": pct(wall, 0.95), "min": min(wall), "runs": len(wall),
                      "sha": hashlib.sha256(answer(r).encode()).hexdigest(), "rows": rows_in(data),
                      "touched": ext.get("touched_uids", 0), "errors": len(r.get("errors") or [])}
        print("  %-42s p50 %9.1f ms  p95 %9.1f ms  rows %4d  touched %9d  (%d runs)%s" % (
            label, out[label]["p50"], out[label]["p95"], out[label]["rows"], out[label]["touched"], len(wall),
            "  %d errors beside the data" % out[label]["errors"] if out[label]["errors"] else ""), flush=True)

    # Throughput: the list page, the request the app sends most, from several clients at once.
    label, query, variables = suite(params)[0]
    if not ONLY and "error" not in out.get(label, {"error": 1}):
        end = time.time() + LOAD_S

        def client(_):
            lat = []
            while time.time() < end:
                t = time.perf_counter()
                gql(c, tok, query, variables)
                lat.append((time.perf_counter() - t) * 1000)
            return lat
        with concurrent.futures.ThreadPoolExecutor(CLIENTS) as pool:
            lat = [x for part in pool.map(client, range(CLIENTS)) for x in part]
        out["_load"] = {"rps": len(lat) / LOAD_S, "p50": statistics.median(lat), "p95": pct(lat, 0.95)}
        print("  %d clients on the list page for %ds: %.1f requests/s, p50 %.0f ms, p95 %.0f ms" % (
            CLIENTS, LOAD_S, out["_load"]["rps"], out["_load"]["p50"], out["_load"]["p95"]), flush=True)
    return out


def compiled(c, tok, arm, params):
    """Size of the DQL each query compiles to, from the alpha's ?debug=true answer."""
    out = {}
    for label, query, variables in suite(params):
        r = gql(c, tok, query, variables, debug=True)
        dql = (r.get("extensions") or {}).get("dql_query", "")
        with open(os.path.join(OUT, arm, re.sub(r"\W+", "_", label) + ".dql"), "w") as f:
            f.write(dql)
        # Every query block has exactly one root function.
        out[label] = {"blocks": len(re.findall(r"\(func:", dql)), "bytes": len(dql)}
    return out


def save(name, obj):
    os.makedirs(OUT, exist_ok=True)
    with open(os.path.join(OUT, name + ".json"), "w") as f:
        json.dump(obj, f, indent=1)


def load(name, default=None):
    try:
        return json.load(open(os.path.join(OUT, name + ".json")))
    except OSError:
        if default is not None:
            return default
        raise SystemExit("missing %s.json: run the earlier stage first" % name)


def cluster(flags, fresh=False):
    return Cluster("rj", 700, flags=flags, extra_alpha_args=ALPHA_ARGS + ["--graphql", LAMBDA], fresh=fresh)


def restart(c, debug=False):
    """Every arm starts from a freshly started alpha, so none inherits a warm cache.
    The alpha only reports the DQL it compiled when started in GraphQL debug mode;
    timings are taken with it off, as Jobli runs."""
    c.stop_alpha(0)
    c.extra = ALPHA_ARGS + ["--graphql", LAMBDA + ("; debug=true" if debug else "")]
    c.start_alpha(0)
    c.wait_ready(0, timeout=900)


def get_schema(c):
    return c.admin("{ getGQLSchema { schema } }")["getGQLSchema"]["schema"]


def set_schema(c, schema):
    r = c.post("/admin", json.dumps({
        "query": "mutation($s: String!) { updateGQLSchema(input: { set: { schema: $s } }) { gqlSchema { id } } }",
        "variables": {"s": schema}}))
    if not r.get("errors"):
        c.wait_idle(ops=("opIndexing",))
        time.sleep(2)
    return r


# ---------------------------------------------------------------- schema variants

def strip_directives(schema, names, keep=lambda body: False):
    """Removes `@name(...)` for the given directive names, wherever it is used as a
    directive: not inside a string, a description or a comment."""
    out, i, n = [], 0, len(schema)
    removed = 0

    def skip_string(j):
        if schema.startswith('"""', j):
            return schema.index('"""', j + 3) + 3
        j += 1
        while schema[j] != '"':
            j += 2 if schema[j] == "\\" else 1
        return j + 1

    while i < n:
        ch = schema[i]
        if ch == '"':
            j = skip_string(i)
            out.append(schema[i:j])
            i = j
        elif ch == "#":
            j = schema.find("\n", i)
            j = n if j < 0 else j
            out.append(schema[i:j])
            i = j
        elif ch == "@":
            m = re.match(r"@(\w+)", schema[i:])
            if m and m.group(1) in names:
                j = i + len(m.group(0))
                k = j
                while k < n and schema[k] in " \t\r\n":
                    k += 1
                if k < n and schema[k] == "(":
                    depth, k = 1, k + 1
                    while depth:
                        if schema[k] == '"':
                            k = skip_string(k)
                            continue
                        if schema[k] == "#":
                            k = schema.index("\n", k)
                            continue
                        depth += {"(": 1, ")": -1}.get(schema[k], 0)
                        k += 1
                    j = k
                if keep(schema[i:j]):
                    out.append(schema[i:j])
                else:
                    removed += 1
                i = j
            else:
                out.append(ch)
                i += 1
        else:
            out.append(ch)
            i += 1
    return "".join(out), removed


WS_RULE = re.compile(r'rule:\s*"""\s*query \(\$ws: String\) \{\s*queryWorkspaceMember \{\s*inWorkspace\s*'
                     r'\(filter: \{ name: \{ eq: \$ws \} \}\)\s*\{\s*__typename\s*\}\s*\}\s*\}\s*"""')
WS_CLAIM = 'rule: "{ $ws: { notIn: [\\"*\\", \\"\\"] } }"'


def schema_without_workspace_rules(schema):
    """Replaces the rule a partition makes redundant, and nothing else.

    Every tenant type implements WorkspaceMember, whose @auth has one rule for each
    of query, add, update and delete: the record's workspace is the one named by the
    ws claim. In a partition that is true of every record there is. The rule becomes
    the check the schema already uses on WorkspaceMember.inWorkspace, that the claim
    is set, which is decided from the token without a query.

    Left alone: the role, group, ownership and deleted-record rules, the @cascadeAuth
    edges, and the 15 places where a role rule names the workspace on its way to the
    caller's bindings.
    """
    start = schema.index("interface WorkspaceMember")
    end = schema.index("\ninterface ", start + 10)
    body, n = WS_RULE.subn(lambda m: WS_CLAIM, schema[start:end])
    return schema[:start] + body + schema[end:], {"rules": n}


def schema_without_rules(schema):
    s, n = strip_directives(schema, {"auth", "cascadeAuth", "cascadeAuthPolicy"})
    return s, {"directives": n}


# ---------------------------------------------------------------- stages

def stage_restore(t):
    with cluster(OFF, fresh=True) as c:
        start = time.time()
        r = c.admin('mutation { restore(input: {location: "%s"}) { code message } }' % BACKUP)
        c.wait_idle(ops=("opRestore",), timeout=3600)
        c.wait_ready(0, timeout=600)
        took = time.time() - start
        n = c.rows("{ q(func: has(dgraph.type)) { count(uid) } }")
        ws = c.rows('{ q(func: eq(Workspace.name, "%s")) { uid c: count(Workspace.hasCandidate) } }' % WS)
        t.check("dev backup restored into the root namespace in %.0fs" % took,
                isinstance(n, list) and n and n[0]["count"] > 1000000, "%s typed nodes; %s" % (n, r))
        t.check("workspace %s is there" % WS, isinstance(ws, list) and len(ws) == 1, ws)
        schema = get_schema(c)
        with open(os.path.join(OUT, "schema_today.graphql"), "w") as f:
            f.write(schema)
        t.check("its GraphQL schema came with it", "Dgraph.Authorization" in schema, "%d bytes" % len(schema))


def stage_today(t):
    with cluster(OFF) as c:
        tok = token()
        ws = c.rows('{ q(func: eq(Workspace.name, "%s")) { uid } }' % WS)[0]["uid"]
        me = c.rows('{ q(func: eq(User.userId, "%s")) @filter(uid_in(User.inWorkspace, %s) OR '
                    'uid_in(WorkspaceMember.inWorkspace, %s)) { uid User.email User.status } }' % (SUB, ws, ws))
        t.check("the token's user is an active member of %s" % WS,
                isinstance(me, list) and any(m.get("User.status") == "ACTIVE" for m in me), me)

        # Pick the record and the search text from the data, once, for every arm.
        first = gql(c, tok, "query { queryCandidate(filter: {deleted: false, has: [email, lastName]}, "
                            "order: {desc: updatedAt}, first: 1) { id email lastName } }", {})
        cand = ((first.get("data") or {}).get("queryCandidate") or [None])[0]
        t.check("GraphQL answers as that user", bool(cand), first.get("errors") or first.get("data"))
        if not cand:
            return
        params = {"ws_id": ws, "candidate_id": cand["id"], "candidate_email": cand["email"],
                  "term": re.sub(r"[^a-z]", "", cand["lastName"].lower())[:4] or "smith"}
        save("params", params)
        print("today: root namespace, partition namespaces off (search text %r)" % params["term"], flush=True)
        res = measure(c, tok, "today", params)
        other = gql(c, token(ws="not-" + WS), suite(params)[3][1], suite(params)[3][2])
        t.check("a token for another workspace name counts no candidates here",
                ((other.get("data") or {}).get("count") or {}).get("count") in (0, None), other.get("data"))
        save("today", res)
        t.check("today: every query answered", all("error" not in v for k, v in res.items() if k[0] != "_"))


EDGE = re.compile(rb"^<(0x[0-9a-f]+)> <([^>]+)> <(0x[0-9a-f]+)>")
SUBJ = re.compile(rb"^<(0x[0-9a-f]+)> <([^>]+)> ")
NS_LABEL = re.compile(rb" <0x[0-9a-f]+> \.$")
TYPE = re.compile(rb'^<(0x[0-9a-f]+)> <dgraph\.type> "([^"]+)"')


def stage_extract(t):
    """Exports the root namespace and keeps the triples of one workspace.

    Today a record says which workspace it is in through `inWorkspace`, or through
    the groups it is in (`Groupable.inGroup`); everything else hangs off a record
    that does. So:

    - a node is the workspace's if one of those edges says so, or if it says
      nothing and is reached from a node that is
    - a node that belongs to another workspace is never followed
    - where the workspace's records point at another workspace's user or app
      (`createdBy`, `fromSource`: the app registry and people who act in several
      workspaces), that one node is copied without its edges. The design reads
      those from namespace 0 through a read-only edge, which the spike does not
      have, so the test carries a copy instead. Business records of another
      workspace are not copied at all.
    """
    with cluster(OFF) as c:
        shutil.rmtree(os.path.join(c.alpha_dir(0), "export"), ignore_errors=True)
        start = time.time()
        # Export is queued as a task and the mutation returns at once; wait for the task.
        task = c.admin('mutation { export(input: {format: "rdf"}) { taskId } }')["export"]["taskId"]
        status = "Queued"
        while status in ("Queued", "Running", "Unknown"):
            time.sleep(2)
            status = c.admin('{ task(input: {id: "%s"}) { status } }' % task)["task"]["status"]
        files = glob.glob(os.path.join(c.alpha_dir(0), "export", "*", "*.rdf.gz"))
        t.check("root namespace exported in %.0fs" % (time.time() - start), status == "Success" and files, status)
        ws = int(c.rows('{ q(func: eq(Workspace.name, "%s")) { uid } }' % WS)[0]["uid"], 16)

    start = time.time()
    out_edges, owner, groups, workspaces, kinds, triples = {}, {}, {}, set(), {}, 0
    counted = (b"Candidate", b"Job", b"JobAd", b"Application", b"Company", b"Contact", b"Placement", b"Note",
               b"User", b"Group")
    for path in files:
        with gzip.open(path, "rb") as f:
            for line in f:
                triples += 1
                m = EDGE.match(line)
                if m:
                    s, o = int(m.group(1), 16), int(m.group(3), 16)
                    out_edges.setdefault(s, []).append(o)
                    if m.group(2).endswith(b"inWorkspace"):
                        owner[s] = o
                    elif m.group(2) == b"Groupable.inGroup":
                        groups.setdefault(s, []).append(o)
                    continue
                m = TYPE.match(line)
                if m:
                    if m.group(2) == b"Workspace":
                        workspaces.add(int(m.group(1), 16))
                    elif m.group(2) in counted:
                        kinds[int(m.group(1), 16)] = m.group(2).decode()
    print("  read %d triples, %d nodes with edges, %d workspaces (%.0fs)" % (
        triples, len(out_edges), len(workspaces), time.time() - start), flush=True)

    def belongs(x):
        """True: this workspace's. False: another's. None: it does not say."""
        if x in workspaces:
            return x == ws
        if x in owner:
            return owner[x] == ws
        said = [owner[g] == ws for g in groups.get(x, ()) if g in owner]
        return any(said) if said else None

    keep = {x for x in list(owner) + list(groups) if belongs(x)} | {ws}
    refs, left_out = set(), set()
    frontier = list(keep)
    while frontier:
        nxt = []
        for x in frontier:
            for o in out_edges.get(x, ()):
                if o in keep or o in refs or o in left_out:
                    continue
                mine = belongs(o)
                if mine is False:
                    # Another workspace's node. Copy a user or an app as a leaf; leave records behind.
                    (left_out if o in groups or o in workspaces else refs).add(o)
                else:
                    keep.add(o)
                    nxt.append(o)
        frontier = nxt
    del out_edges
    have = {}
    for x in keep:
        if x in kinds:
            have[kinds[x]] = have.get(kinds[x], 0) + 1
    ref_kinds = {}
    for x in refs:
        ref_kinds[kinds.get(x, "other")] = ref_kinds.get(kinds.get(x, "other"), 0) + 1

    start = time.time()
    kept, dropped_edges = 0, 0
    with open(os.path.join(OUT, "workspace.rdf"), "wb") as w:
        for path in files:
            with gzip.open(path, "rb") as f:
                for line in f:
                    m = SUBJ.match(line)
                    if not m or m.group(2).startswith(b"dgraph.graphql"):
                        continue
                    s = int(m.group(1), 16)
                    e = EDGE.match(line)
                    if s in refs:
                        if e:
                            continue
                    elif s not in keep:
                        continue
                    elif e and int(e.group(3), 16) not in keep and int(e.group(3), 16) not in refs:
                        dropped_edges += 1
                        continue
                    w.write(NS_LABEL.sub(b" .", line.rstrip(b"\n")) + b"\n")
                    kept += 1
    stats = {"workspace_uid": hex(ws), "triples_in_root": triples, "nodes": len(keep), "reference_copies": len(refs),
             "reference_kinds": ref_kinds, "records_of_others_left_out": len(left_out), "triples": kept,
             "edges_left_behind": dropped_edges, "kept_by_type": have}
    save("extract", stats)
    print("  wrote %d triples for %d nodes (%.0fs): %s" % (kept, len(keep), time.time() - start, have), flush=True)
    t.check("workspace %s extracted: %d of %d triples, %d nodes" % (WS, kept, triples, len(keep)), kept > 100000)
    t.check("%d nodes of other workspaces are referenced from it and copied as leaves" % len(refs),
            len(refs) < len(keep) / 100, ref_kinds)
    print("  %d records of other workspaces that it points at were left out, with %d edges"
          % (len(left_out), dropped_edges), flush=True)


def stage_load(t):
    with cluster(ON) as c:
        tok = token()
        r = c.alter(DIRECTORY)
        t.check("directory predicates added to the root namespace", not r.get("errors"), r.get("errors", ""))
        found = c.rows('{ q(func: eq(WorkspaceEntry.name, "%s")) { ns: WorkspaceEntry.namespace } }' % WS)
        if found:
            # A rerun: empty the partition first. Only the token can name it; a namespace
            # header has no standing with the claim resolver and would act on the root.
            ns = found[0]["ns"]
            r = c.post("/alter", '{"drop_op": "DATA"}', headers={HEADER: tok})
            t.check("partition %d emptied for the reload" % ns, not r.get("errors"), r.get("errors", ""))
        else:
            ns = c.add_partition()
            c.mutate('_:w <WorkspaceEntry.name> "%s" .\n_:w <WorkspaceEntry.namespace> "%d"^^<xs:int> .' % (WS, ns))
            time.sleep(3)
        empty = c.post("/query", "{ q(func: has(dgraph.type)) { count(uid) } }", ctype="application/dql",
                       headers={HEADER: tok})
        t.check("the token's ws claim leads to partition %d, which is empty" % ns,
                (empty.get("data") or {}).get("q") == [{"count": 0}], empty.get("errors") or empty.get("data"))

        def batches():
            buf, size = [], 0
            with open(os.path.join(OUT, "workspace.rdf"), "rb") as f:
                for line in f:
                    buf.append(line)
                    size += len(line)
                    if len(buf) >= 4000 or size > 2 << 20:
                        yield b"".join(buf)
                        buf, size = [], 0
            if buf:
                yield b"".join(buf)

        def send(body, attempts=6):
            for attempt in range(attempts):
                r = c.post("/mutate?commitNow=true", b"{ set {\n" + body + b"} }", ctype="application/rdf",
                           headers={HEADER: tok})
                if not r.get("errors"):
                    return None
                time.sleep(0.05 * (attempt + 1))
            return body, r["errors"][0].get("message", "")[:200]

        # Batches that keep colliding with their neighbours are sent again afterwards, one at a time.
        start = time.time()
        with concurrent.futures.ThreadPoolExecutor(8) as pool:
            again = [e for e in pool.map(send, batches()) if e]
        failed = [e[1] for e in (send(body, 40) for body, _ in again) if e]
        took = time.time() - start
        print("  %d batches had to be sent again on their own" % len(again), flush=True)
        stats = load("extract")
        t.check("%d triples written into the partition through the token in %.0fs" % (stats["triples"], took),
                not failed, failed[:2])
        n = c.post("/query", "{ q(func: has(dgraph.type)) { count(uid) } }", ctype="application/dql",
                   headers={HEADER: tok})
        save("load", {"namespace": ns, "seconds": took, "typed_nodes": (n.get("data") or {}).get("q")})
        _, tablets, _ = c.state()
        t.check("no tablets outside the root namespace", all(x.startswith("0-") for x in tablets),
                "%d tablets" % len(tablets))


def stage_relayout(t):
    """Backs the cluster up and restores it in place.

    The root namespace's data arrived by restore, in sorted tables with every
    posting list complete. The partition's arrived as 16 million edges through
    mutations, and would be read through layers the background has not merged
    yet. After this both are laid out the same way, so the arms differ in where
    the data is and not in how it got there. It is also a backup and restore
    round trip of a real workspace in a partition.
    """
    bdir = os.path.join(ROOT, "run", "real-backup")
    shutil.rmtree(bdir, ignore_errors=True)
    os.makedirs(bdir)
    with cluster(ON) as c:
        tok = token()

        def counts():
            q = ("{ n(func: has(dgraph.type)) { count(uid) } c(func: type(Candidate)) { count(uid) } "
                 "j(func: type(Job)) { count(uid) } e(func: has(Candidate.email)) { count(uid) } }")
            mine = c.post("/query", q, ctype="application/dql", headers={HEADER: tok}).get("data")
            return canonical(mine), canonical(c.dql(q).get("data"))
        before = counts()
        start = time.time()
        c.admin('mutation { backup(input: {destination: "%s", forceFull: true}) { response { code } } }' % bdir)
        while not os.path.exists(os.path.join(bdir, "manifest.json")):
            time.sleep(2)
        c.wait_idle(ops=("opBackup",), timeout=7200)
        size = sum(os.path.getsize(f) for f in glob.glob(os.path.join(bdir, "**", "*.backup"), recursive=True))
        backup_s, start = time.time() - start, time.time()
        c.admin('mutation { restore(input: {location: "%s"}) { code message } }' % bdir)
        c.wait_idle(ops=("opRestore",), timeout=7200)
        c.wait_ready(0, timeout=900)
        after = counts()
        t.check("backup (%.0fs, %d MB) and restore in place (%.0fs): the partition and the root namespace "
                "count the same as before" % (backup_s, size >> 20, time.time() - start), before == after,
                "partition %s" % after[0])
        save("relayout", {"backup_s": backup_s, "backup_mb": size >> 20, "restore_s": time.time() - start,
                          "partition": json.loads(after[0]), "root": json.loads(after[1])})


def foreign_records(t, c, tok, params):
    """Where the partition's answer differs from today's, which records are they?

    With the header resolver the same token can be sent to the root namespace and to
    the partition, so both answers come from one alpha.
    """
    c.flags = HEADER_MODE
    restart(c)
    q = "query { queryJob(filter: {deleted: false}, first: 1000000) { id } }"

    def ids(ns):
        r = c.post("/graphql", json.dumps({"query": q}), ns=ns, headers={HEADER: tok})
        return {x["id"] for x in (r.get("data") or {}).get("queryJob") or []}
    today, mine = ids(None), ids(1)
    extra = sorted(today - mine)
    homes = {}
    if extra:
        rows = c.rows("{ q(func: uid(%s)) { uid Groupable.inGroup { WorkspaceMember.inWorkspace { n: Workspace.name } } } }"
                      % ", ".join(extra))
        for row in rows:
            homes[row["uid"]] = sorted({g["WorkspaceMember.inWorkspace"]["n"] for g in row.get("Groupable.inGroup", [])
                                        if g.get("WorkspaceMember.inWorkspace")})
    # Candidates the workspace rule hides today because they carry no inWorkspace edge,
    # though their group is the workspace's. Without the rule they are visible.
    q = ("{ q(func: type(Candidate)) @filter(NOT eq(Recordable.deleted, true) AND "
         "NOT has(WorkspaceMember.inWorkspace)) { count(uid) } }")
    unlabelled = c.rows(q, ns=1)[0]["count"]

    def counted(arm):
        try:
            return json.load(open(os.path.join(OUT, arm, "Candidate_count.json")))["data"]["count"]["count"]
        except (OSError, KeyError, TypeError):
            return None
    with_rule, without = counted("partition"), counted("ws_rules_off")
    save("foreign", {"today": len(today), "partition": len(mine), "homes": homes, "unlabelled_candidates": unlabelled,
                     "candidates_with_rule": with_rule, "candidates_without_rule": without})
    if with_rule is not None and without is not None:
        t.check("candidates: %d with the workspace rule, %d without it, and the difference is the %d that have "
                "no inWorkspace edge" % (with_rule, without, unlabelled), without - with_rule == unlabelled)
    t.check("jobs: the partition returns %d, today's rules return those and %d more, and every one of the extra "
            "jobs belongs to another workspace" % (len(mine), len(extra)),
            mine <= today and all(h and WS not in h for h in homes.values()) and len(homes) == len(extra), homes)


def stage_partition(t):
    today, params = load("today"), load("params")
    original = open(os.path.join(OUT, "schema_today.graphql")).read()
    ws_off, n_ws = schema_without_workspace_rules(original)
    none, n_none = schema_without_rules(original)
    # The first arm repeats today's measurement on the store as it is now, partition and
    # all. With the header resolver a request that names no partition runs in the root.
    arms = [("today_again", "today again: the root namespace, on the store that now also holds the partition",
             original, HEADER_MODE),
            ("partition", "partition, auth rules unchanged", original, ON),
            ("ws_rules_off", "partition, the workspace rule replaced by a claim check %s" % n_ws, ws_off, ON),
            ("no_rules", "partition, no auth rules at all %s" % n_none, none, ON)]
    wanted = [a for a in os.environ.get("SPIKE_ARMS", "").split(",") if a]
    with cluster(ON) as c:
        tok = token()
        try:
            for arm, title, schema, flags in arms:
                if wanted and arm not in wanted:
                    continue
                c.flags = flags
                restart(c)
                if get_schema(c) != schema:
                    r = set_schema(c, schema)
                    if r.get("errors"):
                        t.check("%s: schema accepted" % arm, False, r["errors"][0].get("message", "")[:300])
                        continue
                print(title, flush=True)
                res = measure(c, tok, arm, params)
                restart(c, debug=True)
                res["_compiled"] = compiled(c, tok, arm, params)
                save(arm, res)
                same = [k for k, v in res.items() if k[0] != "_" and v.get("sha") == today.get(k, {}).get("sha")]
                asked = [k for k in res if k[0] != "_"]
                # The job list may differ, and without the workspace rule so may the candidate
                # count: foreign_records accounts for both, record by record.
                differ = [k for k in asked if k not in same]
                t.check("%s: %d of %d queries return exactly what today's namespace returned" % (
                    arm, len(same), len(asked)),
                    arm == "no_rules" or all(k.startswith("Job list") or (arm == "ws_rules_off" and
                                             k.startswith("Candidate") and "count" in k.lower()) for k in differ),
                    differ[:4])
            if not wanted or "foreign" in wanted:
                foreign_records(t, c, tok, params)
            # The boundary, on real data.
            c.flags = ON
            restart(c)
            other = c.post("/query", "{ q(func: type(Candidate)) { count(uid) } }", ctype="application/dql",
                           headers={HEADER: token(ws="not-" + WS)})
            t.check("a token naming a workspace with no partition is refused", bool(other.get("errors")),
                    (other.get("errors") or [{}])[0].get("message", other.get("data")))
            mine = c.post("/query", "{ q(func: type(Candidate)) { count(uid) } }", ctype="application/dql",
                          headers={HEADER: tok})
            root = c.rows("{ q(func: type(Candidate)) { count(uid) } }")
            t.check("hand-written DQL with the workspace's token sees its candidates only",
                    (mine.get("data") or {}).get("q", [{}])[0].get("count", 0) < root[0]["count"],
                    "%s in the partition, %s in the root namespace" % ((mine.get("data") or {}).get("q"), root))
        finally:
            c.flags = ON
            restart(c)
            set_schema(c, original)


ARMS = [("today", "Today"), ("today_again", "Today, again"), ("partition", "Partition"),
        ("ws_rules_off", "+ ws rules off"), ("no_rules", "No rules")]


def stage_report(t):
    res = {a: load(a, {}) for a, _ in ARMS}
    labels = [k for k in res["today"] if k[0] != "_"]
    print("\nmedian round trip in ms, one request at a time (p95 in brackets)")
    # The gain is the partition's, rules unchanged, over the control on the same store.
    print("%-41s" % "query" + "".join("%20s" % title for _, title in ARMS) + "%8s" % "gain")
    for k in labels:
        line = "%-41s" % k
        for a, _ in ARMS:
            v = res[a].get(k) or {}
            line += "%10.1f (%7.1f)" % (v["p50"], v["p95"]) if "p50" in v else "%20s" % "-"
        base = (res["today_again"].get(k) or res["today"][k]).get("p50")
        mine = (res["partition"].get(k) or {}).get("p50")
        print(line + ("%7.1fx" % (base / mine) if base and mine else ""))
    # Today's DQL is the partition arm's: same schema, same query, same claims.
    cols = [("partition", "Rules unchanged"), ("ws_rules_off", "+ ws rules off"), ("no_rules", "No rules")]
    print("\nDQL blocks each query compiles to / UIDs the server touched")
    print("%-41s%22s" % ("query", "Today") + "".join("%22s" % title for _, title in cols))
    for k in labels:
        blocks = ((res["partition"].get("_compiled") or {}).get(k) or {}).get("blocks", "-")
        line = "%-41s%8s /%12s" % (k, blocks, res["today"][k].get("touched", "-"))
        for a, _ in cols:
            v, d = res[a].get(k) or {}, (res[a].get("_compiled") or {}).get(k) or {}
            line += "%8s /%12s" % (d.get("blocks", "-"), v.get("touched", "-"))
        print(line)
    print("\n%d clients on the candidate list page" % CLIENTS)
    for a, title in ARMS:
        v = res[a].get("_load")
        if v:
            print("  %-16s %8.1f requests/s   p50 %8.0f ms   p95 %8.0f ms" % (title, v["rps"], v["p50"], v["p95"]))


STAGES = [("restore", stage_restore), ("today", stage_today), ("extract", stage_extract), ("load", stage_load),
          ("relayout", stage_relayout), ("partition", stage_partition), ("report", stage_report)]


def main():
    wanted = sys.argv[1:] or [name for name, _ in STAGES]
    if wanted != ["report"] and not (SUB and EMAIL):
        raise SystemExit("set SPIKE_SUB and SPIKE_EMAIL to the userId and email of a member of %s" % WS)
    os.makedirs(OUT, exist_ok=True)
    start_lambda_stub()
    t = Checks()
    for name, fn in STAGES:
        if name in wanted:
            print("== %s" % name, flush=True)
            fn(t)
    sys.exit(t.done())


if __name__ == "__main__":
    main()
