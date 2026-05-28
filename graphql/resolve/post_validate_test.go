/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

// Tests for runPostValidate and collectPostValidateUIDs.
//
// Strategy: call runPostValidate directly (package-level func, same package as tests).
// The mock executor defined in resolver_error_test.go is reused here.
//
// Test schema: a "Review" type with @postValidate and @oldValue on several fields,
// declared inline so we don't touch the shared schema.graphql.

import (
	"context"
	"testing"

	dgoapi "github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/graphql/test"
	"github.com/stretchr/testify/require"
)

// postValidateSchema is the GQL schema used by all postValidate unit tests.
//
// Expressions use expr-lang's all(array, {predicate}) syntax where `.` is the
// current element — e.g. all(nodes, {.after.rating >= 1}). Field names are bare
// GQL names because runPostValidate normalises the Dgraph predicate prefix away.
const postValidateSchema = `
type Review
  @postValidate(
    add: {
      expr:   "all(nodes, {.after.rating >= 1 && .after.rating <= 5})"
      reason: "Rating must be between 1 and 5"
    }
    update: {
      expr:   "all(nodes, {.after.rating > 0})"
      reason: "Updated rating must be positive"
    }
  ) {
  id:      ID!
  rating:  Int    @oldValue
  comment: String @oldValue
}
`

// postValidateAlwaysTrueSchema has an expr that always succeeds.
const postValidateAlwaysTrueSchema = `
type Review
  @postValidate(
    add: { expr: "true" reason: "Always passes" }
  ) {
  id:     ID!
  rating: Int @oldValue
}
`

// postValidateAuthSchema: an auth-dependent expr that verifies auth variable is in scope.
const postValidateAuthSchema = `
type Review
  @postValidate(
    add: {
      expr:   "auth.ROLE == \"admin\" || all(nodes, {.after.rating >= 1})"
      reason: "Only admins can bypass rating minimum"
    }
  ) {
  id:     ID!
  rating: Int @oldValue
}
`

// postValidateAlwaysFalseSchema has an expr that always fails, for rejection tests.
const postValidateAlwaysFalseSchema = `
type Review
  @postValidate(
    add: {
      expr:   "false"
      reason: "Always fails"
    }
  ) {
  id:     ID!
  rating: Int @oldValue
}
`

// postValidateNewVarSchema: expr uses `.new` to check which fields actually changed.
const postValidateNewVarSchema = `
type Review
  @postValidate(
    update: {
      expr:   "all(nodes, {\"rating\" in keys(.new)})"
      reason: "Rating must be explicitly set in every updated node"
    }
  ) {
  id:      ID!
  rating:  Int    @oldValue
  comment: String @oldValue
}
`

// postValidateNoOldValueSchema tests the case where no @oldValue fields exist —
// each node gets an empty before map; the expr uses action which is always available.
const postValidateNoOldValueSchema = `
type Review
  @postValidate(
    add: {
      expr:   "action == \"add\""
      reason: "Must be add action"
    }
  ) {
  id:    ID!
  title: String
}
`

// postValidateDeleteSchema — delete mutations must silently skip @postValidate.
// We use an always-false expr to verify it is never evaluated.
const postValidateDeleteSchema = `
type Review
  @postValidate(
    add: {
      expr:   "false"
      reason: "Should never be reached on delete"
    }
  ) {
  id:     ID!
  rating: Int @oldValue
}
`

// makeAddMutation parses a GQL add mutation operation from the given schema+query strings.
func makeAddMutation(t *testing.T, gqlSchema schema.Schema, gqlMut string) schema.Mutation {
	t.Helper()
	op, err := gqlSchema.Operation(&schema.Request{Query: gqlMut})
	require.NoError(t, err)
	return test.GetMutation(t, op)
}

