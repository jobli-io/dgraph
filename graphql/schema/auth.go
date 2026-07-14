/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/spf13/cast"

	"github.com/dgraph-io/gqlparser/v2/ast"
	"github.com/dgraph-io/gqlparser/v2/gqlerror"
	"github.com/dgraph-io/gqlparser/v2/parser"
	"github.com/dgraph-io/gqlparser/v2/validator"
	"github.com/hypermodeinc/dgraph/v25/dql"
)

const (
	RBACQueryPrefix = "{"
)

type RBACQuery struct {
	Variable string
	Operator string
	Operand  interface{}
	regex    *regexp.Regexp
}

type RuleNode struct {
	Or        []*RuleNode
	And       []*RuleNode
	Not       *RuleNode
	Rule      Query
	DQLRule   *dql.GraphQuery
	RBACRule  *RBACQuery
	Variables ast.VariableDefinitionList
	// CascadeEdgePred is the Dgraph predicate for the cascade traversal edge
	// (e.g. "WorkspaceMember.inWorkspace"). Set by withCascadeEdgePred in
	// cascade_auth_expand.go and read by the DQL auth rewriter.
	CascadeEdgePred string
	// CascadeEdgePredTypeFilter, when non-empty, causes the DQL rewriter to emit
	// @filter(type(X)) on the edge traversal block for this leaf rule.
	CascadeEdgePredTypeFilter string
	// RuleTemplate is the raw (pre-substitution) GraphQL rule string on leaf nodes.
	// Used by cascade_auth_expand.go to re-substitute child @authVariables.
	RuleTemplate string
	// CascadeBundlePred marks a composite bundle node that wraps the authority
	// type's combined auth rules behind a uid_in traversal. Set on AND/OR composite
	// nodes whose children are the authority type's own rules (post-merge).
	// Value is the Dgraph predicate for the cascade edge (same format as CascadeEdgePred).
	CascadeBundlePred string
	// CascadeRootType is the Dgraph type name of the authority (root) node for a
	// cascade bundle (e.g. "Workspace"). Used by formatRuleNode for diagnostics.
	CascadeRootType string
	// CascadeAuthAggregation is the @cascadeAuthPolicy(aggregation) value of the
	// authority type for this bundle node (e.g. "or" when Group declares
	// aggregation:"or"). Set by cascadeAuthRuleForEdge; read by rewriteCascadeBundle
	// to decide whether grandparent uid_in filters are ANDed or ORed onto the
	// authority var's filter (e.g. Group's own IAMResource filter OR inWorkspace).
	CascadeAuthAggregation string
	// CascadeWrapPred is set on a CascadeWrap node representing:
	//   uid_in(CascadeWrapPred, uid(var that satisfies CascadeWrapInner))
	// This is the replacement for the CascadeBundlePred / rewriteCascadeBundle mechanism.
	// When the DQL rewriter encounters this node it:
	//   1. Rewrites CascadeWrapInner to obtain (supportVars, innerFilter)
	//   2. Allocates: "varN as var(func: type(CascadeWrapType)) @filter(innerFilter) @cascade"
	//   3. Returns uid_in(CascadeWrapPred, uid(varN)) as the child-type filter.
	CascadeWrapPred string
	// CascadeWrapType is the Dgraph type name for the authority var block (e.g. "Group").
	CascadeWrapType string
	// CascadeWrapInner is the authority type's full compiled auth rule tree
	// (own @auth merged with its own cascade parents per its aggregation policy).
	CascadeWrapInner *RuleNode
	// CascadeWrapReverse, when true, instructs the DQL query rewriter to compile
	// this CascadeWrap node using a REVERSE lookup strategy (traversing from the
	// authority to the child) rather than a FORWARD lookup strategy.
	CascadeWrapReverse bool
	// CascadeInversePred is the Dgraph predicate that the uid_in filter traverses
	// to scope the child type from the authority's side. On leaf cascade nodes this
	// mirrors CascadeEdgePred; on bundle nodes CascadeBundlePred takes precedence.
	CascadeInversePred string
	// CascadeEdgeForwardPath records the cascade hop chain for a leaf rule node.
	// Each entry is a predicate name in the forward traversal order (e.g.
	// ["inGroup", "inWorkspace"] for a Company→Group→Workspace chain).
	CascadeEdgeForwardPath []string
	// CascadeThroughType marks a synthetic "pass-all" leaf node produced for
	// through-nodes: intermediate types with no own @auth that sit in a multi-level
	// cascade chain (e.g. Company in AdPostRecord→Company→Group→Workspace).
	// Value is the Dgraph type name (e.g. "Company"). The DQL rewriter emits:
	//   var(func: type(Company))   with no @cascade filter
	// and applies grandparent uid_in filters onto that var.
	CascadeThroughType string
}

type AuthContainer struct {
	Password *RuleNode
	Query    *RuleNode
	Add      *RuleNode
	Update   *RuleNode
	Delete   *RuleNode
}

type RuleResult int

const (
	Uncertain RuleResult = iota
	Positive
	Negative
)

func (rq *RBACQuery) checkIfMatchInArray(array []interface{}) RuleResult {
	for _, v := range array {
		if rq.checkIfMatch(v) == Positive {
			return Positive
		}
	}
	return Negative
}

func (rq *RBACQuery) checkIfMatch(value interface{}) RuleResult {
	rules, ok := rq.Operand.([]interface{})
	if ok {
		// this means rule operand is array slice
		for _, r := range rules {
			if evaluate(r, value, rq.regex) == Positive {
				return Positive
			}
		}
		return Negative
	}
	return evaluate(rq.Operand, value, rq.regex)
}

func evaluate(operand interface{}, value interface{}, regex *regexp.Regexp) RuleResult {
	if regex != nil {
		sval, ok := value.(string)
		if ok && regex.MatchString(sval) {
			return Positive
		}
		return Negative
	}

	if reflect.DeepEqual(value, operand) {
		return Positive
	}

	return Negative
}

// EvaluateRBACRule evaluates the auth token based on the auth query
// There are two cases here:
// 1. Auth token has an array of values for the variable.
// 2. Auth token has non-array value for the variable.
// match would be deep equal except for regex match in case of regexp operator.
// In case array one match would made the rule positive.
// For example, Rule {$USER: { eq:"uid"}} and token $USER:["u", "id", "uid"] result in match.
// Rule {$USER: { in: ["uid", "xid"]}} and token $USER:["u", "id", "uid"]  result in match
func (rq *RBACQuery) EvaluateRBACRule(av map[string]interface{}) RuleResult {
	tokenValues, tokenCastErr := cast.ToSliceE(av[rq.Variable])
	// if eq, auth rule value will be matched completely
	// if regexp, auth rule value should always be string and so as token values
	// if in, auth rule will only have array as the value check has to consider that
	if tokenCastErr != nil {
		// this means value for variable in token in not an array
		return rq.checkIfMatch(av[rq.Variable])
	}
	return rq.checkIfMatchInArray(tokenValues)
}

