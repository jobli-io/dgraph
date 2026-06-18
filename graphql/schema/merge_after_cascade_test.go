/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

// Tests for @auth(mergeAfterCascade: true) — Stage 4 post-cascade interface merge.
//
// The feature ensures that an interface's auth rules are AND-merged into all
// implementing concrete types AFTER Stage 3 cascade expansion so that the check
// applies to every access path — including cascade-contributed OR branches.
//
// Algebraic expectation:
//
//	AND-policy type (@cascadeAuthPolicy aggregation:"and", default):
//	  final = AND(AND(base, cascadeBlock), ifaceAuth)
//
//	OR-policy type (@cascadeAuthPolicy aggregation:"or"):
//	  final = AND(OR(base, cascadeBlock), ifaceAuth)
//	        = OR(AND(base, ifaceAuth), AND(cascadeBlock, ifaceAuth))
//
// Each test uses buildSchema() to compile the schema through the full pipeline
// and then probes authRules[typeName].Rules.{Query,Add,Update,Delete} directly.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// Schema helpers
// ─────────────────────────────────────────────────────────────────────────────

// mergeAfterCascadeBase provides:
//   - Authority: concrete type with @auth that acts as a cascade parent.
//   - Manageable: interface with @auth(mergeAfterCascade: true).
//     The interface field is a plain Boolean (no @search on relation) to avoid
//     the @hasInverse requirement that Dgraph enforces on relation @search fields.
//     The auth rules use the `has: isManaged` predicate instead.
const mergeAfterCascadeBase = `

type Authority @auth(
	query: { rule: """
		query {
			queryAuthority { __typename }
		}
	""" }
) {
	id: ID!
	active: Boolean @search
}

interface Manageable
	@generate(
		query: { get: false, query: true, aggregate: false }
		mutation: { add: false, update: false, delete: false }
	)
	@auth(
		mergeAfterCascade: true
		query: {
			or: [
				{ rule: """
					query {
						queryManageable(filter: { not: { has: isManaged } }) { __typename }
					}
				""" }
				{ rule: """
					query {
						queryManageable(filter: { isManaged: true }) { __typename }
					}
				""" }
			]
		}
	)
{
	isManaged: Boolean @search
}
`

// andPolicyWidget — has own @auth + cascade + AND aggregation.
const andPolicyWidgetSchema = mergeAfterCascadeBase + `

type Parent @auth(
	query: { rule: """
		query {
			queryParent { __typename }
		}
	""" }
) {
	id: ID!
	hasWidget: [Widget] @hasInverse(field: inParent)
}

type Widget implements Manageable @cascadeAuthPolicy(aggregation: "and") @auth(
	query: { rule: """
		query {
			queryWidget { __typename }
		}
	""" }
) {
	id: ID!
	isManaged: Boolean @search
	inParent: Parent @cascadeAuth(operations: [query]) @search
}
`

// orPolicyWidget — has own @auth + cascade + OR aggregation.
const orPolicyWidgetSchema = mergeAfterCascadeBase + `

type Parent @auth(
	query: { rule: """
		query {
			queryParent { __typename }
		}
	""" }
) {
	id: ID!
	hasWidget: [Widget] @hasInverse(field: inParent)
}

type Widget implements Manageable @cascadeAuthPolicy(aggregation: "or") @auth(
	query: { rule: """
		query {
			queryWidget { __typename }
		}
	""" }
) {
	id: ID!
	isManaged: Boolean @search
	inParent: Parent @cascadeAuth(operations: [query]) @search
}
`

// noCascadeWidget — has own @auth, no cascade, just Stage 4 merge.
const noCascadeWidgetSchema = mergeAfterCascadeBase + `

type Widget implements Manageable @auth(
	query: { rule: """
		query {
			queryWidget { __typename }
		}
	""" }
) {
	id: ID!
	isManaged: Boolean @search
}
`

