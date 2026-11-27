/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

// SPIKE: what a query read, and whether a changed record could be one of its rows.
//
// The other half of change_facts.go. A subscription keeps, from each run of its query:
//
//   - RunFacts: the predicates the store was asked to read, taken from the DQL that
//     was sent, and whether its rules were answered with query blocks. A rule that
//     became a query block can depend on any record; a rule the caller's grants
//     settled is a filter on the record's own edges, and can be reasoned about.
//   - PageShape: what the GraphQL said, before any rule: which predicates it reads on
//     its root records and which on the records below them.
//
// and RootCandidates is the one question put to the store in place of a whole run:
// could any of these changed records be a root record of this query?

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	dgoapi "github.com/dgraph-io/dgo/v250/protos/api"

	"github.com/dgraph-io/dgraph/v25/dql"
	"github.com/dgraph-io/dgraph/v25/graphql/dgraph"
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/x"
)

// RunFacts is what the queries of one request read. It is filled in by the query
// rewriter when the request's context carries one.
type RunFacts struct {
	mu sync.Mutex
	// Reads is every predicate the store was asked to read, and Reverse those it was
	// asked to follow against their direction.
	Reads   map[string]struct{}
	Reverse map[string]struct{}
	// RuleReads are the predicates read, on the record itself, by rules that were
	// settled into a filter. RuleInverse are the inverse predicates such a rule was
	// read through instead: the same edge, as written on the other record.
	RuleReads   map[string]struct{}
	RuleInverse map[string]struct{}
	// Ruled says a rule was answered with query blocks of its own; RuledBy is one.
	Ruled   bool
	RuledBy string
	// Queries counts the queries noted.
	Queries int
}

type runFactsKey struct{}

// WithRunFacts returns a context under which queries note what they read in rf.
func WithRunFacts(ctx context.Context, rf *RunFacts) context.Context {
	return context.WithValue(ctx, runFactsKey{}, rf)
}

func add(set *map[string]struct{}, pred string) {
	if pred == "" {
		return
	}
	if *set == nil {
		*set = make(map[string]struct{})
	}
	(*set)[pred] = struct{}{}
}

// noteRun records the DQL of one rewritten query, and what its rules came to.
func noteRun(ctx context.Context, blocks []*dql.GraphQuery, vg *VariableGenerator) {
	rf, _ := ctx.Value(runFactsKey{}).(*RunFacts)
	if rf == nil {
		return
	}
	rf.mu.Lock()
	defer rf.mu.Unlock()
	rf.Queries++
	for _, b := range blocks {
		rf.block(b, true)
	}
	if vg != nil {
		rf.Ruled = rf.Ruled || vg.ruled
		if vg.ruledBy != "" {
			rf.RuledBy = vg.ruledBy
		}
		for p := range vg.ruleReads {
			add(&rf.RuleReads, p)
		}
		for p := range vg.ruleInverse {
			add(&rf.RuleInverse, p)
		}
	}
}

func (rf *RunFacts) read(attr string) {
	if i := strings.IndexByte(attr, '@'); i > 0 {
		attr = attr[:i] // a language tag
	}
	switch attr {
	case "", "uid", "var", "dgraph.type":
		return
	}
	if strings.HasPrefix(attr, "~") {
		attr = attr[1:]
		add(&rf.Reverse, attr)
	}
	add(&rf.Reads, attr)
}

func (rf *RunFacts) block(q *dql.GraphQuery, top bool) {
	if q == nil {
		return
	}
	if !top {
		rf.read(q.Attr)
	}
	rf.function(q.Func)
	rf.filter(q.Filter)
	for _, o := range q.Order {
		rf.read(o.Attr)
	}
	for _, g := range q.GroupbyAttrs {
		rf.read(g.Attr)
	}
	for _, c := range q.Children {
		rf.block(c, false)
	}
}

func (rf *RunFacts) function(f *dql.Function) {
	for _, p := range functionReads(f) {
		rf.read(p)
	}
}

func (rf *RunFacts) filter(t *dql.FilterTree) {
	for _, p := range filterTreeReads(t) {
		rf.read(p)
	}
}

// functionReads is the predicate a DQL function reads, if it reads one. The rewriter
// puts it in Attr, or as the first argument.
func functionReads(f *dql.Function) []string {
	if f == nil {
		return nil
	}
	var out []string
	if f.Attr != "" {
		out = append(out, f.Attr)
	}
	if f.Name != "uid" && f.Name != "type" && len(f.Args) > 0 && !f.Args[0].IsValueVar && !f.Args[0].IsDQLVar {
		out = append(out, f.Args[0].Value)
	}
	return out
}