func (node *RuleNode) staticEvaluation(av map[string]interface{}) RuleResult {
	for _, v := range node.Variables {
		if val, ok := av[v.Variable]; !ok || val == nil {
			return Negative
		}
	}
	return Uncertain
}

func (node *RuleNode) EvaluateStatic(av map[string]interface{}) RuleResult {
	if node == nil {
		return Uncertain
	}

	hasUncertain := false
	for _, rule := range node.Or {
		val := rule.EvaluateStatic(av)
		if val == Positive {
			return Positive
		} else if val == Uncertain {
			hasUncertain = true
		}
	}

	if len(node.Or) > 0 && !hasUncertain {
		return Negative
	}

	for _, rule := range node.And {
		val := rule.EvaluateStatic(av)
		if val == Negative {
			return Negative
		} else if val == Uncertain {
			hasUncertain = true
		}
	}

	if len(node.And) > 0 && !hasUncertain {
		return Positive
	}

	if node.Not != nil {
		// In the case of a non-RBAC query, the result indicates whether the query has all the
		// variables in order to evaluate it. Hence, we don't need to negate the value.
		result := node.Not.EvaluateStatic(av)
		if node.Not.RBACRule == nil {
			return result
		}
		switch result {
		case Uncertain:
			return Uncertain
		case Positive:
			return Negative
		case Negative:
			return Positive
		}
	}

	if node.RBACRule != nil {
		return node.RBACRule.EvaluateRBACRule(av)
	}

	if node.Rule != nil {
		return node.staticEvaluation(av)
	}

	// CascadeWrap node: defer to the inner rule for static evaluation.
	// This preserves the "closed-by-default" invariant: if the cascade
	// authority's required variables are absent from the JWT, the whole
	// CascadeWrap evaluates as Negative so that any parent AND also
	// evaluates as Negative (deny-all) rather than dropping the arm.
	if node.CascadeWrapPred != "" && node.CascadeWrapInner != nil {
		return node.CascadeWrapInner.EvaluateStatic(av)
	}

	return Uncertain
}

type TypeAuth struct {
	Rules  *AuthContainer
	Fields map[string]*AuthContainer
}

