# Interface Auth Merge Policy

Controls how a specific interface's `@auth` rules are combined with a concrete implementing type's
own `@auth` rules at schema compile-time.

> **Status: Implemented.**

---

## Background

When a concrete type implements an interface that has `@auth` rules, those rules are currently
**AND-merged** into the concrete type unconditionally:

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

input InterfaceMergePolicy {
  interface: String! # name of the interface to override
  merge: String! # "and" | "or"
  operations: [String!] # optional: query | add | update | delete | password
  # if absent, applies to all operations
}
```

### `mergeInto` on an interface

Sets the **default** merge policy for all concrete types that implement this interface — without
requiring each concrete type to declare `interfacePolicy`.

```graphql
interface IAMResourceProtected @auth(mergeInto: "or", add: { rule: "...IAM admin check..." })
```

Every type implementing `IAMResourceProtected` will now OR-merge its add rule by default.

### `interfacePolicy` on a concrete type

Overrides the merge policy for **specific interfaces** on this type. Can optionally restrict the
override to specific operations — other operations fall through to `mergeInto` or the global AND
default.

```graphql
type Job implements IAMResourceProtected & WorkspaceMember
  @auth(
    interfacePolicy: [
      # OR only for mutations — IAM admin bypasses ownership check on writes
      { interface: "IAMResourceProtected", merge: "or", operations: ["add", "update", "delete"] }
      # AND for queries — both checks always required for reads
      { interface: "IAMResourceProtected", merge: "and", operations: ["query"] }
    ]
    add: { rule: "...own Job rule..." }
  )
```

### Resolution order (per operation)

For each `(concrete type, interface, operation)` triplet:

```
1. Concrete type's interfacePolicy[interface][operation]   ← exact match, highest priority
2. Concrete type's interfacePolicy[interface]["*"]         ← wildcard (no operations specified)
3. Interface's mergeInto                                   ← declared default
4. "and"                                                   ← global default
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

---

## Validation

| Violation                                                            | Error             |
| -------------------------------------------------------------------- | ----------------- |
| `mergeInto` value is not `"and"` or `"or"`                           | Schema load error |
| `interfacePolicy.merge` value is not `"and"` or `"or"`               | Schema load error |
| `interfacePolicy` references an interface the type doesn't implement | Schema load error |
