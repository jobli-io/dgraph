/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
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
//
// Two further steps, each behind its own flag:
//
//   - Group grants (partition-group-grants). Where a cascade is not settled and
//     ends at a type whose records can hold a grant (a Group, a JobBoard), the
//     partition is asked once which of those records pass, and the cascade is
//     compiled against that list of UIDs instead of the walk that finds them.
//     The answer is the one the walk would give, so this changes no result.
//
//   - Tenant rules (partition-tenant-rules). A rule that only walks from the
//     record to the Workspace and tests the Workspace ("the record is in the
//     named workspace", "the caller holds a binding somewhere in the workspace")
//     says the same for every record that has the edge. It is asked once of the
//     Workspace and left out where it holds. Like the first step this treats a
//     non-null edge as present, so a record without it is no longer hidden.

// partitionAuthority is the type a partition holds exactly one of. A real
// implementation would take it from the schema.
const partitionAuthority = "Workspace"

// grantInterface is implemented by the types whose records can hold a grant. A
// real implementation would take it from the schema too.
const grantInterface = "IAMResource"

const maxCachedGrants = 50000

// maxHeld is the longest list of passing records written into a query. Past it
// the rule stays as written.
const maxHeld = 256

// answer is what the partition said about one rule for one caller.
type answer struct {
	// unknown: the rule is not of a shape this file can ask about.
	unknown bool
	// all: at least one record of the authority type exists and every one passes.
	all bool
	// uids are the records that pass, ascending. Nil with tooMany set.
	uids    []uint64
	tooMany bool
	until   time.Time
}

// ruleKey names one question: this caller, this rule, this partition. The rule
// is a node of the loaded schema; holding it in the key keeps it alive, so its
// address cannot come to mean another rule while the answer is remembered.
type ruleKey struct {
	ns     uint64
	caller string
	rule   *schema.RuleNode
}

var grantsConf struct {
	on          bool
	ttl         time.Duration
	groups      bool
	tenantRules bool

	mu     sync.RWMutex
	byText map[string]answer
	byRule map[ruleKey]answer

	// Counters for the proofs: how often the partition was asked, and how often
	// a remembered answer was used instead.
	asked, remembered atomic.Uint64
}

// EnablePartitionGrants turns the fast path on. An answer about a caller is
// believed for ttl, so a revoked grant keeps working for up to that long; with
// ttl zero the partition is asked on every request. groups and tenantRules turn
// on the two further steps described at the top of this file.
func EnablePartitionGrants(ttl time.Duration, groups, tenantRules bool) {
	grantsConf.ttl = ttl
	grantsConf.groups = groups
	grantsConf.tenantRules = tenantRules
	grantsConf.byText = make(map[string]answer)
	grantsConf.byRule = make(map[ruleKey]answer)
	grantsConf.on = true
}

// PartitionGrantsStats returns how many times the partition was asked and how
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

	answers map[*schema.RuleNode]answer
	// byText holds this request's answers by the text of the question: two rules
	// that compile to the same question are asked once.
	byText map[string]answer
	rules  map[simplifyKey]*schema.RuleNode
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
		ns:      ns,
		claims:  claims,
		caller:  string(caller),
		answers: make(map[*schema.RuleNode]answer),
		byText:  make(map[string]answer),
		rules:   make(map[simplifyKey]*schema.RuleNode),
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