func authRules(sch *schema) (map[string]*TypeAuth, error) {
	s := sch.schema
	//TODO: Add position in error.
	var errResult, err error
	authRules := make(map[string]*TypeAuth)

	for _, typ := range s.Types {
		name := typeName(typ)
		authRules[name] = &TypeAuth{Fields: make(map[string]*AuthContainer)}
		auth := typ.Directives.ForName(authDirective)
		if auth != nil {
			authRules[name].Rules, err = parseAuthDirective(sch, typ, auth)
			errResult = AppendGQLErrs(errResult, err)
		}

		for _, field := range typ.Fields {
			auth := field.Directives.ForName(authDirective)
			if auth != nil {
				authRules[name].Fields[field.Name], err = parseAuthDirective(sch, typ, auth)
				errResult = AppendGQLErrs(errResult, err)
			}
		}
	}

	// Resolve each type's own compile-time @authVariables ({{KEY}} placeholders)
	// into its own auth rules NOW, before cascade expansion runs.
	// parseAuthNode stores {{KEY}} rules as RuleTemplate-only (Rule==nil) because
	// the GraphQL parser cannot handle the {{}} syntax. We must substitute here so
	// that when cascadeAuthRuleForEdge reads ta.Rules.Query for an authority type,
	// it finds fully-parsed RuleNodes, not bare templates.
	for _, typ := range s.Types {
		name := typeName(typ)
		ta := authRules[name]
		if ta == nil {
			continue
		}
		authorityAstType := &astType{
			typ:             &ast.Type{NamedType: name},
			inSchema:        sch,
			dgraphPredicate: sch.dgraphPredicate,
		}
		ownVars := authorityAstType.AuthVariables()
		if len(ownVars) == 0 {
			continue
		}
		if ta.Rules != nil {
			ta.Rules.Query = resolveTemplateLeaves(sch, ta.Rules.Query, ownVars, name, false)
			ta.Rules.Add = resolveTemplateLeaves(sch, ta.Rules.Add, ownVars, name, false)
			ta.Rules.Update = resolveTemplateLeaves(sch, ta.Rules.Update, ownVars, name, false)
			ta.Rules.Delete = resolveTemplateLeaves(sch, ta.Rules.Delete, ownVars, name, false)
		}
		for field, ac := range ta.Fields {
			if ac == nil {
				continue
			}
			ta.Fields[field] = &AuthContainer{
				Query:    resolveTemplateLeaves(sch, ac.Query, ownVars, name, false),
				Add:      resolveTemplateLeaves(sch, ac.Add, ownVars, name, false),
				Update:   resolveTemplateLeaves(sch, ac.Update, ownVars, name, false),
				Delete:   resolveTemplateLeaves(sch, ac.Delete, ownVars, name, false),
				Password: resolveTemplateLeaves(sch, ac.Password, ownVars, name, false),
			}
		}
	}

	// Snapshot before interface merging: used by expandCascadeAuth to propagate
	// only a type's own declared @auth bidirectionally (not the interface-level auth).
	authRulesOwnOnly := snapshotTypeAuthMap(authRules)

	// ── Auth rule composition ─────────────────────────────────────────────────
	// The final auth rule for each type is the composition of three stages:
	//
	// Stage 1 — Own @auth (completed above):
	//   Each type's @auth directive is parsed into per-op rules (query, add,
	//   update, delete). @authVariables substitution is applied at this stage.
	//
	// Stage 2 — Interface auth merge (this loop):
	//   For each interface the type implements, its auth rule is merged into
	//   the type's own rule. Merge-op priority (highest → lowest):
	//     (a) @auth(interfacePolicy: [{interface: "X", merge: "or"}]) on the
	//         concrete type — per-interface override.
	//     (b) @auth(mergeInto: "or") on the interface — default for all
	//         implementors of that interface.
	//     (c) "and" — hard-coded fallback.
	//   Result after this loop: base = mergeOp(typeAuth, interfaceAuth)
	//   for each implemented interface.
	//
	// Stage 3 — Cascade auth combination (expandCascadeAuth below):
	//   For each incoming @cascadeAuth edge, buildCascadeRule() constructs a
	//   CascadeWrap node representing the authority type's full auth. Multiple
	//   edge rules are AND-aggregated first (or OR-aggregated when policy is
	//   "or") to form a single cascade block, which is then merged into the
	//   Stage-2 base:
	//     "and" policy: final = AND(base, cascadeBlock)  — restricts access
	//     "or"  policy: final = OR(base,  cascadeBlock)  — adds an access path
	//   Aggregation is resolved from @cascadeAuthPolicy(aggregation:) on the
	//   child type, defaulting to "and" when no type-level policy is declared.
	// ─────────────────────────────────────────────────────────────────────────

	// Merge the Auth rules on interfaces into the implementing types.
	// The merge operator is determined by a two-level priority system:
	//  1. interfacePolicy on the concrete type (highest priority):
	//       @auth(interfacePolicy: [{ interface: "X", merge: "or" }])
	//       @auth(interfacePolicy: [{ interface: "X", merge: "or", operations: ["add"] }])
	//  2. mergeInto on the interface (default for all implementors):
	//       interface X @auth(mergeInto: "or")
	//  3. AND (hard-coded default — lowest priority).
	//
	// When an interfacePolicy entry specifies operations, the override only
	// applies to those operations; the remaining ones fall back to mergeInto / AND.
	for _, typ := range s.Types {
		name := typeName(typ)
		if typ.Kind == ast.Object {
			// Build the per-interface merge-policy override map from interfacePolicy.
			concreteInterfacePolicy := parseInterfacePolicy(typ)

			for _, intrface := range typ.Interfaces {
				interfaceDef := s.Types[intrface]
				if interfaceDef == nil {
					continue
				}
				interfaceName := typeName(interfaceDef)
				if authRules[interfaceName] == nil || authRules[interfaceName].Rules == nil {
					continue
				}

				// Skip interfaces marked mergeAfterCascade: true — those are handled
				// exclusively by Stage 4 (mergePostCascadeInterfaceAuth). Processing
				// them here as well would double-apply the interface auth: once as part
				// of the OR/AND with cascade, and again as the post-cascade AND, producing
				// redundant DQL auth blocks (e.g. duplicate Auth2≡Auth9 var blocks).
				if ifaceAuth := interfaceDef.Directives.ForName(authDirective); ifaceAuth != nil {
					if mac := ifaceAuth.Arguments.ForName("mergeAfterCascade"); mac != nil && mac.Value.Raw == "true" {
						continue
					}
				}

				// Determine the interface-level default merge op (mergeInto or "and").
				defaultMergeOp := "and"
				if iface := interfaceDef.Directives.ForName(authDirective); iface != nil {
					if mi := iface.Arguments.ForName("mergeInto"); mi != nil {
						defaultMergeOp = mi.Value.Raw
					}
				}

				// Per-(interface, operation) override from interfacePolicy.
				entry, hasEntry := concreteInterfacePolicy[interfaceName]

				authRules[name].Rules = mergeAuthRulesWithPolicy(
					authRules[name].Rules,
					authRules[interfaceName].Rules,
					defaultMergeOp,
					entry, hasEntry,
				)
			}
		}
	}

	// Second resolveTemplateLeaves pass — re-sweep concrete types after interface
	// auth has been merged in.
	//
	// Interface @auth rules use {{KEY}} templates with empty placeholder values
	// (e.g. IAMResource declares @authVariables(vars: [{key:"QRY_PERMISSIONS", value:[]}]))
	// as stubs for implementing types to override. The first resolveTemplateLeaves
	// pass (pre-merge, above) ran with each type's OWN @authVariables, so interface
	// rules stayed unresolved (empty vals were skipped). Now that each concrete type's
	// auth tree contains the merged interface rules, we can re-resolve them using the
	// concrete type's non-empty @authVariables — e.g. JobAd's QRY_PERMISSIONS
	// = [_ALL _JOBAD UPDATE ...] fills in IAMResource's empty placeholder.
	//
	// We only target concrete types (Kind == Object) with non-empty authVariables,
	// and only nodes that are STILL unresolved (Rule == nil, RuleTemplate != "").
	for _, typ := range s.Types {
		if typ.Kind != ast.Object {
			continue
		}
		name := typeName(typ)
		ta := authRules[name]
		if ta == nil {
			continue
		}
		concreteAstType := &astType{
			typ:             &ast.Type{NamedType: name},
			inSchema:        sch,
			dgraphPredicate: sch.dgraphPredicate,
		}
		concreteVars := concreteAstType.AuthVariables()
		if len(concreteVars) == 0 {
			continue
		}
		if ta.Rules != nil {
			ta.Rules.Query = resolveTemplateLeaves(sch, ta.Rules.Query, concreteVars, name, true)
			ta.Rules.Add = resolveTemplateLeaves(sch, ta.Rules.Add, concreteVars, name, true)
			ta.Rules.Update = resolveTemplateLeaves(sch, ta.Rules.Update, concreteVars, name, true)
			ta.Rules.Delete = resolveTemplateLeaves(sch, ta.Rules.Delete, concreteVars, name, true)
		}
		for field, ac := range ta.Fields {
			if ac == nil {
				continue
			}
			ta.Fields[field] = &AuthContainer{
				Query:    resolveTemplateLeaves(sch, ac.Query, concreteVars, name, true),
				Add:      resolveTemplateLeaves(sch, ac.Add, concreteVars, name, true),
				Update:   resolveTemplateLeaves(sch, ac.Update, concreteVars, name, true),
				Delete:   resolveTemplateLeaves(sch, ac.Delete, concreteVars, name, true),
				Password: resolveTemplateLeaves(sch, ac.Password, concreteVars, name, true),
			}
		}
	}

	// Post-substitution validation: after both resolveTemplateLeaves passes, scan
	// all rule nodes for remaining <<KEY>> placeholders. An unresolved placeholder
	// means the key is either misspelled or not declared in @authVariables — the
	// rule would silently become a no-op, leaving the type unprotected.
	for typName, ta := range authRules {
		if ta == nil {
			continue
		}
		if ta.Rules != nil {
			if err := scanUnresolvedInContainer(ta.Rules, typName); err != nil {
				errResult = AppendGQLErrs(errResult, err)
			}
		}
		for field, ac := range ta.Fields {
			if ac == nil {
				continue
			}
			if err := scanUnresolvedInContainer(ac, typName+"."+field); err != nil {
				errResult = AppendGQLErrs(errResult, err)
			}
		}
	}

	// Expand @cascadeAuth directives: propagate parent @auth rules into child
	// TypeAuth entries. This runs after interface auth has been merged into
	// concrete types (so each type has its full rule set) but before interfaces
	// are reset to empty (so the function can still read interface auth for
	// implementor-union rules).
	if err = expandCascadeAuth(sch, authRules, authRulesOwnOnly); err != nil {
		errResult = AppendGQLErrs(errResult, err)
	}

	// Stage 4 — Post-cascade interface auth merge.
	//
	// For interfaces whose @auth carries mergeAfterCascade: true, AND their auth
	// rules into each implementing concrete type AFTER Stage 3 cascade expansion.
	//
	// Motivation: Stage 2 runs before cascade, so for OR-policy types
	// (@cascadeAuthPolicy aggregation:"or") a Stage-2 AND-merge produces:
	//   OR(AND(base, ifaceAuth), cascadeBlock)  ← cascade escapes the interface check
	//
	// By deferring to Stage 4 we get:
	//   AND(OR(base, cascadeBlock), ifaceAuth)
	//   = OR(AND(base, ifaceAuth), AND(cascadeBlock, ifaceAuth))  ← both restricted
	//
	// For AND-policy types the result is equivalent:
	//   AND(AND(base, cascadeBlock), ifaceAuth) = AND(base, cascadeBlock, ifaceAuth)
	//
	// The merge operator is always AND — the interface acts as a universal restriction
	// applied to every access path regardless of the type's cascadeAuthPolicy.
	if err = mergePostCascadeInterfaceAuth(sch, authRules); err != nil {
		errResult = AppendGQLErrs(errResult, err)
	}

	// Reinitialize the Interface's auth to be empty as Any operation on interface
	// will be broken into an operation on subsequent implementing types and auth rules
	// will be verified against the types only.
	for _, typ := range s.Types {
		name := typeName(typ)
		if typ.Kind == ast.Interface {
			authRules[name] = &TypeAuth{}
		}
	}

	return authRules, errResult
}

