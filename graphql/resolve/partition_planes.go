/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/pkg/errors"

	dgoapi "github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/dgraph-io/dgraph/v25/dql"
	"github.com/dgraph-io/dgraph/v25/graphql/dgraph"
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/x"
)

// SPIKE: an edge from a tenant's record to a record of the control plane.
//
// The edge is stored in the tenant's partition and holds the ID of a record whose
// data is in the root namespace (IDs are the cluster's, not a namespace's). Nothing
// is written to the control plane through it: a mutation inside a partition may
// name a control record that exists, and only that.

// splitByPlane separates the lookups a mutation makes of the records it names into
// those made where the mutation runs and those made in the root namespace, because
// the type looked for is in the control plane. It returns the context to make the
// second kind in. Outside a partition there is one namespace and nothing is split.
func splitByPlane(ctx context.Context, m schema.Mutation, queries []*dql.GraphQuery,
	types []string) (local, control []*dql.GraphQuery, controlCtx context.Context) {
	ns, err := x.ExtractNamespace(ctx)
	if err != nil || !x.IsPartitionNamespace(ns) || len(queries) != len(types) ||
		m.Operation() == nil || m.Operation().Schema() == nil {
		return queries, nil, ctx
	}
	sch := m.Operation().Schema()
	for i, q := range queries {
		if sch.PlaneOf(types[i]) == schema.PlaneControl {
			control = append(control, q)
		} else {
			local = append(local, q)
		}
	}
	if len(control) == 0 {
		return queries, nil, ctx
	}
	return local, control, x.AttachNamespace(ctx, x.TemplateNamespace(ns))
}

// lookUpByPlane runs the lookups a @default makes of the records it names, each
// where the type looked for lives, and returns what they found together. from is the
// type the default is on; also holds the checks that go with the lookups made here.
func lookUpByPlane(ctx context.Context, from schema.Type, queries []*dql.GraphQuery, types []string,
	also []*dql.GraphQuery) (map[string][]idExistenceRes, error) {
	local, control, controlCtx := queries, []*dql.GraphQuery(nil), ctx
	if ns, err := x.ExtractNamespace(ctx); err == nil && x.IsPartitionNamespace(ns) && len(queries) == len(types) {
		local = nil
		for i, q := range queries {
			if from.PlaneOfType(types[i]) == schema.PlaneControl {
				control = append(control, q)
			} else {
				local = append(local, q)
			}
		}
		if len(control) > 0 {
			controlCtx = x.AttachNamespace(ctx, x.TemplateNamespace(ns))
		}
	}
	found := make(map[string][]idExistenceRes)
	run := func(ctx context.Context, blocks []*dql.GraphQuery, readOnly bool) error {
		if len(blocks) == 0 {
			return nil
		}
		resp, err := NewDgraphExecutor().Execute(ctx, &dgoapi.Request{Query: dgraph.AsString(blocks), ReadOnly: readOnly}, nil)
		if err != nil || resp == nil {
			return err
		}
		return json.Unmarshal(resp.Json, &found)
	}
	if err := run(ctx, append(append([]*dql.GraphQuery{}, local...), also...), false); err != nil {
		return nil, err
	}
	if err := run(controlCtx, control, true); err != nil {
		return nil, err
	}
	return found, nil
}

// noControlRecordsMade refuses a mutation that, running in a partition, would make
// a record of a control type there: a copy the control plane knows nothing of.
func noControlRecordsMade(ctx context.Context, upserts []*UpsertMutation) error {
	ns, err := x.ExtractNamespace(ctx)
	if err != nil || !x.IsPartitionNamespace(ns) {
		return nil
	}
	made := make(map[string]bool)
	for _, u := range upserts {
		for _, typ := range u.NewNodes {
			if typ != nil && typ.Plane() == schema.PlaneControl {
				made[typ.Name()] = true
			}
		}
	}
	if len(made) == 0 {
		return nil
	}
	names := make([]string, 0, len(made))
	for n := range made {
		names = append(names, n)
	}
	sort.Strings(names)
	return errors.Errorf("%s is in the control plane: from inside a workspace a record of it can be named, "+
		"if it exists, and cannot be made", strings.Join(names, ", "))
}

// planeEdge reports whether child, a field of parent's type, is an edge from a
// tenant's record to a record of the control plane.
func planeEdge(parent, child schema.Field) bool {
	if parent == nil || child == nil || parent.Type() == nil || child.Type() == nil {
		return false
	}
	return child.Type().Plane() == schema.PlaneControl && parent.Type().Plane() != schema.PlaneControl
}

// markTemplate marks query blocks as reading the template namespace: the blocks the
// rewriter makes for the rules and filters of a control type reached from a tenant.
func markTemplate(blocks []*dql.GraphQuery) {
	for _, b := range blocks {
		if b != nil {
			b.Template = true
		}
	}
}
