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
	"encoding/json"
	"errors"
	"testing"

	dgoapi "github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/graphql/test"
	"github.com/hypermodeinc/dgraph/v25/x"
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

// TestPostValidate_InheritedFromInterface verifies that @postValidate directives declared
// on an interface are inherited and evaluated when mutating an implementing concrete type.
func TestPostValidate_InheritedFromInterface(t *testing.T) {
	schemaStr := `
interface FormDataIfc
  @generate(query: { get: false, query: false }, mutation: { add: false, update: false })
  @postValidate(
    add: {
      expr:   "all(nodes, {.after.data != \"invalid\"})"
      reason: "Interface validation rejected invalid data"
    }
  ) {
  id:   ID!
  data: String @oldValue
}

type SettingsForm implements FormDataIfc
  @generate(query: { get: true, query: true }, mutation: { add: true, update: true }) {
  id:   ID!
  data: String @oldValue
}
`
	gqlSchema := test.LoadSchemaFromString(t, schemaStr)

	// 1. Valid data -> passes
	mutPass := makeAddMutation(t, gqlSchema, `mutation {
		addSettingsForm(input: [{data: "valid"}]) { settingsForm { id } }
	}`)
	rewriterPass := NewAddRewriter()
	mutRespPass := &dgoapi.Response{Uids: map[string]string{"SettingsForm_1": "0xe1"}}
	exPass := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0xe1","SettingsForm.data":"valid"}]}`}

	err := runPostValidate(context.Background(), mutPass, exPass, rewriterPass, mutRespPass, nil)
	require.NoError(t, err, "valid data should pass inherited @postValidate")

	// 2. Invalid data -> fails with interface reason
	mutFail := makeAddMutation(t, gqlSchema, `mutation {
		addSettingsForm(input: [{data: "invalid"}]) { settingsForm { id } }
	}`)
	rewriterFail := NewAddRewriter()
	mutRespFail := &dgoapi.Response{Uids: map[string]string{"SettingsForm_1": "0xe2"}}
	exFail := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0xe2","SettingsForm.data":"invalid"}]}`}

	errFail := runPostValidate(context.Background(), mutFail, exFail, rewriterFail, mutRespFail, nil)
	require.Error(t, errFail)
	require.Contains(t, errFail.Error(), "Interface validation rejected invalid data")
}

// TestPostValidate_InheritedAndDirectComposed verifies that when both a concrete type and
// an implemented interface declare @postValidate, both are evaluated in sequence.
func TestPostValidate_InheritedAndDirectComposed(t *testing.T) {
	schemaStr := `
interface FormDataIfc
  @generate(query: { get: false, query: false }, mutation: { add: false, update: false })
  @postValidate(
    add: {
      expr:   "all(nodes, {.after.data != \"bad_format\"})"
      reason: "Interface: bad format"
    }
  ) {
  id:   ID!
  data: String @oldValue
}

type PortalForm implements FormDataIfc
  @generate(query: { get: true, query: true }, mutation: { add: true, update: true })
  @postValidate(
    add: {
      expr:   "all(nodes, {.after.data != \"unauthorized\"})"
      reason: "PortalForm: unauthorized"
    }
  ) {
  id:   ID!
  data: String @oldValue
}
`
	gqlSchema := test.LoadSchemaFromString(t, schemaStr)

	// 1. Both pass
	mutPass := makeAddMutation(t, gqlSchema, `mutation {
		addPortalForm(input: [{data: "ok"}]) { portalForm { id } }
	}`)
	rewriterPass := NewAddRewriter()
	mutRespPass := &dgoapi.Response{Uids: map[string]string{"PortalForm_1": "0xf1"}}
	exPass := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0xf1","PortalForm.data":"ok"}]}`}
	err := runPostValidate(context.Background(), mutPass, exPass, rewriterPass, mutRespPass, nil)
	require.NoError(t, err)

	// 2. Concrete type rule fails
	mutFailDirect := makeAddMutation(t, gqlSchema, `mutation {
		addPortalForm(input: [{data: "unauthorized"}]) { portalForm { id } }
	}`)
	rewriterFailDirect := NewAddRewriter()
	mutRespFailDirect := &dgoapi.Response{Uids: map[string]string{"PortalForm_1": "0xf2"}}
	exFailDirect := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0xf2","PortalForm.data":"unauthorized"}]}`}
	errFailDirect := runPostValidate(context.Background(), mutFailDirect, exFailDirect, rewriterFailDirect, mutRespFailDirect, nil)
	require.Error(t, errFailDirect)
	require.Contains(t, errFailDirect.Error(), "PortalForm: unauthorized")

	// 3. Inherited interface rule fails
	mutFailIface := makeAddMutation(t, gqlSchema, `mutation {
		addPortalForm(input: [{data: "bad_format"}]) { portalForm { id } }
	}`)
	rewriterFailIface := NewAddRewriter()
	mutRespFailIface := &dgoapi.Response{Uids: map[string]string{"PortalForm_1": "0xf3"}}
	exFailIface := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0xf3","PortalForm.data":"bad_format"}]}`}
	errFailIface := runPostValidate(context.Background(), mutFailIface, exFailIface, rewriterFailIface, mutRespFailIface, nil)
	require.Error(t, errFailIface)
	require.Contains(t, errFailIface.Error(), "Interface: bad format")
}

