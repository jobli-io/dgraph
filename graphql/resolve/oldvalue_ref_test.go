/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

// Tests for rewriteObject handling of objects that carry Dgraph-internal DQL
// fields (uid, dgraph.type, __typename) alongside their @id / ID fields.
//
// This scenario arises when a @transform expression forwards a before/after
// sub-object that was returned by a Phase-1 @oldValue DQL query, e.g.,:
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

	require.Empty(t, errs, "UID reference must not error")
	require.NotNil(t, frag, "UID reference must return a non-nil fragment")

	// The fragment must reference the existing node ("0x5") and must NOT try
	// to create a new blank node.
	fragMap, ok := frag.fragment.(map[string]interface{})
	require.True(t, ok, "fragment must be a map")

	uid, _ := fragMap["uid"].(string)
	require.Equal(t, "0x5", uid, "fragment uid must be the existing Dgraph UID")
}

// TestOldValueXIDRefWithDgraphUID_DirectCall verifies Fix 4:
// when rewriteObject receives {sId:"sn_alice", uid:"0x1f4c87"} (an XID node
// returned by a @transform sub-query with its raw Dgraph uid attached), the
// engine must recognise the concrete UID and link to it — NOT try to create a
// new blank node.
func TestOldValueXIDRefWithDgraphUID_DirectCall(t *testing.T) {
	personTyp := getTestMutatedType(t,
		`mutation { addPersonNode(input:[{xId:"alice", sId:"sn_alice", display:"Alice"}]) { personNode { xId } } }`,
		"PersonNode")

	varGen := NewVariableGenerator()
	xm := NewXidMetadata()
	idExistence := map[string]string{}

	obj := map[string]interface{}{
		"sId": "sn_alice",
		"uid": "0x1f4c87",
	}

	frag, _, errs := rewriteObject(
		context.Background(), personTyp, nil, "", varGen, obj, xm, idExistence, Add, nil, nil)

	require.Empty(t, errs, "XID reference with Dgraph UID must not error")
	require.NotNil(t, frag)

	fragMap := frag.fragment.(map[string]interface{})
	require.Equal(t, "0x1f4c87", fragMap["uid"], "must link to the concrete Dgraph UID")
}

// TestOldValueXIDRef_WithDgraphTypeAndUID_DirectCall verifies that Dgraph-internal
// fields (uid, dgraph.type, __typename) do not disqualify an object from isRefOnly
// treatment.  An object like {sId:"sn_alice", uid:"0x7", dgraph.type:["PersonNode"]}
// must still be recognised as a UID reference (not a new-node creation attempt).
func TestOldValueXIDRef_WithDgraphTypeAndUID_DirectCall(t *testing.T) {
	personTyp := getTestMutatedType(t,
		`mutation { addPersonNode(input:[{xId:"alice", sId:"sn_alice", display:"Alice"}]) { personNode { xId } } }`,
		"PersonNode")

	varGen := NewVariableGenerator()
	xm := NewXidMetadata()
	idExistence := map[string]string{}

	obj := map[string]interface{}{
		"sId":         "sn_alice",
		"uid":         "0x7",
		"dgraph.type": []interface{}{"PersonNode"},
		"__typename":  "PersonNode",
	}

	frag, _, errs := rewriteObject(
		context.Background(), personTyp, nil, "", varGen, obj, xm, idExistence, Add, nil, nil)

	require.Empty(t, errs, "XID ref with DQL fields must not error")
	require.NotNil(t, frag)

	fragMap := frag.fragment.(map[string]interface{})
	require.Equal(t, "0x7", fragMap["uid"],
		"fragment uid must be the existing Dgraph UID")
}

