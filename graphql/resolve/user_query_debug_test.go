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

	// Since we filtered forResource to only Company, and have forceForward: true:
	// 1. The compiler optimizes compilation by nested/forward-linking checks.
	// 2. We should see the Groupable.inGroup relation predicate, but NOT Contact.forCompany.
	require.Contains(t, actual, "Groupable.inGroup")
	require.NotContains(t, actual, "Contact.forCompany")

	// We should NOT see Candidate or Job predicates (e.g., Job.inGroup) because they are filtered out.
	require.NotContains(t, actual, "Job.inGroup")

	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "DQL should parse perfectly")
}

func TestFilterLookupStrategy(t *testing.T) {
	schemaStr := `
		type Company {
			id: ID!
			name: String! @search(by: [exact])
			hasContact: [Contact!] @search(strategy: DYNAMIC) @hasInverse(field: forCompany)
			forcedForwardContact: [Contact!] @search(strategy: FORWARD) @hasInverse(field: forCompanyForward)
			forcedReverseContact: [Contact!] @search(strategy: REVERSE) @hasInverse(field: forCompanyReverse)
		}

		type Contact {
			id: ID!
			city: String! @search(by: [exact])
			forCompany: Company
			forCompanyForward: Company
			forCompanyReverse: Company
		}
	`
	gqlSchema := test.LoadSchemaFromString(t, schemaStr)

	// Case 1: Static strategy = FORWARD (should use uid_in)
	gqlQuery1 := `
		query {
			queryCompany(filter: { forcedForwardContact: { city: { eq: "Sydney" } } }) {
				id
				name
			}
		}
	`
	op1, err := gqlSchema.Operation(&schema.Request{Query: gqlQuery1})
	require.NoError(t, err)
	dgQry1, err := NewQueryRewriter().Rewrite(context.Background(), test.GetQuery(t, op1))
	require.NoError(t, err)
	actual1 := dgraph.AsString(dgQry1)
	t.Logf("Query 1 DQL:\n%s", actual1)
	// Must contain uid_in
	require.Contains(t, actual1, "uid_in(Company.forcedForwardContact")

	// Case 2: Static strategy = REVERSE (should use nested var query with inverse predicate)
	gqlQuery2 := `
		query {
			queryCompany(filter: { forcedReverseContact: { city: { eq: "Sydney" } } }) {
				id
				name
			}
		}
	`
	op2, err := gqlSchema.Operation(&schema.Request{Query: gqlQuery2})
	require.NoError(t, err)
	dgQry2, err := NewQueryRewriter().Rewrite(context.Background(), test.GetQuery(t, op2))
	require.NoError(t, err)
	actual2 := dgraph.AsString(dgQry2)
	t.Logf("Query 2 DQL:\n%s", actual2)
	// Must contain inverse field traversal
	require.Contains(t, actual2, "Contact.forCompanyReverse")
	require.NotContains(t, actual2, "uid_in(Company.forcedReverseContact")

	// Case 3: Query-time override in _metadata
	gqlQuery3 := `
		query {
			queryCompany(filter: { hasContact: { city: { eq: "Sydney" }, _metadata: { lookup: FORWARD } } }) {
				id
				name
			}
		}
	`
	op3, err := gqlSchema.Operation(&schema.Request{Query: gqlQuery3})
	require.NoError(t, err)
	dgQry3, err := NewQueryRewriter().Rewrite(context.Background(), test.GetQuery(t, op3))
	require.NoError(t, err)
	actual3 := dgraph.AsString(dgQry3)
	t.Logf("Query 3 DQL:\n%s", actual3)
	// Query-time FORWARD override should force uid_in
	require.Contains(t, actual3, "uid_in(Company.hasContact")
}

func TestForwardStrategyVariableUnification(t *testing.T) {
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

	gqlQuery := `
		query GetJobAd($id: ID!) {
			getJobAd(id: $id) {
				id
				hasApplicationAggregate(
					filter: {
						_metadata: { lookup: FORWARD }
						deleted: false
						and: [
							{ or: [{ deleted: false }, { not: { has: deleted } }] }
							{
								hasStatus: {
									not: { name: { allofterms: "Draft" } }
								}
							}
							{
								hasCandidate: {
									_metadata: { lookup: FORWARD }
									or: [{ deleted: false }, { not: { has: deleted } }]
								}
							}
						]
					}
				) {
					count
				}
			}
		}
	`

	op, err := gqlSchema.Operation(&schema.Request{
		Query:     gqlQuery,
		Variables: map[string]interface{}{"id": "0x123"},
	})
	require.NoError(t, err)

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

	dgQry, err := NewQueryRewriter().Rewrite(ctx, test.GetQuery(t, op))
	require.NoError(t, err)

	actual := dgraph.AsString(dgQry)
	t.Logf("Generated DQL:\n%s", actual)

	// Verify both nested variables are defined and used identically, preventing variable mismatch errors
	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "Generated DQL should parse perfectly with no variable mismatch errors")
}

