/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/algo"
	"github.com/dgraph-io/dgraph/v25/chunker"
	"github.com/dgraph-io/dgraph/v25/conn"
	"github.com/dgraph-io/dgraph/v25/dql"
	"github.com/dgraph-io/dgraph/v25/posting"
	"github.com/dgraph-io/dgraph/v25/protos/pb"
	"github.com/dgraph-io/dgraph/v25/schema"
	"github.com/dgraph-io/dgraph/v25/types"
	"github.com/dgraph-io/dgraph/v25/x"
)

type svTestEnv struct {
	t      *testing.T
	dir    string
	ps     *badger.DB
	nextTs uint64
}

func newSVTestEnv(t *testing.T, initialSchema string) *svTestEnv {
	t.Helper()
	dir, err := os.MkdirTemp("", "stringvector_test_")
	require.NoError(t, err)

	opt := badger.DefaultOptions(dir).WithLogger(nil)
	ps, err := badger.OpenManaged(opt)
	require.NoError(t, err)

	pstore = ps
	posting.Init(ps, 0, false)
	Init(ps)
	schema.Init(ps)
	if initialSchema != "" {
		require.NoError(t, schema.ParseBytes([]byte(initialSchema), 1))
	}
	gr = new(groupi)
	gr.gid = 1
	gr.Node = &node{Node: &conn.Node{Id: 1}}
	gr.tablets = make(map[string]*pb.Tablet)
	for _, pred := range schema.State().Predicates() {
		gr.tablets[pred] = &pb.Tablet{GroupId: 1}
	}

	env := &svTestEnv{
		t:      t,
		dir:    dir,
		ps:     ps,
		nextTs: 2,
	}
	t.Cleanup(func() {
		_ = ps.Close()
		_ = os.RemoveAll(dir)
	})
	return env
}

func (e *svTestEnv) applyEdges(rawEdges []*pb.DirectedEdge) uint64 {
	e.t.Helper()
	ctx := context.Background()
	startTs := e.nextTs
	commitTs := e.nextTs + 1
	e.nextTs += 2

	wrtCtx := schema.GetWriteContext(ctx)
	// Stage 1: simulate proposeAndWait (prepareVStringEdges + ValidateAndConvert before Raft).
	proposed := prepareVStringEdges(wrtCtx, rawEdges)
	for _, edge := range proposed {
		if su, ok := schema.State().Get(wrtCtx, edge.Attr); ok {
			require.NoError(e.t, ValidateAndConvert(edge, &su))
		}
	}

	// Stage 2: simulate applyMutations after Raft commit.
	edges := prepareVStringEdges(wrtCtx, proposed)

	txn := posting.Oracle().RegisterStartTs(startTs)
	for _, edge := range edges {
		require.NoError(e.t, runMutation(ctx, edge, txn))
	}
	od := &pb.OracleDelta{
		MaxAssigned: commitTs,
		Txns:        []*pb.TxnStatus{{StartTs: startTs, CommitTs: commitTs}},
	}
	posting.Oracle().ProcessDelta(od)

	txn.Update()
	writer := posting.NewTxnWriter(e.ps)
	require.NoError(e.t, txn.CommitToDisk(writer, commitTs))
	require.NoError(e.t, writer.Flush())
	txn.UpdateCachedKeys(commitTs)
	return commitTs
}

func (e *svTestEnv) mutateJSONSet(payload string) uint64 {
	e.t.Helper()
	nqs, emptyLists, _, err := chunker.ParseJSONWithEmptyLists([]byte(payload), chunker.SetNquads)
	require.NoError(e.t, err)

	var edges []*pb.DirectedEdge
	uidMap := map[string]uint64{}
	for _, nq := range nqs {
		dnq := dql.NQuad{NQuad: nq}
		edge, err := dnq.ToEdgeUsing(uidMap)
		require.NoError(e.t, err)
		edge.Attr = x.AttrInRootNamespace(edge.Attr)
		edge.Op = pb.DirectedEdge_SET
		edges = append(edges, edge)
	}
	for _, nq := range emptyLists {
		dnq := dql.NQuad{NQuad: nq}
		edge, err := dnq.ToEmptyListEdgeUsing(uidMap)
		require.NoError(e.t, err)
		edge.Attr = x.AttrInRootNamespace(edge.Attr)
		edge.Op = pb.DirectedEdge_SET
		edges = append(edges, edge)
	}
	return e.applyEdges(edges)
}