// TestPostValidate_ArrayOfRules_Add verifies that @postValidate accepts an array of
// rules under add: [ { expr, reason }, ... ] and executes them sequentially.
func TestPostValidate_ArrayOfRules_Add(t *testing.T) {
	schemaStr := `
type Product
  @generate(query: { get: true, query: true }, mutation: { add: true, update: true })
  @postValidate(
    add: [
      {
        expr:   "all(nodes, {.after.price > 0})"
        reason: "Product price must be positive"
      },
      {
        expr:   "all(nodes, {.after.stock >= 0})"
        reason: "Stock cannot be negative"
      }
    ]
  ) {
  id:    ID!
  price: Float @oldValue
  stock: Int   @oldValue
}
`
	gqlSchema := test.LoadSchemaFromString(t, schemaStr)

	// 1. All pass
	mutPass := makeAddMutation(t, gqlSchema, `mutation {
		addProduct(input: [{price: 19.99, stock: 10}]) { product { id } }
	}`)
	rewriterPass := NewAddRewriter()
	mutRespPass := &dgoapi.Response{Uids: map[string]string{"Product_1": "0x101"}}
	exPass := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x101","Product.price":19.99,"Product.stock":10}]}`}
	err := runPostValidate(context.Background(), mutPass, exPass, rewriterPass, mutRespPass, nil)
	require.NoError(t, err)

	// 2. First rule fails (price <= 0)
	mutFail1 := makeAddMutation(t, gqlSchema, `mutation {
		addProduct(input: [{price: -5.0, stock: 10}]) { product { id } }
	}`)
	rewriterFail1 := NewAddRewriter()
	mutRespFail1 := &dgoapi.Response{Uids: map[string]string{"Product_1": "0x102"}}
	exFail1 := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x102","Product.price":-5.0,"Product.stock":10}]}`}
	errFail1 := runPostValidate(context.Background(), mutFail1, exFail1, rewriterFail1, mutRespFail1, nil)
	require.Error(t, errFail1)
	require.Contains(t, errFail1.Error(), "Product price must be positive")

	// 3. Second rule fails (stock < 0)
	mutFail2 := makeAddMutation(t, gqlSchema, `mutation {
		addProduct(input: [{price: 15.0, stock: -2}]) { product { id } }
	}`)
	rewriterFail2 := NewAddRewriter()
	mutRespFail2 := &dgoapi.Response{Uids: map[string]string{"Product_1": "0x103"}}
	exFail2 := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x103","Product.price":15.0,"Product.stock":-2}]}`}
	errFail2 := runPostValidate(context.Background(), mutFail2, exFail2, rewriterFail2, mutRespFail2, nil)
	require.Error(t, errFail2)
	require.Contains(t, errFail2.Error(), "Stock cannot be negative")
}

