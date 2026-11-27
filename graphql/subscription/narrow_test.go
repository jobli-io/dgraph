/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package subscription

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/graphql/resolve"
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/graphql/test"
)

const narrowSchema = `
interface Owner {
	id: ID!
	notes: [Note] @hasInverse(field: about)
}
type Person implements Owner {
	name: String @search(by: [regexp])
	team: Team
}
type Team {
	id: ID!
	label: String
}
type Kind {
	id: ID!
	name: String
}
type Note {
	id: ID!
	text: String
	deleted: Boolean @search
	updatedAt: DateTime @search
	about: [Owner] @search @bypassAuth
	kind: Kind
}
`

func set(preds ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(preds))
	for _, p := range preds {
		out[p] = struct{}{}
	}
	return out
}

// pageOf is a page that has run once with its rules settled: reads is what the store
// was asked for beyond what the GraphQL names.
func pageOf(t *testing.T, query string, reads ...string) *page {
	sch := test.LoadSchemaFromString(t, narrowSchema)
	op, err := sch.Operation(&schema.Request{Query: query})
	require.NoError(t, err)
	shape := resolve.ShapeOf(op)
	require.True(t, shape.Plain, shape.Why)
	pg := &page{shape: shape, reads: set(reads...), ran: true, whole: true}
	for p := range shape.RootReads {
		pg.reads[p] = struct{}{}
	}
	for p := range shape.NestedReads {
		pg.reads[p] = struct{}{}
	}
	return pg
}

const notesOfOwner = `query { data: queryNote(filter: {and: [{deleted: false}, {about: {id: ["0x10"]}}]}, order: {desc: updatedAt}) {
	id text kind { id name } about { id ... on Person { name } } } }`

