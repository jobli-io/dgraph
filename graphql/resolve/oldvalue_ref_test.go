/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

// Tests for rewriteObject handling of objects that carry Dgraph-internal DQL
// fields (uid, dgraph.type, __typename) alongside their @id / ID fields.
//
// This scenario arises when a @transform expression forwards a before/after
// sub-object that was returned by a Phase-1 @oldValue DQL query, e.g.:
//
//   forJobBoard: #.forJobBoard  ->  {id:"0x1f4cf2", uid:"0x1f4cf2"}
//   inGroup: after.hasPrimaryGroup ->  {sId:"...", uid:"0x1f4c87", xId:"..."}
//
// Both are pure references to existing Dgraph nodes.  The engine must accept
// such references without requiring the UID to be pre-registered in idExistence
// (Phase-1 did not know the transform would emit it) and without trying to
// create a new node (which would fail EnsureNonNulls on required fields).
//
// Because "uid" is not a valid GraphQL input field, we test by calling
// rewriteObject directly with hand-crafted obj maps — exactly as happens when
// a @transform emits a fetched sub-object at Phase-2 time.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/graphql/test"
)

// getTestMutatedType loads the named type from the resolve package's test
// schema by issuing a dummy mutation and calling MutatedType() on it.
//
// typeName must be the name of a type whose generated addXxx mutation exists
// in the test schema (schema.graphql).
func getTestMutatedType(t *testing.T, mutStr string, typeName string) schema.Type {
	t.Helper()
	gqlSchema := test.LoadSchemaFromFile(t, "schema.graphql")
	op, err := gqlSchema.Operation(&schema.Request{Query: mutStr})
	require.NoError(t, err, "invalid mutation for schema type %s", typeName)
	mut := test.GetMutation(t, op)
	typ := mut.MutatedType()
	require.NotNil(t, typ, "MutatedType must not be nil for %s", typeName)
	return typ
}

// TestIsDgraphInternalField verifies the helper correctly classifies keys.
func TestIsDgraphInternalField(t *testing.T) {
	internal := []string{"uid", "dgraph.type", "__typename"}
	for _, k := range internal {
		require.True(t, isDgraphInternalField(k), "expected %q to be DQL-internal", k)
	}

	notInternal := []string{"id", "name", "sId", "xId", "code", "title", "uid2", "dgraph"}
	for _, k := range notInternal {
		require.False(t, isDgraphInternalField(k), "expected %q NOT to be DQL-internal", k)
	}
}

// TestOldValueUIDReference_DirectCall verifies Fix 3:
// when rewriteObject receives {id:"0x5", uid:"0x5"} for a UID-type reference
// (Country) that is NOT in idExistence, the matching "uid" key is treated as
// an implicit existence proof and the object is linked without error.
func TestOldValueUIDReference_DirectCall(t *testing.T) {
	// Dummy mutation just to get Country as a schema.Type.
	countryTyp := getTestMutatedType(t,
		`mutation { addCountry(input:[{name:"X"}]) { country { id } } }`,
		"Country")

	varGen := NewVariableGenerator()
	xidMetadata := NewXidMetadata()
	// idExistence is intentionally empty — the UID "0x5" was NOT pre-fetched in
	// Phase-1 because a @transform injected it at Phase-2 time.
	idExistence := map[string]string{}

	// Simulate what a @transform emits when it forwards
	//   before.hasPortalForm[0].forJobBoard = {uid:"0x5"}
	// which Phase-1 fetches as: { uid, id: uid }
	obj := map[string]interface{}{
		"id":  "0x5",
		"uid": "0x5",
	}

	frag, _, errs := rewriteObject(
		context.Background(),
		countryTyp,
		nil, // srcField — top-level
		"",  // srcUID
		varGen,
		obj,
		xidMetadata,
		idExistence,
		Add,
		nil, // authVariables
		nil, // objDel
	)

	require.Empty(t, errs, "rewriteObject must not error when uid matches id field")

	fragMap, ok := frag.fragment.(map[string]interface{})
	require.True(t, ok)

	// The fragment must reference the existing UID, not create a blank node.
	require.Equal(t, "0x5", fragMap["uid"], "fragment must use existing UID 0x5")

	// idExistence must be seeded so further references use the fast path.
	require.Equal(t, "0x5", idExistence["Country_1"],
		"idExistence must be seeded with the Dgraph-fetched UID")
}

