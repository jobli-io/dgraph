/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"fmt"

	"github.com/dgraph-io/dgraph/v25/dql"
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/x"
)

// Enforcement of @referenceScope.
//
// A mutation can name a record that already exists: { inGroup: [{ id: "0x1" }] }.
// Before the mutation is rewritten, the rewriter asks the store whether each named
// record exists and is of the field's type. Nothing else is asked of it. An @auth
// rule cannot ask more either: add rules look at the record being added and update
// rules at the record as it was, so neither sees who the named record belongs to.
//
// @referenceScope(claim: "ws", path: "inWorkspace.name") on a type or interface says
// that a record of it can be named only by a caller whose claim equals the value at
// the end of the path. For every such reference the rewriter adds one block beside
// the existence check, over the same record, that reads the value. When the answers
// come back the reference is tested here:
//
//   - A record named by ID that is out of scope is treated as if it were not there.
//     The caller gets the error a missing ID gets, and learns nothing more.
//   - A record named by an @id value that is out of scope fails the mutation. It
//     cannot be treated as missing, because a missing @id value means "create it".
//
// What is tested is a reference to a record that is already there: one the caller
// wrote, in an add or in the set patch of an update, and one a @default made, which
// an expression can build from what the caller sent (see rewriteObject). Not tested:
//
//   - the record a mutation is about; its @auth rules decide that
//   - the remove patch of an update, so that a record tied across the boundary can
//     be untied
//   - a record of a type that has no rule, alone or among the types an interface or
//     union field can hold
//
// @transform cannot name a record that was not named before it ran: no existence
// check is made for what it returns, so an ID it introduces is refused as unknown.
//
// A type takes its own rule and those of its interfaces, and a record is in scope if
// it meets any one of them. A path can pass through a list; one matching value is
// enough. A caller without the claim is in scope of nothing.

const refScopeSuffix = "__scope"

// refScopeCheck is what to test one named record against.
type refScopeCheck struct {
	// byXID: the record was named by an @id value, not by ID.
	byXID    bool
	typeName string
	scopes   []schema.ReferenceScope
	// unscoped are the Dgraph types a record can have that carry no rule.
	unscoped []string
}

// noteReferenceScope adds, for a record the mutation names, the block that reads
// what its type's rules need. existence is the block that checks the record is there.
func (xm *xidMetadata) noteReferenceScope(existence *dql.GraphQuery, typ schema.Type, byXID bool) {
	if xm == nil || existence == nil || typ == nil {
		return
	}
	scopes, unscoped := typ.ReferenceScopes()
	if len(scopes) == 0 {
		return
	}
	children := []*dql.GraphQuery{{Attr: "uid"}, {Attr: "dgraph.type"}}
	for i, s := range scopes {
		// s<i> : <edge> { v : <edge> { v : <value> } }
		leaf := &dql.GraphQuery{Alias: "v", Attr: s.Value}
		for j := len(s.Edges) - 1; j >= 0; j-- {
			leaf = &dql.GraphQuery{Alias: "v", Attr: s.Edges[j], Children: []*dql.GraphQuery{leaf}}
		}
		leaf.Alias = fmt.Sprintf("s%d", i)
		children = append(children, leaf)
	}
	name := existence.Attr + refScopeSuffix
	xm.pendingScope = append(xm.pendingScope, &dql.GraphQuery{
		Attr:     name,
		Func:     existence.Func,
		UID:      existence.UID,
		Children: children,
	})
	xm.pendingChecks = append(xm.pendingChecks,
		&refScopeCheck{byXID: byXID, typeName: typ.Name(), scopes: scopes, unscoped: unscoped})
}

// takeReferenceScopeQueries returns the blocks noted since the last call, and the
// type each result is filtered on, which is none. From here on their results are tested.
func (xm *xidMetadata) takeReferenceScopeQueries() ([]*dql.GraphQuery, []string) {
	if xm == nil || len(xm.pendingScope) == 0 {
		return nil, nil
	}
	queries := xm.pendingScope
	if xm.refScopes == nil {
		xm.refScopes = make(map[string]*refScopeCheck)
	}
	for i, q := range queries {
		xm.refScopes[q.Attr] = xm.pendingChecks[i]
	}
	xm.pendingScope, xm.pendingChecks = nil, nil
	return queries, make([]string, len(queries))
}

