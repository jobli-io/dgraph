/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"
	"testing"

	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/graphql/test"
	"github.com/stretchr/testify/require"
)

func TestTypeLevelValidate_AddMutationRewriter(t *testing.T) {
	const sch = `
		type Event @validate(
			expr: "after.endDate > after.startDate"
			reason: "Type {{.type}}: endDate must be after startDate"
		) {
			id: ID!
			title: String!
			startDate: String!
			endDate: String!
		}
	`

	gqlSchema := test.LoadSchemaFromString(t, sch)
	addRewriter := NewAddRewriter()

	t.Run("violating type validation aborts mutation rewriting", func(t *testing.T) {
		mutQuery := `
			mutation {
				addEvent(input: [{
					title: "Past Conference",
					startDate: "2026-10-10",
					endDate: "2026-10-05"
				}]) {
					event {
						id
						title
					}
				}
			}
		`
		op, err := gqlSchema.Operation(&schema.Request{Query: mutQuery})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		idExistence := make(map[string]string)
		_, _, _ = addRewriter.RewriteQueries(context.Background(), mut)
		_, err = addRewriter.Rewrite(context.Background(), mut, idExistence)
		require.Error(t, err)
		require.Contains(t, err.Error(), "Type Event; Type Event: endDate must be after startDate")
	})

	t.Run("valid input produces DQL mutation successfully", func(t *testing.T) {
		mutQuery := `
			mutation {
				addEvent(input: [{
					title: "Future Conference",
					startDate: "2026-10-01",
					endDate: "2026-10-05"
				}]) {
					event {
						id
						title
					}
				}
			}
		`
		op, err := gqlSchema.Operation(&schema.Request{Query: mutQuery})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		idExistence := make(map[string]string)
		_, _, _ = addRewriter.RewriteQueries(context.Background(), mut)
		dgMut, err := addRewriter.Rewrite(context.Background(), mut, idExistence)
		require.NoError(t, err)
		require.NotEmpty(t, dgMut)
		require.NotEmpty(t, dgMut[0].Mutations)
		require.NotEmpty(t, dgMut[0].Mutations[0].SetJson)
	})
}

func TestTypeLevelValidate_InterfaceInheritanceInRewriter(t *testing.T) {
	const sch = `
		interface Named @validate(
			expr: "after.title != ''"
			reason: "Title cannot be empty"
		) {
			id: ID!
			title: String!
		}

		type Task implements Named @validate(
			expr: "after.priority >= 1 && after.priority <= 5"
			reason: "Priority must be between 1 and 5"
		) {
			id: ID!
			title: String!
			priority: Int!
		}
	`

	gqlSchema := test.LoadSchemaFromString(t, sch)
	addRewriter := NewAddRewriter()

	t.Run("violating interface validation fails", func(t *testing.T) {
		mutQuery := `
			mutation {
				addTask(input: [{
					title: "",
					priority: 3
				}]) {
					task {
						id
					}
				}
			}
		`
		op, err := gqlSchema.Operation(&schema.Request{Query: mutQuery})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		idExistence := make(map[string]string)
		_, _, _ = addRewriter.RewriteQueries(context.Background(), mut)
		_, err = addRewriter.Rewrite(context.Background(), mut, idExistence)
		require.Error(t, err)
		require.Contains(t, err.Error(), "Type Task; Title cannot be empty")
	})

	t.Run("violating concrete type validation fails", func(t *testing.T) {
		mutQuery := `
			mutation {
				addTask(input: [{
					title: "Do homework",
					priority: 10
				}]) {
					task {
						id
					}
				}
			}
		`
		op, err := gqlSchema.Operation(&schema.Request{Query: mutQuery})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		idExistence := make(map[string]string)
		_, _, _ = addRewriter.RewriteQueries(context.Background(), mut)
		_, err = addRewriter.Rewrite(context.Background(), mut, idExistence)
		require.Error(t, err)
		require.Contains(t, err.Error(), "Type Task; Priority must be between 1 and 5")
	})

	t.Run("satisfying both interface and type rules succeeds", func(t *testing.T) {
		mutQuery := `
			mutation {
				addTask(input: [{
					title: "Do homework",
					priority: 3
				}]) {
					task {
						id
					}
				}
			}
		`
		op, err := gqlSchema.Operation(&schema.Request{Query: mutQuery})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		idExistence := make(map[string]string)
		_, _, _ = addRewriter.RewriteQueries(context.Background(), mut)
		dgMut, err := addRewriter.Rewrite(context.Background(), mut, idExistence)
		require.NoError(t, err)
		require.NotNil(t, dgMut)
	})
}

func TestTypeLevelValidate_UpdateMutationRewriter(t *testing.T) {
	const sch = `
		type Event @validate(
			update: [
				{ expr: "input.priority >= 1 && input.priority <= 5", reason: "Updated priority must be between 1 and 5" }
			]
		) {
			id: ID!
			title: String!
			priority: Int!
		}
	`

	gqlSchema := test.LoadSchemaFromString(t, sch)
	updateRewriter := NewUpdateRewriter()

	t.Run("violating update type validation fails", func(t *testing.T) {
		mutQuery := `
			mutation {
				updateEvent(input: {
					filter: { id: ["0x1"] },
					set: { priority: 10 }
				}) {
					event {
						id
					}
				}
			}
		`
		op, err := gqlSchema.Operation(&schema.Request{Query: mutQuery})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		idExistence := make(map[string]string)
		_, _, _ = updateRewriter.RewriteQueries(context.Background(), mut)
		_, err = updateRewriter.Rewrite(context.Background(), mut, idExistence)
		require.Error(t, err)
		require.Contains(t, err.Error(), "Type Event; Updated priority must be between 1 and 5")
	})

	t.Run("satisfying update type validation succeeds", func(t *testing.T) {
		mutQuery := `
			mutation {
				updateEvent(input: {
					filter: { id: ["0x1"] },
					set: { priority: 3 }
				}) {
					event {
						id
					}
				}
			}
		`
		op, err := gqlSchema.Operation(&schema.Request{Query: mutQuery})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		idExistence := make(map[string]string)
		_, _, _ = updateRewriter.RewriteQueries(context.Background(), mut)
		dgMut, err := updateRewriter.Rewrite(context.Background(), mut, idExistence)
		require.NoError(t, err)
		require.NotEmpty(t, dgMut)
	})
}