func filterTreeReads(t *dql.FilterTree) []string {
	if t == nil {
		return nil
	}
	out := functionReads(t.Func)
	for _, c := range t.Child {
		out = append(out, filterTreeReads(c)...)
	}
	return out
}

// settledRule notes a rule the caller's grants settled into a test of one edge of
// the record: pred, read through inverse when the store is asked from the other end.
// blocks are the query blocks the test needs: they hold no rule of their own.
func (v *VariableGenerator) settledRule(pred, inverse string, blocks ...*dql.GraphQuery) {
	if v == nil {
		return
	}
	for _, b := range blocks {
		if v.settledBlocks == nil {
			v.settledBlocks = make(map[*dql.GraphQuery]struct{})
		}
		v.settledBlocks[b] = struct{}{}
	}
	add(&v.ruleReads, pred)
	if inverse != "" {
		add(&v.ruleInverse, strings.TrimPrefix(inverse, "~"))
	}
}

// ruleCameTo notes what the rules of one type came to: a query block that is not
// that of a settled rule is a rule the store has to work out, and the filter is read
// on the record itself.
func (v *VariableGenerator) ruleCameTo(blocks []*dql.GraphQuery, filter *dql.FilterTree) {
	if v == nil {
		return
	}
	for _, b := range blocks {
		if _, settled := v.settledBlocks[b]; settled {
			continue
		}
		if testsOwnPredicates(b) {
			for _, p := range append(functionReads(b.Func), filterTreeReads(b.Filter)...) {
				add(&v.ruleReads, strings.TrimPrefix(p, "~"))
			}
			continue
		}
		v.ruled = true
		if v.ruledBy == "" {
			v.ruledBy = dgraph.AsString([]*dql.GraphQuery{b})
		}
	}
	for _, p := range filterTreeReads(filter) {
		add(&v.ruleReads, strings.TrimPrefix(p, "~"))
	}
}

// testsOwnPredicates reports whether a rule's query block picks records by their own
// predicates alone: it follows no edge to another record. Such a rule is a filter on
// the record, whatever set of records it is asked of.
func testsOwnPredicates(b *dql.GraphQuery) bool {
	if b == nil || b.Func == nil {
		return false
	}
	for _, c := range b.Children {
		if (c.Attr != "dgraph.type" && c.Attr != "uid") || len(c.Children) > 0 || c.Filter != nil {
			return false
		}
	}
	return true
}

// PageShape is what a subscription's GraphQL says it reads, before any rule.
type PageShape struct {
	// Plain is false when the query is not one this reasoning covers; Why says why.
	Plain bool
	Why   string
	// Query is the one root query, and RootType the type of its root records.
	Query    schema.Query
	RootType schema.Type
	// RootReads are the predicates read on the root records; NestedReads those read
	// on the records below them, or by a filter that looks below.
	RootReads   map[string]struct{}
	NestedReads map[string]struct{}
	// NestedTypes are the types the records below the root can have.
	NestedTypes map[string]struct{}
	// FilterInverse are the inverse predicates a nested filter was read through: the
	// same edge as the one the filter names, as written on the record at its far end.
	FilterInverse map[string]struct{}
	// RootTypes are the names a root record's type can have.
	RootTypes map[string]struct{}
	// Counts are the places in the answer (response names, joined by dots) that hold
	// no record: a count of records in place of the records, or a point on a map.
	Counts map[string]struct{}
}

// ShapeOf reads the shape of a subscription's operation.
func ShapeOf(op schema.Operation) *PageShape {
	s := &PageShape{RootReads: map[string]struct{}{}, NestedReads: map[string]struct{}{},
		NestedTypes: map[string]struct{}{}, Counts: map[string]struct{}{},
		FilterInverse: map[string]struct{}{}, RootTypes: map[string]struct{}{}}
	queries := op.Queries()
	if len(queries) != 1 {
		s.Why = "not one root query"
		return s
	}
	q := queries[0]
	s.Query = q
	switch q.QueryType() {
	case schema.FilterQuery, schema.AggregateQuery:
	case schema.GetQuery:
		if _, uid, err := q.IDArgValue(); err != nil || uid == 0 {
			s.Why = "a get that is not by ID"
			return s
		}
	default:
		s.Why = "not a list, a count or a get"
		return s
	}
	if q.IsCustomHTTP() {
		s.Why = "a custom query"
		return s
	}
	s.RootType = q.ConstructedFor()
	if s.RootType == nil {
		s.Why = "no root type"
		return s
	}
	s.RootTypes[s.RootType.Name()] = struct{}{}
	s.RootTypes[s.RootType.DgraphName()] = struct{}{}
	if s.RootType.IsInterface() {
		for _, impl := range s.RootType.ImplementingTypes() {
			s.RootTypes[impl.Name()] = struct{}{}
			s.RootTypes[impl.DgraphName()] = struct{}{}
		}
	}
	if !s.filterReads(s.RootType, extractQueryFilter(q), 0) {
		return s
	}
	s.orderReads(s.RootType, q.ArgValue("order"), 0)
	if q.QueryType() == schema.AggregateQuery {
		if !s.onlyCounts(q) {
			return s
		}
		s.Counts[q.ResponseName()] = struct{}{}
	} else {
		for _, f := range q.SelectionSet() {
			if !s.selection(f, 0, q.ResponseName()) {
				return s
			}
		}
	}
	s.Plain = true
	return s
}

