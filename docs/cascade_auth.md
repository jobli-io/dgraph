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
  authMode: String # "filter" (default) | "enforce"
  depth: Int # 1 (default) | -1 (full chain)
  bidirectional: Boolean # also expose parent when child is accessible
  variableContext: CascadeAuthVariableContext # "self" (default) | "parent" | "adaptive"
  when: String # CEL guard; skip when false
) on FIELD_DEFINITION

enum CascadeAuthOperation {
  query
  add
  update
  delete
}
enum CascadeAuthVariableContext {
  self
  parent
  adaptive
}
```

Place on **edge fields** of the parent type, or on a **parent-reference field** of an interface.

The authority (edge target) may be a **concrete type** or an **interface**. When pointing to an
interface, the engine collects `@auth` rules from all concrete implementors and OR-merges them,
adding a `@filter(type(X))` discriminator per implementor so the DQL filter is type-safe.

### `@authVariables` — on the child type or interface

```graphql
input AuthVariable { key: String!; value: [String!]! }

directive @authVariables(
  vars: [AuthVariable!]!
) on OBJECT | INTERFACE
```

Declares named substitution values injected into the parent's `@auth` rule template at **schema
compile-time**. Keys are referenced in the rule as `{{KEY}}`. The built-in `{TYPE}` placeholder is
always substituted with the concrete child type name.

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
  includeSelf: Boolean # default: false
  skipBidirectional: Boolean # default: false
) on OBJECT | INTERFACE
```

Controls how multiple incoming cascade edges are combined and whether this type contributes
reverse-visibility rules to its authority.

