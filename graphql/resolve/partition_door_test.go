/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/graphql/test"
)

const doorSchema = `
interface IAMResource @grants {
	id: ID!
	label: String
}
type Group implements IAMResource {
	title: String
}
type Hidden implements IAMResource @generate(query: {get: true, query: false, aggregate: false}) {
	secret: String
}
type Workspace implements IAMResource @tenant {
	name: String! @id
}
type Board @grants {
	id: ID!
	title: String
}
type Candidate {
	id: ID!
	name: String
}
`

func TestTheDoorAsksTheTenantLastAndOnlyTypesItCanAsk(t *testing.T) {
	sch := test.LoadSchemaFromString(t, doorSchema)
	require.Equal(t, "Workspace", sch.Tenant())
	require.Equal(t, []string{"Group", "Hidden", "Workspace", "Board"}, sch.GrantHolders(),
		"marked themselves or through an interface, in the order the schema declares them")
	require.Equal(t, []string{
		"query { held: queryGroup(first: 1) { __typename } }",
		"query { held: queryBoard(first: 1) { __typename } }",
		"query { held: queryWorkspace(first: 1) { __typename } }",
	}, DoorQuestions(sch),
		"the tenant last; a type with no query of its own cannot be asked; a type that holds no grants is not asked")
}

func TestATenantThatHoldsNoGrantsIsStillAskedAbout(t *testing.T) {
	sch := test.LoadSchemaFromString(t, `
type Workspace @tenant { name: String! @id }
type Candidate { id: ID! name: String }`)
	require.Empty(t, sch.GrantHolders())
	require.Equal(t, []string{"query { held: queryWorkspace(first: 1) { __typename } }"}, DoorQuestions(sch))
}

func TestASchemaThatMarksNothingLetsNobodyIn(t *testing.T) {
	sch := test.LoadSchemaFromString(t, `
interface IAMResource { id: ID! }
type Workspace implements IAMResource { name: String! @id }`)
	require.Equal(t, "", sch.Tenant())
	require.Empty(t, DoorQuestions(sch), "the names mean nothing: only the markers do")
	require.Empty(t, DoorQuestions(nil))
}

func TestWhatATypeSaysAboutTenancy(t *testing.T) {
	sch := test.LoadSchemaFromString(t, doorSchema)
	typ := func(query string) schema.Type {
		op, err := sch.Operation(&schema.Request{Query: "query { " + query + " { __typename } }"})
		require.NoError(t, err)
		return op.Queries()[0].Type()
	}
	require.True(t, typ("queryWorkspace").IsTenant())
	require.True(t, typ("queryWorkspace").HoldsGrants(), "through the interface")
	require.False(t, typ("queryGroup").IsTenant())
	require.True(t, typ("queryGroup").HoldsGrants())
	require.True(t, typ("queryBoard").HoldsGrants(), "marked itself")
	require.False(t, typ("queryCandidate").HoldsGrants())
	require.Equal(t, "Workspace", typ("queryCandidate").Tenant(), "any type can say which type the tenant is")
}
