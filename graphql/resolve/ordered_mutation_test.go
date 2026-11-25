/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/chunker"
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/graphql/test"
)

const orderedTestSchema = `
enum Stage {
  DRAFT
  REVIEW
  DONE
}

type Workflow {
  id: ID!
  name: String!
  steps: [String!] @ordered(count: true)
  optionalSteps: [String] @ordered
  stages: [Stage!] @ordered
}
`

func TestOrderedMutationRewriter(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, orderedTestSchema)
	ctx := context.Background()

	t.Run("add preserves order and duplicates and supports empty array", func(t *testing.T) {
		op, err := gqlSchema.Operation(&schema.Request{
			Query: `mutation {
				addWorkflow(input: [
					{ name: "w1", steps: ["beta", "alpha", "beta"], stages: [REVIEW, DRAFT, REVIEW] },
					{ name: "w2", steps: [] }
				]) {
					workflow { id name steps stages }
				}
			}`,
		})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		rewriter := NewAddRewriter()
		_, _, err = rewriter.RewriteQueries(ctx, mut)
		require.NoError(t, err)

		upsertMuts, err := rewriter.Rewrite(ctx, mut, map[string]string{})
		require.NoError(t, err)
		require.Len(t, upsertMuts, 1)
		require.Len(t, upsertMuts[0].Mutations, 2)

		var setW1, setW2 map[string]interface{}
		require.NoError(t, json.Unmarshal(upsertMuts[0].Mutations[0].SetJson, &setW1))
		require.NoError(t, json.Unmarshal(upsertMuts[0].Mutations[1].SetJson, &setW2))
		require.Equal(t, []interface{}{"beta", "alpha", "beta"}, setW1["Workflow.steps"])
		require.Equal(t, []interface{}{"REVIEW", "DRAFT", "REVIEW"}, setW1["Workflow.stages"])
		require.Equal(t, []interface{}{}, setW2["Workflow.steps"])

		// Verify chunker emits EmptyLists placeholder for w2's steps: []
		_, emptyLists, _, err := chunker.ParseJSONWithEmptyLists(upsertMuts[0].Mutations[1].SetJson, chunker.SetNquads)
		require.NoError(t, err)
		require.Len(t, emptyLists, 1)
		require.Equal(t, "Workflow.steps", emptyLists[0].Predicate)
	})

	t.Run("update set replaces list or clears with empty array", func(t *testing.T) {
		op, err := gqlSchema.Operation(&schema.Request{
			Query: `mutation {
				updateWorkflow(input: {
					filter: { id: ["0x1"] }
					set: { steps: [] }
				}) {
					workflow { id steps }
				}
			}`,
		})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		rewriter := NewUpdateRewriter()
		_, _, err = rewriter.RewriteQueries(ctx, mut)
		require.NoError(t, err)

		upsertMuts, err := rewriter.Rewrite(ctx, mut, map[string]string{})
		require.NoError(t, err)
		require.Len(t, upsertMuts, 1)
		require.Len(t, upsertMuts[0].Mutations, 1)

		var setMap map[string]interface{}
		require.NoError(t, json.Unmarshal(upsertMuts[0].Mutations[0].SetJson, &setMap))
		require.Equal(t, []interface{}{}, setMap["Workflow.steps"])
	})

	t.Run("update remove with null clears entire ordered list", func(t *testing.T) {
		op, err := gqlSchema.Operation(&schema.Request{
			Query: `mutation {
				updateWorkflow(input: {
					filter: { id: ["0x1"] }
					remove: { steps: null }
				}) {
					workflow { id steps }
				}
			}`,
		})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		rewriter := NewUpdateRewriter()
		_, _, err = rewriter.RewriteQueries(ctx, mut)
		require.NoError(t, err)

		upsertMuts, err := rewriter.Rewrite(ctx, mut, map[string]string{})
		require.NoError(t, err)
		require.Len(t, upsertMuts, 1)
		require.Len(t, upsertMuts[0].Mutations, 1)

		var delMap map[string]interface{}
		require.NoError(t, json.Unmarshal(upsertMuts[0].Mutations[0].DeleteJson, &delMap))
		val, exists := delMap["Workflow.steps"]
		require.True(t, exists)
		require.Nil(t, val)
	})

	t.Run("update remove with specific elements is rejected", func(t *testing.T) {
		op, err := gqlSchema.Operation(&schema.Request{
			Query: `mutation {
				updateWorkflow(input: {
					filter: { id: ["0x1"] }
					remove: { steps: ["alpha"] }
				}) {
					workflow { id steps }
				}
			}`,
		})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		rewriter := NewUpdateRewriter()
		_, _, err = rewriter.RewriteQueries(ctx, mut)
		require.NoError(t, err)

		_, err = rewriter.Rewrite(ctx, mut, map[string]string{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot remove specific elements from @ordered field Workflow.steps")
	})

	t.Run("null element inside nullable @ordered list is rejected", func(t *testing.T) {
		op, err := gqlSchema.Operation(&schema.Request{
			Query: `mutation {
				addWorkflow(input: [{ name: "bad", optionalSteps: ["a", null] }]) {
					workflow { id }
				}
			}`,
		})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		rewriter := NewAddRewriter()
		_, _, err = rewriter.RewriteQueries(ctx, mut)
		require.NoError(t, err)

		_, err = rewriter.Rewrite(ctx, mut, map[string]string{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot store null element in @ordered field Workflow.optionalSteps")
	})

	t.Run("explicit null on add omits @ordered field rather than emitting empty array", func(t *testing.T) {
		op, err := gqlSchema.Operation(&schema.Request{
			Query: `mutation {
				addWorkflow(input: [{ name: "w_null", optionalSteps: null }]) {
					workflow { id }
				}
			}`,
		})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		rewriter := NewAddRewriter()
		_, _, err = rewriter.RewriteQueries(ctx, mut)
		require.NoError(t, err)

		upsertMuts, err := rewriter.Rewrite(ctx, mut, map[string]string{})
		require.NoError(t, err)
		var setMap map[string]interface{}
		require.NoError(t, json.Unmarshal(upsertMuts[0].Mutations[0].SetJson, &setMap))
		_, exists := setMap["Workflow.optionalSteps"]
		require.False(t, exists)
	})

	t.Run("JSON variables for remove null and set empty array", func(t *testing.T) {
		op, err := gqlSchema.Operation(&schema.Request{
			Query: `mutation($patch: UpdateWorkflowInput!) {
				updateWorkflow(input: $patch) {
					workflow { id steps }
				}
			}`,
			Variables: map[string]interface{}{
				"patch": map[string]interface{}{
					"filter": map[string]interface{}{"id": []interface{}{"0x1"}},
					"remove": map[string]interface{}{"steps": nil},
				},
			},
		})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		rewriter := NewUpdateRewriter()
		_, _, err = rewriter.RewriteQueries(ctx, mut)
		require.NoError(t, err)

		upsertMuts, err := rewriter.Rewrite(ctx, mut, map[string]string{})
		require.NoError(t, err)
		var delMap map[string]interface{}
		require.NoError(t, json.Unmarshal(upsertMuts[0].Mutations[0].DeleteJson, &delMap))
		val, exists := delMap["Workflow.steps"]
		require.True(t, exists)
		require.Nil(t, val)
	})
}
