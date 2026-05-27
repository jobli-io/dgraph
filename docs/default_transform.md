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
  expr: String # CEL expression
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

| Form                 | Example                    | Description                |
| -------------------- | -------------------------- | -------------------------- |
| `value: "$now"`      | `@default(value: "$now")`  | Server timestamp (RFC3339) |
| `value: "<literal>"` | `@default(value: "DRAFT")` | Literal string constant    |
| `expr: "<CEL>"`      | `@default(expr: "uuid()")` | CEL expression result      |

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
  expr: String # CEL expression (applied to both add & update)
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

### CEL Context

Same as `@default` — all
[shared CEL variables](directives_reference.md#shared-cel-evaluation-context) are available. In
`@transform`, `value` refers to the **user-supplied input value** (before the transform). After
transformation, the result replaces the field value in the mutation.

---

## CEL Evaluation Order During Rewriting

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
- `expr` must compile as a valid CEL expression.
- `$now` is only valid as a `value` token, not inside `expr`.
