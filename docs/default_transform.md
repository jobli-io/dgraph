# @default and @transform Directives

Two complementary directives for computing and transforming field values during mutation rewriting:

- **`@default`** — injects a value when the field is absent from the mutation input
- **`@transform`** — always recomputes the field value, overriding any user-supplied value

Both run during mutation rewriting, **before** the DQL write is submitted to Dgraph.

---

## `@default`

### Signature

```graphql
directive @default(
  value: String # literal value or "$now"
  expr: String # expr-lang expression
  evaluationOrder: Int # execution priority (lower = earlier)
  add: DgraphDefault # add-specific override
  update: DgraphDefault # update-specific override
) on FIELD_DEFINITION

input DgraphDefault {
  value: String
  expr: String
  evaluationOrder: Int
}
```

### When It Runs

`@default` fires **only when the field is absent** (`nil`) from the mutation input. If the caller
explicitly provides the field, `@default` is not evaluated.

```graphql
type Document {
  id: ID!
  createdAt: DateTime @default(value: "$now") # always set on add, never overridable
  status: String @default(add: { value: "DRAFT" }) # set on add only
  title: String
}
```

### Value Forms

| Form                 | Example                    | Description                 |
| -------------------- | -------------------------- | --------------------------- |
| `value: "$now"`      | `@default(value: "$now")`  | Server timestamp (RFC3339)  |
| `value: "<literal>"` | `@default(value: "DRAFT")` | Literal string constant     |
| `expr: "<expr>"`     | `@default(expr: "uuid()")` | expr-lang expression result |

### Operation-Specific Arms

```graphql
type Workspace {
  # Auto-create a BillingAccount node on add; no-op on update
  hasBillingAccount: BillingAccount @default(add: { expr: "{}" })

  # Set on add; on update recompute from firstName + lastName
  displayName: String
    @default(
      add: {
        expr: "[input?.firstName ?? '', input?.lastName ?? ''] | join(' ') | trim()"
        evaluationOrder: 1
      }
    )
    @default(
      update: {
        expr: "[input?.firstName ?? '', input?.lastName ?? ''] | join(' ') | trim()"
        evaluationOrder: 1
      }
    )
}
```

Root-level `value`/`expr` applies to both add and update. Operation-specific arms take precedence.

### `evaluationOrder`

When multiple fields in a type have `@default` and one default depends on another (e.g.
`displayName` depends on `firstName`), use `evaluationOrder` to control execution order:

```graphql
firstName:   String  # evaluates first (no evaluationOrder = 0)
lastName:    String  # evaluates first (no evaluationOrder = 0)
displayName: String  @default(add: { expr: "...", evaluationOrder: 1 }) # evaluates second
```

Fields with lower `evaluationOrder` are processed first. Fields with the same (or no) order are
processed in schema declaration order.

### Server-Only Fields

Combine with `@generate(mutation: { add: false })` to create fields that **cannot** be set by
callers — only by `@default`:

```graphql
type User {
  id: ID!
  createdAt: DateTime
    @generate(mutation: { add: false }) # hidden from AddUserInput
    @default(value: "$now") # set automatically on add
}
```

---

## `@transform`

### Signature

```graphql
directive @transform(
  expr: String # expr-lang expression (applied to both add & update)
  evaluationOrder: Int
  add: DgraphTransform
  update: DgraphTransform
) on FIELD_DEFINITION

input DgraphTransform {
  expr: String
  evaluationOrder: Int
}
```

### How It Differs from `@default`

|                       | `@default`                 | `@transform`                     |
| --------------------- | -------------------------- | -------------------------------- |
| **Fires when**        | Field is absent from input | **Always**, regardless of input  |
| **User can override** | Yes                        | No — transform always overwrites |
| **Use case**          | Server-generated defaults  | Normalized/computed fields       |

```graphql
type Tag {
  id: ID!
  name: String! @transform(expr: "value.lowerAscii().trim()") # always normalize to lowercase
}
```

### expr-lang Context

