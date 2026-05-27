# Field-level `@generate` Directive

The `@generate` directive can be applied to **individual field definitions** (in addition to
type-level declarations) to control whether a field appears in mutation inputs and query results.

## Motivation

Some fields should be managed **internally** — set automatically via `@default` or `@transform` —
without exposing them to API clients. Common examples:

- `updatedAt`: set by `@default` on every write; clients should never send or see it.
- `createdAt`: set by `@default` on creation; clients can read it, but never update it.
- `secretToken`: generated internally; clients can read nothing about it.

Field-level `@generate` makes this pattern declarative and enforced at the schema level.

> **For write-once _edges_** (relationships between types), see
> [`@hasInverse(immutable: true)`](./immutable_inverse.md) instead. Field-level `@generate` is for
> scalar fields and does not enforce edge immutability.

## Syntax

```graphql
fieldName: FieldType @generate(
  mutation: { add: Boolean, update: Boolean },
  query: Boolean
)
```

All flags are optional and default to `true` (field is included everywhere).

| Flag              | Effect when `false`                                                        |
| ----------------- | -------------------------------------------------------------------------- |
| `mutation.add`    | Field excluded from `AddXxxInput`                                          |
| `mutation.update` | Field excluded from `XxxPatch` (both `set` and `remove`)                   |
| `query`           | Field excluded from the output type — clients cannot query or filter on it |

The flags are **independent** and can be combined freely.

## Examples

### Fully internal field (common `updatedAt` pattern)

```graphql
type Post {
  id: ID!
  title: String!
  updatedAt: DateTime
    @generate(mutation: { add: false, update: false }, query: false)
    @default(add: { expr: "now()" }, update: { expr: "now()" })
}
```

**Result:**

- `updatedAt` is absent from `AddPostInput`, `PostPatch`, and the `Post` output type.
- `@default` sets it automatically on every add and update.
- Clients never see `updatedAt` in queries and cannot set it in mutations.

### Write-once, readable field (`createdAt` pattern)

```graphql
type Post {
  id: ID!
  title: String!
  createdAt: DateTime @generate(mutation: { update: false }) @default(add: { expr: "now()" })
}
```

**Result:**

- `createdAt` is absent from `PostPatch` — it can never be updated after creation.
- `createdAt` is present in `AddPostInput` (optional, because of `@default`).
- Clients can query `createdAt` in results.
- `@default` sets it automatically on creation if the client omits it.

### Set on creation, never accessible to clients

```graphql
type User {
  id: ID!
  email: String! @id
  hash: String
    @generate(mutation: { add: false, update: false }, query: false)
    @default(add: { expr: "sha256(auth.email)" })
}
```

**Result:**

- `hash` is absent from `AddUserInput`, `UserPatch`, and the `User` output type.
- `@default` computes and stores it on creation.
- Clients never see or set `hash`.

## Interaction with `@default` and `@transform`

`@default` and `@transform` always fire regardless of `@generate` flags:

1. The mutation rewriter calls `@default` when `obj[field] == nil`.
2. Since a field hidden from `AddXxxInput` can never be in the client payload, it is always `nil` —
   so `@default` always fires, writing the generated value into the DQL nquads.
3. The field is stored in Dgraph and can be used in subsequent expr-lang expressions.

**Guarantee:** `@generate` on a field does **not** prevent internal mechanisms from writing to that
field. It only restricts what API clients can do.

> **Note on `query: false`:** hiding a field from the GraphQL output type does not delete it from
> Dgraph storage. The data remains in the database and can still be read via DQL or used in
> `@transform` / `@postValidate` expr-lang expressions.

## Validation Rules

1. **`@generate` on an `ID` field** → hard error. The primary key must always be accessible.

2. **`@generate(mutation: { add: false })` on a required (`!`) field with no `@default(add: ...)`**
   → hard error. The field can never be set on creation, leaving it permanently null in violation of
   the schema constraint. Add a `@default(add: ...)` expression to resolve this.

## Relationship to Type-level `@generate`

When `@generate` is placed on a **type** (e.g., `@generate(mutation: { add: false })`), it
suppresses the entire `addXxx` mutation for that type. When placed on a **field**, it only hides
that specific field within otherwise-generated mutation inputs and query output.

Both levels can coexist. Type-level directives take precedence: if the whole `addXxx` mutation is
disabled, field-level add flags have no additional effect.

## Generated Schema Impact

Given:

```graphql
type Post {
  id: ID!
  title: String!
  updatedAt: DateTime
    @generate(mutation: { add: false, update: false }, query: false)
    @default(add: { expr: "now()" }, update: { expr: "now()" })
  createdAt: DateTime @generate(mutation: { update: false }) @default(add: { expr: "now()" })
  score: Int @generate(mutation: { update: false }) @default(add: { value: "0" })
}
```

The generated schema will include:

```graphql
# AddPostInput: updatedAt absent; createdAt and score optional (because of @default)
input AddPostInput {
  title: String!
  createdAt: DateTime
  score: Int
}

# PostPatch: only title; all other fields restricted from updates
input PostPatch {
  title: String
}

# Post output type: updatedAt stripped; createdAt and score present
type Post {
  id: ID!
  title: String!
  createdAt: DateTime
  score: Int
}
```

## See also

- [`@hasInverse(immutable: true)`](./immutable_inverse.md) — write-once enforcement for _edge_
  fields (relationships between types)
- [`@default`](./old_value.md) — automatic field population on add and update
