/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/dgraph-io/dgraph/v25/dql"
	"github.com/dgraph-io/dgraph/v25/edgraph"
	"github.com/dgraph-io/dgraph/v25/graphql/dgraph"
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/x"
)

// Workspace-level grants for partition namespaces (SPIKE).
//
// A cascade rule says "this record is visible if the record it hangs off is":
// a Candidate through its Group, a Group through its Workspace. The rewriter
// compiles that into a walk from the workspace down to the records, for every
// type a query selects.
//
// In a partition there is one Workspace. So the question at the top of every
// such chain, "does this caller pass the Workspace's rule", has one answer for
// the whole request. This file asks it once, by running the rule the schema
// declares against the Workspace node, and remembers the answer for a short
// time. Where the answer is yes, the chain below it holds for every record that
// has the edges the chain walks, and the rewriter leaves the chain out.
//
// Nothing here knows Jobli's access model. A rule is left out only when all of
// this is true:
//
//   - it is a cascade to the Workspace, or to a type whose own cascade reaches
//     the Workspace, and every rule on the way that is AND-ed with it is a claim
//     check that passes
//   - the engine's own compilation of the Workspace rule returns every Workspace
//     in the partition
//   - the record is known to have the edge: the schema makes the field
//     non-null, or the query reached the record through that edge's inverse
//   - it sits where being true removes it: alone, in an AND, or in an OR
//
// Everything else is kept as written: rules about the record (not deleted, in
// the named workspace), rules the caller does not pass, and anything this file
// does not recognise. Only queries take this path; mutations do not.
//
// It is only sound inside a partition. In a shared namespace "every Workspace
// passes" would have to hold for every tenant's workspace at once.

// partitionAuthority is the type a partition holds exactly one of. A real
// implementation would take it from the schema.
const partitionAuthority = "Workspace"

const maxCachedGrants = 50000

type heldEntry struct {
	held  bool
	until time.Time
}

// ruleKey names one question: this caller, this rule, this partition. The rule
// is a node of the loaded schema; holding it in the key keeps it alive, so its
// address cannot come to mean another rule while the answer is remembered.
type ruleKey struct {
	ns     uint64
	caller string
	wrap   *schema.RuleNode
}

var grantsConf struct {
	on  bool
	ttl time.Duration

	mu     sync.RWMutex
	cache  map[string]heldEntry
	byRule map[ruleKey]heldEntry

	// Counters for the proofs: how often the Workspace was asked, and how often
	// a remembered answer was used instead.
	asked, remembered atomic.Uint64
}

// EnablePartitionGrants turns the fast path on. An answer about a caller is
// believed for ttl, so a revoked grant keeps working for up to that long; with
// ttl zero the Workspace is asked on every request.
func EnablePartitionGrants(ttl time.Duration) {
	grantsConf.ttl = ttl
	grantsConf.cache = make(map[string]heldEntry)
	grantsConf.byRule = make(map[ruleKey]heldEntry)
	grantsConf.on = true
}

// PartitionGrantsStats returns how many times the Workspace was asked and how
// many times a remembered answer was used.
func PartitionGrantsStats() (asked, remembered uint64) {
	return grantsConf.asked.Load(), grantsConf.remembered.Load()
}

// arrival says the record being authorised was reached from a record of type
// from, so it has the edge pred pointing back at one.
type arrival struct {
	pred string
	from schema.Type
}

type simplifyKey struct {
	rn   *schema.RuleNode
	pred string
	from string
}

// callerGrants is what one request knows about its caller.
type callerGrants struct {
	ns     uint64
	claims map[string]interface{}
	// caller is every claim of the token, so two tokens share an answer only if
	// no rule could tell them apart.
	caller string

	wraps map[*schema.RuleNode]bool
	rules map[simplifyKey]*schema.RuleNode
}

// newCallerGrants returns nil when the fast path does not apply: it is off, or
// the request is in the root namespace.
func newCallerGrants(ctx context.Context, claims map[string]interface{}) *callerGrants {
	if !grantsConf.on || !x.PartitionNamespacesEnabled() {
		return nil
	}
	ns, err := x.ExtractNamespace(ctx)
	if err != nil || ns == x.RootNamespace {
		return nil
	}
	caller, err := json.Marshal(claims)
	if err != nil {
		return nil
	}
	return &callerGrants{
		ns:     ns,
		claims: claims,
		caller: string(caller),
		wraps:  make(map[*schema.RuleNode]bool),
		rules:  make(map[simplifyKey]*schema.RuleNode),
	}
}

// rules returns the rules that apply to typ for this caller: the selector's,
// less the cascades the caller's workspace-level grants already settle.
func (authRw *authRewriter) rules(typ schema.Type) *schema.RuleNode {
	rn := authRw.selector(typ)
	if rn == nil || typ == nil || authRw.grants == nil {
		return rn
	}
	return authRw.grants.simplify(typ, rn, authRw.arrival)
}

