/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

// Tests for the @cascadeAuth compile-down pass (expandCascadeAuth).
//
// Each test:
//  1. Builds a schema via NewHandler → GQLSchema → FromString.
//  2. Type-asserts the returned Schema to *schema to access authRules directly.
//  3. Asserts the shape of the resulting RuleNode tree for the types under test.
//
// The structure probed is always sch.authRules[typeName].Rules.Query because
// query auth is the primary concern for cascaded read authorization.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hypermodeinc/dgraph/v25/x"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// buildSchema compiles an input schema string through the full pipeline and
// returns the internal *schema so tests can inspect authRules directly.
func buildSchema(t *testing.T, input string) *schema {
	t.Helper()
	handler, err := NewHandler(input, false)
	require.NoError(t, err, "NewHandler failed")
	sch, err := FromString(handler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err, "FromString failed")
	s, ok := sch.(*schema)
	require.True(t, ok, "Schema must be *schema")
	return s
}

// countLeaves counts the number of leaf Rule/DQLRule/RBACRule nodes in a RuleNode
// tree, recursively.
func countLeaves(rn *RuleNode) int {
	if rn == nil {
		return 0
	}
	if rn.Rule != nil || rn.DQLRule != nil || rn.RBACRule != nil {
		return 1
	}
	total := 0
	for _, child := range rn.Or {
		total += countLeaves(child)
	}
	for _, child := range rn.And {
		total += countLeaves(child)
	}
	if rn.Not != nil {
		total += countLeaves(rn.Not)
	}
	return total
}

// collectCascadeInversePreds returns the set of ALL cascade edge field names
// present in cascade leaf rules across the rule tree.
//
// With the new uid_in architecture, the inverse predicate (e.g.
// "WorkspaceMember.inWorkspace") is stamped directly onto RuleNode as
// CascadeInversePred / CascadeBundlePred, rather than being embedded in the
// GQL template text. This helper checks both the new fields AND scans any
// legacy template text, so it works with both old and new architectures.
//
// The returned keys are the *local* field names extracted from the Dgraph
// predicate (e.g. "WorkspaceMember.inWorkspace" → "inWorkspace").
// Pure-own-auth leaves produce no entries.
func collectCascadeInversePreds(rn *RuleNode) map[string]bool {
	out := make(map[string]bool)

	// addPred extracts the local field name (after the last dot) from a Dgraph
	// predicate and stores it in out. e.g. "WorkspaceMember.inWorkspace" → "inWorkspace".
	addPred := func(pred string) {
		if pred == "" {
			return
		}
		if i := strings.LastIndex(pred, "."); i >= 0 {
			out[pred[i+1:]] = true
		} else {
			out[pred] = true
		}
	}

	// extractWrappersFromTemplate walks a reconstructed GQL template and
	// collects all field names that appear as cascade wrapper levels.
	// It skips the first field ("queryXxx"), "__typename", and any args.
	extractWrappersFromTemplate := func(tpl string) {
		depth := 0    // brace depth
		fieldIdx := 0 // which brace-block we're in (0=operation, 1=queryXxx, 2+=wrappers)
		i := 0
		n := len(tpl)
		for i < n {
			switch tpl[i] {
			case '{':
				depth++
				fieldIdx = depth
				i++
			case '}':
				depth--
				i++
			case '(':
				// Skip argument lists entirely.
				depth2 := 1
				i++
				for i < n && depth2 > 0 {
					if tpl[i] == '(' {
						depth2++
					} else if tpl[i] == ')' {
						depth2--
					}
					i++
				}
			case ' ', '\t', '\n', '\r':
				i++
			case '$':
				// Skip variable definitions (used in operation signature).
				for i < n && tpl[i] != ',' && tpl[i] != ')' {
					i++
				}
			default:
				// Read an identifier.
				j := i
				for j < n && tpl[j] != '{' && tpl[j] != '}' &&
					tpl[j] != ' ' && tpl[j] != '\t' &&
					tpl[j] != '\n' && tpl[j] != ':' &&
					tpl[j] != '(' && tpl[j] != ')' &&
					tpl[j] != ',' {
					j++
				}
				word := tpl[i:j]
				i = j
				// depth inside queryXxx body = 2 — those are cascade wrappers.
				// depth=0 is "query", depth=1 is queryXxx — skip both.
				if word == "" || word == "__typename" || word == "query" {
					continue
				}
				if fieldIdx >= 2 && !strings.HasPrefix(word, "query") {
					out[word] = true
				}
			}
		}
	}

	var walk func(*RuleNode)
	walk = func(n *RuleNode) {
		if n == nil {
			return
		}
		// CascadeBundlePred can appear on composite nodes (bundle wrappers with
		// And/Or children), so we check it unconditionally before any leaf guard.
		addPred(n.CascadeBundlePred)

		if n.Rule != nil {
			// Leaf node: also check inverse pred and scan legacy template text.
			addPred(n.CascadeInversePred)
			if tpl := n.RuleTemplate; tpl != "" {
				extractWrappersFromTemplate(tpl)
			}
			return
		}
		if n.DQLRule != nil || n.RBACRule != nil {
			return
		}
		for _, c := range n.Or {
			walk(c)
		}
		for _, c := range n.And {
			walk(c)
		}
		walk(n.Not)
	}
	walk(rn)
	return out
}

// isAnd returns true if rn is a composite AND node (len(And) > 0).
func isAnd(rn *RuleNode) bool { return rn != nil && len(rn.And) > 0 }

// isOr returns true if rn is a composite OR node (len(Or) > 0).
func isOr(rn *RuleNode) bool { return rn != nil && len(rn.Or) > 0 }

