# @cascadeDelete Directive

Automatically delete linked nodes when a parent node is deleted. Cascades follow the directive
recursively — deleting `A` deletes `B`, which deletes `C`, all in one atomic transaction.

---

## Signature

```graphql
directive @cascadeDelete(
  onlyIfOrphan: Boolean # only delete child if no other node references it
  onlyIfOrphanScope: String # "type" (default) | "all"
  filter: String # expr-lang expression; cascade only when true
  depth: Int # max recursion depth (unlimited by default)
  authMode: String # "skip" (default) | "enforce" | "filter"
) on FIELD_DEFINITION
```

Place `@cascadeDelete` on **edge fields** (relationship fields). It cannot be used on scalars,
enums, `@custom` or `@lambda` fields, or types with `@remote`.

---

## Arguments

### `onlyIfOrphan` / `onlyIfOrphanScope`

Only delete a child node if no other node references it.

```graphql
type Order {
  customer: Customer @cascadeDelete(onlyIfOrphan: true)
}
```

`onlyIfOrphanScope` controls which incoming edges are checked:

| Value                | Description                                     |
| -------------------- | ----------------------------------------------- |
| `"type"` _(default)_ | Only count edges from the **same parent type**  |
| `"all"`              | Count **any** incoming edge in the entire graph |

```graphql
settings: WorkspaceSettings @cascadeDelete(
  onlyIfOrphan: true,
  onlyIfOrphanScope: "all"
)
```

---

### `filter`

A expr-lang expression evaluated per child node. Only cascade-delete nodes where the expression
returns `true`.

Available variables:

| Variable | Description                                       |
| -------- | ------------------------------------------------- |
| `value`  | The child node as a map                           |
| `parent` | The parent node                                   |
| `auth`   | JWT auth variables                                |
| `before` | Pre-delete values of `@oldValue` annotated fields |

```graphql
type Campaign {
  drafts: [Draft!] @cascadeDelete(filter: "value.published == false")
}
```

---

### `depth`

Limits how many levels deep the cascade recurses. Omit for unlimited depth.

```graphql
# Tree with self-reference — stop after 5 levels
type Category {
  subcategories: [Category!] @cascadeDelete(depth: 5)
}
```

---

### `authMode`

Controls whether `@auth(delete: ...)` rules are checked on cascaded child types.

| Value                | Behaviour                                                                                  |
| -------------------- | ------------------------------------------------------------------------------------------ |
| `"skip"` _(default)_ | No auth check — all linked nodes are deleted                                               |
| `"enforce"`          | If the caller lacks permission to delete **any** child node, the **entire cascade aborts** |
| `"filter"`           | Only delete children the caller is authorized to delete; silently skip the rest            |

```graphql
type Workspace {
  projects: [Project!] @cascadeDelete(authMode: "enforce")
  settings: WorkspaceSettings @cascadeDelete(authMode: "filter")
  auditLogs: [AuditLog!] @cascadeDelete # "skip" — always delete
}
```

---

## Cascading Chains

Place the directive on each type in the chain:

```graphql
type Workspace {
  projects: [Project!] @cascadeDelete
}

type Project {
  tasks: [Task!] @cascadeDelete
  settings: ProjectSettings @cascadeDelete
}

type Task {
  comments: [Comment!] @cascadeDelete
}
```

Deleting a `Workspace` deletes all its `Project`s, their `Task`s and `ProjectSettings`, and all
`Comment`s — in one Dgraph transaction.

---

## Interface Inheritance

`@cascadeDelete` placed on an interface field is inherited by all implementing types:

```graphql
interface Recordable {
  history: [HistoryEntry!] @cascadeDelete
}

type Invoice implements Recordable {
  # history cascade inherited from Recordable
  lineItems: [LineItem!] @cascadeDelete
}
```

---

## Validation Rules

| Violation                                           | Error                                             |
| --------------------------------------------------- | ------------------------------------------------- |
| Used on a scalar or enum field                      | `@cascadeDelete can only be used on edge fields`  |
| Used on a `@remote` type                            | `@cascadeDelete cannot be used on a @remote type` |
| Used on a `@custom` or `@lambda` field              | Not allowed                                       |
| `depth` is not a positive integer                   | `depth must be a positive integer`                |
| `onlyIfOrphanScope` not `"type"` or `"all"`         | Validation error                                  |
| `authMode` not `"skip"`, `"enforce"`, or `"filter"` | Validation error                                  |
| `filter` expression does not compile                | Compile error with reason                         |
| Circular cascade chain (A → B → A)                  | `@cascadeDelete forms a cycle through type B`     |
