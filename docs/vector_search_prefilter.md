# Vector Search Pre-Filtering (`similar_to` + `@auth` / Hard Filters)

Vector Search Pre-Filtering ensures that GraphQL `@auth` rules and deterministic ("hard") filters
execute **before** `similar_to` (`similarToText`, `similarToVector`, `similarToId`) evaluates vector
similarity, guaranteeing accurate per-tenant `topK` recall without manual `topK` over-fetching.

---

## The Core Problem

Dgraph maintains a global HNSW vector index per predicate (e.g. `JobCategory.search` or
`Candidate.search`). Previously, the GraphQL query rewriter emitted an unfiltered `similar_to`
variable block and intersected its output with `@auth` and hard filters afterward:

```dql
# Legacy (post-filtered): global top-3 across all workspaces, then filtered by workspace @auth
var(func: similar_to(JobCategory.search, 3, "[...]")) {
  queryJobCategory_search_similar as uid
}
JobCategory_1 as var(func: type(JobCategory)) @filter(uid(queryJobCategory_search_similar))
JobCategoryRoot as var(func: uid(JobCategory_1)) @filter(uid(JobCategory_Auth2))
```

In multi-tenant schemas where many workspaces hold similar or identical embeddings (for example,
~397 standard `JobCategory` nodes per workspace), the top `K` global HNSW neighbors almost always
belong to other workspaces. Post-filtering those `K` UIDs through `@auth` drops them all and returns
`[]`.

---

## The Solution

### 1. DQL `uids: uid(<var>)` Named Option on `similar_to`

`similar_to` now accepts an optional `uids: uid(<varName>)` named parameter alongside `ef` and
`distance_threshold`:

```dql
var(func: similar_to(JobCategory.search, 3, "[...]", uids: uid(JobCategoryRoot_PreVec))) {
  queryJobCategory_search_similar as uid
}
```

- **Parser (`dql/parser.go`)**: Registers `<varName>` as a `UidVar` dependency in `NeedsVar` so
  Dgraph's topological query scheduler executes the pre-filter blocks before `similar_to`.
- **Coordinator (`query/query.go`)**: Populates `sg.SrcUIDs` from `<varName>`. When `<varName>`
  resolves to 0 UIDs, `ProcessGraph` short-circuits the worker RPC with an empty result while
  preserving downstream variable population.
- **Worker & HNSW (`worker/task.go`, `tok/hnsw/persistent_hnsw.go`, `tok/hnsw/search_layer.go`)**:
  - When `len(AllowedUIDs) <= 4096` (typical workspace-scoped sets such as `JobCategory`, `McpTool`,
    `MemoryEpisode`, or filtered `Candidate`/`Job` subsets), vectors for `AllowedUIDs` are scored
    directly in memory for **100% exact recall** (skipping nodes that do not yet have an embedding).
  - For larger allowed sets (`> 4096`), HNSW routes upper layers greedily without filter starvation
    and evaluates the UID filter on the bottom layer with exact `filteredOut` heap accounting and
    widened candidate-queue capacity while collecting `expectedNeighbors` allowed results.

### 2. GraphQL Query Rewriter (`applySimilarToPreFilter`)

After `addAuthQueries` runs (across root `query*`, `aggregate*`, `@groupby`, and nested `REVERSE` /
`FORWARD` relation filters in `graphql/resolve/query_rewriter.go`), `applySimilarToPreFilter`
rewires the DQL DAG:

1. **With dynamic `@auth` scaffolding**:
   - Strips `uid(<similarVar>)` out of `varQry.Filter` so `<Type>_1` evaluates `type(T)` + hard
     filters first.
   - Evaluates `@auth` rules into `<Type>Root_PreVec` (without `first`/`offset` truncation).
   - Passes `uids: uid(<Type>Root_PreVec)` into `similar_to(...)`.
   - Narrows `<Type>Root` from `uid(<Type>Root_PreVec)` with `@filter(uid(<similarVar>))` (and
     `first`/`offset`/`order`), ensuring downstream consumers—including child `selectionAuth`
     queries, aggregations, and `groupBy`—only touch the final `topK` results.
2. **Without dynamic `@auth` scaffolding**:
   - When hard filters accompany `similar_to`, emits `<similarVar>_allowed` with those hard filters
     and passes `uids: uid(<similarVar>_allowed)` into `similar_to(...)`.
