"""Can a caller steer a reference the server makes to a record of another workspace
(spike probe, not a proof: it prints and asserts nothing).

Some fields are filled by the server from @default. Most are built from the caller's
own claims. Message.parentMessage is built by lambda from an ID written inside the
message body, <!--jobli-reply:{"id":"0x.."}-->, and the field is read without the
rules of the message it points at. So a reply can be threaded under any message whose
ID the caller writes, and that message read back through the reply.

Root namespace of the cluster proof_real.py built, partition namespaces off. A member
of one workspace, who is in a conversation there, replies to a message of another
workspace: once naming it in the input, once only in the body. Under today's schema
and under today's schema with the @referenceScope lines of graph/.graphql. The stand-in
for lambda answers Message.resolveParentMessage as lambda/src/message.js does.

Everything is committed, on records the probe makes for itself, and removed again.

  SPIKE_SUB=... SPIKE_EMAIL=... python3 probe_server_made_references.py
"""
import http.server
import json
import os
import re
import threading

import proof_real as P
import probe_phase1_writes as W
import probe_cross_workspace as X

WSM = W.WSM
OTHER = W.OTHER_WS
SECRET_BODY = "spike: a message of %s nobody outside should read" % OTHER
REPLY = re.compile(r"<!--\s*jobli-reply:\s*(\{.*?\})\s*-->")


class Stub(W.Stub):
    """W.Stub, with Message.resolveParentMessage answered as message.js answers it."""

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        out = {"code": 200, "allowed": True}
        try:
            b = json.loads(raw)
            if b.get("resolver") == "Message.resolveParentMessage":
                args = b.get("args") or {}
                out = args.get("parentMessage")
                m = REPLY.search(args.get("body") or "") if not out else None
                if m:
                    ident = json.loads(m.group(1)).get("id")
                    out = {"id": ident.strip()} if isinstance(ident, str) and re.fullmatch(r"0x[0-9a-fA-F]+", ident.strip()) else None
        except (ValueError, AttributeError):
            pass
        body = json.dumps(out).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def conversation(c, ws, user=None, body=None):
    """A conversation in a workspace, written as the app's own are: with a participant
    for user, or with one message."""
    rdf = ['_:c <dgraph.type> "%s" .' % t for t in ("Conversation", "WorkspaceMember", "Recordable")]
    rdf += ['_:c <Conversation.subject> "spike" .', '_:c <Recordable.deleted> "false"^^<xs:boolean> .', "_:c <%s> <%s> ." % (WSM, ws)]
    if user:
        rdf += ['_:p <dgraph.type> "ConversationParticipant" .', "_:p <ConversationParticipant.conversation> _:c .",
                "_:p <ConversationParticipant.user> <%s> ." % user, "_:c <Conversation.participants> _:p ."]
    if body:
        rdf += ['_:m <dgraph.type> "%s" .' % t for t in ("Message", "AttachmentOwner", "Recordable", "TokenUsageOwner")]
        rdf += ['_:m <Message.body> "%s" .' % body, '_:m <Message.role> "USER" .', '_:m <Recordable.deleted> "false"^^<xs:boolean> .',
                "_:m <Message.conversation> _:c .", "_:c <Conversation.messages> _:m ."]
    u = c.mutate("\n".join(rdf))["data"]["uids"]
    return u["c"], u.get("m")