func (e *svTestEnv) readStringVector(attr string, uid, readTs uint64) ([]string, bool) {
	e.t.Helper()
	pl, err := posting.GetNoStore(x.DataKey(x.AttrInRootNamespace(attr), uid), readTs)
	require.NoError(e.t, err)
	val, err := pl.Value(readTs)
	if err == posting.ErrNoValue {
		return nil, false
	}
	require.NoError(e.t, err)
	require.Equal(e.t, types.VStringID, val.Tid)
	sv, err := types.Convert(val, types.VStringID)
	require.NoError(e.t, err)
	return sv.Value.([]string), true
}

func TestStringVectorOrderDuplicatesEmptyAndDelete(t *testing.T) {
	env := newSVTestEnv(t, `
		steps: stringvector .
		raw_json: string .
		tags_set: [string] .
		hasPortalForm: [uid] .
		scores: [int] .
	`)

	// 1. Preserve order, duplicates, empty strings, and Unicode; ensure JSON string on plain string
	// predicate is NOT sniffed or altered, and empty [] on [uid], [int], and [string] is a no-op.
	readTs := env.mutateJSONSet(`{
		"uid": "0x1",
		"steps": ["beta", "alpha", "beta", "", "🚀"],
		"raw_json": "[\"not\", \"a\", \"vector\"]",
		"tags_set": [],
		"hasPortalForm": [],
		"scores": []
	}`)

	got, ok := env.readStringVector("steps", 1, readTs)
	require.True(t, ok)
	require.Equal(t, []string{"beta", "alpha", "beta", "", "🚀"}, got)

	// Verify raw_json stayed Posting_STRING with exact literal JSON text.
	rawPl, err := posting.GetNoStore(x.DataKey(x.AttrInRootNamespace("raw_json"), 1), readTs)
	require.NoError(t, err)
	rawVal, err := rawPl.Value(readTs)
	require.NoError(t, err)
	require.Equal(t, types.StringID, rawVal.Tid)
	strConv, err := types.Convert(rawVal, types.StringID)
	require.NoError(t, err)
	require.Equal(t, `["not", "a", "vector"]`, strConv.Value.(string))

	// Verify tags_set had no postings written for [].
	setPl, err := posting.GetNoStore(x.DataKey(x.AttrInRootNamespace("tags_set"), 1), readTs)
	require.NoError(t, err)
	emptySet, err := setPl.IsEmpty(readTs, 0)
	require.NoError(t, err)
	require.True(t, emptySet)

	// 2. Replace whole ordered list.
	readTs = env.mutateJSONSet(`{"uid": "0x1", "steps": ["only_one"]}`)
	got, ok = env.readStringVector("steps", 1, readTs)
	require.True(t, ok)
	require.Equal(t, []string{"only_one"}, got)

	// 3. Explicit empty list [] overwrites with 0-element VSTRING.
	readTs = env.mutateJSONSet(`{"uid": "0x1", "steps": []}`)
	got, ok = env.readStringVector("steps", 1, readTs)
	require.True(t, ok)
	require.Empty(t, got)

	// 4. Delete S P * removes the posting completely.
	readTs = env.applyEdges([]*pb.DirectedEdge{{
		Entity:    1,
		Attr:      x.AttrInRootNamespace("steps"),
		Value:     []byte(x.Star),
		ValueType: pb.Posting_DEFAULT,
		Op:        pb.DirectedEdge_DEL,
	}})
	_, ok = env.readStringVector("steps", 1, readTs)
	require.False(t, ok)
}

