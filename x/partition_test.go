/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package x

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

func TestTemplateAttr(t *testing.T) {
	root := NamespaceAttr(RootNamespace, "Candidate.name")
	part := NamespaceAttr(0x2a, "Candidate.name")

	// Off: nothing is rewritten, so a stock cluster behaves as before.
	SetPartitionNamespaces(false)
	require.Equal(t, part, TemplateAttr(part))
	require.False(t, IsPartitionAttr(part))
	require.False(t, IsPartitionNamespace(0x2a))
	require.Equal(t, uint64(0x2a), TemplateNamespace(0x2a))

	SetPartitionNamespaces(true)
	defer SetPartitionNamespaces(false)

	require.Equal(t, root, TemplateAttr(part))
	require.Equal(t, root, TemplateAttr(root))
	require.True(t, IsPartitionAttr(part))
	require.False(t, IsPartitionAttr(root))
	require.True(t, IsPartitionNamespace(0x2a))
	require.False(t, IsPartitionNamespace(RootNamespace))
	require.Equal(t, RootNamespace, TemplateNamespace(0x2a))

	// A predicate name may itself contain the separator.
	dashed := NamespaceAttr(0x2a, "has-dash.pred-name")
	require.Equal(t, NamespaceAttr(RootNamespace, "has-dash.pred-name"), TemplateAttr(dashed))

	// Namespace 0x10 renders as "10-", which must not be mistaken for the root's "0-".
	require.Equal(t, root, TemplateAttr(NamespaceAttr(0x10, "Candidate.name")))
	require.True(t, IsPartitionAttr(NamespaceAttr(0x10, "Candidate.name")))
}

func TestAttrInNamespaceOf(t *testing.T) {
	asked := NamespaceAttr(0x2a, "Candidate")
	stored := NamespaceAttr(RootNamespace, "Candidate.has-dash")
	require.Equal(t, NamespaceAttr(0x2a, "Candidate.has-dash"), AttrInNamespaceOf(asked, stored))
	require.Equal(t, stored, AttrInNamespaceOf(NamespaceAttr(RootNamespace, "Candidate"), stored))
}

func TestPartitionHeaderResolver(t *testing.T) {
	ns := func(ctx context.Context) uint64 {
		t.Helper()
		got, err := ExtractNamespace(ctx)
		require.NoError(t, err)
		return got
	}

	ctx, err := PartitionHeaderResolver(context.Background())
	require.NoError(t, err)
	require.Equal(t, RootNamespace, ns(ctx))

	// A client-supplied "namespace" value is overwritten, never trusted.
	in := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("namespace", "99", PartitionNsHeader, "7"))
	ctx, err = PartitionHeaderResolver(in)
	require.NoError(t, err)
	require.Equal(t, uint64(7), ns(ctx))

	in = metadata.NewIncomingContext(context.Background(), metadata.Pairs("namespace", "99"))
	ctx, err = PartitionHeaderResolver(in)
	require.NoError(t, err)
	require.Equal(t, RootNamespace, ns(ctx))

	in = metadata.NewIncomingContext(context.Background(), metadata.Pairs(PartitionNsHeader, "seven"))
	_, err = PartitionHeaderResolver(in)
	require.Error(t, err)
}