// makeDeleteMutation parses a GQL delete mutation operation.
func makeDeleteMutation(t *testing.T, gqlSchema schema.Schema, gqlMut string) schema.Mutation {
	t.Helper()
	op, err := gqlSchema.Operation(&schema.Request{Query: gqlMut})
	require.NoError(t, err)
	return test.GetMutation(t, op)
}

// fakeExecutor is a minimal DgraphExecutor that returns a canned JSON response
// for every query (the post-validate node-fetch query).
type fakeExecutor struct {
	// queryResp is the JSON returned for query-only Execute calls.
	queryResp string
	// failMsg, if non-empty, makes every Execute call return an error.
	failMsg string
}

func (fe *fakeExecutor) Execute(_ context.Context, req *dgoapi.Request,
	_ schema.Field) (*dgoapi.Response, error) {
	if fe.failMsg != "" {
		return nil, schema.GQLWrapf(nil, "%s", fe.failMsg)
	}
	// Only respond to pure query calls (no mutations in request).
	if len(req.Mutations) == 0 {
		return &dgoapi.Response{Json: []byte(fe.queryResp)}, nil
	}
	return &dgoapi.Response{}, nil
}

func (fe *fakeExecutor) CommitOrAbort(_ context.Context,
	tc *dgoapi.TxnContext) (*dgoapi.TxnContext, error) {
	return &dgoapi.TxnContext{}, nil
}

// --- Tests ---

// TestPostValidate_AddPasses verifies that when all nodes satisfy the add expr, no error
// is returned.
func TestPostValidate_AddPasses(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, postValidateSchema)
	mut := makeAddMutation(t, gqlSchema, `mutation {
		addReview(input: [{rating: 3, comment: "good"}]) { review { id } }
	}`)

	rewriter := NewAddRewriter()
	// mutResp: one blank-node UID was assigned.
	mutResp := &dgoapi.Response{
		Uids: map[string]string{"Review_1": "0x10"},
	}
	// The executor returns the node with Dgraph predicate keys — normalizePredicateKeys
	// will strip the "Review." prefix, exposing n.after.rating in the expression.
	ex := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x10","Review.rating":3,"Review.comment":"good"}]}`}

	err := runPostValidate(context.Background(), mut, ex, rewriter, mutResp, nil)
	require.NoError(t, err, "expr should pass when rating is in [1,5]")
}

// TestPostValidate_AddFails verifies that when a node violates the add expr, an error
// containing the reason string is returned.
func TestPostValidate_AddFails(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, postValidateAlwaysFalseSchema)
	mut := makeAddMutation(t, gqlSchema, `mutation {
		addReview(input: [{rating: 3}]) { review { id } }
	}`)

	rewriter := NewAddRewriter()
	mutResp := &dgoapi.Response{
		Uids: map[string]string{"Review_1": "0x11"},
	}
	ex := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x11","Review.rating":3}]}`}

	err := runPostValidate(context.Background(), mut, ex, rewriter, mutResp, nil)
	require.Error(t, err, "always-false expr should return an error")
	require.Contains(t, err.Error(), "Always fails")
}

// TestPostValidate_DeleteSkipped verifies that delete mutations are silently skipped even
// when the @postValidate expr is always-false.
func TestPostValidate_DeleteSkipped(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, postValidateDeleteSchema)

	op, err := gqlSchema.Operation(&schema.Request{Query: `mutation {
		deleteReview(filter: {id: ["0x20"]}) { msg }
	}`})
	require.NoError(t, err)
	mut := test.GetMutation(t, op)

	rewriter := NewDeleteRewriter()
	mutResp := &dgoapi.Response{}
	// The executor should never be called for a delete post-validate; use a failing mock.
	ex := &fakeExecutor{failMsg: "should not be called for delete"}

	err = runPostValidate(context.Background(), mut, ex, rewriter, mutResp, nil)
	require.NoError(t, err, "delete mutations must be silently skipped by @postValidate")
}

