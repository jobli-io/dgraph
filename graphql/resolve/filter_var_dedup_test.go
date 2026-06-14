/*
Regression tests for the filter-path var deduplication bug.

Background
----------
When addAuthQueries deduplicates fldAuthQueries it produces an authVarSubst
map (e.g. {"Workspace_Auth14": "Workspace_Auth12"}).  That map must be applied
to ALL places that can reference auth var names:

 1. filter      – the auth predicate attached to rootQry  ✓ (original code)
 2. dgQuery[1:] – filter-path var blocks from addArgumentsToField  ← BUG WAS HERE
 3. selectionAuth – field-selection auth blocks                    ✓ (added earlier)

Without fix #2 a block like

	data_and_0_and_2_inGroup_inWorkspaceRoot as var(…) @filter(uid(Workspace_Auth14)…)

kept referencing the deduplicated-away name after its definition block had been
removed, causing Dgraph to report "Some variables are used but not defined".

We use dql.Parse() to validate — the same approach as the existing queryRewriting
test harness (see auth_test.go:447).
*/
package resolve

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hypermodeinc/dgraph/v25/dql"
	"github.com/hypermodeinc/dgraph/v25/graphql/authorization"
	"github.com/hypermodeinc/dgraph/v25/graphql/dgraph"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/graphql/test"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/stretchr/testify/require"
)

// filterVarDedupSchema is a minimal schema that precisely mirrors the
// Contact/Group/Workspace auth rule structure that triggered the production
// "Workspace_Auth14 used but not defined" bug.
//
// The key structural property: Contact has MULTIPLE auth rules that each
// traverse Group → Workspace with a DIFFERENT filter (hasMember vs hasAdmin).
// This causes two near-identical Workspace auth var blocks to be emitted into
// fldAuthQueries. deduplicateAuthVarBlocks then cascades through sub-vars:
//
//	Workspace_Auth14_hasAdmin  → dropped (maps to Workspace_Auth12_hasMember)
//	Workspace_Auth14           → dropped (now identical to Workspace_Auth12)
//
// When queryContact is called with a filter on inGroup.inWorkspace (which
// generates filter-path var blocks in dgQuery[1:] that reference Workspace auth
// vars), those references must also be updated by the substitution. That was
// the bug.
const filterVarDedupSchema = `
type Workspace @auth(
  query: { or: [
    { rule: """query($USER: String!) {
      queryWorkspace {
        hasMember(filter: { username: { eq: $USER } }) { __typename }
      }
    }""" },
    { rule: """query($USER: String!) {
      queryWorkspace {
        hasMember(filter: { username: { eq: $USER } }) { __typename }
      }
    }""" }
  ]}
) {
  wsID: ID!
  name: String! @search(by: [hash])
  hasGroup: [Group] @hasInverse(field: inWorkspace)
  hasMember: [User]
}

type Group @auth(
  query: { rule: """query($USER: String!) {
    queryGroup {
      inWorkspace {
        hasMember(filter: { username: { eq: $USER } }) { __typename }
      }
    }
  }""" }
) {
  groupID: ID!
  name: String! @search(by: [hash])
  inWorkspace: Workspace! @hasInverse(field: hasGroup) @search
  hasContact: [Contact] @hasInverse(field: inGroup)
}

type User {
  username: String! @id
}

type Contact @auth(
  query: { or: [
    { rule: """query($USER: String!) {
      queryContact {
        inGroup {
          inWorkspace {
            hasMember(filter: { username: { eq: $USER } }) { __typename }
          }
        }
      }
    }""" },
    { rule: """query($USER: String!) {
      queryContact {
        inGroup {
          inWorkspace {
            hasMember(filter: { username: { eq: $USER } }) { __typename }
          }
        }
      }
    }""" }
  ]}
) {
  id: ID!
  firstName: String @search(by: [hash])
  lastName:  String @search(by: [hash])
  inGroup:   Group @hasInverse(field: hasContact) @search
  ownedBy:   User
}
`

