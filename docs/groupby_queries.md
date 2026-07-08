# `groupByXxx` Queries

`groupByXxx` queries let you aggregate data across **buckets** defined by the values of one or more
fields, without fetching individual nodes. They are the analytical counterpart to the flat
`aggregateXxx` query and are powered natively by Dgraph's `@groupby` DQL feature.

## Auto-generated types

For each object or interface type `Xxx` that has at least one **groupable field**, the engine
automatically generates:

| Name                  | Kind                | Purpose                                      |
| --------------------- | ------------------- | -------------------------------------------- |
| `DateTimeGranularity` | Enum (shared, once) | Interval granularity for DateTime bucketing  |
| `XxxGroupableField`   | Enum                | The fields you can group by                  |
| `XxxGroupBySpec`      | Input               | One grouping key (field + optional interval) |
| `XxxGroupByResult`    | Object              | One bucket: key field(s) + aggregate values  |
| `groupByXxx`          | Root query          | The entry point                              |

### `DateTimeGranularity` (shared enum)

```graphql
enum DateTimeGranularity {
  year
  month
  day
  hour
}
```

### `XxxGroupableField`

An enum containing all scalar fields of `Xxx` that produce meaningful grouping buckets. **Edge/UID
fields are excluded** — they produce one bucket per node and are therefore useless for analytics.

```graphql
enum NoteGroupableField {
  status
  priority
  title
  createdAt
}
```

### `XxxGroupBySpec`

```graphql
input NoteGroupBySpec {
  # The field to group by (required).
  field: NoteGroupableField!

  # For DateTime fields only: floor timestamps to this granularity bucket.
  # Requires @search(by: [year|month|day|hour]) on the field in the schema.
  by: DateTimeGranularity

  # For DateTime fields with `by`: UTC-offset timezone string (e.g. "+05:30").
  # Timestamps are shifted by this offset before the floor operation.
  # Use "Z" or omit for UTC.
  tz: String
}
```

### `XxxGroupByResult`

Combines the group key fields with all aggregate fields from `XxxAggregateResult` (excluding `count`
which is always included):

```graphql
type NoteGroupByResult {
  # Group key(s) — one field per spec entry in the groupBy argument
  status: String
  priority: Int
  title: String
  createdAt: DateTime

  # Aggregates (same fields as NoteAggregateResult)
  count: Int!
  titleMin: String
  titleMax: String
  priorityMin: Int
  priorityMax: Int
  prioritySum: Int
  priorityAvg: Float
  # ... and so on for each numeric/string field
}
```

### Root query

```graphql
groupByNote(
  filter: NoteFilter
  groupBy: [NoteGroupBySpec!]!
): [NoteGroupByResult]
```

---

## Basic usage

### Count notes by status

```graphql
{
  groupByNote(groupBy: [{ field: status }]) {
    status
    count
  }
}
```

**Response:**

```json
{
  "groupByNote": [
    { "status": "DRAFT", "count": 12 },
    { "status": "PUBLISHED", "count": 47 },
    { "status": "ARCHIVED", "count": 3 }
  ]
}
```

### Multiple grouping keys

```graphql
{
  groupByNote(groupBy: [{ field: status }, { field: priority }]) {
    status
    priority
    count
    titleMin
  }
}
```

### With a filter

```graphql
{
  groupByNote(filter: { forWorkspace: { id: { eq: "ws-123" } } }, groupBy: [{ field: status }]) {
    status
    count
  }
}
```

---

## DateTime interval bucketing

The `by` argument floors timestamps to a coarser granularity so that all events within the same hour
/ day / month / year land in the same bucket.

```graphql
{
  groupByNote(groupBy: [{ field: createdAt, by: day }]) {
    createdAt # returned as ISO 8601 floor: "2024-01-15T00:00:00Z"
    count
  }
}
```

### Available granularities

| `by` value | Example bucket key     | Notes                             |
| ---------- | ---------------------- | --------------------------------- |
| `year`     | `2024-01-01T00:00:00Z` | Rolled to Jan 1 00:00 of the year |
| `month`    | `2024-03-01T00:00:00Z` | Rolled to the 1st of the month    |
| `day`      | `2024-03-15T00:00:00Z` | Rolled to midnight                |
| `hour`     | `2024-03-15T09:00:00Z` | Rolled to the start of the hour   |

> **Important:** `by` is **only valid for `DateTime` fields**. The request validator will reject
> `by` on `String`, `Int`, `Float`, or `Boolean` fields with an error:
>
> ```
> groupBy: `by` is only valid for DateTime fields; field "status" has type String in type Note.
> ```

### `@search` index requirement

To use `by`, the DateTime field **must** have the matching `@search` index in the schema:

| `by` value | Required index         |
| ---------- | ---------------------- |
| `year`     | `@search(by: [year])`  |
| `month`    | `@search(by: [month])` |
| `day`      | `@search(by: [day])`   |
| `hour`     | `@search(by: [hour])`  |

Example schema:

```graphql
type Note {
  id: ID!
  title: String @search(by: [term, hash])
  status: String @search(by: [hash])
  createdAt: DateTime @search(by: [day]) # enables groupBy: [{ field: createdAt, by: day }]
}
```

> **Warning:** If the `@search` index is absent, the engine will still execute the query (the
> tokenizer is applied in-memory during response processing) but will perform a **full predicate
> scan**. For large datasets this can be very slow. Always add the matching `@search` index when
> using `by`.

