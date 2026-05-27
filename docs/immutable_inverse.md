# `@hasInverse(immutable: true)`

## Overview

The `immutable: Boolean` argument on `@hasInverse` makes a bidirectional edge **write-once**: the
edge can be set when a node is first created, but it cannot be reassigned or removed after that via
a GraphQL mutation.

Node-deletion cascade still clears the edge naturally (existing Dgraph behaviour).

---

## Syntax

```graphql
directive @hasInverse(field: String!, immutable: Boolean) on FIELD_DEFINITION
```

```graphql
type Group {
  workspace: Workspace @hasInverse(field: "groups", immutable: true)
}

type Workspace {
  groups: [Group] @hasInverse(field: "workspace", immutable: true)
}
```

You only need to declare `immutable: true` on **one** side — the validator automatically propagates
the flag to the inverse field. If you declare both sides explicitly, they must agree (asymmetric
declarations are a schema validation error).

---

## Behaviour

### One-to-one (scalar ↔ scalar)

```graphql
type House  { id: ID!; owner: Owner @hasInverse(field: "house", immutable: true) }
type Owner  { id: ID!; house: House @hasInverse(field: "owner", immutable: true) }
```

| Operation                                                                            | Result                                                         |
| ------------------------------------------------------------------------------------ | -------------------------------------------------------------- |
| `addHouse({ owner: { id: "0xO1" } })` — Owner.house currently unset                  | ✅ Allowed — first write                                       |
| Same mutation re-run                                                                 | ✅ Allowed — idempotent (edge already points to same House)    |
| `addHouse({ owner: { id: "0xO1" } })` — Owner.house already set to a different House | ❌ Rejected at runtime — Owner.house is occupied               |
| `updateHouse(set: { owner: ... })`                                                   | ❌ Rejected at schema layer — `owner` absent from `HousePatch` |
| Delete the House node                                                                | ✅ Allowed — Dgraph cascade clears Owner.house                 |

### One-to-many (scalar ↔ list)

```graphql
type Parent { id: ID!; children: [Child] @hasInverse(field: "parent", immutable: true) }
type Child  { id: ID!; parent: Parent    @hasInverse(field: "children", immutable: true) }
```

The **scalar** side (`Child.parent`) is the immutable constraint — a Child's parent cannot change
once set. The list side (`Parent.children`) grows freely.

| Operation                                                                            | Result                                                          |
| ------------------------------------------------------------------------------------ | --------------------------------------------------------------- |
| `addChild({ parent: { id: "0xP1" } })` — Child.parent unset                          | ✅ Allowed — first write                                        |
| `addParent({ children: [{ id: "0xC1" }] })` — Child 0xC1 has no parent yet           | ✅ Allowed — first write for Child.parent                       |
| `addParent({ children: [{ id: "0xC1" }] })` — Child 0xC1 **already has** parent 0xP1 | ❌ Rejected at runtime — would reassign Child.parent            |
| `addParent2({ children: [{ id: "0xC1" }] })` — another new Parent claiming 0xC1      | ❌ Rejected at runtime — Child.parent already occupied          |
| `updateChild(set: { parent: ... })`                                                  | ❌ Rejected at schema layer — `parent` absent from `ChildPatch` |

> **The `A <-> [B], addC <-> [B]` case:** When `A` and `B` are linked (`B.parent = A`), executing
> `addC({ children: [B] })` is **always rejected**. Even though C is a brand-new node, the runtime
> checks B's existing parent before writing — and finds it already occupied by A.

---

## Arguments

| Argument    | Type      | Required | Description                                                   |
| ----------- | --------- | -------- | ------------------------------------------------------------- |
| `field`     | `String!` | Yes      | The name of the inverse field on the target type              |
| `immutable` | `Boolean` | No       | When `true`, the edge pair is write-once. Defaults to `false` |

---

## Constraints

- **Not allowed on many-to-many** (both sides are list types). A `[Parent]` ↔ `[Child]`
  relationship is ambiguous for immutability — use only on one-to-one or one-to-many.
- **Both sides must agree** — if you explicitly declare `@hasInverse` on both fields, both must have
  the same `immutable` value. Mixing `immutable: true` on one side and `false` (or omitted) on the
  other is a schema validation error.
- **Permanent but clearable on deletion** — `immutable` edges are cleared when the node owning the
  edge is deleted (standard Dgraph cascade). The directive only prevents mutation-based
  reassignment.

---

## How enforcement works

### Schema layer (compile-time)

Fields with `@hasInverse(immutable: true)` are **excluded from `XxxPatch`** input types. This covers
both the `set` and `remove` clauses of update mutations — clients receive a GraphQL schema error
before the request reaches the server.

### Runtime layer (pre-mutation existence check)

When an add mutation references an existing node by UID (e.g. `parent: { id: "0xP1" }`), the
pre-mutation existence query fetches the referenced node's immutable inverse field. Before writing,
the runtime checks:

1. Is the inverse field already set on the referenced node?
2. If yes — does it point back to the **same** source node (idempotent re-establish)?
3. If it points to a **different** node → **reject** with an error.

This check fires regardless of whether the **calling** node is new or existing:

- `addC({ children: [B] })` — C is brand-new, but B is checked
- `addA({ children: [B] })` where B already has A as parent — idempotent, allowed

---

## Error messages

```
cannot link to Child 0xC1 — Child.parent is immutable and already points to 0xP1
```

---

## Known limitation: TOCTOU race

Two concurrent mutations both linking to the same node can both pass the pre-check if they execute
between each other's read and write phases. For strict exclusivity under high concurrency, enforce
uniqueness at the application layer (e.g. a distributed lock or a serialised upsert pattern).

---

## Full example: Parent–Child with permanent parentage

```graphql
type Parent {
  id: ID!
  name: String!
  children: [Child] @hasInverse(field: "parent", immutable: true)
}

type Child {
  id: ID!
  name: String!
  parent: Parent @hasInverse(field: "children", immutable: true)
}
```

```graphql
# ✅ First write — establishing Child.parent
mutation {
  addChild(input: [{ name: "Alice", parent: { id: "0xP1" } }]) {
    child {
      id
      parent {
        id
      }
    }
  }
}

# ✅ Adding a new Parent that claims a free Child
mutation {
  addParent(input: [{ name: "New Parent", children: [{ id: "0xC_free" }] }]) {
    parent {
      id
    }
  }
}

# ❌ Blocked at schema layer — parent absent from ChildPatch
mutation {
  updateChild(
    input: {
      filter: { id: ["0xC1"] }
      set: { parent: { id: "0xP2" } } # schema error: parent not in ChildPatch
    }
  ) {
    child {
      id
    }
  }
}

# ❌ Blocked at runtime — Child 0xC1 already has a parent (0xP1)
#    Even though ParentC is brand-new, 0xC1's parent is checked before writing.
mutation {
  addParent(input: [{ name: "ParentC", children: [{ id: "0xC1" }] }]) {
    parent {
      id
    }
  }
}
```

## See also

- [Field-level `@generate`](./field_level_generate.md) — hide scalar fields from mutation inputs or
  query output while keeping them writable internally via `@default`
- [`@oldValue`](./old_value.md) — access pre-mutation field values in expr-lang expressions