func (s *PageShape) note(pred string, depth int) {
	if pred == "" || pred == "uid" || pred == "dgraph.type" {
		return
	}
	pred = strings.TrimPrefix(pred, "~")
	if depth == 0 {
		s.RootReads[pred] = struct{}{}
	} else {
		s.NestedReads[pred] = struct{}{}
	}
}

// below notes the types a record reached through a field can have.
func (s *PageShape) below(t schema.Type) {
	if t == nil {
		return
	}
	s.NestedTypes[t.Name()] = struct{}{}
	s.NestedTypes[t.DgraphName()] = struct{}{}
	if t.IsInterface() {
		for _, impl := range t.ImplementingTypes() {
			s.NestedTypes[impl.Name()] = struct{}{}
			s.NestedTypes[impl.DgraphName()] = struct{}{}
		}
	}
	if t.IsUnion() {
		for _, m := range t.UnionMembers(nil) {
			s.NestedTypes[m.Name()] = struct{}{}
			s.NestedTypes[m.DgraphName()] = struct{}{}
		}
	}
}

// onlyCounts reports whether an aggregate asks for nothing but a count.
func (s *PageShape) onlyCounts(f schema.Field) bool {
	for _, c := range f.SelectionSet() {
		if c.Name() != "count" && c.Name() != schema.Typename {
			s.Why = "an aggregate of a field"
			return false
		}
	}
	return true
}

func (s *PageShape) selection(f schema.Field, depth int, path string) bool {
	if f.Name() == schema.Typename {
		return true
	}
	if f.IsCustomHTTP() {
		s.Why = "a custom field"
		return false
	}
	path += "." + f.ResponseName()
	if f.IsAggregateField() {
		// a count of the records an edge reaches: they are read, and never shown
		counted := f.ConstructedFor()
		if counted == nil || !s.onlyCounts(f) {
			if s.Why == "" {
				s.Why = "a count of an unknown type"
			}
			return false
		}
		s.note(f.ConstructedForDgraphPredicate(), depth)
		s.below(counted)
		s.Counts[path] = struct{}{}
		filter, _ := f.ArgValue("filter").(map[string]interface{})
		return s.filterReads(counted, filter, depth+1)
	}
	s.note(f.DgraphPredicate(), depth)
	if len(f.SelectionSet()) == 0 {
		return true
	}
	t := f.Type()
	if t.IsGeo() {
		s.Counts[path] = struct{}{} // a point or a shape: a value of the record, not a record
		return true
	}
	s.below(t)
	// what the field's own arguments read, they read on the records it reaches
	filter, _ := f.ArgValue("filter").(map[string]interface{})
	if !s.filterReads(t, filter, depth+1) {
		return false
	}
	s.orderReads(t, f.ArgValue("order"), depth+1)
	for _, c := range f.SelectionSet() {
		if !s.selection(c, depth+1, path) {
			return false
		}
	}
	return true
}

func (s *PageShape) orderReads(t schema.Type, order interface{}, depth int) {
	o, _ := order.(map[string]interface{})
	for o != nil {
		for _, dir := range []string{"asc", "desc"} {
			if name, ok := o[dir].(string); ok {
				s.note(t.DgraphPredicate(name), depth)
			}
		}
		o, _ = o["then"].(map[string]interface{})
	}
}