// mergePostCascadeInterfaceAuth implements Stage 4 of the auth rule assembly
// pipeline. For each interface with @auth(mergeAfterCascade: true), it AND-merges
// the interface's compiled auth rules into every concrete type that implements it.
//
// This runs after expandCascadeAuth so the interface check applies to the fully
// composed auth tree — including both own auth and cascade-contributed rules.
// For OR-policy types the AND distributes correctly over OR branches:
//
//	AND(interfaceAuth, OR(base, cascadeBlock))
//	= OR(AND(base, interfaceAuth), AND(cascadeBlock, interfaceAuth))
func mergePostCascadeInterfaceAuth(sch *schema, authRules map[string]*TypeAuth) error {
	s := sch.schema
	for _, ifaceDef := range s.Types {
		if ifaceDef.Kind != ast.Interface {
			continue
		}
		auth := ifaceDef.Directives.ForName(authDirective)
		if auth == nil {
			continue
		}
		mac := auth.Arguments.ForName("mergeAfterCascade")
		if mac == nil || mac.Value.Raw != "true" {
			continue
		}
		ifaceName := typeName(ifaceDef)
		ifaceAuth := authRules[ifaceName]
		if ifaceAuth == nil || ifaceAuth.Rules == nil {
			continue
		}
		// AND this interface's auth into every concrete type that implements it.
		for _, typ := range s.Types {
			if typ.Kind != ast.Object {
				continue
			}
			name := typeName(typ)
			for _, iface := range typ.Interfaces {
				if iface != ifaceName {
					continue
				}
				ta := authRules[name]
				if ta == nil {
					ta = &TypeAuth{Fields: make(map[string]*AuthContainer)}
					authRules[name] = ta
				}
				// Always AND — the interface check is a universal restriction
				// applied regardless of the type's own cascadeAuthPolicy.
				ta.Rules = mergeAuthRulesWithPolicy(
					ta.Rules,
					ifaceAuth.Rules,
					"and",
					interfacePolicyEntry{}, false,
				)
				break
			}
		}
	}
	return nil
}

// snapshotTypeAuthMap creates a shallow copy of the authRules map where each
// TypeAuth value is independently cloned. Used to preserve the pre-interface-merge
// state for expandCascadeAuth's bidirectional propagation logic.
func snapshotTypeAuthMap(src map[string]*TypeAuth) map[string]*TypeAuth {
	out := make(map[string]*TypeAuth, len(src))
	for k, v := range src {
		if v == nil {
			out[k] = nil
			continue
		}
		copy := *v
		out[k] = &copy
	}
	return out
}

// scanUnresolvedInContainer checks all ops in an AuthContainer for rule nodes
// that still contain unresolved <<KEY>> placeholders after both substitution
// passes and returns a descriptive error for the first one found.
func scanUnresolvedInContainer(ac *AuthContainer, location string) error {
	for _, rn := range []*RuleNode{ac.Query, ac.Add, ac.Update, ac.Delete, ac.Password} {
		if err := scanUnresolvedInNode(rn, location); err != nil {
			return err
		}
	}
	return nil
}

// scanUnresolvedInNode recursively walks a RuleNode tree and returns an error
// if any leaf's RuleTemplate still contains an unresolved <<KEY>> placeholder.
// An unresolved placeholder after both resolveTemplateLeaves passes indicates
// a misspelled key or a missing @authVariables declaration.
func scanUnresolvedInNode(rn *RuleNode, location string) error {
	if rn == nil {
		return nil
	}
	if rn.RuleTemplate != "" && rn.Rule == nil && rn.RBACRule == nil {
		if keys := unresolvedAuthVarKeys(rn.RuleTemplate); len(keys) > 0 {
			return gqlerror.Errorf(
				"%s: @auth rule has unresolved placeholder(s) %v after substitution. "+
					"Check that each key is declared in @authVariables on the type.",
				location, keys)
		}
	}
	for _, child := range rn.Or {
		if err := scanUnresolvedInNode(child, location); err != nil {
			return err
		}
	}
	for _, child := range rn.And {
		if err := scanUnresolvedInNode(child, location); err != nil {
			return err
		}
	}
	if err := scanUnresolvedInNode(rn.Not, location); err != nil {
		return err
	}
	if rn.CascadeWrapInner != nil {
		if err := scanUnresolvedInNode(rn.CascadeWrapInner, location); err != nil {
			return err
		}
	}
	return nil
}

