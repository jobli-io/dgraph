/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"

	"github.com/dgraph-io/dgraph/v25/dql"
	"github.com/dgraph-io/dgraph/v25/graphql/dgraph"
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
)

// A read for a query's questions (SPIKE, partition-grants-together).
//
// partition_grants.go asks the partition a question when the rewriter first
// needs its answer, one read each. A list page for a caller with a grant on a
// group raises 28 to 45 of them.
//
// This file gathers them before the query is rewritten. It walks the types the
// query selects with the same code the rewriter will use to simplify their
// rules, but with the asking turned off: a question with no answer is noted and
// the walk does not go on to what it would only look at if the answer were no.
// The noted questions go to the partition in one read. Their answers let the
// walk go further, so it is repeated until it notes nothing new; each round is
// one read. The rewriter then finds every answer where it would have asked.
//
// What is asked, and what an answer means, is unchanged: each question is the
// one partition_grants.go would have compiled. Three things differ:
//
//   - Blocks that several questions share, such as the caller's bindings, are
//     read once.
//   - The query for a set of questions is kept, so only its first use pays for
//     compiling them. What is kept is the question, never an answer.
//   - Requests from one caller that find the answers expired wait for one read
//     instead of each making it. Nobody is given an expired answer.
//
// The walk has to reach every type the rewriter consults the rules of, the way
// the rewriter does: selected fields, filters on related records, and fields
// that leave only some rules on (visit). If it misses one, or a read fails, the
// rewriter asks as before, so a gap costs time and changes no answer.

// question is one thing to ask the partition: whether, or for which records,
// rule holds, where rule sits in the rules of cur.
type question struct {
	cur    schema.Type
	rule   *schema.RuleNode
	tenant bool
}

// maxRounds bounds the reads one query makes before it is rewritten. Questions
// further down than that are asked one at a time.
const maxRounds = 6

type filteredKey struct {
	rules  *schema.RuleNode
	except string
}

var filtered struct {
	sync.Mutex
	of map[filteredKey]*schema.RuleNode
}

// filteredRules returns the rules a field leaves on with
// @bypassAuth(except: ...). The schema builds them anew on every call. A
// question is known by the rule it is about, so here the result is kept: the
// rule a query is rewritten with is then the one its questions were asked for,
// in this request and the next. Nothing writes to a rule once it is built.
func filteredRules(rules *schema.RuleNode, except []string) *schema.RuleNode {
	if !grantsConf.together || rules == nil {
		return schema.FilterRuleNode(rules, except)
	}
	key := filteredKey{rules: rules, except: strings.Join(except, "\x00")}
	filtered.Lock()
	defer filtered.Unlock()
	if out, ok := filtered.of[key]; ok {
		return out
	}
	out := schema.FilterRuleNode(rules, except)
	if filtered.of == nil || len(filtered.of) >= maxCachedGrants {
		filtered.of = make(map[filteredKey]*schema.RuleNode)
	}
	filtered.of[key] = out
	return out
}

type selectorFn func(schema.Type) *schema.RuleNode

