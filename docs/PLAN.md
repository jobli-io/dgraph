### Consolidated Plan: Dgraph GraphQL with Cloud Spanner

This plan outlines the steps to integrate Google Cloud Spanner as a backend for the Dgraph GraphQL
layer, using the GQL ISO standard. The plan is divided into four main phases: Schema Management,
Data Management, Webhook Management, and Finalization.

**Phase 1: Schema Management**

1.  **Schema Translation:**

    - Create a `genSpannerSchema` function to translate the GraphQL schema into Cloud Spanner Data
      Definition Language (DDL).
    - This function will map GraphQL types to Spanner tables, fields to columns, and handle
      relationships using foreign keys and join tables. It will also create Spanner indexes for
      fields with the `@search` directive.

2.  **Schema Application and Updates:**
    - Develop a tool to apply the generated DDL to a Spanner database.
    - This tool will support both initial schema creation and subsequent updates.
    - For updates, the tool will include a "diffing" mechanism to compare the new schema with the
      existing Spanner schema and generate the necessary `ALTER TABLE` statements.
    - To prevent accidental data loss, the tool will provide a preview of the changes and require
      user confirmation before applying them.

**Phase 2: Data Management**

1.  **`Storage` Interface:**

    - Define a `Storage` interface that abstracts the backend data store. This interface will
      include methods for:
      - Executing queries and mutations.
      - Applying and retrieving the schema.

2.  **`DgraphStore` Implementation:**

    - Refactor the existing Dgraph integration into a `DgraphStore` that implements the `Storage`
      interface. This will ensure continued support for Dgraph as a backend.

3.  **`SpannerStore` Implementation:**

    - Create a `SpannerStore` that implements the `Storage` interface for Cloud Spanner. This will
      involve:
      - Connecting to a Spanner instance.
      - Translating generic queries and mutations into Spanner's GQL.
      - Executing GQL against Spanner.
      - Translating Spanner results back to the generic format.

4.  **Update GraphQL Resolvers:**
    - Modify the GraphQL resolvers in `graphql/resolve` to use the `Storage` interface, making them
      backend-agnostic.

**Phase 3: Webhook Management**

1.  **Webhook Configuration:**

    - Leverage the existing `@lambdaOnMutate` directive to allow users to configure webhooks on
      mutations.
    - The configuration will include the webhook URL, which will be stored in a way that is
      accessible to both the Dgraph and Spanner backends.

2.  **Webhook Event Triggering:**

    - The `Storage` interface will be extended with a method for triggering webhooks.
    - The `DgraphStore` and `SpannerStore` will implement this method to trigger webhooks after a
      successful mutation.

3.  **Webhook Payload:**

    - The existing `webhookPayload` struct will be used to create the JSON payload for the webhook
      notification.
    - The payload will be populated with information from the mutation, regardless of which backend
      it was executed on.

4.  **Asynchronous Execution:**
    - The `sendWebhookEvent` function will be used to send the webhook notification asynchronously.
      This will ensure that webhook notifications do not block the main mutation flow.

**Phase 4: Finalization**

1.  **Configuration:**

    - Add a configuration option to the GraphQL server to allow users to select the desired storage
      backend (Dgraph or Cloud Spanner).

2.  **Data Migration:**
    - Devise a strategy and create tools for migrating data from Dgraph to Cloud Spanner.