// resolveTemplateLeaves walks a RuleNode tree and, for each leaf node whose
// RuleTemplate is non-empty (i.e. a {{KEY}} compile-time template), substitutes
// vars into the template and re-parses the result. The leaf is replaced with the
// newly parsed node so that downstream code sees a proper Rule (not a bare template).
//
// This is called in authRules() after parseAuthDirective to ensure that a type's
// own @authVariables constants (e.g. {{QRY_PERMISSIONS}} on Workspace) are fully
// resolved in that type's own auth rules before cascade expansion reads them.
//
// If substitution or re-parsing fails, the original leaf is returned unchanged.
func resolveTemplateLeaves(sch *schema, rn *RuleNode, vars map[string]string, typeName string, force bool) *RuleNode {
	if rn == nil || len(vars) == 0 {
		return rn
	}
	// Leaf with a template: substitute own vars and re-parse.
	if rn.RuleTemplate != "" && (force || (rn.Rule == nil && rn.DQLRule == nil)) {
		substituted, err := substitutAuthVars(rn.RuleTemplate, vars)
		if err != nil {
			return rn // template parse/exec error — defer to next pass (interface stub pattern)
		}
		// RBAC rules are compact single-line forms that start with "{" (e.g.
		// { $scope: { eq: "_all" } }). Full GQL query rules start with the
		// "query" keyword. Use HasPrefix on the trimmed string so that
		// GQL query bodies (which contain many "{" characters) are not
		// mistakenly routed through the RBAC parse path.
		if strings.HasPrefix(strings.TrimSpace(substituted), RBACQueryPrefix) {
			// Became an RBAC rule — parse it.
			typ := sch.schema.Types[typeName]
			if typ == nil {
				return rn
			}
			rbac, err := getRBACQuery(typ, substituted)
			if err != nil {
				return rn // parse failed — leave as-is
			}
			return &RuleNode{RBACRule: rbac, RuleTemplate: rn.RuleTemplate}
		}
		// Attempt GraphQL parse. gqlValidateRule populates ast.Field.Definition on
		// every field via validator.Validate — this is required so that ArgumentMap()
		// can coerce argument types at query time. Rules that bypass this step would
		// produce ast.Field nodes with nil Definition, causing a panic in
		// f.field.ArgumentMap() during query rewriting.
		// If gqlValidateRule fails (e.g. the substituted enum values don't pass the
		// strict validator, or the type's query name doesn't match), leave rn.Rule==nil.
		// The resubstituteRuleNode fallback (rn.Rule != nil) handles this correctly:
		// it skips re-substitution and returns nil for this cascade arm, which is safe.
		node := &RuleNode{RuleTemplate: rn.RuleTemplate}
		// Use inferQueriedTypeDef rather than sch.schema.Types[typeName] directly.
		// Interface auth rules (e.g. IAMResource's "queryIAMResource(...)")
		// query a different type than the concrete type they've been merged into.
		// gqlValidateRule checks f.Name == "query"+typ.Name, so we must pass the
		// type the rule actually queries — inferred from the root field name.
		typDef := inferQueriedTypeDef(sch, substituted, typeName)
		if typDef == nil {
			return rn
		}
		if err := gqlValidateRule(sch, typDef, substituted, node); err != nil {
			return rn // leave as-is; resubstituteRuleNode will handle the nil Rule case
		}
		return node
	}
	// Composite nodes: recurse.
	clone := *rn
	if len(rn.Or) > 0 {
		clone.Or = make([]*RuleNode, len(rn.Or))
		for i, child := range rn.Or {
			clone.Or[i] = resolveTemplateLeaves(sch, child, vars, typeName, force)
		}
	}
	if len(rn.And) > 0 {
		clone.And = make([]*RuleNode, len(rn.And))
		for i, child := range rn.And {
			clone.And[i] = resolveTemplateLeaves(sch, child, vars, typeName, force)
		}
	}
	if rn.Not != nil {
		clone.Not = resolveTemplateLeaves(sch, rn.Not, vars, typeName, force)
	}
	return &clone
}

func mergeAuthNodeWithAnd(objectAuth, interfaceAuth *RuleNode) *RuleNode {
	if objectAuth == nil {
		return interfaceAuth
	}

	if interfaceAuth == nil {
		return objectAuth
	}

	ruleNode := &RuleNode{}
	ruleNode.And = append(ruleNode.And, objectAuth, interfaceAuth)
	return ruleNode
}

// interfacePolicyEntry stores the per-interface merge policy parsed from
// @auth(interfacePolicy: [...]) on a concrete type.  It captures the merge
// operator to use and an optional restricted set of operations the override
// applies to.  A nil operations set means "apply to all operations".
type interfacePolicyEntry struct {
	mergeOp    string          // "and" or "or"
	operations map[string]bool // nil = all ops; non-nil = restricted set
}

// parseInterfacePolicy reads the @auth(interfacePolicy: [...]) argument on a
// concrete type definition and returns a map from interface name to
// interfacePolicyEntry.  Used by authRules() to override the interface's
// default mergeInto value on a per-(concrete type, interface, operation) basis.
//
// Example schema:
//
//	type Group implements WorkspaceMember & IProtected
//	  @auth(interfacePolicy: [
//	    { interface: "WorkspaceMember", merge: "or" }
//	    { interface: "IProtected", merge: "or", operations: ["add", "delete"] }
//	  ]) { … }
//
// Returns:
//
//	{
//	  "WorkspaceMember": {mergeOp: "or", operations: nil},
//	  "IProtected":      {mergeOp: "or", operations: {"add":true, "delete":true}},
//	}
func parseInterfacePolicy(typDef *ast.Definition) map[string]interfacePolicyEntry {
	auth := typDef.Directives.ForName(authDirective)
	if auth == nil {
		return nil
	}
	ip := auth.Arguments.ForName("interfacePolicy")
	if ip == nil || ip.Value == nil {
		return nil
	}
	result := make(map[string]interfacePolicyEntry)
	// ip.Value is a list literal; each child is an InterfaceMergePolicy object literal.
	for _, item := range ip.Value.Children {
		if item.Value == nil {
			continue
		}
		var iface, mergeOp string
		var ops []string
		// item.Value is an object literal with fields "interface", "merge", "operations".
		for _, field := range item.Value.Children {
			if field.Value == nil {
				continue
			}
			switch field.Name {
			case "interface":
				iface = field.Value.Raw
			case "merge":
				mergeOp = field.Value.Raw
			case "operations":
				for _, opChild := range field.Value.Children {
					if opChild.Value != nil {
						ops = append(ops, opChild.Value.Raw)
					}
				}
			}
		}
		if iface == "" || mergeOp == "" {
			continue
		}
		entry := interfacePolicyEntry{mergeOp: mergeOp}
		if len(ops) > 0 {
			entry.operations = make(map[string]bool, len(ops))
			for _, op := range ops {
				entry.operations[op] = true
			}
		}
		result[iface] = entry
	}
	return result
}

