"""Reads and writes on the records the phase 1 repair touches (spike probe, not a
proof: it prints and asserts nothing).

Root namespace of the cluster proof_real.py built, partition namespaces off, rules as
written: how Jobli runs today. For a handful of records of one workspace it takes
away the edges graph/scripts/fix_workspace_and_address_edges.py writes, asks as four
callers, runs the script, and asks again:

  owner     the workspace's owner
  editor    a user made for the test, with the editor role on one group
  outsider  a member of another workspace, with a token for that workspace
  intruder  the same person, with a token that names this workspace

Every mutation in the tables is sent with @dryRun, so none is committed. Then, with
writes that are committed, on records it adds itself:

  - which edges an add writes today (candidate, address, attachment, nested or not)
  - every attachment of the workspace, by what it hangs off, and who reads it
  - an attachment's life: moved between two candidates, left with no owner, deleted,
    and deleted with its candidate, with the change notices the alpha sends to lambda
  - a member of another workspace adding a candidate into a group of this one
  - the two @bypassAuth fields of phase 1

Lambda is a stub that says yes to every check, so quota checks and any permission a
type leaves to lambda are not tested. Everything the probe added is removed again,
with every edge to it, and the schema is put back. SPIKE_CLEAN_AFTER=<uid> only
clears up after a run that died.

  SPIKE_SUB=... SPIKE_EMAIL=... python3 probe_phase1_writes.py
"""
import collections
import http.server
import json
import os
import subprocess
import sys
import threading
import time

import proof_real as P

FIX = os.path.join(P.JOBLI, "graph", "scripts", "fix_workspace_and_address_edges.py")
WSM = "WorkspaceMember.inWorkspace"
EDITOR = "spike-p1-group-editor"
OTHER_WS = os.environ.get("SPIKE_OTHER_WS", "jobadder-test")
CALLERS = ["owner", "editor", "outsider", "intruder"]
LIVE = "NOT eq(Recordable.deleted, true)"
# The attachments of dc98a028 in the dev data that had no workspace edge and no group:
# only the record each is attached to says where it belongs.
ADRIFT = ["0x46a7cd", "0x46a7dd", "0x47e41f", "0x4819b7", "0x482cca", "0x482dea", "0x482e69"]


def uids(v):
    return [x["uid"] for x in (v if isinstance(v, list) else [v] if v else [])]


def rows(c, q):
    r = c.rows(q)
    if isinstance(r, str):
        raise SystemExit("%s\n%s" % (r, q))
    return r


def one(v):
    return v[0] if isinstance(v, list) else v


# ---------------------------------------------------------------- undoing what the probe adds

def new_nodes(c, marker):
    out, after = [], marker
    while True:
        page = uids(rows(c, "{ q(func: has(dgraph.type), first: 10000, after: %s) { uid } }" % after))
        out += page
        if len(page) < 10000:
            return out
        after = page[-1]


def refs(v):
    return [x["uid"] for x in (v if isinstance(v, list) else [v]) if isinstance(x, dict) and "uid" in x]


def edges_after(c, marker):
    """Every edge to a node made after the marker, whichever record holds it. Asks each
    uid predicate in turn: expand(_all_) cannot be used for this, because on this build
    it returns some predicates of a record that has many under a number, not the name."""
    schema = c.dql("schema {}")["data"]["schema"]
    preds = sorted(x["predicate"] for x in schema if x.get("type") == "uid" and not x["predicate"].startswith("dgraph."))
    out = []
    for i in range(0, len(preds), 30):
        chunk = preds[i:i + 30]
        r = c.dql("{ %s }" % " ".join("p%d(func: has(<%s>)) @cascade { uid x: <%s> (after: %s) { uid } }" % (j, x, x, marker)
                                      for j, x in enumerate(chunk)))
        if r.get("errors"):
            raise SystemExit("edge scan: %s" % json.dumps(r["errors"])[:300])
        for j, x in enumerate(chunk):
            for row in r["data"].get("p%d" % j) or []:
                out += [(row["uid"], x, u) for u in refs(row.get("x"))]
    return out


def remove_new(c, marker):
    """Deletes every node made after the marker, and every edge anything has to one."""
    new = new_nodes(c, marker)
    rdf = []
    for u in new:
        rdf += ["<%s> * * ." % u, "<%s> <dgraph.type> * ." % u]
    rdf.append("<%s> <dgraph.type> * ." % marker)
    c.mutate("\n".join(rdf), delete=True)
    edges = edges_after(c, marker)
    if edges:
        c.mutate("\n".join("<%s> <%s> <%s> ." % e for e in edges), delete=True)
    left = rows(c, "{ q(func: has(dgraph.type), after: %s) { uid } }" % marker) + edges_after(c, marker)
    return len(new), len(edges), left


# ---------------------------------------------------------------- the lambda the alpha calls

EVENTS = []
SAMPLES = []


class Stub(http.server.BaseHTTPRequestHandler):
    """Says yes to every check, and keeps what it was asked."""

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        try:
            b = json.loads(raw)
            ev = b.get("event") or {}
            op = ev.get("operation")
            EVENTS.append((b.get("resolver"), ev.get("__typename"), op, len((ev.get(op or "") or {}).get("rootUIDs") or [])))
            if b.get("resolver") == "$webhook" and len(SAMPLES) < 40:
                SAMPLES.append(raw[:700].decode("utf-8", "replace"))
        except (ValueError, AttributeError):
            EVENTS.append(("unreadable", None, None, 0))
        body = b'{"code": 200, "allowed": true}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


def hooks(since):
    """The change notices the alpha sent to lambda since a point, by type and operation."""
    time.sleep(1.5)
    n = collections.Counter("%s %s naming %d record%s" % (t, op, k, "" if k == 1 else "s")
                            for r, t, op, k in EVENTS[since:] if r == "$webhook")
    return ", ".join("%s x%d" % kv for kv in sorted(n.items())) or "none"


def has_edge(c, s, p, o):
    return bool(rows(c, "{ q(func: uid(%s)) @filter(uid_in(%s, %s)) { uid } }" % (s, p, o)))


# ---------------------------------------------------------------- asking