func TestWhichPagesAChangeCanHaveChanged(t *testing.T) {
	// the notes panel of owner 0x10, showing note 0x51 of kind 0x20; the store was also
	// asked for the inverse edge its filter follows, and a rule tests Note.deleted
	held := set("0x10", "0x51", "0x20")
	holds := func(uid string) bool { _, ok := held[uid]; return ok }
	panel := func(t *testing.T) *page {
		pg := pageOf(t, notesOfOwner, "Owner.notes")
		pg.ruleReads = set("Note.deleted")
		return pg
	}
	note := []string{"Note"}

	t.Run("a note added, to anyone: the store is asked whether it is a row", func(t *testing.T) {
		for _, owner := range []string{"0x10", "0x11"} {
			c := &resolve.Change{Kind: resolve.ChangeAdd, Complete: true, Roots: []string{"0x99"}, New: []string{"0x99"},
				NewTypes: []string{"Note"}, Written: []string{"Note.text", "Note.about", "Owner.notes"},
				Subjects: map[string][]string{owner: {"Owner.notes"}},
				Objects:  map[string][]string{owner: {"Note.about"}, "0x20": {"Note.kind"}}}
			v, why, made, changed := panel(t).decide(c, note, holds)
			require.Equal(t, ask, v, why)
			require.Equal(t, []string{"0x99"}, made)
			require.Empty(t, changed)
		}
	})

	t.Run("a note it shows, edited: run", func(t *testing.T) {
		c := &resolve.Change{Kind: resolve.ChangeUpdate, Complete: true, Roots: []string{"0x51"},
			Written: []string{"Note.text"}, Subjects: map[string][]string{"0x51": {"Note.text"}}}
		v, why, _, _ := panel(t).decide(c, note, holds)
		require.Equal(t, wake, v, why)
	})

	t.Run("a note it does not show, edited or removed: the store is asked", func(t *testing.T) {
		for _, pred := range []string{"Note.text", "Note.deleted"} {
			c := &resolve.Change{Kind: resolve.ChangeUpdate, Complete: true, Roots: []string{"0x60"},
				Written: []string{pred}, Subjects: map[string][]string{"0x60": {pred}}}
			v, why, made, changed := panel(t).decide(c, note, holds)
			require.Equal(t, ask, v, why)
			require.Empty(t, made)
			require.Equal(t, []string{"0x60"}, changed)
		}
	})

	t.Run("a person it does not show, renamed: could be shown below a note, so run", func(t *testing.T) {
		c := &resolve.Change{Kind: resolve.ChangeUpdate, Complete: true, Roots: []string{"0x70"},
			Written: []string{"Person.name"}, Subjects: map[string][]string{"0x70": {"Person.name"}}}
		v, why, _, _ := panel(t).decide(c, []string{"Person"}, holds)
		require.Equal(t, wake, v, why)
	})

	t.Run("nothing it reads was written: left alone", func(t *testing.T) {
		c := &resolve.Change{Kind: resolve.ChangeUpdate, Complete: true, Roots: []string{"0x80"},
			Written: []string{"Team.label"}, Subjects: map[string][]string{"0x80": {"Team.label"}}}
		v, why, _, _ := panel(t).decide(c, []string{"Team"}, holds)
		require.Equal(t, skip, v, why)
	})

	t.Run("the far end of the edge its filter follows is not a reason by itself", func(t *testing.T) {
		// owner 0x10 had Owner.notes written, and nothing else was written or made
		c := &resolve.Change{Kind: resolve.ChangeUpdate, Complete: true, Written: []string{"Owner.notes"},
			Subjects: map[string][]string{"0x10": {"Owner.notes"}}}
		v, why, _, _ := panel(t).decide(c, note, holds)
		require.Equal(t, skip, v, why)
	})

	t.Run("a record of a type that cannot be a row, written on a predicate the rows share: left alone", func(t *testing.T) {
		c := &resolve.Change{Kind: resolve.ChangeUpdate, Complete: true, Roots: []string{"0x81"},
			Written: []string{"Note.updatedAt"}, Subjects: map[string][]string{"0x81": {"Note.updatedAt"}}}
		v, why, _, _ := panel(t).decide(c, []string{"Other"}, holds)
		require.Equal(t, skip, v, why)
	})

	t.Run("an edge written against the direction it reads: run if it holds the record pointed at", func(t *testing.T) {
		pg := panel(t)
		pg.reverse = set("Note.kind")
		c := &resolve.Change{Kind: resolve.ChangeUpdate, Complete: true, Written: []string{"Note.kind"},
			Objects: map[string][]string{"0x20": {"Note.kind"}}}
		v, why, _, _ := pg.decide(c, note, holds)
		require.Equal(t, wake, v, why)
	})

	t.Run("a count is asked about a note added, and left alone when one is edited", func(t *testing.T) {
		count := pageOf(t, `query { data: aggregateNote(filter: {and: [{deleted: false}, {about: {id: ["0x10"]}}]}) { count } }`, "Owner.notes")
		added := &resolve.Change{Kind: resolve.ChangeAdd, Complete: true, Roots: []string{"0x99"}, New: []string{"0x99"},
			NewTypes: []string{"Note"}, Written: []string{"Note.text", "Note.about"}}
		v, why, made, _ := count.decide(added, note, holds)
		require.Equal(t, ask, v, why)
		require.Equal(t, []string{"0x99"}, made)

		edited := &resolve.Change{Kind: resolve.ChangeUpdate, Complete: true, Roots: []string{"0x60"},
			Written: []string{"Note.text"}, Subjects: map[string][]string{"0x60": {"Note.text"}}}
		v, why, _, _ = count.decide(edited, note, holds)
		require.Equal(t, skip, v, why)
	})

	t.Run("when it cannot reason, it does as today", func(t *testing.T) {
		edit := &resolve.Change{Kind: resolve.ChangeUpdate, Complete: true, Roots: []string{"0x60"},
			Written: []string{"Note.text"}, Subjects: map[string][]string{"0x60": {"Note.text"}}}
		for name, spoil := range map[string]func(*page, *resolve.Change){
			"the change could not name a record it wrote to": func(_ *page, c *resolve.Change) { c.Complete = false },
			"a delete":                       func(_ *page, c *resolve.Change) { c.Kind = resolve.ChangeDelete },
			"rules the store works out":      func(pg *page, _ *resolve.Change) { pg.ruled = true },
			"a record it shows without name": func(pg *page, _ *resolve.Change) { pg.whole = false },
			"it has not run":                 func(pg *page, _ *resolve.Change) { pg.ran = false },
			"a query that is not one page":   func(pg *page, _ *resolve.Change) { pg.shape = &resolve.PageShape{} },
		} {
			pg, c := panel(t), *edit
			spoil(pg, &c)
			v, _, _, _ := pg.decide(&c, note, holds)
			require.Equal(t, asToday, v, name)
		}
		var none *page
		v, _, _, _ := none.decide(edit, note, holds)
		require.Equal(t, asToday, v)
		v, _, _, _ = panel(t).decide(nil, note, holds)
		require.Equal(t, asToday, v)
	})

	t.Run("while it is running, what it holds is about to change: run again", func(t *testing.T) {
		pg := panel(t)
		pg.begin()
		c := &resolve.Change{Kind: resolve.ChangeUpdate, Complete: true, Written: []string{"Team.label"}}
		v, why, _, _ := pg.decide(c, []string{"Team"}, holds)
		require.Equal(t, wake, v, why)
		pg.end(nil, nil, false)
		v, why, _, _ = pg.decide(c, []string{"Team"}, holds)
		require.Equal(t, skip, v, why)
	})
}

func TestARunIsTakenIn(t *testing.T) {
	pg := pageOf(t, notesOfOwner)
	pg.ran, pg.whole = false, false
	pg.begin()
	pg.end(&resolve.RunFacts{Queries: 1, Reads: set("Note.text", "Owner.notes"), Reverse: set("Note.kind"),
		RuleReads: set("Note.deleted")}, []byte(`{"data":[{"id":"0x51","kind":{"id":"0x20"},"about":[{"id":"0x10"}]}]}`), true)
	require.True(t, pg.ran)
	require.True(t, pg.whole)
	require.False(t, pg.ruled)
	require.Zero(t, pg.running)
	require.Contains(t, pg.reads, "Owner.notes")

	// what was read is kept from every run; how the rules were answered is the last run's
	pg.begin()
	pg.end(&resolve.RunFacts{Queries: 1, Ruled: true, RuledBy: "a block", Reads: set("Group.name")},
		[]byte(`{"data":[{"id":"0x51","kind":{"name":"call"}}]}`), true)
	require.True(t, pg.ruled)
	require.False(t, pg.whole)
	require.Equal(t, "data.kind", pg.unnamed)
	require.Contains(t, pg.reads, "Owner.notes")
	require.Contains(t, pg.reads, "Group.name")

	// a run in which no query was seen says nothing of its rules
	pg.begin()
	pg.end(&resolve.RunFacts{}, []byte(`{"data":[]}`), true)
	require.True(t, pg.ruled)
}

