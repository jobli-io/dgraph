/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/dql"
	"github.com/dgraph-io/dgraph/v25/graphql/dgraph"
)

func eqFilter(pred, val string) *dql.FilterTree {
	return &dql.FilterTree{Func: &dql.Function{Name: "eq", Args: []dql.Arg{{Value: pred}, {Value: val}}}}
}

// bindingsOf is the shape of a question's blocks: the caller's user, the
// bindings of that user for a permission, and the groups those are on.
func bindingsOf(user, permission string) compiledQuestion {
	return compiledQuestion{
		vars: []*dql.GraphQuery{
			{Attr: "var", Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: "User"}}},
				Filter:   eqFilter("User.userId", user),
				Children: []*dql.GraphQuery{{Attr: "User.hasBinding", Var: "Job_Auth1"}}},
			{Attr: "var", Var: "Job_Auth2", Func: &dql.Function{Name: "uid", Args: []dql.Arg{{Value: "Job_Auth1"}}},
				Filter:  eqFilter("IAMBinding.permission", permission),
				Cascade: []string{"__all__"},
				Children: []*dql.GraphQuery{{Attr: "IAMBinding.onGroup", Var: "Job_Auth3",
					Filter: &dql.FilterTree{Op: "not", Child: []*dql.FilterTree{uidOf("Job_Auth1")}}}}},
			{Attr: "var", Var: "Job_Auth4", Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: "Group"}}},
				Filter: &dql.FilterTree{Func: &dql.Function{
					Name: "uid_in", Args: []dql.Arg{{Value: "Group.self"}, {Value: "uid(Job_Auth3)"}}}}},
		},
		of: "Group",
		held: &dql.GraphQuery{Attr: "held", Func: &dql.Function{Name: "uid", Args: []dql.Arg{{Value: "Job_Auth4"}}},
			Children: []*dql.GraphQuery{{Attr: "uid"}}},
	}
}

func together(qs ...compiledQuestion) string {
	var blocks, results []*dql.GraphQuery
	for i := range qs {
		prefixVars(&qs[i], "q"+string(rune('0'+i))+"_")
		blocks = append(blocks, qs[i].vars...)
		qs[i].held.Attr = "held_" + string(rune('0'+i))
		results = append(results, qs[i].held)
	}
	return dgraph.AsString(mergeSameBlocks(append(blocks, results...)))
}

func TestPrefixVarsRenamesEveryUse(t *testing.T) {
	c := bindingsOf("u1", "job.read")
	prefixVars(&c, "q7_")
	text := dgraph.AsString(append(c.vars, c.held))
	require.Equal(t, 0, strings.Count(strings.ReplaceAll(text, "q7_Job_Auth", ""), "Job_Auth"), text)
	// 4 definitions, and a use in a root function, a filter, a uid_in and the result.
	require.Equal(t, 8, strings.Count(text, "q7_Job_Auth"), text)
}

func TestMergeSameBlocks(t *testing.T) {
	t.Run("the same question twice is read once", func(t *testing.T) {
		text := together(bindingsOf("u1", "job.read"), bindingsOf("u1", "job.read"))
		require.Equal(t, 3, strings.Count(text, "var(func:"), text)
		require.NotContains(t, text, "q1_")
		require.Contains(t, text, "held_0(func: uid(q0_Job_Auth4))")
		require.Contains(t, text, "held_1(func: uid(q0_Job_Auth4))")
	})

	t.Run("two questions share what is the same and nothing else", func(t *testing.T) {
		text := together(bindingsOf("u1", "job.read"), bindingsOf("u1", "company.read"))
		// The caller's bindings are read once. What depends on the permission is not shared,
		// and neither is the block that only differs in which bindings it starts from.
		require.Equal(t, 1, strings.Count(text, "eq(User.userId, u1)"), text)
		require.Equal(t, 5, strings.Count(text, "var(func:"), text)
		require.Contains(t, text, "q1_Job_Auth2 as var(func: uid(q0_Job_Auth1))")
		require.Contains(t, text, "@filter(NOT (uid(q0_Job_Auth1)))")
		require.Contains(t, text, "held_0(func: uid(q0_Job_Auth4))")
		require.Contains(t, text, "held_1(func: uid(q1_Job_Auth4))")
	})

	t.Run("questions about different callers share nothing", func(t *testing.T) {
		text := together(bindingsOf("u1", "job.read"), bindingsOf("u2", "job.read"))
		require.Equal(t, 6, strings.Count(text, "var(func:"), text)
		require.Contains(t, text, "held_1(func: uid(q1_Job_Auth4))")
	})

	t.Run("blocks over different lists of UIDs are kept apart", func(t *testing.T) {
		over := func(name string, uids ...uint64) *dql.GraphQuery {
			return &dql.GraphQuery{Attr: "var", Func: &dql.Function{Name: "uid", UID: uids},
				Children: []*dql.GraphQuery{{Attr: "Group.hasJob", Var: name}}}
		}
		require.Len(t, mergeSameBlocks([]*dql.GraphQuery{over("a", 1), over("b", 2)}), 2)
		require.Len(t, mergeSameBlocks([]*dql.GraphQuery{over("a", 1), over("b", 1)}), 1)
	})
}

