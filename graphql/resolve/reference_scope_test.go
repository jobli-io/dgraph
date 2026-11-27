/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/dql"
	"github.com/dgraph-io/dgraph/v25/graphql/dgraph"
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/graphql/test"
)

func scopeRows(t *testing.T, raw string) []idExistenceRes {
	var rows []idExistenceRes
	require.NoError(t, json.Unmarshal([]byte(raw), &rows))
	return rows
}

type scopedRewriter map[string]*refScopeCheck

func (s scopedRewriter) referenceScopeChecks() map[string]*refScopeCheck { return s }

func TestReferenceScopeAllows(t *testing.T) {
	check := &refScopeCheck{
		typeName: "Holder",
		scopes: []schema.ReferenceScope{
			{Claim: "ws", Edges: []string{"Member.inSpace"}, Value: "Space.name"},
			{Claim: "ws", Edges: []string{"Grouped.inGroup", "Member.inSpace"}, Value: "Space.name"},
		},
		unscoped: []string{"Bot"},
	}
	acme := map[string]interface{}{"ws": "acme"}
	cases := []struct {
		name   string
		row    string
		claims map[string]interface{}
		want   bool
	}{
		{"its own edge names the caller's space",
			`[{"uid":"0x1","dgraph.type":["Person"],"s0":{"v":"acme"}}]`, acme, true},
		{"its own edge names another space",
			`[{"uid":"0x1","dgraph.type":["Person"],"s0":{"v":"globex"}}]`, acme, false},
		{"one of its groups is in the caller's space",
			`[{"uid":"0x1","dgraph.type":["Firm"],"s1":[{"v":{"v":"globex"}},{"v":{"v":"acme"}}]}]`, acme, true},
		{"no group is in the caller's space",
			`[{"uid":"0x1","dgraph.type":["Firm"],"s1":[{"v":{"v":"globex"}}]}]`, acme, false},
		{"it has no edge at all", `[{"uid":"0x1","dgraph.type":["Person"]}]`, acme, false},
		{"it is of a type with no rule", `[{"uid":"0x1","dgraph.type":["Bot","Holder"]}]`, acme, true},
		{"the caller has no claim",
			`[{"uid":"0x1","dgraph.type":["Person"],"s0":{"v":"acme"}}]`, map[string]interface{}{}, false},
		{"the caller's claim is empty and so is the value",
			`[{"uid":"0x1","dgraph.type":["Person"],"s0":{"v":""}}]`, map[string]interface{}{"ws": ""}, false},
		{"the caller's claim is not a string",
			`[{"uid":"0x1","dgraph.type":["Person"],"s0":{"v":"7"}}]`, map[string]interface{}{"ws": 7}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, check.allows(scopeRows(t, c.row)[0], c.claims))
		})
	}
}

