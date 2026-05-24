# @oldValue Directive

Marks a field so that its **pre-mutation value** is fetched from the database before an `add` or
`update` mutation runs. The captured value is then available inside `@default`, `@transform`,
`@validate`, and `@cascadeDelete` CEL expressions as the `before` variable.

---

## Signature

```graphql
directive @oldValue(
  fields: [String!] # dot-separated sub-field paths to pre-fetch (edge fields only)
  first: Int # limit pre-fetched results for list-type edges
  sort: String # order results before capping; prefix with "-" for descending
) on FIELD_DEFINITION
```

| Form                         | When to use                                                              |
| ---------------------------- | ------------------------------------------------------------------------ |
| `@oldValue` _(no arguments)_ | Scalar and enum fields — captures the raw stored value                   |
| `@oldValue(fields: [...])`   | Edge (object) fields — selects specific nested sub-fields to pre-fetch   |
| `@oldValue(first: N)`        | List-type edge fields — cap the number of edges fetched in the pre-query |
| `@oldValue(sort: "field")`   | List-type edge fields — order edges before the `first` cap is applied    |

---

## How It Works

When any field tagged with `@oldValue` is present on a type, the mutation pipeline issues an **extra
pre-query** against Dgraph to retrieve those field values for every node being mutated. The data is
then injected into the CEL evaluation context as `before`, a `map[string]interface{}` keyed by field
name.

```
Mutation arrives
  └─> pre-query all @oldValue fields           ← extra Dgraph read
  └─> CEL expressions evaluate (before is now populated)
  └─> DQL write mutation executes
```

The `before` map reflects the **state of the node in the database** at the point the mutation was
received — it is independent of the incoming `input`.

---

## CEL Variables

Inside any `@default`, `@transform`, `@validate`, or `@cascadeDelete(filter:…)` expression, the
following variables relate to `@oldValue`:

| Variable | Type  | Description                                                                                                                    |
| -------- | ----- | ------------------------------------------------------------------------------------------------------------------------------ |
| `before` | `map` | Full pre-mutation state of the node. Keyed by field name. Only fields annotated with `@oldValue` are guaranteed to be present. |
| `after`  | `map` | Merged view of `before` + incoming `input` — the expected post-mutation state.                                                 |
| `new`    | `map` | Only the fields that _changed_ relative to `before`.                                                                           |

---

## Usage

### Scalar / Enum field (no arguments)

```graphql
type User {
  status: UserStatus @search(by: [hash]) @oldValue
  email: String @search(by: [hash]) @oldValue
}
```

`before.status` and `before.email` are available in all CEL expressions on this type.

---

### Edge field with `fields` argument

Use `@oldValue(fields: [...])` on an edge field to select which nested scalar fields to pre-fetch.
Each entry in `fields` is a **dot-separated path** from the edge field's type downward.

#### One level deep

```graphql
type Company {
  hasAddress: [Address] @cascadeDelete @oldValue(fields: ["formattedAddress", "city"])
}
```

Inside a CEL expression on `Company`, `before.hasAddress` is a list of maps:

```cel
# true if the first address's city changed
before.hasAddress[0].city != after.hasAddress[0].city
```

#### Two levels deep

```graphql
type Order {
  owner: User @oldValue(fields: ["profile.firstName", "profile.lastName"])
}
```

The pre-query fetches `User → Profile → firstName/lastName`. Because the DQL query aliases every
level to its GraphQL field name, the result structure in `before` is:

```
before.owner = {
  "uid": "0x1a",
  "profile": {
    "uid": "0x2b",
    "firstName": "Alice",
    "lastName": "Smith"
  }
}
```

CEL access at every level uses the bare GraphQL field name:

```cel
before.owner.profile.firstName   // "Alice"
before.owner.profile.lastName    // "Smith"
```

#### Three levels deep

```graphql
type Workspace {
  owner: Organization @oldValue(fields: ["billing.contact.email", "billing.contact.name"])
}
```

The generated DQL aliases every level:

```dql
owner : Workspace.owner {
  uid
  billing : Organization.billing {
    uid
    contact : Billing.contact {
      uid
      email : Contact.email
      name  : Contact.name
    }
  }
}
```

