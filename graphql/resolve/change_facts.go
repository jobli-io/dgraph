/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

// SPIKE: what a mutation changed, read from the mutation it sent to the store.
//
// A subscription is run again when a mutation touches a type its query mentions. That
// says nothing of WHICH records were touched, so one note added to one candidate runs
// again the notes panel of every other candidate. The mutation sent to the store says
// exactly which records it wrote to, and which predicates on each: this reads it, so
// the subscriptions can be asked a narrower question (see subscription/narrow.go).

import (
	"encoding/json"
	"sort"
	"strings"
	"sync/atomic"

	dgoapi "github.com/dgraph-io/dgo/v250/protos/api"

	"github.com/dgraph-io/dgraph/v25/graphql/schema"
)

var changeFacts atomic.Bool

// EnableChangeFacts makes each mutation say what it wrote, for the subscriptions.
func EnableChangeFacts() {
	changeFacts.Store(true)
}

// ChangeFactsEnabled reports whether mutations say what they wrote.
func ChangeFactsEnabled() bool {
	return changeFacts.Load()
}

// Change kinds.
const (
	ChangeAdd    = "add"
	ChangeUpdate = "update"
	ChangeDelete = "delete"
)

// Change is what one mutation wrote.
type Change struct {
	// Kind is ChangeAdd only when every root record is new: an add that found its
	// record already there (an upsert) is an update.
	Kind string `json:"kind,omitempty"`
	// Roots are the records of the mutated type that it made or matched.
	Roots []string `json:"roots,omitempty"`
	// New is every record it made, and NewTypes the dgraph.type values it gave them.
	New      []string `json:"new,omitempty"`
	NewTypes []string `json:"new_types,omitempty"`
	// Subjects is, for each record that was already there, the predicates written on it.
	Subjects map[string][]string `json:"subjects,omitempty"`
	// Objects is, for each record that was already there, the predicates of the edges
	// written that point at it.
	Objects map[string][]string `json:"objects,omitempty"`
	// Written is every predicate written, on any record.
	Written []string `json:"written,omitempty"`
	// Complete is false when it wrote to a record this could not name: then nothing
	// can be concluded from the lists above. Unnamed says what it could not name.
	Complete bool   `json:"complete,omitempty"`
	Unnamed  string `json:"unnamed,omitempty"`
}

// changeReader gathers a Change over the upserts of one mutation.
type changeReader struct {
	subjects map[string]map[string]struct{} // "0x1", "uid(v)" -> predicates
	objects  map[string]map[string]struct{}
	written  map[string]struct{}
	types    map[string]struct{}
	made     map[string]struct{}
	broken   string
}

// newChangeReader returns nil when nobody is listening, and every method is safe on nil.
func newChangeReader(m schema.Mutation) *changeReader {
	if !changeFacts.Load() || InvalidationFunc == nil || m.MutationType() == schema.DeleteMutation {
		return nil
	}
	return newReader()
}

func newReader() *changeReader {
	return &changeReader{
		subjects: make(map[string]map[string]struct{}),
		objects:  make(map[string]map[string]struct{}),
		written:  make(map[string]struct{}),
		types:    make(map[string]struct{}),
		made:     make(map[string]struct{}),
	}
}

func put(m map[string]map[string]struct{}, key, val string) {
	if m[key] == nil {
		m[key] = make(map[string]struct{})
	}
	m[key][val] = struct{}{}
}

// read takes in the mutations of one upsert, and the records the store made for it.
func (r *changeReader) read(muts []*dgoapi.Mutation, assigned map[string]string) {
	if r == nil {
		return
	}
	for _, uid := range assigned {
		r.made[uid] = struct{}{}
	}
	for _, m := range muts {
		if len(m.GetSetNquads()) > 0 || len(m.GetDelNquads()) > 0 || len(m.GetSet()) > 0 || len(m.GetDel()) > 0 {
			r.broken = "a mutation not written as JSON"
		}
		for _, doc := range []struct {
			json     []byte
			deleting bool
		}{{m.GetSetJson(), false}, {m.GetDeleteJson(), true}} {
			if len(doc.json) == 0 {
				continue
			}
			var v interface{}
			if err := json.Unmarshal(doc.json, &v); err != nil {
				r.broken = "a mutation that could not be read"
				continue
			}
			r.walk(v, doc.deleting)
		}
	}
}

