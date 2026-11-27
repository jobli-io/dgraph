/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"testing"

	dgoapi "github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/dql"
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/graphql/test"
)

func TestChangeReadFromTheMutationSent(t *testing.T) {
	t.Run("an add: the record made, and what it wrote on records already there", func(t *testing.T) {
		r := newReader()
		r.read([]*dgoapi.Mutation{{SetJson: []byte(`{
			"uid": "_:Note_1", "dgraph.type": ["Note", "Recordable"], "Note.text": "hello",
			"Note.about": [{"uid": "0x10", "Owner.notes": [{"uid": "_:Note_1"}]}],
			"Note.kind": {"uid": "0x20"},
			"Note.audit": {"dgraph.type": ["Audit"], "Audit.at": "now"}
		}`)}}, map[string]string{"Note_1": "0x99", "dg.1": "0x9a"})
		c := r.close(true, []string{"0x99"}, nil)
		require.True(t, c.Complete)
		require.Equal(t, ChangeAdd, c.Kind)
		require.Equal(t, []string{"0x99", "0x9a"}, c.New)
		require.Equal(t, []string{"Audit", "Note", "Recordable"}, c.NewTypes)
		// the candidate had its inverse edge written; the kind had nothing written on it
		require.Equal(t, map[string][]string{"0x10": {"Owner.notes"}}, c.Subjects)
		require.Equal(t, map[string][]string{"0x10": {"Note.about"}, "0x20": {"Note.kind"}}, c.Objects)
		require.Contains(t, c.Written, "Note.text")
		require.Contains(t, c.Written, "Audit.at")
		require.Contains(t, c.Written, "dgraph.type")
	})

	t.Run("an update: the records its filter matched are its subjects", func(t *testing.T) {
		r := newReader()
		r.read([]*dgoapi.Mutation{{
			SetJson:    []byte(`{"uid": "uid(x)", "Note.text": "edited"}`),
			DeleteJson: []byte(`{"uid": "uid(x)", "Note.kind": {"uid": "0x20"}}`),
		}}, nil)
		c := r.close(false, []string{"0x1", "0x2"}, map[string]interface{}{})
		require.True(t, c.Complete)
		require.Equal(t, ChangeUpdate, c.Kind)
		require.Equal(t, map[string][]string{"0x1": {"Note.kind", "Note.text"}, "0x2": {"Note.kind", "Note.text"}}, c.Subjects)
		require.Equal(t, map[string][]string{"0x20": {"Note.kind"}}, c.Objects)
	})

	t.Run("an add that found its record there is an update", func(t *testing.T) {
		r := newReader()
		r.read([]*dgoapi.Mutation{{SetJson: []byte(`{"uid": "uid(Tag_1)", "Tag.name": "x"}`)}}, nil)
		c := r.close(true, []string{"0x5"}, map[string]interface{}{"Tag_1": []interface{}{map[string]interface{}{"uid": "0x5"}}})
		require.True(t, c.Complete)
		require.Equal(t, ChangeUpdate, c.Kind)
		require.Equal(t, map[string][]string{"0x5": {"Tag.name"}}, c.Subjects)
	})

	t.Run("a record it cannot name leaves the change incomplete", func(t *testing.T) {
		r := newReader()
		r.read([]*dgoapi.Mutation{{DeleteJson: []byte(`{"uid": "uid(Old_3)", "Owner.notes": {"uid": "uid(x)"}}`)}}, nil)
		c := r.close(false, []string{"0x1"}, map[string]interface{}{})
		require.False(t, c.Complete)
		require.Equal(t, "uid(Old_3)", c.Unnamed)
	})

	t.Run("a whole record deleted, or a mutation not in JSON, leaves it incomplete", func(t *testing.T) {
		r := newReader()
		r.read([]*dgoapi.Mutation{{DeleteJson: []byte(`[{"uid": "0x7"}]`)}}, nil)
		require.False(t, r.close(false, nil, nil).Complete)

		r = newReader()
		r.read([]*dgoapi.Mutation{{SetNquads: []byte(`<0x1> <Note.text> "x" .`)}}, nil)
		require.False(t, r.close(false, nil, nil).Complete)
	})

	t.Run("a point is a value, not a record", func(t *testing.T) {
		r := newReader()
		r.read([]*dgoapi.Mutation{{SetJson: []byte(`{"uid": "0x1", "Person.home": {"type": "Point", "coordinates": [1, 2]}}`)}}, nil)
		c := r.close(false, []string{"0x1"}, nil)
		require.True(t, c.Complete)
		require.Equal(t, []string{"Person.home"}, c.Written)
		require.Empty(t, c.Objects)
	})

	t.Run("nobody listening: nothing is read", func(t *testing.T) {
		var r *changeReader
		r.read([]*dgoapi.Mutation{{SetJson: []byte(`{}`)}}, nil)
		require.Nil(t, r.change(nil, nil, nil))
	})
}

