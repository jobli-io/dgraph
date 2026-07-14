/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

// auth_var_dedup.go — deduplication of semantically identical DQL auth var blocks.
//
// When an auth rule tree has multiple branches that need the same base entity
// query (e.g. "IAMRole nodes with permission in [_ALL, _JOBAD, ...]"), the
// query rewriter emits an independent var block per branch:
//
//	var(func: type(IAMRole)) @filter(eq(IAMRole.permission, "_ALL", ...)) {
//	    JobAd_Auth2_..._forRole as IAMRole.hasBinding
//	}
//	var(func: type(IAMRole)) @filter(eq(IAMRole.permission, "_ALL", ...)) {
//	    JobAd_Auth3_..._forRole as IAMRole.hasBinding
//	}
//	...
//
// These are semantically identical: same root function, same filter, same
// edge predicate. Evaluating them independently wastes Dgraph resources.
//
// deduplicateAuthVarBlocks post-processes the collected auth var list and:
//  1. Identifies groups of semantically identical var blocks via a fingerprint
//     (root func + filter + cascade + child edge attrs) that excludes the
//     block's own var name.
//  2. Keeps one canonical var block per group and builds a substitution map
//     from every non-canonical var name to the canonical one.
//  3. Applies the substitution recursively to all remaining filter trees.
//  4. Repeats until convergence: once leaf vars (IAMRole, User, Plugin,
//     Workspace) are merged, intermediate vars (IAMBinding) may also share
//     the same canonical-leaf fingerprint and merge in the next pass.

import (
	"strings"

	"github.com/hypermodeinc/dgraph/v25/dql"
)

// deduplicateAuthVarBlocks deduplicates semantically identical DQL var blocks
// in the auth query list.  It returns the pruned list and a var→canonical
// substitution map so the caller can update external filter trees (e.g. the
// top-level root-query filter) that reference the removed var names.
func deduplicateAuthVarBlocks(qrys []*dql.GraphQuery) ([]*dql.GraphQuery, map[string]string) {
	if len(qrys) <= 1 {
		return qrys, nil
	}

	subst := make(map[string]string)

	// Multi-pass: repeat until no new substitutions are found.  Each pass
	// may enable further deduplication in subsequent passes (leaf → intermediate).
	for authVarDedupPass(qrys, subst) {
	}

	if len(subst) == 0 {
		return qrys, nil
	}

	// Collect surviving var blocks; apply final substitutions to their filters
	// AND any nested child filters (auth traversal rules embed uid(VarName)
	// references inside Children — e.g. Contact_Auth2's traversal into
	// inGroup.inWorkspace @filter(uid(Workspace_Auth14)) — and these must also
	// be updated so we don't leave dangling var references after dedup.
	result := make([]*dql.GraphQuery, 0, len(qrys))
	for _, q := range qrys {
		if isDroppedAuthVarBlock(q, subst) {
			continue // duplicate — drop
		}
		applyAuthVarSubstToQueries([]*dql.GraphQuery{q}, subst)
		result = append(result, q)
	}
	return result, subst
}

// authVarDedupPass performs one deduplication sweep over qrys.
//
// Steps:
//  1. Apply the current subst to every filter tree (normalise var names).
//  2. Fingerprint each var block (excluding its own assigned var name).
//  3. On fingerprint collision, record a new substitution.
//
// Returns true if any new substitution was added (caller should repeat).
func authVarDedupPass(qrys []*dql.GraphQuery, subst map[string]string) bool {
	// Normalise all filter trees first so fingerprints use canonical names.
	// Must update both q.Filter AND nested child filters (auth traversal rules
	// embed uid(VarName) references inside Children, not only in Filter).
	for _, q := range qrys {
		if q != nil {
			applyAuthVarSubstToQueries([]*dql.GraphQuery{q}, subst)
		}
	}

	seen := make(map[string]string) // fingerprint → canonical var name
	changed := false
	for _, q := range qrys {
		varName := authVarBlockName(q)
		if varName == "" {
			continue
		}
		// Already remapped to a different canonical — will be dropped, skip.
		if canon, ok := subst[varName]; ok && canon != varName {
			continue
		}
		fp := fingerprintAuthVarBlock(q, subst)
		if fp == "" {
			continue
		}
		if canon, ok := seen[fp]; ok {
			// Duplicate: map varName → existing canonical.
			if existing, already := subst[varName]; !already || existing == varName {
				subst[varName] = canon
				changed = true
			}
		} else {
			seen[fp] = varName
			if _, ok := subst[varName]; !ok {
				subst[varName] = varName // identity sentinel
			}
		}
	}
	return changed
}