func TestReferencesOutOfScope(t *testing.T) {
	scope := []schema.ReferenceScope{{Claim: "ws", Edges: []string{"Member.inSpace"}, Value: "Space.name"}}
	claims := func() (map[string]interface{}, error) { return map[string]interface{}{"ws": "acme"}, nil }
	theirs := `[{"uid":"0x1","dgraph.type":["Person"],"s0":{"v":"globex"}}]`
	ours := `[{"uid":"0x2","dgraph.type":["Person"],"s0":{"v":"acme"}}]`

	t.Run("a rewriter that asked nothing is not looked at", func(t *testing.T) {
		called := false
		hidden, err := referencesOutOfScope(scopedRewriter{}, nil, func() (map[string]interface{}, error) {
			called = true
			return nil, nil
		})
		require.NoError(t, err)
		require.Empty(t, hidden)
		require.False(t, called, "the token is not read when nothing is to be tested")
	})

	t.Run("named by ID: theirs is hidden, ours is not, and one that is not there is neither", func(t *testing.T) {
		rw := scopedRewriter{
			"Person_1__scope": {typeName: "Person", scopes: scope},
			"Person_2__scope": {typeName: "Person", scopes: scope},
			"Person_3__scope": {typeName: "Person", scopes: scope},
		}
		hidden, err := referencesOutOfScope(rw, map[string][]idExistenceRes{
			"Person_1__scope": scopeRows(t, theirs),
			"Person_2__scope": scopeRows(t, ours),
		}, claims)
		require.NoError(t, err)
		require.Equal(t, map[string]bool{"Person_1": true}, hidden)
	})

	t.Run("named by an @id value: theirs fails the mutation and does not say which", func(t *testing.T) {
		rw := scopedRewriter{"Person_1__scope": {typeName: "Person", scopes: scope, byXID: true}}
		_, err := referencesOutOfScope(rw, map[string][]idExistenceRes{"Person_1__scope": scopeRows(t, theirs)}, claims)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "0x1")
		require.NotContains(t, err.Error(), "globex")
	})

	t.Run("a token that cannot be read fails the mutation", func(t *testing.T) {
		rw := scopedRewriter{"Person_1__scope": {typeName: "Person", scopes: scope}}
		_, err := referencesOutOfScope(rw, map[string][]idExistenceRes{"Person_1__scope": scopeRows(t, ours)},
			func() (map[string]interface{}, error) { return nil, errors.New("bad token") })
		require.Error(t, err)
	})
}

const referenceScopeSchema = `
type Space @referenceScope(claim: "ws", path: "name") {
	id: ID!
	name: String! @id
}

interface Member @referenceScope(claim: "ws", path: "inSpace.name") {
	inSpace: Space
}

type Team implements Member {
	id: ID!
	title: String
}

interface Grouped @referenceScope(claim: "ws", path: "inTeam.inSpace.name") {
	inTeam: [Team]
}

interface Holder {
	id: ID!
}

type Person implements Holder & Member & Grouped {
	nick: String
}

type Firm implements Holder & Grouped {
	title: String
}

type Bot implements Holder {
	model: String
}

type File {
	id: ID!
	label: String
	heldBy: [Holder]
	inTeam: Team
	madeBy: Bot
}
`

func TestReferenceScopeBlocks(t *testing.T) {
	sch := test.LoadSchemaFromString(t, referenceScopeSchema)
	op, err := sch.Operation(&schema.Request{Query: `mutation {
		addFile(input: [{label: "a", heldBy: [{id: "0x1"}], inTeam: {id: "0x2"}, madeBy: {id: "0x3"}}]) { numUids }
	}`})
	require.NoError(t, err)
	m := op.Mutations()[0]
	arw := NewAddRewriter().(*AddRewriter)
	queries, types, err := arw.RewriteQueries(context.Background(), m)
	require.NoError(t, err)
	require.Equal(t, len(queries), len(types))

	byName := make(map[string]*dql.GraphQuery)
	for _, q := range queries {
		byName[q.Attr] = q
	}
	checks := arw.referenceScopeChecks()
	// A Bot has no rule, so naming one adds no block. A Holder can be a Bot, and a
	// Bot is let through by type.
	require.Len(t, checks, 2)
	var holder, team *refScopeCheck
	for name, c := range checks {
		require.NotNil(t, byName[name], "the block is sent with the existence queries")
		switch c.typeName {
		case "Holder":
			holder = c
			text := dgraph.AsString([]*dql.GraphQuery{byName[name]})
			require.Contains(t, text, "func: uid(0x1)")
			require.Contains(t, text, "Member.inSpace")
			require.Contains(t, text, "Grouped.inTeam")
			require.Contains(t, text, "Space.name")
		case "Team":
			team = c
		}
	}
	require.NotNil(t, holder)
	require.NotNil(t, team)
	require.Equal(t, []string{"Bot"}, holder.unscoped)
	require.Len(t, holder.scopes, 2)
	require.Empty(t, team.unscoped)
	require.Equal(t, []schema.ReferenceScope{{Claim: "ws", Edges: []string{"Member.inSpace"}, Value: "Space.name"}},
		team.scopes)
}

