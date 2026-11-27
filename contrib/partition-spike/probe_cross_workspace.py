"""Can a caller tie a record of one workspace to a record of another (spike probe, not
a proof: it prints and asserts nothing).

Root namespace of the cluster proof_real.py built, partition namespaces off. An
outsider is a member of another workspace, holding a token for that workspace. Each
write is sent with @dryRun under each schema:

  today      schema_today.graphql, the rules as written
  fix        the same, with the Groupable and AttachmentOwner declarations of
             graph/.graphql, when they differ. The per-type rules these once carried
             have been removed from that file; to compare against them again, point
             SPIKE_GRAPHQL at a copy that has them (git show 918a82a:.graphql, in graph)
  scope      today's rules, with the @referenceScope lines of graph/.graphql and nothing
             else: the engine tests every record a mutation names
  both       fix and scope together

The records the probe needs of its own are added for real under today's schema and
removed again, with every edge to them, and the schema is put back.

  SPIKE_SUB=... SPIKE_EMAIL=... python3 probe_cross_workspace.py
"""
import http.server
import json
import os
import re
import threading

import proof_real as P
import probe_phase1_writes as W

WSM = W.WSM
OTHER = W.OTHER_WS


def block(schema, start):
    """The text from a declaration to the opening brace of its body."""
    i = schema.index(start)
    depth, j = 0, i
    # the body opens at the first "{" that is outside any parenthesis
    while True:
        ch = schema[j]
        if ch == '"' and schema[j:j + 3] == '"""':
            j = schema.index('"""', j + 3) + 3
            continue
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
        elif ch == "{" and depth == 0:
            return i, j
        j += 1


def arms(original):
    out = [("today", original)]
    now = open(os.environ.get("SPIKE_GRAPHQL") or os.path.join(P.JOBLI, "graph", ".graphql")).read()
    fixed = original
    for start in ("\ninterface Groupable\n", "\ninterface AttachmentOwner\n"):
        a, _ = block(fixed, start)
        b = fixed.index("\n}\n", a)
        c, _ = block(now, start)
        d = now.index("\n}\n", c)
        fixed = fixed[:a] + now[c:d] + fixed[b:]
    # The check of forResource after an update reads @oldValue on Attachment.forResource.
    # Match the live field, not the same words inside a comment higher up.
    marker = '@oldValue(fields: ["inWorkspace.name", "inGroup.inWorkspace.name"])'
    field = ("    forResource: [AttachmentOwner!]! @search @bypassAuth\n"
             "        @cascadeAuth(operations: [add, update, delete, query], variableContext: self, bidirectional: false)\n")
    if marker in now and field in fixed and marker not in fixed:
        fixed = fixed.replace(field, field.replace("@cascadeAuth", marker + "\n        @cascadeAuth", 1), 1)
    # The blocks copied from graph/.graphql may carry @referenceScope. This arm is the
    # per-type rules alone.
    fixed = "\n".join(l for l in fixed.split("\n") if not l.strip().startswith("@referenceScope("))
    if fixed != original:
        out.append(("fix", fixed))
    # @referenceScope: copy each line of graph/.graphql that carries it to under the
    # same declaration here.
    def scoped(schema):
        lines = now.split("\n")
        for i, line in enumerate(lines):
            if line.strip().startswith("@referenceScope("):
                j = i - 1
                while not re.match(r"(type|interface) ", lines[j]):
                    j -= 1
                head = lines[j] + "\n"
                if schema.count("\n" + head) != 1:
                    raise SystemExit("cannot place %s under %r" % (line.strip(), lines[j][:50]))
                if line.strip() not in schema[schema.index("\n" + head):schema.index("\n" + head) + len(head) + 400]:
                    schema = schema.replace("\n" + head, "\n" + head + line + "\n", 1)
        return schema
    if "@referenceScope(" in now:
        out.append(("scope", scoped(original)))
        if fixed != original:
            out.append(("both", scoped(fixed)))
    return out


def run(c, tok, query, variables):
    d, errs = W.ask(c, tok, query, variables)
    if errs and not d:
        m = errs[0]
        short = "refused" if "uthoriz" in m else "error"
        return short, m
    # A dry run that changes nothing can still say why.
    return str((d or {}).get("numUids")), (errs[0] if errs else "")