// TestPostValidate_NoOldValueFields verifies that types without @oldValue fields still
// have the action variable available in the expression context.
func TestPostValidate_NoOldValueFields(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, postValidateNoOldValueSchema)
	mut := makeAddMutation(t, gqlSchema, `mutation {
		addReview(input: [{}]) { review { id } }
	}`)

	rewriter := NewAddRewriter()
	mutResp := &dgoapi.Response{
		Uids: map[string]string{"Review_1": "0x30"},
	}
	// The post-validate query returns the node with only uid (no @oldValue fields).
	ex := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x30"}]}`}

	err := runPostValidate(context.Background(), mut, ex, rewriter, mutResp, nil)
	require.NoError(t, err, "action == \"add\" expr should pass for add mutations")
}

// TestPostValidate_BulkNodes verifies that all nodes from a bulk add are bundled into the
// nodes array and the expression is evaluated once across all of them.
func TestPostValidate_BulkNodes(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, postValidateSchema)
	mut := makeAddMutation(t, gqlSchema, `mutation {
		addReview(input: [
			{rating: 2, comment: "ok"},
			{rating: 4, comment: "great"},
			{rating: 5, comment: "excellent"}
		]) { review { id } }
	}`)

	rewriter := NewAddRewriter()
	// Three blank-node UIDs assigned.
	mutResp := &dgoapi.Response{
		Uids: map[string]string{
			"Review_1": "0x40",
			"Review_2": "0x41",
			"Review_3": "0x42",
		},
	}
	// All three nodes have valid ratings (1–5); Dgraph predicate names are normalised.
	ex := &fakeExecutor{queryResp: `{"postValidateNodes": [
		{"uid":"0x40","Review.rating":2},
		{"uid":"0x41","Review.rating":4},
		{"uid":"0x42","Review.rating":5}
	]}`}

	err := runPostValidate(context.Background(), mut, ex, rewriter, mutResp, nil)
	require.NoError(t, err, "all three nodes satisfy the expr")
}

// TestPostValidate_BulkNodes_OneFails verifies that when the expr is false for any reason,
// the entire mutation is rejected (tests the rejection path with multiple nodes present).
func TestPostValidate_BulkNodes_OneFails(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, postValidateAlwaysFalseSchema)
	mut := makeAddMutation(t, gqlSchema, `mutation {
		addReview(input: [{rating: 3}, {rating: 0}]) { review { id } }
	}`)

	rewriter := NewAddRewriter()
	mutResp := &dgoapi.Response{
		Uids: map[string]string{
			"Review_1": "0x50",
			"Review_2": "0x51",
		},
	}
	ex := &fakeExecutor{queryResp: `{"postValidateNodes": [
		{"uid":"0x50"},
		{"uid":"0x51"}
	]}`}
	err := runPostValidate(context.Background(), mut, ex, rewriter, mutResp, nil)
	require.Error(t, err, "always-false expr should return an error")
	require.Contains(t, err.Error(), "Always fails")
}

// TestPostValidate_AuthAvailableInExpr verifies the auth variable is in scope.
// Without an auth-configured schema, auth is an empty map — so auth.ROLE is missing
// and the expr falls through to the nodes check.
func TestPostValidate_AuthAvailableInExpr(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, postValidateAuthSchema)
	mut := makeAddMutation(t, gqlSchema, `mutation {
		addReview(input: [{rating: 3}]) { review { id } }
	}`)

	rewriter := NewAddRewriter()
	mutResp := &dgoapi.Response{
		Uids: map[string]string{"Review_1": "0x60"},
	}
	// rating >= 1 is satisfied (auth falls back to empty map, nodes check passes).
	ex := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x60","Review.rating":3}]}`}

	err := runPostValidate(context.Background(), mut, ex, rewriter, mutResp, nil)
	require.NoError(t, err, "expr should pass: auth falls back to empty map, nodes check passes")
}