// visit calls fn for the rules of every type that rewriting f consults, with
// how the type's records are reached, as the rewriter does it.
func visit(f schema.Field, selector selectorFn, claims map[string]interface{},
	fn func(typ schema.Type, rules *schema.RuleNode, arr *arrival)) {

	type visited struct {
		rules      *schema.RuleNode
		typ        string
		pred, from string
	}
	seen := make(map[visited]bool)
	one := func(typ schema.Type, sel selectorFn, arr *arrival) {
		if typ == nil || typ.Name() == "" {
			return
		}
		rules := sel(typ)
		if rules == nil {
			return
		}
		key := visited{rules: rules, typ: typ.Name()}
		if arr != nil {
			key.pred, key.from = arr.pred, arr.from.Name()
		}
		if !seen[key] {
			seen[key] = true
			fn(typ, rules, arr)
		}
	}
	each := func(typ schema.Type, sel selectorFn, arr *arrival) {
		if typ == nil {
			return
		}
		one(typ, sel, arr)
		for _, impl := range typ.ImplementingTypes() {
			one(impl, sel, arr)
		}
		if typ.IsUnion() {
			for _, m := range typ.UnionMembers(nil) {
				one(m, sel, arr)
			}
		}
	}
	// A field with @bypassAuth(except: ...) has its type's rules consulted as
	// they are, and then only the excepted ones are applied, there and below.
	narrowed := func(sel selectorFn, except []string, bypass bool) selectorFn {
		if !bypass || len(except) == 0 {
			return sel
		}
		return func(t schema.Type) *schema.RuleNode { return filteredRules(sel(t), except) }
	}
	// A filter on a related record consults that record's rules too.
	var inFilter func(typ schema.Type, sel selectorFn, filter interface{})
	inFilter = func(typ schema.Type, sel selectorFn, filter interface{}) {
		switch v := filter.(type) {
		case []interface{}:
			for _, item := range v {
				inFilter(typ, sel, item)
			}
		case map[string]interface{}:
			for name, inner := range v {
				if name == "and" || name == "or" || name == "not" {
					inFilter(typ, sel, inner)
					continue
				}
				if _, nested := inner.(map[string]interface{}); !nested || typ == nil {
					continue
				}
				fd := typ.Field(name)
				if fd == nil || fd.Type() == nil {
					continue
				}
				each(fd.Type(), sel, nil)
				each(fd.Type(), narrowed(sel, fd.BypassAuthExcept(), fd.EvaluateBypassAuth(claims)), nil)
				inFilter(fd.Type(), sel, inner)
			}
		}
	}
	var walk func(f schema.Field, sel selectorFn, arr *arrival)
	walk = func(f schema.Field, sel selectorFn, arr *arrival) {
		each(f.Type(), sel, arr)
		each(f.ConstructedFor(), sel, arr)
		below := narrowed(sel, f.BypassAuthExcept(), f.EvaluateBypassAuth(claims))
		each(f.Type(), below, arr)
		// The filter of an aggregate field is on the type it aggregates.
		for _, on := range []schema.Type{f.Type(), f.ConstructedFor()} {
			inFilter(on, sel, f.ArgValue("filter"))
			inFilter(on, below, f.ArgValue("filter"))
		}
		for _, child := range f.SelectionSet() {
			walk(child, below, arrivingBy(f.Type(), child))
		}
	}
	walk(f, selector, nil)
}

// askTogether puts the questions rewriting f raises to the partition before f
// is rewritten, a round of them in one read.
func (g *callerGrants) askTogether(f schema.Field, selector func(schema.Type) *schema.RuleNode) {
	if g == nil || !grantsConf.together || f == nil {
		return
	}
	for round := 0; round < maxRounds; round++ {
		g.noting, g.noted, g.isNoted = true, nil, make(map[*schema.RuleNode]bool)
		visit(f, selector, g.claims, func(typ schema.Type, rules *schema.RuleNode, arr *arrival) {
			g.strip(typ, rules, arr)
		})
		need := g.noted
		g.noting, g.noted, g.isNoted = false, nil, nil
		if len(need) == 0 || !g.readOnce(need) {
			return
		}
	}
}

// readOnce gets the answers to need, from one read unless another request from
// this caller has just made it. It reports whether need is now answered.
func (g *callerGrants) readOnce(need []question) bool {
	if grantsConf.ttl > 0 {
		lock := callerLock(g.ns, g.caller)
		lock.Lock()
		defer lock.Unlock()
		if need = g.notRemembered(need); len(need) == 0 {
			return true
		}
	}
	return g.read(need)
}

func callerLock(ns uint64, caller string) *sync.Mutex {
	key := fmt.Sprintf("%d\x00%s", ns, caller)
	grantsConf.mu.Lock()
	defer grantsConf.mu.Unlock()
	if len(grantsConf.asking) >= maxCachedGrants {
		grantsConf.asking = make(map[string]*sync.Mutex)
	}
	lock := grantsConf.asking[key]
	if lock == nil {
		lock = &sync.Mutex{}
		grantsConf.asking[key] = lock
	}
	return lock
}

// notRemembered takes the answers that are remembered and still believed, and
// returns the questions that are left.
func (g *callerGrants) notRemembered(need []question) []question {
	if grantsConf.ttl <= 0 {
		return need
	}
	left := need[:0:0]
	now := time.Now()
	grantsConf.mu.RLock()
	defer grantsConf.mu.RUnlock()
	for _, q := range need {
		if _, ok := g.answers[q.rule]; ok {
			continue
		}
		a, ok := grantsConf.byRule[ruleKey{ns: g.ns, caller: g.caller, rule: q.rule}]
		if ok && now.Before(a.until) {
			grantsConf.remembered.Add(1)
			g.answers[q.rule] = a
			continue
		}
		left = append(left, q)
	}
	return left
}

