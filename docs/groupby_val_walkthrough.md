# Walkthrough — Native Value Variable `@groupby(val(varName))` Implementation

We have completed the native core-engine implementation of **Option 1**: allowing
`@groupby(val(varName))` directly inside Dgraph's `@groupby` directive. This natively supports
nested field grouping while keeping the query rooted at the original parent level, thus fully
enabling all root-level aggregates/metrics (`avg`, `min`, `max`, `sum`, `count`).

---

## Changes Implemented

### 1. DQL Lexer & Parser Layer

- **[dql/parser.go](file:///Users/idowuayoola/Documents/jobli/dgraph/dql/parser.go)**:
  - Updated `GroupByAttr` to support `VarName` and `IsValueVar`.
  - Updated `parseGroupby` to parse `val(varName)` expressions inside `@groupby` directives and
    register them in `gq.NeedsVar`.
  - **Parser Dependency Bug Fix**: Updated `collectVarsInsideGroupBy` so that variables defined
    inside `@groupby` blocks (e.g. `a as count(uid)`) are correctly added to both `v.Defines` and
    `v.Needs`. This ensures that they are correctly registered as defined and optionally consumed
    without failing the balanced dependency checks when they are used in external query blocks
    (resolving the `Some variables are used but not defined` test failures).

### 2. DQL Formatting & Emission Layer

- **[graphql/dgraph/graphquery.go](file:///Users/idowuayoola/Documents/jobli/dgraph/graphql/dgraph/graphquery.go)**:
  - Updated `@groupby` string serializer to emit `val(attr.VarName)` instead of `Attr` when
    `IsValueVar` is true.

### 3. Query Planner & Child SubGraph Setup

- **[query/query.go](file:///Users/idowuayoola/Documents/jobli/dgraph/query/query.go)**:
  - Ensured child subgraphs for `@groupby` copy `it.VarName` to `Params.Var` for groupby subgraphs.

### 4. Grouping Engine

- **[query/groupby.go](file:///Users/idowuayoola/Documents/jobli/dgraph/query/groupby.go)**:
  - Added `doneVars` context to the `formResult` signature.
  - In `formResult`, extracted value variables directly from the evaluated
    `doneVars[child.Params.Var].Vals` map when grouping on a value variable.

### 5. GraphQL Rewriter & Response Transformer

- **[graphql/resolve/query_rewriter.go](file:///Users/idowuayoola/Documents/jobli/dgraph/graphql/resolve/query_rewriter.go)**:
  - Implemented `buildValueVarBlock` to emit variable assignment DQL blocks for leaf scalar
    predicates (e.g., `__gby_0 as StatusIfc.name`).
  - Updated `groupByQuery` to use `buildValueVarBlock` and output `@groupby(val(__gby_0))` on the
    root query block.
- **[graphql/resolve/query.go](file:///Users/idowuayoola/Documents/jobli/dgraph/graphql/resolve/query.go)**:
  - Updated `buildGroupByPathMap` to record `"val(__gby_i)"` → full GraphQL dot path mappings.
  - Updated `completeGroupByResult` to detect `val(...)` group-by keys in raw DQL responses and
    resolve them via `pathMap`.
- **[graphql/resolve/groupby_result_test.go](file:///Users/idowuayoola/Documents/jobli/dgraph/graphql/resolve/groupby_result_test.go)**:
  - Added a value-variable-based groupby test case verifying raw `val(__gby_0)` results map back
    correctly.

### 6. Feature Documentation

- **[docs/groupby_queries.md](file:///Users/idowuayoola/Documents/jobli/dgraph/docs/groupby_queries.md)**:
  - Updated the "Nested field groupBy" documentation to explain the new value-variable native
    groupby implementation and highlight that all root-level aggregates/metrics (`avg`, `min`,
    `max`, `sum`, `count`) are now fully supported.

---

## Verification Results

### Automated Unit Tests

All unit tests compile and pass successfully:

- **DQL Parser Unit Tests**:

  ```bash
  go test ./dql/... -count=1
  ```

  - **Result**: `ok  github.com/hypermodeinc/dgraph/v25/dql  0.361s` (Passes completely including
    `TestParseGroupbyRoot`, `TestParseGroupbyWithCountVar`, and `TestParseGroupbyWithMaxVar`).

- **Query Engine Unit Tests**:

  ```bash
  go test ./query/... -count=1
  ```

  - **Result**: `ok  github.com/hypermodeinc/dgraph/v25/query  0.805s` (Passes completely).

- **GraphQL Resolve & Schema Unit Tests**:
  ```bash
  go test ./graphql/resolve/... -count=1
  ```
  - **Result**: `ok  github.com/hypermodeinc/dgraph/v25/graphql/resolve  2.306s` (Passes
    completely).
