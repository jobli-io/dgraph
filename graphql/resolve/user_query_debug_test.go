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

func TestCascadeAuthDQL_GetCompanyDebug(t *testing.T) {
	x.Config.GraphQL = z.NewSuperFlag("lambda-url=http://localhost:8086/graphql-worker;").
		MergeAndCheckDefault("lambda-url=;")

	schemaBytes, err := os.ReadFile("/Users/idowuayoola/Documents/jobli/graph/.graphql")
	require.NoError(t, err)

	strSchema := string(schemaBytes)

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
query GetCompany($id: ID) {
  data: getCompany(id: $id) {
    id
    xId
    name
    abn
    phone
    email
    summary
    bulletPoints
    website
    updatedAt
    hasStatus {
      id
      name
      color
      textColor
      __typename
    }
    hasPhoto {
      id
      imageSmall
      __typename
    }
    ownedBy {
      id
      displayName
      hasPhoto {
        id
        imageSmall
        __typename
      }
      firstName
      lastName
      email
      status
      __typename
    }
    inGroup {
      id
      name
      inWorkspace {
        id
        __typename
      }
      iconfont
      __typename
    }
    hasTag {
      id
      name
      color
      category
      __typename
    }
    hasAttachment {
      id
      fileName
      fileSize
      category
      createdAt
      expiryDate
      downloadLink
      __typename
    }
    hasJobAggregate(filter: {and: [{or: [{deleted: false}, {not: {has: deleted}}]}, {has: [title]}, {or: [{not: {has: class}}, {class: {has: name}}]}]}) {
      count
      __typename
    }
    hasPrimaryAddress {
      id
      phone
      fax
      country
      countryCode
      url
      state
      city
      street
      streetName
      streetNumber
      postalCode
      coordinates {
        latitude
        longitude
        __typename
      }
      formattedAddress
      __typename
    }
    hasPrimaryContact(filter: {or: [{deleted: false}, {not: {has: deleted}}]}) {
      id
      firstName
      lastName
      phone
      email
      hasPhoto {
        id
        imageSmall
        __typename
      }
      hasStatus {
        id
        name
        color
        textColor
        __typename
      }
      inGroup {
        id
        name
        inWorkspace {
          id
          __typename
        }
        __typename
      }
      forCompany {
        id
        inGroup {
          id
          __typename
        }
        __typename
      }
      __typename
    }
    hasParentCompany {
      id
      name
      abn
      phone
      hasStatus {
        id
        name
        color
        textColor
        __typename
      }
      hasPhoto {
        id
        imageSmall
        __typename
      }
      __typename
    }
    hasContact(filter: {or: [{deleted: false}, {not: {has: deleted}}]}) {
      id
      firstName
      lastName
      phone
      email
      hasPhoto {
        id
        imageSmall
        __typename
      }
      hasStatus {
        id
        name
        color
        textColor
        __typename
      }
      inGroup {
        id
        name
        inWorkspace {
          id
          __typename
        }
        __typename
      }
      forCompany {
        id
        inGroup {
          id
          __typename
        }
        __typename
      }
      __typename
    }
    hasSubsidiary {
      id
      name
      abn
      phone
      hasPhoto {
        id
        imageSmall
        __typename
      }
      hasStatus {
        id
        name
        color
        textColor
        __typename
      }
      inGroup {
        id
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
			"id": "0x48a31d",
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

	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "DQL should parse without unused-variable errors")
}

func TestBypassAuth(t *testing.T) {
	sch := `
		type Company @auth(
			query: {
				rule: "query($ws: String!) { queryCompany(filter: { name: { eq: $ws } }) { id } }"
			}
		) {
			id: ID!
			name: String! @search(by: [exact])
			hasContact: [Contact!] @bypassAuth
		}

		type Contact @auth(
			query: {
				rule: "query($email: String!) { queryContact(filter: { email: { eq: $email } }) { id } }"
			}
		) {
			id: ID!
			firstName: String!
			email: String! @search(by: [exact])
			hasAddress: [Address!]
		}

		type Address @auth(
			query: {
				rule: "query($city: String!) { queryAddress(filter: { city: { eq: $city } }) { id } }"
			}
		) {
			id: ID!
			city: String! @search(by: [exact])
		}
	`

	authSchemaBytes, err := testutil.AppendAuthInfo([]byte(sch), "HS256", "", false)
	require.NoError(t, err)

	gqlSchema := test.LoadSchemaFromString(t, string(authSchemaBytes))

	authParsed, err := authorization.Parse(string(authSchemaBytes))
	require.NoError(t, err)

	metaInfo := &testutil.AuthMeta{
		PublicKey:       authParsed.VerificationKey,
		Namespace:       authParsed.Namespace,
		Algo:            authParsed.Algo,
		ClosedByDefault: authParsed.ClosedByDefault,
	}

	gqlQuery := `
		query {
			queryCompany {
				id
				name
				hasContact {
					id
					firstName
					hasAddress {
						id
						city
					}
				}
			}
		}
	`

	op, err := gqlSchema.Operation(&schema.Request{
		Query: gqlQuery,
	})
	require.NoError(t, err)
	gqlQry := test.GetQuery(t, op)

	metaInfo.AuthVars = map[string]interface{}{
		"ws":    "test",
		"WS":    "test",
		"email": "another@example.com",
		"EMAIL": "another@example.com",
		"city":  "Sydney",
		"CITY":  "Sydney",
	}
	ctx, err := metaInfo.AddClaimsToContext(context.Background())
	require.NoError(t, err)

	rewriter := NewQueryRewriter()
	dgQuery, err := rewriter.Rewrite(ctx, gqlQry)
	require.NoError(t, err)

	actual := dgraph.AsString(dgQuery)
	t.Logf("Generated DQL:\n%s", actual)

	// Since we bypassed auth for Contact, we should see the parent Company's auth query block,
	// but we should NOT see any Contact auth query block.
	require.Contains(t, actual, "Company_Auth")    // Company auth block should exist
	require.NotContains(t, actual, "Contact_Auth") // Contact auth block must be completely bypassed!

	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "DQL should parse perfectly")
}

func TestInterfaceMemberTypesFilterAuthOptimization(t *testing.T) {
	// Mock lambda URL so the parser accepts @lambda directives
	x.Config.GraphQL = z.NewSuperFlag("lambda-url=http://localhost:8086/graphql-worker;").
		MergeAndCheckDefault("lambda-url=;")

	schemaBytes, err := os.ReadFile("/Users/idowuayoola/Documents/jobli/graph/.graphql")
	require.NoError(t, err)

	strSchema := string(schemaBytes)
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

	// queryNote has a nested filter: forResource { memberTypes: [Company] }
	// where forResource is an interface-typed field.
	// The auth rewriter should omit compiling auth blocks for non-allowed implementing types (like Candidate, Job).
	gqlQuery := `
		query {
			queryNote(filter: { forResource: { memberTypes: [Company] } }) {
				id
				text
			}
		}
	`

	op, err := gqlSchema.Operation(&schema.Request{
		Query: gqlQuery,
	})
	require.NoError(t, err)
	gqlQry := test.GetQuery(t, op)

	metaInfo.AuthVars = map[string]interface{}{
		"sub":   "ZGifl7RD37Pa0fHOdTZwjsxjKHO2",
		"SUB":   "ZGifl7RD37Pa0fHOdTZwjsxjKHO2",
		"ws":    "test",
		"WS":    "test",
		"email": "test@gorillajobs.app",
		"EMAIL": "test@gorillajobs.app",
	}
	ctx, err := metaInfo.AddClaimsToContext(context.Background())
	require.NoError(t, err)

	rewriter := NewQueryRewriter()
	dgQuery, err := rewriter.Rewrite(ctx, gqlQry)
	require.NoError(t, err)

	actual := dgraph.AsString(dgQuery)
	t.Logf("Generated DQL:\n%s", actual)

	// Since we filtered forResource to only Company:
	// We should see Company auth blocks
	require.Contains(t, actual, "Company_Auth")

	// We should NOT see Candidate or Job auth blocks in the generated DQL!
	require.NotContains(t, actual, "Candidate_Auth")
	require.NotContains(t, actual, "Job_Auth")

	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "DQL should parse perfectly")
}