func TestStringVectorIndexesFiltersAndSharedTokenUpdate(t *testing.T) {
	env := newSVTestEnv(t, `
		items: stringvector @index(exact, term, fulltext, trigram, ngram) .
		hashed: stringvector @index(hash) .
		unindexed: stringvector .
	`)

	env.mutateJSONSet(`[
		{
			"uid": "0x1",
			"items": ["quick brown", "alpha", "quick"],
			"hashed": ["secret_token_1", "common"],
			"unindexed": ["delta", "gamma"]
		},
		{
			"uid": "0x2",
			"items": ["lazy dog", "charlie"],
			"hashed": ["secret_token_2", "common"],
			"unindexed": ["epsilon"]
		}
	]`)
	readTs := env.nextTs - 1
	ctx := context.Background()

	runQuery := func(q *pb.Query) []uint64 {
		res, err := processTask(ctx, q, 1)
		require.NoError(t, err)
		if len(res.UidMatrix) == 0 {
			return nil
		}
		if res.IntersectDest {
			acc := &pb.List{Uids: append([]uint64(nil), res.UidMatrix[0].Uids...)}
			for _, ul := range res.UidMatrix[1:] {
				algo.IntersectWith(acc, ul, acc)
			}
			return acc.Uids
		}
		return algo.MergeSorted(res.UidMatrix).Uids
	}

	itemsAttr := x.AttrInRootNamespace("items")
	hashedAttr := x.AttrInRootNamespace("hashed")
	unindexedAttr := x.AttrInRootNamespace("unindexed")

	// exact: eq, gt, between
	require.Equal(t, []uint64{1}, runQuery(&pb.Query{
		Attr:    itemsAttr,
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "eq", Args: []string{"alpha"}},
	}))
	require.Equal(t, []uint64{1, 2}, runQuery(&pb.Query{
		Attr:    itemsAttr,
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "between", Args: []string{"alpha", "charlie"}},
	}))

	// hash (lossy tokenizer -> exercises handleCompareFunction on VStringID!)
	require.Equal(t, []uint64{2}, runQuery(&pb.Query{
		Attr:    hashedAttr,
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "eq", Args: []string{"secret_token_2"}},
	}))

	// term: anyofterms, allofterms (across elements in same stringvector!)
	require.Equal(t, []uint64{1}, runQuery(&pb.Query{
		Attr:    itemsAttr,
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "allofterms", Args: []string{"quick alpha"}},
	}))

	// fulltext: alloftext
	require.Equal(t, []uint64{2}, runQuery(&pb.Query{
		Attr:    itemsAttr,
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "alloftext", Args: []string{"lazy charlie"}},
	}))

	// trigram: regexp & fuzzy match
	require.Equal(t, []uint64{1}, runQuery(&pb.Query{
		Attr:    itemsAttr,
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "regexp", Args: []string{"^alph.*", ""}},
	}))
	require.Equal(t, []uint64{2}, runQuery(&pb.Query{
		Attr:    itemsAttr,
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "match", Args: []string{"charli", "2"}},
	}))

	// ngram
	require.Equal(t, []uint64{1}, runQuery(&pb.Query{
		Attr:    itemsAttr,
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "ngram", Args: []string{"quick brown"}},
	}))

	// unindexed @filter (exercises handleValuePostings compareAttrFn on VStringID)
	require.Equal(t, []uint64{1}, runQuery(&pb.Query{
		Attr:    unindexedAttr,
		ReadTs:  readTs,
		UidList: &pb.List{Uids: []uint64{1, 2}},
		SrcFunc: &pb.SrcFunction{Name: "eq", Args: []string{"gamma"}},
	}))

	// Shared-token update test: update 0x1 items from ["quick brown", "alpha", "quick"]
	// to ["quick silver"] -> "quick" MUST remain indexed while "brown" and "alpha" are removed!
	readTs = env.mutateJSONSet(`{"uid": "0x1", "items": ["quick silver"]}`)
	require.Equal(t, []uint64{1}, runQuery(&pb.Query{
		Attr:    itemsAttr,
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "anyofterms", Args: []string{"quick"}},
	}))
	require.Empty(t, runQuery(&pb.Query{
		Attr:    itemsAttr,
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "anyofterms", Args: []string{"alpha"}},
	}))
	require.Equal(t, []uint64{1}, runQuery(&pb.Query{
		Attr:    itemsAttr,
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "anyofterms", Args: []string{"silver"}},
	}))

	// Sorting must be rejected on stringvector.
	_, sortErr := processSort(ctx, &pb.SortMessage{
		Order:     []*pb.Order{{Attr: itemsAttr}},
		UidMatrix: []*pb.List{{Uids: []uint64{1, 2}}},
		Count:     10,
		ReadTs:    readTs,
	})
	require.Error(t, sortErr)
	require.Contains(t, sortErr.Error(), "Sorting not supported")
}

