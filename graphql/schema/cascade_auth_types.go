/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"strings"

	"github.com/dgraph-io/gqlparser/v2/ast"
	"github.com/dgraph-io/gqlparser/v2/parser"
)

// CascadeAuthFieldConfig holds the parsed @cascadeAuth directive arguments
// for a single field.
type CascadeAuthFieldConfig struct {
	// Operations the cascade applies to (e.g. ["query", "add", "update", "delete"]).
	Operations []string
	// OperationsProvided is true when the operations: argument was explicitly
	// specified in the directive. When false, the default (all four operations)
	// should be used. When true with an empty slice, no operations are covered.
	OperationsProvided bool
	// Bidirectional propagates child auth rules back to the parent.
	Bidirectional bool
	// VariableContext controls @authVariables resolution: "parent", "self", or "adaptive".
	VariableContext string
	// Depth limits cascade hops (-1 = unlimited).
	Depth int
}

// CascadeAuthPolicyConfig holds the parsed @cascadeAuthPolicy directive for a type.
type CascadeAuthPolicyConfig struct {
	// Aggregation is "and" or "or" — how multiple incoming edges are combined.
	Aggregation string
	// SkipBidirectional suppresses bidirectional rule propagation for this type.
	SkipBidirectional bool
	// Skip suppresses all cascade auth expansion for this type. When true the
	// expander treats the type as if it has no incoming @cascadeAuth edges.
	// Use on audit/log types that implement a WorkspaceMember-style interface
	// for the edge field only, without wanting auth enforcement from the authority.
	Skip bool
}

// ---------------------------------------------------------------------------
// Methods on fieldDefinition
// ---------------------------------------------------------------------------

// CascadeAuthConfig returns the @cascadeAuth configuration for this field,
// or nil if the field does not carry @cascadeAuth.
func (fd *fieldDefinition) CascadeAuthConfig() *CascadeAuthFieldConfig {
	dir := fd.fieldDef.Directives.ForName(cascadeAuthDirective)
	if dir == nil {
		return nil
	}
	cfg := &CascadeAuthFieldConfig{Depth: -1}
	if v := dir.Arguments.ForName("operations"); v != nil {
		cfg.OperationsProvided = true
		for _, item := range v.Value.Children {
			cfg.Operations = append(cfg.Operations, item.Value.Raw)
		}
	}
	if v := dir.Arguments.ForName("bidirectional"); v != nil {
		cfg.Bidirectional = v.Value.Raw == "true"
	}
	if v := dir.Arguments.ForName("variableContext"); v != nil {
		cfg.VariableContext = v.Value.Raw
	}
	return cfg
}

// ---------------------------------------------------------------------------
// Methods on astType
// ---------------------------------------------------------------------------

// CascadeAuthPolicyConfig returns the @cascadeAuthPolicy configuration for
// this type, defaulting to aggregation="and" if absent.
func (t *astType) CascadeAuthPolicyConfig() CascadeAuthPolicyConfig {
	def := t.inSchema.schema.Types[t.typ.Name()]
	if def == nil {
		return CascadeAuthPolicyConfig{Aggregation: "and"}
	}
	dir := def.Directives.ForName("cascadeAuthPolicy")
	if dir == nil {
		return CascadeAuthPolicyConfig{Aggregation: "and"}
	}
	cfg := CascadeAuthPolicyConfig{Aggregation: "and"}
	if v := dir.Arguments.ForName("aggregation"); v != nil {
		// v.Value.Raw is the raw SDL token. For a string argument like
		// aggregation: "or", Raw is `"or"` (with surrounding double quotes).
		// Strip them so comparisons like `cfg.Aggregation == "or"` work correctly.
		cfg.Aggregation = strings.Trim(v.Value.Raw, `"`)
	}
	if v := dir.Arguments.ForName("skipBidirectional"); v != nil {
		cfg.SkipBidirectional = v.Value.Raw == "true"
	}
	if v := dir.Arguments.ForName("skip"); v != nil {
		cfg.Skip = v.Value.Raw == "true"
	}
	return cfg
}