| Argument                  | Effect                                                                                                                                 |
| ------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `aggregation: "or"`       | Access via **any** authorized parent path is sufficient (default: `"and"` — all paths required)                                        |
| `includeSelf: true`       | Adds the child's own `@auth` rule as an additional OR path alongside the cascade rules                                                 |
| `skipBidirectional: true` | This type will **not** contribute reverse-visibility rules to its authority type (see [Bidirectional Pitfall](#bidirectional-pitfall)) |

---

## Arguments

### `variableContext`

Controls which type's `@authVariables` are substituted into the parent's rule template:

| Value                    | Behaviour                                                                                                                                                      |
| ------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `"self"`                 | Child type's `@authVariables` — child-specific permissions. **Requires** `@authVariables` on the child; schema load error if missing.                          |
| `"parent"`               | Authority type's own already-substituted rule — used as-is. No `@authVariables` required on the child.                                                         |
| `"adaptive"` _(default)_ | Uses child's `@authVariables` if the child has them (`self` path); silently falls back to authority's rule if not (`parent` path). No schema error either way. |

```graphql
# Workspace's @auth(query:...) template uses {{PERMISSIONS}}
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

---

### Operation-Aware Auth Rules

By default, `@cascadeAuth` reads the authority type's **`query`** `@auth` rule and propagates it
into the child for all operations (add, update, delete, query). When the authority type declares
operation-specific `@auth` rules, the cascade engine now reads the matching rule:

| Child operation | Authority rule used  | Fallback            |
| --------------- | -------------------- | ------------------- |
| `query`         | `@auth(query: ...)`  | —                   |
| `add`           | `@auth(add: ...)`    | `@auth(query: ...)` |
| `update`        | `@auth(update: ...)` | `@auth(query: ...)` |
| `delete`        | `@auth(delete: ...)` | `@auth(query: ...)` |

This means a child type that cascades from `IAMResourceProtected` will use `ADM_PERMISSIONS` for its
`add` cascade and `QRY_PERMISSIONS` for its `query` cascade — matching the intent of the parent's
auth design.

**Graceful fallback:** If the authority's op-specific rule uses `{{KEY}}` placeholders that the
child's `@authVariables` doesn't declare, the engine falls back to the `query` rule for that
cascade. The child remains protected (query-level auth is always the minimum permission set) and the
schema deploys without error. To opt into op-specific cascades, the child must declare matching
`@authVariables` keys.

---

### `authMode`

| Value                  | Behaviour                                                               |
| ---------------------- | ----------------------------------------------------------------------- |
| `"filter"` _(default)_ | Silently exclude child nodes the caller cannot reach                    |
| `"enforce"`            | Return an authorization error if any child node fails the cascade check |

### `depth`

| Value           | Effect                                        |
| --------------- | --------------------------------------------- |
| `1` _(default)_ | Direct parent only                            |
| `N > 1`         | Walk up to N hops                             |
| `-1`            | Full chain until no more `@cascadeAuth` edges |

### `bidirectional`

When `true`, also generates **reverse** rules in addition to the forward cascade:

1. **Reverse visibility** — OR-merges a rule into the parent's `@auth(query:...)` so the parent
   becomes visible when the caller can access any child.
2. **Edge scoping** — Adds a field-level auth rule on the edge so traversing it only returns
   children the caller is authorized to see.

The reverse direction always uses the child's own complete `@auth` rule (evaluated **before** the
cascade expansion runs). It only generates `query` rules — it **never** grants add/update/delete
access to the parent.

> **Note:** The rule fed back to the parent is the child's _own pre-cascade_ `@auth`, not the
> cascade-substituted form. This is intentional — using the cascade-substituted rule would create a
> circular dependency where the parent's auth depends on itself.

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
  @cascadeAuthPolicy(aggregation: "or", includeSelf: false)
  { ... }
```

---

## Multiple Parent Paths (Diamond Pattern)

```graphql
# Company reachable from both Workspace and Group
type Company
  @cascadeAuthPolicy(aggregation: "or")   # either path suffices
  @authVariables(vars: [{ key: "PERMISSIONS", value: ["READ_COMPANY"] }])
  { ... }
```

`aggregation: "and"` (default): caller must be authorized via **all** parent paths.
`aggregation: "or"`: caller needs authorization via **any** one path. `includeSelf: true`: adds the
child's own `@auth` as an additional OR path.

---

## Bidirectional Pitfall

When `bidirectional: true` is used on an edge, the engine generates a reverse-visibility rule on the
authority type using the child's **own pre-cascade** `@auth`. If the child has **no own `@auth`
directive** (only inheriting from an interface), that pre-cascade rule may contain no user-specific
predicate — causing the authority to become visible to any caller in scope.

**Example:** `BillingAccount` implements `WorkspaceMember` (which has
`inWorkspace @cascadeAuth(bidirectional: true)`). If `BillingAccount` has no own `@auth`, its
inherited interface auth only checks the workspace header — no `$sub` check. The bidirectional
engine would then OR-merge a workspace condition with no user check into `Workspace.query`, allowing
any token in scope to see the workspace.

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
- Parent access should only be granted via other children with proper user checks.

---

## Validation Rules

| Violation                                                                                          | Error                                                    |
| -------------------------------------------------------------------------------------------------- | -------------------------------------------------------- |
| Used on a scalar or enum field                                                                     | `@cascadeAuth can only be used on edge fields`           |
| Used on a `@remote` type                                                                           | `@cascadeAuth cannot be used on a @remote type`          |
| Target type has no `@auth` rules and is not an interface with implementing types that have `@auth` | `@cascadeAuth: target type "T" has no @auth rules`       |
| `authMode` not `"filter"` or `"enforce"`                                                           | Validation error                                         |
| `depth` not ≥ 1 or -1                                                                              | `@cascadeAuth: depth must be ≥ 1 or -1`                  |
| `variableContext` not `"self"`, `"parent"`, or `"adaptive"`                                        | Validation error                                         |
| `variableContext: "self"` used but child has no `@authVariables`                                   | Schema load error                                        |
| `{{KEY}}` referenced in template has no matching entry in `@authVariables`                         | Left as literal string — likely a parse error downstream |
| Circular cascade chain (A → B → A)                                                                 | `@cascadeAuth forms a cycle through type B`              |