// noOwnAuthWidget — no own @auth; only cascade + Manageable Stage 4.
const noOwnAuthWidgetSchema = mergeAfterCascadeBase + `

type Parent @auth(
	query: { rule: """
		query {
			queryParent { __typename }
		}
	""" }
) {
	id: ID!
	hasWidget: [Widget] @hasInverse(field: inParent)
}

type Widget implements Manageable @cascadeAuthPolicy(aggregation: "or") {
	id: ID!
	isManaged: Boolean @search
	inParent: Parent @cascadeAuth(operations: [query]) @search
}
`

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestMergeAfterCascade_AndPolicy: AND-policy type → outermost node is AND.
//
//	final = AND(AND(ownAuth, cascadeBlock), manageableAuth)
func TestMergeAfterCascade_AndPolicy(t *testing.T) {
	sch := buildSchema(t, andPolicyWidgetSchema)

	widgetAuth := sch.authRules["Widget"]
	require.NotNil(t, widgetAuth, "Widget must have auth rules")
	require.NotNil(t, widgetAuth.Rules, "Widget.Rules must not be nil")

	q := widgetAuth.Rules.Query
	require.NotNil(t, q, "Widget.Rules.Query must not be nil")

	assert.True(t, isAnd(q),
		"AND-policy Widget.Rules.Query must be an AND node after Stage 4; got:\n%s",
		formatRuleNode(q, 0))
	assert.GreaterOrEqual(t, len(q.And), 2,
		"AND node must have ≥2 children (base+cascade and Manageable)")

	preds := collectCascadeInversePreds(q)
	assert.True(t, preds["inParent"],
		"Cascade edge inParent must appear in Widget.Rules.Query tree")

	assert.True(t, countLeaves(q) >= 3,
		"Rule tree must have ≥3 leaves: ownAuth, cascade, Manageable branches")
}

// TestMergeAfterCascade_OrPolicy: OR-policy type → outermost node is still AND.
//
//	final = AND(OR(ownAuth, cascadeBlock), manageableAuth)
func TestMergeAfterCascade_OrPolicy(t *testing.T) {
	sch := buildSchema(t, orPolicyWidgetSchema)

	widgetAuth := sch.authRules["Widget"]
	require.NotNil(t, widgetAuth)
	require.NotNil(t, widgetAuth.Rules)

	q := widgetAuth.Rules.Query
	require.NotNil(t, q)

	assert.True(t, isAnd(q),
		"OR-policy Widget.Rules.Query must be AND(..., manageable) at outermost level; got:\n%s",
		formatRuleNode(q, 0))
	require.GreaterOrEqual(t, len(q.And), 2)

	preds := collectCascadeInversePreds(q)
	assert.True(t, preds["inParent"],
		"Cascade edge inParent must appear in Widget.Rules.Query tree")
}

// TestMergeAfterCascade_NoCascade: no cascade edge → AND(ownAuth, manageableAuth).
func TestMergeAfterCascade_NoCascade(t *testing.T) {
	sch := buildSchema(t, noCascadeWidgetSchema)

	widgetAuth := sch.authRules["Widget"]
	require.NotNil(t, widgetAuth)
	require.NotNil(t, widgetAuth.Rules)

	q := widgetAuth.Rules.Query
	require.NotNil(t, q)

	assert.True(t, isAnd(q),
		"No-cascade Widget.Rules.Query must be AND(ownAuth, manageable); got:\n%s",
		formatRuleNode(q, 0))
	assert.GreaterOrEqual(t, len(q.And), 2)

	preds := collectCascadeInversePreds(q)
	assert.False(t, preds["inParent"], "No cascade edge should appear in rule tree")
}

// TestMergeAfterCascade_NoOwnAuth: no own @auth → AND(cascadeBlock, manageableAuth).
func TestMergeAfterCascade_NoOwnAuth(t *testing.T) {
	sch := buildSchema(t, noOwnAuthWidgetSchema)

	widgetAuth := sch.authRules["Widget"]
	require.NotNil(t, widgetAuth)
	require.NotNil(t, widgetAuth.Rules)

	q := widgetAuth.Rules.Query
	require.NotNil(t, q,
		"Widget.Rules.Query must not be nil — Manageable Stage 4 contributes even without own @auth")

	assert.True(t, isAnd(q),
		"No-own-auth Widget.Rules.Query must be AND(cascadeBlock, manageable); got:\n%s",
		formatRuleNode(q, 0))

	preds := collectCascadeInversePreds(q)
	assert.True(t, preds["inParent"], "Cascade edge inParent must be present")
}

// TestMergeAfterCascade_NonImplementor: non-implementing types must not receive Manageable auth.
func TestMergeAfterCascade_NonImplementor(t *testing.T) {
	const input = mergeAfterCascadeBase + `

type Bystander {
	id: ID!
	label: String
}

type Widget implements Manageable {
	id: ID!
	isManaged: Boolean @search
}
`
	s := buildSchema(t, input)

	bystanderAuth := s.authRules["Bystander"]
	if bystanderAuth != nil {
		assert.Nil(t, bystanderAuth.Rules,
			"Bystander (non-implementor) must not receive Manageable auth")
	}

	widgetAuth := s.authRules["Widget"]
	require.NotNil(t, widgetAuth)
	require.NotNil(t, widgetAuth.Rules)
	assert.NotNil(t, widgetAuth.Rules.Query,
		"Widget (Manageable implementor) must have query auth from Stage 4")
}