// loadAuthSchemaAndMeta loads the standard e2e auth schema and returns the
// schema string and an AuthMeta suitable for use in unit tests.
func loadAuthSchemaAndMeta(t *testing.T) (string, *testutil.AuthMeta) {
	t.Helper()
	sch, err := os.ReadFile("../e2e/auth/schema.graphql")
	require.NoError(t, err)

	result, err := testutil.AppendAuthInfo(
		sch,
		jwt.SigningMethodHS256.Name,
		"../e2e/auth/sample_public_key.pem",
		false,
	)
	require.NoError(t, err)
	strSchema := string(result)

	authMeta, err := authorization.Parse(strSchema)
	require.NoError(t, err)

	meta := &testutil.AuthMeta{
		PublicKey:       authMeta.VerificationKey,
		Namespace:       authMeta.Namespace,
		Algo:            authMeta.Algo,
		Header:          authMeta.Header,
		ClosedByDefault: authMeta.ClosedByDefault,
		AuthVars:        map[string]interface{}{"USER": "user1", "ROLE": "ADMIN"},
	}
	return strSchema, meta
}

// loadMinimalIAMSchema loads the inline minimal schema with embedded HS256 auth
// config and returns schema string + AuthMeta.
func loadMinimalIAMSchema(t *testing.T) (string, *testutil.AuthMeta) {
	t.Helper()

	strippedSchema := filterVarDedupSchema
	result, err := testutil.AppendAuthInfo(
		[]byte(strippedSchema),
		jwt.SigningMethodHS256.Name,
		"../e2e/auth/sample_public_key.pem",
		false,
	)
	require.NoError(t, err)
	strSchema := string(result)

	authMeta, err := authorization.Parse(strSchema)
	require.NoError(t, err)

	meta := &testutil.AuthMeta{
		PublicKey:       authMeta.VerificationKey,
		Namespace:       authMeta.Namespace,
		Algo:            authMeta.Algo,
		Header:          authMeta.Header,
		ClosedByDefault: authMeta.ClosedByDefault,
		AuthVars:        map[string]interface{}{"USER": "user1"},
	}
	return strSchema, meta
}

// assertDQLValid calls dql.Parse on the string representation of the generated
// query list and fails the test if parsing fails.  dql.Parse detects "used but
// not defined" variables among other structural errors, exactly as Dgraph does
// on the server side.
func assertDQLValid(t *testing.T, queries []*dql.GraphQuery) {
	t.Helper()
	str := dgraph.AsString(queries)
	_, err := dql.Parse(dql.Request{Str: str})
	require.NoError(t, err,
		"Generated DQL failed dql.Parse (likely 'used but not defined' var):\n%s", str)
}

// rewriteGQL is a helper that compiles a GQL query string and rewrites it to
// DQL using the standard QueryRewriter with auth context applied.
func rewriteGQL(t *testing.T, strSchema string, meta *testutil.AuthMeta, gqlQuery string, variables map[string]interface{}) []*dql.GraphQuery {
	t.Helper()
	gqlSchema := test.LoadSchemaFromString(t, strSchema)
	op, err := gqlSchema.Operation(&schema.Request{Query: gqlQuery, Variables: variables})
	require.NoError(t, err)
	q := test.GetQuery(t, op)

	ctx, err := meta.AddClaimsToContext(context.Background())
	require.NoError(t, err)

	rewriter := NewQueryRewriter()
	queries, err := rewriter.Rewrite(ctx, q)
	require.NoError(t, err)
	return queries
}

