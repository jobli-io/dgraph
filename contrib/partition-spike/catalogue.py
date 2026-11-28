"""Placing Jobli's app catalogue in the control plane, as an edit of a schema's text.

`place(schema)` takes Jobli's schema and returns it with IntegrationApp and
IntegrationAppManifest in the control plane (decisions of 29 Nov 2026):

  - the reverse lists go: IntegrationApp.hasCreated / hasUpdated / hasPlugin,
    IntegrationAppManifest.hasInstalledPlugin, Workspace.hasIntegrationApp. The forward
    edges stay: fromSource on every record, Plugin.hasApp, Plugin.installedManifest
  - an app names its owning workspace (ownerWorkspace) in place of an edge to it, and
    is maintained from that workspace by a caller who may: a rule on the record plus
    tenant rules asked of the caller's own workspace
  - records of the control plane have their own photo type, GlobalPhoto, and the
    interface GlobalPhotoOwner for what has one. It is not the catalogue's alone: a
    photo takes its permission from the record it is a photo of
  - the one rule that walked from a workspace's record into the catalogue
    (PluginConnectionAsyncEventStatus) reads the plugin's own clientId

graph/.graphql was edited with this. The probes apply it to the schema snapshot they
run on. `python3 catalogue.py <in> <out>` writes the placed schema of a file.
"""
import sys

OWNED = '''                {
                    rule: """
                    query ($ws: String!) {
                        query%(T)s (filter: { ownerWorkspace: { eq: $ws }%(more)s }) {
                            __typename
                        }
                    }
                    """
                }'''

MAY = '''                #* ... and the caller may maintain apps in that workspace: asked of the caller's own workspace
                {
                    or: [
                        {
                            tenantRule: """
                            query ($sub: String!, $ws: String) {
                                queryWorkspace (filter: {
                                    name: { eq: $ws }
                                    ownedBy: { userId: {eq: $sub} }
                                }) {
                                    __typename
                                }
                            }
                            """
                        }
                        {
                            and: [
                                {
                                    or: [
                                        {
                                            tenantRule: """
                                            query ($azp: String!, $ws: String) {
                                                queryWorkspace (filter: {
                                                    hasIAMBinding: {
                                                        forAppMember: {clientId: { eq: $azp }, and: { deleted: false, or: { not: {has: deleted}}}}
                                                        forRole: {permission: {in: [_ALL] }}
                                                        inWorkspace: {name: {eq: $ws}}
                                                    }
                                                }) {
                                                    __typename
                                                }
                                            }
                                            """
                                        }
                                        {
                                            tenantRule: """
                                            query ($sub: String!, $ws: String) {
                                                queryWorkspace (filter: {
                                                    hasIAMBinding: {
                                                        forUserMember: {userId: {eq: $sub}, status: {eq: ACTIVE}}
                                                        forRole: {permission: {in: [_ALL] }}
                                                        inWorkspace: {name: {eq: $ws}}
                                                    }
                                                }) {
                                                    __typename
                                                }
                                            }
                                            """
                                        }
                                    ]
                                }
                                #* verify token's scope
                                { rule: "{ $scope: { in: [\\"_all\\"] } }" }
                            ]
                        }
                    ]
                }'''


def owner(T, more=""):
    """The rule of a control record a workspace made: it names the caller's workspace,
    and the caller may maintain apps there. more is a further condition on the record."""
    return ('''            and: [
                #* the record names the caller's workspace as its owner ...
''' + OWNED % {"T": T, "more": more} + "\n" + MAY + '''
            ]''')