// validateInterfacePolicy is a typeValidation that enforces well-formedness of
// the @auth(interfacePolicy: [...]) argument on concrete OBJECT types and the
// @auth(mergeInto: ...) argument on INTERFACE types.
//
// Rules checked:
//  1. mergeInto on an INTERFACE must be "and" or "or".
//  2. interfacePolicy on an INTERFACE type is rejected — it is only meaningful
//     on concrete (OBJECT) types.
//  3. Each interfacePolicy entry's merge must be "and" or "or".
//  4. Each interfacePolicy entry's interface must name a real interface type
//     defined in the schema.
//  5. The concrete type must actually implement the referenced interface.
//  6. No interface may be listed more than once in a single interfacePolicy list.
func validateInterfacePolicy(schema *ast.Schema, typ *ast.Definition) gqlerror.List {
	var errs []*gqlerror.Error

	auth := typ.Directives.ForName(authDirective)
	if auth == nil {
		return nil
	}

	// ── INTERFACE types: validate mergeInto; reject interfacePolicy ────────────
	if typ.Kind == ast.Interface {
		if mi := auth.Arguments.ForName("mergeInto"); mi != nil && mi.Value != nil {
			val := mi.Value.Raw
			if val != "and" && val != "or" {
				errs = append(errs, gqlerror.ErrorPosf(typ.Position,
					`Type %s; @auth(mergeInto: %q): must be "and" or "or"`,
					typ.Name, val))
			}
		}
		if ip := auth.Arguments.ForName("interfacePolicy"); ip != nil &&
			ip.Value != nil && len(ip.Value.Children) > 0 {
			errs = append(errs, gqlerror.ErrorPosf(typ.Position,
				`Type %s; @auth(interfacePolicy) is only valid on concrete object types, not on interfaces`,
				typ.Name))
		}
		return errs
	}

	if typ.Kind != ast.Object {
		return nil
	}

	// ── OBJECT types: validate each interfacePolicy entry ──────────────────────
	ip := auth.Arguments.ForName("interfacePolicy")
	if ip == nil || ip.Value == nil {
		return nil
	}

	// Build a set of interfaces this type actually implements for O(1) lookup.
	implementedSet := make(map[string]bool, len(typ.Interfaces))
	for _, iface := range typ.Interfaces {
		implementedSet[iface] = true
	}

	seen := make(map[string]bool)

	for _, item := range ip.Value.Children {
		if item.Value == nil {
			continue
		}
		var iface, mergeOp string
		var opValues []string
		hasOpsField := false
		for _, field := range item.Value.Children {
			if field.Value == nil {
				continue
			}
			switch field.Name {
			case "interface":
				iface = field.Value.Raw
			case "merge":
				mergeOp = field.Value.Raw
			case "operations":
				hasOpsField = true
				for _, opChild := range field.Value.Children {
					if opChild.Value != nil {
						opValues = append(opValues, opChild.Value.Raw)
					}
				}
			default:
				// Unknown field — likely a typo (e.g. "operationsx").
				errs = append(errs, gqlerror.ErrorPosf(typ.Position,
					`Type %s; @auth(interfacePolicy): unknown field %q — valid fields are "interface", "merge", "operations"`,
					typ.Name, field.Name))
			}
		}
		if iface == "" {
			continue
		}

		// Rule 7a: if the operations field is present it must be non-empty.
		if hasOpsField && len(opValues) == 0 {
			errs = append(errs, gqlerror.ErrorPosf(typ.Position,
				`Type %s; @auth(interfacePolicy[%s].operations): empty list is not allowed — omit the field to apply to all operations`,
				typ.Name, iface))
		}
		// Rule 7b: validate each operation name against the CascadeAuthOperation enum values.
		// NOTE: although operations is typed as [CascadeAuthOperation!] in the SDL, gqlparser
		// does not validate enum values nested inside input-object fields in directive arguments
		// at schema compile time, so we enforce this explicitly.
		validOps := map[string]bool{"add": true, "update": true, "delete": true, "query": true}
		for _, opVal := range opValues {
			if !validOps[opVal] {
				errs = append(errs, gqlerror.ErrorPosf(typ.Position,
					`Type %s; @auth(interfacePolicy[%s].operations): %q is not a valid CascadeAuthOperation — must be one of add, update, delete, query`,
					typ.Name, iface, opVal))
			}
		}

		// Rule 6: duplicate interface reference.
		if seen[iface] {
			errs = append(errs, gqlerror.ErrorPosf(typ.Position,
				`Type %s; @auth(interfacePolicy): interface %q is listed more than once`,
				typ.Name, iface))
		}
		seen[iface] = true

		// Rule 3: merge must be "and" or "or".
		if mergeOp != "and" && mergeOp != "or" {
			errs = append(errs, gqlerror.ErrorPosf(typ.Position,
				`Type %s; @auth(interfacePolicy[%s].merge): must be "and" or "or", got %q`,
				typ.Name, iface, mergeOp))
		}

		// Rule 4: referenced interface must exist in the schema.
		ifaceDef, exists := schema.Types[iface]
		if !exists || ifaceDef.Kind != ast.Interface {
			errs = append(errs, gqlerror.ErrorPosf(typ.Position,
				`Type %s; @auth(interfacePolicy): %q is not a defined interface type in the schema`,
				typ.Name, iface))
			continue
		}

		// Rule 5: concrete type must implement the referenced interface.
		if !implementedSet[iface] {
			errs = append(errs, gqlerror.ErrorPosf(typ.Position,
				`Type %s; @auth(interfacePolicy): type does not implement interface %q`,
				typ.Name, iface))
		}
	}

	return errs
}

func mergeAuthRules(
	objectAuthRules,
	interfaceAuthRules *AuthContainer,
	mergeAuthNode func(*RuleNode, *RuleNode) *RuleNode,
) *AuthContainer {
	// return copy of interfaceAuthRules since it is a pointer and otherwise it will lead
	// to unnecessary errors
	if objectAuthRules == nil {
		return &AuthContainer{
			Password: interfaceAuthRules.Password,
			Query:    interfaceAuthRules.Query,
			Add:      interfaceAuthRules.Add,
			Delete:   interfaceAuthRules.Delete,
			Update:   interfaceAuthRules.Update,
		}
	}

	objectAuthRules.Password = mergeAuthNode(objectAuthRules.Password, interfaceAuthRules.Password)
	objectAuthRules.Query = mergeAuthNode(objectAuthRules.Query, interfaceAuthRules.Query)
	objectAuthRules.Add = mergeAuthNode(objectAuthRules.Add, interfaceAuthRules.Add)
	objectAuthRules.Delete = mergeAuthNode(objectAuthRules.Delete, interfaceAuthRules.Delete)
	objectAuthRules.Update = mergeAuthNode(objectAuthRules.Update, interfaceAuthRules.Update)
	return objectAuthRules
}

