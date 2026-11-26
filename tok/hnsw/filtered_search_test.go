/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package hnsw

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/tok/index"
	opt "github.com/dgraph-io/dgraph/v25/tok/options"
	"github.com/dgraph-io/dgraph/v25/x"
)

func TestHNSWSearchWithUIDFilterReturnsTopKAllowed(t *testing.T) {
	ctx := context.Background()

	factory := CreateFactory[float64](64)
	options := opt.NewOptions()
	options.SetOpt(MaxLevelsOpt, 2)
	options.SetOpt(EfSearchOpt, 2)
	options.SetOpt(MetricOpt, GetSimType[float64](Euclidean, 64))

	pred := x.NamespaceAttr(x.RootNamespace, "filtered_search_test_pred")
	rawIdx, err := factory.Create(pred, options, 64)
	require.NoError(t, err)
	ph := rawIdx.(*persistentHNSW[float64])

	data := make(map[string][]byte)
	// Dense cluster of 10 filtered-out nodes (1..10) very close to [0, 0],
	// bridging through node 10 to 4 allowed nodes (101..104) farther away.
	vecs := map[uint64][]float64{
		1:   {0.05, 0},
		2:   {0.10, 0},
		3:   {0.15, 0},
		4:   {0.20, 0},
		5:   {0.25, 0},
		6:   {0.30, 0},
		7:   {0.35, 0},
		8:   {0.40, 0},
		9:   {0.45, 0},
		10:  {0.90, 0}, // bridge to allowed nodes (worse than 1..9!)
		101: {1.10, 0}, // 1st best allowed
		102: {1.20, 0}, // 2nd best allowed
		103: {1.30, 0}, // 3rd best allowed
		104: {2.50, 0}, // 4th allowed (should be evicted from top-3)
	}
	for uid, v := range vecs {
		data[string(DataKey(ph.pred, uid))] = float64ArrayAsBytes(v)
	}
	data[string(DataKey(ph.vecEntryKey, 1))] = Uint64ToBytes(1)

	ph.nodeAllEdges[1] = [][]uint64{{2}, {2, 3, 4, 5, 10}}
	for uid := uint64(2); uid <= 9; uid++ {
		var bottom []uint64
		for other := uint64(1); other <= 9; other++ {
			if other != uid {
				bottom = append(bottom, other)
			}
		}
		ph.nodeAllEdges[uid] = [][]uint64{{1}, bottom}
	}
	ph.nodeAllEdges[10] = [][]uint64{{}, {1, 104, 101, 102, 103}}
	ph.nodeAllEdges[101] = [][]uint64{{}, {10, 102}}
	ph.nodeAllEdges[102] = [][]uint64{{}, {10, 101, 103}}
	ph.nodeAllEdges[103] = [][]uint64{{}, {10, 102, 104}}
	ph.nodeAllEdges[104] = [][]uint64{{}, {10, 103}}

	cache := &memoryCache{data: data}
	allowed := map[uint64]struct{}{101: {}, 102: {}, 103: {}, 104: {}}
	res, err := ph.SearchWithOptions(ctx, cache, []float64{0, 0}, 3, index.VectorIndexOptions[float64]{
		Filter: func(_, _ []float64, uid uint64) bool {
			_, ok := allowed[uid]
			return ok
		},
	})
	require.NoError(t, err)
	require.Equal(t, []uint64{101, 102, 103}, res)
}

func TestHNSWSearchWithAllowedUIDsExactMultiTenant(t *testing.T) {
	ctx := context.Background()

	factory := CreateFactory[float64](64)
	options := opt.NewOptions()
	options.SetOpt(MaxLevelsOpt, 1)
	options.SetOpt(EfSearchOpt, 4)
	options.SetOpt(MetricOpt, GetSimType[float64](Cosine, 64))

	pred := x.NamespaceAttr(x.RootNamespace, "multitenant_cat_search")
	rawIdx, err := factory.Create(pred, options, 64)
	require.NoError(t, err)
	ph := rawIdx.(*persistentHNSW[float64])

	data := make(map[string][]byte)
	// Simulate 10 workspaces x 20 categories = 200 vectors with identical embeddings across workspaces.
	const numWorkspaces = 10
	const numCategories = 20
	var ws9Allowed []uint64
	for ws := 0; ws < numWorkspaces; ws++ {
		for cat := 0; cat < numCategories; cat++ {
			uid := uint64(ws*100 + cat + 1)
			angle := float64(cat) * (math.Pi / 40.0)
			vec := []float64{math.Cos(angle), math.Sin(angle)}
			data[string(DataKey(ph.pred, uid))] = float64ArrayAsBytes(vec)
			if ws == 9 {
				ws9Allowed = append(ws9Allowed, uid)
			}
		}
	}
	// Also include an allowed UID without a stored vector to verify graceful skip.
	ws9Allowed = append(ws9Allowed, 99999)
	data[string(DataKey(ph.vecEntryKey, 1))] = Uint64ToBytes(1)

	cache := &memoryCache{data: data}
	allowedSet := make(map[uint64]struct{}, len(ws9Allowed))
	for _, uid := range ws9Allowed {
		allowedSet[uid] = struct{}{}
	}
	filterFn := func(_, _ []float64, uid uint64) bool {
		_, ok := allowedSet[uid]
		return ok
	}

	// Query aligned with cat=0 ([1, 0]): top-3 in workspace 9 must be 901, 902, 903.
	res, err := ph.SearchWithOptions(ctx, cache, []float64{1, 0}, 3, index.VectorIndexOptions[float64]{
		Filter:      filterFn,
		AllowedUIDs: ws9Allowed,
	})
	require.NoError(t, err)
	require.Equal(t, []uint64{901, 902, 903}, res)

	// SearchWithUidAndOptions (similarToId) starting from workspace 0's cat=0 (UID 1),
	// restricted to workspace 9's AllowedUIDs -> must still return 901, 902, 903.
	resByUid, err := ph.SearchWithUidAndOptions(ctx, cache, 1, 3, index.VectorIndexOptions[float64]{
		Filter:      filterFn,
		AllowedUIDs: ws9Allowed,
	})
	require.NoError(t, err)
	require.Equal(t, []uint64{901, 902, 903}, resByUid)

	// Tight distance threshold (cosine distance <= 0.005 -> only cat=0 and cat=1 qualify).
	threshold := 0.005
	resThreshold, err := ph.SearchWithOptions(ctx, cache, []float64{1, 0}, 5, index.VectorIndexOptions[float64]{
		Filter:            filterFn,
		AllowedUIDs:       ws9Allowed,
		DistanceThreshold: &threshold,
	})
	require.NoError(t, err)
	require.Equal(t, []uint64{901, 902}, resThreshold)
}