// TestMergeAfterCascade_InterfaceRulesCleared: the interface's TypeAuth.Rules must
// be cleared to nil after the pipeline (interface-reset pass runs after Stage 4).
func TestMergeAfterCascade_InterfaceRulesCleared(t *testing.T) {
	s := buildSchema(t, noCascadeWidgetSchema)

	manageableAuth := s.authRules["Manageable"]
	if manageableAuth != nil {
		assert.Nil(t, manageableAuth.Rules,
			"Manageable interface TypeAuth.Rules must be cleared after the pipeline")
	}
}

// TestMergeAfterCascade_QueryOnlyInterfaceDoesNotAffectOtherOps: interface only declares
// query: → Widget's add/update/delete must remain nil.
func TestMergeAfterCascade_QueryOnlyInterfaceDoesNotAffectOtherOps(t *testing.T) {
	s := buildSchema(t, noCascadeWidgetSchema)

	rules := s.authRules["Widget"].Rules
	assert.NotNil(t, rules.Query, "Query must be set by Stage 4")
	assert.Nil(t, rules.Add, "Add must be nil — Manageable has no add rule, Widget has none")
	assert.Nil(t, rules.Update, "Update must be nil")
	assert.Nil(t, rules.Delete, "Delete must be nil")
}

// TestMergeAfterCascade_AllOps: interface declares all four ops → all four are AND-merged.
func TestMergeAfterCascade_AllOps(t *testing.T) {
	const allOpsSchema = `

interface IRestrict
	@generate(
		query: { get: false, query: true, aggregate: false }
		mutation: { add: false, update: false, delete: false }
	)
	@auth(
		mergeAfterCascade: true
		query:  { rule: "query { queryIRestrict { __typename } }" }
		add:    { rule: "query { queryIRestrict { __typename } }" }
		update: { rule: "query { queryIRestrict { __typename } }" }
		delete: { rule: "query { queryIRestrict { __typename } }" }
	)
{
	active: Boolean @search
}

type Record implements IRestrict @auth(
	query:  { rule: "query { queryRecord { __typename } }" }
	add:    { rule: "query { queryRecord { __typename } }" }
	update: { rule: "query { queryRecord { __typename } }" }
	delete: { rule: "query { queryRecord { __typename } }" }
) {
	id: ID!
	active: Boolean @search
}
`
	s := buildSchema(t, allOpsSchema)

	rules := s.authRules["Record"].Rules
	require.NotNil(t, rules)

	assert.True(t, isAnd(rules.Query), "Record.Query must be AND(ownAuth, IRestrict)")
	assert.True(t, isAnd(rules.Add), "Record.Add must be AND(ownAuth, IRestrict)")
	assert.True(t, isAnd(rules.Update), "Record.Update must be AND(ownAuth, IRestrict)")
	assert.True(t, isAnd(rules.Delete), "Record.Delete must be AND(ownAuth, IRestrict)")
}

// TestMergeAfterCascade_NormalInterfaceUnaffected: a standard mergeInto:"or" interface
// is processed by Stage 2 (OR merge) and must NOT be touched by Stage 4.
func TestMergeAfterCascade_NormalInterfaceUnaffected(t *testing.T) {
	const normalSchema = `

interface INormal
	@generate(
		query: { get: false, query: true, aggregate: false }
		mutation: { add: false, update: false, delete: false }
	)
	@auth(
		mergeInto: "or"
		query: { rule: "query { queryINormal { __typename } }" }
	)
{
	active: Boolean @search
}

type NormalImpl implements INormal @auth(
	query: { rule: "query { queryNormalImpl { __typename } }" }
) {
	id: ID!
	active: Boolean @search
}
`
	s := buildSchema(t, normalSchema)

	rules := s.authRules["NormalImpl"].Rules
	require.NotNil(t, rules)

	// Stage 2 OR merge → top-level must be OR (Stage 4 must NOT have touched it).
	assert.True(t, isOr(rules.Query),
		"NormalImpl with mergeInto:\"or\" must be OR(ownAuth, ifaceAuth) from Stage 2; got:\n%s",
		formatRuleNode(rules.Query, 0))
}