// authVarBlockName returns the DQL variable name defined by a var block.
//
// Two structural forms are handled:
//
//  1. Leaf var blocks (IAMRole, User, Plugin, Workspace): the var name lives
//     in the single child's Var field; q.Var is empty.
//     Example:
//     var(func: type(IAMRole)) @filter(...) {
//     JobAd_Auth2_forRole as IAMRole.hasBinding
//     }
//
//  2. Root auth blocks: the var name is q.Var; children select attributes
//     like "dgraph.type" without a Var assignment.
//     Example:
//     JobAd_Auth3 as var(func: uid(JobAd_1)) @filter(...) @cascade { dgraph.type }
func authVarBlockName(q *dql.GraphQuery) string {
	if q == nil || q.Attr != "var" {
		return ""
	}
	if q.Var != "" {
		return q.Var
	}
	if len(q.Children) == 1 && q.Children[0].Var != "" {
		return q.Children[0].Var
	}
	return ""
}

// isDroppedAuthVarBlock reports whether q is a var block whose defining var
// name was remapped to a different canonical (i.e. it is a duplicate to drop).
func isDroppedAuthVarBlock(q *dql.GraphQuery, subst map[string]string) bool {
	varName := authVarBlockName(q)
	if varName == "" {
		return false
	}
	canon, ok := subst[varName]
	return ok && canon != varName
}

// fingerprintAuthVarBlock returns a stable string fingerprint for a DQL var
// block based on its root function, filter tree, cascade markers, and child
// edge attributes.  The fingerprint deliberately excludes the assigned var
// name so that two blocks with identical semantics but different generated
// names produce the same fingerprint.
//
// Returns "" for blocks that cannot be fingerprinted (no Func, etc.).
func fingerprintAuthVarBlock(q *dql.GraphQuery, subst map[string]string) string {
	if q == nil || q.Attr != "var" || q.Func == nil {
		return ""
	}

	var b strings.Builder
	// Root function: e.g. type(IAMRole), uid(JobAd_1)
	b.WriteString(fingerprintAuthFunc(q.Func, subst))
	b.WriteByte('|')
	// Filter tree (with canonical var substitutions)
	b.WriteString(fingerprintAuthFilter(q.Filter, subst))
	b.WriteByte('|')
	// Cascade markers: @cascade or @cascade(fields:[...])
	b.WriteString(strings.Join(q.Cascade, ","))
	b.WriteByte('|')
	// Child subtrees — recursively fingerprint so that structurally different
	// children (different filters, different sub-children) produce different
	// fingerprints. We deliberately exclude child Var fields (those are the
	// generated names we are deduplicating).
	for _, c := range q.Children {
		b.WriteString(fingerprintSubtree(c, subst))
		b.WriteByte(';')
	}
	return b.String()
}

// fingerprintSubtree recursively fingerprints a query node's structure
// (Attr, Filter, Cascade, Children) but deliberately excludes Var so that
// differently-named but structurally identical subtrees compare equal.
func fingerprintSubtree(q *dql.GraphQuery, subst map[string]string) string {
	if q == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(q.Attr)
	b.WriteByte('|')
	b.WriteString(fingerprintAuthFilter(q.Filter, subst))
	b.WriteByte('|')
	b.WriteString(strings.Join(q.Cascade, ","))
	b.WriteByte('|')
	for _, c := range q.Children {
		b.WriteString(fingerprintSubtree(c, subst))
		b.WriteByte(';')
	}
	return b.String()
}

