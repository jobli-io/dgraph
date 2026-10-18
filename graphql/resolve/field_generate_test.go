/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

// Package resolve provides runtime tests for field-level @generate directive behaviour.
//
// The @generate directive on a field controls three independent dimensions:
//   - mutation.add: false    → field is absent from AddXxxInput
//   - mutation.update: false → field is absent from XxxPatch
//   - query: false           → field is absent from the output type (cannot be queried)
//
// Internal mechanisms (@default, @transform) still write to these fields at runtime
// regardless of which flags are set. The field exists in Dgraph storage; it is simply
// not exposed via the generated GraphQL API surface.
package resolve

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/graphql/test"
)

// fieldGenerateTestSchema is the shared Post type used across all field-level @generate tests.
//
//   - updatedAt: fully internal (hidden from add, update, and query) — auto-set by @default.
//   - createdAt: visible in query and add, but hidden from update — auto-set by @default on add.
//   - score:     visible in add and query, but hidden from update — auto-set by @default on add.
//   - title:     fully exposed; normal field with no restrictions.
const fieldGenerateTestSchema = `
type Post {
  id:        ID!
  title:     String!
  updatedAt: DateTime @generate(mutation: { add: false, update: false }, query: false)
                      @default(add: { expr: "now()" }, update: { expr: "now()" })
  createdAt: DateTime @generate(mutation: { update: false })
                      @default(add: { expr: "now()" })
  score:     Int      @generate(mutation: { update: false })
                      @default(add: { value: "0" })
}
`

// TestFieldGenerate_AddInput_HiddenField verifies that a field with
// @generate(mutation: { add: false }) is not present in AddPostInput.
// Attempting to supply it in a GraphQL add operation must cause a schema error.
func TestFieldGenerate_AddInput_HiddenField(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, fieldGenerateTestSchema)

	_, err := gqlSchema.Operation(&schema.Request{
		Query: `mutation {
			addPost(input: [{ title: "Hello", updatedAt: "2024-01-01T00:00:00Z" }]) {
				post { id title }
			}
		}`,
	})
	require.Error(t, err,
		"updatedAt is hidden from AddPostInput via @generate(mutation:{add:false}) — "+
			"supplying it must cause a schema validation error")
}

// TestFieldGenerate_AddInput_VisibleFields verifies that fields NOT marked with
// @generate(mutation:{add:false}) are still accepted in AddPostInput.
func TestFieldGenerate_AddInput_VisibleFields(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, fieldGenerateTestSchema)

	// createdAt and score are both present in AddPostInput (they only restrict updates).
	// We omit updatedAt intentionally — it is hidden from AddPostInput.
	_, err := gqlSchema.Operation(&schema.Request{
		Query: `mutation {
			addPost(input: [{ title: "Hello", createdAt: "2024-01-01T00:00:00Z", score: 10 }]) {
				post { id title createdAt score }
			}
		}`,
	})
	require.NoError(t, err,
		"add mutation supplying only visible fields (title, createdAt, score) must be accepted")
}

// TestFieldGenerate_UpdatePatch_HiddenField_CreatedAt verifies that a field with
// @generate(mutation: { update: false }) is absent from PostPatch.
// Attempting to set createdAt in an update set clause must cause a schema error.
func TestFieldGenerate_UpdatePatch_HiddenField_CreatedAt(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, fieldGenerateTestSchema)

	_, err := gqlSchema.Operation(&schema.Request{
		Query: `mutation {
			updatePost(input: {
				filter: { id: ["0x1"] }
				set: { createdAt: "2024-01-01T00:00:00Z" }
			}) {
				post { id title }
			}
		}`,
	})
	require.Error(t, err,
		"createdAt is hidden from PostPatch via @generate(mutation:{update:false}) — "+
			"supplying it in update.set must cause a schema validation error")
}

// TestFieldGenerate_UpdatePatch_HiddenField_UpdatedAt verifies the same for updatedAt,
// which is hidden from both add and update inputs.
func TestFieldGenerate_UpdatePatch_HiddenField_UpdatedAt(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, fieldGenerateTestSchema)

	_, err := gqlSchema.Operation(&schema.Request{
		Query: `mutation {
			updatePost(input: {
				filter: { id: ["0x1"] }
				set: { updatedAt: "2024-01-01T00:00:00Z" }
			}) {
				post { id title }
			}
		}`,
	})
	require.Error(t, err,
		"updatedAt is hidden from PostPatch via @generate(mutation:{update:false}) — "+
			"supplying it in update.set must cause a schema validation error")
}

// TestFieldGenerate_UpdatePatch_VisibleFields verifies that fields without
// update restrictions are still valid in PostPatch (title is fully exposed).
func TestFieldGenerate_UpdatePatch_VisibleFields(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, fieldGenerateTestSchema)

	_, err := gqlSchema.Operation(&schema.Request{
		Query: `mutation {
			updatePost(input: {
				filter: { id: ["0x1"] }
				set: { title: "Updated title" }
			}) {
				post { id title }
			}
		}`,
	})
	require.NoError(t, err,
		"update mutation setting only the visible title field must be accepted")
}

