/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"testing"

	"github.com/hypermodeinc/dgraph/v25/dql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// VariableGenerator: shared vs separate collision tests
// ─────────────────────────────────────────────────────────────────────────────

// TestSharedVarGen_NoDuplicates verifies that when a single VariableGenerator
// is reused across multiple calls (as happens when the delete rewriter and
// cascade collector share a generator), no duplicate variable names are produced.
//
// This is a regression test for the "Some variables are declared multiple times"
// error that occurred with filter-based cascade deletes.
func TestSharedVarGen_NoDuplicates(t *testing.T) {
	varGen := NewVariableGenerator()

	// Simulate the delete rewriter consuming some variable names
	// (e.g., for removeNodeReference cleanup on the root type).
	name1 := varGen.NextFromTypeName("Note", false)         // Note_1
	name2 := varGen.NextFromTypeName("CreateRecord", false) // CreateRecord_2

	assert.Equal(t, "Note_1", name1)
	assert.Equal(t, "CreateRecord_2", name2)

	// Simulate cascade delete's buildReverseEdgeCleanup generating
	// more variables using the SAME generator — these must not collide.
	name3 := varGen.NextFromTypeName("Note", false)         // Note_3 (not Note_1)
	name4 := varGen.NextFromTypeName("CreateRecord", false) // CreateRecord_4 (not CreateRecord_2)

	assert.Equal(t, "Note_3", name3,
		"shared VarGen must continue counter past previously used names")
	assert.Equal(t, "CreateRecord_4", name4,
		"shared VarGen must continue counter past previously used names")
}

// TestSeparateVarGen_Collides demonstrates the bug that existed before the fix:
// two independent VariableGenerators produce the same variable names for the
// same types.
func TestSeparateVarGen_Collides(t *testing.T) {
	gen1 := NewVariableGenerator()
	gen2 := NewVariableGenerator()

	name1a := gen1.NextFromTypeName("Note", false)         // Note_1
	name1b := gen1.NextFromTypeName("CreateRecord", false) // CreateRecord_2

	name2a := gen2.NextFromTypeName("Note", false)         // Note_1 — collision!
	name2b := gen2.NextFromTypeName("CreateRecord", false) // CreateRecord_2 — collision!

	// With separate generators, names collide — this was the bug.
	assert.Equal(t, name1a, name2a, "separate generators produce identical names (demonstrating the bug)")
	assert.Equal(t, name1b, name2b, "separate generators produce identical names (demonstrating the bug)")
}

// ─────────────────────────────────────────────────────────────────────────────
// BuildCascadePreQuery: augments delete query blocks with a consumer block
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildCascadePreQuery_CollectsAllVars(t *testing.T) {
	// Simulate a delete query with root var "x" and child vars.
	queryBlocks := []*dql.GraphQuery{
		{
			Var:  "x",
			Attr: "deleteNote",
			Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: "Note"}}},
			Children: []*dql.GraphQuery{
				{Attr: "uid"},
				{Var: "CreateRecord_1", Attr: "Recordable.hasCreateRecord"},
				{Var: "Tag_2", Attr: "Tagable.hasTag"},
			},
		},
	}

	result := BuildCascadePreQuery(queryBlocks, "x", "Note")

	// Original blocks + 1 consumer block
	require.Len(t, result, 2, "must append exactly one consumer block")

	consumer := result[1]
	assert.Equal(t, "cascadeRoots", consumer.Attr, "consumer block attr must be 'cascadeRoots'")

	// Consumer must reference ALL variables (x, CreateRecord_1, Tag_2)
	require.NotNil(t, consumer.Func)
	assert.Equal(t, "uid", consumer.Func.Name)

	varNames := make([]string, 0)
	for _, arg := range consumer.Func.Args {
		varNames = append(varNames, arg.Value)
	}
	assert.Contains(t, varNames, "x")
	assert.Contains(t, varNames, "CreateRecord_1")
	assert.Contains(t, varNames, "Tag_2")

	// Consumer must have a type filter for the root type
	require.NotNil(t, consumer.Filter)
	require.NotNil(t, consumer.Filter.Func)
	assert.Equal(t, "type", consumer.Filter.Func.Name)
	require.NotEmpty(t, consumer.Filter.Func.Args)
	assert.Equal(t, "Note", consumer.Filter.Func.Args[0].Value)

	// Consumer must select uid
	require.Len(t, consumer.Children, 1)
	assert.Equal(t, "uid", consumer.Children[0].Attr)
}

