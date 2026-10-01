# @bypassAuth Directive

The `@bypassAuth` directive selectively or conditionally bypasses authorization rules (both standard
`@auth` and cascade-auth `@cascadeAuth`) for child selections reached through a specific edge in the
GraphQL schema.

---

## 1. Directive Signature

```graphql
directive @bypassAuth(except: [String!], if: String) on FIELD_DEFINITION
```

### Arguments

| Argument | Type        | Description                                                                                                                                                                                                     |
| :------- | :---------- | :-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `if`     | `String`    | _(Optional)_ An [expr-lang](https://expr-lang.org) boolean expression evaluated against request auth variables (`auth`). Bypasses auth only when expression evaluates to `true`. Validated at schema load time. |
| `except` | `[String!]` | _(Optional)_ List of type names, interfaces, or relationship fields that remain protected even when bypass is active.                                                                                           |

---

## 2. Behavior & Evaluation Flow

1. **Schema-Load Validation**:

   - The `if` expression is compiled using `expr.Compile` against a typed `exprEvaluationContext`.
   - If the expression has invalid syntax or references undeclared identifiers outside the
     expression environment, schema initialization fails with an actionable compilation error.
   - Any entries in `except` are verified to be valid types, interfaces, or relationship fields on
     the target type.

2. **Runtime Evaluation**:
   - When rewriting a query traversing an edge with `@bypassAuth`:
     - If `if` is omitted: auth is bypassed unconditionally.
     - If `if` is specified: the compiled expression is executed with the request's JWT claims in
       `auth` (e.g. `auth.role`, `auth.userId`).
       - If expression evaluates to `true`: auth bypass is enabled for this edge traversal.
       - If expression evaluates to `false` (or user is unauthenticated): auth is **not** bypassed,
         and standard edge auth rules apply.
   - When auth bypass is enabled:
     - If `except` is omitted: all authorization filters for the child edge are bypassed
       (`rbac = schema.Positive`).
     - If `except` is provided: all child auth rules are bypassed **except** those targeting the
       types/interfaces listed in `except` (`rbac = schema.Uncertain`, filtered rule node).

---

## 3. Schema Examples

### Basic Conditional Bypass

Bypass child `hasUser` authorization rules only if the authenticated user has an `ADMIN` role claim:

```graphql
type Workspace
  @auth(
    query: { rule: "query($ws: String!) { queryWorkspace(filter: { name: { eq: $ws } }) { id } }" }
  ) {
  id: ID!
  name: String! @search(by: [exact])
  hasUser: [User!] @bypassAuth(if: "auth.role == 'ADMIN'")
}

type User
  @auth(
    query: { rule: "query($sub: String!) { queryUser(filter: { userId: { eq: $sub } }) { id } }" }
  ) {
  id: ID!
  userId: String @search(by: [exact])
}
```

- **When `role == 'ADMIN'`**: A query for `hasUser` returns all users connected to the workspace
  without applying individual `userId` filters.
- **When `role == 'MEMBER'` (or unauthenticated)**: The `if` condition evaluates to `false`. The
  `hasUser` edge strictly enforces the `userId: { eq: $sub }` filter.

---

### Conditional Bypass with Exceptions

Bypass inherited interface rules for admins while continuing to enforce direct type rules:

```graphql
interface Member
  @auth(
    query: { rule: "query($role: String!) { queryMember(filter: { role: { eq: $role } }) { id } }" }
  ) {
  id: ID!
  role: String @search(by: [exact])
  inWorkspace: Workspace!
}

type Workspace
  @auth(
    query: { rule: "query($ws: String!) { queryWorkspace(filter: { name: { eq: $ws } }) { id } }" }
  ) {
  id: ID!
  name: String! @search(by: [exact])
  # When ADMIN, bypass Member interface rule, but retain direct User rules
  hasUser: [User!] @bypassAuth(if: "auth.role == 'ADMIN'", except: ["User"])
}

type User implements Member
  @auth(
    interfacePolicy: [{ interface: "Member", merge: "or" }]
    query: { rule: "query($sub: String!) { queryUser(filter: { userId: { eq: $sub } }) { id } }" }
  ) {
  id: ID!
  role: String @search(by: [exact])
  userId: String @search(by: [exact])
  inWorkspace: Workspace!
}
```

---

## 4. Expression Environment

Expressions configured in `if` have access to:

- `auth`: `map[string]any` representing the verified JWT claims and auth variables (e.g.
  `auth.role`, `auth.sub`, `auth.email`, `auth["custom:tenant"]`).
- `__typename`: target type name string.
- `action`: `"query"`.
- Built-in helper functions from `ExprFuncs`: `uuid()`, `sha256()`, `callLambda()`, `error()`,
  `log()`, etc.

### Common Expression Patterns

- Equality check: `if: "auth.role == 'ADMIN'"`
- Membership in list: `if: "auth.role in ['ADMIN', 'SUPERADMIN', 'SYSTEM']"`
- Optional chaining / safe checking: `if: "auth?.role == 'ADMIN'"`
- Boolean flag: `if: "auth.isInternal == true"`
