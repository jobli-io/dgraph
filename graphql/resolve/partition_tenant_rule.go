/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"
	"encoding/json"

	"github.com/golang/glog"

	"github.com/dgraph-io/dgraph/v25/dql"
	"github.com/dgraph-io/dgraph/v25/graphql/authorization"
	"github.com/dgraph-io/dgraph/v25/graphql/dgraph"
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/x"
)

// SPIKE: asking the caller's tenant the tenant rules a request can meet.
//
// A tenantRule (schema/tenant_rule.go) is a question about the tenant a request
// came in for, not about the record being authorised. It is asked here, before the
// request is sent to the plane its type lives in, of the partition the request
// arrived in, and the answer travels with the request as an auth variable no token
// can set. The rules then read it like a claim.
//
// The question goes through the same asking and remembering as the grants of the
// fast path (partition_grants.go): its text holds the caller's claims, so an answer
// is one caller's, and it is kept for the grants ttl.
//
// A request outside a partition has no tenant to ask. It gets no answer, and a rule
// with no answer does not pass.

// withTenantRules returns ctx carrying the answer to each tenant rule in the rules
// of the types f names: the type it returns, the types it selects under it, and
// also, the type a mutation changes.
func withTenantRules(ctx context.Context, sch schema.Schema, f schema.Field, also schema.Type) context.Context {
	if sch == nil || !sch.HasTenantRules() {
		return ctx
	}
	names := make(map[string]bool)
	if also != nil {
		for _, n := range sch.TenantRulesOf(also.Name()) {
			names[n] = true
		}
	}
	tenantRulesUnder(sch, f, names)
	if len(names) == 0 {
		return ctx
	}
	ns, err := x.ExtractNamespace(ctx)
	if err != nil || !x.IsPartitionNamespace(ns) {
		return ctx
	}
	claims, err := sch.Meta().AuthMeta().ExtractCustomClaims(ctx)
	if err != nil || claims == nil || len(claims.AuthVariables) == 0 {
		return ctx
	}
	caller, err := json.Marshal(claims.AuthVariables)
	if err != nil {
		return ctx
	}
	g := &callerGrants{
		ns:      ns,
		claims:  claims.AuthVariables,
		caller:  string(caller),
		answers: make(map[*schema.RuleNode]answer),
		byText:  make(map[string]answer),
		rules:   make(map[simplifyKey]*schema.RuleNode),
	}
	answers := make(map[string]interface{}, len(names))
	for name := range names {
		tenant, rule := sch.TenantRule(name)
		if tenant == nil || rule == nil {
			continue
		}
		a := g.ask(func() (string, bool) { return tenantRuleQuestion(g, tenant, rule) })
		holds := !a.unknown && len(a.uids) > 0
		answers[name] = holds
		glog.V(2).Infof("tenant rule: namespace %d answered %v to %s", ns, holds, name)
	}
	return authorization.WithDerivedVariables(ctx, answers)
}

func tenantRulesUnder(sch schema.Schema, f schema.Field, names map[string]bool) {
	if f == nil {
		return
	}
	if t := f.Type(); t != nil {
		for _, n := range sch.TenantRulesOf(t.Name()) {
			names[n] = true
		}
	}
	for _, c := range f.SelectionSet() {
		tenantRulesUnder(sch, c, names)
	}
}

// tenantRuleQuestion compiles rule, a query on the tenant type, exactly as the
// rewriter compiles a rule of that type, and asks for one tenant record that passes.
func tenantRuleQuestion(g *callerGrants, tenant schema.Type, rule *schema.RuleNode) (string, bool) {
	rw := g.freshRewriter()
	rw.cascadeAuthorityType = tenant.DgraphName()
	blocks, filter := rw.rewriteRuleNode(tenant, rule)
	if filter == nil {
		// the caller lacks a variable the rule needs, or the rule cannot be met
		return "", false
	}
	c := compiledQuestion{
		of: tenant.DgraphName(),
		held: &dql.GraphQuery{
			Attr:     "held",
			Func:     &dql.Function{Name: "type", Args: []dql.Arg{{Value: tenant.DgraphName()}}},
			Filter:   filter,
			Args:     map[string]string{"first": "1"},
			Children: []*dql.GraphQuery{{Attr: "uid"}},
		},
	}
	return dgraph.AsString(append(append([]*dql.GraphQuery{}, blocks...), c.results()...)), true
}