def ask(c, tok, query, variables):
    r = P.gql(c, tok, query, variables)
    d = r.get("data") or {}
    d = next(iter(d.values())) if d else None
    errs = [e.get("message", "") for e in r.get("errors") or []]
    return d, errs


ERRS = []


def cell(c, tok, kind, query, variables, pick=None, note=""):
    d, errs = ask(c, tok, query, variables)
    for m in errs:
        if (note, m) not in ERRS:
            ERRS.append((note, m))
    if errs and d is None:
        return "denied" if "uthoriz" in errs[0] else "error"
    if pick:
        return pick(d)
    if kind == "read":
        return len(d or [])
    return (d or {}).get("numUids")


UPD_C = 'mutation($id: [ID!]) @dryRun { m: updateCandidate(input: {filter: {id: $id}, set: {specialty: "spike dry run"}}) { numUids } }'
DEL_C = "mutation($id: [ID!]!) @dryRun { m: deleteCandidate(filter: {id: $id}) { numUids } }"
UPD_A = 'mutation($id: [ID!]) @dryRun { m: updateAddress(input: {filter: {id: $id}, set: {city: "Spike"}}) { numUids } }'
UPD_B = 'mutation($id: [ID!]) @dryRun { m: updateBilling(input: {filter: {id: $id}, set: {terms: "spike dry run"}}) { numUids } }'
UPD_T = 'mutation($id: [ID!]) @dryRun { m: updateAttachment(input: {filter: {id: $id}, set: {expiryDate: "2030-01-01T00:00:00Z"}}) { numUids } }'
DEL_T = "mutation($id: [ID!]!) @dryRun { m: deleteAttachment(filter: {id: $id}) { numUids } }"
ADD_T = "mutation($in: [AddAttachmentInput!]!) @dryRun { m: addAttachment(input: $in) { numUids } }"
ADD_C = "mutation($in: [AddCandidateInput!]!) @dryRun { m: addCandidate(input: $in) { numUids } }"


def with_child(field):
    return lambda d: sum(1 for x in d or [] if x.get(field))


def children(field):
    return lambda d: sum(len(x.get(field) or []) for x in d or [])


def table(c, toks, t):
    """One row per question, one cell per caller."""
    q = []

    def row(label, kind, query, variables, pick=None):
        q.append((label, kind, query, variables, pick))

    by_id = lambda typ, sel="id": "query($id: [ID!]) { r: query%s(filter: {id: $id}) { %s } }" % (typ, sel)
    A, B, N, A2, A3 = t["A"], t["B"], t["N"], t["A2"], t["A3"]
    row("candidate A: read by ID", "read", by_id("Candidate"), {"id": [A]})
    row("candidate A: update", "write", UPD_C, {"id": [A]})
    row("candidate A: delete", "write", DEL_C, {"id": [A]})
    row("candidate A: its attachments, read through it", "read", by_id("Candidate", "id hasAttachment { id }"), {"id": [A]},
        children("hasAttachment"))
    row("candidate A: its attachment, read by ID", "read", by_id("Attachment"), {"id": [t["T_A"]]})
    row("candidate A: its attachment, update", "write", UPD_T, {"id": [t["T_A"]]})
    row("candidate A: its attachment, delete", "write", DEL_T, {"id": [t["T_A"]]})
    row("candidate A: add an attachment to it", "write", ADD_T, {"in": [{"category": "OTHER", "forResource": [{"id": A}]}]})
    row("candidate A2: its address, read through it", "read", by_id("Candidate", "id hasAddress { id }"), {"id": [A2]},
        with_child("hasAddress"))
    row("candidate A2: its address, update by ID", "write", UPD_A, {"id": [t["A2_addr"]]})
    row("candidate B (other group): read by ID", "read", by_id("Candidate"), {"id": [B]})
    row("candidate B (other group): update", "write", UPD_C, {"id": [B]})
    row("billing R: read by ID", "read", by_id("Billing"), {"id": [t["R"]]})
    row("billing R: update", "write", UPD_B, {"id": [t["R"]]})
    row("attachment T: read by ID", "read", by_id("Attachment"), {"id": [t["T"]]})
    row("attachment T: read through its candidate", "read", by_id("Candidate", "id hasAttachment { id }"), {"id": [A3]},
        lambda d: sum(1 for x in d or [] for a in x.get("hasAttachment") or [] if a["id"] == t["T"]))
    row("attachment T: update", "write", UPD_T, {"id": [t["T"]]})
    row("attachment T: delete", "write", DEL_T, {"id": [t["T"]]})
    row("contact K: read by ID", "read", by_id("Contact"), {"id": [t["K"]]})
    row("contact K: its office address, read through it", "read", by_id("Contact", "id hasOfficeAddress { id }"),
        {"id": [t["K"]]}, with_child("hasOfficeAddress"))
    row("contact K: its office address, update by ID", "write", UPD_A, {"id": [t["K_addr"]]})
    row("user U: its address, read through it", "read", by_id("User", "id hasAddress { id }"), {"id": [t["U"]]},
        with_child("hasAddress"))
    row("user U: its address, update by ID", "write", UPD_A, {"id": [t["U_addr"]]})
    row("control N (untouched): read by ID", "read", by_id("Candidate"), {"id": [N]})
    row("control N (untouched): update", "write", UPD_C, {"id": [N]})
    row("control N (untouched): delete", "write", DEL_C, {"id": [N]})
    row("control N (untouched): add an attachment to it", "write", ADD_T,
        {"in": [{"category": "OTHER", "forResource": [{"id": N}]}]})
    row("add a candidate with an address, into the group", "write", ADD_C,
        {"in": [{"firstName": "Spike", "lastName": "Dryrun", "inGroup": [{"id": t["G"]}], "hasAddress": {"city": "Spike"}}]})
    row("add a candidate, no group given", "write", ADD_C, {"in": [{"firstName": "Spike", "lastName": "Dryrun"}]})
    row("add a company into the group", "write", "mutation($in: [AddCompanyInput!]!) @dryRun { m: addCompany(input: $in) { numUids } }",
        {"in": [{"name": "Spike Dryrun Co", "inGroup": [{"id": t["G"]}]}]})
    if t["adrift"]:
        row("%d attachments with no group, placed by their owner: read" % len(t["adrift"]), "read", by_id("Attachment"),
            {"id": t["adrift"]})
    out = {}
    for label, kind, query, variables, pick in q:
        out[label] = {who: cell(c, toks[who], kind, query, variables, pick, "%s / %s" % (label, who)) for who in CALLERS}
    return out


