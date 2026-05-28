# @validate Directive

Field-level validation that runs **during mutation rewriting** — before the mutation is sent to
Dgraph. If validation fails, the mutation is rejected and Dgraph is never touched.

Compare with `@postValidate` (type-level, runs _after_ commit) — `@validate` is the pre-commit
per-field guard.

---

## Signature

```graphql
directive @validate(
  rule: String # go-playground/validator tag string
  expr: String # expr-lang boolean expression
  reason: String # human-readable message on failure
  add: DgraphValidate
  update: DgraphValidate
) on FIELD_DEFINITION

input DgraphValidate {
  rule: String
  expr: String
  reason: String
}
```

---

## How It Works

Validation fires **during mutation rewriting**, before the DQL mutation is submitted to Dgraph:

```
GraphQL mutation arrives
  └─> @oldValue pre-fetch (if any @oldValue fields)
  └─> mutation rewriting begins
        └─> @validate runs per field        ← runs here
        └─> @default values injected
        └─> @transform values applied
  └─> DQL write submitted to Dgraph
```

If `rule` or `expr` evaluates to a failure, the mutation is aborted immediately and the client
receives an error response. **No data is written.**

---

## Validation Modes

### `rule:` — Struct tag validation

Uses the [go-playground/validator](https://github.com/go-playground/validator) library. The `rule`
value is a standard struct tag string:

```graphql
name: String!
  @validate(rule: "required,max=150", reason: "Name is required and must be under 150 characters")

email: String
  @validate(rule: "omitempty,email", reason: "Must be a valid email address")

website: String
  @validate(rule: "omitempty,http_url", reason: "Must be a valid HTTP URL")

phone: String
  @validate(rule: "omitempty,e164", reason: "Must be a valid E.164 phone number")
```

When the field is `nil` (not provided), `rule` validation is **skipped**. Use `required` in the tag
to make the field mandatory.

### `expr:` — expr-lang expression

An expr-lang boolean expression evaluated against the mutation context. Returns `true` = valid,
`false` = fail.

```graphql
createdBy: String!
  @validate(
    expr: "auth.azp == 'jobli-system'"
    reason: "Only the system client may set this field"
  )

search: String
  @validate(expr: "rawInput?.search == nil || auth.azp == 'jobli-system'")
```

When the field value is `nil`, `expr` validation is **skipped**. To validate that the field must be
present, combine with `rule: "required"`.

### `reason:` — Error message

The human-readable message returned to the client on failure. Supports Go
[`text/template`](https://pkg.go.dev/text/template) syntax. Plain strings are returned as-is.

#### Available template variables

| Variable      | Example access             | Description                                                                      |
| ------------- | -------------------------- | -------------------------------------------------------------------------------- |
| `{{.value}}`  | `{{.value}}`               | The field value being validated                                                  |
| `{{.field}}`  | `{{.field}}`               | The GraphQL field name                                                           |
| `{{.action}}` | `{{.action}}`              | `"add"` or `"update"`                                                            |
| `{{.auth}}`   | `{{index .auth "USERID"}}` | JWT claim map                                                                    |
| `{{.error}}`  | `{{.error}}`               | Message from `error()` call; empty string `""` when `expr` simply returned false |
| `{{.tag}}`    | `{{.tag}}`                 | The failing validator tag (e.g. `"max"`, `"expr"`, `"required"`)                 |

```graphql
# {{.value}} — show the rejected value
price: Float @validate(rule: "gt=0", reason: "Price must be positive, got {{.value}}")

# {{.action}} — operation-aware message
status: String
  @validate(
    update: { expr: "value != \'DELETED\' || auth.role == \'admin\'" }
    reason: "Only admins may set DELETED (action: {{.action}})"
  )

# {{.error}} — message from error() in expr
quota: Int
  @validate(
    expr: "value <= 100 || error(\'Requested \' + string(value) + \' exceeds limit of 100\')"
    reason: "Quota check failed: {{.error}}"
  )
```

> [!NOTE] Plain strings (no `{{`) are returned as-is with zero overhead. If the template fails to
> parse or execute, the literal `reason` string is returned unchanged — validation failure is never
> silently lost.
>
> When `expr` calls `error(msg)`, the expression returns false and `{{.error}}` is populated with
> `msg`. When `expr` simply returns `false`, `{{.error}}` is an empty string `""`.

---

## Operation-Specific Arms

By default, `rule`/`expr` apply to **both** add and update mutations. Use `add:` and `update:` to
provide operation-specific validation:

```graphql
status: String
  @validate(
    add:    { rule: "required", reason: "Status is required on creation" }
    update: { expr: "value != 'DELETED' || auth.role == 'admin'", reason: "Only admins may set DELETED" }
  )
```

When operation-specific arms are present, they take **precedence** over any root-level
`rule`/`expr`.

---

## expr-lang Evaluation Context

| Variable   | Description                                                                |
| ---------- | -------------------------------------------------------------------------- |
| `value`    | The field's new value from the mutation input                              |
| `input`    | Full mutation input object (after `@default` injection)                    |
| `rawInput` | Original user input before `@default` injection                            |
| `before`   | Pre-mutation field values (from `@oldValue` fetch); `{}` if no `@oldValue` |
| `after`    | Merged `before + input` — the expected post-state                          |
| `new`      | Diff map — keys that changed between `before` and `after`                  |
| `remove`   | Fields being removed                                                       |
| `auth`     | JWT claims map (`auth.sub`, `auth.azp`, `auth.ws`, custom claims)          |
| `action`   | `"add"` or `"update"`                                                      |

---

## Multiple Validators on One Field

You can combine both `rule` and `expr` on the same field — both must pass:

```graphql
name: String!
  @validate(
    rule:   "required,max=150"
    expr:   "!name.hasPrefix('_')"
    reason: "Name must be ≤150 characters and must not start with underscore"
  )
```

---

## Schema Validation

- `@validate` may not be used on `@remote` types.
- `expr` must compile as a valid expr-lang boolean expression.
- At least one of `rule`, `expr`, `add`, `update` must be present.

---

## Difference from `@postValidate`

|                | `@validate`                        | `@postValidate`                        |
| -------------- | ---------------------------------- | -------------------------------------- |
| **Placement**  | Field-level                        | Type-level                             |
| **When**       | Before commit (rewriting phase)    | After commit                           |
| **Scope**      | Single field                       | Entire node (all fields)               |
| **Data**       | New value + context                | Written node + `before`/`after`/`new`  |
| **On failure** | Mutation rejected, nothing written | Error returned, data already committed |
| **Use case**   | Input guards, permission checks    | Cross-field invariants, quota checks   |