So in CEL, all levels use bare GraphQL field names:

```cel
before.owner.billing.contact.email
before.owner.billing.contact.name
```

#### Mixing depths in one `fields` list

Paths do not need to have the same depth. A single `@oldValue` can fetch both direct scalars and
deeply nested sub-fields simultaneously:

```graphql
type JobAd {
  company: Company
    @oldValue(
      fields: [
        "name" # depth 1 — Company.name
        "address.city" # depth 2 — Company → Address → city
        "address.country.isoCode" # depth 3 — Company → Address → Country → isoCode
      ]
    )
}
```

All three paths share the DQL sub-query for `address` — the system merges them into a single nested
selection automatically. Because every level is aliased, `before.company` resembles:

```
before.company = {
  "uid": "0x10",
  "name":    "Acme",
  "address": {
    "uid": "0x11",
    "city": "Sydney",
    "country": {
      "uid": "0x12",
      "isoCode": "AU"
    }
  }
}
```

CEL access at all levels is via bare GraphQL field names:

```cel
before.company.name                        // "Acme"
before.company.address.city                // "Sydney"
before.company.address.country.isoCode    // "AU"
```

---

### `first` and `sort` — controlling list-type edge pre-queries

When an edge field is a **list**, the pre-query can return a large number of linked nodes. Use
`first` and `sort` to bound and order the result before it is captured as `before`.

| Argument | Type     | Description                                                                                                                      |
| -------- | -------- | -------------------------------------------------------------------------------------------------------------------------------- |
| `first`  | `Int`    | Maximum number of edges to pre-fetch. Must be a positive integer.                                                                |
| `sort`   | `String` | Dgraph field name to order by. Prefix with `"-"` for descending order (e.g. `"-createdAt"`). Applied _before_ `first` truncates. |

Both arguments are **only valid on list-type edge fields**. Using them on a scalar, enum, or
single-value edge field is a schema validation error.

```graphql
type Candidate {
  # Pre-fetch only the 5 most-recent application history entries.
  applicationHistory: [ApplicationEvent]
    @oldValue(fields: ["status", "createdAt"], sort: "-createdAt", first: 5)
}
```

The resulting `before.applicationHistory` array will contain at most 5 entries, ordered
newest-first, usable in any CEL expression:

```cel
# True if the most-recent history entry was "APPLIED"
before.applicationHistory[0].status == "APPLIED"
```

Using `sort` alone without `first` is valid and simply orders the full list:

```graphql
type Invoice {
  lineItems: [LineItem] @cascadeDelete @oldValue(fields: ["amount", "description"], sort: "amount")
}
```

Using `first` alone without `sort` returns an **unordered** subset — the order is determined by
Dgraph's internal traversal and is not guaranteed:

```graphql
type Workspace {
  members: [User] @oldValue(fields: ["sId"], first: 10)
}
```

---

### Checking a status transition in `@validate`

```graphql
type JobAd {
  status: JobAdStatus @search @oldValue
  title: String!
    @search(by: [hash, term])
    @oldValue
    @validate(
      expr: """
      action == "add" ||
      before.status != "PUBLISHED" ||
      size(value) >= 5
      """
      reason: "Published job ads must have a title of at least 5 characters"
    )
}
```

---

### Computing a change-aware default in `@default`

```graphql
type Message {
  lastReadAt: DateTime @oldValue
  readCount: Int @default(update: { expr: "(before.readCount ?? 0) + 1" })
}
```

---

### Driving a conditional `@transform`

```graphql
type Subscription {
  plan: SubscriptionPlan @search @oldValue
  renewedAt: DateTime
    @transform(update: { expr: "before.plan != input.plan ? now() : before.renewedAt" })
}
```

---

### `before` inside `@cascadeDelete(filter:…)`

The `before` variable in a cascade-delete filter represents the pre-delete values of the **child**
node being evaluated (not the parent). Use `@oldValue` on the child type to populate it.

```graphql
type Campaign {
  drafts: [Draft!] @cascadeDelete(filter: "value.published == false")
}

type Draft {
  published: Boolean @search @oldValue
  title: String
}
```

---

## Combining with Other Directives

`@oldValue` is commonly composed with `@default`, `@transform`, and `@validate`:

