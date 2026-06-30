/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

// Unit tests for NewExprEvaluationContext and diffMapInterface.
//
// These functions compute the before/after/new/input/rawInput variables
// that are exposed to every @default, @transform, @validate, and
// @postValidate expr-lang expression. The semantics are:
//
//   after     = merge(before, input)            — projected state after mutation
//   new       = diffMapInterface(before, input) — fields that changed / were added
//   input     = partially-defaulted mutation object
//   rawInput  = original user payload (pre-default), fallback to input when nil
//   before    = pre-mutation state from the @oldValue query
//
// For add mutations before is nil/empty, so new == clone(input).
// For update mutations new contains only the fields that actually changed.

import (
	"testing"

	"github.com/hypermodeinc/dgraph/v25/x"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// sm (string-map) builds a map[string]interface{} from key-value pairs.
func sm(pairs ...interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i].(string)] = pairs[i+1]
	}
	return out
}

// mkCtx is a convenience wrapper around NewExprEvaluationContext.
func mkCtx(input, before, rawInput map[string]interface{}) exprEvaluationContext {
	return NewExprEvaluationContext("TestType", input, before, nil, AuthCtx{RawInput: rawInput}, "update")
}

// ── NewExprEvaluationContext ──────────────────────────────────────────────────

func TestExprContext_After_MergesBeforeAndInput(t *testing.T) {
	// after = before merged with input; input wins on conflict.
	before := sm("name", "Alice", "age", 30)
	input := sm("name", "Bob", "email", "bob@example.com")

	ec := mkCtx(input, before, nil)

	assert.Equal(t, "Bob", ec.After["name"], "input must override before")
	assert.Equal(t, 30, ec.After["age"], "before-only key must be preserved")
	assert.Equal(t, "bob@example.com", ec.After["email"], "input-only key must appear in after")
}

func TestExprContext_After_EqualsInputWhenBeforeNil(t *testing.T) {
	// add mutation: before is nil → after == input
	input := sm("title", "Hello", "count", 1)

	ec := mkCtx(input, nil, nil)

	assert.Equal(t, "Hello", ec.After["title"])
	assert.Equal(t, 1, ec.After["count"])
	assert.Len(t, ec.After, 2)
}

func TestExprContext_After_DoesNotMutateSourceMaps(t *testing.T) {
	// after is a fresh map; mutating it must not affect input or before.
	before := sm("x", 1)
	input := sm("y", 2)

	ec := mkCtx(input, before, nil)
	ec.After["z"] = 99

	assert.NotContains(t, input, "z", "mutating after must not affect input")
	assert.NotContains(t, before, "z", "mutating after must not affect before")
}

func TestExprContext_New_AddMutation_AllInputFields(t *testing.T) {
	// add mutation (before nil): every input field is new.
	input := sm("name", "Alice", "role", "admin")

	ec := mkCtx(input, nil, nil)

	assert.Equal(t, "Alice", ec.New["name"])
	assert.Equal(t, "admin", ec.New["role"])
	assert.Len(t, ec.New, 2)
}

func TestExprContext_New_UpdateMutation_OnlyChangedFields(t *testing.T) {
	// update: only the field whose value changed appears in new.
	before := sm("name", "Alice", "age", 30, "role", "user")
	input := sm("name", "Alice", "age", 31) // name unchanged, age changed, role absent

	ec := mkCtx(input, before, nil)

	assert.NotContains(t, ec.New, "name", "unchanged field must not be in new")
	assert.Equal(t, 31, ec.New["age"], "changed field must be in new with new value")
	assert.NotContains(t, ec.New, "role", "before-only removed key must not be in new")
}

func TestExprContext_New_UpdateMutation_AddedField(t *testing.T) {
	// A field in input that was absent in before counts as new.
	before := sm("name", "Alice")
	input := sm("name", "Alice", "email", "alice@example.com")

	ec := mkCtx(input, before, nil)

	assert.Equal(t, "alice@example.com", ec.New["email"])
	assert.NotContains(t, ec.New, "name")
}

