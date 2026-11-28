"""Who may maintain a record of the control plane: its owning workspace, and in that
workspace a member who holds the permission (spike probe, not a proof: it prints and
asserts nothing).

An App is in the catalogue, which every workspace reads. A workspace makes one, keeps
it, and can publish it. Two shapes of the same requirement, each on a fresh small
cluster, with the same callers doing the same things:

  today's shape   one namespace. The App has an edge to its Workspace, and the rule
                  follows it: the caller's workspace is that one, and the caller is a
                  member of it who may publish.

  partitions      App is in the control plane and records its owner by name. The
                  rule is two parts: the record's owner is the caller's workspace
                  (an ordinary rule, on the record), and the caller may publish in
                  their own workspace (a tenantRule, asked of the caller's partition).

Callers: a member of acme who may publish, a member of acme who may not, a member of
globex who may publish there, and that last one again with a token that claims, under
the engine's own name, that the tenant rule was answered yes.

  python3 probe_app_ownership.py
"""
import json
import time

import proof_resolver as R
from spikelib import Cluster

AUTH = '# Dgraph.Authorization {"VerificationKey":"%s","Header":"%s","Algo":"HS256","Namespace":"%s","Audience":["%s"]}' % (
    R.SECRET, R.HEADER, R.CLAIMS_NS, R.AUDIENCE)

MEMBER = '''
type Member {
  id: ID!
  email: String! @search(by: [hash])
  mayPublish: Boolean @search
}
'''

# the question the partition shape asks of the caller's tenant
MAY_PUBLISH = "query($sub: String!) { queryWorkspace { members(filter: {email: {eq: $sub}, mayPublish: true}) { id } } }"

TODAY = MEMBER + '''
type Workspace {
  id: ID!
  name: String! @id
  members: [Member]
}

type App @auth(
  query: { or: [
    { rule: "query { queryApp(filter: {published: true}) { clientId } }" },
    { rule: "query($ws: String!) { queryApp { inWorkspace(filter: {name: {eq: $ws}}) { name } } }" } ] },
  add: { rule: "%(edge)s" }, update: { rule: "%(edge)s" }, delete: { rule: "%(edge)s" }
) {
  clientId: String! @id
  title: String
  published: Boolean @search
  inWorkspace: Workspace!
}
''' % {"edge": "query($ws: String!, $sub: String!) { queryApp { inWorkspace(filter: {name: {eq: $ws}}) { "
               "members(filter: {email: {eq: $sub}, mayPublish: true}) { id } } } }"} + AUTH

OWNED = "query($ws: String!) { queryApp(filter: {ownedBy: {eq: $ws}}) { clientId } }"
PARTITIONS = MEMBER + '''
type Workspace @tenant(claim: "ws", name: "name") {
  id: ID!
  name: String! @id
  members: [Member]
}

type App @scope(plane: control) @auth(
  query: { or: [
    { rule: "query { queryApp(filter: {published: true}) { clientId } }" },
    { rule: "%(owned)s" } ] },
  add:    { and: [ { rule: "%(owned)s" }, { tenantRule: "%(may)s" } ] },
  update: { and: [ { rule: "%(owned)s" }, { tenantRule: "%(may)s" } ] },
  delete: { and: [ { rule: "%(owned)s" }, { tenantRule: "%(may)s" } ] }
) {
  clientId: String! @id
  title: String
  published: Boolean @search
  ownedBy: String! @search(by: [hash])
}
''' % {"owned": OWNED, "may": MAY_PUBLISH} + AUTH


def rule_name(text):
    """The name the engine supplies a tenant rule's answer under (schema/tenant_rule.go)."""
    h = 0xcbf29ce484222325
    for b in " ".join(text.split()).encode():
        h = ((h ^ b) * 0x100000001b3) & 0xFFFFFFFFFFFFFFFF
    return "dgraph.derived.tenantRule.%016x" % h


def slug(n):
    return "-".join(n.replace(",", "").split())


def gql(c, tok, query):
    r = c.post("/graphql", json.dumps({"query": query}), headers=R.auth(tok) if tok else None)
    if r.get("errors"):
        return "error: %s" % r["errors"][0].get("message", "")[:110]
    return json.dumps(r.get("data"))[:150]


def set_schema(c, text):
    r = c.admin("mutation($s: String!) { updateGQLSchema(input: { set: { schema: $s } }) { gqlSchema { id } } }",
                variables={"s": text}, check=False)
    if r.get("errors"):
        raise SystemExit("the schema was refused: %s" % r["errors"][0]["message"][:400])


