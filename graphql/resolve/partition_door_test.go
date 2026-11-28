/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/graphql/test"
)

const doorSchema = `
interface IAMResource {
	id: ID!
	label: String
}
type Group implements IAMResource {
	title: String
}
type Hidden implements IAMResource @generate(query: {get: true, query: false, aggregate: false}) {
	secret: String
}
type Workspace implements IAMResource {
	name: String! @id
}
type Candidate {
	id: ID!
	name: String
}
`

func TestTheDoorAsksTheAuthorityLastAndOnlyTypesItCanAsk(t *testing.T) {
	qs := DoorQuestions(test.LoadSchemaFromString(t, doorSchema))
	require.Equal(t, []string{
		"query { held: queryGroup(first: 1) { __typename } }",
		"query { held: queryWorkspace(first: 1) { __typename } }",
	}, qs, "the Workspace last; a type with no query of its own cannot be asked; a type that holds no grants is not asked")
}

func TestASchemaWithNothingThatHoldsGrantsLetsNobodyIn(t *testing.T) {
	require.Empty(t, DoorQuestions(test.LoadSchemaFromString(t, `type Candidate { id: ID! name: String }`)))
	require.Empty(t, DoorQuestions(nil))
}