def show(title, tables):
    names = list(tables)
    print("\n%s" % title)
    print("  %-52s %s" % ("", "   ".join("%-34s" % n for n in names)))
    print("  %-52s %s" % ("", "   ".join("%-34s" % " ".join("%-8s" % w for w in CALLERS) for _ in names)))
    for label in tables[names[0]]:
        cells = []
        for n in names:
            cells.append("%-34s" % " ".join("%-8s" % str(tables[n][label][w])[:8] for w in CALLERS))
        print("  %-52s %s" % (label[:52], "   ".join(cells)))
    seen = {}
    for note, m in ERRS:
        seen.setdefault(m, []).append(note)
    for m, notes in seen.items():
        print("  message: %s\n      on: %s%s" % (m[:400], "; ".join(notes[:6]), " and %d more" % (len(notes) - 6) if len(notes) > 6 else ""))
    del ERRS[:]


# ---------------------------------------------------------------- the probe

def pick_targets(c, ws):
    groups = [g for g in rows(c, "{ q(func: type(Group)) @filter(uid_in(%s, %s) AND %s) { uid n: Group.name "
                                 "jobs: count(Group.hasJob) cands: count(Group.hasCandidate) } }" % (WSM, ws, LIVE))
              if g.get("jobs") and g.get("cands")]
    G = min(groups, key=lambda g: g["jobs"])
    H = max([g for g in groups if g["uid"] != G["uid"]], key=lambda g: g["cands"])
    shape = ("{ uid g: Groupable.inGroup { uid } att: AttachmentOwner.hasAttachment @filter(has(%s) AND %s) { uid } "
             "addr: Candidate.hasAddress @filter(has(Address.forCandidate)) { uid } "
             "a: Candidate.hasApplication { g: Groupable.inGroup { uid } } }" % (WSM, LIVE))
    live = "@filter(%s AND has(%s))" % (LIVE, WSM)
    in_g = rows(c, "{ q(func: uid(%s)) { c: Group.hasCandidate %s %s } }" % (G["uid"], live, shape))[0]["c"]
    in_h = rows(c, "{ q(func: uid(%s)) { c: Group.hasCandidate (first: 3000) %s %s } }" % (H["uid"], live, shape))[0]["c"]
    only_g = [x for x in in_g if set(uids(x.get("g"))) == {G["uid"]}]
    with_att = [x for x in only_g if x.get("att")]
    with_addr = [x for x in only_g if x.get("addr")]
    plain = [x for x in only_g if not x.get("att") and not x.get("addr")]
    if len(with_att) < 2 or not with_addr or not plain:
        raise SystemExit("group %s has too few candidates with an attachment (%d) or an address (%d)" % (
            G["uid"], len(with_att), len(with_addr)))
    A, A3 = with_att[0], with_att[1]
    A2 = next(x for x in with_addr if x["uid"] not in (A["uid"], A3["uid"]))
    B = next(x for x in in_h if G["uid"] not in uids(x.get("g"))
             and G["uid"] not in {u for a in x.get("a") or [] for u in uids(a.get("g"))})
    R = rows(c, "{ q(func: uid(%s)) { b: Group.hasBilling (first: 1) @filter(%s AND has(%s)) { uid } } }" % (G["uid"], LIVE, WSM))
    R = uids(R[0].get("b")) if R else []
    if not R:
        R = uids(rows(c, "{ q(func: type(Billing), first: 1) @filter(uid_in(%s, %s) AND %s) { uid } }" % (WSM, ws, LIVE)))
    K = rows(c, "{ q(func: uid(%s)) { k: Group.hasContact @filter(%s AND has(Contact.hasOfficeAddress)) @cascade { uid "
                "a: Contact.hasOfficeAddress @filter(has(Address.officeAddressForContact)) { uid } } } }" % (G["uid"], LIVE))
    K = (K[0].get("k") if K else None) or []
    if not K:
        K = rows(c, "{ q(func: has(Contact.hasOfficeAddress)) @filter(%s) @cascade { uid a: Contact.hasOfficeAddress "
                    "@filter(has(Address.officeAddressForContact)) { uid } Groupable.inGroup @filter(uid_in(%s, %s)) { uid } } }"
                 % (LIVE, WSM, ws))
    U = rows(c, '{ q(func: type(User)) @filter(uid_in(%s, %s) AND %s AND NOT eq(User.userId, "%s")) @cascade { uid '
                "a: User.hasAddress @filter(has(Address.forUser)) { uid } } }" % (WSM, ws, LIVE, P.SUB))
    if not (R and K and U):
        raise SystemExit("no billing, contact with an office address, or user with an address to try: %s %s %s" % (R, K, U))
    t = {"G": G["uid"], "H": H["uid"], "A": A["uid"], "T_A": uids(A["att"])[0], "A3": A3["uid"], "T": uids(A3["att"])[0],
         "A2": A2["uid"], "A2_addr": uids(A2["addr"])[0], "B": B["uid"], "N": plain[0]["uid"], "R": R[0],
         "K": K[0]["uid"], "K_addr": uids(K[0]["a"])[0], "U": U[0]["uid"], "U_addr": uids(U[0]["a"])[0]}
    print("group G = %s %s, other group H = %s %s" % (G["uid"], G.get("n"), H["uid"], H.get("n")))
    print("targets:", json.dumps(t))
    return t


