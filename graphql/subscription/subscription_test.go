/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package subscription

import (
	"context"
	"testing"
	"time"
)

// MockField implements a simple schema.Field for AST traversing tests.
type MockField struct {
	name       string
	children   []*MockField
	predicates []string
	args       map[string]interface{}
}

func (m *MockField) Name() string                      { return m.name }
func (m *MockField) Alias() string                     { return m.name }
func (m *MockField) DgraphAlias() string               { return m.name }
func (m *MockField) ResponseName() string              { return m.name }
func (m *MockField) RemoteResponseName() string        { return m.name }
func (m *MockField) Skip() bool                        { return false }
func (m *MockField) Include() bool                     { return true }
func (m *MockField) SelectionSet() []interface{}       { return nil } // simplified for test
func (m *MockField) Arguments() map[string]interface{} { return m.args }
func (m *MockField) DgraphPredicate() string {
	if len(m.predicates) > 0 {
		return m.predicates[0]
	}
	return ""
}
func (m *MockField) Type() interface{} { return nil }

func TestDependencyRegistry(t *testing.T) {
	dr := NewDependencyRegistry()

	// 1. Verify Hex Regex
	if !hexUIDRegex.MatchString("0x1") {
		t.Error("hexUIDRegex failed to match 0x1")
	}
	if !hexUIDRegex.MatchString("0x3f9") {
		t.Error("hexUIDRegex failed to match 0x3f9")
	}
	if hexUIDRegex.MatchString("not-a-hex") {
		t.Error("hexUIDRegex incorrectly matched not-a-hex")
	}

	bucketID := uint64(1234)
	subID := uint64(5678)
	key := subKey{bucketID: bucketID, subID: subID}

	dr.Lock()
	dr.subUIDs[key] = make(map[string]struct{})
	dr.subTypes[key] = make(map[string]struct{})
	dr.subPredicates[key] = make(map[string]struct{})
	dr.Unlock()

	// 2. Test manual dependency registrations
	dr.Lock()
	dr.addUIDDep(key, "0x123")
	dr.addTypeDep(key, "User")
	dr.addPredicateDep(key, "User.name")
	dr.Unlock()

	// Verify Index structures
	dr.RLock()
	if _, ok := dr.uidIndex["0x123"][key]; !ok {
		t.Errorf("Expected UID 0x123 to map to subscriber key")
	}
	if _, ok := dr.typeIndex["User"][key]; !ok {
		t.Errorf("Expected Type User to map to subscriber key")
	}
	if _, ok := dr.predicateIndex["User.name"][key]; !ok {
		t.Errorf("Expected Predicate User.name to map to subscriber key")
	}
	dr.RUnlock()

	// 3. Test GetAffectedBuckets
	affected := dr.GetAffectedBuckets([]string{"0x123"}, nil, nil)
	if len(affected) != 1 || affected[0] != bucketID {
		t.Errorf("Expected affected bucket %d, got %v", bucketID, affected)
	}

	affectedTypes := dr.GetAffectedBuckets(nil, []string{"User"}, nil)
	if len(affectedTypes) != 1 || affectedTypes[0] != bucketID {
		t.Errorf("Expected affected bucket %d, got %v", bucketID, affectedTypes)
	}

	// 4. Test Resolved Entity Tracking parsing on response JSON
	mockResponse := []byte(`{
		"getUser": {
			"id": "0x123",
			"name": "John Doe",
			"address": {
				"uid": "0x999",
				"city": "San Francisco"
			}
		}
	}`)

	dr.UpdateResolvedEntities(bucketID, subID, mockResponse)

	dr.RLock()
	if _, ok := dr.uidIndex["0x999"][key]; !ok {
		t.Error("Expected resolved child UID 0x999 to be dynamically registered")
	}
	dr.RUnlock()

	// Verify GetAffectedBuckets matches newly resolved nested UID
	affectedNested := dr.GetAffectedBuckets([]string{"0x999"}, nil, nil)
	if len(affectedNested) != 1 || affectedNested[0] != bucketID {
		t.Errorf("Expected nested update to trigger re-evaluation for bucket %d", bucketID)
	}

	// 5. Test Deregister cleans up all indexes cleanly
	dr.Deregister(bucketID, subID)

	dr.RLock()
	if len(dr.uidIndex) != 0 || len(dr.typeIndex) != 0 || len(dr.predicateIndex) != 0 {
		t.Error("Expected clean indexes after Deregister, but some references remain")
	}
	dr.RUnlock()
}

func TestLocalBrokerDispatchesAsynchronously(t *testing.T) {
	broker := NewLocalBroker()

	received := make(chan *InvalidationMessage, 10)
	err := broker.Subscribe(context.Background(), func(msg *InvalidationMessage) {
		received <- msg
	})
	if err != nil {
		t.Fatalf("Failed to subscribe: %v", err)
	}

	testMsg := &InvalidationMessage{
		UIDs:     []string{"0x1a"},
		Types:    []string{"User"},
		CommitTs: 1002,
	}

	err = broker.Publish(context.Background(), testMsg)
	if err != nil {
		t.Fatalf("Failed to publish: %v", err)
	}

	select {
	case msg := <-received:
		if msg.CommitTs != 1002 || msg.UIDs[0] != "0x1a" {
			t.Errorf("Received incorrect message values: %v", msg)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("Timeout waiting for local broker async dispatch callback")
	}
}