// isRuleLeaf reports whether rn is a plain rule query on the record's own type.
func isRuleLeaf(rn *schema.RuleNode) bool {
	return rn.Rule != nil && rn.CascadeEdgePred == "" && rn.CascadeWrapPred == "" &&
		rn.CascadeBundlePred == "" && rn.CascadeThroughType == ""
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
		if grantsConf.groups {
			return g.narrow(typ, rn), false
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
		// An OR is wholly settled by any one of its arms. Without the further
		// steps its arms are otherwise left exactly as written.
		arms := make([]*schema.RuleNode, 0, len(rn.Or))
		changed := false
		for _, child := range rn.Or {
			c, holds := g.strip(typ, child, arr)
			if holds {
				return nil, true
			}
			if c != child {
				changed = true
			}
			arms = append(arms, c)
		}
		if !changed || !(grantsConf.groups || grantsConf.tenantRules) {
			return rn, false
		}
		cp := *rn
		cp.Or = arms
		return &cp, false
	case rn.RBACRule != nil:
		if rn.EvaluateStatic(g.claims) == schema.Positive {
			return nil, true
		}
		return rn, false
	case isRuleLeaf(rn):
		if g.tenantRuleHolds(typ, rn) {
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
	case isRuleLeaf(rn):
		return g.tenantRuleHolds(cur, rn)
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

// wrapTarget returns the type a cascade from cur leads to, or nil.
func wrapTarget(cur schema.Type, rn *schema.RuleNode) schema.Type {
	if cur == nil {
		return nil
	}
	fd := fieldByPredicate(cur, rn.CascadeWrapPred)
	if fd == nil {
		return nil
	}
	t := fd.Type()
	if t.Name() == rn.CascadeWrapType {
		return t
	}
	for _, impl := range t.ImplementingTypes() {
		if impl.Name() == rn.CascadeWrapType {
			return impl
		}
	}
	return nil
}

func holdsGrants(t schema.Type) bool {
	for _, i := range t.Interfaces() {
		if i == grantInterface {
			return true
		}
	}
	return false
}

// narrow returns the cascade rn with the walk to its authorities replaced by the
// authorities themselves, wherever the partition can say which records pass. A
// cascade to a type that holds grants gets the list. A cascade to any other type
// keeps its own walk and has the cascades inside it narrowed.
func (g *callerGrants) narrow(cur schema.Type, rn *schema.RuleNode) *schema.RuleNode {
	if rn.CascadeWrapInner == nil || rn.CascadeWrapHeldSet {
		return rn
	}
	target := wrapTarget(cur, rn)
	if target == nil {
		return rn
	}
	if holdsGrants(target) {
		a := g.answerFor(rn, func() (string, bool) { return g.wrapQuestion(cur, rn) })
		if !a.unknown && !a.tooMany {
			cp := *rn
			cp.CascadeWrapHeldSet = true
			cp.CascadeWrapHeld = a.uids
			return &cp
		}
	}
	inner, holds := g.strip(target, rn.CascadeWrapInner, nil)
	if holds {
		// Every record of the target passes. That did not settle the cascade,
		// because this record may have no such edge; what is left of it is
		// "has the edge, to a record of that type".
		cp := *rn
		cp.CascadeWrapHeldSet = true
		cp.CascadeWrapAll = true
		return &cp
	}
	if inner == rn.CascadeWrapInner {
		return rn
	}
	cp := *rn
	cp.CascadeWrapInner = inner
	return &cp
}

// rewriteHeldWrap compiles a cascade whose passing authorities are known. It
// keeps the direction the rewriter would have used: from the authorities down
// their inverse edge, or a uid_in on the record's own edge.
func (authRw *authRewriter) rewriteHeldWrap(typ schema.Type, rn *schema.RuleNode) ([]*dql.GraphQuery, *dql.FilterTree) {
	held := rn.CascadeWrapHeld
	if len(held) == 0 && !rn.CascadeWrapAll {
		return nil, &dql.FilterTree{Func: &dql.Function{Name: "uid", UID: []uint64{0}}}
	}
	authorities := &dql.Function{Name: "uid", UID: held}
	if rn.CascadeWrapAll {
		authorities = &dql.Function{Name: "type", Args: []dql.Arg{{Value: rn.CascadeWrapType}}}
	}
	if rn.CascadeWrapReverse && !authRw.forceForward {
		reverseVar := authRw.varGen.Next(typ, "", "", authRw.isWritingAuth)
		invPred := rn.CascadeInversePred
		if invPred == "" {
			invPred = "~" + rn.CascadeWrapPred
		}
		return []*dql.GraphQuery{{
				Attr:     "var",
				Func:     authorities,
				Children: []*dql.GraphQuery{{Attr: invPred, Var: reverseVar + "_uids"}},
			}}, &dql.FilterTree{
				Func: &dql.Function{Name: "uid", Args: []dql.Arg{{Value: reverseVar + "_uids"}}},
			}
	}
	if rn.CascadeWrapAll {
		all := authRw.varGen.Next(typ, "", "", authRw.isWritingAuth)
		return []*dql.GraphQuery{{Var: all, Attr: "var", Func: authorities}}, &dql.FilterTree{
			Func: &dql.Function{Name: "uid_in", Args: []dql.Arg{{Value: rn.CascadeWrapPred}, {Value: "uid(" + all + ")"}}},
		}
	}
	list := make([]string, len(held))
	for i, u := range held {
		list[i] = fmt.Sprintf("%#x", u)
	}
	lit := list[0]
	if len(list) > 1 {
		lit = "[" + strings.Join(list, ", ") + "]"
	}
	return nil, &dql.FilterTree{
		Func: &dql.Function{Name: "uid_in", Args: []dql.Arg{{Value: rn.CascadeWrapPred}, {Value: lit}}},
	}
}

// authorityHolds reports whether every Workspace in the partition passes the
// rule wrap carries for it. cur is the type that has the edge to the Workspace.
func (g *callerGrants) authorityHolds(cur schema.Type, wrap *schema.RuleNode) bool {
	a := g.answerFor(wrap, func() (string, bool) { return g.wrapQuestion(cur, wrap) })
	return !a.unknown && a.all
}

// tenantRuleHolds reports whether rule is one that only tests the Workspace the
// record is in, and the Workspace passes it. The answer is remembered by rule,
// so it relies on every type that carries the rule declaring the edge alike.
func (g *callerGrants) tenantRuleHolds(cur schema.Type, rule *schema.RuleNode) bool {
	if !grantsConf.tenantRules || cur == nil {
		return false
	}
	a := g.answerFor(rule, func() (string, bool) { return g.tenantQuestion(cur, rule) })
	return !a.unknown && a.all
}

// answerFor returns what the partition says about rule for this caller. It asks
// with the query build returns unless this request, or another within the ttl,
// already has. A rule build cannot turn into a question is remembered as unknown.
func (g *callerGrants) answerFor(rule *schema.RuleNode, build func() (string, bool)) answer {
	if a, ok := g.answers[rule]; ok {
		return a
	}
	key := ruleKey{ns: g.ns, caller: g.caller, rule: rule}
	now := time.Now()
	if grantsConf.ttl > 0 {
		grantsConf.mu.RLock()
		a, ok := grantsConf.byRule[key]
		grantsConf.mu.RUnlock()
		if ok && now.Before(a.until) {
			grantsConf.remembered.Add(1)
			g.answers[rule] = a
			return a
		}
	}
	a := g.ask(build)
	g.answers[rule] = a
	if grantsConf.ttl > 0 {
		a.until = now.Add(grantsConf.ttl)
		grantsConf.mu.Lock()
		if len(grantsConf.byRule) >= maxCachedGrants {
			grantsConf.byRule = make(map[ruleKey]answer)
		}
		grantsConf.byRule[key] = a
		grantsConf.mu.Unlock()
	}
	return a
}

func (g *callerGrants) freshRewriter() *authRewriter {
	return &authRewriter{
		authVariables:       g.claims,
		varGen:              NewVariableGenerator(),
		selector:            queryAuthSelector,
		isWritingAuth:       true,
		hasAuthRules:        true,
		forceForward:        true,
		cascadeVarCache:     make(map[cascadeCacheKey]string),
		allowedTypesForEdge: make(map[string][]string),
	}
}

var uidOnly = []*dql.GraphQuery{{Attr: "uid"}}

// wrapQuestion turns a cascade into a query for the authorities that pass it.
// The rule is compiled by the rewriter exactly as it would be inside a query.
func (g *callerGrants) wrapQuestion(cur schema.Type, wrap *schema.RuleNode) (string, bool) {
	blocks, filter := g.freshRewriter().rewriteRuleNode(cur, wrap)
	// The rewriter answers a cascade with uid_in(<edge>, uid(<var>)), where
	// <var> is the set of authorities that pass. Anything else is not understood.
	if filter == nil || filter.Func == nil || filter.Func.Name != "uid_in" ||
		len(filter.Func.Args) != 2 || len(filter.Child) != 0 {
		return "", false
	}
	passing := filter.Func.Args[1].Value
	if !strings.HasPrefix(passing, "uid(") || !strings.HasSuffix(passing, ")") {
		return "", false
	}
	passing = passing[len("uid(") : len(passing)-1]

	blocks = append(blocks,
		&dql.GraphQuery{
			Attr:     "all",
			Func:     &dql.Function{Name: "type", Args: []dql.Arg{{Value: wrap.CascadeWrapType}}},
			Children: uidOnly,
		},
		&dql.GraphQuery{
			Attr:     "held",
			Func:     &dql.Function{Name: "uid", Args: []dql.Arg{{Value: passing}}},
			Children: uidOnly,
		})
	// The caller's claims are compiled into the text, so the text names both the
	// question and who is asking. The type's name only labels variables.
	return strings.ReplaceAll(dgraph.AsString(blocks), cur.Name()+"_Auth", "A"), true
}

// tenantQuestion turns a rule into a query on the Workspace, if the rule is of
// the one shape that allows it: no condition on the record itself, and a single
// selection, the record's non-null edge to the Workspace. What the rule asks of
// the Workspace under that edge is then asked of the Workspace directly.
func (g *callerGrants) tenantQuestion(cur schema.Type, rule *schema.RuleNode) (string, bool) {
	blocks, filter := g.freshRewriter().rewriteRuleNode(cur, rule)
	if len(blocks) != 1 || filter == nil || filter.Func == nil || filter.Func.Name != "uid" {
		return "", false
	}
	root := blocks[0]
	if root.Filter != nil || len(root.Args) != 0 || len(root.Order) != 0 {
		return "", false
	}
	var edge *dql.GraphQuery
	for _, c := range root.Children {
		if c.Attr == "dgraph.type" && c.Filter == nil && len(c.Children) == 0 {
			continue
		}
		if edge != nil {
			return "", false
		}
		edge = c
	}
	if edge == nil || edge.Var != "" || len(edge.Args) != 0 || len(edge.Order) != 0 {
		return "", false
	}
	fd := fieldByPredicate(cur, edge.Attr)
	if fd == nil || fd.Type().Name() != partitionAuthority || fd.Type().Nullable() {
		return "", false
	}
	authority := &dql.Function{Name: "type", Args: []dql.Arg{{Value: partitionAuthority}}}
	return dgraph.AsString([]*dql.GraphQuery{
		{Attr: "all", Func: authority, Children: uidOnly},
		{
			Attr:     "held",
			Func:     authority,
			Filter:   edge.Filter,
			Cascade:  []string{"__all__"},
			Children: append([]*dql.GraphQuery{{Attr: "uid"}}, edge.Children...),
		},
	}), true
}

// ask puts one question to the partition, unless the same text was answered
// within the ttl. Any failure answers unknown, and the rule stays as written.
func (g *callerGrants) ask(build func() (string, bool)) answer {
	query, ok := build()
	if !ok {
		return answer{unknown: true}
	}
	if a, ok := g.byText[query]; ok {
		return a
	}
	key := fmt.Sprintf("%d\x00%s", g.ns, query)
	now := time.Now()
	if grantsConf.ttl > 0 {
		grantsConf.mu.RLock()
		a, ok := grantsConf.byText[key]
		grantsConf.mu.RUnlock()
		if ok && now.Before(a.until) {
			grantsConf.remembered.Add(1)
			g.byText[query] = a
			return a
		}
	}

	grantsConf.asked.Add(1)
	a, err := runAuthorityQuery(g.ns, query)
	if err != nil {
		glog.Warningf("partition grants: could not ask namespace %d, keeping the rule: %v", g.ns, err)
		return answer{unknown: true}
	}
	glog.V(2).Infof("partition grants: asked namespace %d, all pass %v, %d pass", g.ns, a.all, len(a.uids))
	g.byText[query] = a
	if grantsConf.ttl > 0 {
		a.until = now.Add(grantsConf.ttl)
		grantsConf.mu.Lock()
		if len(grantsConf.byText) >= maxCachedGrants {
			grantsConf.byText = make(map[string]answer)
		}
		grantsConf.byText[key] = a
		grantsConf.mu.Unlock()
	}
	return a
}

func runAuthorityQuery(ns uint64, query string) (answer, error) {
	// Read on the engine's own authority, in the partition the request runs in.
	qctx := x.AttachTrustedTenant(context.Background(), ns)
	resp, err := (&edgraph.Server{}).QueryNoAuth(qctx, &api.Request{Query: query, ReadOnly: true})
	if err != nil {
		return answer{}, err
	}
	var out struct {
		All  []struct{ UID string } `json:"all"`
		Held []struct{ UID string } `json:"held"`
	}
	if err := json.Unmarshal(resp.GetJson(), &out); err != nil {
		return answer{}, err
	}
	passing := make(map[string]bool, len(out.Held))
	uids := make([]uint64, 0, len(out.Held))
	for _, h := range out.Held {
		if passing[h.UID] {
			continue
		}
		passing[h.UID] = true
		u, err := strconv.ParseUint(h.UID, 0, 64)
		if err != nil {
			return answer{}, err
		}
		uids = append(uids, u)
	}
	a := answer{all: len(out.All) > 0}
	for _, r := range out.All {
		if !passing[r.UID] {
			a.all = false
			break
		}
	}
	if len(uids) > maxHeld {
		a.tooMany = true
		return a, nil
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	a.uids = uids
	return a, nil
}