// TestFilterPathVarDedupBug_MinimalSchema is the canonical regression test.
//
// It uses filterVarDedupSchema: a minimal Contact/Group/Workspace schema whose
// Contact auth rules generate TWO near-identical Workspace auth var blocks in
// fldAuthQueries (one via hasMember, one via hasAdmin). Deduplication of those
// blocks cascades (sub-var renamed → parent var body changes → parent is now
// identical to earlier var → parent dropped too).
//
// When the query has a filter on inGroup.inWorkspace (producing filter-path var
// blocks in dgQuery[1:] that reference e.g. Workspace_Auth14), the dedup
// substitution {"Workspace_Auth14": "Workspace_Auth12"} must be applied to
// those filter-path blocks too — otherwise Dgraph reports "used but not defined".
//
// This test would FAIL on the unfixed code and PASS with the fix in place.
func TestFilterPathVarDedupBug_MinimalSchema(t *testing.T) {
	strSchema, meta := loadMinimalIAMSchema(t)

	// The EXACT production filter that triggers the bug:
	//   inGroup: { inWorkspace: { id: "0x26e943" } }
	// This causes buildFilter to traverse Group → Workspace and call
	// addAuthQueries(Workspace) which generates Workspace auth vars inside
	// the filter-path var blocks.  Dedup of those Workspace auth vars must
	// update nested child @filter expressions (e.g.
	//   Contact_Auth2's child: inWorkspace @filter(uid(Workspace_Auth14)))
	// not just the top-level Filter of each surviving var block.
	queries := rewriteGQL(t, strSchema, meta, `
		query {
			queryContact(filter: {
				and: [
					{ has: [inGroup] },
					{ inGroup: { inWorkspace: { wsID: "0x26e943" } } }
				]
			}) {
				id
				firstName
				lastName
				inGroup {
					name
					inWorkspace { name }
				}
				ownedBy { username }
			}
		}`, nil)

	assertDQLValid(t, queries)
}

// TestFilterPathVarDedupBug_MinimalSchema_WithVariables exercises the same
// pattern with variables-driven filter (as used by the production GQL client).
func TestFilterPathVarDedupBug_MinimalSchema_WithVariables(t *testing.T) {
	strSchema, meta := loadMinimalIAMSchema(t)

	// Variables-driven variant — mirrors the production query:
	//   filter: { and: [{has:[inGroup]}, {inGroup:{inWorkspace:{id:"..."}}}] }
	var vars map[string]interface{}
	json.Unmarshal([]byte(`{
		"filter": {
			"and": [
				{"has": ["inGroup"]},
				{"inGroup": {"inWorkspace": {"wsID": "0x26e943"}}}
			]
		}
	}`), &vars)

	queries := rewriteGQL(t, strSchema, meta, `
		query queryContacts($filter: ContactFilter) {
			queryContact(filter: $filter) {
				id
				firstName
				lastName
				inGroup { name inWorkspace { name } }
				ownedBy { username }
			}
		}`, vars)

	assertDQLValid(t, queries)
}

// TestFilterPathVarDedupBug_ColumnFilteredByColID exercises the same class of
// bug using the standard e2e auth test schema (Project/Column/Ticket).
// Column's auth rules traverse inProject.roles(permission: VIEW/EDIT/ADMIN)
// which can generate overlapping Project auth var blocks in fldAuthQueries.
func TestFilterPathVarDedupBug_ColumnFilteredByColID(t *testing.T) {
	strSchema, meta := loadAuthSchemaAndMeta(t)

	queries := rewriteGQL(t, strSchema, meta, `
		query {
			queryColumn(filter: { colID: ["0x1", "0x2"] }) {
				colID
				inProject {
					name
					roles {
						permission
						assignedTo { username }
					}
				}
			}
		}`, nil)

	assertDQLValid(t, queries)
}

// TestFilterPathVarDedupBug_TicketFilteredByTitle exercises deeper nesting.
func TestFilterPathVarDedupBug_TicketFilteredByTitle(t *testing.T) {
	strSchema, meta := loadAuthSchemaAndMeta(t)

	queries := rewriteGQL(t, strSchema, meta, `
		query {
			queryTicket(filter: { title: { anyofterms: "important" } }) {
				title
				onColumn {
					colID
					inProject {
						name
						roles(filter: { permission: { eq: VIEW } }) {
							permission
							assignedTo { username }
						}
					}
				}
				assignedTo { username }
			}
		}`, nil)

	assertDQLValid(t, queries)
}
