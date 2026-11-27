/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package subscription

// SPIKE: run a subscription again only if the change can have changed its answer.
//
// Scope routing (poller.go) keeps a change inside its workspace. Inside the workspace
// a change still ran again every subscription that mentions the changed type: one note
// added to one candidate ran the notes panel of every other candidate. Here the change
// says which records it wrote to and which predicates on each (resolve.Change), and
// each subscription knows which predicates its query reads, which records its answer
// holds, and how its rules were answered (resolve.RunFacts, resolve.PageShape).
//
// A subscription's answer can only change if the store was asked to read something
// that was written. So a subscription is run again when
//
//   - it holds a record that was written to, on a predicate it reads; or
//   - a record that was written to, which it does not hold, could now be shown below
//     a record it does hold (the predicate written is one that can hide a record
//     there: read by a nested filter, or by a rule); or
//   - a record that was made or written to could be one of its root records. That is
//     the one thing asked of the store: a query by uid, in place of the whole page.
//
// and is left alone otherwise. It reasons this way only about a subscription whose
// rules the caller's grants settled into plain filters (the partition fast path). A
// rule the store has to work out with query blocks of its own can depend on any
// record, and such a subscription is run as it is today.

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/golang/glog"

	"github.com/dgraph-io/dgraph/v25/graphql/resolve"
)

var changeNarrowing atomic.Bool

// EnableChangeNarrowing turns narrowing on, and makes mutations say what they wrote.
func EnableChangeNarrowing() {
	changeNarrowing.Store(true)
	resolve.EnableChangeFacts()
}

// page is what a bucket's query is known to read.
type page struct {
	mu     sync.Mutex
	shape  *resolve.PageShape
	header http.Header
	// From its runs: what the store was asked to read (kept from every run), whether
	// the rules of the last run were settled, and whether every record of the last
	// answer is named in it.
	reads, reverse, ruleReads, ruleInverse map[string]struct{}
	ruled, whole, ran                      bool
	ruledBy, unnamed                       string
	// running counts the runs in flight: while one is, what the bucket holds is about
	// to change, so nothing is concluded from it.
	running int
	// asking keeps the questions put to the store about this page one at a time: they
	// read the same parsed query.
	asking sync.Mutex
}

func (pg *page) begin() {
	if pg == nil {
		return
	}
	pg.mu.Lock()
	pg.running++
	pg.mu.Unlock()
}

// end takes in what a run read and answered. counted is false for a run that failed.
func (pg *page) end(facts *resolve.RunFacts, data []byte, counted bool) {
	if pg == nil {
		return
	}
	pg.mu.Lock()
	defer pg.mu.Unlock()
	pg.running--
	if !counted || facts == nil {
		return
	}
	merge := func(into *map[string]struct{}, from map[string]struct{}) {
		if *into == nil {
			*into = make(map[string]struct{}, len(from))
		}
		for p := range from {
			(*into)[p] = struct{}{}
		}
	}
	merge(&pg.reads, facts.Reads)
	merge(&pg.reverse, facts.Reverse)
	merge(&pg.ruleReads, facts.RuleReads)
	merge(&pg.ruleInverse, facts.RuleInverse)
	pg.ruled = facts.Ruled || facts.Queries == 0
	pg.ruledBy = facts.RuledBy
	if facts.Queries == 0 {
		pg.ruledBy = "no query was seen to run"
	}
	pg.unnamed = ""
	if pg.shape != nil {
		pg.unnamed = unnamedRecord(data, pg.shape.Counts)
	}
	pg.whole = pg.shape != nil && pg.unnamed == ""
	pg.ran = true
}

// unnamedRecord looks for an object in a query's answer that does not carry the id of
// its record, and returns where it is; "" if every object does, so that the bucket
// holds every record its answer shows. counts are the places in the answer that hold
// a count of records and none of the records.
func unnamedRecord(data []byte, counts map[string]struct{}) string {
	var answer map[string]interface{}
	if err := json.Unmarshal(data, &answer); err != nil {
		return "an answer that could not be read"
	}
	var unnamed func(v interface{}, path string) string
	unnamed = func(v interface{}, path string) string {
		if _, isCount := counts[path]; isCount {
			return ""
		}
		switch node := v.(type) {
		case []interface{}:
			for _, item := range node {
				if at := unnamed(item, path); at != "" {
					return at
				}
			}
		case map[string]interface{}:
			if id, _ := node["id"].(string); !hexUIDRegex.MatchString(id) {
				return path
			}
			for key, child := range node {
				if at := unnamed(child, path+"."+key); at != "" {
					return at
				}
			}
		}
		return ""
	}
	for name, root := range answer {
		if at := unnamed(root, name); at != "" {
			return at
		}
	}
	return ""
}

type verdict int

const (
	asToday verdict = iota // not reasoned about: run it if today's rule says so
	wake
	skip
	ask // run it if the store says a changed record could be one of its root records
)

