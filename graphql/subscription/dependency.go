/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package subscription

import (
	"encoding/json"
	"regexp"
	"sync"

	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
)

var hexUIDRegex = regexp.MustCompile(`^0x[0-9a-fA-F]+$`)

type subKey struct {
	bucketID uint64
	subID    uint64
}

// DependencyRegistry manages active subscription query dependencies.
type DependencyRegistry struct {
	sync.RWMutex
	uidIndex       map[string]map[subKey]struct{}
	typeIndex      map[string]map[subKey]struct{}
	predicateIndex map[string]map[subKey]struct{}

	// Reverse maps for fast cleanup on Deregister
	subUIDs       map[subKey]map[string]struct{}
	subTypes      map[subKey]map[string]struct{}
	subPredicates map[subKey]map[string]struct{}
}

// NewDependencyRegistry returns a new initialized DependencyRegistry.
func NewDependencyRegistry() *DependencyRegistry {
	return &DependencyRegistry{
		uidIndex:       make(map[string]map[subKey]struct{}),
		typeIndex:      make(map[string]map[subKey]struct{}),
		predicateIndex: make(map[string]map[subKey]struct{}),
		subUIDs:        make(map[subKey]map[string]struct{}),
		subTypes:       make(map[subKey]map[string]struct{}),
		subPredicates:  make(map[subKey]map[string]struct{}),
	}
}

// Register parses a GraphQL subscription operation and registers its initial dependencies.
func (dr *DependencyRegistry) Register(bucketID, subID uint64, op schema.Operation) {
	dr.Lock()
	defer dr.Unlock()

	key := subKey{bucketID: bucketID, subID: subID}

	// Initialize reverse maps
	dr.subUIDs[key] = make(map[string]struct{})
	dr.subTypes[key] = make(map[string]struct{})
	dr.subPredicates[key] = make(map[string]struct{})

	for _, q := range op.Queries() {
		dr.traverseField(key, q)
	}
}

func (dr *DependencyRegistry) traverseField(key subKey, f schema.Field) {
	if f == nil {
		return
	}

	// 1. Extract Type Dependency
	if fType := f.Type(); fType != nil {
		typeName := fType.Name()
		if typeName != "" {
			dr.addTypeDep(key, typeName)
		}
		if dgraphName := fType.DgraphName(); dgraphName != "" {
			dr.addTypeDep(key, dgraphName)
		}
	}

	// 2. Extract Predicate Dependency
	if predicate := f.DgraphPredicate(); predicate != "" {
		dr.addPredicateDep(key, predicate)
	}

	// 3. Extract UID dependencies from arguments (e.g. id: "0x1" or ids: ["0x1", "0x2"])
	for _, val := range f.Arguments() {
		dr.extractUIDsFromValue(key, val)
	}

	// 4. Traverse Selection Set Recursively
	for _, child := range f.SelectionSet() {
		dr.traverseField(key, child)
	}
}

func (dr *DependencyRegistry) addUIDDep(key subKey, uid string) {
	if !hexUIDRegex.MatchString(uid) {
		return
	}
	if _, ok := dr.uidIndex[uid]; !ok {
		dr.uidIndex[uid] = make(map[subKey]struct{})
	}
	dr.uidIndex[uid][key] = struct{}{}
	dr.subUIDs[key][uid] = struct{}{}
}

func (dr *DependencyRegistry) addTypeDep(key subKey, typeName string) {
	if typeName == "" {
		return
	}
	if _, ok := dr.typeIndex[typeName]; !ok {
		dr.typeIndex[typeName] = make(map[subKey]struct{})
	}
	dr.typeIndex[typeName][key] = struct{}{}
	dr.subTypes[key][typeName] = struct{}{}
}

func (dr *DependencyRegistry) addPredicateDep(key subKey, pred string) {
	if pred == "" {
		return
	}
	if _, ok := dr.predicateIndex[pred]; !ok {
		dr.predicateIndex[pred] = make(map[subKey]struct{})
	}
	dr.predicateIndex[pred][key] = struct{}{}
	dr.subPredicates[key][pred] = struct{}{}
}

