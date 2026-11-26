/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package query

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/dql"
	"github.com/dgraph-io/dgraph/v25/protos/pb"
	"github.com/dgraph-io/dgraph/v25/x"
)

func TestSimilarToPreFilter_FillVarsAndEmptyShortCircuit(t *testing.T) {
	ctx := x.AttachNamespace(context.Background(), x.RootNamespace)
	dqlStr := `{
		var(func: type(JobCategory)) {
			allowed as uid
		}
		var(func: similar_to(JobCategory.search, 3, "[0.1,0.2]", uids: uid(allowed))) {
			similar as uid
		}
		q(func: uid(similar)) {
			uid
		}
	}`
	parsed, err := dql.Parse(dql.Request{Str: dqlStr})
	require.NoError(t, err)
	require.Len(t, parsed.Query, 3)

	// 1. When `allowed` resolves to 0 UIDs (absent from vars map), fillVars sets empty SrcUIDs,
	// ProcessGraph short-circuits without panicking, and populateVarMap registers `similar`.
	sg, err := ToSubGraph(ctx, parsed.Query[1])
	require.NoError(t, err)

	vars := make(map[string]varValue)
	require.NoError(t, sg.recursiveFillVars(vars))
	require.NotNil(t, sg.SrcUIDs)
	require.Empty(t, sg.SrcUIDs.Uids)

	rch := make(chan error, 1)
	ProcessGraph(ctx, sg, nil, rch)
	require.NoError(t, <-rch)

	require.NoError(t, sg.populateVarMap(vars, nil))
	_, exists := vars["similar"]
	require.True(t, exists, "child variable `similar` must still be registered when pre-filter is empty")

	// 2. When `allowed` resolves to non-empty UIDs, fillVars wires them into sg.SrcUIDs.
	sg2, err := ToSubGraph(ctx, parsed.Query[1])
	require.NoError(t, err)
	varsWithUids := map[string]varValue{
		"allowed": {Uids: &pb.List{Uids: []uint64{10, 20, 30}}},
	}
	require.NoError(t, sg2.recursiveFillVars(varsWithUids))
	require.Equal(t, []uint64{10, 20, 30}, sg2.SrcUIDs.GetUids())
}