// AuthVariables returns the @authVariables substitution map for this type,
// keyed by variable name with a list of allowed values.
//
// The @authVariables directive has shape:
//
//	@authVariables(vars: [AuthVariable!]!)
//	input AuthVariable { key: String! value: [String!]! }
//
// So the directive has one argument named "vars" whose value is a list of
// AuthVariable input objects, each with "key" and "value" children.
func (t *astType) AuthVariables() map[string][]string {
	def := t.inSchema.schema.Types[t.typ.Name()]
	if def == nil {
		return nil
	}
	dir := def.Directives.ForName(authVariablesDirective)
	if dir == nil {
		return nil
	}
	varsArg := dir.Arguments.ForName("vars")
	if varsArg == nil || varsArg.Value == nil {
		return nil
	}
	result := make(map[string][]string)
	// varsArg.Value is a list literal; each child is an AuthVariable object literal.
	for _, item := range varsArg.Value.Children {
		if item.Value == nil {
			continue
		}
		// item.Value is an object literal with fields "key" and "value".
		var key string
		var vals []string
		for _, field := range item.Value.Children {
			switch field.Name {
			case "key":
				if field.Value != nil {
					key = field.Value.Raw
				}
			case "value":
				if field.Value != nil {
					// field.Value is a list literal of strings.
					for _, v := range field.Value.Children {
						if v.Value != nil {
							vals = append(vals, v.Value.Raw)
						}
					}
				}
			}
		}
		if key != "" {
			result[key] = vals
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// ---------------------------------------------------------------------------
// Helper functions used by cascade_auth_expand.go
// ---------------------------------------------------------------------------

// unresolvedAuthVarKeys returns placeholder keys still unresolved after
// substitutAuthVars (i.e. still matching the {{…}} pattern).
func unresolvedAuthVarKeys(rule string) []string {
	var keys []string
	for {
		start := strings.Index(rule, "{{")
		if start == -1 {
			break
		}
		end := strings.Index(rule[start:], "}}")
		if end == -1 {
			break
		}
		keys = append(keys, rule[start+2:start+end])
		rule = rule[start+end+2:]
	}
	return keys
}

// gqlParseRuleForCascade parses a GraphQL query rule string into a RuleNode
// without enforcing that f.Name == "query"+authorityType (unlike gqlValidateRule).
// Used for cascaded rules that query an authority/interface type.
func gqlParseRuleForCascade(sch *schema, rule string, node *RuleNode) error {
	doc, gqlErr := parser.ParseQuery(&ast.Source{Input: rule})
	if gqlErr != nil {
		return gqlErr
	}
	if len(doc.Operations) != 1 {
		return nil
	}
	op := doc.Operations[0]
	if op == nil || op.Operation != "query" {
		return nil
	}
	if len(op.SelectionSet) != 1 {
		return nil
	}
	f, ok := op.SelectionSet[0].(*ast.Field)
	if !ok {
		return nil
	}
	opWrapper := &operation{
		op:                      op,
		query:                   rule,
		doc:                     doc,
		inSchema:                sch,
		interfaceImplFragFields: map[*ast.Field]string{},
	}
	recursivelyExpandFragmentSelections(f, opWrapper)
	node.Rule = &query{
		field: f,
		op:    opWrapper,
		sel:   op.SelectionSet[0],
	}
	node.Variables = op.VariableDefinitions
	return nil
}

// findInversePredicate — defined in cascade_auth_expand.go.

// hasNewLeaves — defined in cascade_auth_expand.go.

// ruleLeafKey returns a string that uniquely identifies a leaf RuleNode,
// incorporating the CascadeEdgePred so distinct cascade arms are treated
// as separate leaves even when they share the same underlying rule.
func ruleLeafKey(rn *RuleNode) string {
	if rn.DQLRule != nil {
		return "DQL:" + rn.DQLRule.Var + rn.CascadeEdgePred
	}
	if rn.RBACRule != nil {
		return "RBAC:" + rn.RBACRule.Variable + rn.RBACRule.Operator + rn.CascadeEdgePred
	}
	if rn.RuleTemplate != "" {
		return "RULE:" + rn.RuleTemplate + "|" + rn.CascadeEdgePred
	}
	return "LEAF:" + rn.CascadeEdgePred
}