def workspace(name, publisher, other):
    return ('mutation { addWorkspace(input: [{name: "%s", members: [{email: "%s", mayPublish: true}, '
            '{email: "%s", mayPublish: false}]}]) { numUids } }' % (name, publisher, other))


def callers(forged_name):
    # a caller the tenant rule refuses, with the answer written into the token: at the top and among the claims
    forged = R.token("acme", sub="bob@acme",
                     extra={forged_name: True, R.CLAIMS_NS: {"sub": "bob@acme", "ws": "acme", forged_name: True}})
    return [
        ("acme, may publish", R.token("acme", sub="ann@acme")),
        ("acme, may not publish", R.token("acme", sub="bob@acme")),
        ("acme, may not, token says yes", forged),
        ("globex, may publish there", R.token("globex", sub="gina@globex")),
    ]


def timed(c, tok, n=40):
    """Median and slowest of n retitles of acme's app by tok, in ms."""
    took = []
    for i in range(n):
        t = time.perf_counter()
        r = c.post("/graphql", json.dumps({"query": 'mutation { updateApp(input: {filter: {clientId: {eq: "acme-app"}}, '
                   'set: {title: "t%d"}}) { numUids } }' % i}), headers=R.auth(tok))
        took.append((time.perf_counter() - t) * 1000)
        if r.get("errors") or r["data"]["updateApp"]["numUids"] != 1:
            return "failed: %s" % json.dumps(r)[:120]
    took.sort()
    return "median %.1f ms, slowest %.1f ms over %d" % (took[len(took) // 2], took[-1], n)


def run(c, who, add_own, add_for_acme, counts):
    w = max(len(n) for n, _ in who)
    steps = [
        ("add an app for its own workspace", lambda n, t: gql(c, t, add_own(n))),
        ("add an app owned by acme", lambda n, t: gql(c, t, add_for_acme(n))),
        ("retitle acme's app", lambda n, t: gql(c, t,
            'mutation { updateApp(input: {filter: {clientId: {eq: "acme-app"}}, set: {title: "by %s"}}) { numUids } }' % n.split(",")[0])),
        ("read acme's unpublished app", lambda n, t: gql(c, t, '{ queryApp(filter: {clientId: {eq: "acme-app"}}) { clientId title } }')),
        ("delete acme's app", lambda n, t: gql(c, t, 'mutation { deleteApp(filter: {clientId: {eq: "acme-app"}}) { numUids } }')),
    ]
    for i, (label, step) in enumerate(steps):
        if i == len(steps) - 1:
            print("\n  retitle acme's app, by its publisher, timed: %s" % timed(c, who[0][1]))
        print("\n  %s" % label)
        # the owner who may publish goes last, so the others try against a record that is there
        for n, t in who[1:] + who[:1]:
            print("    %-*s  %s" % (w, n, step(n, t)))
    print("\n  apps left: %s" % counts())


def main():
    name = rule_name(MAY_PUBLISH)
    who = callers(name)

    print("TODAY'S SHAPE: one namespace, the app has an edge to its workspace\n")
    with Cluster("ao", 490, flags="partition-namespaces=false") as c:
        set_schema(c, TODAY)
        for ws, a, b in (("acme", "ann@acme", "bob@acme"), ("globex", "gina@globex", "gus@globex")):
            print("  workspace %s: %s" % (ws, gql(c, None, workspace(ws, a, b))))
        seed = 'mutation { addApp(input: [{clientId: "acme-app", title: "Acme app", inWorkspace: {name: "acme"}}]) { numUids } }'
        print("  acme's app, by its publisher: %s" % gql(c, who[0][1], seed))
        run(c, who,
            lambda n: 'mutation { addApp(input: [{clientId: "new-%s", inWorkspace: {name: "%s"}}]) { numUids } }' % (
                slug(n), n.split(",")[0]),
            lambda n: 'mutation { addApp(input: [{clientId: "for-acme-%s", inWorkspace: {name: "acme"}}]) { numUids } }' % (slug(n)),
            lambda: gql(c, None, "{ aggregateApp { count } }") + "  (no token: nothing passes the rules)")

    print("\n\nPARTITIONS: the app is in the control plane and names its owner; the permission is asked of the caller's partition")
    print("the tenant rule's answer travels as %s\n" % name)
    flags = "partition-namespaces=true; partition-resolver=jwt; partition-dir-ttl=3s"
    with Cluster("ap", 500, flags=flags) as c:
        set_schema(c, PARTITIONS)
        for ws, a, b in (("acme", "ann@acme", "bob@acme"), ("globex", "gina@globex", "gus@globex")):
            c.add_partition(ws)
        time.sleep(3)
        for ws, a, b in (("acme", "ann@acme", "bob@acme"), ("globex", "gina@globex", "gus@globex")):
            print("  workspace %s, in its partition: %s" % (ws, gql(c, R.token(ws, sub=a), workspace(ws, a, b))))
        seed = 'mutation { addApp(input: [{clientId: "acme-app", title: "Acme app", ownedBy: "acme"}]) { numUids } }'
        print("  acme's app, by its publisher: %s" % gql(c, who[0][1], seed))

        def counts():
            r = c.post("/query", "{ q(func: type(App)) { c: App.clientId o: App.ownedBy } }", ctype="application/dql")
            root = sorted("%s (%s)" % (x.get("c"), x.get("o")) for x in (r.get("data") or {}).get("q") or [])
            r = c.post("/query", "{ q(func: type(App)) { c: App.clientId } }", ctype="application/dql", headers=R.auth(who[0][1]))
            return "in the root namespace %s, in acme's partition %s" % (root, (r.get("data") or {}).get("q") or [])

        run(c, who,
            lambda n: 'mutation { addApp(input: [{clientId: "new-%s", ownedBy: "%s"}]) { numUids } }' % (slug(n), n.split(",")[0]),
            lambda n: 'mutation { addApp(input: [{clientId: "for-acme-%s", ownedBy: "acme"}]) { numUids } }' % (slug(n)),
            counts)

        print("\n  from outside any partition (the workspace claim '*', the root namespace):")
        print("    retitle an app:  %s" % gql(c, R.token("*", sub="ann@acme"),
              'mutation { updateApp(input: {filter: {clientId: {eq: "new-acme-may-publish"}}, set: {title: "from root"}}) { numUids } }'))
        print("    read published:  %s" % gql(c, R.token("*", sub="ann@acme"), "{ queryApp { clientId } }"))


    print("\n\nPARTITIONS, with the partition's answer remembered for 30s (partition-fast-path, partition-grants-ttl)\n")
    with Cluster("aq", 510, flags=flags + "; partition-fast-path=true; partition-grants-ttl=30s") as c:
        set_schema(c, PARTITIONS)
        c.add_partition("acme")
        time.sleep(3)
        print("  workspace acme, in its partition: %s" % gql(c, who[0][1], workspace("acme", "ann@acme", "bob@acme")))
        print("  acme's app, by its publisher: %s" % gql(c, who[0][1], seed))
        print("  retitle acme's app, by its publisher, timed: %s" % timed(c, who[0][1]))
        print("  the member who may not:  %s" % gql(c, who[1][1],
              'mutation { updateApp(input: {filter: {clientId: {eq: "acme-app"}}, set: {title: "no"}}) { numUids } }'))
        grant = 'mutation { updateMember(input: {filter: {email: {eq: "bob@acme"}}, set: {mayPublish: true}}) { numUids } }'
        print("  that member is given the permission: %s" % gql(c, who[0][1], grant))
        print("  the same member, at once: %s" % gql(c, who[1][1],
              'mutation { updateApp(input: {filter: {clientId: {eq: "acme-app"}}, set: {title: "now"}}) { numUids } }'))
        time.sleep(31)
        print("  the same member, 31s on:  %s" % gql(c, who[1][1],
              'mutation { updateApp(input: {filter: {clientId: {eq: "acme-app"}}, set: {title: "later"}}) { numUids } }'))
        print("  the permission is taken away again: %s" % gql(c, who[0][1], grant.replace("mayPublish: true", "mayPublish: false")))
        print("  the same member, at once: %s" % gql(c, who[1][1],
              'mutation { updateApp(input: {filter: {clientId: {eq: "acme-app"}}, set: {title: "still"}}) { numUids } }'))
        time.sleep(31)
        print("  the same member, 31s on:  %s" % gql(c, who[1][1],
              'mutation { updateApp(input: {filter: {clientId: {eq: "acme-app"}}, set: {title: "gone"}}) { numUids } }'))


if __name__ == "__main__":
    main()
