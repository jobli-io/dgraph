# Interface Auth Merge Policy

Controls how a specific interface's `@auth` rules are combined with a concrete implementing type's
own `@auth` rules at schema compile-time.

> **Status: Implemented.**

---

## Background

When a concrete type implements an interface that has `@auth` rules, those rules are **AND-merged**
into the concrete type unconditionally:

```
ConcreteType.query = ConcreteType.own AND Interface.query
```

This is correct for interfaces that represent _requirements_ (a caller must satisfy both), but
doesn't support interfaces that represent _alternative access paths_ (a caller satisfying either the
interface rule OR the concrete type's own rule should be granted access).

---

## `@authVariables` Substitution Behaviour

When an interface's `@auth` rules are merged into a concrete type the substitution behaviour differs
by merge mode:

| Mode                       | `@authVariables` used in the merged arm                             |
| -------------------------- | ------------------------------------------------------------------- |
| **AND** (default)          | Interface's own `@authVariables` — substituted at parse time        |
| **OR** (`interfacePolicy`) | **Concrete type's** `@authVariables` — re-substituted at merge time |

**OR re-substitution** means the OR arm uses the concrete type's permission keys, not the
interface's placeholder values. This is analogous to `variableContext: "self"` in `@cascadeAuth`. If
the concrete type's `@authVariables` don't cover all of the interface rule's `{{KEY}}` placeholders,
the OR arm falls back to the interface's already-substituted node unchanged.

**Example:**

```graphql
interface IAMProtected
  @authVariables(
    vars: [
      { key: "ADM_PERMISSIONS", value: [] } # empty placeholder
    ]
  )
  @auth(add: { rule: "...permission: { in: {{ADM_PERMISSIONS}} }..." })

type Job implements IAMProtected
  @authVariables(
    vars: [
      { key: "ADM_PERMISSIONS", value: [_ALL, _JOB] } # concrete values
    ]
  )
  @auth(
    interfacePolicy: [{ interface: "IAMProtected", merge: "or" }]
    add: { rule: "...Job's own add rule..." }
  )
```

After merge: `Job.add = Job.own OR IAMProtected.add_with_[_ALL_JOB]`

The OR arm uses `[_ALL _JOB]` (Job's values) — not `[]` (the interface's empty placeholder).

## Directive Reference

```graphql
directive @auth(
  mergeInto: String # on interfaces: "and" | "or" (default: "and")
  interfacePolicy: [InterfaceMergePolicy!] # on concrete types: per-interface override
  password: AuthRule
  query: AuthRule
  add: AuthRule
  update: AuthRule
  delete: AuthRule
) on OBJECT | INTERFACE

enum CascadeAuthOperation {
  query
  add
  update
  delete
}

input InterfaceMergePolicy {
  interface: String! # name of the interface to override
  merge: String! # "and" | "or"
  operations: [CascadeAuthOperation!] # optional subset of operations this policy applies to
  # if absent, applies to all four operations
}
```

> **Syntax note:** `operations` values are unquoted enum identifiers, matching the same syntax as
> `@cascadeAuth(operations: [...])`:
>
> ```graphql
> operations: [add, delete]        ✔  enum values — unquoted
> operations: ["add", "delete"]    ✘  string literals — will fail validation
> ```

### `mergeInto` on an interface

Sets the **default** merge policy for all concrete types that implement this interface — without
requiring each concrete type to declare `interfacePolicy`.

```graphql
interface IAMResourceProtected @auth(mergeInto: "or", add: { rule: "...IAM admin check..." })
```

Every type implementing `IAMResourceProtected` will OR-merge its `add` rule by default.

### `interfacePolicy` on a concrete type

Overrides the merge policy for **specific interfaces** on this type. Can optionally restrict the
override to a subset of operations via `operations: [...]` — operations not listed fall through to
`mergeInto` (or the global AND default).

**Each interface may appear at most once** in the list. To apply different merge operators per
operation, use `operations` to declare which operations get the override; the rest fall back to the
interface's `mergeInto` or `"and"`:

```graphql
interface IAMResourceProtected @auth(mergeInto: "and", add: { rule: "...IAM admin check..." })

type Job implements IAMResourceProtected & WorkspaceMember
  @auth(
    interfacePolicy: [
      # OR for mutations — IAM admin role bypasses ownership check on writes.
      # query is not listed, so it falls back to mergeInto: "and".
      { interface: "IAMResourceProtected", merge: "or", operations: [add, update, delete] }
    ]
    add: { rule: "...own Job rule..." }
  )
# Job.add    = Job.own OR IAMResourceProtected.add    (or merge — in operations list)
# Job.update = Job.own OR IAMResourceProtected.update (or merge — in operations list)
# Job.query  = Job.own AND IAMResourceProtected.query (falls back to mergeInto: "and")
```