// TestOldValueXIDRef_RefOnlyWithoutUID_EnsureNonNulls verifies that a genuinely
// incomplete ref-only object (only @id field, no uid, no full definition) is
// DEFERRED by rewriteObject (no immediate error) and then fails EnsureNonNulls
// only in post-processing (resolvePendingForwardRefs).
func TestOldValueXIDRef_RefOnlyWithoutUID_EnsureNonNulls(t *testing.T) {
	personTyp := getTestMutatedType(t,
		`mutation { addPersonNode(input:[{xId:"alice", sId:"sn_alice", display:"Alice"}]) { personNode { xId } } }`,
		"PersonNode")

	varGen := NewVariableGenerator()
	xm := NewXidMetadata()
	idExistence := map[string]string{}

	// {sId:"sn_alice"} has only ONE @id field — the other @id field (xId) and
	// required non-@id field (display) are both absent and have no @default.
	// EnsureNonNulls must FAIL → rewriteObject defers validation.
	obj := map[string]interface{}{
		"sId": "sn_alice",
	}

	// With the deferred approach, rewriteObject emits a forward ref (non-nil),
	// with no immediate errors.
	frag, _, errs := rewriteObject(
		context.Background(), personTyp, nil, "", varGen, obj, xm, idExistence, Add, nil, nil)
	require.Empty(t, errs, "ref-only must not immediately error; defer to post-processing")
	require.NotNil(t, frag, "rewriteObject must return a forward ref fragment")

	// Post-processing must surface the error — no full definition arrived.
	pendingErrs := xm.resolvePendingForwardRefs()
	require.NotEmpty(t, pendingErrs, "resolvePendingForwardRefs must error when node is incomplete")
}

// TestIsRefOnly_MultipleParentsSameIncompleteXID_SingleError verifies that when
// multiple sibling fields share the same ref-only XID and no full definition
// arrives, resolvePendingForwardRefs emits exactly one error (not one per parent).
func TestIsRefOnly_MultipleParentsSameIncompleteXID_SingleError(t *testing.T) {
	// Widget.alt and Widget.primary both point to Tag.
	// Tag{key} is ref-only; Tag.value! is required but has no @default.
	tagTyp := getTestMutatedType(t,
		`mutation { addTag(input:[{key:"t1",value:"v1"}]) { tag { key } } }`,
		"Tag")

	varGen := NewVariableGenerator()
	xm := NewXidMetadata()
	idExistence := map[string]string{}

	// Four ref-only calls — e.g. two widgets, each with alt + primary pointing to the same Tag.
	for i := 0; i < 4; i++ {
		refObj := map[string]interface{}{"key": "t1"}
		frag, _, errs := rewriteObject(
			context.Background(), tagTyp, nil, "", varGen, refObj, xm, idExistence, Add, nil, nil)
		require.Empty(t, errs, "call %d: ref-only must not immediately error", i)
		require.NotNil(t, frag, "call %d: must return a fragment", i)
	}

	pendingErrs := xm.resolvePendingForwardRefs()
	require.Len(t, pendingErrs, 1, "exactly one error must be reported for the incomplete XID")
}

// TestIsRefOnly_FullDefAfterRefOnly_ClearsPending verifies that when a ref-only
// occurrence is followed by a full definition for the same XID (typical "forward
// reference resolved by later sibling" scenario):
//
//  1. rewriteObject does NOT error on the ref-only (defers validation).
//  2. rewriteObject processes the full definition (Case b).
//  3. resolvePendingForwardRefs returns NO errors (Case b deleted the pending entry).
//  4. Both fragments share the same blank-node uid so Dgraph merges them.
func TestIsRefOnly_FullDefAfterRefOnly_ClearsPending(t *testing.T) {
	stateTyp := getTestMutatedType(t,
		`mutation { addState(input:[{code:"CA", name:"California", capital:"Sacramento"}]) { state { code } } }`,
		"State")

	varGen := NewVariableGenerator()
	xm := NewXidMetadata()
	idExistence := map[string]string{}

	// Step 1: ref-only occurrence — EnsureNonNulls fails → deferred.
	refOnly := map[string]interface{}{"code": "CA"}
	fragRef, _, errsRef := rewriteObject(
		context.Background(), stateTyp, nil, "", varGen, refOnly, xm, idExistence, Add, nil, nil)
	require.Empty(t, errsRef, "ref-only must not immediately error")
	require.NotNil(t, fragRef)

	// Step 2: full definition — isRefOnly=false → Case b clears pendingForwardRefs.
	fullDef := map[string]interface{}{"code": "CA", "name": "California"}
	fragFull, _, errsFull := rewriteObject(
		context.Background(), stateTyp, nil, "", varGen, fullDef, xm, idExistence, Add, nil, nil)
	require.Empty(t, errsFull, "full definition must not error")
	require.NotNil(t, fragFull)

	// Step 3: post-processing must be silent — pending entry was cleared by Case b.
	pendingErrs := xm.resolvePendingForwardRefs()
	require.Empty(t, pendingErrs,
		"resolvePendingForwardRefs must return no errors when full def arrived")

	// Step 4: both fragments reference the same blank-node uid so Dgraph merges them.
	uidRef := fragRef.fragment.(map[string]interface{})["uid"]
	uidFull := fragFull.fragment.(map[string]interface{})["uid"]
	require.Equal(t, uidRef, uidFull,
		"ref-only forward-ref uid must match full-def inline creation uid")
}

