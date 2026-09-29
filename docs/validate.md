# @validate Directive

Field-level validation that runs **during mutation rewriting** — before the mutation is sent to
Dgraph. If validation fails, the mutation is rejected and Dgraph is never touched.

Compare with `@postValidate` (type-level, runs _after_ commit) — `@validate` is the pre-commit
per-field guard.

---

## Signature

```graphql
directive @validate(
  rule: String # go-playground/validator tag string (backward compatibility)
  expr: String # expr-lang boolean expression (backward compatibility)
  reason: String # human-readable message on failure / default fallback
  rules: [DgraphValidate] # general rules for add and update (array or single object)
  add: [DgraphValidate] # add-specific rules (array or single object)
  update: [DgraphValidate] # update-specific rules (array or single object)
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
>
> The built-in `error(v)` function accepts:
>
> - `string`: A single error message string.
> - `[]string`: A list of strings, returned as multiple distinct GraphQL errors in the response.
> - `[]map` or `map`: Rich error objects (e.g. `[{ message: "...", code: "...", ... }]`), preserving
>   custom fields in GraphQL error `extensions`.
>
> Additionally, `isDryRun` (boolean) is available in the expr environment and indicates whether the
> enclosing mutation is executing under `@dryRun`.

> [!IMPORTANT] `{{.error}}` requires `expr:` to call `error()` explicitly. It is **not** populated
> by runtime exceptions (nil dereference, etc.) — those panic and surface as a different error. Use
> `error()` as the last step in any conditional that wants to emit a human-readable message:
>
> ```graphql
> # ✅ correct — error() propagates the message into {{.error}}
> expr: "value < 100 || error('limit is 100, got ' + string(value))"
> reason: "Validation failed: {{.error}}"
>
> # ❌ wrong — expr returns false; {{.error}} is always empty
> expr: "value < 100"
> reason: "Validation failed: {{.error}}"   ← renders as "Validation failed: "
> ```

#### `callLambda` + `{{.error}}` pattern

The most powerful use of `{{.error}}` is with `callLambda` for server-side uniqueness or quota
checks. The lambda can return a structured response; the expression extracts the message and calls
`error()` so the reason template receives it:

```graphql
email: String @search(by: [hash, regexp]) @oldValue
  @validate(
    expr: """
      let res = ((before?.email ?? "") != input.email)
        ? callLambda("Candidate.checkUniqueEmail", {"args": {
            "inWorkspace": auth.ws,
            "sId": after.sId,
            "email": input.email,
          }})
        : nil;
      (res?.code ?? 200) == 200 ? true : error(res?.message)
    """,
    reason: "Email validation failed: {{.error}}"
  )
```

When the lambda returns `{ code: 409, message: "email already in use" }`, the client receives:

```
"Email validation failed: email already in use"
```

When the email hasn't changed (`before.email == input.email`), `res` is `nil`, `res?.code` is `nil`,
`nil ?? 200` is `200`, and the expression returns `true` — the lambda is skipped entirely.

> [!NOTE] `{{.error}}` in `@validate` works the same way as in `@postValidate`. Both renderers
> extract the `ExprError.Message` from the `error()` call and pass it as the `{{.error}}` template
> variable. `@validate` required a pointer-receiver fix to `validateExpr` so that the error message
> written inside the validator closure is visible to the `renderValidateReason` call that follows —
> prior to that fix, `{{.error}}` always rendered as `""`.

---

## Operation-Specific Arms and Array of Rules

By default, `rule`/`expr` or `rules:` apply to **both** add and update mutations. Use `add:` and
`update:` to provide operation-specific validation.

### Array of Rules

Each of `rules:`, `add:`, and `update:` accepts a list of `DgraphValidate` objects (or a single
object via GraphQL list coercion):

```graphql
code: String
  @validate(
    rules: [
      { rule: "required", reason: "Code is required" }
    ]
    add: [
      { rule: "min=3", reason: "Code must be at least 3 characters" }
      { expr: "!(value contains ' ')", reason: "Code cannot contain spaces" }
    ]
    update: [
      { rule: "min=3", reason: "Code must be at least 3 characters" }
      { expr: "before != nil ? value == before.code : true", reason: "Code is immutable once created" }
    ]
  )
```

### Rule-Specific Reasons & Fallbacks

Each rule in the array can specify its own `reason`. If an individual rule does not specify a
`reason`, it inherits the top-level `reason` argument as a fallback:

```graphql
tag: String
  @validate(
    reason: "Invalid tag: {{.value}}"
    rules: [
      { rule: "min=2" }                                           # uses top-level reason fallback
      { expr: "!contains(value, '#')", reason: "Tag cannot contain '#'" } # uses custom reason
    ]
  )
```

### Precedence

When operation-specific arms (`add:` or `update:`) are provided, they take **precedence** over
root-level `rules:` and root-level `rule`/`expr` for that operation.

---

## Interface Validation Inheritance

Validation rules defined on interface fields are automatically inherited and composed with any
validation rules declared on implementing concrete types.

```graphql
interface Contactable {
  email: String @validate(rule: "email", reason: "Must be a valid email address")
}

type Employee implements Contactable {
  id: ID!
  email: String
    @validate(
      expr: "value endsWith '@company.com'"
      reason: "Employee email must be on @company.com domain"
    )
}
```

When creating or updating an `Employee`:

1. The interface rule (`rule: "email"`) is evaluated first.
2. The concrete type rule (`expr: "value endsWith '@company.com'"`) is evaluated.
3. Both must pass; failure of either rule reports its corresponding failure `reason`.

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

You can combine both `rule` and `expr` on the same rule object, or provide multiple rules in an
array — all rules must pass:

```graphql
name: String!
  @validate(
    rules: [
      { rule: "required", reason: "Name is required" }
      { rule: "max=150", reason: "Name must be ≤150 characters" }
      { expr: "!value.hasPrefix('_')", reason: "Name must not start with an underscore" }
    ]
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