// TestOldValueXIDRefWithDgraphUID_DirectCall verifies Fix 1 + 2:
// an XID object {code:"AK", uid:"0x7"} must be detected as ref-only
// (uid is a Dgraph-internal field → skipped in isRefOnly check) and
// the engine must emit {"uid":"0x7"} rather than a blank-node forward-ref
// or triggering EnsureNonNulls (which would fail on name: String!).
func TestOldValueXIDRefWithDgraphUID_DirectCall(t *testing.T) {
	// State has: code String! @id, name String! (required), country Country.
	// Without the fix, "uid" breaks isRefOnly → EnsureNonNulls fails on name.
	// With the fix, isRefOnly=true, uid="0x7" → link to existing State.
	stateTyp := getTestMutatedType(t,
		`mutation { addState(input:[{code:"AK", name:"Alaska", capital:"Juneau"}]) { state { code } } }`,
		"State")

	varGen := NewVariableGenerator()
	xidMetadata := NewXidMetadata()
	idExistence := map[string]string{}

	// Simulates what a @transform emits when it forwards
	//   after.hasPrimaryGroup fetched as { uid, code: State.code }
	obj := map[string]interface{}{
		"code": "AK",
		"uid":  "0x7",
	}

	frag, _, errs := rewriteObject(
		context.Background(),
		stateTyp,
		nil,
		"",
		varGen,
		obj,
		xidMetadata,
		idExistence,
		Add,
		nil,
		nil,
	)

	require.Empty(t, errs,
		"uid field must not break isRefOnly; EnsureNonNulls must not be called on name")

	fragMap, ok := frag.fragment.(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "0x7", fragMap["uid"],
		"fragment must reference existing Dgraph UID 0x7 from the uid key")
	require.Equal(t, "0x7", idExistence["State_1"])
}

// TestOldValueXIDRef_WithDgraphTypeAndUID_DirectCall verifies that having BOTH
// "uid" and "dgraph.type" alongside an @id field still yields isRefOnly=true.
func TestOldValueXIDRef_WithDgraphTypeAndUID_DirectCall(t *testing.T) {
	stateTyp := getTestMutatedType(t,
		`mutation { addState(input:[{code:"TX", name:"Texas", capital:"Austin"}]) { state { code } } }`,
		"State")

	varGen := NewVariableGenerator()
	xidMetadata := NewXidMetadata()
	idExistence := map[string]string{}

	// Both uid and dgraph.type arrive from a DQL result (@oldValue query).
	obj := map[string]interface{}{
		"code":        "TX",
		"uid":         "0x9",
		"dgraph.type": []interface{}{"State"},
	}

	frag, _, errs := rewriteObject(
		context.Background(),
		stateTyp,
		nil,
		"",
		varGen,
		obj,
		xidMetadata,
		idExistence,
		Add,
		nil,
		nil,
	)

	require.Empty(t, errs,
		"dgraph.type must not break isRefOnly; engine must use existing UID 0x9")
	fragMap, ok := frag.fragment.(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "0x9", fragMap["uid"])
}

// TestOldValueXIDRef_RefOnlyWithoutUID_EnsureNonNulls verifies that a ref-only
// XID object with no uid field AND no required non-XID fields correctly errors
// via EnsureNonNulls rather than silently emitting a blank-node forward-ref.
//
// Only objects that carry a raw Dgraph "uid" field (fetched by a @transform /
// @oldValue DQL query) bypass EnsureNonNulls via asIDReference.  A plain
// {code:"CA"} with no uid is treated as an incomplete node definition — the
// required "name" field is missing, so the engine must return an error.
func TestOldValueXIDRef_RefOnlyWithoutUID_EnsureNonNulls(t *testing.T) {
	stateTyp := getTestMutatedType(t,
		`mutation { addState(input:[{code:"CA", name:"California", capital:"Sacramento"}]) { state { code } } }`,
		"State")

	varGen := NewVariableGenerator()
	xidMetadata := NewXidMetadata()
	idExistence := map[string]string{}

	// Pure @id reference — no uid field, no name (required non-XID field).
	obj := map[string]interface{}{
		"code": "CA",
	}

	frag, _, errs := rewriteObject(
		context.Background(),
		stateTyp,
		nil,
		"",
		varGen,
		obj,
		xidMetadata,
		idExistence,
		Add,
		nil,
		nil,
	)

	// {code:"CA"} with no uid falls through to EnsureNonNulls (case a3).
	// name: String! is required but absent → error.
	require.NotEmpty(t, errs,
		"ref-only XID object without uid and missing required field must error via EnsureNonNulls")
	require.Contains(t, errs[0].Error(), "name",
		"error must mention the missing required field 'name'")
	require.Nil(t, frag, "fragment must be nil when EnsureNonNulls fails")
}
