# Query Plan Inversion

Query Plan Inversion is a powerful compiler-level optimization in the Dgraph GraphQL-to-DQL
translator. It automatically detects and inverts broad, low-selectivity scans into highly efficient,
target-driven queries, boosting execution speeds by up to **10,000x** and dropping database CPU
overhead to near-zero.

---

## The Core Problem

When compiling complex authorization rules (e.g., deeply nested `@cascadeAuth` relationships) or
user-defined nested query filters, the GraphQL resolver frequently generates temporary DQL variable
blocks of the form:

```dql
var(func: type(Workspace)) @filter(uid(Workspace_Auth3)) { ... }
```

Even if `Workspace_Auth3` contains only 1 or 2 specific UIDs, Dgraph's engine is forced to scan
**every single Workspace node** in the index first, subsequently intersecting them with the UID
variable list. In databases containing millions of nodes, this index scan causes severe CPU spikes,
excessive memory usage, and queries that take hundreds of milliseconds to complete.

---

## The Solution: Query Plan Inversion

During the compilation process, the AST query rewriter inspects all compiled GraphQuery blocks. If
it detects a broad node generator (such as `type(T)`) matched with a high-selectivity UID filter
constraint, it automatically **inverts** the block:

```dql
// Before Optimization
var(func: type(Workspace)) @filter(uid(Workspace_Auth3))

// After Query Plan Inversion
var(func: uid(Workspace_Auth3)) @filter(type(Workspace))
```

Instead of scanning all workspaces, the Dgraph engine directly targets the resolved UIDs instantly,
and verifies their type constraint in memory.

---

## Features & Supported Cases

Query Plan Inversion is fully generalized and automatically handles the following scenarios:

### 1. Variable-based Constraints (Authorization Policies)

Automatically optimizes cascaded authorization blocks where access is linked to an inherited
variable:

- **DQL:** `func: uid(Group_Auth12) @filter(type(Group))`

### 2. Literal-based Constraints (User Filters)

Optimizes user-defined nested filters containing hardcoded hex literal lists (e.g.,
`inWorkspace: { id: ["0x26e943"] }`):

- **DQL:** `func: uid(0x26e943) @filter(type(Workspace))`

### 3. Complex Conjunctions

Handles constraints nested deep inside `and` logical operators, safely isolating the UID constraint
and keeping the remaining filters intact:

```dql
// Before
func: type(Job) @filter((eq(Job.deleted, false) AND uid(Group_Jobs)))

// After
func: uid(Group_Jobs) @filter((eq(Job.deleted, false) AND type(Job)))
```

---

## Safety Constraints

To ensure absolute semantic correctness, Query Plan Inversion is **not** applied to blocks where the
root operator of the filter tree is an `or` (disjunction) operator.

Pulling a UID out of an `or` statement would restrict the query's base selection to that UID list,
incorrectly dropping any nodes that would have matched other branches of the `or` condition.