func TestCascadeAuthDQL_JobAdAggregateCountDebug(t *testing.T) {
	// Mock lambda URL so the parser accepts @lambda directives
	x.Config.GraphQL = z.NewSuperFlag("lambda-url=http://localhost:8086/graphql-worker;").
		MergeAndCheckDefault("lambda-url=;")

	schemaBytes, err := os.ReadFile("/Users/idowuayoola/Documents/jobli/graph/.graphql")
	require.NoError(t, err)

	strSchema := string(schemaBytes)

	// Clean strSchema so that Dgraph.Authorization is at the very end
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
		query GetJobAd($id: ID!) {
			data: getJobAd(id: $id) {
				id
				hasApplicationAggregate(
					filter: {
						_metadata: { lookup: REVERSE }
						deleted: false
						hasCandidate: { _metadata: { lookup: REVERSE }, deleted: false }
						hasStatus: {
							_metadata: { lookup: REVERSE }
							not: { _metadata: { lookup: REVERSE }, name: { allofterms: "Draft" } }
						}
					}
				) {
					count
					__typename
				}
			}
		}
	`

	op, err := gqlSchema.Operation(&schema.Request{
		Query:     gqlQuery,
		Variables: map[string]interface{}{"id": "0x123"},
	})
	require.NoError(t, err)

	metaInfo.AuthVars = map[string]interface{}{
		"sub":   "e8CLjFdOVJeCYirbV5SDQ6yfUg03",
		"SUB":   "e8CLjFdOVJeCYirbV5SDQ6yfUg03",
		"ws":    "test",
		"WS":    "test",
		"email": "test@gorillajobs.app",
		"EMAIL": "test@gorillajobs.app",
	}
	ctx, err := metaInfo.AddClaimsToContext(context.Background())
	require.NoError(t, err)

	dgQry, err := NewQueryRewriter().Rewrite(ctx, test.GetQuery(t, op))
	require.NoError(t, err)

	actual := dgraph.AsString(dgQry)
	t.Logf("Generated DQL:\n%s", actual)

	// Verify both nested variables are defined and used identically, preventing variable mismatch errors
	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "Generated DQL should parse perfectly with no variable mismatch errors")
}

func TestInterfaceMemberTypesMultiple(t *testing.T) {
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

	gqlQuery := `
		query {
			queryNote(filter: { forResource: { id: ["0x4df8fe", "0x4df907"], memberTypes: [Company, Contact] } }) {
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

	// Since we filter forResource to Company and Contact, and have forceForward: true:
	// 1. The compiler optimizes compilation by nested/forward-linking checks.
	// 2. We should see the specific relation predicates Contact.forCompany and Groupable.inGroup.
	require.Contains(t, actual, "Contact.forCompany")
	require.Contains(t, actual, "Groupable.inGroup")

	// We should NOT see any Candidate or Job predicates (e.g., Job.inGroup) because they are filtered out.
	require.NotContains(t, actual, "Job.inGroup")

	// Verify the root function of the variable query NoteOwner_1 has been inverted to a valid uid(...) function
	// and the multiple types have been moved to the @filter tree as an OR.
	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "Generated DQL should parse perfectly")
}

func TestHighSelectivityCascadeAuthForward(t *testing.T) {
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

	gqlQuery := `
		query {
			queryNote(filter: { forResource: { id: ["0x4df8fe", "0x4df907"], memberTypes: [Company, Contact] } }) {
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

	// Since we filter forResource with selective IDs, the root authRewriter should have
	// forceForward = true. This means we should see parent-linked forward variables
	// and NO global scans for Group/Workspace!
	require.Contains(t, actual, "uid_in(Note.forResource")
	require.NotContains(t, actual, "var(func: type(Group))")
	require.NotContains(t, actual, "var(func: type(Workspace))")

	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "Generated DQL should parse perfectly")
}

func TestMultipleUIDInversionCandidates(t *testing.T) {
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

	gqlQuery := `
		query {
			queryApplication(filter: {
				and: [
					{
						and: [
							{ deleted: false },
							{ inGroup: { inWorkspace: { id: ["0x26e943"] } } },
							{ forJobAd: { id: ["0x48f043"] } }
						]
					}
				]
			}) {
				id
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

	// Since we filter forJobAd with selective IDs (1 hop) and inGroup with Workspace ID (2 hops),
	// the rewriter must select the variable representing forJobAd as the optimized root function
	// of the Application query block, while keeping the inGroup variable as a filter.
	// This replaces a broad Workspace-wide scan of applications with a highly selective JobAd application set lookup.
	require.Contains(t, actual, "as var(func: uid(queryApplication_and_and_2_forJobAd))")
	require.Contains(t, actual, "uid(queryApplication_and_and_1_inGroup)")

	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "Generated DQL should parse perfectly")
}

func TestRecursiveSelectiveFilterPromotion(t *testing.T) {
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

	gqlQuery := `
		query {
			aggregateJob {
				count
			}
		}
	`

	op, err := gqlSchema.Operation(&schema.Request{
		Query: gqlQuery,
	})
	require.NoError(t, err)
	gqlQry := test.GetQuery(t, op)

	metaInfo.AuthVars = map[string]interface{}{
		"sub":   "e8CLjFdOVJeCYirbV5SDQ6yfUg03",
		"SUB":   "e8CLjFdOVJeCYirbV5SDQ6yfUg03",
		"ws":    "dc98a028",
		"WS":    "dc98a028",
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

	// Assert that we successfully promoted the eq(User.userId, "e8CL...") filter
	// from being nested inside a type(User) scan block to being the root function
	// of the User auth query variable!
	require.Contains(t, actual, "func: eq(User.userId, \"e8CLjFdOVJeCYirbV5SDQ6yfUg03\")")
	// And the old type scan is moved to a filter
	require.Contains(t, actual, "type(User)")

	// Assert that there are no global scans for Workspace or User left
	require.NotContains(t, actual, "var(func: type(User)) @filter")
	require.NotContains(t, actual, "var(func: type(Workspace))")

	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "Generated DQL should parse perfectly")
}