// mergeAuthRulesWithPolicy merges interfaceAuthRules into objectAuthRules,
// applying per-operation merge operators determined by the concrete type's
// interfacePolicy entry for this interface and the interface's default
// mergeInto value.
//
// For each of add/update/delete/query:
//   - If hasEntry AND (entry.operations is nil OR entry.operations[op]) → use entry.mergeOp.
//   - Otherwise → use defaultMergeOp.
//
// Password always uses defaultMergeOp; it is not a user-facing operation and
// cannot be referenced by interfacePolicy.operations.
func mergeAuthRulesWithPolicy(
	objectAuthRules,
	interfaceAuthRules *AuthContainer,
	defaultMergeOp string,
	entry interfacePolicyEntry,
	hasEntry bool,
) *AuthContainer {
	if objectAuthRules == nil {
		return &AuthContainer{
			Password: interfaceAuthRules.Password,
			Query:    interfaceAuthRules.Query,
			Add:      interfaceAuthRules.Add,
			Delete:   interfaceAuthRules.Delete,
			Update:   interfaceAuthRules.Update,
		}
	}

	// mergeFnFor returns the appropriate merge function for one operation name.
	mergeFnFor := func(opName string) func(*RuleNode, *RuleNode) *RuleNode {
		op := defaultMergeOp
		if hasEntry && (entry.operations == nil || entry.operations[opName]) {
			op = entry.mergeOp
		}
		if op == "or" {
			return mergeAuthNodeWithOr
		}
		return mergeAuthNodeWithAnd
	}

	objectAuthRules.Add = mergeFnFor("add")(objectAuthRules.Add, interfaceAuthRules.Add)
	objectAuthRules.Update = mergeFnFor("update")(objectAuthRules.Update, interfaceAuthRules.Update)
	objectAuthRules.Delete = mergeFnFor("delete")(objectAuthRules.Delete, interfaceAuthRules.Delete)
	objectAuthRules.Query = mergeFnFor("query")(objectAuthRules.Query, interfaceAuthRules.Query)

	// Password is not a user-facing operation; always use the interface default.
	passwordFn := mergeAuthNodeWithAnd
	if defaultMergeOp == "or" {
		passwordFn = mergeAuthNodeWithOr
	}
	objectAuthRules.Password = passwordFn(objectAuthRules.Password, interfaceAuthRules.Password)

	return objectAuthRules
}

func parseAuthDirective(
	sch *schema,
	typ *ast.Definition,
	dir *ast.Directive) (*AuthContainer, error) {

	if dir == nil || len(dir.Arguments) == 0 {
		return nil, nil
	}

	var errResult, err error
	result := &AuthContainer{}

	if pwd := dir.Arguments.ForName("password"); pwd != nil && pwd.Value != nil {
		result.Password, err = parseAuthNode(sch, typ, pwd.Value)
		errResult = AppendGQLErrs(errResult, err)
	}

	if qry := dir.Arguments.ForName("query"); qry != nil && qry.Value != nil {
		result.Query, err = parseAuthNode(sch, typ, qry.Value)
		errResult = AppendGQLErrs(errResult, err)
	}

	if add := dir.Arguments.ForName("add"); add != nil && add.Value != nil {
		result.Add, err = parseAuthNode(sch, typ, add.Value)
		errResult = AppendGQLErrs(errResult, err)
	}

	if upd := dir.Arguments.ForName("update"); upd != nil && upd.Value != nil {
		result.Update, err = parseAuthNode(sch, typ, upd.Value)
		errResult = AppendGQLErrs(errResult, err)
	}

	if del := dir.Arguments.ForName("delete"); del != nil && del.Value != nil {
		result.Delete, err = parseAuthNode(sch, typ, del.Value)
		errResult = AppendGQLErrs(errResult, err)
	}

	return result, errResult
}

func parseAuthNode(sch *schema, typ *ast.Definition, val *ast.Value) (*RuleNode, error) {

	if len(val.Children) == 0 {
		return nil, gqlerror.Errorf("Type %s: @auth: no arguments - "+
			"there should be only one of \"and\", \"or\", \"not\" and \"rule\"", typ.Name)
	}

	numChildren := 0
	var errResult error
	result := &RuleNode{}

	if ors := val.Children.ForName("or"); ors != nil && len(ors.Children) > 0 {
		for _, or := range ors.Children {
			rn, err := parseAuthNode(sch, typ, or.Value)
			result.Or = append(result.Or, rn)
			errResult = AppendGQLErrs(errResult, err)
		}
		if len(result.Or) < 2 {
			errResult = AppendGQLErrs(errResult, gqlerror.Errorf(
				`Type %s: @auth: 'OR' should contain at least two rules`, typ.Name))
		}
		numChildren++
	}

	if ands := val.Children.ForName("and"); ands != nil && len(ands.Children) > 0 {
		for _, and := range ands.Children {
			rn, err := parseAuthNode(sch, typ, and.Value)
			result.And = append(result.And, rn)
			errResult = AppendGQLErrs(errResult, err)
		}
		if len(result.And) < 2 {
			errResult = AppendGQLErrs(errResult, gqlerror.Errorf(
				`Type %s: @auth: 'AND' should contain at least two rules`, typ.Name))
		}
		numChildren++
	}

	if not := val.Children.ForName("not"); not != nil &&
		len(not.Children) == 1 && not.Children[0] != nil {

		var err error
		result.Not, err = parseAuthNode(sch, typ, not)
		errResult = AppendGQLErrs(errResult, err)
		numChildren++
	}

	if rule := val.Children.ForName("rule"); rule != nil {
		var err error
		if strings.Contains(rule.Raw, "<<") {
			// Rule contains @authVariables template placeholders (e.g. <<QRY_SCOPES>>).
			// The << >> syntax is not valid GraphQL or JSON — store as RuleTemplate regardless
			// of whether the rule also starts with the RBAC prefix "{". Attempting to parse
			// an unresolved RBAC template (e.g. { $scope: { in: <<QRY_SCOPES>> } }) as RBAC
			// immediately causes json.Unmarshal to fail on the placeholder, producing a
			// misleading "not a valid GraphQL variable" error. The cascade auth expand pipeline
			// will substitute the variables and re-parse the rule before use.
			result.RuleTemplate = rule.Raw
		} else if strings.HasPrefix(rule.Raw, RBACQueryPrefix) {
			result.RBACRule, err = getRBACQuery(typ, rule.Raw)
		} else {
			// Standard GQL auth rule — validate and parse immediately.
			err = gqlValidateRule(sch, typ, rule.Raw, result)
			// Also preserve the raw rule string as RuleTemplate so that the cascade auth
			// expansion pipeline can re-substitute @authVariables from child types when
			// variableContext:"self" is configured (resubstituteRuleNode re-parses from this).
			result.RuleTemplate = rule.Raw
		}
		errResult = AppendGQLErrs(errResult, err)
		numChildren++
	}

	if numChildren != 1 || len(val.Children) > 1 {
		errResult = AppendGQLErrs(errResult, gqlerror.Errorf("Type %s: @auth: there "+
			"should be only one of \"and\", \"or\", \"not\" and \"rule\"", typ.Name))
	}

	return result, errResult
}