def add_editor(c, ws, group):
    """A user with the workspace's editor role on one group, written as the app's own
    users and bindings are."""
    roles = rows(c, '{ q(func: eq(IAMRole.name, "editor")) @filter(uid_in(%s, %s)) { uid p: IAMRole.permission } }' % (WSM, ws))
    like = rows(c, "{ q(func: uid(%s)) { u: Ownable.ownedBy { t: dgraph.type } "
                   "b: Workspace.hasIAMRoleBinding (first: 1) { t: dgraph.type } } }" % ws)
    role = roles[0]
    u, b = "_:u", "_:b"
    rdf = ['%s <dgraph.type> "%s" .' % (u, x) for x in P.edges_types(like[0]["u"])]
    rdf += ['%s <dgraph.type> "%s" .' % (b, x) for x in P.edges_types(like[0]["b"])]
    rdf += ['%s <User.userId> "%s" .' % (u, EDITOR), '%s <User.status> "ACTIVE" .' % u,
            '%s <User.displayName> "Spike editor" .' % u, '%s <User.email> "editor@spike.invalid" .' % u,
            '%s <Identifiable.sId> "jobli::user::%s" .' % (u, EDITOR), '%s <Recordable.deleted> "false"^^<xs:boolean> .' % u,
            "%s <%s> <%s> ." % (u, WSM, ws), "%s <User.hasIAMBinding> %s ." % (u, b),
            "%s <IAMBinding.forUserMember> %s ." % (b, u), "%s <IAMBinding.forRole> <%s> ." % (b, role["uid"]),
            "%s <IAMBinding.forResource> <%s> ." % (b, group), "%s <%s> <%s> ." % (b, WSM, ws),
            "<%s> <IAMRole.hasBinding> %s ." % (role["uid"], b), "<%s> <IAMResource.hasIAMBinding> %s ." % (group, b),
            "<%s> <Workspace.hasIAMRoleBinding> %s ." % (ws, b), "<%s> <Workspace.hasUser> %s ." % (ws, u)]
    c.mutate("\n".join(rdf))
    return sorted(role.get("p") or [])


def edges_of(c, ws, kind, uid):
    """The edges of one added record, as they are in the data."""
    if kind == "attachment":
        d = c.dql("{ q(func: uid(%s)) { uid w: %s { uid } g: Groupable.inGroup { uid } r: Attachment.forResource { uid "
                  "back: AttachmentOwner.hasAttachment @filter(uid(%s)) { uid } } } "
                  "l(func: uid(%s)) { x: Workspace.hasAttachment @filter(uid(%s)) { uid } } }" % (uid, WSM, uid, ws, uid))["data"]
        a = (d.get("q") or [{}])[0]
        owners = a.get("r") or []
        return {"inWorkspace": bool(a.get("w")), "listed by the workspace": bool((d.get("l") or [{}])[0].get("x")),
                "inGroup": len(a.get("g") or []), "forResource": len(owners),
                "owner lists it": sum(1 for o in owners if o.get("back"))}
    d = c.dql("{ q(func: uid(%s)) { uid w: %s { uid } g: Groupable.inGroup { uid back: Group.hasCandidate @filter(uid(%s)) { uid } } "
              "a: Candidate.hasAddress { uid back: Address.forCandidate { uid } } } "
              "l(func: uid(%s)) { x: Workspace.hasCandidate @filter(uid(%s)) { uid } } }" % (uid, WSM, uid, ws, uid))["data"]
    x = (d.get("q") or [{}])[0]
    addr = one(x.get("a")) or {}
    return {"inWorkspace": bool(x.get("w")), "listed by the workspace": bool((d.get("l") or [{}])[0].get("x")),
            "inGroup": len(x.get("g") or []), "group lists it": sum(1 for g in x.get("g") or [] if g.get("back")),
            "address": bool(addr), "address points back": bool(addr.get("back"))}


def real_adds(c, ws, toks, t):
    """Adds that are committed, as the owner and the editor, to see the edges an add writes."""
    out = {}

    def add(who, label, kind, query, variables, path):
        d, errs = ask(c, toks[who], query, variables)
        nodes = [d] if d else []
        for step in path:
            nodes = [x for n in nodes for x in (n.get(step) if isinstance(n.get(step), list) else [n.get(step)]) if x]
        ids = [x["id"] for x in nodes if x.get("id")]
        if kind:
            out["%s: %s" % (who, label)] = [edges_of(c, ws, kind, i) for i in ids] or "nothing added: %s" % (errs or d)
        elif not ids:
            out["%s: %s" % (who, label)] = "nothing added: %s" % (errs or d)
        return ids

    G = [{"id": t["G"]}]
    for who in ("owner", "editor"):
        cand = add(who, "addCandidate into the group, with an address", "candidate",
                   "mutation($in: [AddCandidateInput!]!) { m: addCandidate(input: $in) { candidate { id } } }",
                   {"in": [{"firstName": "Spike", "lastName": "Phase1", "inGroup": G, "hasAddress": {"city": "Spike"}}]},
                   ["candidate"])
        if not cand:
            continue
        add(who, "addAttachment for that candidate", "attachment",
            "mutation($in: [AddAttachmentInput!]!) { m: addAttachment(input: $in) { attachment { id } } }",
            {"in": [{"category": "RESUME", "forResource": [{"id": cand[0]}]}]}, ["attachment"])
        add(who, "updateCandidate, set hasAttachment to a new one", "attachment",
            "mutation($id: [ID!]) { m: updateCandidate(input: {filter: {id: $id}, set: {hasAttachment: [{category: OTHER}]}}) "
            "{ candidate { hasAttachment(filter: {category: {eq: OTHER}}) { id } } } }", {"id": cand}, ["candidate", "hasAttachment"])
        add(who, "addCandidate with a new attachment inside", "attachment",
            "mutation($in: [AddCandidateInput!]!) { m: addCandidate(input: $in) { candidate { hasAttachment { id } } } }",
            {"in": [{"firstName": "Spike", "lastName": "Phase1", "inGroup": G, "hasAttachment": [{"category": "RESUME"}]}]},
            ["candidate", "hasAttachment"])
        co = add(who, "addCompany into the group", None,
                 "mutation($in: [AddCompanyInput!]!) { m: addCompany(input: $in) { company { id } } }",
                 {"in": [{"name": "Spike Phase1 Co", "inGroup": G}]}, ["company"])
        if co:
            add(who, "updateCompany, set hasAttachment to a new one", "attachment",
                "mutation($id: [ID!]) { m: updateCompany(input: {filter: {id: $id}, set: {hasAttachment: [{category: BUSINESS_TERM}]}}) "
                "{ company { hasAttachment { id } } } }", {"id": co}, ["company", "hasAttachment"])
    return out


