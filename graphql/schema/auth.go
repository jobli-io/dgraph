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
	// CascadeInversePred is the Dgraph predicate that the uid_in filter traverses
	// to scope the child type from the authority's side. On leaf cascade nodes this
	// mirrors CascadeEdgePred; on bundle nodes CascadeBundlePred takes precedence.
	CascadeInversePred string
	// CascadeEdgeForwardPath records the cascade hop chain for a leaf rule node.
	// Each entry is a predicate name in the forward traversal order (e.g.
	// ["inGroup", "inWorkspace"] for a Company→Group→Workspace chain).
	CascadeEdgeForwardPath []string
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
			ta.Rules.Query = resolveTemplateLeaves(sch, ta.Rules.Query, ownVars, name)
			ta.Rules.Add = resolveTemplateLeaves(sch, ta.Rules.Add, ownVars, name)
			ta.Rules.Update = resolveTemplateLeaves(sch, ta.Rules.Update, ownVars, name)
			ta.Rules.Delete = resolveTemplateLeaves(sch, ta.Rules.Delete, ownVars, name)
		}
		for field, ac := range ta.Fields {
			if ac == nil {
				continue
			}
			ta.Fields[field] = &AuthContainer{
				Query:    resolveTemplateLeaves(sch, ac.Query, ownVars, name),
				Add:      resolveTemplateLeaves(sch, ac.Add, ownVars, name),
				Update:   resolveTemplateLeaves(sch, ac.Update, ownVars, name),
				Delete:   resolveTemplateLeaves(sch, ac.Delete, ownVars, name),
				Password: resolveTemplateLeaves(sch, ac.Password, ownVars, name),
			}
		}
	}

	// Snapshot before interface merging: used by expandCascadeAuth to propagate
	// only a type's own declared @auth bidirectionally (not the interface-level auth).
	authRulesOwnOnly := snapshotTypeAuthMap(authRules)

	// Merge the Auth rules on interfaces into the implementing types
	for _, typ := range s.Types {
		name := typeName(typ)
		if typ.Kind == ast.Object {
			for _, intrface := range typ.Interfaces {
				interfaceName := typeName(s.Types[intrface])
				if authRules[interfaceName] != nil && authRules[interfaceName].Rules != nil {
					authRules[name].Rules = mergeAuthRules(
						authRules[name].Rules,
						authRules[interfaceName].Rules,
						mergeAuthNodeWithAnd,
					)
				}
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
func resolveTemplateLeaves(sch *schema, rn *RuleNode, vars map[string][]string, typeName string) *RuleNode {
	if rn == nil || len(vars) == 0 {
		return rn
	}
	// Leaf with a template: substitute own vars and re-parse.
	if rn.RuleTemplate != "" && rn.Rule == nil && rn.DQLRule == nil && rn.RBACRule == nil {
		substituted := substitutAuthVars(rn.RuleTemplate, vars)
		if strings.Contains(substituted, RBACQueryPrefix) {
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
		// Attempt GraphQL parse using the strict type-validating parser first.
		// This sets node.Rule (the compiled query) via gqlValidateRule.
		node := &RuleNode{RuleTemplate: rn.RuleTemplate}
		typ := sch.schema.Types[typeName]
		if typ == nil {
			return rn
		}
		if err := gqlValidateRule(sch, typ, substituted, node); err != nil {
			// gqlValidateRule enforces queryTypeName and full schema validation.
			// This can fail when the substituted enum values don't pass the
			// validator (e.g. enum-typed "in:" filter) or when the type has a
			// non-standard query name. Fall back to the lenient cascade parser
			// (gqlParseRuleForCascade) which just parses syntax.
			if cerr := gqlParseRuleForCascade(sch, substituted, node); cerr != nil {
				return rn // both parsers failed — leave as-is
			}
		}
		return node
	}
	// Composite nodes: recurse.
	clone := *rn
	if len(rn.Or) > 0 {
		clone.Or = make([]*RuleNode, len(rn.Or))
		for i, child := range rn.Or {
			clone.Or[i] = resolveTemplateLeaves(sch, child, vars, typeName)
		}
	}
	if len(rn.And) > 0 {
		clone.And = make([]*RuleNode, len(rn.And))
		for i, child := range rn.And {
			clone.And[i] = resolveTemplateLeaves(sch, child, vars, typeName)
		}
	}
	if rn.Not != nil {
		clone.Not = resolveTemplateLeaves(sch, rn.Not, vars, typeName)
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
		if strings.HasPrefix(rule.Raw, RBACQueryPrefix) {
			result.RBACRule, err = getRBACQuery(typ, rule.Raw)
		} else if strings.Contains(rule.Raw, "{{") {
			// Rule contains @authVariables template placeholders (e.g. {{ADM_PERMISSIONS}}).
			// Passing these to the GraphQL parser produces "Expected Name, found {" because
			// the double-brace syntax is not valid GraphQL. Store as RuleTemplate; the cascade
			// auth expand pipeline will substitute the variables and re-parse before use.
			result.RuleTemplate = rule.Raw
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