func TestBuildCascadePreQuery_NoVars_PassThrough(t *testing.T) {
	// If no variables are defined, BuildCascadePreQuery should not add a consumer.
	queryBlocks := []*dql.GraphQuery{
		{
			Attr: "deleteNote",
			Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: "Note"}}},
			Children: []*dql.GraphQuery{
				{Attr: "uid"},
			},
		},
	}

	result := BuildCascadePreQuery(queryBlocks, "x", "Note")

	// No vars → no consumer block added
	assert.Len(t, result, 1, "no consumer block should be added when no vars exist")
}

func TestBuildCascadePreQuery_NeedsVar_Populated(t *testing.T) {
	// Verify that NeedsVar is populated for each variable reference.
	queryBlocks := []*dql.GraphQuery{
		{
			Var:  "x",
			Attr: "deleteNote",
			Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: "Note"}}},
			Children: []*dql.GraphQuery{
				{Attr: "uid"},
				{Var: "CR_1", Attr: "Recordable.hasCreateRecord"},
			},
		},
	}

	result := BuildCascadePreQuery(queryBlocks, "x", "Note")
	consumer := result[1]

	require.Len(t, consumer.Func.NeedsVar, 2, "NeedsVar must have entries for each variable")

	needsVarNames := make([]string, 0)
	for _, nv := range consumer.Func.NeedsVar {
		needsVarNames = append(needsVarNames, nv.Name)
		assert.Equal(t, dql.UidVar, nv.Typ, "all NeedsVar entries must be UidVar type")
	}
	assert.Contains(t, needsVarNames, "x")
	assert.Contains(t, needsVarNames, "CR_1")
}

func TestBuildCascadePreQuery_NestedVars(t *testing.T) {
	// Variables can be nested multiple levels deep in the query tree.
	queryBlocks := []*dql.GraphQuery{
		{
			Var:  "x",
			Attr: "deleteGroup",
			Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: "Group"}}},
			Children: []*dql.GraphQuery{
				{Attr: "uid"},
				{
					Var:  "Candidate_1",
					Attr: "Group.hasCandidate",
					Children: []*dql.GraphQuery{
						{Var: "Address_2", Attr: "Candidate.hasAddress"},
					},
				},
			},
		},
	}

	result := BuildCascadePreQuery(queryBlocks, "x", "Group")
	consumer := result[1]

	// Must find all 3 vars: x, Candidate_1, Address_2
	varNames := make([]string, 0)
	for _, arg := range consumer.Func.Args {
		varNames = append(varNames, arg.Value)
	}
	assert.Len(t, varNames, 3)
	assert.Contains(t, varNames, "x")
	assert.Contains(t, varNames, "Candidate_1")
	assert.Contains(t, varNames, "Address_2")
}

// ─────────────────────────────────────────────────────────────────────────────
// Deeply nested resolve-level tests
// ─────────────────────────────────────────────────────────────────────────────

