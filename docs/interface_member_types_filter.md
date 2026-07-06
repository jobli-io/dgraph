# Interface `memberTypes` Filter

Allows callers to scope an interface query to a subset of its concrete implementing types.

> **Status: Implemented.**

---

## Background

When you query an interface in Dgraph's GraphQL layer, the generated DQL root is:

```dql
func: type(InterfaceName)
```

Dgraph resolves this to every node whose `dgraph.type` list contains `InterfaceName`, which includes
all implementing types. There is no built-in way to restrict the result to only a subset of
implementors without fetching all of them and post-filtering in the application layer.

`memberTypes` solves this by scoping the DQL `func: type(...)` predicate at query time, allowing the
database to do the work.

---

## Generated Schema

For every interface that has at least one concrete implementing type, the schema generator
automatically produces:

1. **An implementor enum** — one value per concrete type, sorted alphabetically:

   ```graphql
   # generated for:  interface Manageable { ... }
   # implementors:   Workspace, Group, Plugin
   enum ManageableTypename {
     Group
     Plugin
     Workspace
   }
   ```

2. **A `memberTypes` field** in the interface's filter input type:

   ```graphql
   input ManageableFilter {
     memberTypes: [ManageableTypename] # ← scopes func: type(...)
     has: [ManageableHasFilter]
     and: [ManageableFilter]
     or: [ManageableFilter]
     not: ManageableFilter
   }
   ```

> **No directives required.** The enum and filter field are generated automatically for every
> interface. There is nothing to configure in the schema.

---

## Usage

### Filter to one implementor

```graphql
query {
  queryManageable(filter: { memberTypes: [Workspace] }) {
    __typename
    ... on Workspace {
      name
    }
  }
}
```

DQL produced:

```dql
queryManageable(func: type(Workspace)) { ... }
```

### Filter to multiple implementors

```graphql
query {
  queryManageable(filter: { memberTypes: [Workspace, Group] }) {
    __typename
    ... on Workspace {
      name
    }
    ... on Group {
      displayName
    }
  }
}
```

DQL produced:

```dql
queryManageable(func: type(Workspace, Group)) { ... }
```

### Combine with other filter fields

`memberTypes` composes freely with `has`, `and`, `or`, and `not` — but must remain at the **top
level** of the filter (see [Constraints](#constraints)):

```graphql
query {
  queryManageable(filter: { memberTypes: [Workspace], not: { has: managedBy } }) {
    ... on Workspace {
      name
    }
  }
}
```

DQL produced:

```dql
queryManageable(func: type(Workspace)) @filter(NOT has(Manageable.managedBy)) { ... }
```

### Works with aggregate queries

The same filter is reused by `aggregateXxx` queries, so `memberTypes` works there too:

```graphql
query {
  aggregateManageable(filter: { memberTypes: [Workspace] }) {
    count
  }
}
```

### No filter (unchanged behaviour)

Omitting `memberTypes` leaves the query unchanged — all implementors are returned:

```graphql
query {
  queryManageable {
    __typename
  }
  # DQL: func: type(Manageable) — all implementors
}
```

---

## Constraints

### Top-level only

`memberTypes` **must appear at the top level** of the filter. It cannot be nested inside `and`,
`or`, or `not`.

```graphql
# ✅ correct — top-level
queryManageable(filter: { memberTypes: [Workspace], not: { has: managedBy } })

# ❌ error — nested inside or
queryManageable(filter: { or: [{ memberTypes: [Workspace] }, { has: managedBy }] })
```

Attempting to nest it produces a query-validation error:

```
memberTypes is only supported at the top level of an interface filter,
not inside and, or, or not clauses.
Use a top-level memberTypes to scope the query to specific implementing types.
```

The reason is structural: `memberTypes` scopes the DQL `func: type(...)` root predicate, which
exists in a different position than the `@filter(...)` clause that `and`/`or`/`not` build. There is
no DQL equivalent for `OR`-combining two different root type functions.

### Empty list → empty result

```graphql
queryManageable(filter: { memberTypes: [] })
# Returns nothing. DQL: func: uid(0x0)
```

Consistent with how `memberTypes: []` behaves on union filter queries.

### Field name conflict

If the interface already declares a field named `memberTypes`, the synthetic filter field is
**silently skipped** for that interface to avoid generating a duplicate input field. The feature is
unavailable for that interface.

### Valid values only

`memberTypes` values are typed as the generated `XxxTypename` enum. The GraphQL parser rejects any
value that is not a concrete implementor of the interface:

```graphql
# ❌ rejected at parse time — NonExistent is not an implementor of Manageable
queryManageable(filter: { memberTypes: [NonExistent] })
# Error: Value "NonExistent" does not exist in enum "ManageableTypename"
```

---

## Auth interaction

Auth rules are unaffected. When `memberTypes` restricts the query to a subset of implementors, the
per-implementor auth var blocks for the non-selected types are still generated but produce empty
result sets and are harmlessly OR-combined. The net effect is correct: only nodes of the requested
types that also pass their auth rules are returned.

---

## Implementation notes

| Layer          | File                                 | Change                                                                                            |
| -------------- | ------------------------------------ | ------------------------------------------------------------------------------------------------- |
| Schema gen     | `graphql/schema/gqlschema.go`        | `addInterfaceMemberTypesEnum` generates the enum; `addFilterType` injects the `memberTypes` field |
| Query rewriter | `graphql/resolve/query_rewriter.go`  | `addFilter` intercepts `memberTypes`, replaces `q.Func.Args`; `buildFilter` skips it if nested    |
| Validation     | `graphql/schema/validation_rules.go` | `memberTypesCheck` rejects nested usage at query-parse time                                       |
