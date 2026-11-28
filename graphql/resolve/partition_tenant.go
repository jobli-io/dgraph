/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"

	"github.com/pkg/errors"

	"github.com/dgraph-io/dgraph/v25/edgraph"
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/x"
)

// SPIKE: the one record a partition is built around.
//
// A partition is found by its name in the registry of namespaces, and that name is
// what a token's claim is matched to. The schema says which type is the tenant and
// which of its fields holds the tenant's name (@tenant(claim, name)). The two are
// the same name, and this keeps them so: inside a partition, a record of the tenant
// type can only be written under the name the partition is registered under.
//
// The name field is an @id, so there can be one record under that name. That is
// what makes "a partition holds exactly one tenant record" something the engine
// keeps, and not something the rest of the fast path takes on trust.

// checkTenantRecord refuses a mutation that would give a partition a tenant record
// under a name other than the partition's own.
func checkTenantRecord(ctx context.Context, m schema.Mutation, sch schema.Schema) error {
	typ := m.MutatedType()
	if typ == nil || !typ.IsTenant() || sch == nil {
		return nil
	}
	field := sch.TenantNameField()
	if field == "" {
		return nil
	}
	ns, err := x.ExtractNamespace(ctx)
	if err != nil || !x.IsPartitionNamespace(ns) {
		return nil
	}
	var written []interface{}
	switch m.MutationType() {
	case schema.AddMutation:
		inputs, _ := m.ArgValue(schema.InputArgName).([]interface{})
		for _, in := range inputs {
			obj, _ := in.(map[string]interface{})
			written = append(written, obj[field])
		}
	case schema.UpdateMutation:
		in, _ := m.ArgValue(schema.InputArgName).(map[string]interface{})
		if set, _ := in["set"].(map[string]interface{}); set != nil {
			if v, ok := set[field]; ok {
				written = append(written, v)
			}
		}
		if rem, _ := in["remove"].(map[string]interface{}); rem != nil {
			if _, ok := rem[field]; ok {
				return errors.Errorf("%s.%s is the name of the tenant and cannot be removed", typ.Name(), field)
			}
		}
	default:
		return nil
	}
	if len(written) == 0 {
		return nil
	}
	name, err := edgraph.NamespaceName(ctx, ns)
	if err != nil {
		return errors.Wrap(err, "while reading the name the partition is registered under")
	}
	if name == "" {
		return errors.Errorf("this partition has no name in the registry of namespaces, so no %s can be written in it",
			typ.Name())
	}
	for _, v := range written {
		if got, _ := v.(string); got != name {
			return errors.Errorf("this partition is registered as %q: a %s in it must have %s %q, not %q",
				name, typ.Name(), field, name, got)
		}
	}
	return nil
}