// filterReads notes the predicates a GraphQL filter on t reads. False, with Why set,
// if it holds something this does not know how to read.
func (s *PageShape) filterReads(t schema.Type, filter map[string]interface{}, depth int) bool {
	reads, nested, _, ok := filterFacts(t, filter, nil)
	if !ok {
		raw, _ := json.Marshal(filter)
		s.Why = "a filter on " + t.Name() + " this cannot read: " + string(raw)
		return false
	}
	for p := range reads {
		s.note(p, depth)
	}
	for p := range nested.reads {
		s.note(p, depth+1)
	}
	for p := range nested.inverse {
		s.FilterInverse[p] = struct{}{}
	}
	for _, nt := range nested.types {
		s.below(nt)
	}
	return true
}

type nestedFacts struct {
	reads map[string]struct{}
	// inverse are the inverse predicates of the edges the nested filters follow. The
	// store may be asked from that end, but a GraphQL mutation that writes one writes
	// the edge itself too, so it is the edge that is counted as read.
	inverse map[string]struct{}
	types   []schema.Type
}

// filterFacts reads one GraphQL filter object on t: the predicates it reads on the
// record itself, what it reads on the records its nested filters look at, and whether
// a nested filter is one the rules of the nested type apply to (ruled).
func filterFacts(t schema.Type, filter map[string]interface{}, claims map[string]interface{}) (
	reads map[string]struct{}, nested nestedFacts, ruled bool, ok bool) {

	reads = map[string]struct{}{}
	nested.reads = map[string]struct{}{}
	nested.inverse = map[string]struct{}{}
	ok = true
	idName := ""
	if idf := t.IDField(); idf != nil {
		idName = idf.Name()
	}
	var each func(f map[string]interface{})
	each = func(f map[string]interface{}) {
		for key, val := range f {
			if val == nil {
				continue
			}
			switch key {
			case "and", "or", "not":
				switch v := val.(type) {
				case map[string]interface{}:
					each(v)
				case []interface{}:
					for _, item := range v {
						if m, isMap := item.(map[string]interface{}); isMap {
							each(m)
						}
					}
				}
				continue
			case "has":
				list, _ := val.([]interface{})
				for _, name := range list {
					if n, isStr := name.(string); isStr {
						reads[t.DgraphPredicate(n)] = struct{}{}
					}
				}
				continue
			case "memberTypes", "_metadata":
				continue
			}
			if key == idName {
				continue
			}
			fd := t.Field(key)
			if fd == nil || fd.HasEmbeddingDirective() {
				ok = false
				continue
			}
			reads[t.DgraphPredicate(key)] = struct{}{}
			if fd.Type().IsInbuiltOrEnumType() {
				continue
			}
			// a nested filter: the records it looks at are of another type
			if inv := fd.Inverse(); inv != nil {
				nested.inverse[inv.DgraphPredicate()] = struct{}{}
			}
			nested.types = append(nested.types, fd.Type())
			if !(fd.EvaluateBypassAuth(claims) && len(fd.BypassAuthExcept()) == 0) {
				ruled = true
			}
			if m, isMap := val.(map[string]interface{}); isMap {
				r, n, nr, nok := filterFacts(fd.Type(), m, claims)
				ok = ok && nok
				ruled = ruled || nr
				for p := range r {
					nested.reads[p] = struct{}{}
				}
				for p := range n.reads {
					nested.reads[p] = struct{}{}
				}
				for p := range n.inverse {
					nested.inverse[p] = struct{}{}
				}
				nested.types = append(nested.types, n.types...)
			}
		}
	}
	each(filter)
	return reads, nested, ruled, ok
}

// conjuncts splits a GraphQL filter into parts that all have to hold. A filter object
// is the AND of its keys unless it has an `or`, which makes the whole object one part.
func conjuncts(filter map[string]interface{}) []map[string]interface{} {
	if len(filter) == 0 {
		return nil
	}
	if _, hasOr := filter["or"]; hasOr {
		return []map[string]interface{}{filter}
	}
	var out []map[string]interface{}
	for key, val := range filter {
		if val == nil || key == "_metadata" {
			continue
		}
		if key == "and" {
			switch v := val.(type) {
			case map[string]interface{}:
				out = append(out, conjuncts(v)...)
			case []interface{}:
				for _, item := range v {
					if m, isMap := item.(map[string]interface{}); isMap {
						out = append(out, conjuncts(m)...)
					}
				}
			}
			continue
		}
		out = append(out, map[string]interface{}{key: val})
	}
	return out
}

