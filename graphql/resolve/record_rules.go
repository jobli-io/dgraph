/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"strings"

	"github.com/dgraph-io/dgraph/v25/dql"
)

// Rules about the record itself, compiled as filters (SPIKE).
//
// An @auth rule is a query, and the rewriter compiles every one into a block of
// its own and filters the records by that block's result:
//
//	JobRoot as var(func: uid(Job_1), first: 25) @filter(uid(Job_Auth2))
//	Job_Auth2 as var(func: uid(Job_1)) @filter(NOT (eq(Recordable.deleted, true))) @cascade {
//	  dgraph.type
//	}
//
// Where the rule only tests the record's own values, the block is the same set
// as its filter applied to the records directly, and it costs a read of every
// record of the type. This file puts the filter where the block's result was:
//
//	JobRoot as var(func: uid(Job_1), first: 25) @filter(NOT (eq(Recordable.deleted, true)))
//
// It is a rewrite of what the rewriter produced, not of the rule, so it does not
// depend on how the rule was written or on partitions. A block is replaced only
// when all of this is true:
//
//   - it starts from the same records the filter is applied to
//   - its filter compares the record's own values and names no variable
//   - it selects dgraph.type and nothing else
//   - nothing but the filter uses its result
//
// The block also drops a node that has no type at all. To keep that, the filter
// is joined with type(T) unless the records are known to come from type(T).

var inlineRecordRulesOn bool

// EnableInlineRecordRules turns the rewrite on.
func EnableInlineRecordRules() {
	inlineRecordRulesOn = true
}

// ownValueFuncs are the filter functions that test a value of the record itself.
var ownValueFuncs = map[string]bool{
	"eq": true, "le": true, "lt": true, "ge": true, "gt": true, "between": true, "has": true,
	"anyofterms": true, "allofterms": true, "anyoftext": true, "alloftext": true, "regexp": true, "match": true,
}

// inlineRecordRules replaces, in filter, the result of every block of blocks that
// only tests the record itself by that block's own filter, and drops the block.
// filter must be one that is applied to records drawn from uid(root). typ, when
// not empty, is the Dgraph type those records are not yet known to have.
func inlineRecordRules(blocks []*dql.GraphQuery, filter *dql.FilterTree, root, typ string) (
	[]*dql.GraphQuery, *dql.FilterTree) {

	if !inlineRecordRulesOn || filter == nil || root == "" {
		return blocks, filter
	}
	kept := make([]*dql.GraphQuery, 0, len(blocks))
	for i, b := range blocks {
		if !testsOnlyTheRecord(b, root) || !usesVar(filter, b.Var) {
			kept = append(kept, b)
			continue
		}
		usedElsewhere := false
		for j, other := range blocks {
			if j != i && blockUsesVar(other, b.Var) {
				usedElsewhere = true
				break
			}
		}
		if usedElsewhere {
			kept = append(kept, b)
			continue
		}
		with := b.Filter
		if typ != "" {
			with = &dql.FilterTree{Op: "and", Child: []*dql.FilterTree{
				{Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: typ}}}}, b.Filter}}
		}
		filter = replaceVar(filter, b.Var, with)
	}
	return kept, filter
}

// testsOnlyTheRecord reports whether b is
//
//	V as var(func: uid(root)) @filter(<own values>) @cascade { dgraph.type }
func testsOnlyTheRecord(b *dql.GraphQuery, root string) bool {
	if b == nil || b.Var == "" || b.Attr != "var" || b.Func == nil || b.Filter == nil {
		return false
	}
	if b.Func.Name != "uid" || len(b.Func.UID) != 0 || len(b.Func.Args) != 1 || b.Func.Args[0].Value != root {
		return false
	}
	if len(b.Args) != 0 || len(b.Order) != 0 || len(b.Cascade) == 0 || b.Recurse || b.ShortestPathArgs.From != nil ||
		b.ShortestPathArgs.To != nil || len(b.GroupbyAttrs) != 0 || b.Normalize || b.IgnoreReflex {
		return false
	}
	for _, c := range b.Children {
		if c.Attr != "dgraph.type" || c.Var != "" || c.Filter != nil || len(c.Children) != 0 {
			return false
		}
	}
	return len(b.Children) > 0 && ownValues(b.Filter)
}