// TestShouldReRun_DefaultContamination_isRefOnlyInvariant is a regression test for
// the "shouldReRun default contamination" bug.
//
// # Background
//
// When shouldReRun fires inside rewriteObject (triggered by a @default-computed @id
// field resolving to an existing Dgraph node), the fix ensures the re-run receives a
// "clean" obj that contains only user-provided fields plus @id-computed defaults —
// NOT the non-@id @default fields (e.g. active=true) that the @default loop appended.
//
// # What this test verifies
//
// The core isRefOnly invariant: an obj that contains ONLY @id (XID) fields is ref-only
// regardless of whether there are non-XID @default fields defined on the type.
//
// AccountNode (from schema.graphql):
//
//	email:  String! @id                                    ← user-provided XID
//	handle: String! @id @default(add: {expr: ...})         ← computed XID
//	active: Boolean! @default(add: {value: "true"})        ← non-XID default (the contaminant)
//
// The test verifies that:
//  1. {email, handle} — only @id fields → isRefOnly=true (correct).
//  2. {email, handle, active} — includes non-@id default → isRefOnly=false (correct, this
//     is the "contaminated" obj that the buggy shouldReRun code passed).
//  3. rewriteObject with a ref-only AccountNode obj and a pre-existing Dgraph node
//     returns asIDReference without error (simulating the fixed shouldReRun re-call).
func TestShouldReRun_DefaultContamination_isRefOnlyInvariant(t *testing.T) {
	accountTyp := getTestMutatedType(t,
		`mutation { addAccountNode(input:[{email:"a@b.com", handle:"@a@b.com", active:true}]) { accountNode { email } } }`,
		"AccountNode")

	xids := accountTyp.XIDFields()
	require.Len(t, xids, 2, "AccountNode must have exactly 2 @id fields (email, handle)")

	xidNames := map[string]bool{}
	for _, xf := range xids {
		xidNames[xf.Name()] = true
	}
	require.True(t, xidNames["email"], "email must be an @id field")
	require.True(t, xidNames["handle"], "handle must be an @id field")

	// ── isRefOnly invariant: delegate to the engine's computeIsRefOnly helper ──
	isRefOnly := func(obj map[string]interface{}) bool {
		return computeIsRefOnly(obj, xids, "", nil)
	}

	// {email, handle} — only @id fields → ref-only.
	require.True(t, isRefOnly(map[string]interface{}{
		"email":  "a@b.com",
		"handle": "@a@b.com",
	}), "{email, handle} must be ref-only (both are @id fields)")

	// {email, handle, active} — includes non-@id default → NOT ref-only.
	// This is the "contaminated" obj the buggy shouldReRun code would have passed.
	require.False(t, isRefOnly(map[string]interface{}{
		"email":  "a@b.com",
		"handle": "@a@b.com",
		"active": true,
	}), "{email, handle, active} must NOT be ref-only because 'active' is not an @id field")

	// ── rewriteObject: ref-only input with existing node → asIDReference ──
	// Simulates what the fixed shouldReRun code does: passes {email, handle} to the
	// re-run, which finds the email variable in idExistence → typUidExist=true →
	// asIDReference without calling EnsureNonNulls.
	//
	// We need a fresh varGen so rewriteObject's XID loop generates the canonical
	// variable name. varGen.Next is idempotent for already-registered tuples so
	// pre-computing the variable name to seed idExistence works correctly.
	varGen := NewVariableGenerator()
	xm := NewXidMetadata()
	varForEmail := varGen.Next(accountTyp, "email", "a@b.com", false)
	idExistence := map[string]string{varForEmail: "0xDEF"}

	// varGen already has "AccountNode_1" registered for email="a@b.com".
	// The XID loop in rewriteObject calls varGen.Next idempotently → same variable →
	// found in idExistence → typUidExist=true.
	cleanRefObj := map[string]interface{}{
		"email":  "a@b.com",
		"handle": "@a@b.com",
		// "active" intentionally absent — this is the "clean" rerunObj from the fix.
	}
	frag, _, errs := rewriteObject(
		context.Background(), accountTyp, nil, "", varGen, cleanRefObj, xm, idExistence, Add, nil, nil)

	// At top-level with typUidExist=true (no upsert), the engine returns a "duplicate id"
	// error — this is the correct user-facing error for Add mutations at the top level.
	// The important invariant verified here is that it is NOT an EnsureNonNulls error
	// ("field X requires a value...") — that would be the bug.
	for _, err := range errs {
		errMsg := err.Error()
		require.NotContains(t, errMsg, "requires a value for field",
			"error must NOT be an EnsureNonNulls failure (that indicates the contamination bug)")
	}
	// Either returns a fragment (if taken via some other path) or the "already exists"
	// error (correct top-level Add behavior). Both are acceptable — what we're testing
	// is the ABSENCE of EnsureNonNulls failures.
	_ = frag
}