func TestStringVectorCountAndOnlineMigrations(t *testing.T) {
	env := newSVTestEnv(t, `
		steps: stringvector @count .
		plain_steps: stringvector .
		legacy_set: [string] @index(exact) @count .
		legacy_scalar: string .
	`)

	readTs := env.mutateJSONSet(`[
		{"uid": "0x1", "steps": ["a", "b", "a"], "plain_steps": ["a", "b", "a"], "legacy_set": ["x", "y"], "legacy_scalar": "hello world"},
		{"uid": "0x2", "steps": ["only"], "plain_steps": ["only"], "legacy_set": ["z"]},
		{"uid": "0x3", "steps": [], "plain_steps": []}
	]`)
	ctx := context.Background()
	stepsAttr := x.AttrInRootNamespace("steps")
	plainAttr := x.AttrInRootNamespace("plain_steps")

	// 1. DoCount projection returns element counts (3, 1, 0), counting duplicates!
	res, err := processTask(ctx, &pb.Query{
		Attr:    stepsAttr,
		ReadTs:  readTs,
		DoCount: true,
		UidList: &pb.List{Uids: []uint64{1, 2, 3, 4}},
	}, 1)
	require.NoError(t, err)
	require.Equal(t, []uint32{3, 1, 0, 0}, res.Counts)

	// 2. @count index root queries: eq(count(steps), 3), le(count(steps), 3) (must NOT include empty 0x3!)
	res, err = processTask(ctx, &pb.Query{
		Attr:    stepsAttr,
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "eq", Args: []string{"3"}, IsCount: true},
	}, 1)
	require.NoError(t, err)
	require.Equal(t, []uint64{1}, res.UidMatrix[0].Uids)

	res, err = processTask(ctx, &pb.Query{
		Attr:    stepsAttr,
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "le", Args: []string{"3"}, IsCount: true},
	}, 1)
	require.NoError(t, err)
	var leUids []uint64
	for _, ul := range res.UidMatrix {
		leUids = append(leUids, ul.Uids...)
	}
	sort.Slice(leUids, func(i, j int) bool { return leUids[i] < leUids[j] })
	require.Equal(t, []uint64{1, 2}, leUids)

	// 3. @filter count without @count index (compareScalarFn on plain_steps)
	res, err = processTask(ctx, &pb.Query{
		Attr:    plainAttr,
		ReadTs:  readTs,
		UidList: &pb.List{Uids: []uint64{1, 2, 3}},
		SrcFunc: &pb.SrcFunction{Name: "eq", Args: []string{"0"}, IsCount: true},
	}, 1)
	require.NoError(t, err)
	require.Len(t, res.UidMatrix, 1)
	require.Equal(t, []uint64{3}, res.UidMatrix[0].Uids)

	// 4. Online migrations via IndexRebuild:
	//    a) [string] @index(exact) @count -> stringvector @index(exact) @count
	runRebuild := func(attrName string, newSchema *pb.SchemaUpdate) uint64 {
		attr := x.AttrInRootNamespace(attrName)
		newSchema.Predicate = attr
		old, ok := schema.State().Get(ctx, attr)
		require.True(t, ok)

		rbTs := env.nextTs
		env.nextTs += 2
		rb := posting.IndexRebuild{
			Attr:          attr,
			StartTs:       rbTs,
			OldSchema:     &old,
			CurrentSchema: newSchema,
		}
		schema.State().Set(attr, rb.GetQuerySchema())
		schema.State().SetMutSchema(attr, newSchema)
		wrtCtx := schema.GetWriteContext(ctx)
		require.NoError(t, rb.DropIndexes(wrtCtx))
		require.NoError(t, rb.BuildData(wrtCtx))
		if rb.NeedIndexRebuild() {
			require.NoError(t, rb.BuildIndexes(wrtCtx))
		}
		schema.State().Set(attr, newSchema)
		schema.State().DeleteMutSchema(attr)
		posting.ResetCache()
		return rbTs + 1
	}

	readTs = runRebuild("legacy_set", &pb.SchemaUpdate{
		ValueType: pb.Posting_VSTRING,
		Directive: pb.SchemaUpdate_INDEX,
		Tokenizer: []string{"exact"},
		Count:     true,
	})
	migratedElems, ok := env.readStringVector("legacy_set", 1, readTs)
	require.True(t, ok)
	require.ElementsMatch(t, []string{"x", "y"}, migratedElems)

	// Verify count(legacy_set) == 2 after [string] -> stringvector migration.
	res, err = processTask(ctx, &pb.Query{
		Attr:    x.AttrInRootNamespace("legacy_set"),
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "eq", Args: []string{"2"}, IsCount: true},
	}, 1)
	require.NoError(t, err)
	require.Equal(t, []uint64{1}, res.UidMatrix[0].Uids)

	//    b) stringvector @count (["a","b","a"], count=3) -> [string] @count (deduplicates to {"a","b"}, count=2!)
	readTs = runRebuild("steps", &pb.SchemaUpdate{
		ValueType: pb.Posting_STRING,
		List:      true,
		Count:     true,
	})
	res, err = processTask(ctx, &pb.Query{
		Attr:    stepsAttr,
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "eq", Args: []string{"2"}, IsCount: true},
	}, 1)
	require.NoError(t, err)
	require.Equal(t, []uint64{1}, res.UidMatrix[0].Uids)

	//    c) string -> stringvector @index(term)
	readTs = runRebuild("legacy_scalar", &pb.SchemaUpdate{
		ValueType: pb.Posting_VSTRING,
		Directive: pb.SchemaUpdate_INDEX,
		Tokenizer: []string{"term"},
	})
	scalarMigrated, ok := env.readStringVector("legacy_scalar", 1, readTs)
	require.True(t, ok)
	require.Equal(t, []string{"hello world"}, scalarMigrated)
	res, err = processTask(ctx, &pb.Query{
		Attr:    x.AttrInRootNamespace("legacy_scalar"),
		ReadTs:  readTs,
		SrcFunc: &pb.SrcFunction{Name: "anyofterms", Args: []string{"world"}},
	}, 1)
	require.NoError(t, err)
	require.Equal(t, []uint64{1}, res.UidMatrix[0].Uids)
}

