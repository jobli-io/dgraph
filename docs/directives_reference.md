# Custom Directive Reference

This document lists all custom directives added to Dgraph's GraphQL engine by Hypermode/jobli. These
are in addition to Dgraph's built-in directives (`@auth`, `@search`, `@id`, `@dgraph`,
`@hasInverse`, `@generate`, `@cascade`, `@custom`, `@remote`, `@lambda`, `@secret`, etc.).

---

## Quick Reference

| Directive                                          | Placement             | Purpose                                                                                                        | Doc                                                                  |
| -------------------------------------------------- | --------------------- | -------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------- |
| [`@cascadeAuth`](#cascadeauth)                     | `FIELD_DEFINITION`    | Propagate auth from authority type to child                                                                    | [cascade_auth.md](cascade_auth.md)                                   |
| [`@cascadeAuthPolicy`](#cascadeauthpolicy)         | `OBJECT \| INTERFACE` | Control cascade auth aggregation and opt-outs                                                                  | [cascade_auth.md](cascade_auth.md)                                   |
| [`@authVariables`](#authvariables)                 | `OBJECT \| INTERFACE` | Declare `<<KEY>>` template substitution values for `@auth` rules                                               | [cascade_auth.md](cascade_auth.md)                                   |
| [`@cascadeDelete`](#cascadedelete)                 | `FIELD_DEFINITION`    | Auto-delete linked nodes when parent is deleted                                                                | [cascade_delete.md](cascade_delete.md)                               |
| [`@postValidate`](#postvalidate)                   | `OBJECT \| INTERFACE` | Run expr-lang expression after mutation commits                                                                | [post_validate.md](post_validate.md)                                 |
| [`@validate`](#validate)                           | `FIELD_DEFINITION`    | Field-level validation before mutation commits                                                                 | [validate.md](validate.md)                                           |
| [`@default`](#default)                             | `FIELD_DEFINITION`    | Set default field value on add/update                                                                          | [default_transform.md](default_transform.md)                         |
| [`@transform`](#transform)                         | `FIELD_DEFINITION`    | Transform a field value via expr-lang on add/update; include `uid: #.uid` when forwarding `before.*` edge refs | [default_transform.md](default_transform.md)                         |
| [`@oldValue`](#oldvalue)                           | `FIELD_DEFINITION`    | Fetch pre-mutation field values for expr-lang expressions                                                      | [old_value.md](old_value.md)                                         |
| [`@dryRun`](#dryrun)                               | `MUTATION`            | Test prospective mutations and post-validation rules without committing to Badger/Raft                         | [#dryrun](#dryrun)                                                   |
| [`@bypassAuth`](#bypassauth)                       | `FIELD_DEFINITION`    | Conditionally or selectively bypass auth checks on a child edge                                                | [bypass_auth.md](bypass_auth.md)                                     |
| [`@hasInverse(immutable:)`](#hasinverse-immutable) | `FIELD_DEFINITION`    | Make a bidirectional edge write-once                                                                           | [immutable_inverse.md](immutable_inverse.md)                         |
| `memberTypes` filter                               | Interface `XxxFilter` | Scope an interface query to a subset of implementing types                                                     | [interface_member_types_filter.md](interface_member_types_filter.md) |
| `groupByXxx` query                                 | Root query (auto)     | Bucket-aggregate over filtered nodes; DateTime interval bucketing via `by`/`tz`                                | [groupby_queries.md](groupby_queries.md)                             |

---

## Directive Signatures

### `@cascadeAuth`

```graphql
directive @cascadeAuth(
  operations: [CascadeAuthOperation!] # default: [query, add, update, delete]
  depth: Int # default: 1; -1 = unlimited
  bidirectional: Boolean # default: false
  variableContext: CascadeAuthVariableContext # default: adaptive
  interfaceOnly: Boolean # default: false; skips per-implementor expansion for interface authorities
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

### `@cascadeAuthPolicy`

```graphql
directive @cascadeAuthPolicy(
  aggregation: String # "and" (default) | "or"
  skipBidirectional: Boolean # default: false
  skip: Boolean # default: false
) on OBJECT | INTERFACE
```

### `@authVariables`

```graphql
directive @authVariables(vars: [AuthVariable!]!) on OBJECT | INTERFACE

input AuthVariable {
  key: String!
  value: [String!]!
}
```

### `@cascadeDelete`

```graphql
directive @cascadeDelete(
  onlyIfOrphan: Boolean # default: false
  onlyIfOrphanScope: String # "type" (default) | "all"
  filter: String # expr-lang expression
  depth: Int # max hops (default: unlimited)
  authMode: String # "skip" (default) | "enforce" | "filter"
) on FIELD_DEFINITION
```

### `@postValidate`

```graphql
directive @postValidate(
  expr: String # top-level expr-lang (both add & update)
  reason: String
  rules: [DgraphPostValidate] # top-level rules list (both add & update)
  add: [DgraphPostValidate] # add-specific override (array or single object)
  update: [DgraphPostValidate] # update-specific override (array or single object)
) on OBJECT | INTERFACE

input DgraphPostValidate {
  expr: String
  reason: String
}
```

### `@validate`

```graphql
directive @validate(
  rule: String # go-playground/validator tag (backward compatibility)
  expr: String # expr-lang expression (backward compatibility)
  reason: String # default/fallback failure message
  rules: [DgraphValidate] # general rules for add and update (array or single object)
  add: [DgraphValidate] # add-specific rules (array or single object)
  update: [DgraphValidate] # update-specific rules (array or single object)
) on FIELD_DEFINITION

input DgraphValidate {
  rule: String
  expr: String
  reason: String
}
```

### `@default`

```graphql
directive @default(
  value: String # literal value or "$now"
  expr: String # expr-lang expression
  evaluationOrder: Int # execution priority (lower = first)
  refOnly: Boolean # shorthand for both add & update; see default_transform.md §refOnly
  add: DgraphDefault
  update: DgraphDefault
) on FIELD_DEFINITION

input DgraphDefault {
  value: String
  expr: String
  evaluationOrder: Int
  refOnly: Boolean # per-operation override; takes precedence over top-level
}
```

### `@transform`

```graphql
directive @transform(
  expr: String
  evaluationOrder: Int
  add: DgraphTransform
  update: DgraphTransform
) on FIELD_DEFINITION

input DgraphTransform {
  expr: String
  evaluationOrder: Int
}
```

### `@oldValue`

```graphql
directive @oldValue(
  fields: [String!] # dot-separated sub-field paths (edge fields only)
  first: Int # limit pre-fetched results for list-type edges
  sort: String # order before cap; prefix with "-" for descending
) on FIELD_DEFINITION
```

### `@hasInverse` (extended)

```graphql
# Standard Dgraph directive extended with immutable:
directive @hasInverse(
  field: String!
  immutable: Boolean # default: false — makes edge write-once
) on FIELD_DEFINITION
```

---

### `@dryRun`

```graphql
directive @dryRun(enabled: Boolean = false) on MUTATION
```

Evaluates input schemas, field defaults, and `@validate` rules; resolves the prospective graph diff
(`{ before, after, new }`); and runs the `@postValidate` rules array (including `callLambda`
compliance checks).

**Response Payload & Execution Semantics:**

- Queries the uncommitted graph state using the transaction's `StartTs` so that the GraphQL response
  payload (`numUids`, returned objects, selection set fields) returns the prospective result
  identical to what a committed mutation would have returned.
- Stops immediately prior to Badger/Raft commit: aborts and rolls back the transaction, ensuring
  nothing is persisted to the database.
- Webhook notifications (`@lambdaOnMutate`) are not dispatched.
- Injects `isDryRun: true` into the expression evaluation context.
- Returns `"dryRun": true` in the GraphQL response `"extensions"` payload
  (`"extensions": { "dryRun": true, ... }`).
- Supports GraphQL variables (e.g. `mutation ($dry: Boolean) @dryRun(enabled: $dry)`).

---

### `@bypassAuth`

```graphql
directive @bypassAuth(except: [String!], if: String) on FIELD_DEFINITION
```

Bypasses authorization rules (both standard `@auth` and cascade `@cascadeAuth`) when traversing a
child edge in queries.

- `if`: Optional boolean expression evaluated with [expr-lang](https://expr-lang.org) against `auth`
  claims. Bypass is only active if condition evaluates to `true`.
- `except`: Optional list of types, interfaces, or field paths on the child type that remain
  enforced even when bypass is active.

See [bypass_auth.md](bypass_auth.md) for complete documentation and examples.

---

## Shared expr-lang Evaluation Context

All expr-lang expressions (in `@default`, `@transform`, `@validate`, `@postValidate`) share the same
evaluation environment. Variables available at expression evaluation time:

| Variable     | Type     | Description                                                               |
| ------------ | -------- | ------------------------------------------------------------------------- |
| `input`      | `map`    | Mutation input after `@default` injection                                 |
| `rawInput`   | `map`    | Original user-provided input before `@default`                            |
| `before`     | `map`    | Pre-mutation field values (from `@oldValue` fetch)                        |
| `after`      | `map`    | Merged `before` + `input` (hypothetical post-state)                       |
| `new`        | `map`    | Diff: only keys that changed (`{ k: after[k] \| after[k] != before[k] }`) |
| `remove`     | `map`    | Fields being removed in this mutation                                     |
| `auth`       | `map`    | JWT claims (`auth.sub`, `auth.azp`, `auth.ws`, custom claims)             |
| `action`     | `string` | `"add"` or `"update"`                                                     |
| `isDryRun`   | `bool`   | `true` if the mutation has `@dryRun(enabled: true)`                       |
| `__typename` | `string` | GraphQL type name                                                         |

Built-in functions:

| Function                                           | Description                                                                                                                                         |
| -------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| `uuid()`                                           | Generate a new UUID string                                                                                                                          |
| `sha256(s)`                                        | SHA-256 hash of string                                                                                                                              |
| `generateEmbedding(provider, model, text, params)` | Call OpenAI/Gemini embedding API                                                                                                                    |
| `callLambda(name, payload)`                        | Call a registered lambda function (caller's JWT forwarded)                                                                                          |
| `diffMap(obj1, obj2)`                              | Return map of changed keys between two maps                                                                                                         |
| `mapStringWithoutKeys(map, keys)`                  | Return map with specified keys removed                                                                                                              |
| `error(v)`                                         | Abort with error. Accepts `string`, `[]string` (multiple errors), or `[]map` / object (rich error with `message`, `code`, and custom `extensions`). |

> **`@postValidate` only** additionally provides:
>
> - `nodes` — array of `{uid, before, after, new}` maps for all written nodes
> - `before` — array of pre-mutation maps across all nodes
> - `after` — array of post-mutation maps across all nodes
> - `new` — array of modified fields across all nodes
> - `action` — `"add"` or `"update"` (always present)
> - `isDryRun` — boolean flag indicating dry-run execution mode
> - `txn` — uncommitted transaction context (`{"startTs": <uint64>}`) for passing to `callLambda` to
>   execute uncommitted Dgraph queries

---

## Mutation Rewriter: `uid` Trust Signal

When a `@transform` expression emits an object that refers to an existing node fetched from
`before.*` (via `@oldValue`), include the `uid` field in the output to let the mutation rewriter
link to the existing node rather than creating a phantom blank node.

See
[Referencing Existing Nodes from `@transform`](./default_transform.md#referencing-existing-nodes-from-transform)
for the full explanation and pattern.

```graphql
# ✅ correct — uid acts as an existence proof for the mutation rewriter
"hasForm": {"sId": #.sId, "uid": #.uid}

# ❌ wrong — omitting uid causes a phantom empty node to be created
"hasForm": {"sId": #.sId}
```