// TestFieldGenerate_QueryHiddenField_Rejected verifies that @generate(query: false)
// strips the field from the output type, rejecting client queries that select it.
func TestFieldGenerate_QueryHiddenField_Rejected(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, fieldGenerateTestSchema)

	// 1. queryPost selection
	_, err := gqlSchema.Operation(&schema.Request{
		Query: `query {
			queryPost {
				id
				title
				updatedAt
			}
		}`,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), `Cannot query field "updatedAt" on type "Post"`)

	// 2. getPost selection
	_, err = gqlSchema.Operation(&schema.Request{
		Query: `query {
			getPost(id: "0x1") {
				id
				title
				updatedAt
			}
		}`,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), `Cannot query field "updatedAt" on type "Post"`)

	// 3. Mutation payload selection
	_, err = gqlSchema.Operation(&schema.Request{
		Query: `mutation {
			addPost(input: [{ title: "Test" }]) {
				post {
					id
					updatedAt
				}
			}
		}`,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), `Cannot query field "updatedAt" on type "Post"`)
}

// TestFieldGenerate_QueryVisibleFields_Accepted verifies that fields without
// @generate(query: false) remain queryable even when they have mutation restrictions.
// createdAt and score have mutation.update: false but are still visible in query results.
func TestFieldGenerate_QueryVisibleFields_Accepted(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, fieldGenerateTestSchema)

	_, err := gqlSchema.Operation(&schema.Request{
		Query: `query {
			queryPost {
				id
				title
				createdAt
				score
			}
		}`,
	})
	require.NoError(t, err,
		"querying visible fields (title, createdAt, score) must be accepted — "+
			"mutation restrictions do not affect query visibility")
}

// TestFieldGenerate_QueryHiddenField_FiltersAndOrderRejected verifies that
// @generate(query: false) fields are excluded from HasFilter and Orderable enums.
func TestFieldGenerate_QueryHiddenField_FiltersAndOrderRejected(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, fieldGenerateTestSchema)

	// Filter has: [updatedAt] must fail validation
	_, err := gqlSchema.Operation(&schema.Request{
		Query: `query {
			queryPost(filter: { has: [updatedAt] }) {
				id
			}
		}`,
	})
	require.Error(t, err, "filtering on query-hidden field via has must be rejected")
	require.Contains(t, err.Error(), `Expected type PostHasFilter, found updatedAt`)

	// Orderable asc: updatedAt must fail validation
	_, err = gqlSchema.Operation(&schema.Request{
		Query: `query {
			queryPost(order: { asc: updatedAt }) {
				id
			}
		}`,
	})
	require.Error(t, err, "ordering on query-hidden field must be rejected")
	require.Contains(t, err.Error(), `Expected type PostOrderable, found updatedAt`)
}

// TestFieldGenerate_InternalField_RuntimeMetadata verifies that query-hidden fields
// are still preserved internally in schema metadata for @default, @transform, and Dgraph predicates.
func TestFieldGenerate_InternalField_RuntimeMetadata(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, fieldGenerateTestSchema)

	op, err := gqlSchema.Operation(&schema.Request{
		Query: `mutation {
			addPost(input: [{ title: "Test" }]) {
				post { id }
			}
		}`,
	})
	require.NoError(t, err)

	postTyp := op.Mutations()[0].MutatedType()
	require.NotNil(t, postTyp, "Post type must exist")

	// The field definition must be resolved internally
	updatedAtFld := postTyp.Field("updatedAt")
	require.NotNil(t, updatedAtFld, "updatedAt FieldDefinition must be accessible internally")
	require.Equal(t, "updatedAt", updatedAtFld.Name())

	// DgraphPredicate mapping must be correct
	require.Equal(t, "Post.updatedAt", postTyp.DgraphPredicate("updatedAt"))

	// Default value evaluation order must include updatedAt for both add and update
	addDefaults := postTyp.FieldsInDefaultValueEvaluationOrder("add")
	var foundAdd bool
	for _, f := range addDefaults {
		if f.Name() == "updatedAt" {
			foundAdd = true
			break
		}
	}
	require.True(t, foundAdd, "updatedAt must be in FieldsInDefaultValueEvaluationOrder('add')")

	updDefaults := postTyp.FieldsInDefaultValueEvaluationOrder("update")
	var foundUpd bool
	for _, f := range updDefaults {
		if f.Name() == "updatedAt" {
			foundUpd = true
			break
		}
	}
	require.True(t, foundUpd, "updatedAt must be in FieldsInDefaultValueEvaluationOrder('update')")
}
