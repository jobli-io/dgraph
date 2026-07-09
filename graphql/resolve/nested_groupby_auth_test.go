/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/dql"
	"github.com/hypermodeinc/dgraph/v25/graphql/dgraph"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/graphql/test"

	"github.com/dgraph-io/gqlparser/v2/ast"
	"github.com/dgraph-io/gqlparser/v2/parser"
	"github.com/dgraph-io/gqlparser/v2/validator"
)

const nestedGroupByAuthSchema = `
interface WorkspaceMember @generate(query: {aggregate: false}) {
  id: ID!
  inWorkspace: Workspace! @generate(mutation: { add: false, update: false, delete: false })
}

type Workspace {
  name: String @search(by: [exact])
  ownedBy: User
}

type User {
  email: String! @id
  hasPrimaryGroup: Group
}

type Group implements WorkspaceMember @auth(
  query: { rule: """
    query($EMAIL: String!) {
      queryGroup {
        inUsers(filter: { email: { eq: $EMAIL } }) { __typename }
      }
    }
  """ }
) {
  id: ID!
  name: String @search(by: [exact])
  inUsers: [User]
}

type Company {
  name: String @search(by: [exact])
  hasPrimaryGroup: Group
}
`

func TestQueryRewritingNestedGroupByAuth(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, nestedGroupByAuthSchema)

	gqlSrc := `query {
		groupByCompany(groupBy: [{ field: { hasPrimaryGroup: { name: true } } }]) {
			count
			groupKeys {
				path
				value
			}
		}
	}`

	jwtVars := map[string]interface{}{
		"EMAIL": "user@test.com",
	}

	op, err := gqlSchema.Operation(&schema.Request{Query: gqlSrc})
	if err != nil {
		for _, qName := range gqlSchema.Queries(schema.FilterQuery) {
			t.Logf("Filter Query: %s", qName)
		}
		for _, qName := range gqlSchema.Queries(schema.GetQuery) {
			t.Logf("Get Query: %s", qName)
		}
		for _, qName := range gqlSchema.Queries(schema.GroupByQuery) {
			t.Logf("GroupBy Query: %s", qName)
		}
	}
	require.NoError(t, err)
	gqlQuery := test.GetQuery(t, op)

	metaInfo.AuthVars = jwtVars
	ctx, err := metaInfo.AddClaimsToContext(context.Background())
	require.NoError(t, err)

	rewriter := NewQueryRewriter()
	dgQuery, err := rewriter.Rewrite(ctx, gqlQuery)
	require.NoError(t, err)

	actual := dgraph.AsString(dgQuery)
	t.Logf("Generated DQL:\n%s", actual)

	// Assert that:
	// 1. Group_Auth3 helper is defined with cascade filter.
	// 2. Company.hasPrimaryGroup edge has @filter(uid(Group_Auth3)) inline!
	// 3. Root groupByCompany uses @groupby(val(__gby_0))
	require.Contains(t, actual, "Group_Auth3 as var(func: type(Group))")
	require.Contains(t, actual, "Company.hasPrimaryGroup @filter(uid(Group_Auth3))")
	require.Contains(t, actual, "@groupby(val(__gby_0))")

	// Always validate: DQL must parse cleanly with no unused variables.
	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "DQL should parse without unused-variable errors")
}

func TestQueryRewritingNestedGroupByAuthMultiLevel(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, nestedGroupByAuthSchema)

	gqlSrc := `query {
		groupByCompany(groupBy: [{ field: { hasPrimaryGroup: { inWorkspace: { name: true } } } }]) {
			count
			groupKeys {
				path
				value
			}
		}
	}`

	jwtVars := map[string]interface{}{
		"EMAIL": "user@test.com",
	}

	op, err := gqlSchema.Operation(&schema.Request{Query: gqlSrc})
	require.NoError(t, err)
	gqlQuery := test.GetQuery(t, op)

	metaInfo.AuthVars = jwtVars
	ctx, err := metaInfo.AddClaimsToContext(context.Background())
	require.NoError(t, err)

	rewriter := NewQueryRewriter()
	dgQuery, err := rewriter.Rewrite(ctx, gqlQuery)
	require.NoError(t, err)

	actual := dgraph.AsString(dgQuery)
	t.Logf("Generated DQL:\n%s", actual)

	// Assert intermediate variables and aggregations:
	// 1. Group_Auth3 as var(func: type(Group)) (auth query)
	// 2. Company.hasPrimaryGroup filtered with uid(Group_Auth3)
	// 3. __gby_0_p0 as max(val(__gby_0_c)) defined inside Company.hasPrimaryGroup
	// 4. __gby_0 as max(val(__gby_0_p0)) defined at root level
	require.Contains(t, actual, "as var(func: type(Group))")
	require.Contains(t, actual, "Company.hasPrimaryGroup @filter(uid(")
	require.Contains(t, actual, "__gby_0_p0 as max(val(__gby_0_c))")
	require.Contains(t, actual, "__gby_0 as max(val(__gby_0_p0))")
	require.Contains(t, actual, "@groupby(val(__gby_0))")

	// Verify that the generated DQL parses perfectly with no level-aggregation errors.
	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "DQL should parse perfectly with no level-aggregation or unused-variable errors")
}

func TestPrintFields(t *testing.T) {
	handler, err := schema.NewHandler(nestedGroupByAuthSchema, false)
	require.NoError(t, err)
	gqlStr := handler.GQLSchema()
	doc, parseErr := parser.ParseSchemas(validator.Prelude, &ast.Source{Input: gqlStr})
	require.Nil(t, parseErr)

	astSch, gqlErr := validator.ValidateSchemaDocument(doc)
	require.Nil(t, gqlErr)

	qDef := astSch.Types["Query"]
	if qDef != nil {
		for _, f := range qDef.Fields {
			t.Logf("Query Field AST: %s", f.Name)
		}
	}
	for _, typeName := range []string{"CompanyGroupByField", "GroupGroupByField", "WorkspaceGroupByField", "UserGroupByField"} {
		def := astSch.Types[typeName]
		if def != nil {
			t.Logf("Type %s fields:", typeName)
			for _, f := range def.Fields {
				t.Logf("  - %s: %s", f.Name, f.Type.Name())
			}
		} else {
			t.Logf("Type %s NOT FOUND", typeName)
		}
	}
}