BILL = ('mutation($id: [ID!]) @dryRun { m: updateBilling(input: {filter: {id: $id}, set: {hasAddress: {city: "Spike"}}}) '
        "{ numUids billing { id hasAddress { city } } } }")
FORM = ("mutation($id: [ID!]) @dryRun { m: updatePortalForm(input: {filter: {id: $id}, set: {isDisabled: false}}) "
        "{ numUids portalForm { id hasField { addressValue { id } } } } }")


def bypass_fields(c, ws, toks, t, original):
    """The two fields phase 1 gives @bypassAuth: Billing.hasAddress and FormField.addressValue."""
    a = "    addressValue: Address @cascadeDelete\n"
    b = "    hasAddress: Address @cascadeDelete\n}"
    if original.count(a) != 1 or original.count(b) != 1:
        print("\nthe schema no longer has the two fields as phase 1 found them; not tried")
        return {}
    patched = original.replace(a, '    addressValue: Address @cascadeDelete @bypassAuth(except: ["Recordable"])\n')
    patched = patched.replace(b, '    hasAddress: Address @cascadeDelete @bypassAuth(except: ["Recordable"])\n}')
    made = c.mutate('<%s> <Billing.hasAddress> _:a .\n_:a <dgraph.type> "Address" .\n_:a <dgraph.type> "Recordable" .\n'
                    '_:a <dgraph.type> "Identifiable" .\n_:a <Address.city> "Spike" .\n'
                    '_:a <Recordable.deleted> "false"^^<xs:boolean> .' % t["R"])
    addr = made["data"]["uids"]["a"]
    bare = uids(rows(c, "{ q(func: type(Billing), first: 1) @filter(uid_in(%s, %s) AND %s AND NOT has(Billing.hasAddress)) { uid } }"
                     % (WSM, ws, LIVE)))
    forms = rows(c, "{ q(func: has(FormField.addressValue), first: 500) { f: FormField.forForm @filter(type(PortalForm) AND %s) "
                    "{ uid w: %s { n: Workspace.name o: Ownable.ownedBy { User.userId User.email } } } } }" % (LIVE, WSM))
    form = None
    for f in [one(r["f"]) for r in forms if r.get("f")]:
        w = one(f.get("w")) or {}
        o = one(w.get("o")) or {}
        if o.get("User.userId"):
            form = {"id": f["uid"], "ws": w["n"], "member": P.token(ws=w["n"], sub=o["User.userId"], email=o.get("User.email", "")),
                    "stranger": P.token(ws=w["n"], sub=EDITOR, email="editor@spike.invalid")}
            break
    read = "query($id: [ID!]) { r: queryBilling(filter: {id: $id}) { id hasAddress { id } } }"
    read_form = "query($id: [ID!]) { r: queryPortalForm(filter: {id: $id}) { id hasField { addressValue { id } } } }"
    fields = lambda d: sum(1 for x in d or [] for f in x.get("hasField") or [] if f.get("addressValue"))
    out = {}
    for name, schema in (("schema as it is", original), ("with @bypassAuth", patched)):
        r = P.set_schema(c, schema)
        if r.get("errors"):
            print("\n%s: the schema was refused: %s" % (name, r["errors"]))
            continue
        res = {}

        def row(label, kind, query, variables, pick=None, toks=toks, callers=CALLERS):
            res[label] = {w: cell(c, toks[w], kind, query, variables, pick, "%s / %s / %s" % (name, label, w)) if w in callers
                          else "-" for w in CALLERS}

        row("billing R: its address, read through it", "read", read, {"id": [t["R"]]}, with_child("hasAddress"))
        row("billing R: its address, update by ID", "write", UPD_A, {"id": [addr]})
        row("billing R: set a new address, and get it back", "write", BILL, {"id": [t["R"]]},
            lambda d: "%s/%s" % ((d or {}).get("numUids"), sum(1 for x in (d or {}).get("billing") or [] if x.get("hasAddress"))))
        if bare:
            row("a billing with no address: set one, and get it back", "write", BILL, {"id": bare},
                lambda d: "%s/%s" % ((d or {}).get("numUids"), sum(1 for x in (d or {}).get("billing") or [] if x.get("hasAddress"))))
        if form:
            # Here the owner column is the owner of the form's workspace, and the intruder is no member of it.
            ft = {"owner": form["member"], "intruder": form["stranger"]}
            row("a form of %s: address fields, read" % form["ws"], "read", read_form, {"id": [form["id"]]}, fields, ft, ft)
            row("a form of %s: update it, address fields back" % form["ws"], "write", FORM, {"id": [form["id"]]},
                lambda d: "%s/%s" % ((d or {}).get("numUids"), fields((d or {}).get("portalForm"))), ft, ft)
        out[name] = res
    return out


# ---------------------------------------------------------------- attachments

KINDS = ("Candidate", "Application", "Company", "Contact", "Placement", "EmailOutbound", "Message", "Job", "JobAd", "Note", "User")