APP_HEAD = '''type IntegrationApp implements GlobalPhotoOwner
    # The catalogue is one copy for every workspace: its records are in the root namespace.
    # An app made by a workspace names that workspace (ownerWorkspace) and is maintained from it.
    @scope(plane: control)
    @lambdaOnMutate(add: true, update: true, delete: false)
    @generate(
        query: { get: true, query: true, aggregate: false }
        mutation: { add: true, update: true, delete: false }
    ) 
    @postValidate(
        add: {
            expr: """
            let res = callLambda("IntegrationApp.onCreate", {"args": map(nodes, {.after})});
            res?.code == 200 ? true : error(res?.message)
            """,
            reason: "{{.error}}"
        },
        update: {
            expr: """
            let res = callLambda("IntegrationApp.onUpdate", {"args": map(nodes, {.after})});
            res?.code == 200 ? true : error(res?.message)
            """,
            reason: "{{.error}}"
        }
    )
    @auth(
        add: {
            or: [
                {
                    rule: """
                    query ($azp: String!) {
                        queryIntegrationApp (filter: {
                            clientId: { eq: $azp }
                        }) {
                            __typename
                        }
                    }
                    """
                }
                {
OWNER_IN
                }
            ]
        }
        update: {
OWNER
        }
        query: {
OWNER
        }
    ) {
'''

PHOTO = '''"""
Implementable by global types, whose records belong to no workspace, that have a photo.
"""
interface GlobalPhotoOwner
    @scope(plane: control)
    @generate(
        query: { get: false, query: false, aggregate: false }
        mutation: { add: false, update: false, delete: false }
    ) {
    """
    Unique identifier
    """
    id: ID!
    """
    Profile photo
    """
    hasPhoto: GlobalPhoto @cascadeDelete @hasInverse(field: forResource, immutable: true) @bypassAuth
}

"""
Photo of a global record: one that belongs to no workspace, such as an application, or a release's
icon or preview. It is kept with the record, so it is readable wherever the record is.
Who may change it is who may change the record it is a photo of (forResource). A photo that is
of no record, such as a release's icon, is maintained from the workspace it was added from.
"""
type GlobalPhoto implements Publishable
    @scope(plane: control)
    @generate(
        query: { get: true, query: true, aggregate: false }
        mutation: { add: true, update: true, delete: true }
    )
    @cascadeAuthPolicy(aggregation: "or", skipBidirectional: true)
    @lambdaOnMutate(add: true, update: true, delete: true)
    @auth(
        add: {
OWNER
        }
        update: {
OWNER
        }
        delete: {
OWNER
        }
        query: {
OWNER
        }
    ) {
    """
    Unique identifier
    """
    id: ID!
    """
    Secondary resource identifier
    """
    sId: String @id @search(by: [hash])
        @dgraph(pred: "Identifiable.sId") @default(add: {expr: "\\"jobli::globalphoto::\\" + uuid()"})
        @generate(mutation: { add: false, update: false, delete: false })
    """
    Name of the workspace the photo was added from, if it was added from one (read-only).
    """
    ownerWorkspace: String @search(by: [hash])
        @generate(mutation: { add: false, update: false, delete: false })
        @default(add: {expr: "auth.ws != nil && auth.ws != \\"\\" && auth.ws != \\"*\\" ? auth.ws : nil"})
    """
    Name of the file. This field is read-only.
    """
    fileName: String
        @validate(expr: """ auth.azp == "jobli-system" """, reason: "Cannot be mutated by App.")
    """
    Size is the length of the object's content in bytes. This field is read-only.
    """
    fileSize: Int @search
        @validate(expr: """ auth.azp == "jobli-system" """, reason: "Cannot be mutated by App.")
    """
    Policy for direct file uploading. Apply policy to a multipart/form-data request to upload file.
    Newly uploaded file replaces the existing one.
    """
    postPolicy: DocumentPostPolicy @lambda
    """
    Temporary link to securely download the file.
    """
    downloadLink(size: PhotoSize): String @lambda
    """
    Base64 encoded image (read-only)
    """
    imageSmall: String @deprecated(reason: "Use publicLink(size: small) instead.")
    """
    Base64 encoded image (read-only)
    """
    image: String @deprecated(reason: "Use publicLink(size: medium) instead.")
    """
    Hash is the MD5 hash of the media content. This field is read-only
    """
    hash: String
        @validate(expr: """ auth.azp == "jobli-system" """, reason: "Cannot be mutated by App.")
    """
    The record the photo is of. It is given when the photo is added and does not change:
    the photo is then maintained by whoever may maintain that record.
    """
    forResource: GlobalPhotoOwner @search @bypassAuth
        @generate(mutation: { add: true, update: false, delete: false })
        @cascadeAuth(operations: [query, add, update, delete], bidirectional: false, variableContext: parent)
}

'''