### Resolution order (per operation)

For each `(concrete type, interface, operation)` triplet the merge operator is chosen by this
priority chain:

```
1. interfacePolicy entry for this interface WITH this operation in its operations list  ← highest
2. interfacePolicy entry for this interface WITH no operations list (applies to all ops)
3. interface's mergeInto value
4. "and"                                                                                ← lowest
```

---

## Merge Semantics with Multiple Interfaces

When a type implements multiple interfaces with different policies, the final rule is constructed in
two deterministic phases **regardless of interface declaration order**:

**Phase 1:** AND-merge all interfaces resolved to `"and"`:

```
andGroup = concreteType.own AND andInterface1.rules AND andInterface2.rules ...
```

**Phase 2:** OR-merge all interfaces resolved to `"or"` on top:

```
finalRule = andGroup OR orInterface1.rules OR orInterface2.rules ...
```

**Example:**

```graphql
interface IAMResourceProtected @auth(mergeInto: "or", add: { rule: "...IAM check..." })
interface WorkspaceMember @auth(query: { rule: "...ws == $ws..." })

type Job implements IAMResourceProtected & WorkspaceMember
  @auth(add: { rule: "...own Job add rule..." })
# IAMResourceProtected: inherits "or" from mergeInto
# WorkspaceMember: defaults to "and"
```

Resulting `Job.add` rule:

```
(Job.own AND WorkspaceMember.add) OR IAMResourceProtected.add
```

Resulting `Job.query` rule:

```
Job.own AND WorkspaceMember.query AND IAMResourceProtected.query
```

_(IAMResourceProtected has no query rule here; WorkspaceMember default is AND)_

---

## Use Cases

### Interface as a hard requirement (AND — default)

No `mergeInto` or `interfacePolicy` needed. The default AND policy means the interface rule is
always required in addition to the concrete type's own rule.

```graphql
interface WorkspaceMember @auth(query: { rule: "...ws scope check..." })

type Job implements WorkspaceMember @auth(query: { rule: "...own Job rule..." })
# Job.query = Job.own AND WorkspaceMember.query
```

### Interface as an alternative access path (OR via mergeInto)

The interface declares itself as an OR gate — any implementor gets OR merge by default.

```graphql
interface IAMResourceProtected @auth(mergeInto: "or", add: { rule: "...admin role check..." })

type Job implements IAMResourceProtected & WorkspaceMember
  @auth(add: { rule: "...must own this specific Job..." })
# Job.add = (Job.own AND WorkspaceMember.add) OR IAMResourceProtected.add
```

### Selectively tightening an OR interface back to AND

A high-security type overrides the interface's OR default back to AND:

```graphql
type SensitiveDocument implements IAMResourceProtected
  @auth(
    interfacePolicy: [{ interface: "IAMResourceProtected", merge: "and" }]
    add: { rule: "...must also be the document owner..." }
  )
# SensitiveDocument.add = SensitiveDocument.own AND IAMResourceProtected.add
```

### Per-operation override (OR for mutations, AND for queries)

When the interface defaults to AND but mutations need OR, restrict the override with `operations`:

```graphql
interface IAMResourceProtected @auth(mergeInto: "and", add: { rule: "...IAM check..." })

type Report implements IAMResourceProtected
  @auth(
    interfacePolicy: [
      { interface: "IAMResourceProtected", merge: "or", operations: [add, update, delete] }
      # query is not listed → falls back to mergeInto "and"
    ]
    add: { rule: "...own Report rule..." }
  )
# Report.add    = Report.own OR  IAMResourceProtected.add    (OR — in list)
# Report.update = Report.own OR  IAMResourceProtected.update (OR — in list)
# Report.query  = Report.own AND IAMResourceProtected.query  (AND — falls back to mergeInto)
```

---

## Validation

All violations are caught at schema load time.

| Violation                                                                                     | Error             |
| --------------------------------------------------------------------------------------------- | ----------------- |
| `mergeInto` value is not `"and"` or `"or"`                                                    | Schema load error |
| `interfacePolicy` on an `INTERFACE` type (only valid on `OBJECT`)                             | Schema load error |
| `interfacePolicy.merge` value is not `"and"` or `"or"`                                        | Schema load error |
| `interfacePolicy.interface` names a type that is not an interface in the schema               | Schema load error |
| `interfacePolicy` references an interface the type doesn't implement                          | Schema load error |
| Same interface listed more than once in a single `interfacePolicy` list                       | Schema load error |
| `interfacePolicy.operations` is an empty list `[]` (omit the field to mean "all")             | Schema load error |
| `interfacePolicy.operations` contains an invalid value (not one of `add update delete query`) | Schema load error |
| Unknown field in an `InterfaceMergePolicy` entry (e.g. `operationsx`)                         | Schema load error |