// formatRuleNode renders a RuleNode tree as an indented human-readable string
// for manual inspection. Call via logAuthRules() inside a test.
func formatRuleNode(rn *RuleNode, depth int) string {
	if rn == nil {
		return strings.Repeat("  ", depth) + "<nil>\n"
	}
	indent := strings.Repeat("  ", depth)
	var sb strings.Builder

	// Helper: append cascade metadata annotations for any node.
	appendCascadeMeta := func() {
		if rn.CascadeBundlePred != "" {
			fmt.Fprintf(&sb, "%s  ↳ bundlePred=%q rootType=%q\n",
				indent, rn.CascadeBundlePred, rn.CascadeRootType)
		}
		if rn.CascadeInversePred != "" {
			fmt.Fprintf(&sb, "%s  ↳ inversePred=%q\n", indent, rn.CascadeInversePred)
		}
	}

	switch {
	case len(rn.And) > 0:
		fmt.Fprintf(&sb, "%sAND (%d children):\n", indent, len(rn.And))
		appendCascadeMeta()
		for i, child := range rn.And {
			fmt.Fprintf(&sb, "%s  [%d]\n", indent, i)
			sb.WriteString(formatRuleNode(child, depth+2))
		}
	case len(rn.Or) > 0:
		fmt.Fprintf(&sb, "%sOR (%d children):\n", indent, len(rn.Or))
		appendCascadeMeta()
		for i, child := range rn.Or {
			fmt.Fprintf(&sb, "%s  [%d]\n", indent, i)
			sb.WriteString(formatRuleNode(child, depth+2))
		}
	case rn.Not != nil:
		fmt.Fprintf(&sb, "%sNOT:\n", indent)
		appendCascadeMeta()
		sb.WriteString(formatRuleNode(rn.Not, depth+1))
	case rn.RBACRule != nil:
		fmt.Fprintf(&sb, "%sRBAC leaf: var=%q op=%q operand=%v chain=%v\n",
			indent, rn.RBACRule.Variable, rn.RBACRule.Operator, rn.RBACRule.Operand,
			rn.CascadeEdgeForwardPath)
		appendCascadeMeta()
	case rn.DQLRule != nil:
		fmt.Fprintf(&sb, "%sDQL leaf: func=%v typeFilter=%q\n",
			indent, rn.DQLRule.Func, rn.CascadeEdgePredTypeFilter)
		appendCascadeMeta()
	case rn.Rule != nil:
		tpl := rn.RuleTemplate
		kind := "cascade-synthesised"
		if tpl == "" {
			// Original @auth rule — the Template is only populated in the cascade path.
			// Fall back to the Query field name as a breadcrumb.
			tpl = fmt.Sprintf("(original @auth rule for field %q — run logAuthRules before cascade expansion for full GQL)", rn.Rule.Name())
			kind = "original @auth"
		}
		fmt.Fprintf(&sb, "%sGQL leaf [%s]: chain=%v typeFilter=%q\n%srule:\n%s\n",
			indent, kind, rn.CascadeEdgeForwardPath, rn.CascadeEdgePredTypeFilter, indent, tpl)
		appendCascadeMeta()
	default:
		fmt.Fprintf(&sb, "%s<empty node>\n", indent)
		appendCascadeMeta()
	}
	return sb.String()
}

