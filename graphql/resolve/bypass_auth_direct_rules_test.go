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

func TestBypassAuth_UpdateMutationEdgeSeverance(t *testing.T) {
	sch := `
		type Workspace {
			id: ID!
			name: String! @search(by: [exact])
		}

		type Manifest @auth(
			update: {
				rule: "query($ws: String!) { queryManifest { inWorkspace(filter: { name: { eq: $ws } }) { id } } }"
			}
		) {
			id: ID!
			version: String
			inWorkspace: Workspace!
			plugin: Plugin @hasInverse(field: installedManifest)
			otherPlugin: Plugin @hasInverse(field: normalManifest)
			conditionalPlugin: Plugin @hasInverse(field: conditionalManifest)
		}

		type Plugin @auth(
			update: {
				rule: "query($ws: String!) { queryPlugin { inWorkspace(filter: { name: { eq: $ws } }) { id } } }"
			}
		) {
			id: ID!
			inWorkspace: Workspace!
			installedManifest: Manifest @bypassAuth
			normalManifest: Manifest
			conditionalManifest: Manifest @bypassAuth(if: "auth.role == 'ADMIN'")
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

	metaInfo.AuthVars = map[string]interface{}{
		"ws":   "my-tenant-ws",
		"role": "ADMIN",
	}
	ctx, err := metaInfo.AddClaimsToContext(context.Background())
	require.NoError(t, err)

	// Case 1: Updating installedManifest (has @bypassAuth)
	// Severing the edge should NOT generate update auth queries on Manifest!
	{
		gqlMutation := `
			mutation {
				updatePlugin(input: {
					filter: { id: ["0x123"] },
					set: { installedManifest: { id: "0x456" } }
				}) {
					plugin {
						id
					}
				}
			}
		`
		op, err := gqlSchema.Operation(&schema.Request{
			Query: gqlMutation,
		})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		rewriter := NewUpdateRewriter()
		queries, _, err := rewriter.RewriteQueries(ctx, mut)
		require.NoError(t, err)

		idExistence := make(map[string]string)
		for _, q := range queries {
			idExistence[q.Attr] = "0x456"
		}

		upsert, err := rewriter.Rewrite(ctx, mut, idExistence)
		require.NoError(t, err)
		require.Len(t, upsert, 1)

		actualDQL := dgraph.AsString(upsert[0].Query)
		t.Logf("Generated DQL for @bypassAuth edge update:\n%s", actualDQL)
		if len(upsert[0].Mutations) > 0 {
			t.Logf("DeleteJson:\n%s", string(upsert[0].Mutations[0].DeleteJson))
			t.Logf("SetJson:\n%s", string(upsert[0].Mutations[0].SetJson))
		}

		// The variables defined in upsert.Query (Manifest_5, Plugin_4) are consumed
		// by the mutation block (DeleteJson). When parsing the query standalone, pass
		// the mutation variables into ParseWithNeedVars just as Dgraph worker does.
		_, parseErr := dql.ParseWithNeedVars(dql.Request{Str: actualDQL}, []string{"Manifest_5", "Plugin_4"})
		require.NoError(t, parseErr, "Generated DQL must parse cleanly with mutation needVars")

		// 1. The old edge severance query must exist
		require.Contains(t, actualDQL, "Plugin.installedManifest @filter(NOT (uid(0x456)))")
		// 2. The old node's update auth check (Manifest_...auth) must be completely omitted
		require.NotContains(t, actualDQL, "Manifest_5.auth")
		require.NotContains(t, actualDQL, ".auth(func: uid(")
	}

	// Case 2: Updating normalManifest (WITHOUT @bypassAuth)
	// Severing the edge MUST generate update auth queries checking Manifest permissions!
	{
		gqlMutation := `
			mutation {
				updatePlugin(input: {
					filter: { id: ["0x123"] },
					set: { normalManifest: { id: "0x789" } }
				}) {
					plugin {
						id
					}
				}
			}
		`
		op, err := gqlSchema.Operation(&schema.Request{
			Query: gqlMutation,
		})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		rewriter := NewUpdateRewriter()
		queries, _, err := rewriter.RewriteQueries(ctx, mut)
		require.NoError(t, err)

		idExistence := make(map[string]string)
		for _, q := range queries {
			idExistence[q.Attr] = "0x789"
		}

		upsert, err := rewriter.Rewrite(ctx, mut, idExistence)
		require.NoError(t, err)
		require.Len(t, upsert, 1)

		actualDQL := dgraph.AsString(upsert[0].Query)
		t.Logf("Generated DQL for normal edge update:\n%s", actualDQL)

		_, parseErr := dql.Parse(dql.Request{Str: actualDQL})
		require.NoError(t, parseErr, "Generated DQL must parse cleanly")

		// 1. The old edge severance query must exist
		require.Contains(t, actualDQL, "Plugin.normalManifest @filter(NOT (uid(0x789)))")
		// 2. The old node's update auth check MUST be present
		require.Contains(t, actualDQL, ".auth(func: uid(")
		require.Contains(t, actualDQL, "Manifest.inWorkspace")
	}

	// Case 3: Updating conditionalManifest when role is ADMIN (condition passes -> auth bypassed)
	{
		gqlMutation := `
			mutation {
				updatePlugin(input: {
					filter: { id: ["0x123"] },
					set: { conditionalManifest: { id: "0xaaa" } }
				}) {
					plugin {
						id
					}
				}
			}
		`
		op, err := gqlSchema.Operation(&schema.Request{
			Query: gqlMutation,
		})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		rewriter := NewUpdateRewriter()
		queries, _, err := rewriter.RewriteQueries(ctx, mut)
		require.NoError(t, err)

		idExistence := make(map[string]string)
		for _, q := range queries {
			idExistence[q.Attr] = "0xaaa"
		}

		upsert, err := rewriter.Rewrite(ctx, mut, idExistence)
		require.NoError(t, err)
		require.Len(t, upsert, 1)

		actualDQL := dgraph.AsString(upsert[0].Query)
		t.Logf("Generated DQL for conditional edge update (ADMIN):\n%s", actualDQL)

		// ADMIN satisfies if condition -> bypasses Manifest update auth
		require.NotContains(t, actualDQL, ".auth(func: uid(")
	}

	// Case 4: Updating conditionalManifest when role is USER (condition fails -> auth enforced)
	{
		userMeta := &testutil.AuthMeta{
			PublicKey:       authParsed.VerificationKey,
			Namespace:       authParsed.Namespace,
			Algo:            authParsed.Algo,
			ClosedByDefault: authParsed.ClosedByDefault,
			AuthVars: map[string]interface{}{
				"ws":   "my-tenant-ws",
				"role": "USER",
			},
		}
		userCtx, err := userMeta.AddClaimsToContext(context.Background())
		require.NoError(t, err)

		gqlMutation := `
			mutation {
				updatePlugin(input: {
					filter: { id: ["0x123"] },
					set: { conditionalManifest: { id: "0xbbb" } }
				}) {
					plugin {
						id
					}
				}
			}
		`
		op, err := gqlSchema.Operation(&schema.Request{
			Query: gqlMutation,
		})
		require.NoError(t, err)
		mut := test.GetMutation(t, op)

		rewriter := NewUpdateRewriter()
		queries, _, err := rewriter.RewriteQueries(userCtx, mut)
		require.NoError(t, err)

		idExistence := make(map[string]string)
		for _, q := range queries {
			idExistence[q.Attr] = "0xbbb"
		}

		upsert, err := rewriter.Rewrite(userCtx, mut, idExistence)
		require.NoError(t, err)
		require.Len(t, upsert, 1)

		actualDQL := dgraph.AsString(upsert[0].Query)
		t.Logf("Generated DQL for conditional edge update (USER):\n%s", actualDQL)

		// USER fails if condition -> enforces Manifest update auth
		require.Contains(t, actualDQL, ".auth(func: uid(")
		require.Contains(t, actualDQL, "Manifest.inWorkspace")
	}
}
