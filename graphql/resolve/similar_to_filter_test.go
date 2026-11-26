package resolve

import (
	"context"
	"strings"
	"testing"

	"github.com/dgraph-io/dgraph/v25/dql"
	"github.com/dgraph-io/dgraph/v25/graphql/dgraph"
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/graphql/test"
	"github.com/stretchr/testify/require"
)

func TestAppendSimilarToOptionArgs(t *testing.T) {
	base := []dql.Arg{
		{Value: "JobCategory.search"},
		{Value: "3"},
		{Value: `"[0.1,0.2]"`},
	}

	// Without distance or ef, no extra positional args should be appended.
	noOpts := appendSimilarToOptionArgs(append([]dql.Arg(nil), base...), map[string]interface{}{
		"topK": 3,
		"text": "CTO",
	})
	require.Len(t, noOpts, 3)

	// With distance and ef, named parameters must be emitted instead of positional args.
	withOpts := appendSimilarToOptionArgs(append([]dql.Arg(nil), base...), map[string]interface{}{
		"topK":     3,
		"distance": 0.45,
		"ef":       64,
	})
	require.Equal(t, []dql.Arg{
		{Value: "JobCategory.search"},
		{Value: "3"},
		{Value: `"[0.1,0.2]"`},
		{Value: "ef: 64"},
		{Value: "distance_threshold: 0.45"},
	}, withOpts)
}

func TestApplySimilarToPreFilterWithAuth(t *testing.T) {
	simQry := &dql.GraphQuery{
		Attr: "var",
		Func: &dql.Function{
			Name: "similar_to",
			Args: []dql.Arg{
				{Value: "JobCategory.search"},
				{Value: "3"},
				{Value: `"[0.1,0.2]"`},
			},
		},
		Children: []*dql.GraphQuery{{Attr: "uid", Var: "queryJobCategory_search_similar"}},
	}
	rootQry := &dql.GraphQuery{
		Attr: "var",
		Var:  "JobCategoryRoot",
		Func: &dql.Function{Name: "uid", Args: []dql.Arg{{Value: "JobCategory1"}}},
		Args: map[string]string{"first": "3"},
		Filter: &dql.FilterTree{
			Func: &dql.Function{Name: "uid", Args: []dql.Arg{{Value: "JobCategory_Auth1"}}},
		},
	}
	varQry := &dql.GraphQuery{
		Attr: "var",
		Var:  "JobCategory1",
		Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: "JobCategory"}}},
		Filter: &dql.FilterTree{
			Op: "and",
			Child: []*dql.FilterTree{
				{Func: &dql.Function{Name: "eq", Args: []dql.Arg{{Value: "JobCategory.name"}, {Value: `"Tech"`}}}},
				{Func: &dql.Function{Name: "uid", Args: []dql.Arg{{Value: "queryJobCategory_search_similar"}}}},
			},
		},
	}
	mainQry := &dql.GraphQuery{
		Attr: "queryJobCategory",
		Func: &dql.Function{Name: "uid", Args: []dql.Arg{{Value: "JobCategoryRoot"}}},
		Children: []*dql.GraphQuery{
			{Attr: "JobCategory.name"},
		},
	}
	authQry := &dql.GraphQuery{
		Attr:     "var",
		Func:     &dql.Function{Name: "type", Args: []dql.Arg{{Value: "Workspace"}}},
		Children: []*dql.GraphQuery{{Attr: "Workspace.hasJobCategory", Var: "JobCategory_Auth1"}},
	}

	res := applySimilarToPreFilter([]*dql.GraphQuery{mainQry, simQry, rootQry, varQry, authQry}, nil)
	dqlStr := dgraph.AsString(res)

	// 1. similar_to must receive uids: uid(JobCategoryRoot_PreVec)
	require.Contains(t, dqlStr, `similar_to(JobCategory.search, 3, "[0.1,0.2]", uids: uid(JobCategoryRoot_PreVec))`)
	// 2. varQry must only have the hard filter (eq), not uid(queryJobCategory_search_similar)
	require.NotNil(t, varQry.Filter)
	require.NotNil(t, varQry.Filter.Func)
	require.Equal(t, "eq", varQry.Filter.Func.Name)
	// 3. JobCategoryRoot narrows JobCategoryRoot_PreVec post-vector with first: 3 and uid(queryJobCategory_search_similar)
	require.Equal(t, "JobCategoryRoot_PreVec", rootQry.Func.Args[0].Value)
	require.Equal(t, "3", rootQry.Args["first"])
	require.Equal(t, "uid", rootQry.Filter.Func.Name)
	require.Equal(t, "queryJobCategory_search_similar", rootQry.Filter.Func.Args[0].Value)
	// 4. mainQry still consumes JobCategoryRoot directly (preserving selectionAuth / aggregate / groupBy semantics)
	require.Nil(t, mainQry.Filter)
	require.Equal(t, "JobCategoryRoot", mainQry.Func.Args[0].Value)
	// 5. Resulting DQL must parse cleanly
	_, err := dql.Parse(dql.Request{Str: dqlStr})
	require.NoError(t, err)
}