def placed(schema):
    return "\ntype IntegrationApp implements GlobalPhotoOwner\n" in schema


def place(schema):
    """The schema with the catalogue placed. A schema already placed is returned as it is."""
    if placed(schema):
        return schema
    s = [schema]

    def sub(old, new):
        if s[0].count(old) != 1:
            raise SystemExit("placing the catalogue: expected once, found %d times: %r" % (s[0].count(old), old[:90]))
        s[0] = s[0].replace(old, new)

    def between(start, end, new):
        a = s[0].index(start)
        b = s[0].index(end, a)
        s[0] = s[0][:a] + new + s[0][b:]

    # the reverse lists on the workspace's side and on records
    sub('''    """
    Reference to developer apps
    """
    hasIntegrationApp: [IntegrationApp] @hasInverse(field: inWorkspace, immutable: true)
''', '')
    sub("    fromSource: IntegrationApp @hasInverse(field: hasCreated) @search @bypassAuth\n",
        "    fromSource: IntegrationApp @search @bypassAuth\n")

    # the one rule that walked from a workspace's record into the catalogue
    sub("queryPluginConnectionAsyncEventStatus(filter: { forPlugin: { installedManifest: { forApp: {clientId: { eq: $azp }}} }}) {",
        "queryPluginConnectionAsyncEventStatus(filter: { forPlugin: { clientId: { eq: $azp } } }) {")

    # the app: placed, with its rules written on it in place of the walk through its workspace
    ob = owner("IntegrationApp")
    head = APP_HEAD.replace("OWNER_IN", "\n".join("        " + l if l else l for l in ob.split("\n"))).replace("OWNER", ob)
    between("type IntegrationApp implements PhotoOwner\n",
            '    """\n    Database resource identifier.\n    Warning: Value can change in event of database migration.\n'
            '    """\n    id: ID!\n    """\n    Application client identifier', head)
    sub('''    """
    Installed app settings per workspace
    """
    hasPlugin: [Plugin] @hasInverse(field: hasApp, immutable: true)
        @generate(mutation: { add: false, update: false, delete: false })
    """
    Resources created by application
    """
    hasCreated: [Recordable] @hasInverse(field: fromSource)
        @generate(mutation: { add: false, update: false, delete: false })
    """
    Resources updated by application
    """
    hasUpdated: [UpdateRecord] @hasInverse(field: fromSource)
        @generate(mutation: { add: false, update: false, delete: false })
''', '')
    between('''    """
    The workspace the app belongs to
    """
    inWorkspace: Workspace @hasInverse(field: hasIntegrationApp)''',
            '''    """
    High-level telemetry summary, time-series metrics, and operation breakdown.''',
            '''    """
    Name of the workspace the app belongs to: the workspace it was made from, which maintains it (read-only).
    An app that registered itself has none.
    """
    ownerWorkspace: String @search(by: [hash])
        @validate(add: {expr:""" auth.ws != nil && auth.ws != "" && auth.ws != "*" """, reason: "An application is made from a workspace. Make sure to set the Jobli-Workspace header to the workspace name."})
        @generate(mutation: { add: false, update: false, delete: false })
        @default(add: {expr: "auth.azp != after.clientId ? auth.ws : nil", evaluationOrder: 10})
''')

    # the manifest
    sub("type IntegrationAppManifest\n", "type IntegrationAppManifest\n    @scope(plane: control)\n")
    sub("    hasPreview: [Photo] @cascadeDelete @bypassAuth\n", "    hasPreview: [GlobalPhoto] @cascadeDelete @bypassAuth\n")
    sub("    hasIcon: Photo @cascadeDelete @bypassAuth\n", "    hasIcon: GlobalPhoto @cascadeDelete @bypassAuth\n")
    sub('''    """
    Active workspace plugin installations using this release version.
    """
    hasInstalledPlugin: [Plugin] @hasInverse(field: installedManifest)
        @generate(mutation: { add: false, update: false, delete: false })
        @cascadeAuth(operations:[query] variableContext: parent, bidirectional: false)
}''', "}")

    # the photo type of the control plane and its owner interface, ahead of the manifest's status enum.
    # A photo that is of a record takes that record's rules; one that is of none (an icon,
    # a preview) keeps to the workspace it was added from.
    anchor = '"""\nRelease lifecycle state of an application manifest version.\n"""\nenum AppReleaseStatus {'
    sub(anchor, PHOTO.replace("OWNER", owner("GlobalPhoto", ", not: { has: forResource }")) + anchor)
    return s[0]


