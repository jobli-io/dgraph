#!/usr/bin/env python3
"""Proof point for partition namespaces (spike): the JWT claim is the boundary.

The first spike chose the namespace from a plain request header. Here the alpha
runs with partition-resolver=jwt: the namespace comes from the workspace claim
of the JWT that the root schema's `# Dgraph.Authorization` line declares, looked
up in a directory in the root namespace. This checks the resolver from the
outside, as a client that wants to read another workspace's data would.

The JWT settings copy Jobli's local ones: HS256, the X-Auth-Token header, claims
under https://api.jobli.io/jwt/claims, an audience, and ClosedByDefault.

usage: proof_resolver.py
"""

import base64
import hashlib
import hmac
import json
import sys
import time

from spikelib import Checks, Cluster

SECRET = "spikesecret"
HEADER = "X-Auth-Token"
CLAIMS_NS = "https://api.jobli.io/jwt/claims"
AUDIENCE = "urn://api.jobli.io/graphql"

SCHEMA = '''
type Workspace @auth(
  query: { rule: "query ($ws: String) { queryWorkspace(filter: { name: { eq: $ws } }) { name } }" }
) {
  id: ID!
  name: String! @id
  notes: [Note] @hasInverse(field: inWorkspace)
}

type Note @auth(
  query: { rule: "query ($ws: String) { queryNote { inWorkspace(filter: { name: { eq: $ws } }) { name } } }" }
) {
  id: ID!
  text: String! @search(by: [term])
  inWorkspace: Workspace!
}

# Dgraph.Authorization {"VerificationKey":"%s","Header":"%s","Algo":"HS256","Namespace":"%s","Audience":["%s"],"ClosedByDefault":true}
''' % (SECRET, HEADER, CLAIMS_NS, AUDIENCE)

DIRECTORY = """
WorkspaceEntry.name: string @index(exact) @upsert .
WorkspaceEntry.namespace: int .
"""


def b64(raw):
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def token(ws, secret=SECRET, alg="HS256", exp=3600, aud=AUDIENCE, sub="user-1", extra=None):
    head = {"alg": alg, "typ": "JWT"}
    body = {"sub": sub, "aud": [aud], CLAIMS_NS: {"sub": sub}}
    if ws is not None:
        body[CLAIMS_NS]["ws"] = ws
    if exp is not None:
        body["exp"] = int(time.time()) + exp
    body.update(extra or {})
    signing = b64(json.dumps(head).encode()) + "." + b64(json.dumps(body).encode())
    if alg == "none":
        return signing + "."
    sig = hmac.new(secret.encode(), signing.encode(), hashlib.sha256).digest()
    return signing + "." + b64(sig)


def auth(tok):
    return {HEADER: tok}


def note_texts(c, tok=None, ns_header=None, headers=None):
    """Note texts visible over DQL, which applies no @auth rules: only the namespace decides."""
    h = dict(headers or {})
    if tok is not None:
        h.update(auth(tok))
    r = c.post("/query", "{ q(func: has(Note.text)) { t: Note.text } }", ns_header, "application/dql", headers=h)
    if r.get("errors"):
        return "rejected: %s" % r["errors"][0]["message"][:110]
    return sorted(x["t"] for x in (r.get("data") or {}).get("q") or [])


def gql_notes(c, tok=None, ns_header=None):
    r = c.graphql("{ queryNote { text } }", ns_header, headers=auth(tok) if tok is not None else None)
    if r.get("errors"):
        return "rejected: %s" % r["errors"][0]["message"][:110]
    return sorted(x["text"] for x in (r.get("data") or {}).get("queryNote") or [])


def add_ws(c, tok, name, texts):
    q = "mutation($in: [AddWorkspaceInput!]!) { addWorkspace(input: $in) { numUids } }"
    v = {"in": [{"name": name, "notes": [{"text": t} for t in texts]}]}
    return c.graphql(q, headers=auth(tok), variables=v)


def rejected(v):
    return isinstance(v, str) and v.startswith("rejected")