// remember keeps a for this request and, for the ttl counted from when the
// question was put, for the caller's later ones.
func (g *callerGrants) remember(rule *schema.RuleNode, a answer, asked time.Time) {
	g.answers[rule] = a
	if grantsConf.ttl <= 0 {
		return
	}
	a.until = asked.Add(grantsConf.ttl)
	grantsConf.mu.Lock()
	if len(grantsConf.byRule) >= maxCachedGrants {
		grantsConf.byRule = make(map[ruleKey]answer)
	}
	grantsConf.byRule[ruleKey{ns: g.ns, caller: g.caller, rule: rule}] = a
	grantsConf.mu.Unlock()
}

// prepared is the query for a set of questions, and where each answer is in
// what it returns.
type prepared struct {
	query   string
	slots   []slot
	unknown []*schema.RuleNode
	// blocks is how many blocks find the answers, and compiled how many the
	// questions came to before the ones they share were merged.
	blocks, compiled int
}

type slot struct {
	rule *schema.RuleNode
	// all, held and passed name the blocks of the answer. passed is empty
	// where the question has no such block.
	all, held, passed string
}

// maxPrepared bounds the queries kept. One is a few tens of kilobytes.
const maxPrepared = 500

var preparedQueries struct {
	sync.Mutex
	of map[string]*prepared
}

// prepare compiles need into one query. The query depends on the caller's
// claims and on the rules, which are nodes of the loaded schema, and on nothing
// else, so it is kept under those.
func (g *callerGrants) prepare(need []question) (*prepared, bool) {
	var key strings.Builder
	fmt.Fprintf(&key, "%d\x00%s", g.ns, g.caller)
	for _, q := range need {
		fmt.Fprintf(&key, "\x00%p", q.rule)
	}
	preparedQueries.Lock()
	p, ok := preparedQueries.of[key.String()]
	preparedQueries.Unlock()
	if ok {
		return p, true
	}

	p = &prepared{}
	var blocks, results []*dql.GraphQuery
	types := make(map[string]bool)
	for i, q := range need {
		var c compiledQuestion
		var ok bool
		if q.tenant {
			c, ok = g.compileTenant(q.cur, q.rule)
		} else {
			c, ok = g.compileWrap(q.cur, q.rule)
		}
		if !ok {
			p.unknown = append(p.unknown, q.rule)
			continue
		}
		// Every question was compiled with its own variable names, from 1.
		prefixVars(&c, fmt.Sprintf("q%d_", i))
		blocks = append(blocks, c.vars...)
		s := slot{rule: q.rule, all: "all_" + c.of, held: fmt.Sprintf("held_%d", i)}
		if !types[c.of] {
			types[c.of] = true
			all := allOf(c.of)
			all.Attr = s.all
			results = append(results, all)
		}
		c.held.Attr = s.held
		results = append(results, c.held)
		if c.passed != nil {
			s.passed = fmt.Sprintf("passed_%d", i)
			c.passed.Attr = s.passed
			results = append(results, c.passed)
		}
		p.slots = append(p.slots, s)
	}
	if len(p.slots) > 0 {
		merged := mergeSameBlocks(append(blocks, results...))
		found := merged[:len(merged)-len(results)]
		// Once their blocks are merged, questions that ask the same thing
		// return the same thing. It is returned once.
		results, same := mergeSameResults(merged[len(found):])
		for i := range p.slots {
			s := &p.slots[i]
			if name, ok := same[s.held]; ok {
				s.held = name
			}
			if name, ok := same[s.passed]; ok {
				s.passed = name
			}
		}
		p.query = dgraph.AsString(append(found, results...))
		p.blocks, p.compiled = len(found), len(blocks)
	}
	preparedQueries.Lock()
	if preparedQueries.of == nil || len(preparedQueries.of) >= maxPrepared {
		preparedQueries.of = make(map[string]*prepared)
	}
	preparedQueries.of[key.String()] = p
	preparedQueries.Unlock()
	return p, false
}

