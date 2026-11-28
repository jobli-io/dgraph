/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	"github.com/dgraph-io/gqlparser/v2/ast"
	"github.com/dgraph-io/gqlparser/v2/gqlerror"

	"github.com/dgraph-io/dgraph/v25/graphql/authorization"
)

// SPIKE: a rule asked of the caller's tenant.
//
//	@auth(update: { tenantRule: "query($USER: String!) { queryWorkspace { ... } }" })
//
// A tenantRule is a query on the type marked @tenant. It is not asked of the record
// being authorised: it is asked, once for the caller, of the tenant the request
// came in for, and holds if that tenant passes it. So it can stand in the rules of
// a type whose records are not in the tenant at all (@scope(plane: control)), where
// there is no edge from the record to the tenant for an ordinary rule to follow.
//
// In the rule tree it is a claim rule on a name only the engine can set: the
// request asks the tenant and supplies the answer under that name (see
// resolve/partition_tenant_rule.go). A request that has no answer, because it is
// not in a partition or the question could not be asked, does not pass the rule.

const tenantRuleArg = "tenantRule"

// TenantRulePrefix starts the name a tenant rule's answer is supplied under. No
// claim of a token is read under a name that starts with it.
const TenantRulePrefix = authorization.DerivedPrefix + "tenantRule."

func parseTenantRule(sch *schema, typ *ast.Definition, text string) (*RBACQuery, error) {
	tenant := sch.schema.Types[tenantOf(sch.schema)]
	if tenant == nil {
		return nil, gqlerror.Errorf("Type %s: @auth: a tenantRule is asked of the tenant, "+
			"and no type is marked @tenant", typ.Name)
	}
	if strings.Contains(text, "<<") || strings.Contains(text, "{{") {
		return nil, gqlerror.Errorf("Type %s: @auth: a tenantRule cannot use @authVariables "+
			"placeholders", typ.Name)
	}
	inner := &RuleNode{}
	if err := gqlValidateRule(sch, tenant, text, inner); err != nil {
		return nil, gqlerror.Errorf("Type %s: @auth: tenantRule: it must be one query on the "+
			"tenant type, query%s [reason : %s]", typ.Name, tenant.Name, err.Error())
	}
	// The name is made from the text, so that every alpha, and every load of the
	// schema, gives the same rule the same name.
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.Join(strings.Fields(text), " ")))
	name := fmt.Sprintf("%s%016x", TenantRulePrefix, h.Sum64())
	if sch.tenantRules == nil {
		sch.tenantRules = make(map[string]*RuleNode)
	}
	sch.tenantRules[name] = inner
	return &RBACQuery{Variable: name, Operator: "eq", Operand: true}, nil
}

// tenantRuleNames adds to names the tenant rules in the tree under rn.
func tenantRuleNames(rn *RuleNode, names map[string]bool) {
	if rn == nil {
		return
	}
	if rn.RBACRule != nil && strings.HasPrefix(rn.RBACRule.Variable, TenantRulePrefix) {
		names[rn.RBACRule.Variable] = true
	}
	for _, c := range rn.Or {
		tenantRuleNames(c, names)
	}
	for _, c := range rn.And {
		tenantRuleNames(c, names)
	}
	tenantRuleNames(rn.Not, names)
	tenantRuleNames(rn.CascadeWrapInner, names)
}

func (ac *AuthContainer) tenantRuleNames(names map[string]bool) {
	if ac == nil {
		return
	}
	for _, rn := range []*RuleNode{ac.Password, ac.Query, ac.Add, ac.Update, ac.Delete} {
		tenantRuleNames(rn, names)
	}
}

func (s *schema) HasTenantRules() bool {
	return s != nil && len(s.tenantRules) > 0
}

func (s *schema) TenantRule(name string) (Type, *RuleNode) {
	if s == nil || s.tenantRules[name] == nil {
		return nil, nil
	}
	tenant := &astType{
		typ:             &ast.Type{NamedType: tenantOf(s.schema)},
		inSchema:        s,
		dgraphPredicate: s.dgraphPredicate,
	}
	return tenant, s.tenantRules[name]
}

func (s *schema) TenantRulesOf(of string) []string {
	if !s.HasTenantRules() {
		return nil
	}
	s.tenantRulesOnce.Do(func() {
		s.tenantRulesByType = make(map[string][]string)
		own := func(def *ast.Definition, into map[string]bool) {
			ta := s.authRules[typeName(def)]
			if ta == nil {
				return
			}
			ta.Rules.tenantRuleNames(into)
			for _, f := range ta.Fields {
				f.tenantRuleNames(into)
			}
		}
		for typ, def := range s.schema.Types {
			names := make(map[string]bool)
			own(def, names)
			// a field of an interface or union type returns records of the types
			// under it, each under its own rules
			if def.Kind == ast.Interface || def.Kind == ast.Union {
				for _, impl := range s.schema.GetPossibleTypes(def) {
					own(impl, names)
				}
			}
			if len(names) == 0 {
				continue
			}
			list := make([]string, 0, len(names))
			for n := range names {
				list = append(list, n)
			}
			sort.Strings(list)
			s.tenantRulesByType[typ] = list
		}
	})
	return s.tenantRulesByType[of]
}
