package resolve

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/dql"
	"github.com/hypermodeinc/dgraph/v25/graphql/authorization"
	"github.com/hypermodeinc/dgraph/v25/graphql/dgraph"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/graphql/test"
	"github.com/hypermodeinc/dgraph/v25/testutil"
)

func TestBypassAuth_DirectTypeRulesExcept(t *testing.T) {
	sch := `
		interface Member @auth(
			query: {
				rule: "query($role: String!) { queryMember(filter: { role: { eq: $role } }) { id } }"
			}
		) {
			id: ID!
			role: String @search(by: [exact])
			inWorkspace: Workspace!
		}

		type Workspace @auth(
			query: {
				rule: "query($ws: String!) { queryWorkspace(filter: { name: { eq: $ws } }) { id } }"
			}
		) {
			id: ID!
			name: String! @search(by: [exact])
			hasUser: [User!] @bypassAuth(except: ["User", "Member"])
		}

		type User implements Member @auth(
			interfacePolicy: [
				{ interface: "Member", merge: "or" }
			]
			query: {
				or: [
					{ rule: "query($sub: String!) { queryUser(filter: { userId: { eq: $sub } }) { id } }" }
					{ rule: "query($email: String!) { queryUser(filter: { email: { eq: $email } }) { id } }" }
				]
			}
		) {
			id: ID!
			role: String @search(by: [exact])
			userId: String @search(by: [exact])
			email: String @search(by: [exact])
			inWorkspace: Workspace!
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
			queryWorkspace {
				id
				name
				hasUser {
					id
					userId
					email
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
		"ws":    "test-ws",
		"WS":    "test-ws",
		"sub":   "user-123",
		"SUB":   "user-123",
		"email": "user@example.com",
		"EMAIL": "user@example.com",
		"role":  "ADMIN",
		"ROLE":  "ADMIN",
	}
	ctx, err := metaInfo.AddClaimsToContext(context.Background())
	require.NoError(t, err)

	rewriter := NewQueryRewriter()
	dgQuery, err := rewriter.Rewrite(ctx, gqlQry)
	require.NoError(t, err)

	actual := dgraph.AsString(dgQuery)
	t.Logf("Generated DQL:\n%s", actual)

	// DQL must parse cleanly
	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "DQL should parse without errors")

	// Direct User query rules must be preserved on hasUser
	require.True(t, strings.Contains(actual, "user-123"), "Direct User userId rule should be retained")
	require.True(t, strings.Contains(actual, "user@example.com"), "Direct User email rule should be retained")
	// Interface Member rule must also be preserved
	require.True(t, strings.Contains(actual, "ADMIN"), "Interface Member rule should be retained")
}

func TestBypassAuth_DirectTypeOnlyExcept(t *testing.T) {
	sch := `
		interface Member @auth(
			query: {
				rule: "query($role: String!) { queryMember(filter: { role: { eq: $role } }) { id } }"
			}
		) {
			id: ID!
			role: String @search(by: [exact])
			inWorkspace: Workspace!
		}

		type Workspace @auth(
			query: {
				rule: "query($ws: String!) { queryWorkspace(filter: { name: { eq: $ws } }) { id } }"
			}
		) {
			id: ID!
			name: String! @search(by: [exact])
			hasUser: [User!] @bypassAuth(except: ["User"])
		}

		type User implements Member @auth(
			interfacePolicy: [
				{ interface: "Member", merge: "or" }
			]
			query: {
				or: [
					{ rule: "query($sub: String!) { queryUser(filter: { userId: { eq: $sub } }) { id } }" }
					{ rule: "query($email: String!) { queryUser(filter: { email: { eq: $email } }) { id } }" }
				]
			}
		) {
			id: ID!
			role: String @search(by: [exact])
			userId: String @search(by: [exact])
			email: String @search(by: [exact])
			inWorkspace: Workspace!
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
			queryWorkspace {
				id
				name
				hasUser {
					id
					userId
					email
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
		"ws":    "test-ws",
		"WS":    "test-ws",
		"sub":   "user-123",
		"SUB":   "user-123",
		"email": "user@example.com",
		"EMAIL": "user@example.com",
		"role":  "ADMIN",
		"ROLE":  "ADMIN",
	}
	ctx, err := metaInfo.AddClaimsToContext(context.Background())
	require.NoError(t, err)

	rewriter := NewQueryRewriter()
	dgQuery, err := rewriter.Rewrite(ctx, gqlQry)
	require.NoError(t, err)

	actual := dgraph.AsString(dgQuery)
	t.Logf("Generated DQL:\n%s", actual)

	// DQL must parse cleanly
	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "DQL should parse without errors")

	// Direct User query rules must be preserved on hasUser
	require.True(t, strings.Contains(actual, "user-123"), "Direct User userId rule should be retained")
	require.True(t, strings.Contains(actual, "user@example.com"), "Direct User email rule should be retained")
	// The Member interface rule must NOT be present on the child filter
	require.False(t, strings.Contains(actual, "ADMIN"), "Unexcepted interface rule should be stripped on child")
}

func TestBypassAuth_IfCondition(t *testing.T) {
	sch := `
		type Workspace @auth(
			query: {
				rule: "query($ws: String!) { queryWorkspace(filter: { name: { eq: $ws } }) { id } }"
			}
		) {
			id: ID!
			name: String! @search(by: [exact])
			hasUser: [User!] @bypassAuth(if: "auth.role == 'ADMIN'")
		}

		type User @auth(
			query: {
				rule: "query($sub: String!) { queryUser(filter: { userId: { eq: $sub } }) { id } }"
			}
		) {
			id: ID!
			userId: String @search(by: [exact])
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
			queryWorkspace {
				id
				name
				hasUser {
					id
					userId
				}
			}
		}
	`

	op, err := gqlSchema.Operation(&schema.Request{
		Query: gqlQuery,
	})
	require.NoError(t, err)
	gqlQry := test.GetQuery(t, op)

	// Case 1: If condition passes (role == 'ADMIN') -> auth is bypassed on hasUser
	{
		metaInfo.AuthVars = map[string]interface{}{
			"ws":   "test-ws",
			"sub":  "user-123",
			"role": "ADMIN",
		}
		ctx, err := metaInfo.AddClaimsToContext(context.Background())
		require.NoError(t, err)

		rewriter := NewQueryRewriter()
		dgQuery, err := rewriter.Rewrite(ctx, gqlQry)
		require.NoError(t, err)

		actual := dgraph.AsString(dgQuery)
		t.Logf("Generated DQL (ADMIN):\n%s", actual)

		_, parseErr := dql.Parse(dql.Request{Str: actual})
		require.NoError(t, parseErr, "DQL should parse without errors")

		require.True(t, strings.Contains(actual, "test-ws"), "Workspace rule should be retained")
		require.False(t, strings.Contains(actual, "user-123"), "hasUser auth rule should be bypassed for ADMIN")
	}

	// Case 2: If condition fails (role == 'MEMBER') -> auth is NOT bypassed, child auth rule is enforced
	{
		metaInfo.AuthVars = map[string]interface{}{
			"ws":   "test-ws",
			"sub":  "user-123",
			"role": "MEMBER",
		}
		ctx, err := metaInfo.AddClaimsToContext(context.Background())
		require.NoError(t, err)

		rewriter := NewQueryRewriter()
		dgQuery, err := rewriter.Rewrite(ctx, gqlQry)
		require.NoError(t, err)

		actual := dgraph.AsString(dgQuery)
		t.Logf("Generated DQL (MEMBER):\n%s", actual)

		_, parseErr := dql.Parse(dql.Request{Str: actual})
		require.NoError(t, parseErr, "DQL should parse without errors")

		require.True(t, strings.Contains(actual, "test-ws"), "Workspace rule should be retained")
		require.True(t, strings.Contains(actual, "user-123"), "hasUser auth rule should NOT be bypassed for MEMBER")
	}

	// Case 3: No auth variables (unauthenticated / missing claim) -> auth is NOT bypassed
	{
		metaInfo.AuthVars = map[string]interface{}{
			"ws":  "test-ws",
			"sub": "user-123",
		}
		ctx, err := metaInfo.AddClaimsToContext(context.Background())
		require.NoError(t, err)

		rewriter := NewQueryRewriter()
		dgQuery, err := rewriter.Rewrite(ctx, gqlQry)
		require.NoError(t, err)

		actual := dgraph.AsString(dgQuery)
		t.Logf("Generated DQL (Missing role claim):\n%s", actual)

		_, parseErr := dql.Parse(dql.Request{Str: actual})
		require.NoError(t, parseErr, "DQL should parse without errors")

		require.True(t, strings.Contains(actual, "user-123"), "hasUser auth rule should NOT be bypassed when role claim is missing")
	}
}

func TestBypassAuth_IfConditionWithExcept(t *testing.T) {
	sch := `
		interface Member @auth(
			query: {
				rule: "query($role: String!) { queryMember(filter: { role: { eq: $role } }) { id } }"
			}
		) {
			id: ID!
			role: String @search(by: [exact])
			inWorkspace: Workspace!
		}

		type Workspace @auth(
			query: {
				rule: "query($ws: String!) { queryWorkspace(filter: { name: { eq: $ws } }) { id } }"
			}
		) {
			id: ID!
			name: String! @search(by: [exact])
			hasUser: [User!] @bypassAuth(if: "auth.role == 'ADMIN'", except: ["User"])
		}

		type User implements Member @auth(
			interfacePolicy: [
				{ interface: "Member", merge: "or" }
			]
			query: {
				or: [
					{ rule: "query($sub: String!) { queryUser(filter: { userId: { eq: $sub } }) { id } }" }
					{ rule: "query($email: String!) { queryUser(filter: { email: { eq: $email } }) { id } }" }
				]
			}
		) {
			id: ID!
			role: String @search(by: [exact])
			userId: String @search(by: [exact])
			email: String @search(by: [exact])
			inWorkspace: Workspace!
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
			queryWorkspace {
				id
				name
				hasUser {
					id
					userId
					email
				}
			}
		}
	`

	op, err := gqlSchema.Operation(&schema.Request{
		Query: gqlQuery,
	})
	require.NoError(t, err)
	gqlQry := test.GetQuery(t, op)

	// When role is ADMIN: if condition passes, so bypass applies EXCEPT for User direct rules.
	// That means User direct rules (user-123, email) ARE enforced, but Member interface rule (ADMIN) is bypassed.
	{
		metaInfo.AuthVars = map[string]interface{}{
			"ws":    "test-ws",
			"sub":   "user-123",
			"email": "user@example.com",
			"role":  "ADMIN",
		}
		ctx, err := metaInfo.AddClaimsToContext(context.Background())
		require.NoError(t, err)

		rewriter := NewQueryRewriter()
		dgQuery, err := rewriter.Rewrite(ctx, gqlQry)
		require.NoError(t, err)

		actual := dgraph.AsString(dgQuery)
		t.Logf("Generated DQL (ADMIN with except User):\n%s", actual)

		_, parseErr := dql.Parse(dql.Request{Str: actual})
		require.NoError(t, parseErr, "DQL should parse without errors")

		require.True(t, strings.Contains(actual, "user-123"), "Direct User userId rule should be retained due to except")
		require.False(t, strings.Contains(actual, "ADMIN"), "Interface Member rule should be bypassed")
	}

	// When role is MEMBER: if condition fails, so bypass does NOT apply at all!
	// Both Member interface rule AND User direct rules are enforced.
	{
		metaInfo.AuthVars = map[string]interface{}{
			"ws":    "test-ws",
			"sub":   "user-123",
			"email": "user@example.com",
			"role":  "MEMBER",
		}
		ctx, err := metaInfo.AddClaimsToContext(context.Background())
		require.NoError(t, err)

		rewriter := NewQueryRewriter()
		dgQuery, err := rewriter.Rewrite(ctx, gqlQry)
		require.NoError(t, err)

		actual := dgraph.AsString(dgQuery)
		t.Logf("Generated DQL (MEMBER with except User):\n%s", actual)

		_, parseErr := dql.Parse(dql.Request{Str: actual})
		require.NoError(t, parseErr, "DQL should parse without errors")

		require.True(t, strings.Contains(actual, "user-123"), "Direct User userId rule should be retained")
		require.True(t, strings.Contains(actual, "MEMBER"), "Interface Member rule should be enforced because bypass condition failed")
	}
}
