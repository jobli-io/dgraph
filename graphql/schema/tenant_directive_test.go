/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
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