// RootCandidates asks the store whether any of the given records could be a root
// record of q, as far as that can be told from the parts of q's filter that:
//
//   - carry no rule of another type (those could hide a record or show it for
//     reasons that are not in the filter), and
//   - for the records that were already there (changed), read no predicate that was
//     written: such a part is as true of the record now as it was before the change.
//
// A record that fails those parts was not a root record before the change and is not
// one now, whatever its rules say. made are records that did not exist before, so
// every rule-free part counts for them. It answers true when it cannot tell. header
// is what the subscriber presented: the question is asked as them, in their namespace.
func RootCandidates(ctx context.Context, header http.Header, q schema.Query, made, changed, written []string) (bool, error) {
	if len(made)+len(changed) == 0 {
		return false, nil
	}
	typ := q.ConstructedFor()
	// as Resolve does for a request: the caller's token, then the namespace it names
	ctx, err := q.GetAuthMeta().AttachAuthorizationJwt(ctx, header)
	if err != nil {
		return true, err
	}
	if ctx, err = x.ResolveTenant(ctx); err != nil {
		return true, err
	}
	claims, err := q.GetAuthMeta().ExtractCustomClaims(ctx)
	if err != nil {
		return true, err
	}
	var filter map[string]interface{}
	if q.QueryType() == schema.GetQuery {
		_, uid, err := q.IDArgValue()
		idf := typ.IDField()
		if err != nil || uid == 0 || idf == nil {
			return true, err
		}
		filter = map[string]interface{}{idf.Name(): []interface{}{uidString(uid)}}
	} else if filter, err = copyFilterArg(extractQueryFilter(q)); err != nil {
		return true, err
	}
	forMade, forChanged := usableParts(typ, filter, claims.AuthVariables, written)
	if (len(made) > 0 && len(forMade) == 0) || (len(changed) > 0 && len(forChanged) == 0) {
		return true, nil // nothing to tell them apart by
	}
	var blocks []*dql.GraphQuery
	ask := func(name string, uids []string, parts []interface{}) error {
		if len(uids) == 0 {
			return nil
		}
		ids, err := convertIDsWithErr(uids)
		if err != nil {
			return err
		}
		// a copy each: building a filter takes keys out of the object it is given
		own, err := copyFilterArg(map[string]interface{}{"and": parts})
		if err != nil {
			return err
		}
		rw := &authRewriter{authVariables: claims.AuthVariables, isWritingAuth: true, varGen: NewVariableGenerator(),
			cascadeVarCache: make(map[cascadeCacheKey]string), allowedTypesForEdge: make(map[string][]string)}
		tree, vars := buildFilter(typ, own, rw, name)
		block := &dql.GraphQuery{Attr: name, Func: &dql.Function{Name: "uid", UID: ids},
			Filter: tree, Children: []*dql.GraphQuery{{Attr: "uid"}}}
		addTypeFilter(block, typ)
		blocks = append(blocks, block)
		blocks = append(blocks, vars...)
		return nil
	}
	if err := ask("made", made, forMade); err != nil {
		return true, err
	}
	if err := ask("changed", changed, forChanged); err != nil {
		return true, err
	}
	resp, err := NewDgraphExecutor().Execute(ctx, &dgoapi.Request{Query: dgraph.AsString(blocks), ReadOnly: true}, nil)
	if err != nil {
		return true, err
	}
	var got map[string][]interface{}
	if err := json.Unmarshal(resp.GetJson(), &got); err != nil {
		return true, err
	}
	return len(got["made"])+len(got["changed"]) > 0, nil
}

// usableParts picks the parts of a filter on typ that say something of a changed
// record whatever its rules: forMade for a record that was not there before, and
// forChanged, those of them that read nothing that was written, for one that was.
func usableParts(typ schema.Type, filter map[string]interface{}, claims map[string]interface{}, written []string) (
	forMade, forChanged []interface{}) {

	wrote := make(map[string]struct{}, len(written))
	for _, p := range written {
		wrote[p] = struct{}{}
	}
	for _, part := range conjuncts(filter) {
		reads, nested, ruled, ok := filterFacts(typ, part, claims)
		if ruled || !ok {
			continue
		}
		forMade = append(forMade, part)
		stable := true
		for p := range reads {
			if _, w := wrote[p]; w {
				stable = false
			}
		}
		for p := range nested.reads {
			if _, w := wrote[p]; w {
				stable = false
			}
		}
		if stable {
			forChanged = append(forChanged, part)
		}
	}
	return forMade, forChanged
}

func uidString(uid uint64) string {
	return fmt.Sprintf("%#x", uid)
}

// copyFilterArg is a deep copy of a filter, keeping numbers as they were written.
func copyFilterArg(filter map[string]interface{}) (map[string]interface{}, error) {
	if filter == nil {
		return nil, nil
	}
	raw, err := json.Marshal(filter)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]interface{}
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
