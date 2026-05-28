# `@postValidate` Directive

## Overview

`@postValidate` is a **type-level** directive that runs an expr-lang expression **after** a mutation
has been written to Dgraph but **before** the transaction is committed. If the expression evaluates
to `false`, the transaction is aborted and a validation error is returned — no data is persisted.

This complements `@validate`, which runs at the **field level** before the mutation executes.

---

## Syntax

```graphql
directive @postValidate(
  expr: String
  reason: String
  add: DgraphPostValidate
  update: DgraphPostValidate
) on OBJECT | INTERFACE

input DgraphPostValidate {
  expr: String
  reason: String
}
```

---

## Arguments

| Argument | Type                 | Description                                                                                                                                                                                                                           |
| -------- | -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `expr`   | `String`             | expr-lang expression applied to **both** `add` and `update` mutations.                                                                                                                                                                |
| `reason` | `String`             | Error message returned when validation fails. Supports Go [`text/template`](https://pkg.go.dev/text/template) syntax — embed `{{.count}}`, `{{.action}}`, `{{index (index .nodes 0) "after"}}` etc. Plain strings are returned as-is. |
| `add`    | `DgraphPostValidate` | Operation-specific `expr`/`reason` for **add** only. Takes precedence over the top-level `expr`.                                                                                                                                      |
| `update` | `DgraphPostValidate` | Operation-specific `expr`/`reason` for **update** only. Takes precedence over the top-level `expr`.                                                                                                                                   |

---

## Expression Context

The expression is evaluated **once per mutation** against the full batch of mutated nodes. Top-level
variables available to every expression:

| Variable | Type     | Description                                                                                                                                    |
| -------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `nodes`  | `[]map`  | Array of all mutated nodes of this type (root + nested, any depth). Each element has `uid`, `before`, `after`, and `new`. See structure below. |
| `action` | `string` | `"add"` or `"update"` — same as `@validate`'s `action`.                                                                                        |
| `auth`   | `map`    | JWT auth variables — same as `@validate`'s `auth`. Defaults to `{}` when no auth is configured or no JWT is present.                           |

### Helper functions

The same set of helper functions available in `@validate` are also available:

| Function               | Signature                                              | Description                                                      |
| ---------------------- | ------------------------------------------------------ | ---------------------------------------------------------------- |
| `callLambda`           | `(name string, payload map) (any, error)`              | Invoke a registered lambda by name, forwarding the caller's JWT. |
| `uuid`                 | `() string`                                            | Generate a new UUIDv4 string.                                    |
| `sha256`               | `(str string) string`                                  | Hex-encoded SHA-256 hash.                                        |
| `generateEmbedding`    | `(provider, model, text string, params map) []float32` | Generate a vector embedding via the configured provider.         |
| `diffMap`              | `(a, b map) (map, error)`                              | Returns keys present in `b` whose value differs from `a`.        |
| `mapStringWithoutKeys` | `(m map, keys []string) map`                           | Returns `m` minus the specified keys.                            |
| `error`                | `(v any) (any, error)`                                 | Immediately abort the expression with `v` as the error message.  |

### `nodes` element structure

Each element of `nodes` is a map with four keys:

```
{
  uid:    string  — the Dgraph UID of the node (e.g. "0x1a")
  before: map     — pre-mutation @oldValue fields (empty for new nodes)
  after:  map     — post-mutation @oldValue fields, read within the uncommitted transaction
  new:    map     — fields that changed: keys present in `after` whose value differs from `before`
}
```

All field names inside `before`, `after`, and `new` are **bare GraphQL field names** (e.g. `rating`,
not `Review.rating`). The Dgraph predicate prefix is stripped automatically.

### `new` variable

`new` mirrors `@validate`'s `new` variable — it contains only the fields that were actually modified
by this mutation:

- **Add mutations**: `before` is always `{}`, so `new` equals `after` (every field is "new").
- **Update mutations**: `new` contains only fields where `after[field] != before[field]`. Fields
  that were not touched are absent from `new`.

This lets you distinguish "was this field explicitly written?" from "what is its current value?".

### `before` state per scenario

| Scenario                                     | `before` content                                       |
| -------------------------------------------- | ------------------------------------------------------ |
| Add — new node                               | `{}` — no prior state exists                           |
| Add-upsert — node already existed            | Individual pre-mutation snapshot for that node         |
| Nested same-type (any depth)                 | Individual per-node snapshot — accurate at every level |
| Update — root filter matches 1 node          | Individual pre-mutation snapshot                       |
| Update — root filter matches N nodes         | **Merged** map — see note below                        |
| Nested new nodes (add-within-update)         | `{}` — they are new                                    |
| Nested existing nodes (referenced by ID/XID) | Individual per-node snapshot                           |

> [!NOTE] > **Merged `before` for bulk update mutations**
>
> When an update filter matches multiple nodes, Dgraph collects all their `@oldValue` fields into a
> single shared map by iterating over the result set and overwriting each field — whichever row was
> processed last wins per field. All matched nodes in `nodes` therefore see the same merged
> `before`, not their individual pre-mutation state.
>
> This is the same constraint that `@validate`'s `before` variable has. It is safe to use `before`
> for schema-level invariants (e.g. checking that `before.status` was `"draft"` when the filter
> itself requires `status == "draft"`), but not for per-node value comparisons across a
> heterogeneous result set.

---

## Expression Predicate Syntax

`@postValidate` uses [expr-lang/expr](https://expr-lang.org). The `nodes` array is passed as a
whole; use **global array functions** with a `{predicate}` block where `.` refers to the current
element:

| Pattern                                   | Description                                   |
| ----------------------------------------- | --------------------------------------------- |
| `all(nodes, {.after.field >= x})`         | Every node must satisfy the condition         |
| `any(nodes, {.after.status == "active"})` | At least one node matches                     |
| `filter(nodes, {.after.active == true})`  | Returns the subset of matching nodes          |
| `len(nodes) <= 10`                        | Batch size limit                              |
| `action == "add"`                         | Restrict expression to add mutations          |
| `"fieldName" in keys(.new)`               | Field was explicitly written in this mutation |

> [!IMPORTANT] Use `all(nodes, {.after.field})` — **not** `nodes.all(n, n.after.field)`. The
> `{predicate}` block syntax binds `.` to each element. The method-call form `array.all(n, ...)`
> does not correctly bind the iteration variable when `AllowUndefinedVariables` is active, causing
> runtime nil-access errors.

---

## Behaviour

| Scenario                      | Behaviour                                                                                                           |
| ----------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| Expression returns `true`     | Transaction commits normally.                                                                                       |
| Expression returns `false`    | Transaction aborted; `reason` (rendered as template, `{{.error}}` = `""`) returned as error.                        |
| Expression calls `error(msg)` | Transaction aborted; `reason` rendered with `{{.error}}` = `msg`. If no `reason`, the raw error string is returned. |
| Delete mutation               | Silently skipped — nodes are already removed.                                                                       |
| No `@oldValue` fields         | `before`/`after`/`new` are empty maps; `auth`/`action` still usable.                                                |
| Bulk / nested mutations       | All nodes bundled into one `nodes` array; expression runs once.                                                     |
| No UIDs in mutation response  | Skipped — nothing to validate (e.g. no-op update).                                                                  |

---

## `reason` as a Template

The `reason` string is processed as a [Go `text/template`](https://pkg.go.dev/text/template) before
being returned to the caller. This lets you embed context values directly in the error message. The
same variable names available in `expr` are available in `reason`.

### Available template variables

| Variable | Example access             | Description                                                                                                                                                |
| -------- | -------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `nodes`  | `{{len .nodes}}`           | The full mutated-node batch                                                                                                                                |
| `count`  | `{{.count}}`               | Shorthand for `len(nodes)`                                                                                                                                 |
| `action` | `{{.action}}`              | `"add"` or `"update"`                                                                                                                                      |
| `auth`   | `{{index .auth "USERID"}}` | JWT claim variables                                                                                                                                        |
| `error`  | `{{.error}}`               | Underlying error message when the expression itself errored (e.g. lambda returned non-200). Empty string `""` when the expression simply returned `false`. |

> [!NOTE] Plain strings (no `{{`) are returned as-is with zero overhead. If the template fails to
> parse or execute, the literal `reason` string is returned unchanged — validation failure is never
> silently lost.
>
> When `reason` is set and the expression **errors at runtime** (e.g. `callLambda` returns non-200),
> the reason template is used as the user-facing message instead of the raw Go error. When `reason`
> is not set, the raw error is wrapped and returned as usual.

### `@postValidate` examples

```graphql
type Group
  @postValidate(
    add: {
      expr: "len(nodes) <= 10"
      reason: "Cannot add {{.count}} groups in one request — maximum is 10"
    }
  ) {
  id: ID!
}
```

```graphql
type Order
  @postValidate(
    update: {
      expr: "all(nodes, {.after.status != \"cancelled\" || .after.refundProcessed == true})"
      reason: "Cancelled orders must have a processed refund (action: {{.action}})"
    }
  ) {
  id: ID!
  status: String @oldValue
  refundProcessed: Boolean @oldValue
}
```

### `@validate` examples

For `@validate`, the template data is the same map used for `expr` evaluation — including `before`,
`after`, `new`, `input`, `action`, `auth`, and the field `value`:

```graphql
type Product {
  price: Float @validate(expr: "value > 0", reason: "Price must be positive, got {{.value}}")
}
```

```graphql
type Bid {
  amount: Float
    @validate(
      update: {
        expr: "value > before.amount"
        reason: "New bid {{.value}} must exceed current bid {{index .before \"amount\"}}"
      }
    )
}
```

## Examples

### State transition guard (using `before` and `after`)

```graphql
type Post
  @postValidate(
    update: {
      expr: """
      all(nodes, {
        .before.status != "published" ||
        .after.status  == "archived"
      })
      """
      reason: "Published posts can only transition to archived"
    }
  ) {
  id: ID!
  status: String @oldValue
}
```

### Enforce non-empty body on published posts

```graphql
type Post
  @postValidate(
    expr: "all(nodes, {.after.status != \"published\" || .after.body != \"\"})"
    reason: "Published posts must have a non-empty body"
  ) {
  id: ID!
  status: String @oldValue
  body: String @oldValue
}
```

### Cross-node aggregate: at most 3 draft posts per mutation

```graphql
type Post
  @postValidate(
    add: {
      expr: "len(filter(nodes, {.after.status == \"draft\"})) <= 3"
      reason: "Cannot create more than 3 draft posts in a single mutation"
    }
  ) {
  id: ID!
  status: String @oldValue
}
```

### Require a field to actually change on update (using `new`)

```graphql
type Document
  @postValidate(
    update: {
      expr: "all(nodes, {\"version\" in keys(.new)})"
      reason: "version must be explicitly incremented on every update"
    }
  ) {
  id: ID!
  version: Int @oldValue
  content: String @oldValue
}
```

### Only allow fields to increase, never decrease (using `before` and `after`)

```graphql
type Leaderboard
  @postValidate(
    update: {
      expr: "all(nodes, {.after.score >= .before.score})"
      reason: "Scores may only increase"
    }
  ) {
  id: ID!
  score: Int @oldValue
}
```

### Auth-gated validation

```graphql
type Post
  @postValidate(
    add: {
      expr: """
      auth.ROLE == "admin" ||
      all(nodes, {.after.status != "published"})
      """
      reason: "Only admins can publish posts directly on creation"
    }
  ) {
  id: ID!
  status: String @oldValue
}
```

### Different rules for add vs update

```graphql
type Order
  @postValidate(
    add: {
      expr: "all(nodes, {.after.totalAmount > 0})"
      reason: "New orders must have a positive total amount"
    }
    update: {
      expr: "all(nodes, {.after.status != \"cancelled\" || .after.refundProcessed == true})"
      reason: "Cancelled orders must have a processed refund"
    }
  ) {
  id: ID!
  totalAmount: Float @oldValue
  status: String @oldValue
  refundProcessed: Boolean @oldValue
}
```

### Custom logic via lambda (`callLambda`)

For validation that requires external state or complex business logic, delegate to a lambda:

```graphql
type Group
  @postValidate(
    add: {
      expr: """
      let res = callLambda("Group.checkBulkQuota", {"nodes": nodes, "action": action});
      res.allowed == true
      """
      reason: "Group quota check failed: {{.error}}"
    }
  ) {
  id: ID!
  name: String @oldValue
  ownerId: String @oldValue
}
```

`{{.error}}` is populated when the expression **errors at runtime** — for example when the lambda
returns a non-200 HTTP status. The caller receives a clean message like:

```
"Group quota check failed: lambda Group.checkBulkQuota returned non-200 status: 400 Bad Request, body: ..."
```

When the expression returns `false` (no runtime error), `{{.error}}` is an empty string. You can
combine both cases:

```graphql
reason: "{{if .error}}Lambda error: {{.error}}{{else}}{{.count}} groups exceeds quota{{end}}"
```

The lambda receives the full `nodes` batch (with `uid`, `before`, `after`, `new`), can query
external systems or Dgraph directly, and must return a response that the expression can evaluate.
The caller's JWT is forwarded automatically.

### Validate on add only; skip on update

```graphql
type Invoice
  @postValidate(
    add: {
      expr: "all(nodes, {.after.lineItemCount > 0})"
      reason: "Invoice must have at least one line item when created"
    }
  ) {
  id: ID!
  lineItemCount: Int @oldValue
}
```

---

## Schema Validation

The schema loader enforces at load time:

- `@postValidate` is **not allowed** on `@remote` types.
- At least one `expr` must be present (top-level or inside `add`/`update`).
- Every `expr` is compiled at **schema load time** against a zero-value typed env (`nodes: []map`,
  `action: string`, `auth: map`) with `AllowUndefinedVariables` so runtime-only data (JWT claims,
  node values) does not cause false positives. Syntax errors are reported as schema load errors —
  identical to `@validate`, `@transform`, and `@default`.

---

## Implementation Notes

- **UID collection**: `mutResp.GetUids()` is filtered by the blank-node prefix `TypeName_` (e.g.
  `Post_`) — the naming scheme of `VariableGenerator.Next`. This captures every node of the type
  created at any nesting depth in one pass, including self-referencing types. For update mutations,
  root-level existing UIDs are also extracted via `extractMutated`.
- **`before` join**: `GetOldValueMap()` on the rewriter returns the `variableOldValueMap` populated
  during the existence-query phase (before the mutation runs). Each UID is matched back to its
  blank-node variable name via the inverted `mutResp.GetUids()` map. No extra Dgraph round-trips are
  needed — the data is already in memory.
- **`new` computation**: computed inline per-node as `{ k: after[k] | after[k] != before[k] }`. For
  add mutations `before` is `{}` so `new == after`.
- **Single `after` round-trip**: All matched UIDs are fetched in one DQL query
  (`uid(uid1, uid2, ...)`) within the same uncommitted transaction.
- **`@oldValue` infrastructure**: `getFieldsForExistsQuery` is reused to build the DQL selection for
  both `before` (existence queries) and `after` (post-validate query).
- **Field name normalisation**: Dgraph returns predicate-namespaced keys (`Review.rating`).
  `normalizePredicateKeys` strips the type prefix before populating `before`, `after`, and `new`, so
  expressions always use bare GQL field names.
- **`auth` nil-safety**: `authVars` defaults to `{}` when `ExtractCustomClaims` fails (no auth
  configured, absent/invalid JWT), so `auth.*` field access is always safe.
- **Expression env**: compiled with `expr.Env(postValidateEnv{})` (typed struct so the type checker
  knows `nodes` element type and `{predicate}` blocks bind `.` correctly) plus
  `expr.AllowUndefinedVariables()` (allows chained `interface{}` field access on `.after.field`).
  `expr.Run` receives the same struct type — passing a `map` when `Env` is a struct causes a reflect
  panic.