// walk reads one JSON node of a mutation, or a list of them.
func (r *changeReader) walk(v interface{}, deleting bool) {
	switch node := v.(type) {
	case []interface{}:
		for _, item := range node {
			r.walk(item, deleting)
		}
	case map[string]interface{}:
		subject, _ := node["uid"].(string)
		if deleting && len(node) == 1 && subject != "" {
			r.broken = "the whole of " + subject + " deleted" // { "uid": ... } alone
		}
		for pred, val := range node {
			switch pred {
			case "uid":
				continue
			case "dgraph.type":
				r.written[pred] = struct{}{}
				switch t := val.(type) {
				case string:
					r.types[t] = struct{}{}
				case []interface{}:
					for _, s := range t {
						if name, ok := s.(string); ok {
							r.types[name] = struct{}{}
						}
					}
				}
				continue
			}
			r.written[pred] = struct{}{}
			if subject != "" {
				put(r.subjects, subject, pred)
			}
			r.edges(pred, val, deleting)
		}
	}
}

// edges reads the value of one predicate: the records it points at, and what is
// written on them in passing.
func (r *changeReader) edges(pred string, val interface{}, deleting bool) {
	switch o := val.(type) {
	case []interface{}:
		for _, item := range o {
			r.edges(pred, item, deleting)
		}
	case map[string]interface{}:
		if object, ok := o["uid"].(string); ok && object != "" {
			put(r.objects, object, pred)
			r.walk(o, false) // one key is not a delete of the whole record here
			return
		}
		if _, geo := o["type"]; geo && o["coordinates"] != nil {
			return // a point or a polygon, not a record
		}
		r.walk(o, false) // a record made in place, with no name of its own
	}
}

// change closes the reading: roots are the records of the mutated type it made or
// matched, and result the answers of the queries the upserts asked.
func (r *changeReader) change(m schema.Mutation, roots []string, result map[string]interface{}) *Change {
	if r == nil {
		return nil
	}
	return r.close(m.MutationType() == schema.AddMutation, roots, result)
}

func (r *changeReader) close(isAdd bool, roots []string, result map[string]interface{}) *Change {
	c := &Change{Kind: ChangeUpdate, Roots: roots, Complete: r.broken == "", Unnamed: r.broken,
		Subjects: make(map[string][]string), Objects: make(map[string][]string)}
	for uid := range r.made {
		c.New = append(c.New, uid)
	}
	if isAdd {
		c.Kind = ChangeAdd
		for _, root := range roots {
			if _, isNew := r.made[root]; !isNew {
				c.Kind = ChangeUpdate
			}
		}
	}
	// name resolves who a mutation node was: a record already there (its uids), a
	// record made here (nothing to add), or one that cannot be named.
	name := func(who string) ([]string, bool) {
		switch {
		case strings.HasPrefix(who, "0x"):
			if _, isNew := r.made[who]; isNew {
				return nil, true
			}
			return []string{who}, true
		case strings.HasPrefix(who, "_:"):
			return nil, true
		case strings.HasPrefix(who, "uid(") && strings.HasSuffix(who, ")"):
			v := who[len("uid(") : len(who)-1]
			if v == MutationQueryVar {
				return existing(roots, r.made), true
			}
			if _, asked := result[v]; asked {
				return existing(extractMutated(result, v), r.made), true
			}
		}
		return nil, false
	}
	fill := func(from map[string]map[string]struct{}, to map[string][]string) {
		for who, preds := range from {
			uids, ok := name(who)
			if !ok {
				c.Complete, c.Unnamed = false, who
				continue
			}
			for _, uid := range uids {
				for pred := range preds {
					to[uid] = append(to[uid], pred)
				}
			}
		}
		for uid := range to {
			sort.Strings(to[uid])
		}
	}
	fill(r.subjects, c.Subjects)
	fill(r.objects, c.Objects)
	for pred := range r.written {
		c.Written = append(c.Written, pred)
	}
	for t := range r.types {
		c.NewTypes = append(c.NewTypes, t)
	}
	sort.Strings(c.New)
	sort.Strings(c.Written)
	sort.Strings(c.NewTypes)
	return c
}

func existing(uids []string, made map[string]struct{}) []string {
	var out []string
	for _, uid := range uids {
		if _, isNew := made[uid]; !isNew {
			out = append(out, uid)
		}
	}
	return out
}