func getRBACQuery(typ *ast.Definition, rule string) (*RBACQuery, error) {
	rbacRegex, err :=
		regexp.Compile(`^{[\s]?(.*?)[\s]?:[\s]?{[\s]?(\w*)[\s]?:[\s]?(.*)[\s]?}[\s]?}$`)
	if err != nil {
		return nil, gqlerror.Errorf("Type %s: @auth: `%s` error while parsing rule.",
			typ.Name, err)
	}

	idx := rbacRegex.FindAllStringSubmatchIndex(rule, -1)
	if len(idx) != 1 || len(idx[0]) != 8 || rule != rule[idx[0][0]:idx[0][1]] {
		return nil, gqlerror.Errorf("Type %s: @auth: `%s` is not a valid rule.",
			typ.Name, rule)
	}
	//	bool, for booleans
	//	float64, for numbers
	//	string, for strings
	//	[]interface{}, for JSON arrays
	//	map[string]interface{}, for JSON objects
	//	nil for JSON null
	var op interface{}
	if err = json.Unmarshal([]byte(rule[idx[0][6]:idx[0][7]]), &op); err != nil {
		return nil, gqlerror.Errorf("Type %s: @auth: `%s` is not a valid GraphQL variable.",
			typ.Name, rule[idx[0][2]:idx[0][3]])
	}

	//objects with nil values are not supported in rules
	if op == nil {
		return nil, gqlerror.Errorf("Type %s: @auth: `%s` operator has invalid value. "+
			"null values aren't supported.", typ.Name, rule[idx[0][4]:idx[0][5]])
	}
	query := &RBACQuery{
		Variable: rule[idx[0][2]:idx[0][3]],
		Operator: rule[idx[0][4]:idx[0][5]],
		Operand:  op,
	}
	if err = validateRBACQuery(typ, query); err != nil {
		return nil, err
	}
	// we have validated that variable is like $XYZ.
	// For further uses we will ensure that we won't get the $ sign while evaluation
	query.Variable = query.Variable[1:]

	// we will be sticking to compile once principle.
	// regex in rule will be compiled once and used again.
	if query.Operator == "regexp" {
		query.regex, err = regexp.Compile(query.Operand.(string))
		if err != nil {
			return nil, gqlerror.Errorf("Type %s: @auth: `%s` does not have a valid regex expression.",
				typ.Name, query.Variable)
		}
	}
	return query, nil
}

func validateRBACQuery(typ *ast.Definition, rbacQuery *RBACQuery) error {
	// validate rule operators
	if ok, reason := validateRBACOperators(typ, rbacQuery); !ok {
		return gqlerror.Errorf("%v", reason)
	}

	// validate variable name
	if !strings.HasPrefix(rbacQuery.Variable, "$") {
		return gqlerror.Errorf("Type %s: @auth: `%s` is not a valid GraphQL variable.",
			typ.Name, rbacQuery.Variable)
	}
	return nil
}

func validateRBACOperators(typ *ast.Definition, query *RBACQuery) (bool, string) {
	switch query.Operator {
	case "eq":
		// Array values in eq operator will not be supported.
		// They are handled in a different way to manage all possible situations
		_, isArray := query.Operand.([]interface{})
		if isArray {
			return false, fmt.Sprintf("Type %s: @auth: `%s` operator has invalid value `%v`."+
				" Array values in eq operator will not be supported.",
				typ.Name, query.Operator, query.Operand)
		}
	case "regexp":
		_, ok := query.Operand.(string)
		if !ok {
			return false, fmt.Sprintf("Type %s: @auth: `%s` operator has invalid value `%v`."+
				" Value should be of type String.", typ.Name, query.Operator, query.Operand)
		}
	case "in":
		// auth rule value should be of array type
		_, ok := query.Operand.([]interface{})
		if !ok {
			return false, fmt.Sprintf("Type %s: @auth: `%s` operator has invalid value `%v`."+
				" Value should be an array.", typ.Name, query.Operator, query.Operand)
		}
	default:
		return false, fmt.Sprintf("Type %s: @auth: `%s` operator is not supported.",
			typ.Name, query.Operator)
	}

	return true, ""
}

func gqlValidateRule(sch *schema, typ *ast.Definition, rule string, node *RuleNode) error {
	doc, gqlErr := parser.ParseQuery(&ast.Source{Input: rule})
	if gqlErr != nil {
		return gqlerror.Errorf("Type %s: @auth: failed to parse GraphQL rule "+
			"[reason : %s]", typ.Name, gqlErr.Message)
	}

	if len(doc.Operations) != 1 {
		return gqlerror.Errorf("Type %s: @auth: a rule should be "+
			"exactly one query, found %v GraphQL operations", typ.Name, len(doc.Operations))
	}

	op := doc.Operations[0]
	if op == nil {
		return gqlerror.Errorf("Type %s: @auth: a rule should be "+
			"exactly one query, found an empty GraphQL operation", typ.Name)
	}

	if op.Operation != "query" {
		return gqlerror.Errorf("Type %s: @auth: a rule should be exactly"+
			" one query, found an %s", typ.Name, op.Name)
	}

	listErr := validator.Validate(sch.schema, doc, nil)
	if len(listErr) != 0 {
		var errs error
		for _, err := range listErr {
			errs = AppendGQLErrs(errs, gqlerror.Errorf("Type %s: @auth: failed to "+
				"validate GraphQL rule [reason : %s]", typ.Name, err.Message))
		}
		return errs
	}

	if len(op.SelectionSet) != 1 {
		return gqlerror.Errorf("Type %s: @auth: a rule should be exactly one "+
			"query, found %v queries", typ.Name, len(op.SelectionSet))
	}

	f, ok := op.SelectionSet[0].(*ast.Field)
	if !ok {
		return gqlerror.Errorf("Type %s: @auth: error couldn't generate query from rule",
			typ.Name)
	}

	if f.Name != "query"+typ.Name {
		return gqlerror.Errorf("Type %s: @auth: expected only query%s "+
			"rules,but found %s", typ.Name, typ.Name, f.Name)
	}

	opWrapper := &operation{
		op:                      op,
		query:                   rule,
		doc:                     doc,
		inSchema:                sch,
		interfaceImplFragFields: map[*ast.Field]string{},
		// need to fill in vars at query time
	}

	// recursively expand fragments in operation as selection set fields
	recursivelyExpandFragmentSelections(f, opWrapper)

	node.Rule = &query{
		field: f,
		op:    opWrapper,
		sel:   op.SelectionSet[0]}
	node.Variables = op.VariableDefinitions
	return nil
}
