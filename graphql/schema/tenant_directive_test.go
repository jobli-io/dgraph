/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTenantAndGrantsMarkers(t *testing.T) {
	load := func(sch string) error {
		_, err := NewHandler(sch, false)
		return err
	}

	t.Run("one tenant, with grants held on it and on other types", func(t *testing.T) {
		require.NoError(t, load(`
			interface Resource @grants { id: ID! }
			type Workspace implements Resource @tenant { name: String! @id }
			type Group implements Resource { title: String }
			type Board @grants { id: ID! title: String }`))
	})

	t.Run("a schema that marks nothing is as it was", func(t *testing.T) {
		require.NoError(t, load(`type Workspace { name: String! @id }`))
	})

	t.Run("two tenants are refused", func(t *testing.T) {
		err := load(`
			type Workspace @tenant { name: String! @id }
			type Organisation @tenant { name: String! @id }`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "a schema can have only one tenant type")
	})

	t.Run("grants with no tenant to hold them in are refused", func(t *testing.T) {
		err := load(`
			interface Resource @grants { id: ID! }
			type Group implements Resource { title: String }`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "@grants needs a type with @tenant")
	})

	t.Run("a tenant is a type, not an interface", func(t *testing.T) {
		err := load(`
			interface Owner @tenant { id: ID! }
			type Workspace implements Owner { name: String! @id }`)
		require.Error(t, err)
	})

	t.Run("a tenant is stored", func(t *testing.T) {
		err := load(`
			type Workspace @tenant @remote { name: String! }
			type Other { id: ID! }`)
		require.Error(t, err)
	})
}

func TestAccessDeclaresARuleAboutTheTenant(t *testing.T) {
	load := func(sch string) error {
		_, err := NewHandler(sch, false)
		return err
	}
	const base = `
		type Workspace @tenant {
			name: String! @id
			members: [Member] @hasInverse(field: inWorkspace)
		}
		type Member { id: ID! userId: String! @search(by: [hash]) inWorkspace: Workspace! }
		interface InWorkspace { id: ID! inWorkspace: Workspace! }
	`

	t.Run("a rule that only walks to the tenant, on the type or through another interface", func(t *testing.T) {
		require.NoError(t, load(base+`
			interface Shared @access(scope: SHARED) @auth(query: { and: [
				{ rule: """query ($sub: String!) { queryShared { ...on InWorkspace {
					inWorkspace { members(filter: {userId: {eq: $sub}}) { __typename } } } } }""" }
				{ rule: "{ $scope: { in: [\"read\"] } }" }
			] }) { id: ID! }
			type Candidate implements Shared & InWorkspace { name: String }
			type Note @access(scope: TENANT) @auth(query: { rule: """query ($ws: String!) {
				queryNote { inWorkspace(filter: {name: {eq: $ws}}) { __typename } } }""" }) {
				id: ID!
				text: String
				inWorkspace: Workspace!
			}`))
	})

	for name, c := range map[string]struct{ rule, want string }{
		"a rule that filters the records themselves": {
			`query { queryNote(filter: {text: {eq: "x"}}) { inWorkspace { __typename } } }`, "has arguments"},
		"a rule that selects something else of the record": {
			`query { queryNote { inWorkspace { __typename } author { __typename } } }`, "not a non-null edge to Workspace"},
		"a rule that never reaches the tenant": {
			`query ($sub: String!) { queryNote { __typename } }`, "selects it 0 times"},
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			err := load(base + `
				type Note @access(scope: SHARED) @auth(query: { rule: """` + c.rule + `""" }) {
					id: ID!
					text: String @search(by: [hash])
					inWorkspace: Workspace!
					author: Member
				}`)
			require.Error(t, err)
			require.Contains(t, err.Error(), c.want)
		})
	}

	t.Run("an edge to the tenant that can be missing is refused", func(t *testing.T) {
		err := load(base + `
			type Note @access(scope: SHARED) @auth(query: { rule: """query {
				queryNote { maybe { __typename } } }""" }) { id: ID! maybe: Workspace }`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not a non-null edge to Workspace")
	})

	t.Run("with no rule to declare anything about, it is refused", func(t *testing.T) {
		err := load(base + `type Note @access(scope: SHARED) { id: ID! inWorkspace: Workspace! }`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "needs a query rule")
	})

	t.Run("with no tenant in the schema, it is refused", func(t *testing.T) {
		err := load(`type Note @access(scope: SHARED) { id: ID! text: String }`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "needs a type with @tenant")
	})

	t.Run("a scope that is not one of the two is refused", func(t *testing.T) {
		require.Error(t, load(base+`type Note @access(scope: PUBLIC) { id: ID! inWorkspace: Workspace! }`))
	})
}

