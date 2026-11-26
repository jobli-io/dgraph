#!/usr/bin/env python3
"""Proof points for partition namespaces (spike): isolation and inheritance.

Runs against an alpha started with --feature-flags "partition-namespaces=true".
It applies a small GraphQL schema to the root namespace, creates partitions and
checks that each partition sees only its own data while Zero's tablet list does
not grow.

usage: proof_isolation.py [alpha http address, default localhost:8280]
"""

import json
import sys
import time
import urllib.error
import urllib.request

ALPHA = "http://" + (sys.argv[1] if len(sys.argv) > 1 else "localhost:8280")
NS_HEADER = "x-dgraph-partition-ns"

SCHEMA = """
type Workspace {
  id: ID!
  name: String! @id
  members: [Candidate] @hasInverse(field: inWorkspace)
}
type Candidate {
  id: ID!
  email: String! @id
  name: String @search(by: [term, exact])
  score: Int @search
  inWorkspace: Workspace
}
"""

results = []


def call(path, body, ns=None, ctype="application/json"):
    data = body if isinstance(body, bytes) else body.encode()
    req = urllib.request.Request(ALPHA + path, data=data, method="POST")
    req.add_header("Content-Type", ctype)
    if ns is not None:
        req.add_header(NS_HEADER, str(ns))
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return json.loads(resp.read() or b"{}")
    except urllib.error.HTTPError as e:
        return json.loads(e.read() or b"{}")


def gql(query, ns=None):
    return call("/graphql", json.dumps({"query": query}), ns)


def admin(query):
    return call("/admin", json.dumps({"query": query}))


def dql(query, ns=None):
    return call("/query", query, ns, "application/dql")


def mutate(rdf, ns=None):
    return call("/mutate?commitNow=true", "{ set { %s } }" % rdf, ns, "application/rdf")


def alter(body, ns=None):
    return call("/alter", body, ns, "text/plain")


def tablets():
    with urllib.request.urlopen(ALPHA + "/state", timeout=30) as resp:
        state = json.loads(resp.read())
    out = []
    for group in state.get("groups", {}).values():
        out.extend(group.get("tablets", {}).keys())
    return out


def check(name, ok, detail=""):
    results.append(ok)
    print("%s  %s%s" % ("PASS" if ok else "FAIL", name, ("  -> " + detail) if detail else ""))


def names(resp, field="queryCandidate"):
    rows = (resp.get("data") or {}).get(field) or []
    return sorted(r.get("name") or "" for r in rows)


def errors(resp):
    return json.dumps(resp.get("errors") or resp.get("data") or resp)[:160]