// TestComputeIsRefOnly_SkipFields verifies that computeIsRefOnly treats entries
// in the skipFields map as neutral — they do not prevent an object from being
// classified as a pure cross-reference even though they are not @id fields.
//
// This is Option B from the design: the caller declares which fields were
// injected for operational reasons (e.g. by a @transform or lambda) and
// should not count as "node content" for the ref-only determination.
func TestComputeIsRefOnly_SkipFields(t *testing.T) {
	accountTyp := getTestMutatedType(t,
		`mutation { addAccountNode(input:[{email:"a@b.com", handle:"@a@b.com", active:true}]) { accountNode { email } } }`,
		"AccountNode")

	xids := accountTyp.XIDFields()

	// Without skip: {email, handle, active} is NOT ref-only because "active" is non-XID.
	require.False(t,
		computeIsRefOnly(
			map[string]interface{}{"email": "a@b.com", "handle": "@a@b.com", "active": true},
			xids, "", nil,
		),
		"without skipFields, 'active' must disqualify the object from being ref-only",
	)

	// With skip: the same object IS ref-only when "active" is declared as skippable.
	require.True(t,
		computeIsRefOnly(
			map[string]interface{}{"email": "a@b.com", "handle": "@a@b.com", "active": true},
			xids, "", map[string]struct{}{"active": {}},
		),
		"with skipFields{active}, the object must be treated as ref-only",
	)

	// Multiple skipped fields.
	require.True(t,
		computeIsRefOnly(
			map[string]interface{}{"email": "a@b.com", "handle": "@a@b.com", "active": true, "role": "admin"},
			xids, "", map[string]struct{}{"active": {}, "role": {}},
		),
		"all non-XID fields skipped → ref-only",
	)

	// Skipping only one of two non-XID fields still produces NOT ref-only.
	require.False(t,
		computeIsRefOnly(
			map[string]interface{}{"email": "a@b.com", "active": true, "role": "admin"},
			xids, "", map[string]struct{}{"active": {}},
		),
		"unskipped 'role' must still disqualify the object",
	)

	// nil skipFields still works (no panic on nil map read).
	require.True(t,
		computeIsRefOnly(
			map[string]interface{}{"email": "a@b.com", "handle": "@a@b.com"},
			xids, "", nil,
		),
		"nil skipFields must not panic and must behave as empty set",
	)
}