func TestApplySimilarToPreFilterWithoutAuth(t *testing.T) {
	simQry := &dql.GraphQuery{
		Attr: "var",
		Func: &dql.Function{
			Name: "similar_to",
			Args: []dql.Arg{
				{Value: "JobCategory.search"},
				{Value: "3"},
				{Value: `"[0.1,0.2]"`},
			},
		},
		Children: []*dql.GraphQuery{{Attr: "uid", Var: "queryJobCategory_search_similar"}},
	}
	mainQry := &dql.GraphQuery{
		Attr: "queryJobCategory",
		Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: "JobCategory"}}},
		Filter: &dql.FilterTree{
			Op: "and",
			Child: []*dql.FilterTree{
				{Func: &dql.Function{Name: "eq", Args: []dql.Arg{{Value: "JobCategory.name"}, {Value: `"Tech"`}}}},
				{Func: &dql.Function{Name: "uid", Args: []dql.Arg{{Value: "queryJobCategory_search_similar"}}}},
			},
		},
		Children: []*dql.GraphQuery{
			{Attr: "JobCategory.name"},
		},
	}

	res := applySimilarToPreFilter([]*dql.GraphQuery{mainQry, simQry}, nil)
	dqlStr := dgraph.AsString(res)

	require.Contains(t, dqlStr, `similar_to(JobCategory.search, 3, "[0.1,0.2]", uids: uid(queryJobCategory_search_similar_allowed))`)
	require.Len(t, res, 3)
	require.Equal(t, "uid", mainQry.Func.Name)
	require.Equal(t, "queryJobCategory_search_similar_allowed", mainQry.Func.Args[0].Value)
	require.Equal(t, "queryJobCategory_search_similar", mainQry.Filter.Func.Args[0].Value)

	_, err := dql.Parse(dql.Request{Str: dqlStr})
	require.NoError(t, err)
}

func TestApplySimilarToPreFilterNoHardFilterNoAuth(t *testing.T) {
	simQry := &dql.GraphQuery{
		Attr: "var",
		Func: &dql.Function{
			Name: "similar_to",
			Args: []dql.Arg{
				{Value: "JobCategory.search"},
				{Value: "3"},
				{Value: `"[0.1,0.2]"`},
			},
		},
		Children: []*dql.GraphQuery{{Attr: "uid", Var: "queryJobCategory_search_similar"}},
	}
	mainQry := &dql.GraphQuery{
		Attr: "queryJobCategory",
		Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: "JobCategory"}}},
		Filter: &dql.FilterTree{
			Func: &dql.Function{Name: "uid", Args: []dql.Arg{{Value: "queryJobCategory_search_similar"}}},
		},
		Children: []*dql.GraphQuery{{Attr: "JobCategory.name"}},
	}

	res := applySimilarToPreFilter([]*dql.GraphQuery{mainQry, simQry}, nil)
	require.Len(t, res, 2)
	require.Len(t, simQry.Func.Args, 3)
}

const similarToPreFilterValidationSchema = `
type User {
  id: ID!
  userId: String! @id @search(by: [hash])
}

type Workspace @auth(
  query: { rule: """
    query($sub: String!, $ws: String!) {
      queryWorkspace(filter: { name: { eq: $ws } }) {
        hasUser(filter: { userId: { eq: $sub } }) {
          __typename
        }
      }
    }
  """ }
) {
  id: ID!
  name: String! @id @search(by: [hash])
  hasUser: [User]
  hasJobCategory: [JobCategory] @hasInverse(field: inWorkspace)
  hasCandidate: [Candidate] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  id: ID!
  inWorkspace: Workspace @search
}

interface CommonWorkspaceMember @auth(
  query: { rule: """
    query($sub: String!, $ws: String!) {
      queryCommonWorkspaceMember {
        ...on WorkspaceMember {
          inWorkspace(filter: { name: { eq: $ws } }) {
            hasUser(filter: { userId: { eq: $sub } }) {
              __typename
            }
          }
        }
      }
    }
  """ }
) {
  id: ID!
}

type JobCategory implements WorkspaceMember & CommonWorkspaceMember {
  id: ID!
  name: String! @search(by: [hash, term])
  type: String! @search(by: [hash, term])
  search: [Float!] @search(by: ["hnsw"]) @embedding
}

type Candidate implements WorkspaceMember & CommonWorkspaceMember {
  id: ID!
  fullName: String! @search(by: [term])
  deleted: Boolean @search
  hasApplication: [Application] @hasInverse(field: hasCandidate)
  search: [Float!] @search(by: ["hnsw"]) @embedding
}

type Application {
  id: ID!
  deleted: Boolean @search
  hasCandidate: Candidate @search @hasInverse(field: hasApplication)
}
`

