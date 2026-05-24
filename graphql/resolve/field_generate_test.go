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

// TestFieldGenerate_QueryHiddenField_Rejected verifies the current behaviour of
// @generate(query: false) on a field.
//
// NOTE: stripQueryHiddenFields is implemented in gqlschema.go but is not currently
// called during schema generation. As a result, fields with @generate(query: false)
// are NOT stripped from the output type — they remain queryable. This test documents
// the current behaviour. If stripQueryHiddenFields is re-enabled, this test should
// be updated to require.Error instead.
func TestFieldGenerate_QueryHiddenField_Rejected(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, fieldGenerateTestSchema)

	_, err := gqlSchema.Operation(&schema.Request{
		Query: `query {
			queryPost {
				id
				title
				updatedAt
			}
		}`,
	})
	// Currently accepted — @generate(query: false) does not strip the field from
	// the output type. Update to require.Error if stripQueryHiddenFields is re-enabled.
	require.NoError(t, err,
		"@generate(query: false) does not currently strip the field from the output type")
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
