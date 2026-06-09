# @cascadeAuth Directive

Propagates `@auth` rules from a parent type to its linked child nodes. Access to a child resource is
gated by authorization on the parent that owns it — all enforced at schema compile-time with zero
per-request overhead.

---

## Directives

### `@cascadeAuth` — on the edge field

```graphql
directive @cascadeAuth(
  operations: [CascadeAuthOperation!] # default: [query, add, update, delete]
  depth: Int # default: 1; -1 = unlimited chain
  bidirectional: Boolean # default: false
  variableContext: CascadeAuthVariableContext # default: adaptive
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

The authority may be a **concrete type** or an **interface**. When pointing to an interface, the
engine collects `@auth` rules from all concrete implementors and OR-merges them, adding a
`@filter(type(X))` discriminator per implementor so the DQL filter is type-safe.

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

---

## Migration from Previous API

The following arguments were **removed** from `@cascadeAuth`:

| Removed argument | Was on               | Replacement                                                                                                                                 |
| ---------------- | -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `authMode`       | `@cascadeAuth`       | No replacement — cascade auth always uses filter mode (silent exclusion)                                                                    |
| `when`           | `@cascadeAuth`       | No replacement — use `operations: [...]` to restrict which ops are protected                                                                |
| `includeSelf`    | `@cascadeAuthPolicy` | No replacement — dropped entirely. To mix cascade + self auth, give the type its own `@auth` rule and use `aggregation: "or"` on the policy |