func TestScopePlacesTypesInAPlane(t *testing.T) {
	load := func(sch string) error {
		_, err := NewHandler(sch, false)
		return err
	}
	const base = `
		type Workspace @tenant { name: String! @id }
		interface InWorkspace @scope(plane: tenant) { id: ID! inWorkspace: Workspace! }
		type App @scope(plane: control) { clientId: String! @id title: String }
	`

	t.Run("a tenant type may point at a control type, one way", func(t *testing.T) {
		require.NoError(t, load(base+`
			type Plugin implements InWorkspace { label: String hasApp: App }
			type Candidate { id: ID! name: String madeBy: App }`))
	})

	t.Run("a schema that places nothing is as it was", func(t *testing.T) {
		require.NoError(t, load(`
			type App { clientId: String! @id plugins: [Plugin] @hasInverse(field: hasApp) }
			type Plugin { id: ID! hasApp: App }`))
	})

	for name, c := range map[string]struct{ sch, want string }{
		"an edge into the control plane with an inverse": {`
			type App2 @scope(plane: control) { clientId: String! @id plugins: [Plugin] }
			type Plugin implements InWorkspace { label: String hasApp: App2 @hasInverse(field: plugins) }`,
			"cannot hold an edge to Plugin"},
		"an inverse declared from the tenant side": {`
			type Plugin implements InWorkspace { label: String hasApp: App @hasInverse(field: title) }`,
			"read-only and cannot have @hasInverse"},
		"a control type that points at a tenant type": {`
			type Catalogue @scope(plane: control) { id: ID! featured: [Candidate] }
			type Candidate { id: ID! name: String }`,
			"a control type cannot hold an edge to Candidate"},
		"a type in two planes": {`
			interface Listed @scope(plane: control) { id: ID! }
			type Plugin implements InWorkspace & Listed { label: String }`,
			"puts it in two planes"},
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			err := load(base + c.sch)
			require.Error(t, err)
			require.Contains(t, err.Error(), c.want)
		})
	}
}

func TestTenantSaysWhichClaimNamesIt(t *testing.T) {
	sch := func(s string) (Schema, error) {
		h, err := NewHandler(s, false)
		if err != nil {
			return nil, err
		}
		return FromString(h.GQLSchema(), 0)
	}
	s, err := sch(`type Workspace @tenant(claim: "ws", name: "name") { name: String! @id title: String }`)
	require.NoError(t, err)
	require.Equal(t, "ws", s.TenantClaim())
	require.Equal(t, "name", s.TenantNameField())

	s, err = sch(`type Workspace @tenant { name: String! @id }`)
	require.NoError(t, err)
	require.Equal(t, "", s.TenantClaim(), "a bare marker says nothing about the claim")

	for name, bad := range map[string]string{
		"a claim with no name field":       `type Workspace @tenant(claim: "ws") { name: String! @id }`,
		"a name field that does not exist": `type Workspace @tenant(claim: "ws", name: "slug") { name: String! @id }`,
		"a name field that is not an @id":  `type Workspace @tenant(claim: "ws", name: "title") { name: String! @id title: String! }`,
		"a name field that can be missing": `type Workspace @tenant(claim: "ws", name: "title") { name: String! @id title: String @id }`,
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			_, err := sch(bad)
			require.Error(t, err)
			require.Contains(t, err.Error(), "@tenant")
		})
	}
}