func TestStringVectorExportRoundTrip(t *testing.T) {
	env := newSVTestEnv(t, `steps: stringvector .`)
	readTs := env.mutateJSONSet(`{"uid": "0x1", "steps": ["first", "second", "first"]}`)

	pl, err := posting.GetNoStore(x.DataKey(x.AttrInRootNamespace("steps"), 1), readTs)
	require.NoError(t, err)

	exp := &exporter{
		pl:        pl,
		uid:       1,
		attr:      "steps",
		namespace: 0,
		readTs:    readTs,
	}

	// JSON export outputs raw JSON array ["first","second","first"]
	jsonKV, err := exp.toJSON()
	require.NoError(t, err)
	require.Len(t, jsonKV.Kv, 1)
	var exportedObj struct {
		UID   string   `json:"uid"`
		Steps []string `json:"steps"`
	}
	require.NoError(t, json.Unmarshal(jsonKV.Kv[0].Value, &exportedObj))
	require.Equal(t, []string{"first", "second", "first"}, exportedObj.Steps)

	// RDF export outputs escaped JSON array with ^^<xs:[]string> and round-trips through ParseRDFs
	rdfKV, err := exp.toRDF()
	require.NoError(t, err)
	require.Len(t, rdfKV.Kv, 1)
	require.Contains(t, string(rdfKV.Kv[0].Value), "^^<xs:[]string>")

	nqs, _, err := chunker.ParseRDFs(rdfKV.Kv[0].Value)
	require.NoError(t, err)
	require.Len(t, nqs, 1)
	dnq := dql.NQuad{NQuad: nqs[0]}
	edge, err := dnq.ToEdgeUsing(map[string]uint64{})
	require.NoError(t, err)
	su, ok := schema.State().Get(context.Background(), x.AttrInRootNamespace("steps"))
	require.True(t, ok)
	require.NoError(t, ValidateAndConvert(edge, &su))
	require.Equal(t, pb.Posting_VSTRING, edge.ValueType)
	require.Equal(t, []string{"first", "second", "first"}, types.BytesAsStringArray(edge.Value))
}
