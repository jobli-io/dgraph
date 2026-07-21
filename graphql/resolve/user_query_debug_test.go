package resolve

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v25/dql"
	"github.com/hypermodeinc/dgraph/v25/graphql/authorization"
	"github.com/hypermodeinc/dgraph/v25/graphql/dgraph"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/graphql/test"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func TestCascadeAuthDQL_UserQueryDebug(t *testing.T) {
	// Mock lambda URL so the parser accepts @lambda directives
	x.Config.GraphQL = z.NewSuperFlag("lambda-url=http://localhost:8086/graphql-worker;").
		MergeAndCheckDefault("lambda-url=;")

	schemaBytes, err := os.ReadFile("/Users/idowuayoola/Documents/jobli/graph/.graphql")
	require.NoError(t, err)

	strSchema := string(schemaBytes)

	// Clean strSchema so that Dgraph.Authorization is at the very end
	// because authorization.Parse takes everything from Dgraph.Authorization to the end of the string.
	authIdx := strings.LastIndex(strSchema, "Dgraph.Authorization")
	require.NotEqual(t, -1, authIdx)
	endOfLine := strings.Index(strSchema[authIdx:], "\n")
	if endOfLine != -1 {
		strSchema = strSchema[:authIdx+endOfLine]
	}

	gqlSchema := test.LoadSchemaFromString(t, string(schemaBytes))

	authParsed, err := authorization.Parse(strSchema)
	require.NoError(t, err)

	metaInfo := &testutil.AuthMeta{
		PublicKey:       authParsed.VerificationKey,
		Namespace:       authParsed.Namespace,
		Algo:            authParsed.Algo,
		ClosedByDefault: authParsed.ClosedByDefault,
	}

	gqlQuery := `
query RestoreAppContext($workspaceId: ID!, $email: String!) {
  data: getWorkspace(id: $workspaceId) {
    id
    name
    displayName
    phone
    website
    email
    abn
    hasNoteType(order: {asc: name}) @cascade(fields: ["name"]) {
      id
      xId
      sId
      name
      iconfont
      uiResourceTypes
      createdBy {
        id
        displayName
        firstName
        lastName
        email
        hasPhoto {
          id
          imageSmall
          __typename
        }
        __typename
      }
      __typename
    }
    hasIAMRole {
      id
      name
      permission
      iconfont
      __typename
    }
    hasPhoto {
      id
      imageSmall
      postPolicy {
        url
        fields {
          key
          value
          __typename
        }
        __typename
      }
      __typename
    }
    hasUser(filter: {email: {eq: $email}}) {
      id
      userId
      email
      status
      firstName
      lastName
      position
      mobile
      displayName
      hasAddress {
        id
        street
        city
        postalCode
        country
        countryCode
        state
        formattedAddress
        coordinates {
          latitude
          longitude
          __typename
        }
        __typename
      }
      hasIAMBinding {
        id
        forRole {
          id
          name
          permission
          __typename
        }
        forResource {
          id
          __typename
        }
        __typename
      }
      hasPhoto {
        id
        image
        imageSmall
        postPolicy {
          url
          fields {
            key
            value
            __typename
          }
          __typename
        }
        __typename
      }
      __typename
    }
    hasGroup(filter: {deleted: false, or: {not: {has: deleted}}}) {
      id
      name
      iconfont
      __typename
    }
    hasJobBoard(filter: {managedBy: {deleted: false}}) {
      id
      xId
      sId
      name
      managedBy {
        id
        settingsData
        __typename
      }
      imageUrl
      schema
      uischema
      data
      externalValidationUrl
      __typename
    }
    hasPlugin(filter: {deleted: false}) {
      id
      colorIconUrl
      manifest
      settingsData
      status {
        code
        message
        __typename
      }
      __typename
    }
    hasEmailService {
      id
      domains {
        id
        valid
        __typename
      }
      __typename
    }
    __typename
  }
}
`

	op, err := gqlSchema.Operation(&schema.Request{
		Query: gqlQuery,
		Variables: map[string]interface{}{
			"workspaceId": "0x26e943",
			"email":       "idowu@gorillajobs.com.au",
		},
	})
	require.NoError(t, err)
	gqlQry := test.GetQuery(t, op)

	metaInfo.AuthVars = map[string]interface{}{
		"sub":   "e8CLjFdOVJeCYirbV5SDQ6yfUg03",
		"SUB":   "e8CLjFdOVJeCYirbV5SDQ6yfUg03",
		"ws":    "test",
		"WS":    "test",
		"email": "idowu@gorillajobs.com.au",
		"EMAIL": "idowu@gorillajobs.com.au",
	}
	ctx, err := metaInfo.AddClaimsToContext(context.Background())
	require.NoError(t, err)

	rewriter := NewQueryRewriter()
	dgQuery, err := rewriter.Rewrite(ctx, gqlQry)
	require.NoError(t, err)

	actual := dgraph.AsString(dgQuery)
	t.Logf("Generated DQL:\n%s", actual)

	// Always validate: DQL must parse cleanly with no unused variables.
	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "DQL should parse without unused-variable errors")
}