const factsSchema = `
interface Owner {
	id: ID!
	notes: [Note] @hasInverse(field: about)
}
type Person implements Owner {
	name: String @search(by: [regexp])
	home: Point
	team: Team @search
}
type Team {
	id: ID!
	label: String @search(by: [hash])
	members: [Person] @hasInverse(field: team)
}
type Kind {
	id: ID!
	name: String @search(by: [hash])
	ofNotes: [Note] @hasInverse(field: kind)
}
type Note {
	id: ID!
	text: String @search(by: [fulltext])
	deleted: Boolean @search
	updatedAt: DateTime @search
	about: [Owner] @search @bypassAuth
	kind: Kind @search
}
`

func factsQuery(t *testing.T, query string) schema.Query {
	sch := test.LoadSchemaFromString(t, factsSchema)
	op, err := sch.Operation(&schema.Request{Query: query})
	require.NoError(t, err)
	return test.GetQuery(t, op)
}

func keys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

func TestPageShape(t *testing.T) {
	sch := test.LoadSchemaFromString(t, factsSchema)
	shape := func(query string) *PageShape {
		op, err := sch.Operation(&schema.Request{Query: query})
		require.NoError(t, err)
		return ShapeOf(op)
	}

	t.Run("a list: what it reads on its rows, and below them", func(t *testing.T) {
		s := shape(`query { data: queryNote(
				filter: {and: [{deleted: false}, {about: {id: ["0x10"]}}]}, order: {desc: updatedAt}, first: 5) {
			id text kind { id name }
			about { id ... on Person { name home { latitude longitude } team(filter: {label: {eq: "a"}}) { id } } }
			aboutAggregate(filter: {has: [notes]}) { count }
		} }`)
		require.True(t, s.Plain, s.Why)
		require.Equal(t, "Note", s.RootType.Name())
		require.ElementsMatch(t, []string{"Note.text", "Note.kind", "Note.about", "Note.deleted", "Note.updatedAt"}, keys(s.RootReads))
		require.ElementsMatch(t, []string{"Kind.name", "Person.name", "Person.home", "Person.team", "Team.label", "Owner.notes"},
			keys(s.NestedReads))
		for _, typ := range []string{"Kind", "Owner", "Person", "Team"} {
			require.Contains(t, s.NestedTypes, typ)
		}
		require.NotContains(t, s.NestedTypes, "Note")
		// the edge the filter follows is also written on the owner, as its inverse
		require.Equal(t, []string{"Owner.notes"}, keys(s.FilterInverse))
		require.ElementsMatch(t, []string{"data.about.home", "data.aboutAggregate"}, keys(s.Counts))
	})

	t.Run("a count is a page of its own type", func(t *testing.T) {
		s := shape(`query { n: aggregateNote(filter: {deleted: false}) { count } }`)
		require.True(t, s.Plain, s.Why)
		require.Equal(t, "Note", s.RootType.Name())
		require.Equal(t, []string{"Note.deleted"}, keys(s.RootReads))
		require.Equal(t, []string{"n"}, keys(s.Counts))
	})

	t.Run("an interface page can have rows of each type that implements it", func(t *testing.T) {
		s := shape(`query { queryOwner { id } }`)
		require.True(t, s.Plain, s.Why)
		require.Contains(t, s.RootTypes, "Person")
	})

	t.Run("what is not reasoned about", func(t *testing.T) {
		require.False(t, shape(`query { queryNote { id } queryKind { id } }`).Plain)
		require.False(t, shape(`query { aggregateNote { count textMax } }`).Plain)
	})
}