func TestReferenceScopeNotAskedForWhatAnUpdateRemoves(t *testing.T) {
	sch := test.LoadSchemaFromString(t, referenceScopeSchema)
	op, err := sch.Operation(&schema.Request{Query: `mutation {
		updateFile(input: {filter: {id: ["0x9"]}, set: {inTeam: {id: "0x2"}}, remove: {heldBy: [{id: "0x1"}]}}) { numUids }
	}`})
	require.NoError(t, err)
	urw := NewUpdateRewriter().(*UpdateRewriter)
	_, _, err = urw.RewriteQueries(context.Background(), op.Mutations()[0])
	require.NoError(t, err)
	checks := urw.referenceScopeChecks()
	require.Len(t, checks, 1)
	for _, c := range checks {
		require.Equal(t, "Team", c.typeName)
	}
}

// A @default can build a reference from what the caller sent. rewriteObject takes the
// blocks for it and tests them itself, against the claims it was given.
func TestReferenceScopeOfAReferenceTheServerMade(t *testing.T) {
	scope := []schema.ReferenceScope{{Claim: "ws", Edges: []string{"Member.inSpace"}, Value: "Space.name"}}
	theirs := `[{"uid":"0x1","dgraph.type":["Person"],"s0":{"v":"globex"}}]`
	ours := `[{"uid":"0x2","dgraph.type":["Person"],"s0":{"v":"acme"}}]`
	held := map[string]interface{}{"ws": "acme"}

	t.Run("taken blocks are handed over once, and are not left for the mutation's own pass", func(t *testing.T) {
		xm := &xidMetadata{
			pendingScope:  []*dql.GraphQuery{{Attr: "Person_1__scope"}},
			pendingChecks: []*refScopeCheck{{typeName: "Person", scopes: scope}},
		}
		queries, checks := xm.takeReferenceScopeChecks()
		require.Len(t, queries, 1)
		require.Contains(t, checks, "Person_1__scope")
		require.Empty(t, xm.refScopes)
		queries, checks = xm.takeReferenceScopeChecks()
		require.Empty(t, queries)
		require.Empty(t, checks)
	})

	t.Run("nothing taken, nothing tested", func(t *testing.T) {
		hidden, err := outOfScope(nil, map[string][]idExistenceRes{"Person_1": scopeRows(t, theirs)}, held)
		require.NoError(t, err)
		require.Empty(t, hidden)
	})

	t.Run("named by ID: theirs is hidden, ours is not", func(t *testing.T) {
		checks := map[string]*refScopeCheck{
			"Person_1__scope": {typeName: "Person", scopes: scope},
			"Person_2__scope": {typeName: "Person", scopes: scope},
		}
		hidden, err := outOfScope(checks, map[string][]idExistenceRes{
			"Person_1__scope": scopeRows(t, theirs),
			"Person_2__scope": scopeRows(t, ours),
		}, held)
		require.NoError(t, err)
		require.Equal(t, map[string]bool{"Person_1": true}, hidden)
	})

	t.Run("a caller with no claims can have no scoped record named for it", func(t *testing.T) {
		checks := map[string]*refScopeCheck{"Person_2__scope": {typeName: "Person", scopes: scope}}
		hidden, err := outOfScope(checks, map[string][]idExistenceRes{"Person_2__scope": scopeRows(t, ours)}, nil)
		require.NoError(t, err)
		require.Equal(t, map[string]bool{"Person_2": true}, hidden)
	})

	t.Run("named by an @id value: theirs fails the mutation", func(t *testing.T) {
		checks := map[string]*refScopeCheck{"Person_1__scope": {typeName: "Person", scopes: scope, byXID: true}}
		_, err := outOfScope(checks, map[string][]idExistenceRes{"Person_1__scope": scopeRows(t, theirs)}, held)
		require.Error(t, err)
	})
}
