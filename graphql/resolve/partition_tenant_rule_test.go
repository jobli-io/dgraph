/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/graphql/test"
)

const tenantRuleSchema = `
type Member {
	id: ID!
	email: String! @search(by: [hash])
	mayPublish: Boolean @search
}
type Workspace @tenant(claim: "ws", name: "name") {
	name: String! @id
	members: [Member]
}
type App @scope(plane: control) @auth(update: { and: [
	{ rule: "query($ws: String!) { queryApp(filter: {ownedBy: {eq: $ws}}) { slug } }" },
	{ tenantRule: "query($sub: String!) { queryWorkspace { members(filter: {email: {eq: $sub}, mayPublish: true}) { id } } }" } ] }) {
	slug: String! @id
	ownedBy: String! @search(by: [hash])
}
type Plugin {
	id: ID!
	hasApp: App
}
`

func TestATenantRuleBecomesAQuestionForTheTenant(t *testing.T) {
	sch := test.LoadSchemaFromString(t, tenantRuleSchema)
	names := sch.TenantRulesOf("App")
	require.Len(t, names, 1)
	tenant, rule := sch.TenantRule(names[0])

	ask := func(claims map[string]interface{}) (string, bool) {
		g := &callerGrants{claims: claims}
		return tenantRuleQuestion(g, tenant, rule)
	}

	q, ok := ask(map[string]interface{}{"sub": "ann@acme"})
	require.True(t, ok)
	// one tenant record that passes is enough, and it is the caller's own values that are asked about
	require.Contains(t, q, `held(func: type(Workspace), first: 1) @filter(uid(`)
	require.Contains(t, q, `var(func: type(Workspace)) @cascade`)
	require.Contains(t, q, `eq(Member.email, "ann@acme")`)
	require.Contains(t, q, `eq(Member.mayPublish, true)`)
	require.NotContains(t, q, "App", "nothing of the record's type is in the question")

	other, ok := ask(map[string]interface{}{"sub": "bob@acme"})
	require.True(t, ok)
	require.NotEqual(t, q, other, "the text is one caller's, so an answer is not shared between callers")

	_, ok = ask(map[string]interface{}{"ws": "acme"})
	require.False(t, ok, "a caller without the claim the rule needs is not asked about, and does not pass")
}

func TestTenantRulesAreFoundUnderWhatARequestNames(t *testing.T) {
	sch := test.LoadSchemaFromString(t, tenantRuleSchema)
	names := func(query string) map[string]bool {
		op, err := sch.Operation(&schema.Request{Query: query})
		require.NoError(t, err)
		found := make(map[string]bool)
		for _, q := range op.Queries() {
			tenantRulesUnder(sch, q, found)
		}
		return found
	}
	require.Len(t, names(`{ queryApp { slug } }`), 1)
	require.Len(t, names(`{ queryPlugin { id hasApp { slug } } }`), 1, "reached through an edge")
	require.Empty(t, names(`{ queryPlugin { id } }`))

	// Outside a partition there is no tenant to ask: the request goes on with no answer.
	op, err := sch.Operation(&schema.Request{Query: `{ queryApp { slug } }`})
	require.NoError(t, err)
	ctx := context.Background()
	require.Equal(t, ctx, withTenantRules(ctx, sch, op.Queries()[0], nil))
}