// TestPostValidate_ArrayOfRules_RootRules verifies that @postValidate accepts an array of
// rules under rules: [ { expr, reason }, ... ] that applies across operations.
func TestPostValidate_ArrayOfRules_RootRules(t *testing.T) {
	schemaStr := `
type Account
  @generate(query: { get: true, query: true }, mutation: { add: true, update: true })
  @postValidate(
    reason: "Default account error"
    rules: [
      {
        expr:   "all(nodes, {.after.name != \"\"})"
        reason: "Name is required"
      },
      {
        expr:   "all(nodes, {.after.limit >= 100})"
      }
    ]
  ) {
  id:    ID!
  name:  String @oldValue
  limit: Int    @oldValue
}
`
	gqlSchema := test.LoadSchemaFromString(t, schemaStr)

	// 1. Both pass
	mutPass := makeAddMutation(t, gqlSchema, `mutation {
		addAccount(input: [{name: "Alice", limit: 500}]) { account { id } }
	}`)
	rewriterPass := NewAddRewriter()
	mutRespPass := &dgoapi.Response{Uids: map[string]string{"Account_1": "0x201"}}
	exPass := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x201","Account.name":"Alice","Account.limit":500}]}`}
	err := runPostValidate(context.Background(), mutPass, exPass, rewriterPass, mutRespPass, nil)
	require.NoError(t, err)

	// 2. First rule fails (custom reason)
	mutFail1 := makeAddMutation(t, gqlSchema, `mutation {
		addAccount(input: [{name: "", limit: 500}]) { account { id } }
	}`)
	rewriterFail1 := NewAddRewriter()
	mutRespFail1 := &dgoapi.Response{Uids: map[string]string{"Account_1": "0x202"}}
	exFail1 := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x202","Account.name":"","Account.limit":500}]}`}
	errFail1 := runPostValidate(context.Background(), mutFail1, exFail1, rewriterFail1, mutRespFail1, nil)
	require.Error(t, errFail1)
	require.Contains(t, errFail1.Error(), "Name is required")

	// 3. Second rule fails (falls back to default root reason)
	mutFail2 := makeAddMutation(t, gqlSchema, `mutation {
		addAccount(input: [{name: "Bob", limit: 50}]) { account { id } }
	}`)
	rewriterFail2 := NewAddRewriter()
	mutRespFail2 := &dgoapi.Response{Uids: map[string]string{"Account_1": "0x203"}}
	exFail2 := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x203","Account.name":"Bob","Account.limit":50}]}`}
	errFail2 := runPostValidate(context.Background(), mutFail2, exFail2, rewriterFail2, mutRespFail2, nil)
	require.Error(t, errFail2)
	require.Contains(t, errFail2.Error(), "Default account error")
}

func makeAddMutationWithVars(t *testing.T, gqlSchema schema.Schema, gqlMut string, vars map[string]interface{}) schema.Mutation {
	t.Helper()
	op, err := gqlSchema.Operation(&schema.Request{Query: gqlMut, Variables: vars})
	require.NoError(t, err)
	return test.GetMutation(t, op)
}