Same as `@default` — all
[shared expr-lang variables](directives_reference.md#shared-expr-lang-evaluation-context) are
available. In `@transform`, `value` refers to the **user-supplied input value** (before the
transform). After transformation, the result replaces the field value in the mutation.

---

## expr-lang Evaluation Order During Rewriting

```
Pre-query: @oldValue fields fetched → before populated

Mutation rewriting:
  1. @validate runs (can abort early)
  2. @default runs (in evaluationOrder, for absent fields)
  3. @transform runs (in evaluationOrder, always overwrites)
  4. DQL mutation submitted

Post-mutation:
  5. @postValidate runs (after commit)
```

---

## Schema Validation

- `@default` and `@transform` may not be used on `@remote` types.
- `@default` may not be used on `@custom` or `@lambda` fields.
- At least one of `value`, `expr`, `add`, `update` must be present on `@default`.
- `expr` must compile as a valid expr-lang expression.
- `$now` is only valid as a `value` token, not inside `expr`.

---

## Typed Defaults: `expr` vs `value`

`@default(value: "...")` always produces a **string**, even if the field is typed `Boolean`, `Int`,
etc. Use `expr` when the field type requires a non-string literal:

```graphql
# ❌ Wrong — stores the string "false", not the boolean false
isDraft: Boolean @default(add: { value: "false" })

# ✅ Correct — stores boolean false
isDraft: Boolean @default(add: { expr: "false" })

# ✅ Also correct for integers
retryCount: Int @default(add: { expr: "0" })
```

> **Rule of thumb:** use `value:` only for `String` and `DateTime` fields (and `"$now"` for
> timestamps). Use `expr:` for `Boolean`, `Int`, `Int64`, `Float`, and for any expression that reads
> other fields.

---

## Referencing Existing Nodes from `@transform`

When a `@transform` expression builds an object that references an **already-existing** node (e.g. a
portal form fetched from `before.hasPortalForm`), the output must include the node's `uid` field so
the mutation rewriter can link to the existing node rather than creating a phantom blank node.

### Why this matters

The mutation pipeline runs in two phases:

1. **Phase 1 (existence queries)**: pre-fetches UIDs for nodes referenced in the **user-supplied
   mutation input** and registers them in `idExistence`.
2. **Phase 2 (mutation rewriting)**: converts the input tree (including `@transform` outputs) into
   DQL mutations. For each nested object, the rewriter checks `idExistence` to decide whether to
   link to an existing node or create a new one.

`@transform` fires **during Phase 2**. The nodes it references from `before.*` were fetched by the
Phase-1 pre-query but were **not registered in `idExistence`** (because they weren't in the user's
input). Without a hint, the rewriter treats the `{sId: "..."}` reference as a new node and emits a
blank-node forward-ref, creating an empty phantom node.

### The `uid` trust signal

The `uid` field is a DQL-internal key that can **never appear in user-supplied GraphQL input** (the
schema rejects it). Its presence in a transform output is therefore an unforgeable signal that the
object came from a server-side DQL query — and its UID can be trusted without a Phase-1 existence
check.

When the rewriter encounters an XID-only object (`isRefOnly = true`) that also carries a `uid`
field, it links directly to the existing node instead of creating a blank node:

```
{sId: "jobli::portalform::3bbd7879", uid: "0x2191dc"}
  → asIDReference("0x2191dc")   → {"uid": "0x2191dc"}   ✅ links existing node

{sId: "jobli::portalform::3bbd7879"}
  → blank-node forward-ref      → {"uid": "_:PortalForm_8"}  ⚠️ phantom new node
```

### Pattern: include `uid: #.uid` for nodes from `before`

For any `@transform` that maps over `before.*` edge results and produces references to those
existing nodes, include `"uid": #.uid` in the output object:

```graphql
# ✅ Correct — existing PortalForms from before.hasPortalForm are linked, not recreated
hasAdPostRecord: [AdPostRecord]
  @transform(expr: """
    let oldForms = before.hasPortalForm ?? [];
    map(oldForms, {
      {
        "forJobBoard": #.forJobBoard,
        "inGroup": after.hasPrimaryGroup,
        "hasForm": {"sId": #.sId, "uid": #.uid}
        # ^ uid is the Dgraph UID from Phase-1 pre-query; acts as existence proof
      }
    })
  """, evaluationOrder: 1001)
```

For **new** nodes being created in the same mutation (where `#.uid` is `nil`), `uid: nil` is
harmlessly ignored — the rewriter falls back to creating the node normally via the XID path.

### Why not just use `{"id": #.uid}`?

The `id: ID!` field path in the rewriter requires the UID to be pre-registered in `idExistence`
(from a Phase-1 existence query). For transform-injected nodes that weren't in the user's input,
this check fails. Including `uid` alongside (or instead of) `id` bypasses the Phase-1 check because
`uid` is the trust signal. You can use either:

| Transform output               | Works? | Notes                                           |
| ------------------------------ | ------ | ----------------------------------------------- |
| `{"sId": #.sId, "uid": #.uid}` | ✅     | Recommended — minimal, clean                    |
| `{"id": #.uid, "uid": #.uid}`  | ✅     | Both fields present; Fix 3 fires                |
| `{"id": #.uid}`                | ❌     | `uid` trust signal missing; Phase-1 check fails |
| `{"sId": #.sId}`               | ❌     | No uid → phantom blank node                     |

> **Note:** `#.uid` is available on nodes fetched by `@oldValue` pre-queries. `#.id` is only
> available if `"id"` was explicitly included in the `@oldValue(fields: [...])` list.

---

## See also

- [`@oldValue`](./old_value.md) — populate `before` with pre-mutation node state
- [`@postValidate`](./post_validate.md) — type-level validation that runs after commit
- [Directives Reference](./directives_reference.md) — all custom directives at a glance
