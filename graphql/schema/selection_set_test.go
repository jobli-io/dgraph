/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/hypermodeinc/dgraph/v25/x"
	"github.com/stretchr/testify/require"
)

func TestSelectionSetString_And_GetBodyForLambda(t *testing.T) {
	schemaStr := `
type Workspace {
	id: ID!
	name: String! @search(by: [hash])
}

type User {
	id: ID!
	email: String!
	firstName: String
	lastName: String
	inWorkspace: [Workspace]
}

type Query {
	me: [User] @lambda
}
`

	schHandler, errs := NewHandler(schemaStr, false)
	require.NoError(t, errs)

	sch, err := FromString(schHandler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err)

	queryStr := `
query ($ws: String) {
	me {
		id
		email
		aliasName: firstName
		inWorkspace(filter: { name: { eq: $ws } }) {
			id
			name
		}
	}
}
`

	vars := map[string]interface{}{
		"ws": "AcmeCorp",
	}

	op, err := sch.Operation(&Request{
		Query:     queryStr,
		Variables: vars,
	})
	require.NoError(t, err)
	require.NotNil(t, op)

	queries := op.Queries()
	require.Len(t, queries, 1)

	meQuery := queries[0]
	require.Equal(t, "me", meQuery.Name())

	selStr := meQuery.SelectionSetString()
	require.NotEmpty(t, selStr)

	// Verify that SelectionSetString contains requested fields, aliases, nested relations, and inlined variables
	require.True(t, strings.HasPrefix(strings.TrimSpace(selStr), "{"))
	require.True(t, strings.HasSuffix(strings.TrimSpace(selStr), "}"))
	require.Contains(t, selStr, "id")
	require.Contains(t, selStr, "email")
	require.Contains(t, selStr, "aliasName: firstName")
	require.Contains(t, selStr, "inWorkspace")
	// Variable $ws should be inlined as "AcmeCorp"
	require.Contains(t, selStr, `eq: "AcmeCorp"`)
	require.Contains(t, selStr, "name")

	// Test GetBodyForLambda
	body := GetBodyForLambda(context.Background(), meQuery, nil, nil)
	require.NotNil(t, body)
	require.Equal(t, "Query.me", body["resolver"])

	info, ok := body["info"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "me", info["fieldName"])
	require.Equal(t, selStr, info["selectionSet"])
}

func TestSelectionSetString_PaginationAndFragments(t *testing.T) {
	schemaStr := `
interface Named {
	name: String!
}

type Role implements Named {
	id: ID!
	name: String! @search(by: [hash])
}

type User {
	id: ID!
	email: String!
	roles: [Role]
}

type Query {
	me: [User] @lambda
}
`

	schHandler, errs := NewHandler(schemaStr, false)
	require.NoError(t, errs)

	sch, err := FromString(schHandler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err)

	queryStr := `
query ($limit: Int) {
	me {
		id
		email
		roles(first: $limit, order: { asc: name }) {
			... on Role {
				id
				name
			}
		}
	}
}
`

	vars := map[string]interface{}{
		"limit": int64(5),
	}

	op, err := sch.Operation(&Request{
		Query:     queryStr,
		Variables: vars,
	})
	require.NoError(t, err)

	queries := op.Queries()
	require.Len(t, queries, 1)

	selStr := queries[0].SelectionSetString()
	require.NotEmpty(t, selStr)
	require.Contains(t, selStr, "roles(first: 5, order: {asc:name})")
	require.Contains(t, selStr, "id")
	require.Contains(t, selStr, "name")
}