// TestExprError_MultiAndRichErrors tests error(v) supporting single string, list of strings,
// and rich error objects with message, code, and extensions.
func TestExprError_MultiAndRichErrors(t *testing.T) {
	schemaStr := `
type Item
  @generate(query: { get: true, query: true }, mutation: { add: true })
  @postValidate(
    rules: [
      {
        expr: "all(nodes, {.after.code != \"MULTI\" ? true : error([\"err1: code is multi\", \"err2: cannot proceed\"])})"
      },
      {
        expr: "all(nodes, {.after.code != \"RICH\" ? true : error([{message: \"rich error occurred\", code: \"ERR_INVALID_CODE\", severity: \"critical\"}])})"
      }
    ]
  ) {
  id:   ID!
  code: String @oldValue
}
`
	gqlSchema := test.LoadSchemaFromString(t, schemaStr)

	// 1. Multi-error list of strings: error(["err1...", "err2..."])
	mutMulti := makeAddMutation(t, gqlSchema, `mutation {
		addItem(input: [{code: "MULTI"}]) { item { id } }
	}`)
	rewriterMulti := NewAddRewriter()
	mutRespMulti := &dgoapi.Response{Uids: map[string]string{"Item_1": "0x301"}}
	exMulti := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x301","Item.code":"MULTI"}]}`}
	errMulti := runPostValidate(context.Background(), mutMulti, exMulti, rewriterMulti, mutRespMulti, nil)
	require.Error(t, errMulti)

	var exprErrors *schema.ExprErrors
	require.True(t, errors.As(errMulti, &exprErrors), "error should be *schema.ExprErrors")
	require.Len(t, exprErrors.Errors, 2)
	require.Equal(t, "err1: code is multi", exprErrors.Errors[0].Message)
	require.Equal(t, "err2: cannot proceed", exprErrors.Errors[1].Message)

	// Verify schema.AsGQLErrors unpacks both errors
	gqlErrs := schema.AsGQLErrors(errMulti)
	require.Len(t, gqlErrs, 2)
	require.Equal(t, "err1: code is multi", gqlErrs[0].Message)
	require.Equal(t, "err2: cannot proceed", gqlErrs[1].Message)

	// 2. Rich error object: error([{message: "...", code: "...", severity: "..."}])
	mutRich := makeAddMutation(t, gqlSchema, `mutation {
		addItem(input: [{code: "RICH"}]) { item { id } }
	}`)
	rewriterRich := NewAddRewriter()
	mutRespRich := &dgoapi.Response{Uids: map[string]string{"Item_1": "0x302"}}
	exRich := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x302","Item.code":"RICH"}]}`}
	errRich := runPostValidate(context.Background(), mutRich, exRich, rewriterRich, mutRespRich, nil)
	require.Error(t, errRich)

	var richErrors *schema.ExprErrors
	require.True(t, errors.As(errRich, &richErrors))
	require.Len(t, richErrors.Errors, 1)
	require.Equal(t, "rich error occurred", richErrors.Errors[0].Message)
	require.Equal(t, "ERR_INVALID_CODE", richErrors.Errors[0].Extensions["code"])
	require.Equal(t, "critical", richErrors.Errors[0].Extensions["severity"])

	// Verify extensions are preserved in x.GqlErrorList via AsGQLErrors and PrependPath
	prepended := schema.PrependPath(errRich, "addItem")
	gqlRichList, ok := prepended.(x.GqlErrorList)
	require.True(t, ok)
	require.Len(t, gqlRichList, 1)
	require.Equal(t, "rich error occurred", gqlRichList[0].Message)
	require.Equal(t, "ERR_INVALID_CODE", gqlRichList[0].Extensions["code"])
	require.Equal(t, []interface{}{"addItem"}, gqlRichList[0].Path)
}

// TestDryRun_DirectiveParsing verifies that @dryRun directive is correctly parsed
// with boolean literals, omitted arguments, and GraphQL variables.
func TestDryRun_DirectiveParsing(t *testing.T) {
	schemaStr := `
type Entity
  @generate(query: { get: true }, mutation: { add: true }) {
  id:   ID!
  name: String
}
`
	gqlSchema := test.LoadSchemaFromString(t, schemaStr)

	// 1. @dryRun(enabled: true)
	mutTrue := makeAddMutation(t, gqlSchema, `mutation @dryRun(enabled: true) {
		addEntity(input: [{name: "Test"}]) { entity { id } }
	}`)
	require.True(t, mutTrue.IsDryRun())

	// 2. @dryRun (without arguments, explicit directive present)
	mutDefault := makeAddMutation(t, gqlSchema, `mutation @dryRun {
		addEntity(input: [{name: "Test"}]) { entity { id } }
	}`)
	require.True(t, mutDefault.IsDryRun())

	// 3. @dryRun(enabled: false)
	mutFalse := makeAddMutation(t, gqlSchema, `mutation @dryRun(enabled: false) {
		addEntity(input: [{name: "Test"}]) { entity { id } }
	}`)
	require.False(t, mutFalse.IsDryRun())

	// 4. No @dryRun directive
	mutNone := makeAddMutation(t, gqlSchema, `mutation {
		addEntity(input: [{name: "Test"}]) { entity { id } }
	}`)
	require.False(t, mutNone.IsDryRun())

	// 5. GraphQL variables: $dry = true
	mutVarTrue := makeAddMutationWithVars(t, gqlSchema, `mutation ($dry: Boolean = false) @dryRun(enabled: $dry) {
		addEntity(input: [{name: "Test"}]) { entity { id } }
	}`, map[string]interface{}{"dry": true})
	require.True(t, mutVarTrue.IsDryRun())

	// 6. GraphQL variables: $dry = false
	mutVarFalse := makeAddMutationWithVars(t, gqlSchema, `mutation ($dry: Boolean = true) @dryRun(enabled: $dry) {
		addEntity(input: [{name: "Test"}]) { entity { id } }
	}`, map[string]interface{}{"dry": false})
	require.False(t, mutVarFalse.IsDryRun())

	// 7. GraphQL variables default value ($dry not provided)
	mutVarOmitted := makeAddMutationWithVars(t, gqlSchema, `mutation ($dry: Boolean = false) @dryRun(enabled: $dry) {
		addEntity(input: [{name: "Test"}]) { entity { id } }
	}`, map[string]interface{}{})
	require.False(t, mutVarOmitted.IsDryRun())
}