def census(c, ws, toks):
    """Every attachment of the workspace, by what it hangs off, and how many each caller can read by ID."""
    att = rows(c, "{ q(func: type(Attachment)) @filter(uid_in(%s, %s)) { uid d: Recordable.deleted g: count(Groupable.inGroup) "
                  "r: Attachment.forResource { t: dgraph.type d: Recordable.deleted } } }" % (WSM, ws))
    classes = {}
    for a in att:
        owners = a.get("r") or []
        kinds = sorted({k for o in owners for k in KINDS if k in (o.get("t") or [])})
        what = "+".join(kinds) if kinds else ("nothing" if not owners else "other")
        if owners and all(o.get("d") for o in owners):
            what += " (deleted)"
        key = ("deleted attachment" if a.get("d") else "on " + what, "in a group" if a.get("g") else "no group")
        classes.setdefault(key, []).append(a["uid"])
    q = "query($id: [ID!]) { r: queryAttachment(filter: {id: $id}, first: 5000) { id } }"
    out = {}
    print("\nevery attachment of %s (%d), and how many each caller reads by ID" % (P.WS, len(att)))
    print("  %-44s %-11s %6s   %s" % ("", "", "total", " ".join("%-8s" % w for w in CALLERS)))
    for key in sorted(classes, key=lambda k: -len(classes[k])):
        ids = classes[key]
        got = {w: cell(c, toks[w], "read", q, {"id": ids}, None, "%s, %s / %s" % (key + (w,))) for w in CALLERS}
        out["%s, %s" % key] = dict(got, total=len(ids))
        print("  %-44s %-11s %6d   %s" % (key[0][:44], key[1], len(ids), " ".join("%-8s" % got[w] for w in CALLERS)))
    agg = {w: cell(c, toks[w], "read", "query { r: aggregateAttachment { count } }", {}, lambda d: (d or {}).get("count"))
           for w in CALLERS}
    print("  %-44s %-11s %6s   %s" % ("aggregateAttachment, no filter", "", "", " ".join("%-8s" % agg[w] for w in CALLERS)))
    out["aggregateAttachment"] = agg
    for note, m in ERRS:
        print("  message: %s (%s)" % (m[:300], note))
    del ERRS[:]
    return out


def lifecycle(c, ws, toks, t):
    """Committed writes on records the probe adds: move an attachment between two
    candidates, take its last owner away, delete it, delete the candidates."""
    steps = []
    G = [{"id": t["G"]}]
    names = {}

    def new(label, query, variables, key):
        d, errs = ask(c, toks["owner"], query, variables)
        ids = [x["id"] for x in (d or {}).get(key) or []]
        if not ids:
            steps.append((label, "nothing added: %s" % (errs or d)))
            raise LookupError(label)
        names[ids[0]] = label
        return ids[0]

    def listed(kind, uid):
        d = c.dql("{ w(func: uid(%s)) @filter(uid_in(Workspace.has%s, %s)) { uid } "
                  "g(func: has(Group.has%s)) @filter(uid_in(Group.has%s, %s)) { uid } }" % (ws, kind, uid, kind, kind, uid))["data"]
        return "workspace list %s, group list %s" % ("yes" if d.get("w") else "no", "yes" if d.get("g") else "no")

    def look(att):
        d = c.dql("{ q(func: uid(%s)) { uid t: dgraph.type d: Recordable.deleted r: Attachment.forResource { uid } } "
                  "o(func: has(AttachmentOwner.hasAttachment)) @filter(uid_in(AttachmentOwner.hasAttachment, %s)) { uid } }"
                  % (att, att))["data"]
        a = (d.get("q") or [{}])[0]
        state = "gone" if not a.get("t") else "deleted" if a.get("d") else "live"
        return "%s %s, forResource [%s], listed by [%s], %s" % (
            names[att], state, ", ".join(sorted(names.get(u, u) for u in refs(a.get("r") or []))),
            ", ".join(sorted(names.get(u["uid"], u["uid"]) for u in d.get("o") or [])), listed("Attachment", att))

    def cand(uid):
        d = c.dql("{ q(func: uid(%s)) { uid t: dgraph.type d: Recordable.deleted } }" % uid)["data"].get("q") or [{}]
        return "%s %s, %s" % (names[uid], "gone" if not d[0].get("t") else "deleted" if d[0].get("d") else "live",
                              listed("Candidate", uid))

    def step(label, query, variables, see, who="owner"):
        since = len(EVENTS)
        d, errs = ask(c, toks[who], query, variables)
        sent = hooks(since)
        steps.append((label, "numUids %s%s; notices to lambda: %s\n      %s" % (
            (d or {}).get("numUids"), " %s" % [e[:160] for e in errs] if errs else "", sent, "\n      ".join(f() for f in see))))

    add_c = "mutation($in: [AddCandidateInput!]!) { m: addCandidate(input: $in) { candidate { id } } }"
    add_t = "mutation($in: [AddAttachmentInput!]!) { m: addAttachment(input: $in) { attachment { id } } }"
    upd_t = "mutation($in: UpdateAttachmentInput!) { m: updateAttachment(input: $in) { numUids } }"
    del_t = "mutation($id: [ID!]!) { m: deleteAttachment(filter: {id: $id}) { numUids } }"
    try:
        X = new("X", add_c, {"in": [{"firstName": "Spike", "lastName": "Phase1 X", "inGroup": G}]}, "candidate")
        Y = new("Y", add_c, {"in": [{"firstName": "Spike", "lastName": "Phase1 Y", "inGroup": G}]}, "candidate")
        T1 = new("T1", add_t, {"in": [{"category": "RESUME", "forResource": [{"id": X}, {"id": Y}]}]}, "attachment")
        T2 = new("T2", add_t, {"in": [{"category": "RESUME", "forResource": [{"id": X}]}]}, "attachment")
        T3 = new("T3", add_t, {"in": [{"category": "OTHER", "forResource": [{"id": Y}]}]}, "attachment")
        T4 = new("T4", add_t, {"in": [{"category": "OTHER", "forResource": [{"id": Y}]}]}, "attachment")
        steps.append(("owner adds candidates X and Y, T1 on both, T2 on X, T3 and T4 on Y",
                      "\n      ".join(look(x) for x in (T1, T2, T3, T4))))
        by_id = "query($id: [ID!]) { r: queryAttachment(filter: {id: $id}) { id } }"
        reads = lambda ids: lambda: "read by ID: " + ", ".join(
            "%s %s" % (w, cell(c, toks[w], "read", by_id, {"id": ids})) for w in ("owner", "editor", "intruder"))
        step("editor updates T2", upd_t, {"in": {"filter": {"id": [T2]}, "set": {"expiryDate": "2030-01-01T00:00:00Z"}}},
             [lambda: look(T2)], "editor")
        step("editor deletes T2", del_t, {"id": [T2]}, [lambda: look(T2)], "editor")
        step("intruder updates T2", upd_t, {"in": {"filter": {"id": [T2]}, "set": {"expiryDate": "2031-01-01T00:00:00Z"}}},
             [lambda: look(T2)], "intruder")
        step("intruder deletes T2", del_t, {"id": [T2]}, [lambda: look(T2)], "intruder")
        step("owner: updateAttachment T2, set forResource to Y as well", upd_t,
             {"in": {"filter": {"id": [T2]}, "set": {"forResource": [{"id": Y}]}}}, [lambda: look(T2)])
        step("owner: updateAttachment T2, remove X from forResource", upd_t,
             {"in": {"filter": {"id": [T2]}, "remove": {"forResource": [{"id": X}]}}}, [lambda: look(T2)])
        step("owner: updateAttachment T4, remove Y, its only owner", upd_t,
             {"in": {"filter": {"id": [T4]}, "remove": {"forResource": [{"id": Y}]}}}, [lambda: look(T4), reads([T4])])
        step("owner: deleteAttachment T3", del_t, {"id": [T3]}, [lambda: look(T3), reads([T3])])
        step("owner: deleteCandidate X", DEL_C.replace(" @dryRun", ""), {"id": [X]},
             [lambda: cand(X), lambda: look(T1), reads([T1])])
        step("owner: deleteCandidate Y", DEL_C.replace(" @dryRun", ""), {"id": [Y]},
             [lambda: cand(Y), lambda: look(T1), lambda: look(T2), reads([T1, T2])])
    except LookupError:
        pass
    with open(os.path.join(P.OUT, "phase1_lambda_notices.txt"), "w") as f:
        f.write("\n".join(SAMPLES))
    print("\nan attachment's life, committed, on records the probe added")
    for label, seen in steps:
        print("  %s\n      %s" % (label, seen))
    del ERRS[:]
    return steps


