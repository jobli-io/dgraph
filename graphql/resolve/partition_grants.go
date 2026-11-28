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
//     Where every record of that type is already known to pass, there is no
//     list to ask for and the cascade is compiled as "has the edge, to a
//     record of that type". Like the first step, that takes a non-null edge on
//     the authority as present.
//
//   - Tenant rules (partition-tenant-rules). A rule that only walks from the
//     record to the Workspace and tests the Workspace ("the record is in the
//     named workspace", "the caller holds a binding somewhere in the workspace")
//     says the same for every record that has the edge. It is asked once of the
//     Workspace and left out where it holds. Like the first step this treats a
//     non-null edge as present, so a record without it is no longer hidden.

// The schema says which type a partition holds exactly one record of (@tenant) and
// which types' records can hold a grant (@grants). Nothing here names either. A
// schema that marks no tenant gets no fast path: every rule runs as written.

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
	// pending: not asked yet. The question was noted for the next read of
	// several together (partition_grants_lookup.go); unknown is set with it.
	pending bool
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
	// together: ask every question a query can raise in one read, before the
	// query is rewritten (partition_grants_lookup.go).
	together bool
	// mutations: the rules a mutation asks are simplified as a query's are.
	mutations bool

	mu     sync.RWMutex
	byText map[string]answer
	byRule map[ruleKey]answer
	// asking holds one lock per caller, so that requests that find the same
	// answers expired wait for one read instead of each making it.
	asking map[string]*sync.Mutex

	// Counters for the proofs: how often the partition was asked, and how often
	// a remembered answer was used instead.
	asked, remembered atomic.Uint64
	// lookups counts the reads that asked several questions together, questions
	// what they asked, and failed the ones that did not come back.
	lookups, questions, failed atomic.Uint64
}

// EnablePartitionGrants turns the fast path on. An answer about a caller is
// believed for ttl, so a revoked grant keeps working for up to that long; with
// ttl zero the partition is asked on every request. groups and tenantRules turn
// on the two further steps described at the top of this file. together asks a
// query's questions in one read.
func EnablePartitionGrants(ttl time.Duration, groups, tenantRules, together bool) {
	grantsConf.ttl = ttl
	grantsConf.groups = groups
	grantsConf.tenantRules = tenantRules
	grantsConf.together = together
	grantsConf.asking = make(map[string]*sync.Mutex)
	grantsConf.byText = make(map[string]answer)
	grantsConf.byRule = make(map[ruleKey]answer)
	grantsConf.on = true
}

// EnablePartitionGrantsForMutations lets the rules a mutation asks take the fast
// path too. Without it only queries do.
func EnablePartitionGrantsForMutations() {
	grantsConf.mutations = true
}

// mutationGrants returns what a mutation knows about its caller, for the rules of
// typ that selector picks, or nil when mutations are left on the rules as written.
// The questions those rules raise are put to the partition first, together.
//
// lists says whether a cascade may be compiled against the remembered list of the
// records that pass. It may not where the mutation can have made such a record
// itself: the list was read before the mutation ran and does not have it.
func mutationGrants(ctx context.Context, claims map[string]interface{}, typ schema.Type,
	selector func(schema.Type) *schema.RuleNode, lists bool) *callerGrants {

	if !grantsConf.mutations {
		return nil
	}
	g := newCallerGrants(ctx, claims)
	if g == nil {
		return nil
	}
	g.noLists = !lists
	g.askRulesTogether(typ, selector)
	return g
}