// arrivingBy describes how the records of field f are reached from a record of
// type parent, or returns nil if the edge has no declared inverse.
func arrivingBy(parent schema.Type, f schema.Field) *arrival {
	if parent == nil || parent.IsInterface() || parent.IsUnion() {
		return nil
	}
	def := parent.Field(f.Name())
	if def == nil {
		return nil
	}
	inv := def.Inverse()
	if inv == nil {
		return nil
	}
	return &arrival{pred: inv.DgraphPredicate(), from: parent}
}

func (g *callerGrants) simplify(typ schema.Type, rn *schema.RuleNode, arr *arrival) *schema.RuleNode {
	key := simplifyKey{rn: rn}
	if arr != nil {
		key.pred, key.from = arr.pred, arr.from.Name()
	}
	if out, ok := g.rules[key]; ok {
		return out
	}
	out, _ := g.strip(typ, rn, arr)
	g.rules[key] = out
	return out
}

// strip returns rn without the parts the caller's grants settle. The second
// result is true when nothing is left, that is when rn as a whole holds.
func (g *callerGrants) strip(typ schema.Type, rn *schema.RuleNode, arr *arrival) (*schema.RuleNode, bool) {
	switch {
	case rn == nil:
		return nil, false
	case rn.CascadeWrapPred != "":
		if g.settled(typ, rn, arr) {
			return nil, true
		}
		return rn, false
	case len(rn.And) > 0:
		if rn.CascadeBundlePred != "" {
			return rn, false
		}
		kept := make([]*schema.RuleNode, 0, len(rn.And))
		changed := false
		for _, child := range rn.And {
			c, holds := g.strip(typ, child, arr)
			if holds {
				changed = true
				continue
			}
			if c != child {
				changed = true
			}
			kept = append(kept, c)
		}
		if len(kept) == 0 {
			return nil, true
		}
		if !changed {
			return rn, false
		}
		cp := *rn
		cp.And = kept
		return &cp, false
	case len(rn.Or) > 0:
		// An OR is either wholly settled by one of its arms or left exactly as
		// written; its arms are never rewritten.
		for _, child := range rn.Or {
			if _, holds := g.strip(typ, child, arr); holds {
				return nil, true
			}
		}
		return rn, false
	case rn.RBACRule != nil:
		if rn.EvaluateStatic(g.claims) == schema.Positive {
			return nil, true
		}
		return rn, false
	default:
		return rn, false
	}
}

// settled reports whether rn holds for every record of cur that has the edges
// rn walks. Unlike strip it never gives the benefit of the doubt: a rule it
// cannot decide is not settled.
func (g *callerGrants) settled(cur schema.Type, rn *schema.RuleNode, arr *arrival) bool {
	switch {
	case rn == nil || cur == nil:
		return false
	case rn.CascadeWrapPred != "":
		if rn.CascadeWrapInner == nil {
			return false
		}
		// The record must be known to have the edge, and to a record of the
		// type the rule names.
		var next schema.Type
		if arr != nil && arr.pred == rn.CascadeWrapPred && arr.from.Name() == rn.CascadeWrapType {
			next = arr.from
		} else if fd := fieldByPredicate(cur, rn.CascadeWrapPred); fd != nil &&
			fd.Type().Name() == rn.CascadeWrapType && !fd.Type().Nullable() {
			next = fd.Type()
		}
		if next == nil {
			return false
		}
		if rn.CascadeWrapType == partitionAuthority {
			return g.authorityHolds(cur, rn)
		}
		return g.settled(next, rn.CascadeWrapInner, nil)
	case len(rn.And) > 0:
		if rn.CascadeBundlePred != "" {
			return false
		}
		for _, child := range rn.And {
			if !g.settled(cur, child, arr) {
				return false
			}
		}
		return true
	case len(rn.Or) > 0:
		for _, child := range rn.Or {
			if g.settled(cur, child, arr) {
				return true
			}
		}
		return false
	case rn.RBACRule != nil:
		return rn.EvaluateStatic(g.claims) == schema.Positive
	default:
		return false
	}
}

func fieldByPredicate(typ schema.Type, pred string) schema.FieldDefinition {
	for _, fd := range typ.Fields() {
		if fd.DgraphPredicate() == pred {
			return fd
		}
	}
	return nil
}