// TestBuildCascadePreQuery_DeeplyNested_5Levels simulates a delete query for
// a 5-level cascade chain: Org → Division → Team → Member → Badge.
// Verifies that all 5 variable names are collected into the consumer block.
func TestBuildCascadePreQuery_DeeplyNested_5Levels(t *testing.T) {
	queryBlocks := []*dql.GraphQuery{
		{
			Var:  "x",
			Attr: "deleteOrg",
			Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: "Org"}}},
			Children: []*dql.GraphQuery{
				{Attr: "uid"},
				{
					Var:  "Division_1",
					Attr: "Org.hasDivision",
					Children: []*dql.GraphQuery{
						{
							Var:  "Team_2",
							Attr: "Division.hasTeam",
							Children: []*dql.GraphQuery{
								{
									Var:  "Member_3",
									Attr: "Team.hasMember",
									Children: []*dql.GraphQuery{
										{Var: "Badge_4", Attr: "Member.hasBadge"},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	result := BuildCascadePreQuery(queryBlocks, "x", "Org")
	require.Len(t, result, 2, "must add consumer block")

	consumer := result[1]
	varNames := make([]string, 0)
	for _, arg := range consumer.Func.Args {
		varNames = append(varNames, arg.Value)
	}

	// All 5 variables must be collected
	assert.Len(t, varNames, 5, "5-level chain must produce 5 variables")
	assert.Contains(t, varNames, "x")
	assert.Contains(t, varNames, "Division_1")
	assert.Contains(t, varNames, "Team_2")
	assert.Contains(t, varNames, "Member_3")
	assert.Contains(t, varNames, "Badge_4")
}

// TestBuildCascadePreQuery_DiamondPattern simulates a diamond-shaped query
// tree: root → B (with child D), root → C (with child D2).
func TestBuildCascadePreQuery_DiamondPattern(t *testing.T) {
	queryBlocks := []*dql.GraphQuery{
		{
			Var:  "x",
			Attr: "deleteA",
			Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: "A"}}},
			Children: []*dql.GraphQuery{
				{Attr: "uid"},
				{
					Var:  "B_1",
					Attr: "A.hasB",
					Children: []*dql.GraphQuery{
						{Var: "D_2", Attr: "B.hasD"},
					},
				},
				{
					Var:  "C_3",
					Attr: "A.hasC",
					Children: []*dql.GraphQuery{
						{Var: "D_4", Attr: "C.hasD"},
					},
				},
			},
		},
	}

	result := BuildCascadePreQuery(queryBlocks, "x", "A")
	require.Len(t, result, 2)

	consumer := result[1]
	varNames := make([]string, 0)
	for _, arg := range consumer.Func.Args {
		varNames = append(varNames, arg.Value)
	}

	assert.Len(t, varNames, 5, "diamond must collect x, B_1, D_2, C_3, D_4")
	assert.Contains(t, varNames, "x")
	assert.Contains(t, varNames, "B_1")
	assert.Contains(t, varNames, "D_2")
	assert.Contains(t, varNames, "C_3")
	assert.Contains(t, varNames, "D_4")
}

// TestBuildCascadePreQuery_FanOut_MultipleBlocks verifies that when the upsert query
// has multiple blocks (e.g. root delete block + auth var blocks), only the root block
// and its children are included in the consumer's uid() function.
// Auth var blocks are intentionally excluded to avoid expensive traversals causing timeouts.
func TestBuildCascadePreQuery_FanOut_MultipleBlocks(t *testing.T) {
	queryBlocks := []*dql.GraphQuery{
		{
			Var:  "x",
			Attr: "deleteRoot",
			Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: "Root"}}},
			Children: []*dql.GraphQuery{
				{Attr: "uid"},
				{Var: "Alpha_1", Attr: "Root.hasAlpha"},
				{Var: "Beta_2", Attr: "Root.hasBeta"},
				{Var: "Gamma_3", Attr: "Root.hasGamma"},
			},
		},
		// Simulate a cascadeAuth var block — must NOT be included in consumer.
		{
			Var:  "Root_Auth4",
			Attr: "var",
			Func: &dql.Function{Name: "uid", Args: []dql.Arg{{Value: "x"}}},
			Children: []*dql.GraphQuery{
				{Attr: "Root.workspace"},
			},
		},
	}

	result := BuildCascadePreQuery(queryBlocks, "x", "Root")
	require.Len(t, result, 3, "original 2 blocks + 1 consumer")

	consumer := result[2]
	varNames := make([]string, 0)
	for _, arg := range consumer.Func.Args {
		varNames = append(varNames, arg.Value)
	}

	// Only root block vars: x, Alpha_1, Beta_2, Gamma_3
	// Root_Auth4 must NOT be present — it's an auth var block, not part of delete query.
	assert.Len(t, varNames, 4, "only root block vars must be collected, not auth block vars")
	assert.Contains(t, varNames, "x")
	assert.Contains(t, varNames, "Alpha_1")
	assert.Contains(t, varNames, "Beta_2")
	assert.Contains(t, varNames, "Gamma_3")
	assert.NotContains(t, varNames, "Root_Auth4", "auth var block must NOT be included in consumer")
}

// TestSharedVarGen_Deeply_Nested_5Types verifies that a single VarGen
// produces 10 unique variable names across 5 type names (2 each),
// simulating what happens when both the delete rewriter and cascade
// collector generate reverse-edge cleanup vars for a 5-level chain.
func TestSharedVarGen_Deeply_Nested_5Types(t *testing.T) {
	varGen := NewVariableGenerator()

	types := []string{"Org", "Division", "Team", "Member", "Badge"}

	// Round 1: delete rewriter generates vars
	round1 := make(map[string]string, len(types))
	for _, typ := range types {
		round1[typ] = varGen.NextFromTypeName(typ, false)
	}

	// Round 2: cascade collector generates vars for same types
	round2 := make(map[string]string, len(types))
	for _, typ := range types {
		round2[typ] = varGen.NextFromTypeName(typ, false)
	}

	// All 10 names must be unique
	allNames := make(map[string]bool)
	for _, name := range round1 {
		assert.False(t, allNames[name], "round1 var %q must be unique", name)
		allNames[name] = true
	}
	for _, name := range round2 {
		assert.False(t, allNames[name], "round2 var %q must be unique (no collision with round1)", name)
		allNames[name] = true
	}
	assert.Len(t, allNames, 10, "10 total unique variable names must be generated")

	// Verify no overlap between rounds
	for typ := range round1 {
		assert.NotEqual(t, round1[typ], round2[typ],
			"same type %q must get different var names in round 1 vs 2", typ)
	}
}
