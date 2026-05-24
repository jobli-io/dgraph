# Auth Variable Deduplication & Mutation Auth

Two related enhancements that ensure mutations use the correct permission set and generate compact
DQL with deduplicated auth variable blocks.

---

## 1. Operation-Aware Cascade Auth Rules

### Problem

Before this change, `@cascadeAuth` always propagated the authority type's **`query`** auth rule into
child types — regardless of whether the cascade was being applied to an `add`, `update`, or `delete`
operation. This meant:

- `addNote` mutation: Note's cascade auth checked Workspace/IAM with `QRY_PERMISSIONS`
- `updateNote` mutation: same `QRY_PERMISSIONS` again — not `UPD_PERMISSIONS`
- `deleteNote` mutation: same `QRY_PERMISSIONS` — not `ADM_PERMISSIONS`

Items could be created or deleted by callers who only held read-level IAM roles.

### How It Works Now

At schema compile-time (`expandCascadeAuth`), when generating the cascade rule for operation `op`
(one of `query`, `add`, `update`, `delete`), the engine now reads the authority type's
**op-specific** `@auth` rule:

| Child operation | Authority rule used                                   |
| --------------- | ----------------------------------------------------- |
| `query`         | `@auth(query: ...)`                                   |
| `add`           | `@auth(add: ...)`, fallback to `@auth(query: ...)`    |
| `update`        | `@auth(update: ...)`, fallback to `@auth(query: ...)` |
| `delete`        | `@auth(delete: ...)`, fallback to `@auth(query: ...)` |

The fallback to `query` means that authority types which only declare `@auth(query: ...)` (no
`add`/`update`/`delete` sections) continue to work as before — their query rule is propagated for
all operations.

### Graceful Fallback for Missing `@authVariables` Keys

When the authority's op-specific rule template uses `{{KEY}}` placeholders that the child type's
`@authVariables` doesn't declare, the engine falls back to the query rule for that cascade rather
than erroring. This allows child types to opt out of op-specific cascades by simply not declaring
the relevant keys.

**Example:**

```graphql
type Workspace
  @authVariables(
    vars: [
      { key: "QRY_PERMISSIONS", value: [_ALL, _WORKSPACE, READ, READ_WORKSPACE] }
      { key: "UPD_PERMISSIONS", value: [_ALL, _WORKSPACE, UPDATE] }
      { key: "ADM_PERMISSIONS", value: [] }
    ]
  )
  @auth(
    query: { rule: "...forRole: { permission: { in: {{QRY_PERMISSIONS}} } }..." }
    update: { rule: "...forRole: { permission: { in: {{UPD_PERMISSIONS}} } }..." }
    delete: { rule: "...forRole: { permission: { in: {{ADM_PERMISSIONS}} } }..." }
  )
```

```graphql
# Plugin only declares QRY_PERMISSIONS — it can't resolve ADM_PERMISSIONS or UPD_PERMISSIONS.
# The cascade engine falls back to Workspace's query rule for the add/update/delete cascades.
type Plugin implements WorkspaceMember
  @authVariables(vars: [{ key: "QRY_PERMISSIONS", value: [_ALL, READ, _PLUGIN] }])
  @cascadeAuthPolicy(aggregation: "or", skipBidirectional: true)
```

If Plugin declared `ADM_PERMISSIONS`, the engine would use Workspace's `delete` rule when building
Plugin's `delete` cascade auth. Without it, Plugin's add/update/delete cascade auth falls back to
Workspace's `query` rule — which is still protective.

### Schema Rule

> **Authority types must declare all `@authVariables` keys** that appear in their own `@auth` rules.
> Child types only need to declare keys that appear in the authority's **query** rule, plus any
> op-specific keys they want to opt into.

---

## 2. Auth Variable Deduplication (`authVarCache`)

### Problem

The mutation result payload query (e.g. `AddNotePayload.note`) generates DQL by calling the same
auth-rule rewriter used by `queryNote` — but previously each mutation path constructed a **fresh**
`authRewriter` with no cache. This caused identical auth variable blocks (e.g. `Workspace_Auth`,
`Group_Auth`) to be emitted once per OR-branch of the cascade tree, producing ~250 DQL variables
where `queryNote` produced ~36.

### Fix

Each mutation rewrite pass now initialises a shared `authVarCache` (`map[string]*authVar`) that is
passed to all `authRewriter` constructions within that mutation. Identical auth blocks (same cache
key) are emitted once and reused by `uid_in()` references in subsequent branches.

This affects:

- `AddRewriter` — payload result query
- `UpdateRewriter` — payload result query and pre-query
- `DeleteRewriter` — pre-query (for `@queryOnDelete` fields)
- Nested object rewriters

The cache is **scoped per mutation operation** (not shared between unrelated mutations) to ensure
correctness.

### Cache Key Design

The cache key for a leaf auth var block is derived from:

1. The authority type name (e.g. `Workspace`)
2. The DQL filter tree fingerprint (a stable string hash of the filter predicates and values)
3. The cascade inverse predicate (e.g. `WorkspaceMember.inWorkspace`)

The filter tree fingerprint is **branch-independent** — two OR branches that produce identical
Workspace filters reuse the same var block regardless of which branch they appear in.

---

## 3. `@authVariables` Rules

### Key Requirements

| Requirement                                                                                       | Detail                                                                      |
| ------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| All `{{KEY}}` placeholders in a type's own `@auth` rules must be declared in its `@authVariables` | Schema deploy will fail with an "unresolved placeholder" error              |
| Child types need keys for the auth rules they cascade from authority types                        | Only the keys used in the authority's op-specific rules the child opts into |
| Keys with empty `value: []` are valid                                                             | Produces an empty `in` list — effectively no match for that arm of the rule |

### Per-Operation `@authVariables`

The same `@authVariables` key map is used for all operations on a type. If you need different
permission sets per operation, declare separate keys:

```graphql
type MyType
  @authVariables(
    vars: [
      { key: "ADM_PERMISSIONS", value: [_ALL, _TYPE] }
      { key: "UPD_PERMISSIONS", value: [_ALL, _TYPE, UPDATE, UPDATE_TYPE] }
      { key: "QRY_PERMISSIONS", value: [_ALL, _TYPE, UPDATE, UPDATE_TYPE, READ, READ_TYPE] }
    ]
  )
  @auth(
    add: { rule: "...in: {{ADM_PERMISSIONS}}..." }
    update: { rule: "...in: {{UPD_PERMISSIONS}}..." }
    delete: { rule: "...in: {{ADM_PERMISSIONS}}..." }
    query: { rule: "...in: {{QRY_PERMISSIONS}}..." }
  )
```

---

## 4. Delete Mutation Pre-Query Ordering

The delete mutation pipeline executes in this order:

1. **Pre-query** — fetches `@queryOnDelete` / `@oldValue` fields from nodes _before_ deletion
2. **Main delete upsert** — removes the nodes from the graph
3. **Payload assembly** — uses pre-query results (nodes no longer exist in graph)

This ordering guarantees that `@queryOnDelete` fields contain real data, not empty results from
querying already-deleted nodes.