func TestExprContext_New_EmptyWhenNothingChanged(t *testing.T) {
	before := sm("x", 1, "y", "hello")
	input := sm("x", 1, "y", "hello")

	ec := mkCtx(input, before, nil)

	assert.Empty(t, ec.New)
}

func TestExprContext_New_EmptyWhenInputNil(t *testing.T) {
	ec := mkCtx(nil, sm("x", 1), nil)
	assert.Empty(t, ec.New)
}

func TestExprContext_Before_PassedThrough(t *testing.T) {
	before := sm("status", "ACTIVE", "score", 42)

	ec := mkCtx(sm("score", 99), before, nil)

	assert.Equal(t, "ACTIVE", ec.Before["status"])
	assert.Equal(t, 42, ec.Before["score"], "before holds pre-mutation values unchanged")
}

func TestExprContext_Before_NilForAddMutation(t *testing.T) {
	ec := mkCtx(sm("title", "T"), nil, nil)
	assert.Nil(t, ec.Before)
}

func TestExprContext_Input_PassedThrough(t *testing.T) {
	input := sm("a", 1, "b", "two")

	ec := mkCtx(input, nil, nil)

	assert.Equal(t, 1, ec.Input["a"])
	assert.Equal(t, "two", ec.Input["b"])
}

func TestExprContext_RawInput_UsesAuthCtxRawInput(t *testing.T) {
	// When AuthCtx.RawInput is set it represents the original user payload
	// before any @default expressions populated additional fields.
	input := sm("title", "defaulted-title", "score", 10) // score added by @default
	raw := sm("title", "original-title")                 // only what the user sent

	ec := mkCtx(input, nil, raw)

	assert.Equal(t, "original-title", ec.RawInput["title"])
	assert.NotContains(t, ec.RawInput, "score",
		"@default-computed field must not appear in rawInput")
}

func TestExprContext_RawInput_FallsBackToInput(t *testing.T) {
	// When AuthCtx.RawInput is nil, rawInput == input.
	input := sm("x", 1)

	ec := mkCtx(input, nil, nil)

	assert.Equal(t, ec.Input, ec.RawInput)
}

func TestExprContext_Action_SetCorrectly(t *testing.T) {
	ec := NewExprEvaluationContext("T", sm("x", 1), nil, nil, AuthCtx{}, "add")
	assert.Equal(t, "add", ec.Action)

	ec2 := NewExprEvaluationContext("T", sm("x", 1), nil, nil, AuthCtx{}, "update")
	assert.Equal(t, "update", ec2.Action)
}

// ── diffMapInterface ──────────────────────────────────────────────────────────

func TestDiffMap_BothNil(t *testing.T) {
	diff, err := diffMapInterface(nil, nil)
	require.NoError(t, err)
	assert.Empty(t, diff)
}

func TestDiffMap_Obj1Nil_AllObj2IsNew(t *testing.T) {
	diff, err := diffMapInterface(nil, sm("a", 1, "b", "two"))
	require.NoError(t, err)
	assert.Equal(t, 1, diff["a"])
	assert.Equal(t, "two", diff["b"])
}

func TestDiffMap_Obj2Nil_EmptyDiff(t *testing.T) {
	diff, err := diffMapInterface(sm("a", 1), nil)
	require.NoError(t, err)
	assert.Empty(t, diff)
}

func TestDiffMap_ChangedScalar(t *testing.T) {
	diff, err := diffMapInterface(sm("score", 10), sm("score", 20))
	require.NoError(t, err)
	assert.Equal(t, 20, diff["score"])
}

func TestDiffMap_UnchangedScalar_NotInDiff(t *testing.T) {
	diff, err := diffMapInterface(sm("name", "Alice"), sm("name", "Alice"))
	require.NoError(t, err)
	assert.NotContains(t, diff, "name")
}

func TestDiffMap_AddedKey(t *testing.T) {
	diff, err := diffMapInterface(sm("a", 1), sm("a", 1, "b", 2))
	require.NoError(t, err)
	assert.Equal(t, 2, diff["b"])
	assert.NotContains(t, diff, "a")
}

