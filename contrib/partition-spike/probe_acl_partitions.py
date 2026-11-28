"""Dgraph's own ACL with partition namespaces: what works as the fork stands (spike probe,
not a proof: it prints and asserts nothing).

A fresh small cluster with --acl, partition namespaces on, and the namespace taken from
the ACL access token as on any multi-tenant Dgraph (partition-resolver=acl). It tries
what an operator would do:

  - log in to the root namespace as its guardian, and make a partition
  - log in to the partition as a named guardian of the root namespace, and read and
    write it by DQL; check that neither token reads the other namespace
  - try to log in to the partition as a root user who is not a guardian, with a wrong
    password, and as nobody
  - the same again after the alpha restarts, and after the guardian is demoted
  - list namespaces, and delete the partition
  - what the audit log says about each request

  python3 probe_acl_partitions.py
"""
import glob
import gzip
import json
import os
import time
import urllib.request

from spikelib import ROOT, Cluster

PASSWORD = "password"
FLAGS = "partition-namespaces=true; partition-resolver=acl"


def wait_healthy(c, timeout=180):
    end = time.time() + timeout
    while time.time() < end:
        try:
            with urllib.request.urlopen(c.alpha_http(0) + "/health", timeout=5) as r:
                if json.loads(r.read())[0].get("status") == "healthy":
                    return
        except Exception:
            pass
        time.sleep(1)
    raise SystemExit("the alpha did not become healthy")


def admin(c, query, token=None):
    return c.post("/admin", json.dumps({"query": query}), headers={"X-Dgraph-AccessToken": token} if token else None)


def login(c, user, ns, password=PASSWORD):
    end = time.time() + 60
    while True:
        r = admin(c, 'mutation { login(userId: "%s", password: "%s", namespace: %d) { response { accessJWT } } }' % (user, password, ns))
        tok = (((r.get("data") or {}).get("login") or {}).get("response") or {}).get("accessJWT")
        if tok or time.time() > end:
            return tok, (r.get("errors") or [{}])[0].get("message", "")[:110]
        time.sleep(1)


def short(r):
    if r.get("errors"):
        return "refused: %s" % r["errors"][0].get("message", "")[:100]
    return json.dumps(r.get("data"))[:140]


def dql(c, q, token):
    return c.post("/query", q, ctype="application/dql", headers={"X-Dgraph-AccessToken": token} if token else None)


def mutate(c, rdf, token):
    return c.post("/mutate?commitNow=true", "{ set { %s } }" % rdf, ctype="application/rdf",
                  headers={"X-Dgraph-AccessToken": token} if token else None)


