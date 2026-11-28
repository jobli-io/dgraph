"""Two planes: a type the schema places in the control plane, asked for from inside a
tenant's partition (spike probe, not a proof: it prints and asserts nothing).

A fresh small cluster under the claim-based resolver. The schema has a catalogue type,
App, and tenant types that point at it: Plugin.hasApp. The catalogue is written once in
the root namespace. Then, with the token of one workspace:

  - list the catalogue, count it, and read it next to the workspace's own records
  - read the app under a plugin
  - make a plugin that names an app of the catalogue
  - change an app

and afterwards count the App records in each namespace.

  python3 probe_planes.py            # App carries @scope(plane: control)
  SCOPE=0 python3 probe_planes.py    # the same schema with no placement
"""
import json
import os
import time

import proof_resolver as R
from spikelib import Cluster

SCOPED = os.environ.get("SCOPE", "1") == "1"
SCHEMA = '''
type Workspace @tenant(claim: "ws", name: "name") {
  id: ID!
  name: String! @id
  title: String
}

type App %s {
  clientId: String! @id
  title: String
}

type Plugin {
  id: ID!
  label: String! @search(by: [exact])
  hasApp: App
}

type Note {
  id: ID!
  text: String!
}

# Dgraph.Authorization {"VerificationKey":"%s","Header":"%s","Algo":"HS256","Namespace":"%s","Audience":["%s"]}
''' % ("@scope(plane: control)" if SCOPED else "", R.SECRET, R.HEADER, R.CLAIMS_NS, R.AUDIENCE)


def gql(c, tok, query):
    r = c.post("/graphql", json.dumps({"query": query}), headers=R.auth(tok) if tok else None)
    if r.get("errors"):
        return "error: %s" % r["errors"][0].get("message", "")[:120]
    return json.dumps(r.get("data"))[:230]


def apps_in(c, tok):
    r = c.post("/query", "{ q(func: type(App)) { c: App.clientId t: App.title } }", ctype="application/dql",
               headers=R.auth(tok) if tok else None)
    return sorted("%s (%s)" % (x.get("c"), x.get("t") or "no title") for x in (r.get("data") or {}).get("q") or [])


def main():
    # the alpha is started with a claim nobody sends: the schema's @tenant says which claim names the tenant
    flags = "partition-namespaces=true; partition-resolver=jwt; partition-dir-ttl=3s; partition-claim=not-this-one"
    with Cluster("pl", 480, flags=flags) as c:
        r = c.admin("mutation($s: String!) { updateGQLSchema(input: { set: { schema: $s } }) { gqlSchema { id } } }",
                    variables={"s": SCHEMA}, check=False)
        if r.get("errors"):
            raise SystemExit("the schema was refused: %s" % r["errors"][0]["message"][:300])
        root = R.token("*")
        print("App is %s" % ("placed in the control plane" if SCOPED else "not placed"))
        print("the catalogue, written in the root namespace: %s" % gql(c, root,
              'mutation { addApp(input: [{clientId: "web", title: "Web app"}, {clientId: "seek", title: "Seek"}]) { numUids } }'))
        c.add_partition("acme")
        time.sleep(3)
        acme = R.token("acme")
        print("\nas the workspace acme:")
        print("  add a note:                          %s" % gql(c, acme, 'mutation { addNote(input: [{text: "acme note"}]) { numUids } }'))
        print("  list the catalogue:                  %s" % gql(c, acme, "{ queryApp { clientId title } }"))
        print("  count the catalogue:                 %s" % gql(c, acme, "{ aggregateApp { count } }"))
        print("  the catalogue beside its own notes:  %s" % gql(c, acme, "{ queryApp { clientId } queryNote { text } }"))
        print("  add a plugin for the app web:        %s" % gql(c, acme,
              'mutation { addPlugin(input: [{label: "acme web", hasApp: {clientId: "web"}}]) { numUids plugin { label hasApp { clientId title } } } }'))
        print("  read the plugin with its app:        %s" % gql(c, acme, "{ queryPlugin { label hasApp { clientId title } } }"))
        print("  its plugins of the app web only:     %s" % gql(c, acme,
              '{ queryPlugin { label hasApp(filter: {clientId: {eq: "web"}}) { title } } }'))
        print("  its plugins of the app seek only:    %s" % gql(c, acme,
              '{ queryPlugin { label hasApp(filter: {clientId: {eq: "seek"}}) { title } } }'))
        r = c.post("/query", "{ q(func: type(Plugin)) { Plugin.label Plugin.hasApp @template { App.title } } }",
                   ctype="application/dql", headers=R.auth(acme))
        print("  the same reach by hand-written DQL:  %s" % ((r.get("errors") or [{}])[0].get("message") or json.dumps(r.get("data")))[:120])
        r = c.post("/query", "{ q(func: type(Plugin)) { Plugin.label Plugin.hasApp { App.title } } }",
                   ctype="application/dql", headers=R.auth(acme))
        print("  plain DQL through the edge:          %s" % json.dumps(r.get("data"))[:120])
        print("  add a plugin for an app nobody made: %s" % gql(c, acme,
              'mutation { addPlugin(input: [{label: "acme ghost", hasApp: {clientId: "ghost"}}]) { numUids } }'))
        print("  add a plugin with a new app inside:  %s" % gql(c, acme,
              'mutation { addPlugin(input: [{label: "acme own", hasApp: {clientId: "own", title: "Our own"}}]) { numUids } }'))
        print("  its plugins:                         %s" % gql(c, acme, "{ queryPlugin { label } }"))
        print("  change the title of the app seek:    %s" % gql(c, acme,
              'mutation { updateApp(input: {filter: {clientId: {eq: "seek"}}, set: {title: "changed from acme"}}) { numUids } }'))
        print("  add its Workspace record, as acme:   %s" % gql(c, acme, 'mutation { addWorkspace(input: [{name: "acme", title: "Acme"}]) { numUids } }'))
        print("  add a Workspace under another name:  %s" % gql(c, acme, 'mutation { addWorkspace(input: [{name: "globex"}]) { numUids } }'))
        print("  rename its Workspace record:         %s" % gql(c, acme,
              'mutation { updateWorkspace(input: {filter: {name: {eq: "acme"}}, set: {name: "acme2"}}) { numUids } }'))
        print("  change its Workspace's title:        %s" % gql(c, acme,
              'mutation { updateWorkspace(input: {filter: {name: {eq: "acme"}}, set: {title: "Acme Ltd"}}) { numUids } }'))
        print("  its Workspace records:               %s" % gql(c, acme, "{ queryWorkspace { name title } }"))
        print("\nApp records in the root namespace:     %s" % apps_in(c, None))
        print("App records in acme's partition:       %s" % apps_in(c, acme))


if __name__ == "__main__":
    main()