def main():
    if not (P.SUB and P.EMAIL):
        raise SystemExit("set SPIKE_SUB and SPIKE_EMAIL to the userId and email of the owner of %s" % P.WS)
    server = http.server.ThreadingHTTPServer(("127.0.0.1", P.LAMBDA_PORT), W.Stub)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    original = open(os.path.join(P.OUT, "schema_today.graphql")).read()
    with P.cluster(P.OFF) as c:
        c.wait_ready(0, timeout=900)
        if P.get_schema(c) != original:
            P.set_schema(c, original)
        rows = lambda q: W.rows(c, q)
        ws = rows('{ q(func: eq(Workspace.name, "%s")) { uid } }' % P.WS)[0]["uid"]
        ows = rows('{ q(func: eq(Workspace.name, "%s")) { uid } }' % OTHER)[0]["uid"]
        while True:
            stale = sorted(W.uids(rows("{ q(func: type(SpikeMarker)) { uid } }")), key=lambda u: int(u, 16))
            if not stale:
                break
            print("an earlier run left its records behind: removed %s" % (W.remove_new(c, stale[0]),))
        count = ("{ c(func: type(Candidate)) { count(uid) } a(func: type(Attachment)) { count(uid) } "
                 "o(func: type(Company)) { count(uid) } l(func: uid(%s, %s)) { c: count(Workspace.hasCandidate) "
                 "a: count(Workspace.hasAttachment) } }" % (ws, ows))
        before = c.dql(count)["data"]
        marker = c.mutate('_:m <dgraph.type> "SpikeMarker" .')["data"]["uids"]["m"]
        try:
            t = W.pick_targets(c, ws)
            G, H, A, N = t["G"], t["H"], t["A"], t["N"]
            other = rows('{ q(func: uid(%s)) { u: Workspace.hasUser @filter(eq(User.status, "ACTIVE")) { User.userId User.email } } }' % ows)[0]["u"]
            here = {u.get("User.userId") for u in rows("{ q(func: uid(%s)) { u: Workspace.hasUser { User.userId } } }" % ws)[0]["u"]}
            out_user = next(u for u in other if u.get("User.userId") and u["User.userId"] not in here)
            W.add_editor(c, ws, G)
            toks = {"owner": P.token(), "editor": P.token(sub=W.EDITOR, email="editor@spike.invalid"),
                    "outsider": P.token(ws=OTHER, sub=out_user["User.userId"], email=out_user.get("User.email", ""))}
            og = rows("{ q(func: type(Group)) @filter(uid_in(%s, %s) AND %s) { uid n: Group.name } }" % (WSM, ows, W.LIVE))
            if not og:
                raise SystemExit("%s has no live group" % OTHER)
            OG = og[0]["uid"]
            print("outsider's own group: %s %s of %s" % (OG, og[0].get("n"), OTHER))

            # The outsider's own records, added for real under today's schema.
            d, e = W.ask(c, toks["outsider"], "mutation($in: [AddCandidateInput!]!) { m: addCandidate(input: $in) { candidate { id } } }",
                         {"in": [{"firstName": "Spike", "lastName": "Cross Own", "inGroup": [{"id": OG}]}]})
            OC = d["candidate"][0]["id"] if d else None
            d2, e2 = W.ask(c, toks["outsider"], "mutation($in: [AddAttachmentInput!]!) { m: addAttachment(input: $in) { attachment { id } } }",
                           {"in": [{"category": "OTHER", "inGroup": [{"id": OG}], "forResource": [{"id": OC}]}]}) if OC else (None, e)
            OT = d2["attachment"][0]["id"] if d2 else None
            print("outsider's own candidate %s and attachment %s %s" % (OC, OT, (e or e2 or "")))

            g = lambda *ids: [{"id": i} for i in ids]
            cand = "mutation($in: [AddCandidateInput!]!) @dryRun { m: addCandidate(input: $in) { numUids } }"
            comp = "mutation($in: [AddCompanyInput!]!) @dryRun { m: addCompany(input: $in) { numUids } }"
            att = "mutation($in: [AddAttachmentInput!]!) @dryRun { m: addAttachment(input: $in) { numUids } }"
            updc = "mutation($in: UpdateCandidateInput!) @dryRun { m: updateCandidate(input: $in) { numUids } }"
            updo = "mutation($in: UpdateCompanyInput!) @dryRun { m: updateCompany(input: $in) { numUids } }"
            updt = "mutation($in: UpdateAttachmentInput!) @dryRun { m: updateAttachment(input: $in) { numUids } }"
            note = "mutation($in: [AddNoteInput!]!) @dryRun { m: addNote(input: $in) { numUids } }"
            tag = "mutation($in: [AddTagInput!]!) @dryRun { m: addTag(input: $in) { numUids } }"
            app = "mutation($in: [AddApplicationInput!]!) @dryRun { m: addApplication(input: $in) { numUids } }"
            msg = rows("{ q(func: type(Message), first: 200) @cascade { uid c: Message.conversation @filter(uid_in(%s, %s)) { uid } } }" % (WSM, ws))
            MSG = msg[0]["uid"] if msg else None
            print("a message of %s: %s" % (P.WS, MSG))
            C = lambda **k: [dict({"firstName": "Spike", "lastName": "Cross"}, **k)]
            O = lambda **k: [dict({"name": "Spike Cross Co"}, **k)]
            T = lambda **k: [dict({"category": "OTHER"}, **k)]
            cases = [
                ("should be refused", [
                    ("outsider", "add candidate into a group of %s" % P.WS, cand, {"in": C(inGroup=g(G))}),
                    ("outsider", "add candidate into own group and a group of %s" % P.WS, cand, {"in": C(inGroup=g(OG, G))}),
                    ("outsider", "add company into a group of %s" % P.WS, comp, {"in": O(inGroup=g(G))}),
                    ("outsider", "add company into own group and a group of %s" % P.WS, comp, {"in": O(inGroup=g(OG, G))}),
                    ("outsider", "update own candidate: add a group of %s" % P.WS, updc,
                     {"in": {"filter": {"id": [OC]}, "set": {"inGroup": g(G)}}}),
                    ("outsider", "add attachment in own group, on a candidate of %s" % P.WS, att,
                     {"in": T(inGroup=g(OG), forResource=g(A))}),
                    ("outsider", "add attachment in own group, on own candidate and one of %s" % P.WS, att,
                     {"in": T(inGroup=g(OG), forResource=g(OC, A))}),
                    ("outsider", "add attachment in a group of %s, on own candidate" % P.WS, att,
                     {"in": T(inGroup=g(G), forResource=g(OC))}),
                    ("outsider", "update own attachment: also on a candidate of %s" % P.WS, updt,
                     {"in": {"filter": {"id": [OT]}, "set": {"forResource": g(A)}}}),
                    ("outsider", "update own candidate: take an attachment of %s" % P.WS, updc,
                     {"in": {"filter": {"id": [OC]}, "set": {"hasAttachment": g(t["T_A"])}}}),
                    ("outsider", "add candidate with an attachment of %s inside" % P.WS, cand,
                     {"in": C(inGroup=g(OG), hasAttachment=g(t["T_A"]))}),
                    ("outsider", "add a note on a candidate of %s" % P.WS, note, {"in": [{"text": "spike", "forResource": g(A)}]}),
                    ("outsider", "add a note mentioning a user of %s" % P.WS, note, {"in": [{"text": "spike", "forResource": g(OC), "mentions": g(t["U"])}]}),
                    ("outsider", "add a tag on a candidate of %s" % P.WS, tag, {"in": [{"name": "spike-cross", "tagable": g(A)}]}),
                    ("outsider", "add a tag in a group of %s" % P.WS, tag, {"in": [{"name": "spike-cross", "inGroup": g(G)}]}),
                    ("outsider", "add an application for a candidate of %s" % P.WS, app, {"in": [{"hasCandidate": {"id": A}, "inGroup": g(OG)}]}),
                    ("outsider", "update own candidate: an address of %s" % P.WS, updc,
                     {"in": {"filter": {"id": [OC]}, "set": {"hasAddress": {"id": t["A2_addr"]}}}}),
                    ("outsider", "add an attachment on a message of %s" % P.WS, att, {"in": T(inGroup=g(OG), forResource=g(MSG))} if MSG else None),
                    ("outsider", "add a candidate naming the workspace %s" % P.WS, cand, {"in": C(inGroup=g(OG), inWorkspace={"id": ws})}),
                ]),
                ("should be allowed", [
                    ("outsider", "add candidate into own group", cand, {"in": C(inGroup=g(OG))}),
                    ("outsider", "add company into own group", comp, {"in": O(inGroup=g(OG))}),
                    ("outsider", "add attachment on own candidate", att, {"in": T(inGroup=g(OG), forResource=g(OC))}),
                    ("outsider", "update own attachment", updt, {"in": {"filter": {"id": [OT]}, "set": {"expiryDate": "2030-01-01T00:00:00Z"}}}),
                    ("owner", "add candidate into a group", cand, {"in": C(inGroup=g(G))}),
                    ("owner", "add candidate into two groups", cand, {"in": C(inGroup=g(G, H))}),
                    ("owner", "add candidate, no group given", cand, {"in": C()}),
                    ("owner", "add company into a group", comp, {"in": O(inGroup=g(G))}),
                    ("owner", "update a candidate: add another group", updc, {"in": {"filter": {"id": [N]}, "set": {"inGroup": g(H)}}}),
                    ("owner", "update a candidate: remove a group", updc, {"in": {"filter": {"id": [N]}, "remove": {"inGroup": g(G)}}}),
                    ("owner", "update a candidate: a plain field", updc, {"in": {"filter": {"id": [N]}, "set": {"specialty": "spike"}}}),
                    ("owner", "add attachment on a candidate", att, {"in": T(forResource=g(N))}),
                    ("owner", "update an attachment: also on another candidate", updt,
                     {"in": {"filter": {"id": [t["T_A"]]}, "set": {"forResource": g(N)}}}),
                    ("owner", "add candidate with a new attachment inside", cand, {"in": C(inGroup=g(G), hasAttachment=[{"category": "RESUME"}])}),
                    ("editor", "add candidate into their group", cand, {"in": C(inGroup=g(G))}),
                    ("editor", "add company into their group", comp, {"in": O(inGroup=g(G))}),
                    ("editor", "add attachment on a candidate of their group", att, {"in": T(forResource=g(N))}),
                    ("editor", "update a candidate of their group", updc, {"in": {"filter": {"id": [N]}, "set": {"specialty": "spike"}}}),
                    ("outsider", "update own candidate: remove a group of %s" % P.WS, updc,
                     {"in": {"filter": {"id": [OC]}, "remove": {"inGroup": g(G)}}}),
                    ("outsider", "add a note on own candidate", note, {"in": [{"text": "spike", "forResource": g(OC)}]}),
                    ("outsider", "add a tag on own candidate", tag, {"in": [{"name": "spike-cross", "tagable": g(OC)}]}),
                    ("outsider", "add an application for own candidate", app, {"in": [{"hasCandidate": {"id": OC}, "inGroup": g(OG)}]}),
                    ("owner", "add a note on a candidate", note, {"in": [{"text": "spike", "forResource": g(N)}]}),
                    ("owner", "add a note mentioning a user", note, {"in": [{"text": "spike", "forResource": g(N), "mentions": g(t["U"])}]}),
                    ("owner", "add a tag on a candidate", tag, {"in": [{"name": "spike-cross", "tagable": g(N)}]}),
                    ("owner", "add an application for a candidate", app, {"in": [{"hasCandidate": {"id": N}, "inGroup": g(G)}]}),
                    ("owner", "add an attachment on a message", att, {"in": T(forResource=g(MSG))} if MSG else None),
                    ("editor", "add a note on a candidate of their group", note, {"in": [{"text": "spike", "forResource": g(N)}]}),
                ]),
            ]
            schemas = arms(original)
            results, messages = {}, {}
            for name, schema in schemas:
                r = P.set_schema(c, schema)
                if r.get("errors"):
                    print("\n%s: the schema was refused: %s" % (name, json.dumps(r["errors"])[:600]))
                    continue
                for _, group in cases:
                    for who, label, q, v in group:
                        if v is None:
                            continue
                        got, m = run(c, toks[who], q, v)
                        results[(name, who, label)] = got
                        if m:
                            messages.setdefault(re.sub(r"0x[0-9a-f]+", "<uid>", m)[:260], []).append("%s / %s: %s" % (name, who, label))
            names = [n for n, _ in schemas if any(k[0] == n for k in results)]
            for title, group in cases:
                print("\n%s (numUids of a dry run)" % title)
                print("  %-9s %-58s %s" % ("", "", " ".join("%-8s" % n for n in names)))
                for who, label, _, _ in group:
                    print("  %-9s %-58s %s" % (who, label[:58], " ".join("%-8s" % results.get((n, who, label), "-") for n in names)))
            for m, where in messages.items():
                print("  message: %s\n      on: %s%s" % (m, "; ".join(where[:3]), " and %d more" % (len(where) - 3) if len(where) > 3 else ""))

            # Committed, under each schema: what each side then sees. The ties are cut again after each.
            add_t = "mutation($in: [AddAttachmentInput!]!) { m: addAttachment(input: $in) { attachment { id } } }"
            seen = "query($id: [ID!]) { r: queryCandidate(filter: {id: $id}) { hasAttachment { id fileName category } } }"
            held = lambda who, cand: [x["id"] for r in (W.ask(c, toks[who], seen, {"id": [cand]})[0] or []) for x in r.get("hasAttachment") or []]
            print("\ncommitted writes, and what each side then reads")
            for name, schema in schemas:
                if name not in names:
                    continue
                P.set_schema(c, schema)
                d, e = W.ask(c, toks["outsider"], add_t, {"in": [{"category": "OTHER", "inGroup": [{"id": OG}], "forResource": [{"id": A}]}]})
                ids = [x["id"] for x in (d or {}).get("attachment") or []]
                if ids:
                    print("  %-6s outsider hangs a new attachment on a candidate of %s: accepted; read through that candidate by owner: %s, editor: %s" % (
                        name, P.WS, ids[0] in held("owner", A), ids[0] in held("editor", A)))
                    c.mutate("<%s> <AttachmentOwner.hasAttachment> <%s> .\n<%s> <Attachment.forResource> <%s> ." % (A, ids[0], ids[0], A), delete=True)
                else:
                    print("  %-6s outsider hangs a new attachment on a candidate of %s: refused %s" % (name, P.WS, [m[:110] for m in e]))
                for label, q, v, cut in (
                        ("outsider puts an attachment of %s on their own candidate" % P.WS,
                         "mutation($in: UpdateCandidateInput!) { m: updateCandidate(input: $in) { numUids } }",
                         {"in": {"filter": {"id": [OC]}, "set": {"hasAttachment": g(t["T_A"])}}}, (OC, t["T_A"])),
                        ("outsider puts their own attachment on a candidate of %s" % P.WS,
                         "mutation($in: UpdateAttachmentInput!) { m: updateAttachment(input: $in) { numUids } }",
                         {"in": {"filter": {"id": [OT]}, "set": {"forResource": g(A)}}}, (A, OT)),
                        ("outsider puts their own candidate in a group of %s" % P.WS,
                         "mutation($in: UpdateCandidateInput!) { m: updateCandidate(input: $in) { numUids } }",
                         {"in": {"filter": {"id": [OC]}, "set": {"inGroup": g(G)}}}, None)):
                    d, e = W.ask(c, toks["outsider"], q, v)
                    n = (d or {}).get("numUids")
                    if cut:
                        tied = W.has_edge(c, cut[0], "AttachmentOwner.hasAttachment", cut[1])
                        extra = ""
                        if tied and cut[0] == OC:
                            extra = "; the outsider reads it through their candidate: %s" % (cut[1] in held("outsider", OC))
                        if tied and cut[0] == A:
                            extra = "; read through that candidate by owner: %s" % (cut[1] in held("owner", A))
                        print("  %-6s %s: numUids %s, tied: %s%s %s" % (name, label, n, tied, extra, [m[:110] for m in e] or ""))
                        if tied:
                            c.mutate("<%s> <AttachmentOwner.hasAttachment> <%s> .\n<%s> <Attachment.forResource> <%s> ." % (cut + (cut[1], cut[0])), delete=True)
                    else:
                        tied = W.has_edge(c, OC, "Groupable.inGroup", G)
                        print("  %-6s %s: numUids %s, in the group: %s %s" % (name, label, n, tied, [m[:110] for m in e] or ""))
                        if tied:
                            c.mutate("<%s> <Groupable.inGroup> <%s> .\n<%s> <Group.hasCandidate> <%s> ." % (OC, G, G, OC), delete=True)
        finally:
            if P.get_schema(c) != original:
                P.set_schema(c, original)
            n, edges, left = W.remove_new(c, marker)
            after = c.dql(count)["data"]
            print("\nremoved %d nodes the probe added and %d edges to them; still there: %s; counts unchanged: %s" % (
                n, edges, left, json.dumps(before, sort_keys=True) == json.dumps(after, sort_keys=True)))


if __name__ == "__main__":
    main()