// TestPostValidate_NoMutatedUIDs verifies that when no UIDs are present in mutResp
// (e.g. a no-op update), runPostValidate returns nil without querying Dgraph.
func TestPostValidate_NoMutatedUIDs(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, postValidateSchema)
	mut := makeAddMutation(t, gqlSchema, `mutation {
		addReview(input: [{rating: 3}]) { review { id } }
	}`)

	rewriter := NewAddRewriter()
	// Empty response — no UIDs assigned (Dgraph decided nothing to write).
	mutResp := &dgoapi.Response{Uids: map[string]string{}}
	ex := &fakeExecutor{failMsg: "should not query when no UIDs"}

	err := runPostValidate(context.Background(), mut, ex, rewriter, mutResp, nil)
	require.NoError(t, err, "no UIDs → nothing to validate → no error")
}

// TestPostValidate_NewVarAvailable verifies the `.new` per-node field is accessible.
// For add mutations before is empty, so new == after. The expr checks "rating" is in
// the keys of .new, which must be true when a rating is set on add.
func TestPostValidate_NewVarAvailable(t *testing.T) {
	// Schema: update expr checks "rating" in keys(.new)
	// We use an add mutation — before is empty so new == after.
	const newVarSchema = `
type Review
  @postValidate(
    add: {
      expr:   "all(nodes, {\"rating\" in keys(.new)})"
      reason: "Rating must be present in new fields"
    }
  ) {
  id:     ID!
  rating: Int    @oldValue
}`
	gqlSchema := test.LoadSchemaFromString(t, newVarSchema)
	mut := makeAddMutation(t, gqlSchema, `mutation {
		addReview(input: [{rating: 4}]) { review { id } }
	}`)

	rewriter := NewAddRewriter()
	mutResp := &dgoapi.Response{Uids: map[string]string{"Review_1": "0x70"}}
	// rating is in the post-mutation node — for an add, before={} so new==after.
	ex := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x70","Review.rating":4}]}`}

	err := runPostValidate(context.Background(), mut, ex, rewriter, mutResp, nil)
	require.NoError(t, err, "rating in new fields → expr passes")
}

// TestPostValidate_NewVarMissingField verifies that a field not present in after is
// absent from .new, causing the expression to fail.
func TestPostValidate_NewVarMissingField(t *testing.T) {
	const newVarSchema = `
type Review
  @postValidate(
    add: {
      expr:   "all(nodes, {\"comment\" in keys(.new)})"
      reason: "Comment must be provided"
    }
  ) {
  id:      ID!
  rating:  Int    @oldValue
  comment: String @oldValue
}`
	gqlSchema := test.LoadSchemaFromString(t, newVarSchema)
	// Input has no comment — after won't have "comment", so .new won't have it either.
	mut := makeAddMutation(t, gqlSchema, `mutation {
		addReview(input: [{rating: 4}]) { review { id } }
	}`)

	rewriter := NewAddRewriter()
	mutResp := &dgoapi.Response{Uids: map[string]string{"Review_1": "0x71"}}
	// Dgraph response only has rating — comment absent from after → absent from new.
	ex := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x71","Review.rating":4}]}`}

	err := runPostValidate(context.Background(), mut, ex, rewriter, mutResp, nil)
	require.Error(t, err, "comment absent from new → expr fails")
	require.Contains(t, err.Error(), "Comment must be provided")
}

// --- collectPostValidateUIDs tests ---

// TestCollectPostValidateUIDs_Add verifies blank-node UIDs matching the type prefix
// are extracted and the inverted map is correct.
func TestCollectPostValidateUIDs_Add(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, postValidateSchema)
	mut := makeAddMutation(t, gqlSchema, `mutation {
		addReview(input: [{rating: 3}]) { review { id } }
	}`)

	mutResp := &dgoapi.Response{
		Uids: map[string]string{
			"Review_1": "0xa0",
			"Review_2": "0xa1",
			"Other_1":  "0xa2", // different type — must be excluded
		},
	}
	uids, uidToBlank := collectPostValidateUIDs("Review", mut, mutResp, nil)

	require.ElementsMatch(t, []string{"0xa0", "0xa1"}, uids)
	require.Equal(t, "Review_1", uidToBlank["0xa0"])
	require.Equal(t, "Review_2", uidToBlank["0xa1"])
	require.NotContains(t, uidToBlank, "0xa2", "other-type UIDs must not appear")
}