def main():
    resp = call("/admin/schema", SCHEMA)
    check("schema applied to the root namespace", "errors" not in resp, errors(resp) if "errors" in resp else "")

    # Zero's tablet list reaches the alpha with the next membership update.
    before = tablets()
    for _ in range(20):
        if any(t.endswith("Candidate.email") for t in before):
            break
        time.sleep(0.5)
        before = tablets()

    created = []
    for _ in range(3):
        r = admin("mutation { addNamespace { namespaceId } }")
        created.append(r["data"]["addNamespace"]["namespaceId"])
    a, b, c = created
    after = tablets()
    check(
        "creating 3 partitions adds no tablets in Zero",
        len(after) == len(before) and all(t.startswith("0-") for t in after),
        "tablets %d -> %d" % (len(before), len(after)),
    )

    # The same @id values in two partitions: uniqueness is per partition.
    add = """mutation { addWorkspace(input: [{name: "acme", members: [
        {email: "a@x.com", name: "%s", score: %d}, {email: "b@x.com", name: "%s", score: %d}]}])
        { numUids } }"""
    r = gql(add % ("Ada Lovelace", 10, "Grace Hopper", 20), a)
    check("partition %d: add workspace and members over GraphQL" % a, "errors" not in r, errors(r))
    r = gql(add % ("Alan Turing", 30, "Edsger Dijkstra", 40), b)
    check("partition %d: same @id values accepted" % b, "errors" not in r, errors(r))
    r = gql('mutation { addWorkspace(input: [{name: "root-ws", members: [{email: "r@x.com", name: "Root User"}]}]) { numUids } }')
    check("root namespace: add its own data", "errors" not in r, errors(r))

    q = "{ queryCandidate { name } }"
    check("partition %d sees only its candidates" % a, names(gql(q, a)) == ["Ada Lovelace", "Grace Hopper"], str(names(gql(q, a))))
    check("partition %d sees only its candidates" % b, names(gql(q, b)) == ["Alan Turing", "Edsger Dijkstra"], str(names(gql(q, b))))
    check("root namespace sees only its candidates", names(gql(q)) == ["Root User"], str(names(gql(q))))
    check("partition %d (no data) sees nothing" % c, names(gql(q, c)) == [], str(names(gql(q, c))))

    # Index lookups: the term index is shared in schema, separate in data.
    q = '{ queryCandidate(filter: {name: {anyofterms: "Ada Alan Root"}}) { name } }'
    check("term index, partition %d" % a, names(gql(q, a)) == ["Ada Lovelace"], str(names(gql(q, a))))
    check("term index, partition %d" % b, names(gql(q, b)) == ["Alan Turing"], str(names(gql(q, b))))
    q = "{ queryCandidate(filter: {score: {ge: 20}}) { name } }"
    check("int index, partition %d" % a, names(gql(q, a)) == ["Grace Hopper"], str(names(gql(q, a))))

    # @id lookup and inverse edge traversal.
    r = gql('{ getWorkspace(name: "acme") { members { name } } }', b)
    got = sorted(m["name"] for m in ((r.get("data") or {}).get("getWorkspace") or {}).get("members") or [])
    check("@id lookup and edge traversal, partition %d" % b, got == ["Alan Turing", "Edsger Dijkstra"], str(got))
    r = gql('{ getCandidate(email: "a@x.com") { name inWorkspace { name } } }', a)
    got = (r.get("data") or {}).get("getCandidate") or {}
    check("inverse edge, partition %d" % a, got.get("name") == "Ada Lovelace" and (got.get("inWorkspace") or {}).get("name") == "acme", json.dumps(got))

    # DQL: type index, expand(_all_) through the template's type, and count.
    q = "{ q(func: type(Candidate)) { expand(_all_) } }"
    got = sorted(n.get("Candidate.name", "") for n in (dql(q, a).get("data") or {}).get("q", []))
    check("DQL type() and expand(_all_), partition %d" % a, got == ["Ada Lovelace", "Grace Hopper"], str(got))
    q = "{ q(func: has(Candidate.email)) { count(uid) } }"
    counts = [((dql(q, n).get("data") or {}).get("q") or [{}])[0].get("count") for n in (a, b, c, None)]
    check("DQL count per namespace", counts == [2, 2, 0, 1], "a,b,c,root = %s" % counts)

    # A uid is global, but the data behind it is keyed by namespace.
    uid = (dql('{ q(func: eq(Candidate.email, "a@x.com")) { uid } }', a).get("data") or {}).get("q", [{}])[0].get("uid")
    leaked = (dql("{ q(func: uid(%s)) { Candidate.name Candidate.email } }" % uid, b).get("data") or {}).get("q", [])
    check("a uid from partition %d exposes nothing in partition %d" % (a, b), leaked == [], "%s -> %s" % (uid, leaked))
    leaked = (dql("{ q(func: uid(%s)) { Candidate.name Candidate.email } }" % uid).get("data") or {}).get("q", [])
    check("a uid from partition %d exposes nothing in the root namespace" % a, leaked == [], str(leaked))

    # A partition cannot own schema.
    r = alter("spikeNewPred: string @index(exact) .", a)
    check("alter from a partition is rejected", "errors" in r, errors(r))
    r = mutate('_:n <spikeUnknownPred> "x" .', a)
    check("mutation on a predicate missing from the template is rejected", "errors" in r, errors(r))
    r = mutate('_:n <Candidate.name> "DQL Written" .\n_:n <Candidate.email> "d@x.com" .\n_:n <dgraph.type> "Candidate" .', a)
    check("DQL mutation on template predicates works in a partition", "errors" not in r, errors(r))
    got = names(gql('{ queryCandidate(filter: {name: {anyofterms: "Written"}}) { name } }', a))
    check("... and is indexed and visible there only", got == ["DQL Written"] and names(gql('{ queryCandidate(filter: {name: {anyofterms: "Written"}}) { name } }', b)) == [], str(got))

    # Lifecycle: dropping one partition's data leaves the others alone.
    r = alter('{"drop_op": "DATA"}', a)
    check("drop data in partition %d" % a, "errors" not in r, errors(r))
    q = "{ queryCandidate { name } }"
    check("partition %d is empty after its drop" % a, names(gql(q, a)) == [], str(names(gql(q, a))))
    check("partition %d is untouched" % b, names(gql(q, b)) == ["Alan Turing", "Edsger Dijkstra"], str(names(gql(q, b))))
    check("root namespace is untouched", names(gql(q)) == ["Root User"], str(names(gql(q))))
    r = gql('mutation { addCandidate(input: [{email: "z@x.com", name: "Zed After"}]) { numUids } }', a)
    check("partition %d accepts writes again after the drop" % a, "errors" not in r and names(gql(q, a)) == ["Zed After"], errors(r))

    r = admin("mutation { deleteNamespace(input: {namespaceId: %d}) { namespaceId } }" % b)
    check("delete partition %d" % b, "errors" not in r, errors(r))
    check("root namespace survives the delete", names(gql(q)) == ["Root User"], str(names(gql(q))))
    check("partition %d survives the delete" % a, names(gql(q, a)) == ["Zed After"], str(names(gql(q, a))))

    time.sleep(2)
    final = tablets()
    extra = sorted(set(final) - set(before))
    check("tablet list unchanged at the end", sorted(final) == sorted(before), "tablets %d -> %d %s" % (len(before), len(final), extra or ""))

    print("\n%d/%d passed" % (sum(results), len(results)))
    sys.exit(0 if all(results) else 1)


if __name__ == "__main__":
    main()
