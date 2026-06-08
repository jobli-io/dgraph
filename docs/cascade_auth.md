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
  self # A's vars for the full chain
  parent # immediate authority's (B's) vars, frozen for full chain
  adaptive # A's vars if present, else nearest ancestor's
  propagate # nearest ancestor's vars starting from B, frozen for full chain
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

Declares named substitution values injected into the authority's `@auth` rule template at **schema
compile-time**. Keys are referenced in the rule as `{{KEY}}`.

> **Substitution happens before GraphQL parsing.** `{{KEY}}` is not valid GraphQL syntax — it must
> be replaced before the rule string is fed to the validator. Rules without `@authVariables` on
> their type will fail to parse if they contain unresolved `{{KEY}}` placeholders.

> **Values are substituted verbatim** (no automatic quoting). For enum filter fields this produces
> the correct unquoted form, e.g. `{{PERMISSIONS}}` → `[_ALL, READ_WORKSPACE]`. For string-typed
> fields, include the quotes inside the value: `value: ["\"mystring\""]`.

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

Controls which type's `@authVariables` are substituted into the authority's rule template across a
cascade chain `A → B → C` (A's authority is B, B's authority is C):

| Value                    | B's rule uses                                          | C's rule uses                     | Schema error if…                                |
| ------------------------ | ------------------------------------------------------ | --------------------------------- | ----------------------------------------------- |
| `"self"`                 | A's vars                                               | A's vars                          | A has no `@authVariables`                       |
| `"parent"`               | B's vars (frozen for whole chain)                      | B's vars (frozen for whole chain) | B has no `@authVariables`                       |
| `"adaptive"` _(default)_ | A's vars (if A has them) else nearest ancestor's       | same                              | Neither A nor any ancestor has `@authVariables` |
| `"propagate"`            | C's vars (nearest ancestor with vars, starting from B) | C's vars (same donor, frozen)     | No ancestor has `@authVariables`                |

**`self`** — the outermost queried type's vars propagate through the entire chain. Every
intermediate type in the chain must also have `@authVariables` (schema error if missing at any hop).

**`parent`** — the immediate authority's (B's) vars are substituted into every rule in the chain.
`outerChildTypeName` is advanced to B at depth=0, so all deeper hops use B's vars too. Requires B to
declare `@authVariables`; schema error if missing.

**`adaptive`** _(default)_ — tries A's vars first; if absent, falls back to each hop's nearest
ancestor with vars. At least one type in the chain must have `@authVariables`; schema error if none.

**`propagate`** — walks from B upward to find the first ancestor with `@authVariables`, then
_freezes_ those vars for the entire chain. Use this when the authority type (B) may not always have
vars but a grandparent (C) always does. Requires at least one ancestor to have `@authVariables`;
schema error if none found.

```graphql
# Workspace's @auth template uses {{PERMISSIONS}}
# Candidate provides its own PERMISSIONS via @authVariables
interface WorkspaceMember {
  inWorkspace: Workspace! @cascadeAuth(variableContext: self)
}

type Workspace
  @authVariables(vars: [
    { key: "PERMISSIONS", value: ["_ALL", "READ_WORKSPACE", "_WORKSPACE"] }
  ])
  @auth(query: { rule: """
    query($sub: String!, $azp: String!) {
      queryWorkspace(filter: {
        hasIAMBinding: {
          or: [
            { forUserMember: { userId: { eq: $sub } } }
            { forAppMember:  { clientId: { eq: $azp } } }
          ]
          forRole: { permission: { in: {{PERMISSIONS}} } }
        }
      }) { __typename }
    }
  """ })

type Candidate
  @authVariables(vars: [
    { key: "PERMISSIONS", value: ["_ALL", "READ", "_CANDIDATE", "READ_CANDIDATE"] }
  ]) { ... }
```

At schema load, `expandCascadeAuth()` generates a rule for `Candidate` using Workspace's template
with **Candidate's** `PERMISSIONS` substituted — identical to the hand-written `@auth` rule, but
automatic.

### Chained `variableContext` — three-hop example `A → B → C`

For a chain where A has `variableContext: parent` on its A→B edge and B has
`variableContext: parent` on its B→C edge:

```
depth=0 (A→B, parent): outerChildTypeName advances to B → B's vars for B's rule
depth=1 (B→C, parent): outerChildTypeName advances to C → C's vars for C's rule

Result: B's rule uses B's vars ✓   C's rule uses C's vars ✓
```

Each `parent` hop advances the substitution anchor to its own immediate authority. They compose
naturally — no conflict.

For `propagate` when B has no vars:

```
depth=0 (A→B, propagate): walk up from B → no vars → walk to C → C has vars → freeze C
  B's rule: C's vars substituted in
  C's rule: C's vars substituted in
```

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

**Graceful fallback:** If the authority's op-specific rule uses `{{KEY}}` placeholders that the
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

| Violation                                                                                          | Error                                              |
| -------------------------------------------------------------------------------------------------- | -------------------------------------------------- |
| Used on a scalar or enum field                                                                     | `@cascadeAuth can only be used on edge fields`     |
| Used on a `@remote` type                                                                           | `@cascadeAuth cannot be used on a @remote type`    |
| Target type has no `@auth` rules and is not an interface with implementing types that have `@auth` | `@cascadeAuth: target type "T" has no @auth rules` |
| `depth` not ≥ 1 or -1                                                                              | `@cascadeAuth: depth must be ≥ 1 or -1`            |
| `variableContext` not one of `"self"`, `"parent"`, `"adaptive"`, `"propagate"`                     | Validation error                                   |
| `variableContext: "self"` and child has no `@authVariables`                                        | Schema load error                                  |
| `variableContext: "parent"` and immediate authority has no `@authVariables`                        | Schema load error                                  |
| `variableContext: "adaptive"` and neither child nor any ancestor has `@authVariables`              | Schema load error                                  |
| `variableContext: "propagate"` and no ancestor in the chain has `@authVariables`                   | Schema load error                                  |
| `{{KEY}}` referenced in template has no matching entry in `@authVariables`                         | Unresolved placeholder error at schema load        |
| Circular cascade chain (A → B → A)                                                                 | `@cascadeAuth forms a cycle through type B`        |

---

## Migration from Previous API

The following arguments were **removed** from `@cascadeAuth`:

| Removed argument | Was on               | Replacement                                                                                                                                 |
| ---------------- | -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `authMode`       | `@cascadeAuth`       | No replacement — cascade auth always uses filter mode (silent exclusion)                                                                    |
| `when`           | `@cascadeAuth`       | No replacement — use `operations: [...]` to restrict which ops are protected                                                                |
| `includeSelf`    | `@cascadeAuthPolicy` | No replacement — dropped entirely. To mix cascade + self auth, give the type its own `@auth` rule and use `aggregation: "or"` on the policy |