// TestCollectPostValidateUIDs_Update verifies that update mutations also include
// existing node UIDs from the result map (via extractMutated).
func TestCollectPostValidateUIDs_Update(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, postValidateSchema)
	op, err := gqlSchema.Operation(&schema.Request{Query: `mutation {
		updateReview(input: {
			filter: {id: ["0xb0", "0xb1"]},
			set: {rating: 4}
		}) { review { id } }
	}`})
	require.NoError(t, err)
	mut := test.GetMutation(t, op)

	// mutResp has no assigned blank nodes (update assigns none).
	mutResp := &dgoapi.Response{Uids: map[string]string{}}
	// result map simulates what Dgraph returns after an update: matched UIDs under typeName.
	result := map[string]interface{}{
		"updateReview": []interface{}{
			map[string]interface{}{"uid": "0xb0"},
			map[string]interface{}{"uid": "0xb1"},
		},
	}
	uids, uidToBlank := collectPostValidateUIDs("Review", mut, mutResp, result)

	require.ElementsMatch(t, []string{"0xb0", "0xb1"}, uids)
	// Update UIDs don't have a blank-node name — they map to empty string.
	require.Equal(t, "", uidToBlank["0xb0"])
	require.Equal(t, "", uidToBlank["0xb1"])
}

// TestCollectPostValidateUIDs_Empty verifies that nil inputs return empty slices.
func TestCollectPostValidateUIDs_Empty(t *testing.T) {
	gqlSchema := test.LoadSchemaFromString(t, postValidateSchema)
	mut := makeAddMutation(t, gqlSchema, `mutation {
		addReview(input: [{rating: 1}]) { review { id } }
	}`)
	uids, uidToBlank := collectPostValidateUIDs("Review", mut, nil, nil)
	require.Empty(t, uids)
	require.Empty(t, uidToBlank)
}

// --- renderPostValidateReason unit tests ---

// TestRenderPostValidateReason_PlainString verifies strings without "{{"
// are returned as-is with no template processing.
func TestRenderPostValidateReason_PlainString(t *testing.T) {
	nodes := []map[string]interface{}{{"uid": "0x1"}}
	out, err := renderPostValidateReason("No quota remaining", nodes, "add", nil, "")
	require.NoError(t, err)
	require.Equal(t, "No quota remaining", out)
}

// TestRenderPostValidateReason_CountTemplate verifies {{.count}} is substituted.
func TestRenderPostValidateReason_CountTemplate(t *testing.T) {
	nodes := []map[string]interface{}{{"uid": "0x1"}, {"uid": "0x2"}, {"uid": "0x3"}}
	out, err := renderPostValidateReason(
		"Cannot add {{.count}} items in one request", nodes, "add", nil, "")
	require.NoError(t, err)
	require.Equal(t, "Cannot add 3 items in one request", out)
}

// TestRenderPostValidateReason_ActionTemplate verifies {{.action}} is substituted.
func TestRenderPostValidateReason_ActionTemplate(t *testing.T) {
	nodes := []map[string]interface{}{{"uid": "0x1"}}
	out, err := renderPostValidateReason("Forbidden on {{.action}}", nodes, "update", nil, "")
	require.NoError(t, err)
	require.Equal(t, "Forbidden on update", out)
}