def main():
    work = os.path.join(ROOT, "run", "acl")
    secret, audit = os.path.join(ROOT, "run", "acl-secret"), os.path.join(ROOT, "run", "acl-audit")
    open(secret, "w").write("0123456789abcdef0123456789abcdef")
    for f in glob.glob(os.path.join(audit, "*")):
        os.remove(f)
    os.makedirs(audit, exist_ok=True)
    c = Cluster("acl", 760, flags=FLAGS, fresh=True,
                extra_alpha_args=["--acl", "secret-file=%s" % secret, "--audit", "output=%s" % audit])
    c.start(wait=False)
    try:
        wait_healthy(c)
        root, why = login(c, "groot", 0)
        print("log in to the root namespace as groot: %s" % ("a token" if root else "REFUSED: " + why))
        print("alter in the root namespace:           %s" % short(c.post("/alter", "name: string @index(exact) .\nsecret: string .",
                                                                    ctype="text/plain", headers={"X-Dgraph-AccessToken": root})))
        print("a read with no token:                  %s" % short(dql(c, "{ q(func: has(name)) { name } }", None)))
        r = admin(c, 'mutation { addNamespace(input: {password: "%s"}) { namespaceId message } }' % PASSWORD, root)
        ns = ((r.get("data") or {}).get("addNamespace") or {}).get("namespaceId")
        print("make a namespace as the root guardian: %s" % (ns if ns else short(r)))
        r = admin(c, 'mutation { addNamespace(input: {password: "%s"}) { namespaceId } }' % PASSWORD, None)
        print("make a namespace with no token:        %s" % short(r))
        if not ns:
            return
        # the operators are users of the root namespace: alice is a guardian, carol may read `name` and no more
        print("add a group that may read name:        %s" % short(admin(c, 'mutation { addGroup(input: [{name: "readers", '
              'rules: [{predicate: "name", permission: 4}]}]) { group { name } } }', root))[:70])
        for who, group in (("alice", "guardians"), ("carol", "readers")):
            r = admin(c, 'mutation { addUser(input: [{name: "%s", password: "password12", groups: [{name: "%s"}]}]) '
                         '{ user { name } } }' % (who, group), root)
            print("add %s to %-9s                  %s" % (who, group + ":", short(r)[:70]))
        time.sleep(2)
        print("write in the root namespace:           %s" % short(mutate(c, '_:a <name> "in root" .\n_:a <secret> "root secret" .', root))[:40])

        def enter(who, password="password12"):
            tok, why = login(c, who, ns, password)
            print("%-6s logs in to partition %d:          %s" % (who, ns, "a token" if tok else "REFUSED: " + why))
            return tok

        def tried(when):
            print("-- %s" % when)
            alice = enter("alice")
            enter("carol")
            enter("alice", "wrong-password")
            enter("mallory")
            if alice:
                print("alice writes in the partition:         %s" % short(mutate(c, '_:a <name> "in the partition, %s" .' % when, alice))[:40])
                print("alice reads the partition:             %s" % short(dql(c, "{ q(func: has(name)) { name secret } }", alice)))
            carol0, _ = login(c, "carol", 0, "password12")
            print("carol reads the root namespace:        %s" % short(dql(c, "{ q(func: has(name)) { name secret } }", carol0)))
            root_again, _ = login(c, "groot", 0)
            print("the root guardian reads the root:      %s" % short(dql(c, "{ q(func: has(name)) { name } }", root_again)))
            return alice, root_again

        alice, root = tried("before a restart")
        c.stop_alpha(0)
        c.start_alpha(0)
        wait_healthy(c)
        alice, root = tried("after a restart")
        r = admin(c, 'mutation { updateUser(input: {filter: {name: {eq: "alice"}}, remove: {groups: [{name: "guardians"}]}}) { user { name } } }', root)
        print("-- alice is taken out of guardians:    %s" % short(r)[:60])
        time.sleep(2)
        enter("alice")
        print("the token alice already holds:         %s" % short(dql(c, "{ q(func: has(name)) { name } }", alice))[:90])
        part = alice
        r = admin(c, "{ state { namespaces } }", root)
        print("namespaces the root guardian is shown: %s" % short(r)[:120])
        r = admin(c, "mutation { deleteNamespace(input: {namespaceId: %d}) { namespaceId message } }" % ns, part)
        print("delete the partition with a partition token: %s" % short(r)[:110])
        r = admin(c, "mutation { deleteNamespace(input: {namespaceId: %d}) { namespaceId message } }" % ns, root)
        print("delete it as the root guardian:        %s" % short(r)[:110])
        time.sleep(2)
    finally:
        c.stop()
    lines = []
    for f in sorted(glob.glob(os.path.join(audit, "*"))):
        raw = open(f, "rb").read()
        try:
            raw = gzip.decompress(raw)
        except OSError:
            pass
        lines += [l for l in raw.decode("utf-8", "replace").split("\n") if l.strip()]
    print("\naudit log: %d entries in %s" % (len(lines), [os.path.basename(f) for f in glob.glob(os.path.join(audit, "*"))]))
    seen = {}
    for l in lines:
        try:
            e = json.loads(l)
        except ValueError:
            continue
        key = (e.get("user"), e.get("namespace"), e.get("endpoint"))
        seen[key] = seen.get(key, 0) + 1
    for (user, n, endpoint), k in sorted(seen.items(), key=lambda kv: str(kv[0])):
        print("  user %-22s namespace %-4s %-22s x%d" % (user, n, endpoint, k))
    if lines:
        print("  one entry: %s" % lines[-1][:400])


if __name__ == "__main__":
    main()