def main():
    if not (P.SUB and P.EMAIL):
        raise SystemExit("set SPIKE_SUB and SPIKE_EMAIL to the userId and email of the owner of %s" % P.WS)
    server = http.server.ThreadingHTTPServer(("127.0.0.1", P.LAMBDA_PORT), Stub)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    original = open(os.path.join(P.OUT, "schema_today.graphql")).read()
    with P.cluster(P.OFF) as c:
        c.wait_ready(0, timeout=900)
        if P.get_schema(c) != original:
            P.set_schema(c, original)
        rows = lambda q: W.rows(c, q)
        while True:
            stale = sorted(W.uids(rows("{ q(func: type(SpikeMarker)) { uid } }")), key=lambda u: int(u, 16))
            if not stale:
                break
            print("an earlier run left its records behind: removed %s" % (W.remove_new(c, stale[0]),))
        ws = rows('{ q(func: eq(Workspace.name, "%s")) { uid } }' % P.WS)[0]["uid"]
        ows = rows('{ q(func: eq(Workspace.name, "%s")) { uid } }' % OTHER)[0]["uid"]
        me = rows('{ q(func: eq(User.userId, "%s")) @filter(uid_in(%s, %s)) { uid } }' % (P.SUB, WSM, ws))[0]["uid"]
        marker = c.mutate('_:m <dgraph.type> "SpikeMarker" .')["data"]["uids"]["m"]
        tok = P.token()
        try:
            mine, _ = conversation(c, ws, user=me)
            _, theirs = conversation(c, ows, body=SECRET_BODY)
            print("the caller's conversation in %s: %s; a message of %s: %s" % (P.WS, mine, OTHER, theirs))
            add = "mutation($in: [AddMessageInput!]!) { m: addMessage(input: $in) { message { id } } }"
            read = "query($id: ID!) { r: getMessage(id: $id) { parentMessage { id body } } }"
            direct = "query($id: ID!) { r: getMessage(id: $id) { id body } }"
            comment = lambda uid: '<!--jobli-reply:{"name":"You","text":"spike","id":"%s"}-->spike reply' % uid
            schemas = [(n, s) for n, s in X.arms(original) if n in ("today", "scope")]
            print("\n%-6s %-58s %s" % ("", "", "accepted  parent tied  parent's body read back"))
            for name, schema in schemas:
                r = P.set_schema(c, schema)
                if r.get("errors"):
                    print("%s: the schema was refused: %s" % (name, json.dumps(r["errors"])[:600]))
                    continue
                d, e = W.ask(c, tok, direct, {"id": theirs})
                print("%-6s the caller reads the message of %s by its ID: %s" % (name, OTHER, "yes" if d else "no"))
                d, e = W.ask(c, tok, add, {"in": [{"conversation": {"id": mine}, "role": "USER", "body": "spike first"}]})
                first = ((d or {}).get("message") or [{}])[0].get("id")
                print("%-6s %-58s %s" % (name, "a plain message in own conversation", "yes" if first else "no  %s" % (e[:1],)))
                cases = [
                    ("a reply to it, the parent named in the input", {"parentMessage": {"id": first}, "body": "spike reply"}),
                    ("a reply to it, the parent named only in the body", {"body": comment(first)}),
                    ("a reply to the message of %s, named in the input" % OTHER, {"parentMessage": {"id": theirs}, "body": "spike reply"}),
                    ("a reply to the message of %s, named only in the body" % OTHER, {"body": comment(theirs)}),
                ]
                for label, fields in cases:
                    d, e = W.ask(c, tok, add, {"in": [dict({"conversation": {"id": mine}, "role": "USER"}, **fields)]})
                    new = ((d or {}).get("message") or [{}])[0].get("id")
                    tied = seen = ""
                    if new:
                        p = W.uids(rows("{ q(func: uid(%s)) { p: Message.parentMessage { uid } } }" % new)[0].get("p") if rows(
                            "{ q(func: uid(%s)) { p: Message.parentMessage { uid } } }" % new) else None)
                        tied = "yes" if p else "no"
                        got, _ = W.ask(c, tok, read, {"id": new})
                        body = ((got or {}).get("parentMessage") or {}).get("body")
                        seen = "yes" if body else "no"
                        if body == SECRET_BODY:
                            seen = "yes: the other workspace's message"
                    print("%-6s %-58s %-9s %-12s %s%s" % (name, label[:58], "yes" if new else "no", tied, seen,
                                                         ("" if new else "  " + re.sub(r"0x[0-9a-f]+", "<uid>", (e or [""])[0])[:120])))
        finally:
            if P.get_schema(c) != original:
                P.set_schema(c, original)
            n, edges, left = W.remove_new(c, marker)
            print("\nremoved %d nodes the probe added and %d edges to them; still there: %s" % (n, edges, left))


if __name__ == "__main__":
    main()
