/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/dql"
	"github.com/dgraph-io/dgraph/v25/graphql/dgraph"
)

func uidOf(name string) *dql.FilterTree {
	return &dql.FilterTree{Func: &dql.Function{Name: "uid", Args: []dql.Arg{{Value: name}}}}
}

func notDeleted() *dql.FilterTree {
	return &dql.FilterTree{Op: "not", Child: []*dql.FilterTree{{Func: &dql.Function{
		Name: "eq", Args: []dql.Arg{{Value: "Recordable.deleted"}, {Value: "true"}}}}}}
}

// recordBlock is the block the rewriter writes for a rule that only tests the record.
func recordBlock(name, root string, filter *dql.FilterTree) *dql.GraphQuery {
	return &dql.GraphQuery{
		Var: name, Attr: "var",
		Func:     &dql.Function{Name: "uid", Args: []dql.Arg{{Value: root}}},
		Filter:   filter,
		Cascade:  []string{"__all__"},
		Children: []*dql.GraphQuery{{Attr: "dgraph.type"}},
	}
}

// walkBlock is a block for a rule that follows an edge.
func walkBlock(name, root string) *dql.GraphQuery {
	return &dql.GraphQuery{
		Var: name, Attr: "var",
		Func:    &dql.Function{Name: "uid", Args: []dql.Arg{{Value: root}}},
		Cascade: []string{"__all__"},
		Children: []*dql.GraphQuery{{Attr: "Job.inGroup",
			Filter:   &dql.FilterTree{Func: &dql.Function{Name: "uid", UID: []uint64{1}}},
			Children: []*dql.GraphQuery{{Attr: "uid"}}}},
	}
}

func rootOver(root string, filter *dql.FilterTree) string {
	return dgraph.AsString([]*dql.GraphQuery{{
		Var: "JobRoot", Attr: "var",
		Func:   &dql.Function{Name: "uid", Args: []dql.Arg{{Value: root}}},
		Filter: filter,
	}})
}

func withInlineRecordRules(t *testing.T) {
	was := inlineRecordRulesOn
	inlineRecordRulesOn = true
	t.Cleanup(func() { inlineRecordRulesOn = was })
}