func TestUsableParts(t *testing.T) {
	q := factsQuery(t, `query { queryNote(filter: {
		and: [{deleted: false}, {about: {id: ["0x10"]}}, {kind: {name: {eq: "call"}}}, {or: [{text: {alloftext: "a"}}, {has: [updatedAt]}]}]
	}) { id } }`)
	typ := q.ConstructedFor()

	t.Run("a filter splits into the parts that all have to hold", func(t *testing.T) {
		require.Len(t, conjuncts(extractQueryFilter(q)), 4)
		// an object with an `or` is one part: its other keys are one side of the or
		require.Len(t, conjuncts(map[string]interface{}{"deleted": false, "or": map[string]interface{}{"has": []interface{}{"text"}}}), 1)
		require.Empty(t, conjuncts(nil))
	})

	t.Run("a part that goes through another type's rules is not used", func(t *testing.T) {
		made, changed := usableParts(typ, extractQueryFilter(q), nil, nil)
		// `kind` is filtered through Kind's rules; `about` is @bypassAuth
		require.Len(t, made, 3)
		require.Len(t, changed, 3)
		for _, part := range made {
			require.NotContains(t, part, "kind")
		}
	})

	t.Run("for a record that was there, a part that reads what was written says nothing", func(t *testing.T) {
		made, changed := usableParts(typ, extractQueryFilter(q), nil, []string{"Note.deleted", "Note.text"})
		require.Len(t, made, 3)
		require.Len(t, changed, 1)
		require.Contains(t, changed[0], "about")

		_, changed = usableParts(typ, extractQueryFilter(q), nil, []string{"Note.about", "Owner.notes"})
		require.Len(t, changed, 2)
		for _, part := range changed {
			require.NotContains(t, part, "about")
		}
	})
}

func TestWhatARunRead(t *testing.T) {
	rf := &RunFacts{}
	settled := &dql.GraphQuery{Attr: "var", Func: &dql.Function{Name: "uid", UID: []uint64{7}},
		Children: []*dql.GraphQuery{{Attr: "Group.hasNote", Var: "Note_3_uids"}}}
	own := &dql.GraphQuery{Attr: "var", Var: "Note_Auth2", Func: &dql.Function{Name: "uid"},
		Filter:   &dql.FilterTree{Op: "not", Child: []*dql.FilterTree{{Func: &dql.Function{Name: "eq", Args: []dql.Arg{{Value: "Note.deleted"}, {Value: "true"}}}}}},
		Children: []*dql.GraphQuery{{Attr: "dgraph.type"}}}
	rule := &dql.GraphQuery{Attr: "var", Var: "Note_Auth4", Func: &dql.Function{Name: "uid"},
		Children: []*dql.GraphQuery{{Attr: "Note.inSpace", Children: []*dql.GraphQuery{{Attr: "Space.members"}}}}}

	vg := NewVariableGenerator()
	vg.settledRule("Note.inGroup", "Group.hasNote", settled)
	vg.ruleCameTo([]*dql.GraphQuery{settled, own}, &dql.FilterTree{
		Func: &dql.Function{Name: "uid_in", Args: []dql.Arg{{Value: "Note.owner"}, {Value: "0x5"}}}})
	require.False(t, vg.ruled, "a settled rule and a test of the record's own predicates are filters")

	main := &dql.GraphQuery{Attr: "queryNote", Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: "Note"}}},
		Filter: &dql.FilterTree{Func: &dql.Function{Name: "uid_in", Args: []dql.Arg{{Value: "Note.about"}, {Value: "0x10"}}}},
		Children: []*dql.GraphQuery{{Attr: "Note.text@en"}, {Attr: "uid"},
			{Attr: "~Note.kind", Children: []*dql.GraphQuery{{Attr: "Kind.name"}}}}}
	ctx := WithRunFacts(t.Context(), rf)
	noteRun(ctx, []*dql.GraphQuery{main, settled, own}, vg)
	require.False(t, rf.Ruled)
	require.Equal(t, 1, rf.Queries)
	require.ElementsMatch(t, []string{"Note.about", "Note.text", "Note.kind", "Kind.name", "Group.hasNote", "Note.deleted"}, keys(rf.Reads))
	require.Equal(t, []string{"Note.kind"}, keys(rf.Reverse))
	require.ElementsMatch(t, []string{"Note.inGroup", "Note.deleted", "Note.owner"}, keys(rf.RuleReads))
	require.Equal(t, []string{"Group.hasNote"}, keys(rf.RuleInverse))

	t.Run("a rule that follows an edge to another record is one the store works out", func(t *testing.T) {
		vg := NewVariableGenerator()
		vg.ruleCameTo([]*dql.GraphQuery{rule}, nil)
		require.True(t, vg.ruled)
		rf := &RunFacts{}
		noteRun(WithRunFacts(t.Context(), rf), []*dql.GraphQuery{main, rule}, vg)
		require.True(t, rf.Ruled)
		require.NotEmpty(t, rf.RuledBy)
	})

	t.Run("nobody asked: nothing is noted", func(t *testing.T) {
		noteRun(t.Context(), []*dql.GraphQuery{main}, nil)
	})
}
