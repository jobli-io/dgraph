# @passiveSubscription Directive

Query custom HTTP and lambda fields safely inside GraphQL Subscriptions by explicitly opting in at
both the schema and query levels.

---

## Signature

```graphql
directive @passiveSubscription on FIELD_DEFINITION | FIELD
```

Place `@passiveSubscription` on:

1. **Field Definitions** (in the schema): Authorizes the custom HTTP/lambda field as safe to resolve
   on subscription trigger events.
2. **Fields** (in client queries): Acknowledges that the client developer understands the field is
   passive (updates inside the remote service/lambda will not trigger subscription pushes).

---

## Overview

In Dgraph, custom HTTP (`@custom(http: ...)`) and lambda (`@lambda`) fields are resolved by making
external network calls. Since external changes to these remote systems do not generate internal
mutation events inside Dgraph, querying them inside traditional subscriptions could be unpredictable
or resource-intensive.

Following the redesign of Dgraph's subscription engine to use **event-driven invalidations**,
subscription streams only wake up and push payloads when mutations occur within Dgraph on the parent
types. This makes it completely safe to resolve custom/lambda fields inside subscriptions—provided
that both the API author and the client developer explicitly consent to the passive nature of the
field.

---

## Dual-Layer Verification

To prevent accidental queries and maintain a strict contract, `@passiveSubscription` enforces
dual-layer validation at compile/parse time:

```mermaid
graph TD
    A[GraphQL Subscription Query Received] --> B{Is Field IsCustomHTTP?}
    B -- No --> C[Allow Query]
    B -- Yes --> D{Is @passiveSubscription on Schema Field?}
    D -- No --> E[Reject: "Custom field `field` is not supported in graphql subscription"]
    D -- Yes --> F{Is @passiveSubscription on Query Field?}
    F -- No --> G[Reject: "Field `field` is a custom/lambda field and requires the `@passiveSubscription` directive to be queried..."]
    F -- Yes --> H[Allow Query and Resolve on trigger]
```

### 1. Schema-Level Authorization (API Designer)

The API designer must explicitly allow a custom field to be returned in subscriptions by adding
`@passiveSubscription` to the field definition:

```graphql
type Job {
  id: ID!
  title: String!
  # Authorized for subscription querying
  aiSummary: String!
    @custom(
      http: { url: "https://api.jobli.io/summarize", method: "POST", body: "{ title: \"$title\" }" }
    )
    @passiveSubscription
}
```

### 2. Query-Level Acknowledgment (Client Developer)

The client developer querying the subscription must explicitly append `@passiveSubscription` to the
field in their selection set to acknowledge that the field is passive:

```graphql
subscription {
  queryJob {
    id
    title
    # Acknowledging passive updates
    aiSummary @passiveSubscription
  }
}
```

---

## Validation Errors

### Schema-Level Validation Failure

If the field is queried in a subscription, but `@passiveSubscription` was **not** authorized in the
schema definition:

```json
{
  "errors": [
    {
      "message": "Custom field `aiSummary` is not supported in graphql subscription"
    }
  ]
}
```

### Query-Level Validation Failure

If `@passiveSubscription` is authorized in the schema, but the client developer **omitted**
`@passiveSubscription` in their subscription query document:

```json
{
  "errors": [
    {
      "message": "Field `aiSummary` is a custom/lambda field and requires the `@passiveSubscription` directive to be queried in a subscription."
    }
  ]
}
```
