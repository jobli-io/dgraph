# @cascadeAuth Directive

Propagates `@auth` rules from a parent type to its linked child nodes. Access to a child resource is
gated by authorization on the parent that owns it — all enforced at schema compile-time with zero
per-request overhead.

---

## Directives

### `@auth` — interface-level extension

The `@auth` directive gains one new argument for interface-to-concrete-type rule propagation:

```graphql
directive @auth(
  query: AuthRule
  add: AuthRule
  update: AuthRule
  delete: AuthRule
  mergeAfterCascade: Boolean # default: false; only meaningful on interfaces
) on OBJECT | INTERFACE
```

See
[mergeAfterCascade — Post-Cascade Interface Auth](#mergeaftercascade--post-cascade-interface-auth)
for full documentation.

---

### `@cascadeAuth` — on the edge field

```graphql
directive @cascadeAuth(
  operations: [CascadeAuthOperation!] # default: [query, add, update, delete]
  depth: Int # default: 1; -1 = unlimited chain
  bidirectional: Boolean # default: false
  variableContext: CascadeAuthVariableContext # default: adaptive
  interfaceOnly: Boolean # default: false; see §interfaceOnly below
) on FIELD_DEFINITION

enum CascadeAuthOperation {
  query
  add
  update
  delete
}
enum CascadeAuthVariableContext {
  self # child re-substitutes its own vars into authority's template
  parent # child inherits authority's compiled rule as-is (no re-substitution)
  adaptive # try self first; if child has no vars, fall back to parent's compiled rule
}
```

Place on **edge fields** that point to an authority type. The authority type must declare `@auth`
rules (or be an interface whose concrete implementors do).

The authority may be a **concrete type** or an **interface**. When pointing to an interface the
default behaviour (`interfaceOnly: false`) collects `@auth` rules from every concrete implementor
and OR-merges them, adding a `@filter(type(X))` discriminator per implementor so each DQL branch is
type-safe. When the interface has its own self-contained `@auth` rule you can skip that expansion
with `interfaceOnly: true` — see the [`interfaceOnly`](#interfaceonly) section below.

### `@authVariables` — on the child type or interface

```graphql
input AuthVariable {
  key: String!
  value: [String!]!
}

directive @authVariables(vars: [AuthVariable!]!) on OBJECT | INTERFACE
```

Declares named substitution **key-value pairs** injected into the authority's `@auth` rule template
at **schema compile-time**. Keys are referenced in the rule as `<<KEY>>`.

> **Substitution happens before GraphQL parsing.** `<<KEY>>` is not valid GraphQL syntax — it must
> be replaced before the rule string is fed to the validator. Rules without `@authVariables` on
> their type will fail to parse if they contain unresolved `<<KEY>>` placeholders.

> **Values are substituted verbatim** (no automatic quoting). For enum filter fields this produces
> the correct unquoted form, e.g. `<<PERMISSIONS>>` → `[_ALL, READ_WORKSPACE]`. For string-typed
> fields, include the quotes inside the value: `value: ["\"mystring\""]`.

> **Misspelled keys are a hard schema error.** If a placeholder key does not match any entry in
> `@authVariables` after both substitution passes, schema load is rejected with a clear error
> message listing the unresolved key name. This prevents silent no-op rules.

#### Template Engine

The substitution engine is Go's `text/template` with `<<` / `>>` as delimiters. This unlocks
**transform pipelines** using the `|` operator:

| Transform   | Input                      | Output                     | Use case                             |
| ----------- | -------------------------- | -------------------------- | ------------------------------------ |
| `toStrings` | `[_ALL, _EMAIL, READ]`     | `["_ALL","_EMAIL","READ"]` | Enum list → quoted JSON string array |
| `lower`     | `["_ALL","_EMAIL","READ"]` | `["_all","_email","read"]` | Lowercase (any string or JSON array) |

**Pipeline example — RBAC `in` rule from an enum list:**

```graphql
# @authVariables declares an enum-format list (for use in GQL filters):
@authVariables(vars: [
  { key: "QRY_PERMISSIONS", value: ["_ALL", "_EMAIL", "READ", "READ_EMAIL"] }
])

# Auth rule — GQL filter (enum values, no quotes needed):
@auth(query: { rule: """
  query($sub: String!) {
    queryJobAd(filter: {
      hasIAMBinding: { forRole: { permission: { in: <<QRY_PERMISSIONS>> } } }
    }) { __typename }
  }
""" })
```

For RBAC scope rules, the same list must be lowercase JSON strings. Use `toStrings | lower`:

```graphql
# RBAC rule — $scope is a JWT string claim, must match lowercase quoted values:
@auth(query: { rule: "{ $scope: { in: <<QRY_PERMISSIONS | toStrings | lower>> } }" })
```

If the values are already lowercase JSON strings (no conversion needed), declare them directly:

```graphql
@authVariables(vars: [
  { key: "QRY_SCOPES", value: ["_all", "read"] }
])
@auth(query: { rule: "{ $scope: { in: <<QRY_SCOPES>> } }" })
```

**Built-in `<<TYPE>>` placeholder:**

When used inside `resolveAuthVariables` (cascade expansion), `<<TYPE>>` is replaced by the concrete
type name. This allows interface auth rules to reference the implementing type's query root without
hard-coding it.

**Delimiter isolation:** `<<` / `>>` delimiters do not conflict with:

- GraphQL syntax (`{`, `}`, `$var`, `"""`), or
- RBAC rule JSON (`{ $scope: { in: [...] } }`), or
- Go template `{{.error}}` strings used in `@validate` reason fields (those use `{{` / `}}`)

### `@cascadeAuthPolicy` — on the child type

```graphql
directive @cascadeAuthPolicy(
  aggregation: String # "and" (default) | "or"
  skipBidirectional: Boolean # default: false
  skip: Boolean # default: false
) on OBJECT | INTERFACE
```

Controls how multiple incoming cascade edges are combined and whether this type participates in
cascade auth at all.

| Argument                  | Effect                                                                                                                                      |
| ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `aggregation: "or"`       | Access via **any** authorized parent path is sufficient (default: `"and"` — all parent paths must be satisfied simultaneously)              |
| `skipBidirectional: true` | This type will **not** contribute reverse-visibility rules back to its authority type (see [Bidirectional Pitfall](#bidirectional-pitfall)) |
| `skip: true`              | Completely opt this type out of cascade auth expansion — it receives no auth rules from authority types                                     |

---

## Arguments

### `variableContext`

Controls how the child relates to the authority's `@auth` rule across a cascade chain `A → B → C`
(A's authority is B, B's authority is C). The `@auth` rule on the authority type may be a
**self-contained literal rule** (no `<<KEY>>` placeholders) or a **template** (uses `<<KEY>>`
substitution values declared by `@authVariables`).

| Value                    | What the child receives                                                                                 | `@authVariables` required on child                    |
| ------------------------ | ------------------------------------------------------------------------------------------------------- | ----------------------------------------------------- |
| `"self"`                 | Authority's template re-substituted with **child's own** key-value pairs from `@authVariables`          | **Yes** — schema error if missing                     |
| `"parent"`               | Authority's **compiled rule** (type `@auth` + cascade auth) inherited as-is. No re-substitution         | **No**                                                |
| `"adaptive"` _(default)_ | **Self** if child has `@authVariables`; otherwise **parent** fallback (authority's compiled rule as-is) | No (but at least one path must work — see Validation) |

---

#### `"self"` — child provides all substitution values

The child substitutes **its own `@authVariables` key-value pairs** into the authority's raw `@auth`
template. All `<<KEY>>` placeholders required by the template must be declared in the child's
`@authVariables`. Schema error if the child has no `@authVariables`.

In a chain `A → B → C` where both edges use `variableContext: self`, A's vars are used for **both**
B's rule and C's rule — the outermost queried type's vars propagate through the whole chain.

```graphql
# Workspace @auth template uses <<PERMISSIONS>>
# Candidate declares its own PERMISSIONS via @authVariables
type Candidate
  @authVariables(vars: [
    { key: "PERMISSIONS", value: ["_ALL", "READ", "_CANDIDATE", "READ_CANDIDATE"] }
  ]) { ... }

interface CandidateMember {
  forCandidate: Candidate @cascadeAuth(variableContext: self)
}
```

At schema load, `expandCascadeAuth()` generates a rule for the child using Workspace's template with
**Candidate's** `PERMISSIONS` substituted.

---

#### `"parent"` — child inherits authority's compiled rule

The child takes the authority's **compiled rule** as-is — no re-substitution from the child's side.
The compiled rule is the combination of the authority's own `@auth` (compiled) AND its cascade chain
protection.

- **Authority has a self-contained rule** (`@auth` with no `<<KEY>>` placeholders): passed through
  unchanged.
- **Authority has a template `@auth`** (`<<KEY>>` present): the authority compiles its own rule via
  its own cascade chain (when the authority is itself a cascade child). The child simply inherits
  the result. If the authority has a template but no cascade chain to compile it, schema validation
  rejects the configuration.
- **No `@authVariables` check on the authority**: the child never provides or checks vars for
  `parent`. The authority's compilation is its own concern.

```graphql
# Group has its own @auth (self-contained literal).
# Company inherits Group's fully-compiled rule (type @auth + Group's cascade to Workspace).
type Company implements GroupMember { ... }

interface GroupMember {
  inGroup: Group @cascadeAuth(variableContext: parent)
}
```

---

#### `"adaptive"` _(default)_ — try self, fall back to parent

1. **Self first**: if the child has `@authVariables`, re-substitute the child's key-value pairs into
   the authority's template (same as `self`).
2. **Parent fallback**: if the child has no `@authVariables`, inherit the authority's compiled rule
   as-is (same as `parent`).

Schema error only when **both paths are blocked**: child has no `@authVariables` AND the authority
has a `<<KEY>>` template with no cascade chain to compile it (uncompilable, zero protection).

```graphql
# All three work with adaptive:
#   1. Child has @authVariables → self path
#   2. Authority has self-contained rule → parent fallback (pass through)
#   3. Authority is cascade-protected → parent fallback (inherits cascade protection)

interface WorkspaceMember {
  inWorkspace: Workspace! @cascadeAuth()  # variableContext: adaptive (default)
}
```

---

### Chained `variableContext` — three-hop example `A → B → C`

Each edge in the chain uses its own declared `variableContext`. They compose naturally:

```
A → B (variableContext: self):   B's rule = re-substituted with A's @authVariables
B → C (variableContext: parent): C's rule = C's compiled rule inherited as-is
```

For `parent`, the substitution anchor advances to the authority at each hop:

```
A → B (variableContext: parent): B's compiled rule (B's @auth + B's cascade to C)
B → C (variableContext: parent): C's compiled rule (C's @auth + C's own chain)
```

Each hop resolves independently. There is no global "frozen anchor" for `parent` — the authority at
each hop compiles its own rule.

---

### `operations`

Controls which mutation operations the cascade auth rule is applied to. Omitting `operations`
applies cascade auth to **all four operations** (query, add, update, delete).

```graphql
# Only protect reads — add/update/delete bypass cascade auth on this edge
forJobAd: JobAd @cascadeAuth(operations: [query], variableContext: self, bidirectional: true)

# Protect reads and writes but not deletes
inWorkspace: Workspace! @cascadeAuth(operations: [query, add, update])
```

For each operation that IS in the list, the engine reads the authority type's matching `@auth` rule:

| Child operation | Authority rule used  | Fallback            |
| --------------- | -------------------- | ------------------- |
| `query`         | `@auth(query: ...)`  | —                   |
| `add`           | `@auth(add: ...)`    | `@auth(query: ...)` |
| `update`        | `@auth(update: ...)` | `@auth(query: ...)` |
| `delete`        | `@auth(delete: ...)` | `@auth(query: ...)` |

**Graceful fallback:** If the authority's op-specific rule uses `<<KEY>>` placeholders that the
child's `@authVariables` doesn't declare, the engine falls back to the `query` rule for that
operation. The child remains protected and the schema deploys without error.

---

### `interfaceOnly`

Applies only when the edge's authority type is an **interface**.

| Value               | Behaviour                                                                                                                                                                              |
| ------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `false` _(default)_ | Expand to every concrete implementor. Each implementor gets its own CascadeWrap branch with a `type(ConcreteType)` discriminator. Correct when implementors have different auth rules. |
| `true`              | Use the interface's own `@auth` rule directly. Emits a single `var(func: type(InterfaceName))` block. No per-implementor expansion.                                                    |

In Dgraph, `func: type(InterfaceName)` matches every implementing node because interface names are
stored in `dgraph.type` alongside the concrete type name — so the filter is still correct.

**When to use `interfaceOnly: true`:**

- The interface has a self-contained `@auth` rule that already covers all its implementors.
- You want to avoid the O(implementors) CascadeWrap branches that inflate query size.
- The interface's rule uses `<<KEY>>` templates that the child type provides via `@authVariables`
  (works with any `variableContext` mode).

**DQL comparison —
`forResource: NoteOwner @cascadeAuth(operations: [query], variableContext: adaptive)`:**

```dql
# interfaceOnly: false (default) — one branch per implementor
Attachment_Auth as var(func: uid(AttachmentRoot)) @filter(
  uid_in(Attachment.forResource, uid(Candidate_Auth)) OR
  uid_in(Attachment.forResource, uid(Contact_Auth)) OR
  uid_in(Attachment.forResource, uid(JobAd_Auth))
) @cascade
Candidate_Auth as var(func: type(Candidate)) @filter(...)
Contact_Auth   as var(func: type(Contact))   @filter(...)
JobAd_Auth     as var(func: type(JobAd))     @filter(...)
```

```dql
# interfaceOnly: true — one block regardless of implementor count
Attachment_Auth  as var(func: uid(AttachmentRoot))
  @filter(uid_in(Attachment.forResource, uid(NoteOwner_Auth))) @cascade
NoteOwner_Auth   as var(func: type(NoteOwner)) @filter(... interface's own @auth rule ...)
```

**Example:**

```graphql
type Attachment
  @authVariables(
    vars: [{ key: "QRY_PERMISSIONS", value: ["_ALL", "READ", "_ATTACHMENT", "READ_ATTACHMENT"] }]
  ) {
  # Default: expands to type(Candidate), type(Contact), type(JobAd) …
  forResource: NoteOwner @cascadeAuth(operations: [query], variableContext: adaptive)

  # Opt-in: single type(NoteOwner) block — requires NoteOwner to have its own @auth
  forResource: NoteOwner
    @cascadeAuth(operations: [query], variableContext: adaptive, interfaceOnly: true)
}
```

> **Requirement:** the interface must have a `@auth` rule for the targeted operation. If it has
> none, Case 2 produces no restriction — every node of `type(InterfaceName)` passes — which may be
> intentional (world-readable authority) or a misconfiguration. The engine will not error; review
> your intent carefully before enabling this on write operations.

---

### `depth`

| Value           | Effect                                        |
| --------------- | --------------------------------------------- |
| `1` _(default)_ | Direct authority only                         |
| `N > 1`         | Walk up to N hops through the cascade chain   |
| `-1`            | Full chain until no more `@cascadeAuth` edges |

---

### `bidirectional`

When `true`, also generates **reverse** rules so that the authority type becomes visible when the
caller can access any child:

1. **Reverse visibility** — OR-merges a rule into the authority's `@auth(query:...)` so the
   authority is visible when the caller can reach any child.
2. **Edge scoping** — Adds a field-level auth rule on the inverse edge so traversing from authority
   to child only returns children the caller is authorized to see.

The reverse direction always uses the child's own **pre-cascade** `@auth` rule (before cascade
expansion runs). It only generates `query` rules — it **never** grants add/update/delete access to
the authority.

> **Note:** The rule fed back to the authority is the child's _own_ `@auth`, not the
> cascade-substituted form. This prevents circular dependencies.

---

## `@cascadeAuthPolicy` in Depth

### `aggregation`

When a type has **multiple** incoming `@cascadeAuth` edges (implements multiple cascade-auth
interfaces), `aggregation` controls how those paths are combined:

```graphql
# Company reachable from both Workspace (inWorkspace) and Group (inGroup)
type Company
  @cascadeAuthPolicy(aggregation: "or")   # either path suffices
  @authVariables(vars: [{ key: "PERMISSIONS", value: ["READ_COMPANY"] }])
  { ... }
```

| Value               | Behaviour                                                         |
| ------------------- | ----------------------------------------------------------------- |
| `"and"` _(default)_ | Caller must be authorized via **all** parent paths simultaneously |
| `"or"`              | Caller needs authorization via **any** one path                   |

> **Common pitfall:** A type that implements two cascade-auth interfaces but is not always linked to
> both authority types (e.g. optional group membership) must use `aggregation: "or"`. With the
> default `"and"`, a node that has only `inWorkspace` set (no `inGroup`) will always fail auth
> because the engine requires both paths to be satisfied.

> **Interaction with `mergeAfterCascade`:** When an interface carries `mergeAfterCascade: true`, its
> auth rules are AND-merged into the concrete type's final rule **after** cascade expansion (Stage
> 4). For `aggregation: "or"` types this means the interface check wraps the entire OR expression —
> every access path (direct `@auth` _and_ each cascade branch) must satisfy the interface
> restriction. For `aggregation: "and"` types the result is semantically equivalent to Stage 2
> merging but is applied later for consistency. See
> [mergeAfterCascade — Post-Cascade Interface Auth](#mergeaftercascade--post-cascade-interface-auth).

### `skip`

```graphql
type AuditLog implements WorkspaceMember @cascadeAuthPolicy(skip: true) {
  id: ID!
  inWorkspace: Workspace!
}
```

Completely opts this type out of cascade auth expansion. The type receives **no** propagated auth
rules from any authority type, even if it implements cascade-auth interfaces. Use when:

- The type is a structural-only member of an interface (audit logs, through-nodes, internal records)
- Auth is intentionally handled entirely by the type's own explicit `@auth` directive
- Adding cascade auth to this type would produce incorrect DQL (e.g. authority's filter does not
  apply to this type's use case)

### `skipBidirectional`

Prevents this type from contributing reverse-visibility rules back to its authority. See
[Bidirectional Pitfall](#bidirectional-pitfall) below.

---

## Cascading Chains

```graphql
# Workspace → Group → Candidate chain
interface WorkspaceMember {
  inWorkspace: Workspace! @cascadeAuth(depth: -1, variableContext: self)
}

interface Groupable {
  inGroup: [Group!]! @cascadeAuth(variableContext: self)
}

type Workspace
  @authVariables(vars: [{ key: "PERMISSIONS", value: ["_ALL", "READ_WORKSPACE"] }])
  @auth(query: { rule: "..." })

type Group
  @authVariables(vars: [{ key: "PERMISSIONS", value: ["_ALL", "READ_GROUP"] }])
  @auth(query: { rule: "..." })

type Candidate
  @authVariables(vars: [{ key: "PERMISSIONS", value: ["_ALL", "READ_CANDIDATE", "_CANDIDATE"] }])
  @cascadeAuthPolicy(aggregation: "or")
  { ... }
```

With `depth: -1`, the engine recursively builds the full auth chain:

```dql
Workspace_var as var(func: type(Workspace)) @filter(workspaceAuth) @cascade
Group_var     as var(func: type(Group))     @filter(groupAuth AND uid_in(inWorkspace, uid(Workspace_var))) @cascade
Candidate     @filter(uid_in(inGroup, uid(Group_var)))
```

---

## Multiple Parent Paths (Diamond Pattern)

When a type is reachable via multiple authority types, use `aggregation: "or"` to require only one
path:

```graphql
type AdPostRecord
  @cascadeAuthPolicy(aggregation: "or", skipBidirectional: true)
  @authVariables(vars: [{ key: "PERMISSIONS", value: ["_ALL", "READ"] }]) {
  forJobAd: JobAd @cascadeAuth(operations: [query], variableContext: self, bidirectional: true)
  forJobBoard: JobBoard
    @cascadeAuth(operations: [query], variableContext: self, bidirectional: true)
}
```

The caller can see the record if they can access either the job ad OR the job board. With default
`"and"`, they would need access to both simultaneously.

---

## `mergeAfterCascade` — Post-Cascade Interface Auth

### The Ordering Problem

Cascade auth expansion runs in four stages:

| Stage | What happens                                                                     |
| ----- | -------------------------------------------------------------------------------- |
| 1     | Each type compiles its own `@auth` rules                                         |
| 2     | Interface `mergeInto` rules are AND-merged into concrete types                   |
| 3     | Cascade auth blocks are generated and appended (OR or AND per `aggregation`)     |
| 4     | **`mergeAfterCascade` interface rules are AND-merged into the final expression** |

The `mergeInto` mechanism (Stage 2) works correctly for `aggregation: "and"` types, but produces a
logically incorrect result for `aggregation: "or"` types.

For an OR-policy type with a Stage-2 interface merge the algebra is:

```
# Stage 2 merge then Stage 3 OR-cascade:
OR(
  AND(base_auth, ifaceAuth),   ← direct auth path: correctly restricted
  cascadeBlock                 ← cascade path: interface check ESCAPES
)
```

The cascade branch is outside the `AND`, so a caller granted access via the cascade path bypasses
the interface restriction entirely.

### How `mergeAfterCascade: true` Fixes It (Stage 4)

By deferring the interface merge to Stage 4 — after cascade expansion — the interface rule wraps the
**complete** OR expression:

```
# Stage 3 OR-cascade then Stage 4 AND-merge:
AND(
  OR(base_auth, cascadeBlock),
  ifaceAuth
)

# Which distributes to:
OR(
  AND(base_auth,   ifaceAuth),   ← direct auth path: restricted ✓
  AND(cascadeBlock, ifaceAuth)   ← cascade path: restricted ✓
)
```

For `aggregation: "and"` types the result is equally correct — the interface constraint is just
appended to the AND chain: `AND(base_auth, cascadeBlock, ifaceAuth)`.

> **The merge operator is always AND.** There is no `interfacePolicy` or per-operation override for
> `mergeAfterCascade`. The interface acts as a universal access restriction regardless of the
> concrete type's `@cascadeAuthPolicy`. All four operations (query, add, update, delete) are
> affected equally; the appropriate `@auth` field per operation is merged in.

### Schema Example — The `Manageable` Interface

A common real-world pattern is a plugin-ownership check that must restrict _every_ access path, not
only the direct `@auth` path:

```graphql
# Interface: every Manageable resource must be owned by the requesting plugin.
interface Manageable
  @auth(
    mergeAfterCascade: true
    query: {
      rule: """
      query($pluginId: String!) {
        queryManageable(filter: { managedBy: { eq: $pluginId } }) { __typename }
      }
      """
    }
  ) {
  managedBy: String!
}

# CompanyStatus is also a WorkspaceMember → accessible via workspace cascade.
# aggregation: "or" — direct auth OR workspace cascade is sufficient.
type CompanyStatus implements Manageable & WorkspaceMember
  @cascadeAuthPolicy(aggregation: "or")
  @auth(
    query: {
      rule: """
      query($pluginId: String!) {
        queryCompanyStatus(filter: { managedBy: { eq: $pluginId } }) { __typename }
      }
      """
    }
  ) {
  id: ID!
  managedBy: String!
  inWorkspace: Workspace!
}
```

**Without `mergeAfterCascade`** (Stage 2 merge + `aggregation: "or"`):

```
OR(
  AND(companyStatusAuth, manageableAuth),  ← correct
  workspaceCascade                          ← bypasses manageableAuth ✗
)
```

A system plugin that only has workspace cascade access could read any `CompanyStatus` node
regardless of `managedBy`.

**With `mergeAfterCascade: true`** (Stage 4 AND-merge):

```
AND(
  OR(companyStatusAuth, workspaceCascade),
  manageableAuth
)
= OR(
    AND(companyStatusAuth, manageableAuth),  ← correct ✓
    AND(workspaceCascade,  manageableAuth)   ← correct ✓
  )
```

Every access path now requires the `managedBy` check.

### Key Differences vs. `mergeInto`

> [!IMPORTANT] > `mergeAfterCascade: true` and `mergeInto` (Stage 2) are mutually exclusive in
> intent. Using both on the same interface will apply the interface auth rules **twice** — once in
> Stage 2 (as part of the base auth) and once in Stage 4 (wrapping the whole expression). For
> OR-policy types this produces a more restrictive result than intended. Use **only
> `mergeAfterCascade: true`** when the interface must restrict cascade-accessed nodes.

| Property                    | `mergeInto` (Stage 2)            | `mergeAfterCascade` (Stage 4)            |
| --------------------------- | -------------------------------- | ---------------------------------------- |
| Runs before cascade         | ✓ Yes                            | ✗ No — runs after                        |
| Wraps cascade branches      | ✗ No — cascade branch can escape | ✓ Yes — AND wraps the full OR expression |
| Safe for `aggregation: or`  | ✗ No                             | ✓ Yes                                    |
| Safe for `aggregation: and` | ✓ Yes                            | ✓ Yes                                    |
| Merge operator              | AND                              | AND (always; no override)                |
| Operations affected         | Per `@auth` field                | All four (query/add/update/delete)       |

### Notes and Constraints

- **Self-contained rules only.** No `@authVariables` template substitution is performed in Stage 4.
  The interface's `@auth` rules must be fully compiled literals — `<<KEY>>` placeholders are not
  resolved against concrete type variables at this stage. If the interface rule contains unresolved
  placeholders, schema load will be rejected with an unresolved-key error.

- **All four operations are affected equally.** For each operation the engine picks the interface's
  matching `@auth` field (query/add/update/delete) and AND-merges it into the concrete type's
  compiled rule for that operation. There is no per-operation opt-out.

- **Only on interfaces.** `mergeAfterCascade: true` on a concrete type (`OBJECT`) is a validation
  error — the argument is meaningless on types that have no implementors to merge into.

- **No `interfacePolicy` override.** The merge operator is always AND. The interface acts as an
  unconditional access gate — it cannot be loosened to OR at any granularity.

---

## Bidirectional Pitfall

When `bidirectional: true` is used on an edge, the engine generates a reverse-visibility rule on the
authority using the child's **own pre-cascade** `@auth`. If the child has **no own `@auth`
directive** (only inheriting from an interface), that pre-cascade rule may contain no user-specific
predicate — causing the authority to become visible to any caller in scope.

**Fix:** Apply `@cascadeAuthPolicy(skipBidirectional: true)` to any type that should **not**
contribute reverse rules to its authority:

```graphql
type BillingAccount implements WorkspaceMember
  @generate(query: { get: true, query: true })
  @cascadeAuthPolicy(skipBidirectional: true) {
  id: ID!
}
```

This is the right fix when:

- The type has no own `@auth(query:...)` with a user-specific predicate, **and**
- Its inherited interface auth is intentionally permissive (workspace-scoped but not user-scoped),
  **and**
- Authority access should only be granted via other children with proper user checks.

---

## Validation Rules

| Violation                                                                                                                | Error                                                                    |
| ------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------ |
| Used on a scalar or enum field                                                                                           | `@cascadeAuth can only be used on edge fields`                           |
| Used on a `@remote` type                                                                                                 | `@cascadeAuth cannot be used on a @remote type`                          |
| Target type has no `@auth` rules and is not an interface with implementing types that have `@auth`                       | `@cascadeAuth: target type "T" has no @auth rules`                       |
| `depth` not ≥ 1 or -1                                                                                                    | `@cascadeAuth: depth must be ≥ 1 or -1`                                  |
| `variableContext` not one of `"self"`, `"parent"`, `"adaptive"`                                                          | Validation error                                                         |
| `variableContext: "self"` and child has no `@authVariables`                                                              | Schema load error — child must declare all required key-value pairs      |
| `variableContext: "parent"` or `"adaptive"` (parent fallback) and authority has `<<KEY>>` template with no cascade chain | Schema load error — template is uncompilable, child gets zero protection |
| `<<KEY>>` referenced in template has no matching entry in `@authVariables`                                               | Hard schema rejection — unresolved placeholder error at schema load      |
| Circular cascade chain (A → B → A)                                                                                       | `@cascadeAuth forms a cycle through type B`                              |
| `interfaceOnly: true` on a non-interface authority type                                                                  | Silently ignored — flag has no effect on concrete types                  |

---

## Migration from Previous API

The following arguments were **removed** from `@cascadeAuth`:

| Removed argument | Was on               | Replacement                                                                                                                                 |
| ---------------- | -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `authMode`       | `@cascadeAuth`       | No replacement — cascade auth always uses filter mode (silent exclusion)                                                                    |
| `when`           | `@cascadeAuth`       | No replacement — use `operations: [...]` to restrict which ops are protected                                                                |
| `includeSelf`    | `@cascadeAuthPolicy` | No replacement — dropped entirely. To mix cascade + self auth, give the type its own `@auth` rule and use `aggregation: "or"` on the policy |