func (dr *DependencyRegistry) extractUIDsFromValue(key subKey, val interface{}) {
	switch v := val.(type) {
	case string:
		dr.addUIDDep(key, v)
	case []interface{}:
		for _, item := range v {
			dr.extractUIDsFromValue(key, item)
		}
	case map[string]interface{}:
		for _, item := range v {
			dr.extractUIDsFromValue(key, item)
		}
	}
}

// Deregister cleans up all dependencies registered for a subscriber.
func (dr *DependencyRegistry) Deregister(bucketID, subID uint64) {
	dr.Lock()
	defer dr.Unlock()

	key := subKey{bucketID: bucketID, subID: subID}

	// 1. Clean up UIDs index
	if uids, ok := dr.subUIDs[key]; ok {
		for uid := range uids {
			if subs, ok2 := dr.uidIndex[uid]; ok2 {
				delete(subs, key)
				if len(subs) == 0 {
					delete(dr.uidIndex, uid)
				}
			}
		}
		delete(dr.subUIDs, key)
	}

	// 2. Clean up Types index
	if types, ok := dr.subTypes[key]; ok {
		for typeName := range types {
			if subs, ok2 := dr.typeIndex[typeName]; ok2 {
				delete(subs, key)
				if len(subs) == 0 {
					delete(dr.typeIndex, typeName)
				}
			}
		}
		delete(dr.subTypes, key)
	}

	// 3. Clean up Predicates index
	if preds, ok := dr.subPredicates[key]; ok {
		for pred := range preds {
			if subs, ok2 := dr.predicateIndex[pred]; ok2 {
				delete(subs, key)
				if len(subs) == 0 {
					delete(dr.predicateIndex, pred)
				}
			}
		}
		delete(dr.subPredicates, key)
	}
}

// UpdateResolvedEntities parses query response payload to dynamically track nested entity UIDs.
func (dr *DependencyRegistry) UpdateResolvedEntities(bucketID, subID uint64, responseJSON []byte) {
	var payload interface{}
	if err := json.Unmarshal(responseJSON, &payload); err != nil {
		return
	}

	dr.Lock()
	defer dr.Unlock()

	key := subKey{bucketID: bucketID, subID: subID}
	if _, ok := dr.subUIDs[key]; !ok {
		return // Subscriber no longer exists
	}

	dr.traverseJSON(key, payload)
}

func (dr *DependencyRegistry) traverseJSON(key subKey, val interface{}) {
	switch v := val.(type) {
	case string:
		dr.addUIDDep(key, v)
	case []interface{}:
		for _, item := range v {
			dr.traverseJSON(key, item)
		}
	case map[string]interface{}:
		for k, item := range v {
			// standard fields indicating UID in GraphQL
			if k == "id" || k == "uid" {
				if str, ok := item.(string); ok {
					dr.addUIDDep(key, str)
				}
			}
			dr.traverseJSON(key, item)
		}
	}
}

// GetAffectedBuckets maps mutated UIDs, Types, and Predicates to active subscription bucket IDs.
func (dr *DependencyRegistry) GetAffectedBuckets(mutatedUIDs, mutatedTypes, mutatedPredicates []string) []uint64 {
	dr.RLock()
	defer dr.RUnlock()

	affectedBuckets := make(map[uint64]struct{})

	// helper to add matching bucketIDs
	addMatching := func(subs map[subKey]struct{}) {
		for sub := range subs {
			affectedBuckets[sub.bucketID] = struct{}{}
		}
	}

	// 1. Check UIDs
	for _, uid := range mutatedUIDs {
		if subs, ok := dr.uidIndex[uid]; ok {
			addMatching(subs)
		}
	}

	// 2. Check Types
	for _, typeName := range mutatedTypes {
		if subs, ok := dr.typeIndex[typeName]; ok {
			addMatching(subs)
		}
	}

	// 3. Check Predicates
	for _, pred := range mutatedPredicates {
		if subs, ok := dr.predicateIndex[pred]; ok {
			addMatching(subs)
		}
	}

	res := make([]uint64, 0, len(affectedBuckets))
	for bid := range affectedBuckets {
		res = append(res, bid)
	}
	return res
}