---

## Timezone-aware bucketing

By default, timestamps are floored in **UTC**. Supply `tz` to shift timestamps to a local timezone
before flooring:

```graphql
{
  groupByNote(groupBy: [{ field: createdAt, by: day, tz: "+05:30" }]) {
    createdAt # returned as UTC equivalent of the local midnight
    count
  }
}
```

`tz` is a UTC offset string: `"Z"`, `"+00:00"`, `"-07:00"`, `"+05:30"`, etc.

> **Note:** The bucket keys in the response are always returned as **UTC ISO 8601 timestamps** (e.g.
> `"2024-03-14T18:30:00Z"` for midnight IST `+05:30`). Client code must apply the reverse offset to
> display a human-readable local date.

> **Important:** `tz` is only valid when `by` is also specified. Supplying `tz` alone is rejected:
>
> ```
> groupBy: `tz` (timezone offset) is only valid when `by` is also specified for field "createdAt"
> ```

---

## Interface types

`groupByXxx` is generated for **interface types** just as for concrete types. Grouping is over all
nodes that implement the interface (equivalent to `func: type(Xxx)` in DQL), subject to your normal
auth rules.

```graphql
interface Manageable {
  id: ID!
  status: String @search(by: [hash])
}

type Job implements Manageable { ... }
type Placement implements Manageable { ... }
```

Generated query:

```graphql
groupByManageable(
  filter: ManageableFilter
  groupBy: [ManageableGroupBySpec!]!
): [ManageableGroupByResult]
```

---

## Controlling generation with `@generate`

By default, `groupByXxx` is generated for every type with at least one groupable field. Use
`@generate(groupBy: false)` to suppress it:

```graphql
type InternalLog @generate(groupBy: false) {
  id: ID!
  level: String
  message: String
}
```

---

## Validation rules

The engine validates `groupByXxx` arguments at **request-parse time** (before DQL is generated):

| Constraint                            | Error message                                                                      |
| ------------------------------------- | ---------------------------------------------------------------------------------- |
| `by` on non-DateTime field            | `groupBy: 'by' is only valid for DateTime fields; field "X" has type Y in type Z.` |
| `by` without matching `@search` index | `groupBy: 'by: day' on field "X" in type Y requires @search(by: [day]).`           |
| `tz` without `by`                     | `groupBy: 'tz' is only valid when 'by' is also specified for field "X".`           |

---

## Notes and limitations (v1)

- **Sorting is not supported.** Buckets are returned in Dgraph's natural order. Client-side sorting
  is recommended for UI display.
- **Pagination (`first`, `offset`) is not supported.** All buckets are returned.
- **Nested groupBy** (grouping within a nested relationship) is supported for single-valued edges
  (see below). List edges cannot be used as intermediate navigation steps.
- **Variable arguments:** When the `groupBy` list is provided as a GraphQL variable (e.g.
  `$spec: [NoteGroupBySpec!]!`), the `by`/`tz` constraints cannot be validated at parse time and are
  enforced only during query execution.

---

## Nested field groupBy

You can group by a scalar field on a related type reachable via a single-valued edge using the
`XxxGroupByField` input object notation. Set each intermediate edge to a nested object and the
terminal scalar to `true`.

### Example: group companies by their status name

```graphql
{
  groupByCompany(groupBy: [{ field: { hasStatus: { name: true } } }]) {
    count
    groupKeys {
      path # "hasStatus.name"
      value # e.g. "Active"
    }
  }
}
```

**Response:**

```json
{
  "groupByCompany": [
    { "count": 12, "groupKeys": [{ "path": "hasStatus.name", "value": "Active" }] },
    { "count": 3, "groupKeys": [{ "path": "hasStatus.name", "value": "Inactive" }] }
  ]
}
```

### How it works

For nested specs, the engine traverses the edge path from the root set and collects the leaf scalar
values into a **value variable** inside an auxiliary `var()` block:

1. **Leaf-value collection:** A `var()` block traverses the edge path from the authenticated root
   set, extracts the leaf scalar value as a child value variable (e.g.,
   `__gby_0_c as StatusIfc.name`), and aggregates it up to the root level using a parent value
   variable assignment (`__gby_0 as max(val(__gby_0_c))`).
2. **Value-variable `@groupby`:** The main `groupByXxx` query runs on the root type UIDs
   (`CompanyRoot`) and natively groups by the parent value variable using `@groupby(val(__gby_0))`.

The DQL emitted for the example above is:

```dql
var(func: uid(CompanyRoot)) {
  Company.hasStatus {
    __gby_0_c as StatusIfc.name
  }
  __gby_0 as max(val(__gby_0_c))
}
groupByCompany(func: uid(CompanyRoot)) @groupby(val(__gby_0)) {
  count(uid)
}
```

> [!TIP] Because the main `groupByXxx` query remains rooted at the root type (e.g., `CompanyRoot`),
> **all root-level aggregate fields (`avg`, `min`, `max`, `sum`, `count`) function natively and are
> fully supported** for nested field groupBy!

> [!IMPORTANT] **Multiple nested specs** are fully and concurrently supported in the same `groupBy`
> list. You can group by multiple separate nested fields (e.g., both `hasPrimaryGroup.name` and
> `createdBy.email`) in a single query. Each nested spec will generate its own auxiliary value
> variable block.