// TestDryRun_IsDryRunInExprContext verifies that isDryRun boolean is injected into
// postValidateEnv and accessible inside CEL @postValidate expressions.
func TestDryRun_IsDryRunInExprContext(t *testing.T) {
	schemaStr := `
type DryEntity
  @generate(query: { get: true }, mutation: { add: true })
  @postValidate(
    add: {
      expr: "!isDryRun || error(\"intercepted by dry run rule\")"
    }
  ) {
  id:   ID!
  name: String @oldValue
}
`
	gqlSchema := test.LoadSchemaFromString(t, schemaStr)

	// 1. Dry run enabled: isDryRun == true -> triggers error("intercepted by dry run rule")
	mutDry := makeAddMutation(t, gqlSchema, `mutation @dryRun(enabled: true) {
		addDryEntity(input: [{name: "Test"}]) { dryEntity { id } }
	}`)
	require.True(t, mutDry.IsDryRun())

	rewriterDry := NewAddRewriter()
	mutRespDry := &dgoapi.Response{Uids: map[string]string{"DryEntity_1": "0x401"}}
	exDry := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x401","DryEntity.name":"Test"}]}`}
	errDry := runPostValidate(context.Background(), mutDry, exDry, rewriterDry, mutRespDry, nil)
	require.Error(t, errDry)
	require.Contains(t, errDry.Error(), "intercepted by dry run rule")

	// 2. Normal run: isDryRun == false -> !isDryRun is true, passes without error
	mutNormal := makeAddMutation(t, gqlSchema, `mutation @dryRun(enabled: false) {
		addDryEntity(input: [{name: "Test"}]) { dryEntity { id } }
	}`)
	require.False(t, mutNormal.IsDryRun())

	rewriterNormal := NewAddRewriter()
	mutRespNormal := &dgoapi.Response{Uids: map[string]string{"DryEntity_1": "0x402"}}
	exNormal := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x402","DryEntity.name":"Test"}]}`}
	errNormal := runPostValidate(context.Background(), mutNormal, exNormal, rewriterNormal, mutRespNormal, nil)
	require.NoError(t, errNormal)
}