func TestEndToEndRewrite_SimilarToPreFilter(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, similarToPreFilterValidationSchema)
	claims := map[string]interface{}{
		"sub": "user-1",
		"ws":  "dc98a028",
	}

	rewriteAndValidate := func(t *testing.T, gqlSrc string) string {
		t.Helper()
		op, err := gqlSchema.Operation(&schema.Request{Query: gqlSrc})
		require.NoError(t, err)
		gqlQuery := test.GetQuery(t, op)

		metaInfo.AuthVars = claims
		ctx, err := metaInfo.AddClaimsToContext(context.Background())
		require.NoError(t, err)

		rewriter := NewQueryRewriter()
		dgQuery, dgVars, err := rewriter.Rewrite(ctx, gqlQuery)
		require.NoError(t, err)

		dqlStr := dgraph.AsString(dgQuery)
		_, parseErr := dql.Parse(dql.Request{Str: dqlStr, Variables: dgVars})
		require.NoError(t, parseErr, "generated DQL failed dql.Parse:\n%s", dqlStr)
		return dqlStr
	}

	t.Run("root queryJobCategory with CommonWorkspaceMember auth and pagination", func(t *testing.T) {
		dqlStr := rewriteAndValidate(t, `
			query {
				queryJobCategory(
					filter: {
						search: { similarToVector: { vector: [0.1, 0.2], topK: 3 } }
					}
					first: 3
				) {
					id
					name
					type
				}
			}
		`)
		require.Contains(t, dqlStr, `similar_to(JobCategory.search, 3, "[0.1,0.2]", uids: uid(JobCategoryRoot_PreVec))`)
		require.Contains(t, dqlStr, `JobCategoryRoot as var(func: uid(JobCategoryRoot_PreVec), first: 3) @filter(uid(queryJobCategory_search_similar))`)
	})

	t.Run("root queryJobCategory with child selectionAuth uses narrowed JobCategoryRoot", func(t *testing.T) {
		dqlStr := rewriteAndValidate(t, `
			query {
				queryJobCategory(
					filter: {
						name: { eq: "Information & Communication Technology" }
						search: { similarToVector: { vector: [0.1, 0.2], topK: 3 } }
					}
				) {
					id
					name
					inWorkspace {
						id
						name
					}
				}
			}
		`)
		require.Contains(t, dqlStr, `uids: uid(JobCategoryRoot_PreVec)`)
		// Child selectionAuth for inWorkspace must NOT traverse from JobCategoryRoot_PreVec.
		for _, line := range strings.Split(dqlStr, "\n") {
			if strings.Contains(line, "inWorkspace") {
				require.NotContains(t, line, "JobCategoryRoot_PreVec")
			}
		}
	})

	t.Run("similarToId with auth and hard filter combines val(v1) and uids option", func(t *testing.T) {
		dqlStr := rewriteAndValidate(t, `
			query {
				queryJobCategory(
					filter: {
						name: { eq: "Science & Technology" }
						search: { similarToId: { id: "0x29bd7d", topK: 3 } }
					}
				) {
					id
					name
				}
			}
		`)
		require.Contains(t, dqlStr, `similar_to(JobCategory.search, 3, val(queryJobCategory_search_vec), uids: uid(JobCategoryRoot_PreVec))`)
	})

	t.Run("aggregateJobCategory with auth and similarToVector", func(t *testing.T) {
		dqlStr := rewriteAndValidate(t, `
			query {
				aggregateJobCategory(
					filter: {
						search: { similarToVector: { vector: [0.1, 0.2], topK: 3 } }
					}
				) {
					count
				}
			}
		`)
		require.Contains(t, dqlStr, `similar_to(JobCategory.search, 3, "[0.1,0.2]", uids: uid(JobCategoryRoot_PreVec))`)
	})

	t.Run("nested compound filter on Application -> Candidate pre-filters Candidate similar_to", func(t *testing.T) {
		dqlStr := rewriteAndValidate(t, `
			query {
				queryApplication(
					filter: {
						deleted: false
						hasCandidate: {
							deleted: false
							search: { similarToVector: { vector: [0.5, 0.5], topK: 10 } }
						}
					}
				) {
					id
				}
			}
		`)
		require.Contains(t, dqlStr, `similar_to(Candidate.search, 10, "[0.5,0.5]", uids: uid(queryApplication_hasCandidateRoot_PreVec))`)
	})
}