def cross_workspace(c, ws, toks, t):
    """A member of another workspace, with a token for that workspace, adds a candidate
    and names a group of this one. Committed, to see what is written."""
    out = {}
    d, errs = ask(c, toks["outsider"], "mutation($in: [AddCandidateInput!]!) { m: addCandidate(input: $in) { numUids candidate { id } } }",
                  {"in": [{"firstName": "Spike", "lastName": "Phase1 Outsider", "inGroup": [{"id": t["G"]}]}]})
    ids = [x["id"] for x in (d or {}).get("candidate") or []]
    out["add"] = "numUids %s, errors %s" % ((d or {}).get("numUids"), [e[:200] for e in errs])
    if ids:
        x = ids[0]
        e = c.dql("{ q(func: uid(%s)) { uid w: %s { n: Workspace.name } g: Groupable.inGroup { uid n: Group.name w: %s { n: Workspace.name } } "
                  "p: Groupable.hasPrimaryGroup { uid n: Group.name } } "
                  "g(func: uid(%s)) @filter(uid_in(Group.hasCandidate, %s)) { uid } "
                  "l(func: type(Workspace)) @filter(uid_in(Workspace.hasCandidate, %s)) { n: Workspace.name } }"
                  % (x, WSM, WSM, t["G"], x, x))["data"]
        q = (e.get("q") or [{}])[0]
        out["the candidate is in workspace"] = [w.get("n") for w in ([q["w"]] if isinstance(q.get("w"), dict) else q.get("w") or [])]
        out["its groups"] = [(g.get("n"), [w.get("n") for w in ([g["w"]] if isinstance(g.get("w"), dict) else g.get("w") or [])])
                             for g in q.get("g") or []]
        out["the group of %s now lists it" % P.WS] = bool(e.get("g"))
        out["workspaces that list it"] = [w.get("n") for w in e.get("l") or []]
        by_id = "query($id: [ID!]) { r: queryCandidate(filter: {id: $id}) { id } }"
        out["who reads it by ID"] = {w: cell(c, toks[w], "read", by_id, {"id": [x]}) for w in CALLERS}
        through = ("query($id: [ID!]) { r: queryCandidate(filter: {id: $id}) { id inGroup { id name "
                   "hasCandidate(first: 5) { id firstName } hasJob(first: 5) { id } } } }")
        d2, errs2 = ask(c, toks["outsider"], through, {"id": [x]})
        groups = [g for r in d2 or [] for g in r.get("inGroup") or []]
        out["what the outsider reads through it"] = {
            "groups": [g.get("name") for g in groups], "candidates of the group": sum(len(g.get("hasCandidate") or []) for g in groups),
            "jobs of the group": sum(len(g.get("hasJob") or []) for g in groups), "errors": [e[:160] for e in errs2]}
        seen = "query($g: [ID!], $id: [ID!]) { r: queryGroup(filter: {id: $g}) { id hasCandidate(filter: {id: $id}) { id } } }"
        out["%s reads it through the group" % P.WS] = {
            w: cell(c, toks[w], "read", seen, {"g": [t["G"]], "id": [x]}, children("hasCandidate")) for w in ("owner", "editor")}
    print("\na member of %s adds a candidate and names a group of %s" % (OTHER_WS, P.WS))
    for k, v in out.items():
        print("  %-44s %s" % (k, json.dumps(v)))
    del ERRS[:]
    return out


