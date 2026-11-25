/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"testing"

	"github.com/stretchr/testify/require"

	dschema "github.com/dgraph-io/dgraph/v25/schema"
)

func TestOrderedDirectiveValidationAndDGSchema(t *testing.T) {
	t.Run("valid String and Enum list variants and interface inheritance", func(t *testing.T) {
		input := `
			enum Stage {
				TODO
				IN_PROGRESS
				DONE
			}
			interface Sequenced {
				id: ID!
				steps: [String!] @ordered(count: true) @search(by: [exact, term])
			}
			type Pipeline implements Sequenced {
				id: ID!
				steps: [String!]
				nullableList: [String] @ordered
				nonNullList: [String!]! @ordered
				stageHistory: [Stage!] @ordered(count: true)
			}
		`
		handler, errs := NewHandler(input, false)
		require.Empty(t, errs)

		dgSchema := handler.DGSchema()
		require.Contains(t, dgSchema, "Sequenced.steps: stringvector @index(exact, term) @count .")
		require.Contains(t, dgSchema, "Pipeline.nullableList: stringvector .")
		require.Contains(t, dgSchema, "Pipeline.nonNullList: stringvector .")
		require.Contains(t, dgSchema, "Pipeline.stageHistory: stringvector @index(hash) @count .")

		parsed, err := dschema.Parse(dgSchema)
		require.NoError(t, err)
		require.NotEmpty(t, parsed.Preds)
	})

	invalidCases := []struct {
		name        string
		schema      string
		errContains string
	}{
		{
			name: "scalar non-list String rejected",
			schema: `
				type Task {
					id: ID!
					title: String @ordered
				}
			`,
			errContains: "@ordered directive only applies to list fields of String or Enum",
		},
		{
			name: "Int list rejected",
			schema: `
				type Task {
					id: ID!
					ranks: [Int!] @ordered
				}
			`,
			errContains: "@ordered directive only applies to list fields of String or Enum",
		},
		{
			name: "Object list rejected",
			schema: `
				type Item {
					id: ID!
					name: String
				}
				type Bag {
					id: ID!
					items: [Item] @ordered
				}
			`,
			errContains: "@ordered directive only applies to list fields of String or Enum",
		},
		{
			name: "type-level @ordered rejected",
			schema: `
				type Task @ordered {
					id: ID!
					steps: [String]
				}
			`,
			errContains: "not applicable",
		},
		{
			name: "conflicting @dgraph pred between unordered [String] and @ordered [String]",
			schema: `
				type A {
					id: ID!
					tags: [String] @dgraph(pred: "shared_tags")
				}
				type B {
					id: ID!
					tags: [String] @ordered @dgraph(pred: "shared_tags")
				}
			`,
			errContains: "has type stringvector, which is different",
		},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := NewHandler(tc.schema, false)
			require.NotEmpty(t, errs)
			require.Contains(t, errs.Error(), tc.errContains)
		})
	}
}