func ownValues(f *dql.FilterTree) bool {
	if f == nil {
		return false
	}
	if f.Func != nil {
		if !ownValueFuncs[f.Func.Name] || len(f.Func.UID) != 0 || len(f.Func.NeedsVar) != 0 || len(f.Child) != 0 ||
			f.Func.IsCount || f.Func.IsValueVar || f.Func.IsLenVar {
			return false
		}
		for _, a := range f.Func.Args {
			if a.IsValueVar || a.IsDQLVar || strings.Contains(a.Value, "uid(") || strings.Contains(a.Value, "val(") {
				return false
			}
		}
		return true
	}
	if f.Op != "and" && f.Op != "or" && f.Op != "not" {
		return false
	}
	if len(f.Child) == 0 {
		return false
	}
	for _, c := range f.Child {
		if !ownValues(c) {
			return false
		}
	}
	return true
}

func isVarRef(f *dql.FilterTree, name string) bool {
	return f != nil && f.Func != nil && f.Func.Name == "uid" && len(f.Func.UID) == 0 && len(f.Func.Args) == 1 &&
		f.Func.Args[0].Value == name && len(f.Child) == 0
}

// namesVar reports whether s mentions the variable name as a whole word.
func namesVar(s, name string) bool {
	for i := strings.Index(s, name); i >= 0; {
		end := i + len(name)
		before := i == 0 || !isWordByte(s[i-1])
		after := end == len(s) || !isWordByte(s[end])
		if before && after {
			return true
		}
		next := strings.Index(s[end:], name)
		if next < 0 {
			break
		}
		i = end + next
	}
	return false
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func funcUsesVar(fn *dql.Function, name string) bool {
	if fn == nil {
		return false
	}
	for _, a := range fn.Args {
		if namesVar(a.Value, name) {
			return true
		}
	}
	for _, v := range fn.NeedsVar {
		if v.Name == name {
			return true
		}
	}
	return false
}

func usesVar(f *dql.FilterTree, name string) bool {
	if f == nil {
		return false
	}
	if funcUsesVar(f.Func, name) {
		return true
	}
	for _, c := range f.Child {
		if usesVar(c, name) {
			return true
		}
	}
	return false
}

func blockUsesVar(q *dql.GraphQuery, name string) bool {
	if q == nil {
		return false
	}
	if funcUsesVar(q.Func, name) || usesVar(q.Filter, name) {
		return true
	}
	for _, v := range q.NeedsVar {
		if v.Name == name {
			return true
		}
	}
	for _, c := range q.Children {
		if blockUsesVar(c, name) {
			return true
		}
	}
	return false
}

// replaceVar returns f with every uid(name) replaced by a copy of with. A use of
// the variable in any other form is left alone; the caller has checked for those.
func replaceVar(f *dql.FilterTree, name string, with *dql.FilterTree) *dql.FilterTree {
	if f == nil {
		return nil
	}
	if isVarRef(f, name) {
		return copyFilter(with)
	}
	if len(f.Child) == 0 {
		return f
	}
	out := *f
	out.Child = make([]*dql.FilterTree, len(f.Child))
	for i, c := range f.Child {
		out.Child[i] = replaceVar(c, name, with)
	}
	return &out
}

func copyFilter(f *dql.FilterTree) *dql.FilterTree {
	if f == nil {
		return nil
	}
	out := &dql.FilterTree{Op: f.Op}
	if f.Func != nil {
		fn := *f.Func
		fn.Args = append([]dql.Arg(nil), f.Func.Args...)
		out.Func = &fn
	}
	for _, c := range f.Child {
		out.Child = append(out.Child, copyFilter(c))
	}
	return out
}