func TestMergeSameResults(t *testing.T) {
	held := func(name, of string) *dql.GraphQuery {
		return &dql.GraphQuery{Attr: name, Func: &dql.Function{Name: "uid", Args: []dql.Arg{{Value: of}}},
			Children: []*dql.GraphQuery{{Attr: "uid"}}}
	}
	kept, same := mergeSameResults([]*dql.GraphQuery{held("held_0", "a"), held("held_1", "b"), held("held_2", "a")})
	require.Len(t, kept, 2)
	require.Equal(t, map[string]string{"held_2": "held_0"}, same)
	// The blocks kept still have their own names.
	require.Equal(t, "held_0", kept[0].Attr)
	require.Equal(t, "held_1", kept[1].Attr)
}

func rows(uids ...string) []row {
	out := make([]row, len(uids))
	for i, u := range uids {
		out[i] = row{UID: u}
	}
	return out
}

func TestAnswerOf(t *testing.T) {
	count := func(n int) []row { return []row{{Count: n}} }

	t.Run("every record passes", func(t *testing.T) {
		a, err := answerOf(count(2), rows("0x2", "0x1"), count(2), true)
		require.NoError(t, err)
		require.True(t, a.all)
		require.Equal(t, []uint64{1, 2}, a.uids)
	})
	t.Run("some pass", func(t *testing.T) {
		a, err := answerOf(count(3), rows("0x2", "0x1"), count(2), true)
		require.NoError(t, err)
		require.False(t, a.all)
		require.Equal(t, []uint64{1, 2}, a.uids)
	})
	t.Run("none pass", func(t *testing.T) {
		a, err := answerOf(count(3), nil, count(0), true)
		require.NoError(t, err)
		require.False(t, a.all || a.tooMany || a.unknown)
		require.Empty(t, a.uids)
	})
	t.Run("a type with no records is not one whose records all pass", func(t *testing.T) {
		a, err := answerOf(count(0), nil, count(0), true)
		require.NoError(t, err)
		require.False(t, a.all)
	})
	t.Run("a record that passes and is not of the type does not make up the number", func(t *testing.T) {
		// Two records of the type, one passes; so does a record of another type.
		a, err := answerOf(count(2), rows("0x1", "0x9"), count(1), true)
		require.NoError(t, err)
		require.False(t, a.all)
	})
	t.Run("without a count, the records returned are counted", func(t *testing.T) {
		a, err := answerOf(count(1), rows("0x1"), nil, false)
		require.NoError(t, err)
		require.True(t, a.all)
		a, err = answerOf(count(1), nil, nil, false)
		require.NoError(t, err)
		require.False(t, a.all)
	})
	t.Run("more pass than a query will list", func(t *testing.T) {
		many := make([]string, maxHeld+1)
		for i := range many {
			many[i] = "0x" + strconv.FormatInt(int64(i+1), 16)
		}
		a, err := answerOf(count(60000), rows(many...), count(60000), true)
		require.NoError(t, err)
		require.True(t, a.all)
		require.True(t, a.tooMany)
		require.Nil(t, a.uids)
	})
	t.Run("a row that is not a record is an error", func(t *testing.T) {
		_, err := answerOf(count(1), rows("nope"), count(1), true)
		require.Error(t, err)
	})
}

// A block over a list of UIDs has no arguments. Two over different lists were
// taken for the same block, and the second one's users were given the first's.
func TestAuthVarDedupKeepsUIDListsApart(t *testing.T) {
	over := func(name string, uids ...uint64) *dql.GraphQuery {
		return &dql.GraphQuery{Attr: "var", Func: &dql.Function{Name: "uid", UID: uids},
			Children: []*dql.GraphQuery{{Attr: "Group.hasJob", Var: name}}}
	}
	kept, subst := deduplicateAuthVarBlocks([]*dql.GraphQuery{over("Job_Auth1_uids", 1), over("Job_Auth2_uids", 2)})
	require.Len(t, kept, 2)
	require.NotEqual(t, "Job_Auth1_uids", subst["Job_Auth2_uids"])

	kept, subst = deduplicateAuthVarBlocks([]*dql.GraphQuery{over("Job_Auth1_uids", 1), over("Job_Auth2_uids", 1)})
	require.Len(t, kept, 1)
	require.Equal(t, "Job_Auth1_uids", subst["Job_Auth2_uids"])
}