// logAuthRules logs the query RuleNode tree for typeName via t.Logf so it
// appears in `go test -v` output for manual inspection.
func logAuthRules(t *testing.T, s *schema, typeName string) {
	t.Helper()
	auth := s.authRules[typeName]
	if auth == nil || auth.Rules == nil || auth.Rules.Query == nil {
		t.Logf("[%s] query auth: <none>", typeName)
		return
	}
	t.Logf("[%s] query auth tree (leaves=%d):\n%s",
		typeName, countLeaves(auth.Rules.Query),
		formatRuleNode(auth.Rules.Query, 1))
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 1: two-level cascade (WorkspaceMember → Workspace)
//
//	Workspace @auth(query:{...}) { name: String }
//	WorkspaceMember interface { inWorkspace: Workspace @cascadeAuth() }
//	Group implements WorkspaceMember @auth(query:{...}) { name: String }
//
// Expected: Group.query = AND(Group own auth, Workspace auth via inWorkspace edge)
// ─────────────────────────────────────────────────────────────────────────────
func TestCascadeAuth_TwoLevel(t *testing.T) {
	const input = `
type Workspace @auth(
  query: { rule: """
    query {
      queryWorkspace { __typename }
    }
  """ }
) {
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth()
}

type Group implements WorkspaceMember @auth(
  query: { rule: """
    query {
      queryGroup { __typename }
    }
  """ }
) {
  name: String
}
`
	s := buildSchema(t, input)
	logAuthRules(t, s, "Workspace")
	logAuthRules(t, s, "Group")

	groupAuth := s.authRules["Group"]
	require.NotNil(t, groupAuth, "Group must have auth rules")
	require.NotNil(t, groupAuth.Rules, "Group.Rules must not be nil")
	require.NotNil(t, groupAuth.Rules.Query, "Group.Rules.Query must not be nil")

	q := groupAuth.Rules.Query

	// The final rule must be a composite of Group's own rule AND the cascade arm.
	// The cascade arm should have CascadeEdgePred set to the inWorkspace predicate.
	t.Run("group query auth is composite AND", func(t *testing.T) {
		assert.True(t, isAnd(q), "Group's query rule should be combined with AND")
	})

	t.Run("group query auth has inWorkspace cascade edge", func(t *testing.T) {
		preds := collectCascadeInversePreds(q)
		hasCascade := false
		for pred := range preds {
			if pred == "inWorkspace" {
				hasCascade = true
				break
			}
		}
		assert.True(t, hasCascade, "At least one leaf should have inWorkspace in chain")
	})

	t.Run("group query auth has exactly 2 leaf rules", func(t *testing.T) {
		// 1 own rule + 1 workspace cascade rule
		assert.Equal(t, 2, countLeaves(q))
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 2: three-level cascade (Company → Group → Workspace)
//
//	Workspace @auth(query:{W})   { name }
//	WorkspaceMember { inWorkspace @cascadeAuth() }
//	Group implements WorkspaceMember @auth(query:{G}) { name }
//	GroupMember { inGroup: Group @cascadeAuth() }
//	Company implements GroupMember { name }
//
// Propagation rules:
//   - Group.query  = AND(G, W via inWorkspace)  — 2 leafs
//   - Company.query = (W via inWorkspace via inGroup) AND (G via inGroup) — 2 leafs,
//     both wrapped by the inGroup edge pred.
//
// ─────────────────────────────────────────────────────────────────────────────
func TestCascadeAuth_ThreeLevel(t *testing.T) {
	const input = `
type Workspace @auth(
  query: { rule: """
    query {
      queryWorkspace { __typename }
    }
  """ }
) {
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth()
}

type Group implements WorkspaceMember @auth(
  query: { rule: """
    query {
      queryGroup { __typename }
    }
  """ }
) {
  name: String
  hasCompanies: [Company] @hasInverse(field: inGroup)
}

interface GroupMember {
  inGroup: Group @cascadeAuth()
}

type Company implements GroupMember {
  name: String
}
`
	s := buildSchema(t, input)
	logAuthRules(t, s, "Workspace")
	logAuthRules(t, s, "Group")
	logAuthRules(t, s, "Company")

	// ── Group ────────────────────────────────────────────────────────────────
	t.Run("Group receives Workspace cascade", func(t *testing.T) {
		groupAuth := s.authRules["Group"]
		require.NotNil(t, groupAuth)
		require.NotNil(t, groupAuth.Rules)
		require.NotNil(t, groupAuth.Rules.Query)
		q := groupAuth.Rules.Query
		assert.True(t, isAnd(q), "Group query should be AND composite")
		assert.Equal(t, 2, countLeaves(q), "Group should have 2 leaves: own + Workspace cascade")
		preds := collectCascadeInversePreds(q)
		hasCascade := false
		for pred := range preds {
			if pred == "inWorkspace" {
				hasCascade = true
			}
		}
		assert.True(t, hasCascade, "Group should have inWorkspace in chain")
	})

	// ── Company ──────────────────────────────────────────────────────────────
	t.Run("Company inherits combined Group+Workspace auth via inGroup", func(t *testing.T) {
		companyAuth := s.authRules["Company"]
		require.NotNil(t, companyAuth, "Company must have auth (from cascadeAuth)")
		require.NotNil(t, companyAuth.Rules)
		require.NotNil(t, companyAuth.Rules.Query)
		q := companyAuth.Rules.Query

		// Company has no own @auth — its entire auth comes from cascading Group.
		// Group's combined auth (G + W_via_inWorkspace) is pulled down and wrapped
		// in a bundle node carrying inGroup, preserving the full lineage.
		t.Run("is composite (AND)", func(t *testing.T) {
			assert.True(t, isAnd(q), "Company query should be AND composite")
		})

		t.Run("carries exactly 2 leaf rules (Group and Workspace)", func(t *testing.T) {
			assert.Equal(t, 2, countLeaves(q))
		})

		t.Run("outermost edge pred is inGroup (bundle node)", func(t *testing.T) {
			preds := collectCascadeInversePreds(q)
			assert.True(t, preds["inGroup"],
				"expected inGroup cascade pred on bundle node; got: %v", preds)
		})

		t.Run("inner Workspace leaf preserves inWorkspace lineage", func(t *testing.T) {
			preds := collectCascadeInversePreds(q)
			assert.True(t, preds["inWorkspace"],
				"expected inWorkspace to be preserved inside bundle; got: %v", preds)
		})
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 3: cascadeAuthPolicy OR aggregation
//
//	 Workspace @auth(query:{W}) { name }
//	 WorkspaceMember { inWorkspace @cascadeAuth() }
//	 Group implements WorkspaceMember @auth(query:{G}) @cascadeAuthPolicy(aggregation:"or") { name }
//
//	Expected: Group.query = OR(G, W via inWorkspace)
//
// ─────────────────────────────────────────────────────────────────────────────
func TestCascadeAuth_ORPolicy(t *testing.T) {
	const input = `
type Workspace @auth(
  query: { rule: """
    query {
      queryWorkspace { __typename }
    }
  """ }
) {
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth()
}

type Group implements WorkspaceMember
  @auth(
    query: { rule: """
      query {
        queryGroup { __typename }
      }
    """ }
  )
  @cascadeAuthPolicy(aggregation: "or")
{
  name: String
}
`
	s := buildSchema(t, input)
	logAuthRules(t, s, "Workspace")
	logAuthRules(t, s, "Group")

	groupAuth := s.authRules["Group"]
	require.NotNil(t, groupAuth)
	require.NotNil(t, groupAuth.Rules)
	require.NotNil(t, groupAuth.Rules.Query)
	q := groupAuth.Rules.Query

	t.Run("group query auth combined with OR when policy is 'or'", func(t *testing.T) {
		assert.True(t, isOr(q), "Group query should be combined with OR when @cascadeAuthPolicy(aggregation:\"or\")")
	})
	t.Run("has exactly 2 leaf rules", func(t *testing.T) {
		assert.Equal(t, 2, countLeaves(q))
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 4: cascadeAuthPolicy AND aggregation (default)
//
// When no @cascadeAuthPolicy is present the default is AND.
// ─────────────────────────────────────────────────────────────────────────────
func TestCascadeAuth_DefaultANDPolicy(t *testing.T) {
	const input = `
type Workspace @auth(
  query: { rule: """
    query {
      queryWorkspace { __typename }
    }
  """ }
) {
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth()
}

type Group implements WorkspaceMember @auth(
  query: { rule: """
    query {
      queryGroup { __typename }
    }
  """ }
) {
  name: String
}
`
	s := buildSchema(t, input)
	logAuthRules(t, s, "Workspace")
	logAuthRules(t, s, "Group")

	q := s.authRules["Group"].Rules.Query
	t.Run("default policy is AND", func(t *testing.T) {
		assert.True(t, isAnd(q), "Without @cascadeAuthPolicy, combination should be AND")
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 5: No auth on authority — schema validation rejects this
//
// The system validates at schema-build time that @cascadeAuth cannot point to a
// type with no @auth directive, because it would be a security no-op (the
// parent provides no rules to propagate). NewHandler must return an error.
// ─────────────────────────────────────────────────────────────────────────────
func TestCascadeAuth_NoAuthorityAuth_IsRejectedAtSchemaTime(t *testing.T) {
	const input = `
type Workspace {
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth()
}

type Group implements WorkspaceMember @auth(
  query: { rule: """
    query {
      queryGroup { __typename }
    }
  """ }
) {
  name: String
}
`
	_, err := NewHandler(input, false)
	require.Error(t, err, "NewHandler must return an error when @cascadeAuth target has no @auth")
	require.Contains(t, err.Error(), "no @auth directive",
		"error message should explain that the target type has no @auth directive")
}

// ─────────────────────────────────────────────────────────────────────────────
// Validation: @cascadeAuth does NOT require @hasInverse.
//
// The cascade expand pass reconstructs the GQL rule as a forward @cascade
// traversal (e.g. queryGroup { inWorkspace { __typename } }), which does not
// need the inverse link at all. @hasInverse is only needed by the filter/
// inverse-predicate query path, which is a separate code path.
// ─────────────────────────────────────────────────────────────────────────────
func TestCascadeAuth_HasInverse_IsRequired(t *testing.T) {
	// Schema with no @hasInverse on either side — must now be REJECTED because
	// the uid_in architecture requires the inverse predicate to construct the
	// index-based filter. Without @hasInverse, the expand pass cannot determine
	// the inverse predicate and returns a validation error at schema compile time.
	const input = `
type Workspace @auth(
  query: { rule: """
    query {
      queryWorkspace { __typename }
    }
  """ }
) {
  name: String
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth()
}

type Group implements WorkspaceMember @auth(
  query: { rule: """
    query {
      queryGroup { __typename }
    }
  """ }
) {
  name: String
}
`
	_, err := NewHandler(input, false)
	require.Error(t, err,
		"@cascadeAuth without @hasInverse must fail — the uid_in pattern "+
			"requires the inverse predicate to construct the index-based filter")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 6: Determinism — building the same schema twice must produce identical
//
//	CascadeEdgePred sets and leaf counts.
//
// ─────────────────────────────────────────────────────────────────────────────
func TestCascadeAuth_Determinism(t *testing.T) {
	const input = `
type Workspace @auth(
  query: { rule: """
    query {
      queryWorkspace { __typename }
    }
  """ }
) {
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth()
}

type Group implements WorkspaceMember @auth(
  query: { rule: """
    query {
      queryGroup { __typename }
    }
  """ }
) {
  name: String
  hasCompanies: [Company] @hasInverse(field: inGroup)
}

interface GroupMember {
  inGroup: Group @cascadeAuth()
}

type Company implements GroupMember {
  name: String
}
`
	const N = 5
	var allLeafCounts []int
	var allPredSets []map[string]bool

	for i := 0; i < N; i++ {
		s := buildSchema(t, input)
		if i == 0 {
			logAuthRules(t, s, "Workspace")
			logAuthRules(t, s, "Group")
			logAuthRules(t, s, "Company")
		}
		companyAuth := s.authRules["Company"]
		require.NotNil(t, companyAuth)
		require.NotNil(t, companyAuth.Rules)
		require.NotNil(t, companyAuth.Rules.Query)
		allLeafCounts = append(allLeafCounts, countLeaves(companyAuth.Rules.Query))
		allPredSets = append(allPredSets, collectCascadeInversePreds(companyAuth.Rules.Query))
	}

	t.Run("leaf count is stable across schema recompilations", func(t *testing.T) {
		first := allLeafCounts[0]
		for i, c := range allLeafCounts {
			assert.Equal(t, first, c, "iteration %d produced different leaf count", i)
		}
	})

	t.Run("cascade edge pred set is stable across schema recompilations", func(t *testing.T) {
		first := allPredSets[0]
		for i, ps := range allPredSets {
			assert.Equal(t, first, ps, "iteration %d produced different cascade pred set", i)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 7: Company inherits from Group via interface — OR policy at Group level
//
//	       propagates naturally to Company (Company still receives full Group auth)
//
//	Workspace @auth(W) → Group @auth(G) @cascadeAuthPolicy(or) → Company
//
//	Group.query = OR(G, W via inWorkspace)                          [2 leafs]
//	Company.query = Group combined (G OR W) via inGroup            [2 leafs]
//
// ─────────────────────────────────────────────────────────────────────────────
func TestCascadeAuth_ThreeLevelGroupORPropagates(t *testing.T) {
	const input = `
type Workspace @auth(
  query: { rule: """
    query {
      queryWorkspace { __typename }
    }
  """ }
) {
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth()
}

type Group implements WorkspaceMember
  @auth(
    query: { rule: """
      query {
        queryGroup { __typename }
      }
    """ }
  )
  @cascadeAuthPolicy(aggregation: "or")
{
  name: String
  hasCompanies: [Company] @hasInverse(field: inGroup)
}

interface GroupMember {
  inGroup: Group @cascadeAuth()
}

type Company implements GroupMember {
  name: String
}
`
	s := buildSchema(t, input)
	logAuthRules(t, s, "Workspace")
	logAuthRules(t, s, "Group")
	logAuthRules(t, s, "Company")

	// Verify Group gets OR(G, W)
	groupQ := s.authRules["Group"].Rules.Query
	require.NotNil(t, groupQ)
	t.Run("Group.query is OR(G, W)", func(t *testing.T) {
		assert.True(t, isOr(groupQ))
		assert.Equal(t, 2, countLeaves(groupQ))
	})

	// Verify Company pulls down the full Group combined auth (OR(G, W) via inGroup)
	companyAuth := s.authRules["Company"]
	require.NotNil(t, companyAuth)
	require.NotNil(t, companyAuth.Rules)
	require.NotNil(t, companyAuth.Rules.Query)
	companyQ := companyAuth.Rules.Query

	t.Run("Company.query has 2 leafs (inherits Group's OR block through inGroup)", func(t *testing.T) {
		assert.Equal(t, 2, countLeaves(companyQ))
	})

	t.Run("All Company leafs go through inGroup", func(t *testing.T) {
		preds := collectCascadeInversePreds(companyQ)
		// The outermost bundle node carries inGroup.
		assert.True(t, preds["inGroup"],
			"Company should have inGroup as outermost bundle pred; preds: %v", preds)
		// The inner Workspace leaf retains its inWorkspace cascade field inside the bundle.
		assert.True(t, preds["inWorkspace"],
			"Company should have inWorkspace preserved inside bundle; preds: %v", preds)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Interface auth tests
//
// Interface auth is AND-merged into concrete implementing types during
// authRules() — BEFORE expandCascadeAuth runs.  So by the time the cascade
// pass executes it already sees the combined (interface AND concrete) auth as
// the concrete type's own rules.
//
// The tests below verify:
//  8.  Interface @auth alone (no concrete @auth) — concrete type picks up the
//      interface rule; cascade wraps it correctly.
//  9.  Interface @auth + concrete @auth — the concrete type has AND(I, C);
//      cascade wraps the full AND block.
// 10.  Three-level with interface auth at the WorkspaceMember level:
//      Group.query = AND(I_wm, G, W_cascade)  [3 leaf rules]
//      Company.query = pulls Group's full auth through inGroup edge.
// 11.  Interface auth on the GroupMember side:
//      The GroupMember interface auth (I_gm) is AND-merged into Company, then
//      the cascade also stitches in Group's combined auth.
// ─────────────────────────────────────────────────────────────────────────────

// Test 8: Interface has @auth, concrete type has no @auth.
//
//	Workspace @auth(W) { name }
//	WorkspaceMember @auth(I_wm) { inWorkspace @cascadeAuth() }
//	Group implements WorkspaceMember { name }   ← no own @auth
//
// Before cascade: Group.query = I_wm  (merged from interface)
// After  cascade: Group.query = AND(I_wm, W via inWorkspace)  [2 leafs]
func TestCascadeAuth_InterfaceAuthOnly_TwoLevel(t *testing.T) {
	const input = `
type Workspace @auth(
  query: { rule: """
    query {
      queryWorkspace { __typename }
    }
  """ }
) {
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember @auth(
  query: { rule: """
    query {
      queryWorkspaceMember { __typename }
    }
  """ }
) {
  inWorkspace: Workspace @cascadeAuth()
}

type Group implements WorkspaceMember {
  name: String
}
`
	s := buildSchema(t, input)
	logAuthRules(t, s, "Workspace")
	logAuthRules(t, s, "WorkspaceMember")
	logAuthRules(t, s, "Group")

	groupAuth := s.authRules["Group"]
	require.NotNil(t, groupAuth, "Group must have auth (from interface)")
	require.NotNil(t, groupAuth.Rules)
	require.NotNil(t, groupAuth.Rules.Query)
	q := groupAuth.Rules.Query

	t.Run("group query auth is AND(interface, cascade)", func(t *testing.T) {
		assert.True(t, isAnd(q), "Group query rule should be AND-composite")
	})

	t.Run("has exactly 2 leaf rules: interface auth + Workspace cascade", func(t *testing.T) {
		assert.Equal(t, 2, countLeaves(q))
	})

	t.Run("cascade edge pred is inWorkspace", func(t *testing.T) {
		preds := collectCascadeInversePreds(q)
		hasCascade := false
		for pred := range preds {
			if pred == "inWorkspace" {
				hasCascade = true
			}
		}
		assert.True(t, hasCascade, "One leaf should carry a inWorkspace inverse pred")
	})
}

// Test 9: Interface @auth + concrete @auth + cascade.
//
//	Workspace @auth(W) { name }
//	WorkspaceMember @auth(I_wm) { inWorkspace @cascadeAuth() }
//	Group implements WorkspaceMember @auth(G) { name }
//
// Before cascade: Group.query = AND(I_wm, G)        [2 leafs — interface merged in]
// After  cascade: Group.query = AND(I_wm, G, W_via_inWorkspace)  [3 leafs]
func TestCascadeAuth_InterfaceAndConcreteAuth_TwoLevel(t *testing.T) {
	const input = `
type Workspace @auth(
  query: { rule: """
    query {
      queryWorkspace { __typename }
    }
  """ }
) {
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember @auth(
  query: { rule: """
    query {
      queryWorkspaceMember { __typename }
    }
  """ }
) {
  inWorkspace: Workspace @cascadeAuth()
}

type Group implements WorkspaceMember @auth(
  query: { rule: """
    query {
      queryGroup { __typename }
    }
  """ }
) {
  name: String
}
`
	s := buildSchema(t, input)
	logAuthRules(t, s, "Workspace")
	logAuthRules(t, s, "WorkspaceMember")
	logAuthRules(t, s, "Group")

	groupAuth := s.authRules["Group"]
	require.NotNil(t, groupAuth)
	require.NotNil(t, groupAuth.Rules)
	require.NotNil(t, groupAuth.Rules.Query)
	q := groupAuth.Rules.Query

	t.Run("group query auth is composite AND", func(t *testing.T) {
		assert.True(t, isAnd(q))
	})

	t.Run("has exactly 3 leaf rules: interface + concrete + cascade", func(t *testing.T) {
		// AND(I_wm, G) from interface merge, then cascade appends W_via_inWorkspace.
		assert.Equal(t, 3, countLeaves(q))
	})

	t.Run("one leaf carries inWorkspace cascade pred", func(t *testing.T) {
		preds := collectCascadeInversePreds(q)
		hasCascade := false
		for pred := range preds {
			if pred == "inWorkspace" {
				hasCascade = true
			}
		}
		assert.True(t, hasCascade, "should have inWorkspace in chain")
	})
}

// Test 10: Three levels, interface auth on WorkspaceMember only.
//
//	Workspace @auth(W)                                               { name }
//	WorkspaceMember @auth(I_wm) { inWorkspace @cascadeAuth() }
//	Group implements WorkspaceMember @auth(G)                        { name }
//	GroupMember                    { inGroup: Group @cascadeAuth() }
//	Company implements GroupMember                                   { name }
//
// Group.query (before cascade) = AND(I_wm, G)
// Group.query (after  cascade) = AND(I_wm, G, W_via_inWorkspace)              [3 leafs]
// Company.query                 = Group's 3-leaf AND, all via inGroup edge     [3 leafs]
func TestCascadeAuth_ThreeLevel_WithInterfaceAuth(t *testing.T) {
	const input = `
type Workspace @auth(
  query: { rule: """
    query {
      queryWorkspace { __typename }
    }
  """ }
) {
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember @auth(
  query: { rule: """
    query {
      queryWorkspaceMember { __typename }
    }
  """ }
) {
  inWorkspace: Workspace @cascadeAuth()
}

type Group implements WorkspaceMember @auth(
  query: { rule: """
    query {
      queryGroup { __typename }
    }
  """ }
) {
  name: String
  hasCompanies: [Company] @hasInverse(field: inGroup)
}

interface GroupMember {
  inGroup: Group @cascadeAuth()
}

type Company implements GroupMember {
  name: String
}
`
	s := buildSchema(t, input)
	logAuthRules(t, s, "Workspace")
	logAuthRules(t, s, "WorkspaceMember")
	logAuthRules(t, s, "Group")
	logAuthRules(t, s, "Company")

	// ── Group assertions ─────────────────────────────────────────────────────
	t.Run("Group query has 3 leaves: I_wm + G + W_cascade", func(t *testing.T) {
		groupAuth := s.authRules["Group"]
		require.NotNil(t, groupAuth)
		require.NotNil(t, groupAuth.Rules)
		require.NotNil(t, groupAuth.Rules.Query)
		q := groupAuth.Rules.Query
		assert.True(t, isAnd(q), "Group query should be AND composite")
		assert.Equal(t, 3, countLeaves(q))
		preds := collectCascadeInversePreds(q)
		hasCascade := false
		for pred := range preds {
			if pred == "inWorkspace" {
				hasCascade = true
			}
		}
		assert.True(t, hasCascade, "Group should have inWorkspace in chain")
	})

	// ── Company assertions ───────────────────────────────────────────────────
	t.Run("Company query inherits all 3 Group leaves through inGroup", func(t *testing.T) {
		companyAuth := s.authRules["Company"]
		require.NotNil(t, companyAuth)
		require.NotNil(t, companyAuth.Rules)
		require.NotNil(t, companyAuth.Rules.Query)
		q := companyAuth.Rules.Query
		assert.Equal(t, 3, countLeaves(q), "Company inherits Group's 3-rule AND block")
		preds := collectCascadeInversePreds(q)
		// Outermost wrapper field is inGroup.
		assert.True(t, preds["inGroup"],
			"Company should have inGroup as outermost bundle pred; preds: %v", preds)
		// The inner Workspace leaf retains its inWorkspace field inside the bundle.
		assert.True(t, preds["inWorkspace"],
			"Company should have inWorkspace preserved inside bundle; preds: %v", preds)
	})
}

// Test 11: Interface auth on GroupMember (the cascading interface itself).
//
//	Workspace @auth(W) { name }
//	WorkspaceMember { inWorkspace @cascadeAuth() }
//	Group @auth(G)                                                   { name }
//	GroupMember @auth(I_gm) { inGroup: Group @cascadeAuth() }
//	Company implements GroupMember                                   { name }
//
// Group.query  (after cascade) = AND(G, W_via_inWorkspace)              [2 leafs]
// Company.query (after merge+cascade) = AND(I_gm, Group_combined_via_inGroup) [3 leafs]
//
//	where Group_combined is AND(G, W_via_inWorkspace) → 2 leaves fronted by inGroup
func TestCascadeAuth_GroupMemberInterfaceAuth(t *testing.T) {
	const input = `
type Workspace @auth(
  query: { rule: """
    query {
      queryWorkspace { __typename }
    }
  """ }
) {
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth()
}

type Group implements WorkspaceMember @auth(
  query: { rule: """
    query {
      queryGroup { __typename }
    }
  """ }
) {
  name: String
  hasCompanies: [Company] @hasInverse(field: inGroup)
}

interface GroupMember @auth(
  query: { rule: """
    query {
      queryGroupMember { __typename }
    }
  """ }
) {
  inGroup: Group @cascadeAuth()
}

type Company implements GroupMember {
  name: String
}
`
	s := buildSchema(t, input)
	logAuthRules(t, s, "Workspace")
	logAuthRules(t, s, "Group")
	logAuthRules(t, s, "GroupMember")
	logAuthRules(t, s, "Company")

	// Group should be AND(G, W_via_inWorkspace): 2 leafs.
	t.Run("Group absorbs Workspace cascade: 2 leaves", func(t *testing.T) {
		groupAuth := s.authRules["Group"]
		require.NotNil(t, groupAuth)
		require.NotNil(t, groupAuth.Rules)
		require.NotNil(t, groupAuth.Rules.Query)
		q := groupAuth.Rules.Query
		assert.Equal(t, 2, countLeaves(q))
	})

	// Company before cascade = AND(I_gm) [interface merge]
	// Company after cascade = AND(I_gm, Group_combined_via_inGroup)
	// Group_combined has 2 leaves → Company total = 1 (I_gm) + 2 (Group) = 3 leafs.
	t.Run("Company query has 3 leaves: I_gm + AND(G, W) via inGroup", func(t *testing.T) {
		companyAuth := s.authRules["Company"]
		require.NotNil(t, companyAuth)
		require.NotNil(t, companyAuth.Rules)
		require.NotNil(t, companyAuth.Rules.Query)
		q := companyAuth.Rules.Query
		assert.True(t, isAnd(q))
		assert.Equal(t, 3, countLeaves(q))
	})

	t.Run("At least one Company leaf carries inGroup cascade pred", func(t *testing.T) {
		q := s.authRules["Company"].Rules.Query
		preds := collectCascadeInversePreds(q)
		assert.True(t, preds["inGroup"],
			"Company should have inGroup in its inverse chain; got: %v", preds)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers for variable-handling tests
// ─────────────────────────────────────────────────────────────────────────────

// collectLeafTemplates walks a RuleNode tree and returns all non-empty
// RuleTemplate strings found in GQL leaf nodes (cascade-synthesised rules).
// These templates still carry the raw $VARIABLE placeholders from the
// authority's @auth rule — substitution happens at query time.
func collectLeafTemplates(rn *RuleNode) []string {
	var out []string
	var walk func(*RuleNode)
	walk = func(n *RuleNode) {
		if n == nil {
			return
		}
		if n.Rule != nil {
			if n.RuleTemplate != "" {
				out = append(out, n.RuleTemplate)
			}
			return
		}
		for _, c := range n.And {
			walk(c)
		}
		for _, c := range n.Or {
			walk(c)
		}
		walk(n.Not)
	}
	walk(rn)
	return out
}

// templatesContaining returns the subset of templates that contain all of the
// given substrings.
func templatesContaining(templates []string, substrings ...string) []string {
	var out []string
	for _, tpl := range templates {
		ok := true
		for _, s := range substrings {
			if !strings.Contains(tpl, s) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, tpl)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Test: variable placeholders survive cascade GQL reconstruction
//
// The authority's @auth rule uses $EMAIL. After cascade expansion the
// synthesised child rule (queryGroup { inWorkspace { … } }) must still
// contain the raw "$EMAIL" placeholder — substitution only happens at
// query-evaluation time based on the JWT claims.
// ─────────────────────────────────────────────────────────────────────────────
func TestCascadeAuth_Variable_PlaceholderPreservedInCascadeLeaf(t *testing.T) {
	const input = `
type User {
  email: String! @id
}

type Workspace @auth(
  query: { rule: """
    query($EMAIL: String!) {
      queryWorkspace {
        inUsers(filter: { email: { eq: $EMAIL } }) { __typename }
      }
    }
  """ }
) {
  name: String
  inUsers: [User]
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth()
}

type Group implements WorkspaceMember @auth(
  query: { rule: """
    query($EMAIL: String!) {
      queryGroup {
        inUsers(filter: { email: { eq: $EMAIL } }) { __typename }
      }
    }
  """ }
) {
  name: String
  inUsers: [User]
}
`
	s := buildSchema(t, input)
	logAuthRules(t, s, "Workspace")
	logAuthRules(t, s, "Group")

	groupAuth := s.authRules["Group"]
	require.NotNil(t, groupAuth)
	require.NotNil(t, groupAuth.Rules)
	require.NotNil(t, groupAuth.Rules.Query)

	q := groupAuth.Rules.Query

	t.Run("group query is composite AND (own + cascade)", func(t *testing.T) {
		assert.True(t, isAnd(q))
		assert.Equal(t, 2, countLeaves(q))
	})

	t.Run("cascade leaf rule template preserves $EMAIL placeholder", func(t *testing.T) {
		// The cascade leaf carries the authority's (Workspace) GQL auth rule with
		// $EMAIL preserved verbatim for JWT substitution at query time.
		// Under the uid_in architecture the template no longer contains the forward
		// inWorkspace traversal — only the authority's own query body.
		templates := collectLeafTemplates(q)
		cascadeLeafs := templatesContaining(templates, "$EMAIL")
		assert.NotEmpty(t, cascadeLeafs,
			"At least one cascade leaf should embed '$EMAIL'; templates: %v", templates)
	})

	t.Run("cascade leaf rule template contains inWorkspace traversal", func(t *testing.T) {
		preds := collectCascadeInversePreds(q)
		assert.True(t, preds["inWorkspace"],
			"Cascade leaf must traverse inWorkspace edge; preds: %v", preds)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Test: multiple variables in authority's rule — all placeholders preserved
//
// Workspace uses both $EMAIL and $DOMAIN in its @auth rule (compound AND filter).
// After cascade expansion, the synthesised child rule for Group must preserve
// BOTH placeholders so the query-time rewriter can substitute them from the JWT.
// ─────────────────────────────────────────────────────────────────────────────
func TestCascadeAuth_Variable_MultipleVarsPreservedInCascadeLeaf(t *testing.T) {
	const input = `
type User {
  email: String! @id
  domain: String @search(by: [exact])
}

type Workspace @auth(
  query: { rule: """
    query($EMAIL: String!, $DOMAIN: String!) {
      queryWorkspace {
        inUsers(filter: { and: [
          { email: { eq: $EMAIL } }
          { domain: { eq: $DOMAIN } }
        ]}) { __typename }
      }
    }
  """ }
) {
  name: String
  inUsers: [User]
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth()
}

type Group implements WorkspaceMember @auth(
  query: { rule: """
    query($EMAIL: String!) {
      queryGroup {
        inUsers(filter: { email: { eq: $EMAIL } }) { __typename }
      }
    }
  """ }
) {
  name: String
  inUsers: [User]
}
`
	s := buildSchema(t, input)
	logAuthRules(t, s, "Workspace")
	logAuthRules(t, s, "Group")

	groupAuth := s.authRules["Group"]
	require.NotNil(t, groupAuth)
	require.NotNil(t, groupAuth.Rules)
	require.NotNil(t, groupAuth.Rules.Query)

	q := groupAuth.Rules.Query

	t.Run("group query is composite AND (own + cascade)", func(t *testing.T) {
		assert.True(t, isAnd(q))
		assert.Equal(t, 2, countLeaves(q))
	})

	t.Run("cascade leaf template preserves both $EMAIL and $DOMAIN placeholders", func(t *testing.T) {
		templates := collectLeafTemplates(q)
		// Under the uid_in architecture the template holds the authority's own
		// query body (without forward inWorkspace traversal), but both variables
		// must still be present for JWT substitution at query time.
		cascadeLeafs := templatesContaining(templates, "$EMAIL", "$DOMAIN")
		assert.NotEmpty(t, cascadeLeafs,
			"cascade leaf must embed both $EMAIL and $DOMAIN; templates: %v", templates)
	})

	t.Run("cascade leaf preserves compound AND filter structure", func(t *testing.T) {
		templates := collectLeafTemplates(q)
		// The authority uses `and: [{email},{domain}]` — the reconstruction must
		// not flatten or drop either predicate.
		cascadeLeafs := templatesContaining(templates, "email", "domain")
		assert.NotEmpty(t, cascadeLeafs,
			"cascade leaf must preserve both filter fields (email, domain); templates: %v", templates)
	})

	t.Run("cascade edge traversal is inWorkspace", func(t *testing.T) {
		preds := collectCascadeInversePreds(q)
		assert.True(t, preds["inWorkspace"],
			"cascade leaf must traverse inWorkspace; preds: %v", preds)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Test: variableContext "self" — child's @authVariables remaps placeholder
//
// With variableContext: "self", the cascade leaf for Group should use Group's
// own @authVariables mapping (EMAIL → USER_EMAIL) so that the reconstructed
// rule substitutes $USER_EMAIL (the child's claim) instead of $EMAIL.
// ─────────────────────────────────────────────────────────────────────────────
func TestCascadeAuth_Variable_SelfContextRemapsPlaceholder(t *testing.T) {
	const input = `
type User {
  email: String! @id
}

type Workspace @auth(
  query: { rule: """
    query($EMAIL: String!) {
      queryWorkspace {
        inUsers(filter: { email: { eq: $EMAIL } }) { __typename }
      }
    }
  """ }
) {
  name: String
  inUsers: [User]
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth(variableContext: "self")
}

type Group implements WorkspaceMember
  @authVariables(vars: [{key: "EMAIL", value: ["USER_EMAIL"]}])
  @auth(
    query: { rule: """
      query($USER_EMAIL: String!) {
        queryGroup {
          inUsers(filter: { email: { eq: $USER_EMAIL } }) { __typename }
        }
      }
    """ }
  )
{
  name: String
  inUsers: [User]
}
`
	s := buildSchema(t, input)
	logAuthRules(t, s, "Workspace")
	logAuthRules(t, s, "Group")

	groupAuth := s.authRules["Group"]
	require.NotNil(t, groupAuth)
	require.NotNil(t, groupAuth.Rules)
	require.NotNil(t, groupAuth.Rules.Query)

	q := groupAuth.Rules.Query

	t.Run("group query is composite AND (own + cascade)", func(t *testing.T) {
		assert.True(t, isAnd(q))
		assert.Equal(t, 2, countLeaves(q))
	})

	t.Run("cascade leaf rule traverses inWorkspace edge", func(t *testing.T) {
		preds := collectCascadeInversePreds(q)
		assert.True(t, preds["inWorkspace"],
			"cascade leaf must traverse inWorkspace even with variableContext:self; preds: %v", preds)
	})

	t.Run("group's own leaf rule uses $USER_EMAIL (child's claim)", func(t *testing.T) {
		// The own-auth leaf (queryGroup { inUsers… }) should use $USER_EMAIL.
		templates := collectLeafTemplates(q)
		ownLeafs := templatesContaining(templates, "queryGroup", "inUsers", "$USER_EMAIL")
		assert.NotEmpty(t, ownLeafs,
			"Group's own leaf rule must use $USER_EMAIL; templates: %v", templates)
	})
}