// TestRenderPostValidateReason_ErrorTemplate verifies {{.error}} carries the
// payload from error() calls, not the full CEL stacktrace.
func TestRenderPostValidateReason_ErrorTemplate(t *testing.T) {
	nodes := []map[string]interface{}{{"uid": "0x1"}}
	out, err := renderPostValidateReason(
		"Validation failed: {{.error}}", nodes, "add", nil,
		"quota exceeded. Limit: 10, Used: 10, Requested: 1.")
	require.NoError(t, err)
	require.Equal(t, "Validation failed: quota exceeded. Limit: 10, Used: 10, Requested: 1.", out)
}

// TestRenderPostValidateReason_AuthTemplate verifies {{index .auth "KEY"}} access.
func TestRenderPostValidateReason_AuthTemplate(t *testing.T) {
	nodes := []map[string]interface{}{{"uid": "0x1"}}
	auth := map[string]interface{}{"USER": "alice"}
	out, err := renderPostValidateReason(
		`Forbidden for user {{index .auth "USER"}}`, nodes, "add", auth, "")
	require.NoError(t, err)
	require.Equal(t, "Forbidden for user alice", out)
}

// TestRenderPostValidateReason_InvalidTemplate verifies a malformed template
// returns an error so the caller can fall back to the raw reason string.
func TestRenderPostValidateReason_InvalidTemplate(t *testing.T) {
	_, err := renderPostValidateReason("{{.unclosed", nil, "add", nil, "")
	require.Error(t, err, "malformed template must return an error")
}

// --- end-to-end: reason as template when expr returns false ---

// TestPostValidate_ReasonTemplate_ExprFalse verifies that {{.count}} and
// {{.action}} are substituted when the expression returns false.
func TestPostValidate_ReasonTemplate_ExprFalse(t *testing.T) {
	const schemaStr = `
type Review
  @postValidate(
    add: {
      expr:   "len(nodes) <= 1"
      reason: "Cannot add {{.count}} reviews in one request (action: {{.action}})"
    }
  ) {
  id:    ID!
  title: String
}`
	gqlSchema := test.LoadSchemaFromString(t, schemaStr)
	mut := makeAddMutation(t, gqlSchema, `mutation {
		addReview(input: [{}, {}]) { review { id } }
	}`)
	rewriter := NewAddRewriter()
	mutResp := &dgoapi.Response{Uids: map[string]string{"Review_1": "0xc1", "Review_2": "0xc2"}}
	ex := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0xc1"},{"uid":"0xc2"}]}`}

	err := runPostValidate(context.Background(), mut, ex, rewriter, mutResp, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Cannot add 2 reviews in one request (action: add)")
}

// --- end-to-end: reason with {{.error}} when expr calls error() ---

// TestPostValidate_ReasonTemplate_ExprError verifies that when the expression
// calls error(msg), the reason template is rendered with {{.error}} = msg —
// the clean payload via ExprError/errors.As, not the raw CEL stacktrace.
func TestPostValidate_ReasonTemplate_ExprError(t *testing.T) {
	const schemaStr = `
type Review
  @postValidate(
    add: {
      expr:   "error(\"quota exceeded: used 10 of 10\")"
      reason: "Validation failed: {{.error}}"
    }
  ) {
  id:    ID!
  title: String
}`
	gqlSchema := test.LoadSchemaFromString(t, schemaStr)
	mut := makeAddMutation(t, gqlSchema, `mutation {
		addReview(input: [{}]) { review { id } }
	}`)
	rewriter := NewAddRewriter()
	mutResp := &dgoapi.Response{Uids: map[string]string{"Review_1": "0xd1"}}
	ex := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0xd1"}]}`}

	err := runPostValidate(context.Background(), mut, ex, rewriter, mutResp, nil)
	require.Error(t, err)
	// {{.error}} must be the clean message, not the CEL annotation.
	require.Contains(t, err.Error(), "Validation failed: quota exceeded: used 10 of 10")
	require.NotContains(t, err.Error(), "(1:", "CEL line:col annotation must not appear")
}