// decide says what a change means for the page. holds reports whether the bucket's
// answer holds a record. made and changed are the records to ask the store about.
func (pg *page) decide(c *resolve.Change, types []string, holds func(uid string) bool) (
	v verdict, why string, made, changed []string) {

	if pg == nil || pg.shape == nil {
		return asToday, "nothing known of its query", nil, nil
	}
	if c == nil || c.Kind == resolve.ChangeDelete {
		return asToday, "the change does not say what it wrote", nil, nil
	}
	if !c.Complete {
		return asToday, "the change wrote to a record it could not name: " + c.Unnamed, nil, nil
	}
	pg.mu.Lock()
	defer pg.mu.Unlock()
	switch {
	case pg.running > 0:
		return wake, "it is running", nil, nil
	case !pg.shape.Plain:
		return asToday, pg.shape.Why, nil, nil
	case !pg.ran:
		return asToday, "it has not run", nil, nil
	case pg.ruled:
		return asToday, "its rules are worked out by the store: " + strings.Join(strings.Fields(pg.ruledBy), " "), nil, nil
	case !pg.whole:
		return asToday, "it shows records it does not name, at " + pg.unnamed, nil, nil
	}
	in := func(set map[string]struct{}, p string) bool { _, ok := set[p]; return ok }
	asked := func(p string) bool { // what the GraphQL itself reads
		return in(pg.shape.RootReads, p) || in(pg.shape.NestedReads, p)
	}
	// reads: the page depends on p. Either its GraphQL or a settled rule names p, or
	// the store was asked to read it. An edge that a filter or a settled rule tests
	// may be read by the store from its far end only, through the inverse predicate:
	// the edge itself still counts as read, and the inverse does not count by itself,
	// because a GraphQL mutation that writes one writes the other.
	reads := func(p string) bool {
		if asked(p) || in(pg.ruleReads, p) {
			return true
		}
		return in(pg.reads, p) && !in(pg.ruleInverse, p) && !in(pg.shape.FilterInverse, p)
	}
	// hides: p is read where it can decide whether a record below the root is shown.
	hides := func(p string) bool {
		return in(pg.ruleReads, p) || in(pg.shape.NestedReads, p) || (reads(p) && !in(pg.shape.RootReads, p))
	}
	for uid, preds := range c.Subjects {
		if !holds(uid) {
			continue
		}
		for _, p := range preds {
			if reads(p) {
				return wake, "it holds " + uid + ", written on " + p, nil, nil
			}
		}
	}
	for uid, preds := range c.Objects {
		for _, p := range preds {
			if in(pg.reverse, p) && (holds(uid) || hides(p)) {
				return wake, "it reads " + p + " from " + uid + ", which an edge written points at", nil, nil
			}
		}
	}
	nested, rootType := false, false
	for _, t := range types {
		nested = nested || in(pg.shape.NestedTypes, t)
		rootType = rootType || in(pg.shape.RootTypes, t)
	}
	roots := make(map[string]struct{}, len(c.Roots))
	for _, r := range c.Roots {
		roots[r] = struct{}{}
	}
	for uid, preds := range c.Subjects {
		if holds(uid) {
			continue
		}
		read, hidden := false, ""
		for _, p := range preds {
			if reads(p) {
				read = true
				if hides(p) {
					hidden = p
				}
			}
		}
		if !read {
			continue
		}
		_, isRoot := roots[uid]
		if hidden != "" && (!isRoot || nested) {
			return wake, uid + " may show below a record it holds: written on " + hidden, nil, nil
		}
		if isRoot && !rootType {
			continue // a record of the mutated type, which no root record here can be
		}
		changed = append(changed, uid)
	}
	for _, t := range c.NewTypes {
		if t == pg.shape.RootType.DgraphName() {
			made = c.New
		}
	}
	if len(made)+len(changed) == 0 {
		return skip, "nothing it reads was written", nil, nil
	}
	return ask, "", made, changed
}

// narrow routes one change to the buckets it can have changed. today is the set of
// buckets today's rule would run, and held those of them that hold a record the
// mutation returned. The caller holds the lock.
func (p *Poller) narrow(msg *InvalidationMessage, today, held map[uint64]struct{}) {
	for bucketID, where := range p.bucketScope {
		_, mentioned := today[bucketID]
		pg := where.page
		if msg.HasNamespace && where.hasNs && msg.Namespace != where.ns {
			// Another namespace's records: nothing here can be asked about them.
			if mentioned {
				p.trigger(bucketID, msg)
			}
			continue
		}
		if _, holding := held[bucketID]; !holding && scopeRouting.Load() && !p.reaches(bucketID, msg) {
			continue
		}
		bucket := bucketID
		holds := func(uid string) bool { return p.dependencyReg.Holds(bucket, uid) }
		v, why, made, changed := pg.decide(msg.Change, msg.Types, holds)
		if v == asToday {
			if !mentioned {
				continue
			}
			v = wake
		}
		if glog.V(2) {
			glog.Infof("subscription narrowing: bucket %d: %s: %s", bucketID, [...]string{"", "run", "left", "asked"}[v], why)
		}
		switch v {
		case wake:
			p.trigger(bucketID, msg)
		case skip:
			counted("skipped")
		case ask:
			counted("checked")
			go func() {
				pg.asking.Lock()
				could, err := resolve.RootCandidates(subscriberContext(pg.header), pg.header, pg.shape.Query, made, changed, msg.Change.Written)
				pg.asking.Unlock()
				if err != nil {
					glog.Warningf("subscription narrowing: bucket %d is run, the store could not be asked: %v", bucket, err)
				}
				if could || err != nil {
					p.RLock()
					p.trigger(bucket, msg)
					p.RUnlock()
					return
				}
				counted("skipped")
			}()
		}
	}
}