// read puts need to the partition in one query and remembers the answers. It
// reports whether it got them.
func (g *callerGrants) read(need []question) bool {
	asked := time.Now()
	p, kept := g.prepare(need)
	for _, rule := range p.unknown {
		// As when it is asked on its own: not a question, the rule stays.
		g.remember(rule, answer{unknown: true}, asked)
	}
	if len(p.slots) == 0 {
		return true
	}
	built := time.Since(asked)

	grantsConf.lookups.Add(1)
	grantsConf.questions.Add(uint64(len(p.slots)))
	out, err := readPartition(g.ns, p.query)
	if err != nil {
		grantsConf.failed.Add(1)
		glog.Warningf("partition grants: could not read %d questions together in namespace %d, "+
			"asking one at a time: %v", len(p.slots), g.ns, err)
		return false
	}
	for _, s := range p.slots {
		a, err := answerOf(out[s.all], out[s.held], out[s.passed], s.passed != "")
		if err != nil {
			return false
		}
		g.remember(s.rule, a, asked)
	}
	glog.V(2).Infof("partition grants: read %d questions together in namespace %d: %d blocks of %d, "+
		"built in %s, answered in %s (kept %v, %d bytes)", len(p.slots), g.ns, p.blocks, p.compiled,
		built.Round(10*time.Microsecond), (time.Since(asked) - built).Round(10*time.Microsecond), kept, len(p.query))
	return true
}

// mergeSameResults drops every block that returns what an earlier one does. It
// returns the blocks kept and, for each one dropped, the name of the one that
// stands for it.
func mergeSameResults(results []*dql.GraphQuery) ([]*dql.GraphQuery, map[string]string) {
	first := make(map[string]string)
	same := make(map[string]string)
	kept := make([]*dql.GraphQuery, 0, len(results))
	for _, r := range results {
		name := r.Attr
		r.Attr = "r"
		text := dgraph.AsString([]*dql.GraphQuery{r})
		r.Attr = name
		if earlier, ok := first[text]; ok {
			same[name] = earlier
			continue
		}
		first[text] = name
		kept = append(kept, r)
	}
	return kept, same
}

// definitions returns the nodes of q that define a variable, in a fixed order.
func definitions(q *dql.GraphQuery, out []*dql.GraphQuery) []*dql.GraphQuery {
	if q == nil {
		return out
	}
	if q.Var != "" {
		out = append(out, q)
	}
	for _, c := range q.Children {
		out = definitions(c, out)
	}
	return out
}

// prefixVars renames every variable c defines, and its uses inside c.
func prefixVars(c *compiledQuestion, prefix string) {
	var defs []*dql.GraphQuery
	for _, b := range c.vars {
		defs = definitions(b, defs)
	}
	if len(defs) == 0 {
		return
	}
	renamed := make(map[string]string, len(defs))
	for _, d := range defs {
		renamed[d.Var] = prefix + d.Var
	}
	applyAuthVarSubstToQueries(c.vars, renamed)
	applyAuthVarSubstToQueries([]*dql.GraphQuery{c.held, c.passed}, renamed)
	for _, d := range defs {
		d.Var = renamed[d.Var]
	}
}

// mergeSameBlocks drops every block that is, but for the names of the variables
// it defines, the same text as an earlier one, and points its users at the
// earlier one. Merging two blocks can make two more the same, so it repeats
// until nothing merges. The printed query is compared, so anything that makes
// two blocks read differently keeps them apart.
func mergeSameBlocks(blocks []*dql.GraphQuery) []*dql.GraphQuery {
	for {
		first := make(map[string][]*dql.GraphQuery)
		renamed := make(map[string]string)
		kept := make([]*dql.GraphQuery, 0, len(blocks))
		for _, b := range blocks {
			defs := definitions(b, nil)
			if len(defs) == 0 {
				kept = append(kept, b)
				continue
			}
			names := make([]string, len(defs))
			for i, d := range defs {
				names[i] = d.Var
				d.Var = fmt.Sprintf("D%d", i)
			}
			text := dgraph.AsString([]*dql.GraphQuery{b})
			for i, d := range defs {
				d.Var = names[i]
			}
			same, ok := first[text]
			if !ok {
				first[text] = defs
				kept = append(kept, b)
				continue
			}
			for i, name := range names {
				renamed[name] = same[i].Var
			}
		}
		if len(renamed) == 0 {
			return kept
		}
		applyAuthVarSubstToQueries(kept, renamed)
		blocks = kept
	}
}