# ---------------------------------------------------------------- the data
#
# What the migration does for the catalogue, on a cluster that holds everything in the
# root namespace and one workspace again in a partition, each record under the ID it
# has in the root. One way. strip_partition goes first, under the schema of today;
# migrate_root after the placed schema is loaded.

CATALOGUE = ("IntegrationApp", "IntegrationAppManifest")
PHOTO_FIELDS = (("Photo.fileName", "fileName"), ("Photo.fileSize", "fileSize"), ("Photo.hash", "hash"))


def _rows(c, q, ns):
    r = c.post("/query", q, ns=ns, ctype="application/dql")
    if r.get("errors"):
        raise SystemExit("catalogue: %s\n%s" % (r["errors"][0].get("message", "")[:300], q))
    return r.get("data") or {}


def _lit(v):
    return '"%s"' % str(v).replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")


def migrate_root(c):
    """In the root namespace: every app names the workspace it has an edge to, and the
    photos of apps and manifests are given the control plane's photo type. Nothing is
    removed: the root namespace still answers under the schema of today.

    Run it with the placed schema loaded: an edge written before its predicate is
    declared is stored as a list, and the schema then refuses to make it single."""
    d = _rows(c, """{
      apps(func: type(IntegrationApp)) {
        uid
        ws: WorkspaceMember.inWorkspace { n: Workspace.name }
        photo: PhotoOwner.hasPhoto { uid sId: Identifiable.sId fileName: Photo.fileName fileSize: Photo.fileSize hash: Photo.hash
                                     imageSmall: Photo.imageSmall image: Photo.image
                                     ws: WorkspaceMember.inWorkspace { n: Workspace.name } }
      }
      manifests(func: type(IntegrationAppManifest)) {
        uid
        app: IntegrationAppManifest.forApp { ws: WorkspaceMember.inWorkspace { n: Workspace.name } }
        icon: IntegrationAppManifest.hasIcon { uid fileName: Photo.fileName fileSize: Photo.fileSize hash: Photo.hash imageSmall: Photo.imageSmall image: Photo.image
                                               ws: WorkspaceMember.inWorkspace { n: Workspace.name } }
        preview: IntegrationAppManifest.hasPreview { uid fileName: Photo.fileName fileSize: Photo.fileSize hash: Photo.hash imageSmall: Photo.imageSmall image: Photo.image
                                                     ws: WorkspaceMember.inWorkspace { n: Workspace.name } }
      }
    }""", 0)

    def one(v):
        return (v[0] if isinstance(v, list) else v) if v else None

    rdf, named, photos = [], 0, 0

    def photo(ph, owner, app=None):
        rdf.append('<%s> <dgraph.type> "GlobalPhoto" .' % ph["uid"])
        rdf.append('<%s> <dgraph.type> "Publishable" .' % ph["uid"])
        for field in ("fileName", "fileSize", "hash", "imageSmall", "image"):
            if ph.get(field) is not None:
                rdf.append("<%s> <GlobalPhoto.%s> %s ." % (ph["uid"], field, _lit(ph[field])))
        ws = (one(ph.get("ws")) or {}).get("n") or owner
        if ws:
            rdf.append("<%s> <GlobalPhoto.ownerWorkspace> %s ." % (ph["uid"], _lit(ws)))
        if app:
            rdf.append("<%s> <GlobalPhoto.forResource> <%s> ." % (ph["uid"], app))
            rdf.append("<%s> <GlobalPhotoOwner.hasPhoto> <%s> ." % (app, ph["uid"]))

    for a in d.get("apps") or []:
        rdf.append('<%s> <dgraph.type> "GlobalPhotoOwner" .' % a["uid"])
        ws = (one(a.get("ws")) or {}).get("n")
        if ws:
            rdf.append("<%s> <IntegrationApp.ownerWorkspace> %s ." % (a["uid"], _lit(ws)))
            named += 1
        ph = one(a.get("photo"))
        if ph:
            photo(ph, ws, a["uid"])
            photos += 1
    for m in d.get("manifests") or []:
        owner = (one((one(m.get("app")) or {}).get("ws")) or {}).get("n")
        for ph in ([one(m.get("icon"))] if m.get("icon") else []) + (m.get("preview") or []):
            photo(ph, owner)
            photos += 1
    if rdf:
        c.mutate("\n".join(rdf), ns=0)
    return {"apps": len(d.get("apps") or []), "apps that name an owner": named,
            "manifests": len(d.get("manifests") or []), "photos given the control plane's type": photos}