// TestDryRun_ResolverCommitAbort verifies that mutation resolver aborts the transaction
// and rolls back immediately prior to Badger/Raft commit when @dryRun(enabled: true) is set.
func TestDryRun_ResolverCommitAbort(t *testing.T) {
	schemaStr := `
type TestAudit
  @generate(query: { get: true }, mutation: { add: true })
  @postValidate(
    add: {
      expr: "all(nodes, {.after.title != \"\"})"
    }
  ) {
  id:    ID!
  title: String @oldValue
}
`
	gqlSchema := test.LoadSchemaFromString(t, schemaStr)

	type trackingExecutor struct {
		fakeExecutor
		commitOrAbortCalled bool
		txnAborted          bool
	}

	exec := &trackingExecutor{
		fakeExecutor: fakeExecutor{
			queryResp: `{"postValidateNodes": [{"uid":"0x501","TestAudit.title":"Audit 1"}]}`,
		},
	}

	// Override CommitOrAbort to record invocation
	var commitCalled bool
	var aborted bool
	mockExec := &mockExecutorWithCommit{
		fakeExecutor: exec.fakeExecutor,
		onCommitOrAbort: func(tc *dgoapi.TxnContext) {
			commitCalled = true
			if tc != nil && tc.Aborted {
				aborted = true
			}
		},
	}

	resolver := NewDgraphResolver(NewAddRewriter(), mockExec)

	mut := makeAddMutation(t, gqlSchema, `mutation @dryRun(enabled: true) {
		addTestAudit(input: [{title: "Audit 1"}]) { testAudit { id } }
	}`)

	resolved, success := resolver.Resolve(context.Background(), mut)
	require.True(t, success)
	require.Nil(t, resolved.Err)
	require.True(t, commitCalled, "CommitOrAbort should be called to abort the transaction")
	require.True(t, aborted, "Transaction should be marked Aborted before Badger/Raft commit")

	// Payload data should return empty result (numUids = 0)
	require.NotNil(t, resolved.Data)
}

type mockExecutorWithCommit struct {
	fakeExecutor
	onCommitOrAbort func(tc *dgoapi.TxnContext)
}

func (m *mockExecutorWithCommit) Execute(ctx context.Context, req *dgoapi.Request, f schema.Field) (*dgoapi.Response, error) {
	if len(req.Mutations) > 0 {
		return &dgoapi.Response{
			Json: []byte(`{"updateTestUser": [{"uid":"0x1"},{"uid":"0x2"}]}`),
			Uids: map[string]string{"TestAudit_1": "0x501"},
			Txn:  &dgoapi.TxnContext{StartTs: 5555},
		}, nil
	}
	return m.fakeExecutor.Execute(ctx, req, f)
}

func (m *mockExecutorWithCommit) CommitOrAbort(ctx context.Context, tc *dgoapi.TxnContext) (*dgoapi.TxnContext, error) {
	if m.onCommitOrAbort != nil {
		m.onCommitOrAbort(tc)
	}
	return &dgoapi.TxnContext{}, nil
}

// TestDryRun_ProspectiveResultMatchesCommitted verifies that @dryRun(enabled: true)
// returns the prospective GraphQL payload (numUids, returned fields) matching what
// a committed mutation would return, while aborting the transaction before commit.
func TestDryRun_ProspectiveResultMatchesCommitted(t *testing.T) {
	schemaStr := `
type TestUser
  @generate(query: { get: true }, mutation: { add: true, update: true }) {
  id:        ID!
  email:     String!
  firstName: String
}
`
	gqlSchema := test.LoadSchemaFromString(t, schemaStr)

	var queryReq *dgoapi.Request
	var commitCalled bool
	var aborted bool

	mockExec := &mockExecutorWithCommit{
		fakeExecutor: fakeExecutor{
			queryResp: `{"testUser": [{"email":"test@gorillajobs.app","firstName":"xxxxxxzzz"},{"email":"chat-user2@gorillajobs.app","firstName":"xxxxxxzzz"}]}`,
		},
		onCommitOrAbort: func(tc *dgoapi.TxnContext) {
			commitCalled = true
			if tc != nil && tc.Aborted {
				aborted = true
			}
		},
	}

	exec := &dryRunQueryInspector{
		mock: mockExec,
		onExecute: func(req *dgoapi.Request) {
			if len(req.Mutations) == 0 {
				queryReq = req
			}
		},
	}

	resolver := NewDgraphResolver(NewUpdateRewriter(), exec)

	mut := makeAddMutation(t, gqlSchema, `mutation @dryRun(enabled: true) {
		updateTestUser(input: {
			filter: {}
			set: { firstName: "xxxxxxzzz" }
		}) {
			numUids
			testUser {
				email
				firstName
			}
		}
	}`)

	resolved, success := resolver.Resolve(context.Background(), mut)
	require.True(t, success)
	require.Nil(t, resolved.Err)
	require.True(t, commitCalled, "CommitOrAbort should be called to abort the transaction")
	require.True(t, aborted, "Transaction should be marked Aborted before commit")
	require.NotNil(t, queryReq, "Prospective query should be executed")
	require.False(t, queryReq.ReadOnly, "Prospective query must have ReadOnly: false to read uncommitted txn")
	require.Equal(t, uint64(5555), queryReq.StartTs, "Prospective query must pass StartTs to read uncommitted state")

	// Payload data should reflect returned users and numUids
	var res map[string]interface{}
	require.NoError(t, json.Unmarshal(resolved.Data, &res))
	updatePayload := res["updateTestUser"].(map[string]interface{})
	require.EqualValues(t, 2, updatePayload["numUids"])
	users := updatePayload["testUser"].([]interface{})
	require.Len(t, users, 2)
	u1 := users[0].(map[string]interface{})
	require.Equal(t, "test@gorillajobs.app", u1["email"])
	require.Equal(t, "xxxxxxzzz", u1["firstName"])

	require.NotNil(t, resolved.Extensions)
	require.True(t, resolved.Extensions.DryRun, "Extensions.DryRun must be true for dry-run mutations")
}