```graphql
type UserInvitation {
  status: UserInvitationStatus
    @search(by: [hash])
    @default(add: { value: "PENDING" })
    @oldValue
    @validate(
      expr: """
      action == "add" ||
      before.status != "ACCEPTED"
      """
      reason: "Accepted invitations cannot be modified"
    )
}
```

---

## Nested field access: key reference

The DQL query generated for `@oldValue(fields: [...])` applies a `gqlName : DgraphPredicate` alias
at **every level** of the selection tree — both for edge nodes and scalar leaves. Because Dgraph
returns the aliased name as the JSON key, **all levels use bare GraphQL field names** in the
`before` map.

| Nesting level  | Key in `before`   | Example                          |
| -------------- | ----------------- | -------------------------------- |
| Top-level edge | Bare GraphQL name | `before.owner`                   |
| Level-2 scalar | Bare GraphQL name | `before.owner.name`              |
| Level-2 edge   | Bare GraphQL name | `before.owner.profile`           |
| Level-3 scalar | Bare GraphQL name | `before.owner.profile.firstName` |
| Any depth      | Bare GraphQL name | `before.a.b.c.d`                 |

The pattern for any depth:

```cel
before.<field1>.<field2>.<field3>...<leafField>
```

> **List fields:** If any edge in the path is a list type, that level is a CEL list and requires
> indexing:
>
> ```cel
> # owner is a scalar edge, addresses is a list edge
> before.owner.addresses[0].city
>
> # owners is a list edge
> before.owners[0].profile.firstName
> ```

---

## Validation Rules

| Violation                                                         | Error                                                                                                           |
| ----------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `@oldValue(fields: [...])` on a scalar or enum field              | `@oldValue(fields: [...]) is not allowed on scalar or enum fields; use @oldValue without arguments on scalars.` |
| A path in `fields` references an unknown field                    | `@oldValue path "…" references unknown field "…" on type …`                                                     |
| A path traverses through a scalar/enum mid-segment                | `@oldValue path "…" cannot traverse through scalar/enum field "…" on type …`                                    |
| A path creates a cycle in the type graph                          | `@oldValue path "…" contains a cycle at type …`                                                                 |
| `first` or `sort` on a scalar, enum, or single-value edge field   | `@oldValue(first: ...) is only allowed on list-type edge fields.`                                               |
| `first` is not a positive integer                                 | `@oldValue first must be a positive integer, got "…".`                                                          |
| `sort` references a field that does not exist on the element type | `@oldValue sort references unknown field "…" on type ….`                                                        |
| `sort` references a non-orderable (edge/object) field             | `@oldValue sort field "…" on type … must be a scalar or enum (orderable).`                                      |
| `sort` value is empty after stripping the `"-"` prefix            | `@oldValue sort value must not be empty after stripping the "-" prefix.`                                        |

---

## Placement Constraints

- Place on **field definitions** only (`on FIELD_DEFINITION`).
- The no-argument form `@oldValue` works on any field type (scalar, enum, or edge).
- The `fields` argument is **only valid on edge (object) fields** — it selects sub-fields to
  pre-fetch from the linked object.
- `@oldValue` has no effect on `@custom` or `@lambda` fields (no Dgraph pre-query is executed for
  those).
- **`id` path segments are supported** — the `id` field (type `ID!`) maps to Dgraph's `uid`.
  Declaring `"id"` in a path generates `id : uid` in the DQL query, so the result carries both
  `"uid"` and `"id"` with the same value:

  ```graphql
  type Organization {
    id: ID!
    name: String
  }
  type Workspace {
    id: ID!
    owner: Organization @oldValue(fields: ["id", "name"])
  }
  ```

  ```cel
  before.owner.id    // ✅ "0x1abc" — the Organization's uid
  before.owner.uid   // ✅ same value, always present without declaration
  before.owner.name  // ✅ the Organization's name
  ```

---

## See also

- [`@hasInverse(immutable: true)`](./immutable_inverse.md) — write-once enforcement for edge fields;
  uses the same pre-query mechanism internally
- [Field-level `@generate`](./field_level_generate.md) — hide scalar fields from mutation inputs or
  query output
