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

### Filter via a nested interface-typed field

`memberTypes` also works when applied to a nested field whose type is an interface. For example, if
`Note.forResource` is a field that references a `Resource` interface:

```graphql
query {
  aggregateNote(filter: { forResource: { memberTypes: [Company] } }) {
    count
  }
}
```

DQL produced for the nested var query:

```dql
var(func: type(Company)) { Note.forResource as Resource.hasNote }
```

Without `memberTypes`:

```dql
var(func: type(Resource)) { Note.forResource as Resource.hasNote }  # all implementors
```

This lets you efficiently count or query objects by what _kind_ of resource they are linked to,
without fetching and post-filtering in the application layer.

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

### Not composable with `and`/`or`/`not`

`memberTypes` **cannot be nested inside** `and`, `or`, or `not`. It must be the direct top-level key
of whichever filter object it appears in — whether that is a root query filter or a nested field
filter.

```graphql
# ✅ correct — top-level of the root query filter
queryManageable(filter: { memberTypes: [Workspace], not: { has: managedBy } })

# ✅ correct — top-level of a nested field filter
aggregateNote(filter: { forResource: { memberTypes: [Company] } })

# ❌ error — memberTypes nested inside or
queryManageable(filter: { or: [{ memberTypes: [Workspace] }, { has: managedBy }] })
```

Attempting to compose it inside a logical combinator produces a query-validation error:

```
memberTypes is only supported at the top level of an interface filter,
not inside and, or, or not clauses.
Use a top-level memberTypes to scope the query to specific implementing types.
```

The reason is structural: `memberTypes` replaces the DQL `func: type(...)` predicate, which exists
in a different position than the `@filter(...)` clause that `and`/`or`/`not` build. There is no DQL
equivalent for OR-combining two different root type functions.

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

## Auth interaction & Optimization

When `memberTypes` restricts the query to a subset of implementors, the query rewriter automatically
optimizes the generated authorization sub-queries using a **Drop-Scoping Optimization**:

1. **Rule Suppression & Early Bypassing**: The `allowedTypesForEdge` mapping is populated in the
   `authRewriter` under both the Dgraph predicate (e.g., `Note.forResource`) and the interface type
   name (e.g., `NoteOwner`).
2. **`CascadeWrap` and `CascadeEdge` Drop-Scoping**: During the rule tree traversal in
   `rewriteRuleNode`, if the compiler encounters a `CascadeWrap` or a `CascadeEdge` node belonging
   to a non-selected implementing type, it discards that entire compiled cascade branch by returning
   `nil, nil`.
3. **No Unused/Circular Blocks**: None of the per-implementor auth rules, variables, or dependency
   tracking blocks (e.g., intermediate `Group`, `Workspace`, or `Billing` scopes) are generated for
   the excluded implementing types.

This highly optimizes the generated DQL statement, removing massive dependency-tracking clauses,
preventing circular dependencies, and significantly reducing Dgraph's execution overhead. Only nodes
of the requested types that also pass their auth rules are compiled, authorized, and returned.

---

## Implementation notes

| Layer          | File                                 | Change                                                                                                                                                                               |
| -------------- | ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Schema gen     | `graphql/schema/gqlschema.go`        | `addInterfaceMemberTypesEnum` generates the enum; `addFilterType` injects the `memberTypes` field                                                                                    |
| Query rewriter | `graphql/resolve/query_rewriter.go`  | `addFilter` intercepts `memberTypes` at root, scopes `q.Func.Args`; `buildFilter`'s nested field branch extracts it before building the nested var query and scopes `nestedQry.Func` |
| Validation     | `graphql/schema/validation_rules.go` | `memberTypesCheck` rejects nested usage at query-parse time                                                                                                                          |
