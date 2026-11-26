/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/protos/pb"
	"github.com/dgraph-io/dgraph/v25/types"
	"github.com/dgraph-io/dgraph/v25/x"
)

// A partition namespace reads the template's schema, and what it gets back is
// named in its own namespace so that callers go on to read its own data.
func TestPartitionReadsTemplateSchema(t *testing.T) {
	saved := pstate
	pstate = new(state)
	pstate.init()
	defer func() { pstate = saved }()

	rootName := x.NamespaceAttr(x.RootNamespace, "Candidate.name")
	rootEdge := x.NamespaceAttr(x.RootNamespace, "Candidate.inWorkspace")
	partName := x.NamespaceAttr(7, "Candidate.name")
	partEdge := x.NamespaceAttr(7, "Candidate.inWorkspace")

	State().Set(rootName, &pb.SchemaUpdate{
		Predicate: rootName, ValueType: pb.Posting_STRING,
		Directive: pb.SchemaUpdate_INDEX, Tokenizer: []string{"term"}, Upsert: true, Lang: true,
	})
	State().Set(rootEdge, &pb.SchemaUpdate{
		Predicate: rootEdge, ValueType: pb.Posting_UID, List: true,
		Directive: pb.SchemaUpdate_REVERSE, Count: true,
	})
	State().SetType(x.NamespaceAttr(x.RootNamespace, "Candidate"), &pb.TypeUpdate{
		TypeName: x.NamespaceAttr(x.RootNamespace, "Candidate"),
		Fields:   []*pb.SchemaUpdate{{Predicate: rootName}, {Predicate: rootEdge}},
	})
	ctx := context.Background()

	// Off: a non-root namespace has no schema unless it defines its own.
	x.SetPartitionNamespaces(false)
	_, ok := State().Get(ctx, partName)
	require.False(t, ok)
	require.False(t, State().IsIndexed(ctx, partName))

	x.SetPartitionNamespaces(true)
	defer x.SetPartitionNamespaces(false)

	su, ok := State().Get(ctx, partName)
	require.True(t, ok)
	require.Equal(t, partName, su.Predicate)
	require.Equal(t, []string{"term"}, su.Tokenizer)

	// The template's own entry is not modified by handing out a renamed copy.
	su, ok = State().Get(ctx, rootName)
	require.True(t, ok)
	require.Equal(t, rootName, su.Predicate)

	typ, err := State().TypeOf(partName)
	require.NoError(t, err)
	require.Equal(t, types.StringID, typ)
	require.True(t, State().IsIndexed(ctx, partName))
	require.Equal(t, []string{"term"}, State().TokenizerNames(ctx, partName))
	require.True(t, State().HasUpsert(partName))
	require.True(t, State().HasLang(partName))
	require.True(t, State().IsList(partEdge))
	require.True(t, State().IsReversed(ctx, partEdge))
	require.True(t, State().HasCount(ctx, partEdge))
	require.Equal(t, []string{partName}, State().PredicatesToDelete(partName))

	// A type's fields come back in the partition's namespace: expand(_all_) uses
	// them as-is, and the template's names would read the root namespace's data.
	tu, ok := State().GetType(x.NamespaceAttr(7, "Candidate"))
	require.True(t, ok)
	require.Equal(t, x.NamespaceAttr(7, "Candidate"), tu.TypeName)
	require.Equal(t, partName, tu.Fields[0].Predicate)
	require.Equal(t, partEdge, tu.Fields[1].Predicate)

	tu, ok = State().GetType(x.NamespaceAttr(x.RootNamespace, "Candidate"))
	require.True(t, ok)
	require.Equal(t, rootName, tu.Fields[0].Predicate)

	// The partition owns no schema entries.
	for _, p := range State().Predicates() {
		require.False(t, x.IsPartitionAttr(p), p)
	}
}