func TestInlineRecordRules(t *testing.T) {
	withInlineRecordRules(t)

	and := func(c ...*dql.FilterTree) *dql.FilterTree { return &dql.FilterTree{Op: "and", Child: c} }

	t.Run("the block becomes its filter and is dropped", func(t *testing.T) {
		blocks := []*dql.GraphQuery{walkBlock("Job_Auth2", "Job_1"), recordBlock("Job_Auth3", "Job_1", notDeleted())}
		kept, filter := inlineRecordRules(blocks, and(uidOf("Job_Auth2"), uidOf("Job_Auth3")), "Job_1", "")
		require.Len(t, kept, 1)
		require.Equal(t, "Job_Auth2", kept[0].Var)
		require.Equal(t, "query {\n  JobRoot as var(func: uid(Job_1)) "+
			"@filter((uid(Job_Auth2) AND NOT (eq(Recordable.deleted, true))))\n}", rootOver("Job_1", filter))
	})

	t.Run("records not known to have the type keep the type test", func(t *testing.T) {
		blocks := []*dql.GraphQuery{recordBlock("Job_Auth3", "Job_1", notDeleted())}
		kept, filter := inlineRecordRules(blocks, uidOf("Job_Auth3"), "Job_1", "Job")
		require.Empty(t, kept)
		require.Equal(t, "query {\n  JobRoot as var(func: uid(Job_1)) "+
			"@filter((type(Job) AND NOT (eq(Recordable.deleted, true))))\n}", rootOver("Job_1", filter))
	})

	t.Run("the caller's filter and blocks are not changed in place", func(t *testing.T) {
		block := recordBlock("Job_Auth3", "Job_1", notDeleted())
		before := and(uidOf("Job_Auth2"), uidOf("Job_Auth3"))
		_, after := inlineRecordRules([]*dql.GraphQuery{block}, before, "Job_1", "")
		require.Equal(t, "uid", before.Child[1].Func.Name)
		require.NotSame(t, block.Filter, after.Child[1])
	})

	left := func(t *testing.T, name string, blocks []*dql.GraphQuery, filter *dql.FilterTree, root string) {
		t.Run(name, func(t *testing.T) {
			want := rootOver(root, filter)
			kept, got := inlineRecordRules(blocks, filter, root, "")
			require.Len(t, kept, len(blocks))
			require.Equal(t, want, rootOver(root, got))
		})
	}

	left(t, "a block that follows an edge is left",
		[]*dql.GraphQuery{walkBlock("Job_Auth2", "Job_1")}, uidOf("Job_Auth2"), "Job_1")

	left(t, "a block over other records is left",
		[]*dql.GraphQuery{recordBlock("Job_Auth3", "Job_9", notDeleted())}, uidOf("Job_Auth3"), "Job_1")

	left(t, "a block whose filter names a variable is left",
		[]*dql.GraphQuery{recordBlock("Job_Auth3", "Job_1", and(notDeleted(), uidOf("Group_Auth7")))},
		uidOf("Job_Auth3"), "Job_1")

	uidIn := &dql.FilterTree{Func: &dql.Function{
		Name: "uid_in", Args: []dql.Arg{{Value: "Job.inGroup"}, {Value: "0x1"}}}}
	left(t, "a block whose filter follows an edge is left",
		[]*dql.GraphQuery{recordBlock("Job_Auth3", "Job_1", uidIn)}, uidOf("Job_Auth3"), "Job_1")

	counted := &dql.FilterTree{Func: &dql.Function{
		Name: "gt", IsCount: true, Args: []dql.Arg{{Value: "Job.hasApplications"}, {Value: "0"}}}}
	left(t, "a block whose filter counts an edge is left",
		[]*dql.GraphQuery{recordBlock("Job_Auth3", "Job_1", counted)}, uidOf("Job_Auth3"), "Job_1")

	paged := recordBlock("Job_Auth3", "Job_1", notDeleted())
	paged.Args = map[string]string{"first": "1"}
	left(t, "a block that takes a page is left", []*dql.GraphQuery{paged}, uidOf("Job_Auth3"), "Job_1")

	uncascaded := recordBlock("Job_Auth3", "Job_1", notDeleted())
	uncascaded.Cascade = nil
	left(t, "a block without @cascade is left", []*dql.GraphQuery{uncascaded}, uidOf("Job_Auth3"), "Job_1")

	selects := recordBlock("Job_Auth3", "Job_1", notDeleted())
	selects.Children = append(selects.Children, &dql.GraphQuery{Attr: "Job.title"})
	left(t, "a block that selects a value is left", []*dql.GraphQuery{selects}, uidOf("Job_Auth3"), "Job_1")

	user := walkBlock("Job_Auth2", "Job_1")
	user.Children[0].Filter = uidOf("Job_Auth3")
	left(t, "a block another block reads is left",
		[]*dql.GraphQuery{user, recordBlock("Job_Auth3", "Job_1", notDeleted())},
		and(uidOf("Job_Auth2"), uidOf("Job_Auth3")), "Job_1")

	left(t, "a block the filter does not use is left",
		[]*dql.GraphQuery{recordBlock("Job_Auth3", "Job_1", notDeleted())}, uidOf("Job_Auth30"), "Job_1")

	t.Run("off, nothing changes", func(t *testing.T) {
		inlineRecordRulesOn = false
		defer func() { inlineRecordRulesOn = true }()
		blocks := []*dql.GraphQuery{recordBlock("Job_Auth3", "Job_1", notDeleted())}
		kept, filter := inlineRecordRules(blocks, uidOf("Job_Auth3"), "Job_1", "")
		require.Len(t, kept, 1)
		require.Equal(t, "uid", filter.Func.Name)
	})
}