def main():
    t = Checks()
    flags = "partition-namespaces=true; partition-resolver=jwt; partition-dir-ttl=3s"
    with Cluster("rs", 450, flags=flags) as c:
        r = c.admin("mutation($s: String!) { updateGQLSchema(input: { set: { schema: $s } }) { gqlSchema { id } } }",
                    variables={"s": SCHEMA})
        r = c.alter(DIRECTORY)
        t.check("schema and directory predicates in the root namespace", not r.get("errors"), r.get("errors", ""))
        a, b = c.add_partition(), c.add_partition()
        c.mutate('_:a <WorkspaceEntry.name> "acme" .\n_:a <WorkspaceEntry.namespace> "%d"^^<xs:int> .\n'
                 '_:b <WorkspaceEntry.name> "globex" .\n_:b <WorkspaceEntry.namespace> "%d"^^<xs:int> .' % (a, b))
        time.sleep(3)
        acme, globex = token("acme"), token("globex")

        # -- the claim routes the request
        r = add_ws(c, acme, "acme", ["acme secret one", "acme secret two"])
        t.check("token for acme writes over GraphQL", not r.get("errors"), r.get("errors") or r.get("data"))
        r = add_ws(c, globex, "globex", ["globex secret"])
        t.check("token for globex writes over GraphQL", not r.get("errors"), r.get("errors") or r.get("data"))
        only_a, only_b = ["acme secret one", "acme secret two"], ["globex secret"]
        t.check("GraphQL: acme's token sees only acme", gql_notes(c, acme) == only_a, gql_notes(c, acme))
        t.check("GraphQL: globex's token sees only globex", gql_notes(c, globex) == only_b, gql_notes(c, globex))
        t.check("DQL: acme's token sees only acme", note_texts(c, acme) == only_a, note_texts(c, acme))
        t.check("DQL: globex's token sees only globex", note_texts(c, globex) == only_b, note_texts(c, globex))

        # -- no credential
        t.check("no token, GraphQL: refused by ClosedByDefault", rejected(gql_notes(c)), gql_notes(c))
        t.check("no token, DQL: lands in the root namespace, which holds no tenant data",
                note_texts(c) == [], note_texts(c))

        # -- the old header has no standing
        t.check("no token + partition header naming acme: still the root namespace",
                note_texts(c, ns_header=a) == [], note_texts(c, ns_header=a))
        t.check("globex's token + partition header naming acme: still globex",
                note_texts(c, globex, ns_header=a) == only_b, note_texts(c, globex, ns_header=a))
        t.check("globex's token + a namespace header naming acme: still globex",
                note_texts(c, globex, headers={"namespace": str(a)}) == only_b)

        # -- tokens that must not verify
        bad = {
            "signed with another key": token("acme", secret="not-the-secret"),
            "signature removed (alg none)": token("acme", alg="none"),
            "expired": token("acme", exp=-60),
            "wrong audience": token("acme", aud="urn://somewhere-else"),
            "payload edited after signing (globex -> acme)": None,
            "not a JWT": "acme",
        }
        head, body, sig = globex.split(".")
        edited = json.loads(base64.urlsafe_b64decode(body + "=" * (-len(body) % 4)))
        edited[CLAIMS_NS]["ws"] = "acme"
        bad["payload edited after signing (globex -> acme)"] = head + "." + b64(json.dumps(edited).encode()) + "." + sig
        for name, tok in bad.items():
            d, g = note_texts(c, tok), gql_notes(c, tok)
            t.check("token %s: rejected on DQL and GraphQL" % name, rejected(d) and rejected(g), d)

        # -- claims that verify but name no tenant
        d = note_texts(c, token("initech"))
        t.check("valid token for a workspace not in the directory: rejected, not sent to the root",
                rejected(d), d)
        t.check("valid token with ws \"*\" (discovery): root namespace, no tenant data",
                note_texts(c, token("*")) == [], note_texts(c, token("*")))
        t.check("valid token without a ws claim: root namespace, no tenant data",
                note_texts(c, token(None)) == [], note_texts(c, token(None)))
        d = note_texts(c, token("acme", extra={"namespace": b, "ws": "globex"}))
        t.check("top-level namespace and ws claims do not override the claims namespace",
                d == only_a, d)

        # -- reaching across by UID
        uid = c.post("/query", "{ q(func: has(Note.text), first: 1) { uid } }", ctype="application/dql",
                     headers=auth(acme))["data"]["q"][0]["uid"]
        r = c.post("/query", "{ q(func: uid(%s)) { uid t: Note.text expand(_all_) } }" % uid,
                   ctype="application/dql", headers=auth(globex))
        seen = [x for x in (r.get("data") or {}).get("q") or [] if len(x) > 1]
        t.check("globex reads an acme UID over DQL: no fields come back", seen == [], r.get("data"))
        r = c.graphql('{ getNote(id: "%s") { text } }' % uid, headers=auth(globex))
        t.check("globex reads an acme UID over GraphQL: nothing",
                (r.get("data") or {}).get("getNote") is None, r.get("data"))
        c.post("/mutate?commitNow=true", '{ set { <%s> <Note.text> "overwritten by globex" . } }' % uid,
               ctype="application/rdf", headers=auth(globex))
        t.check("globex writes to an acme UID: acme's note is unchanged",
                note_texts(c, acme) == only_a, note_texts(c, acme))
        c.post("/mutate?commitNow=true", "{ delete { <%s> <Note.text> * . } }" % uid, ctype="application/rdf",
               headers=auth(globex))
        t.check("globex deletes an acme UID: acme's note is still there",
                note_texts(c, acme) == only_a, note_texts(c, acme))
        r = c.post("/alter", '{"drop_all": true}', ctype="application/json", headers=auth(globex))
        t.check("globex's token cannot drop everything", note_texts(c, acme) == only_a and bool(r.get("errors")),
                (r.get("errors") or [{}])[0].get("message", r))
        r = c.post("/alter", "Note.text: string .", ctype="text/plain", headers=auth(globex))
        t.check("a tenant token cannot alter the schema", bool(r.get("errors")),
                (r.get("errors") or [{}])[0].get("message", ""))
        r = c.post("/admin", json.dumps({"query": "mutation { deleteNamespace(input: {namespaceId: %d}) "
                                                  "{ namespaceId } }" % a}), headers=auth(globex))
        t.check("globex's token cannot use the admin API to delete acme's partition",
                bool(r.get("errors")) and note_texts(c, acme) == only_a,
                (r.get("errors") or [{}])[0].get("message", r))
        r = c.post("/admin", json.dumps({"query": "mutation { updateGQLSchema(input: {set: {schema: "
                                                  "\"type T { id: ID! name: String }\"}}) { gqlSchema { id } } }"}),
                   headers=auth(globex))
        t.check("globex's token cannot replace the GraphQL schema",
                bool(r.get("errors")) and gql_notes(c, acme) == only_a,
                (r.get("errors") or [{}])[0].get("message", r))
        r = c.post("/alter", '{"drop_op": "DATA"}', ctype="application/json", headers=auth(globex))
        t.check("globex drops its own data: acme is untouched",
                note_texts(c, acme) == only_a and note_texts(c, globex) == [], note_texts(c, globex))

        # -- the directory is the authority
        uid_a = c.rows('{ q(func: eq(WorkspaceEntry.name, "acme")) { uid } }')[0]["uid"]
        c.post("/mutate?commitNow=true", "{ delete { <%s> <WorkspaceEntry.name> * .\n"
               "<%s> <WorkspaceEntry.namespace> * . } }" % (uid_a, uid_a), ctype="application/rdf")
        t.check("acme's entry is removed from the directory",
                c.rows('{ q(func: eq(WorkspaceEntry.name, "acme")) { uid } }') == [])
        time.sleep(4)
        d = note_texts(c, acme)
        t.check("after acme leaves the directory its token is rejected within the cache time", rejected(d), d)
        c.mutate('_:a <WorkspaceEntry.name> "acme" .\n_:a <WorkspaceEntry.namespace> "%d"^^<xs:int> .' % a)
        time.sleep(3)
        t.check("and works again once the entry is back", note_texts(c, acme) == only_a, note_texts(c, acme))
        c.mutate('_:x <WorkspaceEntry.name> "dup" .\n_:x <WorkspaceEntry.namespace> "%d"^^<xs:int> .\n'
                 '_:y <WorkspaceEntry.name> "dup" .\n_:y <WorkspaceEntry.namespace> "%d"^^<xs:int> .' % (a, b))
        d = note_texts(c, token("dup"))
        t.check("a name with two directory entries is rejected", rejected(d), d)
        c.mutate('_:z <WorkspaceEntry.name> "rootish" .\n_:z <WorkspaceEntry.namespace> "0"^^<xs:int> .')
        d = note_texts(c, token("rootish"))
        t.check("a directory entry cannot send a workspace to the root namespace", rejected(d), d)

        # -- cost of resolving on every request
        def timed(tokens):
            start = time.time()
            for tok in tokens:
                c.post("/query", "{ q(func: has(Note.text), first: 1) { uid } }", ctype="application/dql",
                       headers=auth(tok))
            return (time.time() - start) * 1000 / len(tokens)
        n = 500
        fresh = [token("acme", sub="user-%d" % i) for i in range(n)]  # each verified and looked up
        timed([acme] * 50)
        seen_before, first_use = timed([acme] * n), timed(fresh)
        print("round trip per DQL request in acme: %.2f ms with a token seen before, "
              "%.2f ms with a token used for the first time" % (seen_before, first_use))

        _, tablets, _ = c.state()
        t.check("no tablets outside the root namespace", all(x.startswith("0-") for x in tablets),
                "%d tablets" % len(tablets))
    sys.exit(t.done())


if __name__ == "__main__":
    main()