func TestDiffMap_RemovedKey_NotInDiff(t *testing.T) {
	// A key present in obj1 but absent in obj2 must NOT appear in the diff —
	// diffMapInterface reports "what is new in obj2", not "what was removed".
	diff, err := diffMapInterface(sm("a", 1, "b", 2), sm("a", 1))
	require.NoError(t, err)
	assert.NotContains(t, diff, "b")
	assert.NotContains(t, diff, "a")
}

func TestDiffMap_NestedMap_ChangedSubfield(t *testing.T) {
	// Nested maps are recursed; differing leaf is exposed as "parent.child".
	obj1 := sm("ref", sm("uid", "0x1", "name", "Alice"))
	obj2 := sm("ref", sm("uid", "0x1", "name", "Bob"))

	diff, err := diffMapInterface(obj1, obj2)
	require.NoError(t, err)

	assert.Equal(t, "Bob", diff["ref.name"], "changed nested field must appear as key.subkey")
	assert.NotContains(t, diff, "ref.uid", "unchanged nested field must not appear")
	assert.NotContains(t, diff, "ref", "parent key itself must not appear when recursing")
}

func TestDiffMap_NestedMap_NoChange_EmptyDiff(t *testing.T) {
	obj1 := sm("ref", sm("uid", "0x1"))
	obj2 := sm("ref", sm("uid", "0x1"))

	diff, err := diffMapInterface(obj1, obj2)
	require.NoError(t, err)
	assert.Empty(t, diff)
}

func TestDiffMap_MixedTypes_MapVsScalar(t *testing.T) {
	// One side map, other scalar → treated as changed, scalar value wins.
	obj1 := sm("field", sm("uid", "0x1"))
	obj2 := sm("field", "plain-string")

	diff, err := diffMapInterface(obj1, obj2)
	require.NoError(t, err)
	assert.Equal(t, "plain-string", diff["field"])
}

func TestDiffMap_MultipleKeys_MixedChanges(t *testing.T) {
	before := sm("a", 1, "b", "unchanged", "c", 3)
	input := sm("a", 99, "b", "unchanged", "d", "added")

	diff, err := diffMapInterface(before, input)
	require.NoError(t, err)

	assert.Equal(t, 99, diff["a"], "changed scalar")
	assert.NotContains(t, diff, "b", "unchanged scalar")
	assert.Equal(t, "added", diff["d"], "newly added key")
	assert.NotContains(t, diff, "c", "removed key must not appear")
}

// ── getDefaultValue typed-nil normalisation ───────────────────────────────────
//
// When an @default expr returns a typed nil (e.g. []float32(nil) from
// generateEmbedding when the API fails), the interface{} wrapper is non-nil
// even though the underlying value is nil. Without normalisation, the caller's
// `if value != nil` guard passes and the nil slice is written to obj, then
// marshalled as JSON "null" which Dgraph rejects with "cannot convert null to
// vfloat". getDefaultValue must normalise typed nils to untyped nil.

func TestGetDefaultValue_TypedNilExprResult_ReturnedAsNil(t *testing.T) {
	// Build a minimal schema with a Float list field that has an @default expr
	// that evaluates to a typed nil (simulating generateEmbedding failure).
	const gqlSchema = `
		type Widget {
			id: ID!
			name: String!
			vec: [Float] @default(expr: "nil")
		}
	`
	handler, err := NewHandler(gqlSchema, false)
	require.NoError(t, err)

	sch, err := FromString(handler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err)

	s, ok := sch.(*schema)
	require.True(t, ok)

	astSch := s.schema
	widgetDef := astSch.Types["Widget"]
	require.NotNil(t, widgetDef)

	vecFd := widgetDef.Fields.ForName("vec")
	require.NotNil(t, vecFd, "vec field must exist")

	auth := AuthCtx{}
	parent := map[string]interface{}{}

	val, _, err := getDefaultValue(astSch, vecFd, "add", "Widget", parent, auth, nil, nil)
	require.NoError(t, err)
	// A nil expr result must be returned as untyped nil, not a typed-nil interface.
	assert.Nil(t, val,
		"typed nil from @default expr must be normalised to untyped nil so the caller's `value != nil` guard works")
}