// mutationResultGrants is mutationGrants for the query that reads a mutation's
// result back, which is rewritten as any query is.
func mutationResultGrants(ctx context.Context, claims map[string]interface{}, f schema.Field) *callerGrants {
	if !grantsConf.mutations {
		return nil
	}
	g := newCallerGrants(ctx, claims)
	g.askTogether(f, queryAuthSelector)
	return g
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

	// noting is set while the questions a query raises are being gathered: a
	// question with no answer yet is added to noted instead of being asked.
	noting  bool
	noted   []question
	isNoted map[*schema.RuleNode]bool
	// waiting counts the times, while noting, that an answer was wanted that
	// could settle a rule and was not there. A cascade that sees it rise does
	// not go on to ask which records pass, which it would only want if that
	// answer were no. Outside noting it never changes.
	waiting int
	// noLists: a cascade is not compiled against a list of the records that pass.
	noLists bool
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
		before := g.waiting
		if g.settled(typ, rn, arr) {
			return nil, true
		}
		if g.waiting != before {
			// Which records pass is only asked once it is known that not
			// all of them do.
			return rn, false
		}
		if grantsConf.groups && !g.noLists {
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
		if tenant := cur.Tenant(); tenant != "" && rn.CascadeWrapType == tenant {
			return g.authorityHolds(cur, rn)
		}
		return g.settled(next, rn.CascadeWrapInner, nil)
	case len(rn.And) > 0:
		if rn.CascadeBundlePred != "" {
			return false
		}
		awaited := false
		for _, child := range rn.And {
			before := g.waiting
			if !g.settled(cur, child, arr) {
				if g.waiting == before {
					return false
				}
				// Not known yet. The rest is looked at all the same, so that
				// what it would take to settle this is asked in one round.
				awaited = true
			}
		}
		return !awaited
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
	return t != nil && t.HoldsGrants()
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
	// If every record of the target passes, there is no list to ask for. That
	// did not settle the cascade, because this record may have no such edge;
	// what is left of it is "has the edge, to a record of that type". A type
	// that holds grants can have a great many records (Candidate does), and
	// asking which of them pass means running its whole rule over all of them.
	before := g.waiting
	if g.settled(target, rn.CascadeWrapInner, nil) {
		cp := *rn
		cp.CascadeWrapHeldSet = true
		cp.CascadeWrapAll = true
		return &cp
	}
	if g.waiting != before {
		return rn
	}
	if holdsGrants(target) {
		a := g.answerFor(cur, rn, false)
		if a.pending {
			// Its answer will say which records pass, and then nothing below
			// this point is looked at. So nothing below it is asked about yet.
			return rn
		}
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
		block := &dql.GraphQuery{
			Attr:     "var",
			Func:     authorities,
			Children: []*dql.GraphQuery{{Attr: invPred, Var: reverseVar + "_uids"}},
		}
		authRw.varGen.settledRule(rn.CascadeWrapPred, invPred, block)
		return []*dql.GraphQuery{block}, &dql.FilterTree{
			Func: &dql.Function{Name: "uid", Args: []dql.Arg{{Value: reverseVar + "_uids"}}},
		}
	}
	if rn.CascadeWrapAll {
		all := authRw.varGen.Next(typ, "", "", authRw.isWritingAuth)
		block := &dql.GraphQuery{Var: all, Attr: "var", Func: authorities}
		authRw.varGen.settledRule(rn.CascadeWrapPred, "", block)
		return []*dql.GraphQuery{block}, &dql.FilterTree{
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
	authRw.varGen.settledRule(rn.CascadeWrapPred, "")
	return nil, &dql.FilterTree{
		Func: &dql.Function{Name: "uid_in", Args: []dql.Arg{{Value: rn.CascadeWrapPred}, {Value: lit}}},
	}
}

// authorityHolds reports whether every Workspace in the partition passes the
// rule wrap carries for it. cur is the type that has the edge to the Workspace.
func (g *callerGrants) authorityHolds(cur schema.Type, wrap *schema.RuleNode) bool {
	a := g.answerFor(cur, wrap, false)
	if a.pending {
		g.waiting++
	}
	return !a.unknown && a.all
}

// tenantRuleHolds reports whether rule is one the schema declares to test only the
// tenant the record is in (@access), and the tenant passes it. The answer is remembered by rule,
// so it relies on every type that carries the rule declaring the edge alike.
func (g *callerGrants) tenantRuleHolds(cur schema.Type, rule *schema.RuleNode) bool {
	if !grantsConf.tenantRules || cur == nil {
		return false
	}
	if !cur.TenantLevel(rule) {
		// Only a rule the schema declares with @access is asked of the tenant. One
		// that merely has the shape is left as written, and said so once.
		g.noteUndeclared(cur, rule)
		return false
	}
	a := g.answerFor(cur, rule, true)
	if a.pending {
		g.waiting++
	}
	return !a.unknown && a.all
}

// undeclared remembers the rules already looked at by noteUndeclared.
var undeclared sync.Map

// noteUndeclared logs, once for each rule text, a rule that tests only the tenant but
// is not written under @access. It runs as written; the log says what to declare.
func (g *callerGrants) noteUndeclared(cur schema.Type, rule *schema.RuleNode) {
	if rule == nil || rule.RuleTemplate == "" || !glog.V(2) {
		return
	}
	if _, seen := undeclared.LoadOrStore(rule.RuleTemplate, true); seen {
		return
	}
	if _, ok := g.compileTenant(cur, rule); ok {
		glog.Infof("partition grants: a rule on type %s tests only the tenant but is not declared "+
			"with @access; it runs as written: %.160q", cur.Name(), strings.Join(strings.Fields(rule.RuleTemplate), " "))
	}
}

// answerFor returns what the partition says about rule, which sits in the rules
// of cur, for this caller. It asks unless this request, or another within the
// ttl, already has. tenant says which of the two kinds of question rule is. A
// rule that cannot be turned into a question is remembered as unknown.
func (g *callerGrants) answerFor(cur schema.Type, rule *schema.RuleNode, tenant bool) answer {
	if a, ok := g.answers[rule]; ok {
		return a
	}
	build := func() (string, bool) {
		if tenant {
			return g.tenantQuestion(cur, rule)
		}
		return g.wrapQuestion(cur, rule)
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
	if g.noting {
		if !g.isNoted[rule] {
			g.isNoted[rule] = true
			g.noted = append(g.noted, question{cur: cur, rule: rule, tenant: tenant})
		}
		return answer{unknown: true, pending: true}
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

// compiledQuestion is one question as DQL: the blocks that find the records that
// pass, the type whose records are asked about, and the blocks that return the
// answer.
//
// The answer is three things: how many records the type has, how many of them
// pass, and the first maxHeld+1 that pass. A type that holds grants can have a
// great many records (Candidate does), so neither set is returned whole.
type compiledQuestion struct {
	vars []*dql.GraphQuery
	of   string
	// held returns the records that pass. passed, if set, counts the ones of
	// the type among them; without it they are counted from held.
	held, passed *dql.GraphQuery
}

var countOnly = []*dql.GraphQuery{{Attr: "count(uid)"}}

func allOf(typ string) *dql.GraphQuery {
	return &dql.GraphQuery{
		Attr:     "all",
		Func:     &dql.Function{Name: "type", Args: []dql.Arg{{Value: typ}}},
		Children: countOnly,
	}
}

// results returns the blocks that return the answer to c.
func (c compiledQuestion) results() []*dql.GraphQuery {
	out := []*dql.GraphQuery{allOf(c.of), c.held}
	if c.passed != nil {
		out = append(out, c.passed)
	}
	return out
}

// compileWrap turns a cascade into a query for the authorities that pass it.
// The rule is compiled by the rewriter exactly as it would be inside a query.
func (g *callerGrants) compileWrap(cur schema.Type, wrap *schema.RuleNode) (compiledQuestion, bool) {
	blocks, filter := g.freshRewriter().rewriteRuleNode(cur, wrap)
	// The rewriter answers a cascade with uid_in(<edge>, uid(<var>)), where
	// <var> is the set of authorities that pass. Anything else is not understood.
	if filter == nil || filter.Func == nil || filter.Func.Name != "uid_in" ||
		len(filter.Func.Args) != 2 || len(filter.Child) != 0 {
		return compiledQuestion{}, false
	}
	passing := filter.Func.Args[1].Value
	if !strings.HasPrefix(passing, "uid(") || !strings.HasSuffix(passing, ")") {
		return compiledQuestion{}, false
	}
	passing = passing[len("uid(") : len(passing)-1]
	return compiledQuestion{
		vars: blocks,
		of:   wrap.CascadeWrapType,
		held: &dql.GraphQuery{
			Attr:     "held",
			Func:     &dql.Function{Name: "uid", Args: []dql.Arg{{Value: passing}}},
			Args:     map[string]string{"first": strconv.Itoa(maxHeld + 1)},
			Children: []*dql.GraphQuery{{Attr: "uid"}},
		},
		passed: &dql.GraphQuery{
			Attr: "passed",
			Func: &dql.Function{Name: "uid", Args: []dql.Arg{{Value: passing}}},
			Filter: &dql.FilterTree{
				Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: wrap.CascadeWrapType}}}},
			Children: countOnly,
		},
	}, true
}

func (g *callerGrants) wrapQuestion(cur schema.Type, wrap *schema.RuleNode) (string, bool) {
	c, ok := g.compileWrap(cur, wrap)
	if !ok {
		return "", false
	}
	// The caller's claims are compiled into the text, so the text names both the
	// question and who is asking. The type's name only labels variables.
	blocks := append(append([]*dql.GraphQuery{}, c.vars...), c.results()...)
	return strings.ReplaceAll(dgraph.AsString(blocks), cur.Name()+"_Auth", "A"), true
}

// compileTenant turns a rule into a query on the Workspace, if the rule is of
// the one shape that allows it: no condition on the record itself, and a single
// selection, the record's non-null edge to the Workspace. What the rule asks of
// the Workspace under that edge is then asked of the Workspace directly.
func (g *callerGrants) compileTenant(cur schema.Type, rule *schema.RuleNode) (compiledQuestion, bool) {
	none := compiledQuestion{}
	blocks, filter := g.freshRewriter().rewriteRuleNode(cur, rule)
	if len(blocks) != 1 || filter == nil || filter.Func == nil || filter.Func.Name != "uid" {
		return none, false
	}
	root := blocks[0]
	if root.Filter != nil || len(root.Args) != 0 || len(root.Order) != 0 {
		return none, false
	}
	var edge *dql.GraphQuery
	for _, c := range root.Children {
		if c.Attr == "dgraph.type" && c.Filter == nil && len(c.Children) == 0 {
			continue
		}
		if edge != nil {
			return none, false
		}
		edge = c
	}
	if edge == nil || edge.Var != "" || len(edge.Args) != 0 || len(edge.Order) != 0 {
		return none, false
	}
	fd := fieldByPredicate(cur, edge.Attr)
	if fd == nil || !fd.Type().IsTenant() || fd.Type().Nullable() {
		return none, false
	}
	tenant := fd.Type().DgraphName()
	return compiledQuestion{
		of: tenant,
		held: &dql.GraphQuery{
			Attr:     "held",
			Func:     &dql.Function{Name: "type", Args: []dql.Arg{{Value: tenant}}},
			Filter:   edge.Filter,
			Cascade:  []string{"__all__"},
			Children: append([]*dql.GraphQuery{{Attr: "uid"}}, edge.Children...),
		},
	}, true
}

func (g *callerGrants) tenantQuestion(cur schema.Type, rule *schema.RuleNode) (string, bool) {
	c, ok := g.compileTenant(cur, rule)
	if !ok {
		return "", false
	}
	return dgraph.AsString(c.results()), true
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

// row is a row of an answer: a record, or a count of records.
type row struct {
	UID   string `json:"uid"`
	Count int    `json:"count"`
}

func runAuthorityQuery(ns uint64, query string) (answer, error) {
	out, err := readPartition(ns, query)
	if err != nil {
		return answer{}, err
	}
	passed, counted := out["passed"]
	return answerOf(out["all"], out["held"], passed, counted)
}

// readPartition runs query on the engine's own authority, in the partition the
// request runs in, and returns the rows of each block by its name.
func readPartition(ns uint64, query string) (map[string][]row, error) {
	qctx := x.AttachTrustedTenant(context.Background(), ns)
	resp, err := (&edgraph.Server{}).QueryNoAuth(qctx, &api.Request{Query: query, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	out := make(map[string][]row)
	if err := json.Unmarshal(resp.GetJson(), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func counted(rows []row) int {
	n := 0
	for _, r := range rows {
		n += r.Count
	}
	return n
}

// answerOf reads an answer: all is the count of the type's records, held the
// first records that pass, and passed the count of the type's records that
// pass. Where the question has no passed block, isCounted is false and the
// records in held are all of them, and are of the type.
func answerOf(all, held, passed []row, isCounted bool) (answer, error) {
	seen := make(map[string]bool, len(held))
	uids := make([]uint64, 0, len(held))
	for _, h := range held {
		if h.UID == "" || seen[h.UID] {
			continue
		}
		seen[h.UID] = true
		u, err := strconv.ParseUint(h.UID, 0, 64)
		if err != nil {
			return answer{}, err
		}
		uids = append(uids, u)
	}
	total, pass := counted(all), len(uids)
	if isCounted {
		pass = counted(passed)
	}
	a := answer{all: total > 0 && pass == total}
	if len(uids) > maxHeld {
		a.tooMany = true
		return a, nil
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	a.uids = uids
	return a, nil
}