def main():
    if not (P.SUB and P.EMAIL):
        raise SystemExit("set SPIKE_SUB and SPIKE_EMAIL to the userId and email of the owner of %s" % P.WS)
    server = http.server.ThreadingHTTPServer(("127.0.0.1", P.LAMBDA_PORT), Stub)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    original = open(os.path.join(P.OUT, "schema_today.graphql")).read()
    with P.cluster(P.OFF) as c:
        c.wait_ready(0, timeout=900)
        after = os.environ.get("SPIKE_CLEAN_AFTER")
        if after:
            # Clears up after a run that died: every edge to a node made after this uid.
            edges = edges_after(c, after)
            by = collections.Counter(e[1] for e in edges)
            print("edges to nodes after %s: %d %s" % (after, len(edges), dict(by)))
            if edges:
                c.mutate("\n".join("<%s> <%s> <%s> ." % e for e in edges), delete=True)
            print("left: %d edges, %d nodes" % (len(edges_after(c, after)), len(new_nodes(c, after))))
            return
        if P.get_schema(c) != original:
            print("schema differs from schema_today; setting it")
            P.set_schema(c, original)
        ws = rows(c, '{ q(func: eq(Workspace.name, "%s")) { uid } }' % P.WS)[0]["uid"]
        count = ("{ c(func: type(Candidate)) { count(uid) } a(func: type(Attachment)) { count(uid) } "
                 "d(func: type(Address)) { count(uid) } b(func: type(Billing)) { count(uid) } u(func: type(User)) { count(uid) } "
                 "l(func: uid(%s)) { c: count(Workspace.hasCandidate) a: count(Workspace.hasAttachment) u: count(Workspace.hasUser) "
                 "b: count(Workspace.hasIAMRoleBinding) Recordable.updatedAt } }" % ws)
        while True:
            stale = sorted(uids(rows(c, "{ q(func: type(SpikeMarker)) { uid } }")), key=lambda u: int(u, 16))
            if not stale:
                break
            n, triples, left = remove_new(c, stale[0])
            print("an earlier run left its records behind: removed %d nodes and %d edges to them; still there: %s" % (n, triples, left))
        before_counts = c.dql(count)["data"]
        marker = c.mutate('_:m <dgraph.type> "SpikeMarker" .')["data"]["uids"]["m"]
        taken, out = [], {}
        try:
            t = pick_targets(c, ws)
            t["adrift"] = [a for a in ADRIFT if has_edge(c, a, WSM, ws)]
            snapshot = rows(c, "{ q(func: uid(%s)) { uid Candidate.specialty Recordable.deleted Recordable.updatedAt "
                               "Address.city Billing.terms Attachment.expiryDate } }" % ", ".join(
                                   t[k] for k in t if k not in ("G", "H", "adrift")))
            other = rows(c, '{ q(func: eq(Workspace.name, "%s")) { u: Workspace.hasUser @filter(eq(User.status, "ACTIVE")) '
                            "{ uid User.userId User.email } } }" % OTHER_WS)[0]["u"]
            here = {u.get("User.userId") for u in rows(c, "{ q(func: uid(%s)) { u: Workspace.hasUser { User.userId } } }" % ws)[0]["u"]}
            outsider = next(u for u in other if u.get("User.userId") and u["User.userId"] not in here)
            print("editor role on G holds %s; outsider is a member of %s and not of %s" % (add_editor(c, ws, t["G"]), OTHER_WS, P.WS))
            toks = {"owner": P.token(), "editor": P.token(sub=EDITOR, email="editor@spike.invalid"),
                    "outsider": P.token(ws=OTHER_WS, sub=outsider["User.userId"], email=outsider.get("User.email", "")),
                    "intruder": P.token(sub=outsider["User.userId"], email=outsider.get("User.email", ""))}

            # Take away the edges the script writes, as they were before the repair.
            for x in (t["A"], t["A2"], t["B"]):
                taken += [(x, WSM, ws), (ws, "Workspace.hasCandidate", x)]
            taken += [(t["R"], WSM, ws), (ws, "Workspace.hasBilling", t["R"]),
                      (t["T"], WSM, ws), (ws, "Workspace.hasAttachment", t["T"]),
                      (t["K_addr"], "Address.officeAddressForContact", t["K"]), (t["U_addr"], "Address.forUser", t["U"])]
            for a in t["adrift"]:
                taken += [(a, WSM, ws), (ws, "Workspace.hasAttachment", a)]
            missing = [e for e in taken if not has_edge(c, *e)]
            if missing:
                raise SystemExit("edges expected in the repaired data are not there: %s" % missing)
            c.mutate("\n".join("<%s> <%s> <%s> ." % e for e in taken), delete=True)
            tables = {"edges missing": table(c, toks, t)}

            fix = subprocess.run([sys.executable, FIX, "--alpha", c.alpha_http(0), "--workspace", P.WS],
                                 capture_output=True, text=True)
            written = [l for l in fix.stdout.splitlines() if "triples" in l]
            back = [e for e in taken if has_edge(c, *e)]
            print("\nthe script: %s (exit %d); it wrote back %d of the %d edges taken away" % (
                "; ".join(written), fix.returncode, len(back), len(taken)))
            if fix.returncode not in (0,):
                print(fix.stdout[-1500:], fix.stderr[-800:])
            tables["after the script"] = table(c, toks, t)
            show("reads return records, writes return numUids (dry run)", tables)
            out["tables"] = tables

            adds = real_adds(c, ws, toks, t)
            print("\nadds that were committed, and the edges each new record has")
            for k, v in adds.items():
                print("  %-62s %s" % (k, json.dumps(v)))
            out["adds"] = adds

            out["census"] = census(c, ws, toks)
            out["lifecycle"] = lifecycle(c, ws, toks, t)
            out["cross"] = cross_workspace(c, ws, toks, t)

            fields = bypass_fields(c, ws, toks, t, original)
            if fields:
                show("the two @bypassAuth fields (reads: records with the address; writes: numUids, or numUids/addresses returned)",
                     fields)
            out["bypass"] = fields
            out["targets"] = t
        finally:
            if P.get_schema(c) != original:
                P.set_schema(c, original)
            lost = [e for e in taken if not has_edge(c, *e)]
            if lost:
                c.mutate("\n".join("<%s> <%s> <%s> ." % e for e in lost))
                print("\nput back by the probe, not by the script: %s" % lost)
            n, triples, left = remove_new(c, marker)
            after_counts = c.dql(count)["data"]
            print("\nremoved %d nodes the probe added and %d edges to nodes it added; still there: %s" % (n, triples, left))
            print("counts unchanged: %s" % (json.dumps(before_counts, sort_keys=True) == json.dumps(after_counts, sort_keys=True)))
            if before_counts != after_counts:
                print("  before %s\n  after  %s" % (json.dumps(before_counts, sort_keys=True), json.dumps(after_counts, sort_keys=True)))
            try:
                again = rows(c, "{ q(func: uid(%s)) { uid Candidate.specialty Recordable.deleted Recordable.updatedAt "
                                "Address.city Billing.terms Attachment.expiryDate } }" % ", ".join(
                                    t[k] for k in t if k not in ("G", "H", "adrift")))
                print("targets unchanged by the dry runs: %s" % (json.dumps(snapshot, sort_keys=True) == json.dumps(again, sort_keys=True)))
            except NameError:
                pass
        P.save("phase1_writes", out)


if __name__ == "__main__":
    main()