// authorityHolds reports whether every Workspace in the partition passes the
// rule wrap carries for it. cur is the type that has the edge to the Workspace.
//
// The rule is compiled by the rewriter exactly as it would be inside a query,
// then run on its own. Any failure answers no, and the rule stays as written.
func (g *callerGrants) authorityHolds(cur schema.Type, wrap *schema.RuleNode) bool {
	if held, ok := g.wraps[wrap]; ok {
		return held
	}
	key := ruleKey{ns: g.ns, caller: g.caller, wrap: wrap}
	now := time.Now()
	if grantsConf.ttl > 0 {
		grantsConf.mu.RLock()
		e, ok := grantsConf.byRule[key]
		grantsConf.mu.RUnlock()
		if ok && now.Before(e.until) {
			grantsConf.remembered.Add(1)
			g.wraps[wrap] = e.held
			return e.held
		}
	}
	held := g.askAuthority(cur, wrap)
	g.wraps[wrap] = held
	if grantsConf.ttl > 0 {
		grantsConf.mu.Lock()
		if len(grantsConf.byRule) >= maxCachedGrants {
			grantsConf.byRule = make(map[ruleKey]heldEntry)
		}
		grantsConf.byRule[key] = heldEntry{held: held, until: now.Add(grantsConf.ttl)}
		grantsConf.mu.Unlock()
	}
	return held
}

func (g *callerGrants) askAuthority(cur schema.Type, wrap *schema.RuleNode) bool {
	rw := &authRewriter{
		authVariables:       g.claims,
		varGen:              NewVariableGenerator(),
		selector:            queryAuthSelector,
		isWritingAuth:       true,
		hasAuthRules:        true,
		forceForward:        true,
		cascadeVarCache:     make(map[cascadeCacheKey]string),
		allowedTypesForEdge: make(map[string][]string),
	}
	blocks, filter := rw.rewriteRuleNode(cur, wrap)
	// The rewriter answers a cascade with uid_in(<edge>, uid(<var>)), where
	// <var> is the set of authorities that pass. Anything else is not understood.
	if filter == nil || filter.Func == nil || filter.Func.Name != "uid_in" ||
		len(filter.Func.Args) != 2 || len(filter.Child) != 0 {
		return false
	}
	passing := filter.Func.Args[1].Value
	if !strings.HasPrefix(passing, "uid(") || !strings.HasSuffix(passing, ")") {
		return false
	}
	passing = passing[len("uid(") : len(passing)-1]

	uid := []*dql.GraphQuery{{Attr: "uid"}}
	blocks = append(blocks,
		&dql.GraphQuery{
			Attr:     "all",
			Func:     &dql.Function{Name: "type", Args: []dql.Arg{{Value: partitionAuthority}}},
			Children: uid,
		},
		&dql.GraphQuery{
			Attr:     "held",
			Func:     &dql.Function{Name: "uid", Args: []dql.Arg{{Value: passing}}},
			Children: uid,
		})
	// The caller's claims are compiled into the text, so the text names both the
	// question and who is asking. The type's name only labels variables.
	query := strings.ReplaceAll(dgraph.AsString(blocks), cur.Name()+"_Auth", "A")

	key := fmt.Sprintf("%d\x00%s", g.ns, query)
	now := time.Now()
	if grantsConf.ttl > 0 {
		grantsConf.mu.RLock()
		e, ok := grantsConf.cache[key]
		grantsConf.mu.RUnlock()
		if ok && now.Before(e.until) {
			grantsConf.remembered.Add(1)
			return e.held
		}
	}

	grantsConf.asked.Add(1)
	held, err := runAuthorityQuery(g.ns, query)
	if err != nil {
		glog.Warningf("partition grants: could not ask the %s in namespace %d, keeping the rule: %v",
			partitionAuthority, g.ns, err)
		return false
	}
	glog.V(2).Infof("partition grants: asked the %s in namespace %d about a rule on %s, answer %v",
		partitionAuthority, g.ns, cur.Name(), held)
	if grantsConf.ttl > 0 {
		grantsConf.mu.Lock()
		if len(grantsConf.cache) >= maxCachedGrants {
			grantsConf.cache = make(map[string]heldEntry)
		}
		grantsConf.cache[key] = heldEntry{held: held, until: now.Add(grantsConf.ttl)}
		grantsConf.mu.Unlock()
	}
	return held
}

func runAuthorityQuery(ns uint64, query string) (bool, error) {
	// Read on the engine's own authority, in the partition the request runs in.
	qctx := x.AttachTrustedTenant(context.Background(), ns)
	resp, err := (&edgraph.Server{}).QueryNoAuth(qctx, &api.Request{Query: query, ReadOnly: true})
	if err != nil {
		return false, err
	}
	var out struct {
		All  []struct{ UID string } `json:"all"`
		Held []struct{ UID string } `json:"held"`
	}
	if err := json.Unmarshal(resp.GetJson(), &out); err != nil {
		return false, err
	}
	if len(out.All) == 0 {
		return false, nil
	}
	passing := make(map[string]bool, len(out.Held))
	for _, h := range out.Held {
		passing[h.UID] = true
	}
	for _, a := range out.All {
		if !passing[a.UID] {
			return false, nil
		}
	}
	return true, nil
}