type dryRunQueryInspector struct {
	mock      *mockExecutorWithCommit
	onExecute func(req *dgoapi.Request)
}

func (d *dryRunQueryInspector) Execute(ctx context.Context, req *dgoapi.Request, f schema.Field) (*dgoapi.Response, error) {
	if d.onExecute != nil {
		d.onExecute(req)
	}
	return d.mock.Execute(ctx, req, f)
}

func (d *dryRunQueryInspector) CommitOrAbort(ctx context.Context, tc *dgoapi.TxnContext) (*dgoapi.TxnContext, error) {
	return d.mock.CommitOrAbort(ctx, tc)
}

// TestPostValidate_TxnStartTsInContext verifies that mutResp.Txn.StartTs is passed
// into the postValidate context as txn.startTs, allowing expressions and lambdas
// to query Dgraph within the uncommitted transaction.
func TestPostValidate_TxnStartTsInContext(t *testing.T) {
	schemaStr := `
type Placement
  @generate(query: { get: true }, mutation: { add: true })
  @postValidate(
    add: {
      expr: "txn.startTs > 0 ? true : error(\"txn.startTs missing or zero\")"
    }
  ) {
  id:     ID!
  status: String @oldValue
}
`
	gqlSchema := test.LoadSchemaFromString(t, schemaStr)

	// 1. With non-zero StartTs: should pass
	mutPass := makeAddMutation(t, gqlSchema, `mutation {
		addPlacement(input: [{status: "PENDING"}]) { placement { id } }
	}`)
	rewriterPass := NewAddRewriter()
	mutRespPass := &dgoapi.Response{
		Uids: map[string]string{"Placement_1": "0x601"},
		Txn:  &dgoapi.TxnContext{StartTs: 42001},
	}
	exPass := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x601","Placement.status":"PENDING"}]}`}
	errPass := runPostValidate(context.Background(), mutPass, exPass, rewriterPass, mutRespPass, nil)
	require.NoError(t, errPass)

	// 2. With zero StartTs: should fail with custom error
	mutFail := makeAddMutation(t, gqlSchema, `mutation {
		addPlacement(input: [{status: "PENDING"}]) { placement { id } }
	}`)
	rewriterFail := NewAddRewriter()
	mutRespFail := &dgoapi.Response{
		Uids: map[string]string{"Placement_1": "0x602"},
		Txn:  &dgoapi.TxnContext{StartTs: 0},
	}
	exFail := &fakeExecutor{queryResp: `{"postValidateNodes": [{"uid":"0x602","Placement.status":"PENDING"}]}`}
	errFail := runPostValidate(context.Background(), mutFail, exFail, rewriterFail, mutRespFail, nil)
	require.Error(t, errFail)
	require.Contains(t, errFail.Error(), "txn.startTs missing or zero")
}