func TestATenantRuleIsAskedOfTheTenant(t *testing.T) {
	const base = `
		type Member { id: ID! email: String! @search(by: [hash]) mayPublish: Boolean @search }
		type Workspace @tenant(claim: "ws", name: "name") { name: String! @id members: [Member] }
		# Dgraph.Authorization {"VerificationKey":"secret","Header":"X-Auth","Namespace":"https://x/claims","Algo":"HS256"}
	`
	const rule = `query($USER: String!) { queryWorkspace { members(filter: {email: {eq: $USER}, mayPublish: true}) { id } } }`
	load := func(sch string) (Schema, error) {
		h, err := NewHandler(sch, false)
		if err != nil {
			return nil, err
		}
		return FromString(h.GQLSchema(), 0)
	}

	t.Run("on a control type, beside a rule about the record", func(t *testing.T) {
		sch, err := load(base + `
			type App @scope(plane: control) @auth(update: { and: [
				{ rule: "query($ws: String!) { queryApp(filter: {ownedBy: {eq: $ws}}) { slug } }" },
				{ tenantRule: "` + rule + `" } ] }) {
				slug: String! @id
				ownedBy: String! @search(by: [hash])
			}`)
		require.NoError(t, err)
		require.True(t, sch.HasTenantRules())

		names := sch.TenantRulesOf("App")
		require.Len(t, names, 1)
		require.True(t, strings.HasPrefix(names[0], TenantRulePrefix))
		require.Empty(t, sch.TenantRulesOf("Member"))

		tenant, inner := sch.TenantRule(names[0])
		require.NotNil(t, inner)
		require.NotNil(t, inner.Rule)
		require.Equal(t, "Workspace", tenant.Name())

		// With no answer the rule does not pass; nor with one a token could not have set.
		var leaf *RBACQuery
		for typ, ta := range sch.(*schema).authRules {
			if typ == "App" {
				leaf = ta.Rules.Update.And[1].RBACRule
			}
		}
		require.NotNil(t, leaf)
		require.Equal(t, Negative, leaf.EvaluateRBACRule(map[string]interface{}{}))
		require.Equal(t, Negative, leaf.EvaluateRBACRule(map[string]interface{}{names[0]: false}))
		require.Equal(t, Negative, leaf.EvaluateRBACRule(map[string]interface{}{names[0]: "true"}))
		require.Equal(t, Positive, leaf.EvaluateRBACRule(map[string]interface{}{names[0]: true}))
	})

	t.Run("the same text has the same name wherever it is written", func(t *testing.T) {
		sch, err := load(base + `
			type App @auth(update: { tenantRule: "` + rule + `" }) { slug: String! @id }
			type Plan @auth(delete: { tenantRule: "` + rule + `" }) { code: String! @id }`)
		require.NoError(t, err)
		require.Equal(t, sch.TenantRulesOf("App"), sch.TenantRulesOf("Plan"))
	})

	t.Run("an interface answers for the types under it", func(t *testing.T) {
		sch, err := load(base + `
			interface Listed { slug: String! @id }
			type App implements Listed @auth(query: { tenantRule: "` + rule + `" }) { title: String }`)
		require.NoError(t, err)
		require.Len(t, sch.TenantRulesOf("Listed"), 1)
	})

	t.Run("it needs a tenant type", func(t *testing.T) {
		_, err := load(`
			type App @auth(update: { tenantRule: "query { queryApp { slug } }" }) { slug: String! @id }`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no type is marked @tenant")
	})

	t.Run("it is a query on the tenant type and no other", func(t *testing.T) {
		_, err := load(base + `
			type App @auth(update: { tenantRule: "query { queryApp { slug } }" }) { slug: String! @id }`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "it must be one query on the tenant type, queryWorkspace")
	})

	t.Run("it takes no placeholders", func(t *testing.T) {
		_, err := load(base + `
			type App @auth(update: { tenantRule: "query { queryWorkspace(filter: <<SCOPE>>) { name } }" }) { slug: String! @id }`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot use @authVariables placeholders")
	})

	t.Run("it stands alone in its place, like a rule", func(t *testing.T) {
		_, err := load(base + `
			type App @auth(update: { rule: "query { queryApp { slug } }", tenantRule: "` + rule + `" }) { slug: String! @id }`)
		require.Error(t, err)
	})
}