func TestRecordsAnAnswerDoesNotName(t *testing.T) {
	counts := set("data.home", "data.notesAggregate", "n")
	require.Empty(t, unnamedRecord([]byte(`{"data":[{"id":"0x1","kind":{"id":"0x2","name":"a"},"about":[{"id":"0x3"}]}]}`), nil))
	require.Empty(t, unnamedRecord([]byte(`{"data":[]}`), nil))
	require.Empty(t, unnamedRecord([]byte(`{"data":null}`), nil))
	require.Equal(t, "data.kind", unnamedRecord([]byte(`{"data":[{"id":"0x1","kind":{"name":"a"}}]}`), nil))
	require.Equal(t, "data", unnamedRecord([]byte(`{"data":[{"text":"a"}]}`), nil))
	// a count, and a point on a map, hold no record
	require.Empty(t, unnamedRecord([]byte(`{"data":[{"id":"0x1","home":{"latitude":1,"longitude":2},"notesAggregate":{"count":3}}]}`), counts))
	require.Empty(t, unnamedRecord([]byte(`{"n":{"count":4}}`), counts))
	require.NotEmpty(t, unnamedRecord([]byte(`not json`), nil))
}

func TestACountIsFiledUnderTheTypeItCounts(t *testing.T) {
	count := pageOf(t, `query { data: aggregateNote(filter: {about: {id: ["0x10"]}}) { count } }`)
	dr := NewDependencyRegistry()
	key := subKey{bucketID: 1, subID: 1}
	dr.subUIDs[key], dr.subTypes[key], dr.subPredicates[key] = map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	_, mentioning := dr.AffectedBuckets(nil, []string{"Note"}, nil)
	require.Empty(t, mentioning)

	dr.Mention(1, 1, count.shape.RootTypes, count.shape.NestedTypes)
	_, mentioning = dr.AffectedBuckets(nil, []string{"Note"}, nil)
	require.Equal(t, []uint64{1}, mentioning)
	// the type its filter looks at, too
	_, mentioning = dr.AffectedBuckets(nil, []string{"Person"}, nil)
	require.Equal(t, []uint64{1}, mentioning)

	dr.Mention(2, 2, count.shape.RootTypes) // nobody: nothing filed
	require.False(t, dr.Holds(1, "0x10"))
	dr.addUIDDep(key, "0x10")
	require.True(t, dr.Holds(1, "0x10"))
	require.False(t, dr.Holds(2, "0x10"))
}

// A note moved from one owner to another. The count of each owner's notes is asked of
// the store from the owner's end, through the inverse predicate, so the DQL never
// names the edge itself. The proof cluster caught this: both counts were left stale.
func TestAnEdgeReadOnlyFromItsFarEndStillCounts(t *testing.T) {
	held := set("0x10")
	holds := func(uid string) bool { _, ok := held[uid]; return ok }
	moved := &resolve.Change{Kind: resolve.ChangeUpdate, Complete: true, Roots: []string{"0x60"},
		Written:  []string{"Note.about", "Owner.notes"},
		Subjects: map[string][]string{"0x60": {"Note.about"}, "0x10": {"Owner.notes"}, "0x11": {"Owner.notes"}},
		Objects:  map[string][]string{"0x10": {"Note.about"}, "0x11": {"Note.about"}}}

	count := pageOf(t, `query { data: aggregateNote(filter: {about: {id: ["0x10"]}}) { count } }`)
	count.reads = set("Owner.notes") // all the store was asked to read
	v, why, _, changed := count.decide(moved, []string{"Note"}, holds)
	require.Equal(t, ask, v, why)
	require.Equal(t, []string{"0x60"}, changed)

	// the same for a rule settled into a test of an edge, read from the group's end
	list := pageOf(t, `query { data: queryNote { id text } }`)
	list.reads, list.ruleReads, list.ruleInverse = set("Note.text", "Group.hasNote"), set("Note.inGroup"), set("Group.hasNote")
	regrouped := &resolve.Change{Kind: resolve.ChangeUpdate, Complete: true, Roots: []string{"0x60"},
		Written:  []string{"Note.inGroup", "Group.hasNote"},
		Subjects: map[string][]string{"0x60": {"Note.inGroup"}, "0x7": {"Group.hasNote"}}}
	v, why, _, changed = list.decide(regrouped, []string{"Note"}, holds)
	require.Equal(t, ask, v, why)
	require.Equal(t, []string{"0x60"}, changed)
}