// fingerprintAuthFilter returns a canonical string for a FilterTree node,
// resolving uid() and uid_in() variable references through subst so that
// blocks referencing the same canonical vars produce the same fingerprint.
func fingerprintAuthFilter(f *dql.FilterTree, subst map[string]string) string {
	if f == nil {
		return ""
	}
	if f.Func != nil {
		return fingerprintAuthFunc(f.Func, subst)
	}
	parts := make([]string, len(f.Child))
	for i, c := range f.Child {
		parts[i] = fingerprintAuthFilter(c, subst)
	}
	return f.Op + "(" + strings.Join(parts, ",") + ")"
}

// fingerprintAuthFunc returns a canonical string for a DQL Function,
// resolving uid() and uid_in() variable arguments through subst.
func fingerprintAuthFunc(fn *dql.Function, subst map[string]string) string {
	if fn == nil {
		return ""
	}
	args := make([]string, len(fn.Args))
	for i, a := range fn.Args {
		val := a.Value
		switch fn.Name {
		case "uid":
			// Each arg is a direct DQL var name.
			if canon, ok := subst[val]; ok {
				val = canon
			}
		case "uid_in":
			// Second arg is the string literal "uid(VarName)".
			if i == 1 && strings.HasPrefix(val, "uid(") && strings.HasSuffix(val, ")") {
				inner := val[4 : len(val)-1]
				if canon, ok := subst[inner]; ok {
					val = "uid(" + canon + ")"
				}
			}
		}
		args[i] = val
	}
	return fn.Name + "(" + strings.Join(args, ",") + ")"
}

// applyAuthVarSubst recursively walks a FilterTree and substitutes all
// uid() and uid_in() variable references using subst.
// This is applied to surviving var blocks after deduplication and to external
// filter trees (e.g. the top-level root-query filter) so that all references
// use canonical var names.
func applyAuthVarSubst(f *dql.FilterTree, subst map[string]string) {
	if f == nil || len(subst) == 0 {
		return
	}
	if f.Func != nil {
		switch f.Func.Name {
		case "uid":
			for i, a := range f.Func.Args {
				if canon, ok := subst[a.Value]; ok {
					f.Func.Args[i].Value = canon
				}
			}
		case "uid_in":
			// Second arg is the string "uid(VarName)".
			if len(f.Func.Args) == 2 {
				val := f.Func.Args[1].Value
				if strings.HasPrefix(val, "uid(") && strings.HasSuffix(val, ")") {
					inner := val[4 : len(val)-1]
					if canon, ok := subst[inner]; ok {
						f.Func.Args[1].Value = "uid(" + canon + ")"
					}
				}
			}
		}
	}
	for _, c := range f.Child {
		applyAuthVarSubst(c, subst)
	}
}

// applyAuthVarSubstToQueries applies a deduplication substitution map to all
// filter trees and root functions within a slice of DQL var blocks. This must be
// called on selectionAuth blocks (generated by addSelectionSetFrom) after
// deduplicateAuthVarBlocks has processed fldAuthQueries, so that any
// deduplicated var names are also updated in the selectionAuth filter trees.
func applyAuthVarSubstToQueries(qrys []*dql.GraphQuery, subst map[string]string) {
	if len(subst) == 0 || len(qrys) == 0 {
		return
	}
	for _, q := range qrys {
		if q == nil {
			continue
		}
		if q.Func != nil && q.Func.Name == "uid" {
			for i, a := range q.Func.Args {
				if canon, ok := subst[a.Value]; ok {
					q.Func.Args[i].Value = canon
				}
			}
		}
		applyAuthVarSubst(q.Filter, subst)
		// Recurse into children — some selectionAuth blocks have nested children
		// that also carry filter trees referencing auth vars.
		applyAuthVarSubstToQueries(q.Children, subst)
	}
}