def strip_partition(c, ns):
    """In a partition: the copies of the catalogue's records are removed, with their
    photos and the workspace's list of its apps. The edges into the catalogue stay as
    they are; the IDs they hold are the root's."""
    d = _rows(c, """{
      cat as var(func: type(IntegrationApp)) { ph as PhotoOwner.hasPhoto }
      man as var(func: type(IntegrationAppManifest)) { ic as IntegrationAppManifest.hasIcon pv as IntegrationAppManifest.hasPreview }
      apps(func: uid(cat)) { uid }
      manifests(func: uid(man)) { uid }
      photos(func: uid(ph, ic, pv)) { uid }
      lists(func: has(Workspace.hasIntegrationApp)) { uid }
    }""", ns)
    gone = [x["uid"] for k in ("apps", "manifests", "photos") for x in d.get(k) or []]
    rdf = ["<%s> * * ." % u for u in gone]
    # predicates the node's types no longer list once the schema is placed
    for u in (x["uid"] for x in d.get("apps") or []):
        rdf += ["<%s> <%s> * ." % (u, p) for p in (
            "IntegrationApp.hasCreated", "IntegrationApp.hasUpdated", "IntegrationApp.hasPlugin",
            "WorkspaceMember.inWorkspace", "PhotoOwner.hasPhoto", "dgraph.type")]
    for u in (x["uid"] for x in d.get("manifests") or []):
        rdf += ["<%s> <%s> * ." % (u, p) for p in ("IntegrationAppManifest.hasInstalledPlugin", "dgraph.type")]
    rdf += ["<%s> <Workspace.hasIntegrationApp> * ." % x["uid"] for x in d.get("lists") or []]
    for i in range(0, len(rdf), 500):
        c.mutate("\n".join(rdf[i:i + 500]), ns=ns, delete=True)
    left = _rows(c, """{
      apps(func: has(IntegrationApp.clientId)) { count(uid) }
      manifests(func: has(IntegrationAppManifest.version)) { count(uid) }
      created(func: has(IntegrationApp.hasCreated)) { count(uid) }
      edges(func: has(Recordable.fromSource)) { count(uid) }
      plugins(func: has(Plugin.hasApp)) { count(uid) }
    }""", ns)
    return {"removed": {k: len(d.get(k) or []) for k in ("apps", "manifests", "photos")},
            "left in the partition": {k: v[0]["count"] for k, v in left.items()}}


if __name__ == "__main__":
    open(sys.argv[2], "w").write(place(open(sys.argv[1]).read()))