// takeReferenceScopeChecks returns the blocks noted since the last call with the test
// for each, for a caller that runs and tests them itself.
func (xm *xidMetadata) takeReferenceScopeChecks() ([]*dql.GraphQuery, map[string]*refScopeCheck) {
	if xm == nil || len(xm.pendingScope) == 0 {
		return nil, nil
	}
	queries := xm.pendingScope
	checks := make(map[string]*refScopeCheck, len(queries))
	for i, q := range queries {
		checks[q.Attr] = xm.pendingChecks[i]
	}
	xm.pendingScope, xm.pendingChecks = nil, nil
	return queries, checks
}

// dropReferenceScopeQueries forgets the blocks noted since the last call.
func (xm *xidMetadata) dropReferenceScopeQueries() {
	if xm != nil {
		xm.pendingScope, xm.pendingChecks = nil, nil
	}
}

func (rw *Rewriter) referenceScopeChecks() map[string]*refScopeCheck {
	if rw == nil || rw.XidMetadata == nil {
		return nil
	}
	return rw.XidMetadata.refScopes
}

// referenceScoped is a mutation rewriter that may have asked for scope blocks.
type referenceScoped interface {
	referenceScopeChecks() map[string]*refScopeCheck
}

// scopeValues returns the values at the end of a scope's path in a result row.
func scopeValues(v interface{}) []string {
	switch val := v.(type) {
	case nil:
		return nil
	case map[string]interface{}:
		return scopeValues(val["v"])
	case []interface{}:
		var out []string
		for _, item := range val {
			out = append(out, scopeValues(item)...)
		}
		return out
	case string:
		return []string{val}
	default:
		return []string{fmt.Sprint(val)}
	}
}

// allows reports whether the record in row can be named by a caller with claims.
func (c *refScopeCheck) allows(row idExistenceRes, claims map[string]interface{}) bool {
	for _, t := range c.unscoped {
		if x.HasString(row.Types, t) {
			return true
		}
	}
	for i, s := range c.scopes {
		want, ok := claims[s.Claim].(string)
		if !ok || want == "" {
			continue
		}
		for _, got := range scopeValues(row.OldValues[fmt.Sprintf("s%d", i)]) {
			if got == want {
				return true
			}
		}
	}
	return false
}

// referencesOutOfScope tests the records a mutation names against their types'
// @referenceScope rules. It returns the names of the existence blocks whose record,
// named by ID, is to be treated as not there, and an error if a record named by an
// @id value is out of scope.
func referencesOutOfScope(rewriter interface{}, results map[string][]idExistenceRes,
	claims func() (map[string]interface{}, error)) (map[string]bool, error) {

	rs, ok := rewriter.(referenceScoped)
	if !ok {
		return nil, nil
	}
	checks := rs.referenceScopeChecks()
	if len(checks) == 0 {
		return nil, nil
	}
	held, err := claims()
	if err != nil {
		return nil, err
	}
	return outOfScope(checks, results, held)
}

// outOfScope tests the rows of each scope block against the claims a caller holds.
func outOfScope(checks map[string]*refScopeCheck, results map[string][]idExistenceRes,
	held map[string]interface{}) (map[string]bool, error) {

	if len(checks) == 0 {
		return nil, nil
	}
	hidden := make(map[string]bool)
	for name, check := range checks {
		for _, row := range results[name] {
			if row.Uid == "" || check.allows(row, held) {
				continue
			}
			if check.byXID {
				// Said without the value: the caller may not know the record exists.
				return nil, x.GqlErrorf("a %s this mutation names cannot be used by the caller",
					check.typeName)
			}
			hidden[name[:len(name)-len(refScopeSuffix)]] = true
		}
	}
	return hidden, nil
}
