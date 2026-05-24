/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

//nolint:lll
package resolve

// ---------------------------------------------------------------------------
// cascade_auth_exec_test.go — data-driven tests with example fixture data
//
// These tests complement the DQL-pinning tests in cascade_auth_dql_test.go.
// Where those tests verify the generated query *text*, these tests verify that
// the generated query *semantics* are correct: given a concrete Dgraph store
// state, the right entities are visible to the right users.
//
// Approach
// --------
// We do NOT require a live Dgraph. Instead we implement a lightweight
// fixtureDB that can evaluate the subset of DQL variable patterns we generate:
//
//   Pattern 1  var(func: type(T))
//              → all UIDs whose stored type == T
//
//   Pattern 2  var(func: uid(varName)) @cascade { pred @filter(eq(p,v)) }
//              → UIDs in varName whose `pred` edge contains a node with
//                predicate p == v  (the @cascade node-presence check)
//
//   Pattern 3  var(func: type(T)) { pred @filter(eq(p,v)) }
//   (no @cascade) → same node set as Pattern 1; the child filter only
//              controls which *child* UIDs are retained for child-var
//              assignments.  When the child block contains a var-assign
//              ("pred as ChildVar") the assigned var collects the matching
//              child UIDs.
//
//   Pattern 4  var(func: uid(varName)) { ChildVar as inversePred }
//              → collect UIDs reachable via inversePred from varName nodes
//
//   Pattern 5  @filter(uid(A) AND uid(B))  /  @filter(uid(A) OR uid(B))
//              → set intersection / union applied to the root query
//
// The fixtureDB stores nodes as:
//   uid → { typeName, edges map[predName][]uid, scalars map[predName]string }
//
// All edges and scalars use Dgraph-style predicate names (e.g. "Group.inUsers",
// "User.email").  @hasInverse reverse edges are stored explicitly so that the
// evaluator can walk them (Dgraph auto-maintains these in prod).
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/dql"
	"github.com/hypermodeinc/dgraph/v25/graphql/dgraph"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/graphql/test"
	"github.com/hypermodeinc/dgraph/v25/testutil"
)

// ---------------------------------------------------------------------------
// fixtureNode — in-memory representation of a single Dgraph node
// ---------------------------------------------------------------------------

type fixtureNode struct {
	typ     string              // e.g. "Group"
	scalars map[string]string   // predName → scalar value, e.g. "User.email" → "alice@example.com"
	edges   map[string][]uint64 // predName → list of target UIDs
}

// ---------------------------------------------------------------------------
// fixtureDB — the in-memory store
// ---------------------------------------------------------------------------

type uidSet = map[uint64]bool

type fixtureDB struct {
	nodes map[uint64]*fixtureNode
}

func newFixtureDB() *fixtureDB {
	return &fixtureDB{nodes: make(map[uint64]*fixtureNode)}
}

func (db *fixtureDB) addNode(uid uint64, typ string) *fixtureNode {
	n := &fixtureNode{
		typ:     typ,
		scalars: make(map[string]string),
		edges:   make(map[string][]uint64),
	}
	db.nodes[uid] = n
	return n
}

func (n *fixtureNode) setScalar(pred, val string) *fixtureNode {
	n.scalars[pred] = val
	return n
}

func (n *fixtureNode) addEdge(pred string, targets ...uint64) *fixtureNode {
	n.edges[pred] = append(n.edges[pred], targets...)
	return n
}

// ---------------------------------------------------------------------------
// Variable evaluator — walks the DQL query statement-by-statement and fills
// a vars map[string]uidSet (variable name → UID set).
//
// IMPORTANT: dql.Parse sets Attr="" for ALL top-level blocks. We identify a
// block as a var-block if it has q.Var != "" OR has a child with Var != "".
// The main result block is the one with q.Var == "" and no child with Var != "".
//
// We use fixed-point iteration because the DQL rewriter may emit GroupRoot
// (which depends on Group_Auth2, Group_Auth4) before those vars are set.
// ---------------------------------------------------------------------------

// evalVars evaluates all var blocks in parsedQuery and returns the populated
// vars map. Uses fixed-point iteration so dependency ordering doesn't matter.
func (db *fixtureDB) evalVars(parsedQuery *dql.Result) (map[string]uidSet, error) {
	vars := make(map[string]uidSet)
	for {
		prevSize := len(vars)
		if err := db.evalVarsOnce(parsedQuery, vars); err != nil {
			return nil, err
		}
		if len(vars) == prevSize {
			break // stable: no new vars were added this pass.
		}
	}
	return vars, nil
}

// evalVarsOnce does a single pass over all var blocks, writing into vars.
func (db *fixtureDB) evalVarsOnce(parsedQuery *dql.Result, vars map[string]uidSet) error {
	for _, q := range parsedQuery.Query {
		// Determine which kind of block this is.
		hasOuterVar := q.Var != ""
		hasInnerVar := false
		for _, child := range q.Children {
			if child.Var != "" {
				hasInnerVar = true
				break
			}
		}
		if !hasOuterVar && !hasInnerVar {
			continue // main result-shaping block: skip.
		}

		rootUIDs, evalErr := db.evalRootFunc(q, vars)
		if evalErr != nil {
			return evalErr
		}

		if hasInnerVar {
			// Anonymous-hop block: "var(func:uid(A)) { B as pred }".
			// Root UIDs are traversed to collect inner vars; outer var (if
			// present) also gets assigned.
			if hasOuterVar {
				if len(q.Cascade) > 0 {
					filtered := make(uidSet)
					for uid := range rootUIDs {
						if db.cascadeMatches(uid, q.Children) {
							filtered[uid] = true
						}
					}
					vars[q.Var] = filtered
				} else {
					vars[q.Var] = rootUIDs
				}
			}
			for _, child := range q.Children {
				if child.Var == "" {
					continue
				}
				collected := make(uidSet)
				for uid := range rootUIDs {
					node, ok := db.nodes[uid]
					if !ok {
						continue
					}
					for _, targetUID := range node.edges[child.Attr] {
						if !db.nodeMatchesFilter(targetUID, child.Filter) {
							continue
						}
						collected[targetUID] = true
					}
				}
				vars[child.Var] = collected
			}
		} else if len(q.Cascade) > 0 {
			// @cascade: keep only root UIDs where at least one child passes.
			filtered := make(uidSet)
			for uid := range rootUIDs {
				if db.cascadeMatches(uid, q.Children) {
					filtered[uid] = true
				}
			}
			if q.Var != "" {
				vars[q.Var] = filtered
			}
		} else if len(q.Children) > 0 {
			// Implicit cascade: children act as filters on the root UID set.
			// In Dgraph, any child-edge traversal in a var block narrows the
			// root set to nodes where the traversal returns non-empty results.
			filtered := make(uidSet)
			for uid := range rootUIDs {
				if db.cascadeMatches(uid, q.Children) {
					filtered[uid] = true
				}
			}
			if q.Var != "" {
				vars[q.Var] = filtered
			}
		} else {
			// No children, no cascade: outer var = root UIDs as-is.
			if q.Var != "" {
				vars[q.Var] = rootUIDs
			}
		}
	}
	return nil
}

// evalRootFunc evaluates the func: clause of a GraphQuery, returning the UID
// set it selects from the fixture.
func (db *fixtureDB) evalRootFunc(q *dql.GraphQuery, vars map[string]uidSet) (uidSet, error) {
	if q.Func == nil {
		return make(uidSet), nil
	}
	switch q.Func.Name {
	case "type":
		if len(q.Func.Args) == 0 {
			return make(uidSet), nil
		}
		typName := q.Func.Args[0].Value
		result := make(uidSet)
		for uid, node := range db.nodes {
			if node.typ == typName {
				result[uid] = true
			}
		}
		// Apply root filter (e.g. @filter(uid(A) AND uid(B)))
		if q.Filter != nil {
			return db.applyFilter(result, q.Filter, vars), nil
		}
		return result, nil

	case "uid":
		result := make(uidSet)
		// uid(VarName) stores the variable reference in NeedsVar, not Args.
		// uid(0x1) stores literal UIDs in q.Func.UID.
		for _, needsVar := range q.Func.NeedsVar {
			if s, ok := vars[needsVar.Name]; ok {
				for uid := range s {
					result[uid] = true
				}
			}
		}
		// Also handle any literal UIDs.
		for _, uid := range q.Func.UID {
			result[uid] = true
		}
		// Apply root filter (e.g. @filter(uid(A) AND uid(B)))
		if q.Filter != nil {
			return db.applyFilter(result, q.Filter, vars), nil
		}
		return result, nil
	}
	return make(uidSet), fmt.Errorf("unsupported func: %s", q.Func.Name)
}

// applyFilter restricts a UID set according to a FilterTree.
func (db *fixtureDB) applyFilter(uids uidSet, f *dql.FilterTree, vars map[string]uidSet) uidSet {
	if f == nil {
		return uids
	}
	switch f.Op {
	case "and":
		result := copySet(uids)
		for _, child := range f.Child {
			result = db.applyFilter(result, child, vars)
		}
		return result
	case "or":
		result := make(uidSet)
		for _, child := range f.Child {
			for uid := range db.applyFilter(uids, child, vars) {
				result[uid] = true
			}
		}
		return result
	case "not":
		if len(f.Child) == 0 {
			return uids
		}
		excluded := db.applyFilter(uids, f.Child[0], vars)
		result := make(uidSet)
		for uid := range uids {
			if !excluded[uid] {
				result[uid] = true
			}
		}
		return result
	}
	// Leaf filter
	if f.Func == nil {
		return uids
	}
	switch f.Func.Name {
	case "uid":
		// Keep only UIDs that are in the variable set(s) referenced.
		// Variable names in uid() filters are in NeedsVar, not Args.
		allowed := make(uidSet)
		for _, nv := range f.Func.NeedsVar {
			if s, ok := vars[nv.Name]; ok {
				for uid := range s {
					allowed[uid] = true
				}
			}
		}
		// Also account for literal UIDs in the filter.
		for _, uid := range f.Func.UID {
			allowed[uid] = true
		}
		result := make(uidSet)
		for uid := range uids {
			if allowed[uid] {
				result[uid] = true
			}
		}
		return result
	case "uid_in":
		// uid_in(pred, uid(var)) — keep nodes that have at least one edge
		// on pred pointing to a UID in the variable set. This is the
		// Dgraph index-based reverse lookup used by cascade auth.
		//
		// Parsed structure: Func.Attr = pred, Func.NeedsVar = [varName]
		pred := f.Func.Attr
		allowed := make(uidSet)
		for _, nv := range f.Func.NeedsVar {
			if s, ok := vars[nv.Name]; ok {
				for uid := range s {
					allowed[uid] = true
				}
			}
		}
		result := make(uidSet)
		for uid := range uids {
			node, ok := db.nodes[uid]
			if !ok {
				continue
			}
			targets, ok := node.edges[pred]
			if !ok {
				continue
			}
			for _, targetUID := range targets {
				if allowed[targetUID] {
					result[uid] = true
					break
				}
			}
		}
		return result
	case "type":
		if len(f.Func.Args) == 0 {
			return uids
		}
		typName := f.Func.Args[0].Value
		result := make(uidSet)
		for uid := range uids {
			if n, ok := db.nodes[uid]; ok && n.typ == typName {
				result[uid] = true
			}
		}
		return result
	}
	return uids
}

// cascadeMatches returns true if node uid satisfies all children in the DQL
// @cascade block.  Each child is matched by checking:
//  1. The node has at least one edge for child.Attr.
//  2. At least one target of that edge passes child.Filter (if set).
//  3. At least one target of that edge recursively passes child.Children (if
//     set) — enabling multi-level forward-edge @cascade blocks like:
//     { inGroup { Group.inUsers @filter(...) } }
func (db *fixtureDB) cascadeMatches(uid uint64, children []*dql.GraphQuery) bool {
	node, ok := db.nodes[uid]
	if !ok {
		return false
	}
	for _, child := range children {
		targets, ok := node.edges[child.Attr]
		if !ok || len(targets) == 0 {
			// No edges for this predicate → @cascade fails.
			return false
		}
		// At least one target must pass both direct filter and nested children.
		found := false
		for _, targetUID := range targets {
			if !db.nodeMatchesFilter(targetUID, child.Filter) {
				continue
			}
			if len(child.Children) > 0 && !db.cascadeMatches(targetUID, child.Children) {
				continue
			}
			found = true
			break
		}
		if !found {
			return false
		}
	}
	return true
}

// nodeMatchesFilter checks whether a node UID satisfies a FilterTree that
// references scalar predicates (e.g. eq(User.email, "alice")).
func (db *fixtureDB) nodeMatchesFilter(uid uint64, f *dql.FilterTree) bool {
	if f == nil {
		return true
	}
	node, ok := db.nodes[uid]
	if !ok {
		return false
	}
	switch f.Op {
	case "and":
		for _, child := range f.Child {
			if !db.nodeMatchesFilter(uid, child) {
				return false
			}
		}
		return true
	case "or":
		for _, child := range f.Child {
			if db.nodeMatchesFilter(uid, child) {
				return true
			}
		}
		return false
	case "not":
		if len(f.Child) == 0 {
			return true
		}
		return !db.nodeMatchesFilter(uid, f.Child[0])
	}
	if f.Func == nil {
		return true
	}
	switch f.Func.Name {
	case "eq":
		// In DQL's parsed Function: Attr = predicate name, Args[0] = value to compare.
		// e.g. eq(User.email, "alice@example.com") → Attr="User.email", Args[0]="alice@example.com"
		if len(f.Func.Args) < 1 || f.Func.Attr == "" {
			return false
		}
		pred := f.Func.Attr
		wantVal := f.Func.Args[0].Value
		if got, ok := node.scalars[pred]; ok {
			return got == wantVal
		}
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Main result evaluation — given the populated vars map, evaluate the root
// query block and return the UIDs of matching entities.
// ---------------------------------------------------------------------------

func (db *fixtureDB) evalRootQuery(parsedQuery *dql.Result, vars map[string]uidSet) ([]uint64, error) {
	for _, q := range parsedQuery.Query {
		// Skip var blocks (same detection used in evalVarsOnce).
		isVarBlock := q.Var != ""
		if !isVarBlock {
			for _, child := range q.Children {
				if child.Var != "" {
					isVarBlock = true
					break
				}
			}
		}
		if isVarBlock {
			continue
		}
		// q is the main query block (e.g. "queryGroup(func: uid(GroupRoot))").
		rootUIDs, err := db.evalRootFunc(q, vars)
		if err != nil {
			return nil, err
		}
		var result []uint64
		for uid := range rootUIDs {
			result = append(result, uid)
		}
		sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
		return result, nil
	}
	return nil, nil
}

// copySet returns a shallow copy of a uidSet.
func copySet(s uidSet) uidSet {
	out := make(uidSet, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// Helper: resolveWithFixture
// Runs the full GQL→DQL rewrite pipeline, then evaluates the generated DQL
// in-memory via the fixtureDB. Returns the names of the matching entities,
// sorted for deterministic comparison.
//
// NOTE: We bypass the GQL resolver pipeline intentionally here. The purpose
// of these tests is to verify two things:
//  1. The generated DQL is structurally correct (parses without errors).
//  2. The DQL semantics are correct: evaluated against example data, only the
//     right entities are visible.
//
// The full resolver pipeline adds response-shaping concerns (null completion,
// error wrapping, field ordering) orthogonal to auth correctness.
// ---------------------------------------------------------------------------

func resolveWithFixture(
	t *testing.T,
	gqlSchema schema.Schema,
	metaInfo *testutil.AuthMeta,
	jwtVars map[string]interface{},
	gqlSrc string,
	db *fixtureDB,
	namePred string,
) []string {
	t.Helper()

	op, err := gqlSchema.Operation(&schema.Request{Query: gqlSrc})
	require.NoError(t, err)
	gqlQuery := test.GetQuery(t, op)

	metaInfo.AuthVars = jwtVars
	ctx, err := metaInfo.AddClaimsToContext(context.Background())
	require.NoError(t, err)

	rewriter := NewQueryRewriter()
	dgQuery, err := rewriter.Rewrite(ctx, gqlQuery)
	require.NoError(t, err)

	actual := dgraph.AsString(dgQuery)
	t.Logf("Generated DQL:\n%s", actual)

	// 1. Validate the DQL parses cleanly (no unused-variable errors).
	parsed, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "DQL should parse without unused-variable errors")

	// 2. Evaluate the variable blocks against the fixture.
	vars, evalErr := db.evalVars(&parsed)
	require.NoError(t, evalErr, "fixture var evaluation should not error")
	t.Logf("Evaluated vars: %v", vars)

	// 3. Evaluate the root query block to get the matching UID set.
	uids, rootErr := db.evalRootQuery(&parsed, vars)
	require.NoError(t, rootErr, "fixture root query evaluation should not error")
	t.Logf("Matching UIDs: %v", uids)

	// 4. Convert UIDs to entity names via the fixture store.
	var names []string
	for _, uid := range uids {
		node := db.nodes[uid]
		if node == nil {
			continue
		}
		if namePred != "" {
			if name, ok := node.scalars[namePred]; ok && name != "" {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// ---------------------------------------------------------------------------
// baseTwoLevelFixture — shared fixture for TwoLevel schema tests
//
// Nodes:
//   User    uid=1   email=alice@example.com
//   User    uid=2   email=bob@example.com
//   Workspace uid=10  name=WsA  inUsers=[alice]  hasGroups=[GrpA, GrpC]
//   Workspace uid=11  name=WsB  inUsers=[bob]    hasGroups=[GrpB]
//   Group   uid=20  name=GrpA  inUsers=[alice]   inWorkspace=WsA
//   Group   uid=21  name=GrpB  inUsers=[bob]     inWorkspace=WsB
//   Group   uid=22  name=GrpC  inUsers=[]        inWorkspace=WsA  (no user member)
// ---------------------------------------------------------------------------

func baseTwoLevelFixture() *fixtureDB {
	db := newFixtureDB()

	// Users
	db.addNode(1, "User").setScalar("User.email", "alice@example.com")
	db.addNode(2, "User").setScalar("User.email", "bob@example.com")

	// Workspaces (store @hasInverse edges manually)
	db.addNode(10, "Workspace").
		setScalar("Workspace.name", "WsA").
		addEdge("Workspace.inUsers", 1).       // alice
		addEdge("Workspace.hasGroups", 20, 22) // GrpA, GrpC

	db.addNode(11, "Workspace").
		setScalar("Workspace.name", "WsB").
		addEdge("Workspace.inUsers", 2).   // bob
		addEdge("Workspace.hasGroups", 21) // GrpB

	// Groups
	db.addNode(20, "Group").
		setScalar("Group.name", "GrpA").
		addEdge("Group.inUsers", 1).               // alice
		addEdge("WorkspaceMember.inWorkspace", 10) // WsA — forward Dgraph pred

	db.addNode(21, "Group").
		setScalar("Group.name", "GrpB").
		addEdge("Group.inUsers", 2).               // bob
		addEdge("WorkspaceMember.inWorkspace", 11) // WsB

	db.addNode(22, "Group").
		setScalar("Group.name", "GrpC").
		// no inUsers — nobody is a member
		addEdge("WorkspaceMember.inWorkspace", 10) // WsA (still in Alice's workspace)

	return db
}

// ---------------------------------------------------------------------------
// baseThreeLevelFixture — extends baseTwoLevelFixture with Company nodes
//
//   Company uid=30  name=CompA  inGroup=GrpA  (Alice's group, Alice's workspace)
//   Company uid=31  name=CompB  inGroup=GrpB  (Bob's group, Bob's workspace)
//   Company uid=32  name=CompC  inGroup=GrpC  (no-member group, Alice's workspace)
// ---------------------------------------------------------------------------

func baseThreeLevelFixture() *fixtureDB {
	db := baseTwoLevelFixture()

	// Add @hasInverse back-edges from Group to Companies
	db.nodes[20].addEdge("Group.hasCompanies", 30)
	db.nodes[21].addEdge("Group.hasCompanies", 31)
	db.nodes[22].addEdge("Group.hasCompanies", 32)

	db.addNode(30, "Company").
		setScalar("Company.name", "CompA").
		addEdge("Company.inGroup", 20).
		addEdge("GroupMember.inGroup", 20) // interface pred alias

	db.addNode(31, "Company").
		setScalar("Company.name", "CompB").
		addEdge("Company.inGroup", 21).
		addEdge("GroupMember.inGroup", 21)

	db.addNode(32, "Company").
		setScalar("Company.name", "CompC").
		addEdge("Company.inGroup", 22).
		addEdge("GroupMember.inGroup", 22)

	return db
}

// ---------------------------------------------------------------------------
// TestCascadeAuthExec_TwoLevel_AND_AliceSeesOnlyHerGroup
//
// AND policy (default): Group is visible if BOTH Group.inUsers contains the
// user AND the Group's Workspace is authorized for that user.
//
//	Alice → GrpA  VISIBLE   (Alice is a member AND WsA is hers)
//	Alice → GrpB  HIDDEN    (Alice not a member AND WsB not hers)
//	Alice → GrpC  HIDDEN    (Alice not a member — AND requires both guards)
//
// ---------------------------------------------------------------------------
func TestCascadeAuthExec_TwoLevel_AND_AliceSeesOnlyHerGroup(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, cascadeAuthBaseSchema)
	db := baseTwoLevelFixture()

	got := resolveWithFixture(t, gqlSchema, metaInfo,
		map[string]interface{}{"EMAIL": "alice@example.com"},
		`query { queryGroup { name } }`,
		db, "Group.name",
	)
	require.Equal(t, []string{"GrpA"}, got,
		"Alice should only see GrpA (she is a member AND WsA is her workspace)")
}

// ---------------------------------------------------------------------------
// TestCascadeAuthExec_TwoLevel_AND_BobSeesOnlyHisGroup
// ---------------------------------------------------------------------------
func TestCascadeAuthExec_TwoLevel_AND_BobSeesOnlyHisGroup(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, cascadeAuthBaseSchema)
	db := baseTwoLevelFixture()

	got := resolveWithFixture(t, gqlSchema, metaInfo,
		map[string]interface{}{"EMAIL": "bob@example.com"},
		`query { queryGroup { name } }`,
		db, "Group.name",
	)
	require.Equal(t, []string{"GrpB"}, got,
		"Bob should only see GrpB (he is a member AND WsB is his workspace)")
}

// ---------------------------------------------------------------------------
// TestCascadeAuthExec_TwoLevel_AND_WorkspaceOnlyMemberHidden
//
// GrpC is in WsA (Alice's workspace) but has no members.
// Under AND policy Alice cannot see it — Group.inUsers guard fails.
// ---------------------------------------------------------------------------
func TestCascadeAuthExec_TwoLevel_AND_WorkspaceOnlyMemberHidden(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, cascadeAuthBaseSchema)
	db := baseTwoLevelFixture()

	got := resolveWithFixture(t, gqlSchema, metaInfo,
		map[string]interface{}{"EMAIL": "alice@example.com"},
		`query { queryGroup { name } }`,
		db, "Group.name",
	)
	require.NotContains(t, got, "GrpC",
		"GrpC should be hidden: Alice is not a member (AND policy requires both guards)")
}

// ---------------------------------------------------------------------------
// TestCascadeAuthExec_TwoLevel_OR_AliceSeesMemberAndWorkspaceGroups
//
// OR policy: Group is visible if EITHER the user is a member OR the
// Group's Workspace is authorized.
//
//	Alice → GrpA  VISIBLE  (member AND workspace — passes both)
//	Alice → GrpC  VISIBLE  (not a member but WsA is hers — workspace guard passes)
//	Alice → GrpB  HIDDEN   (neither Alice is a member nor is WsB hers)
//
// ---------------------------------------------------------------------------
func TestCascadeAuthExec_TwoLevel_OR_AliceSeesMemberAndWorkspaceGroups(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, cascadeAuthORPolicySchema)
	db := baseTwoLevelFixture()

	got := resolveWithFixture(t, gqlSchema, metaInfo,
		map[string]interface{}{"EMAIL": "alice@example.com"},
		`query { queryGroup { name } }`,
		db, "Group.name",
	)
	require.Equal(t, []string{"GrpA", "GrpC"}, got,
		"Under OR: Alice sees GrpA (member) and GrpC (in her workspace)")
}

// ---------------------------------------------------------------------------
// TestCascadeAuthExec_TwoLevel_OR_BobSeesOnlyHisGroup
// Under OR: Bob only sees GrpB (his group). GrpA and GrpC are in Alice's
// workspace; Bob is not a member of either.
// ---------------------------------------------------------------------------
func TestCascadeAuthExec_TwoLevel_OR_BobSeesOnlyHisGroup(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, cascadeAuthORPolicySchema)
	db := baseTwoLevelFixture()

	got := resolveWithFixture(t, gqlSchema, metaInfo,
		map[string]interface{}{"EMAIL": "bob@example.com"},
		`query { queryGroup { name } }`,
		db, "Group.name",
	)
	require.Equal(t, []string{"GrpB"}, got,
		"Under OR: Bob sees only GrpB (member of GrpB and only WsB is his)")
}

// ---------------------------------------------------------------------------
// TestCascadeAuthExec_ThreeLevel_AND_AliceSeesCompA
//
// Three-level chain: Company → Group → Workspace.
// AND policy (default for base schema):
//
//	CompA  VISIBLE  (GrpA has Alice AND WsA is Alice's)
//	CompB  HIDDEN   (GrpB has Bob, not Alice)
//	CompC  HIDDEN   (GrpC has no member — Group.inUsers guard fails)
//
// ---------------------------------------------------------------------------
func TestCascadeAuthExec_ThreeLevel_AND_AliceSeesCompA(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, cascadeAuthBaseSchema)
	db := baseThreeLevelFixture()

	got := resolveWithFixture(t, gqlSchema, metaInfo,
		map[string]interface{}{"EMAIL": "alice@example.com"},
		`query { queryCompany { name } }`,
		db, "Company.name",
	)
	require.Equal(t, []string{"CompA"}, got,
		"Alice should see only CompA through GrpA→WsA auth chain")
}

// ---------------------------------------------------------------------------
// TestCascadeAuthExec_ThreeLevel_AND_BobSeesCompB
// ---------------------------------------------------------------------------
func TestCascadeAuthExec_ThreeLevel_AND_BobSeesCompB(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, cascadeAuthBaseSchema)
	db := baseThreeLevelFixture()

	got := resolveWithFixture(t, gqlSchema, metaInfo,
		map[string]interface{}{"EMAIL": "bob@example.com"},
		`query { queryCompany { name } }`,
		db, "Company.name",
	)
	require.Equal(t, []string{"CompB"}, got,
		"Bob should see only CompB through GrpB→WsB auth chain")
}

// ---------------------------------------------------------------------------
// TestCascadeAuthExec_ThreeLevel_AND_UnauthorizedEmailSeesNothing
// A user with no matching email sees no entities.
// ---------------------------------------------------------------------------
func TestCascadeAuthExec_ThreeLevel_AND_UnauthorizedEmailSeesNothing(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, cascadeAuthBaseSchema)
	db := baseThreeLevelFixture()

	got := resolveWithFixture(t, gqlSchema, metaInfo,
		map[string]interface{}{"EMAIL": "nobody@example.com"},
		`query { queryCompany { name } }`,
		db, "Company.name",
	)
	require.Empty(t, got, "Unknown user should see no companies")
}

// ---------------------------------------------------------------------------
// Variable-handling exec tests
//
// These tests verify that JWT variable substitution in the authority's @auth
// rule is semantically correct when the rule is propagated as a cascade leaf.
// We reuse the base two-level fixture (Group → Workspace) and the multi-var
// fixture (Group → Workspace with $EMAIL + $DOMAIN guard) to validate that:
//   - a missing JWT variable causes no entities to be visible (deny-all)
//   - multiple variables are all required to be satisfied simultaneously
//   - only nodes satisfying every variable filter are returned
// ---------------------------------------------------------------------------

// multiVarFixture extends baseTwoLevelFixture so User nodes also carry a
// "User.domain" scalar value, enabling the $DOMAIN filter in the Workspace rule.
func multiVarFixture() *fixtureDB {
	db := baseTwoLevelFixture()
	// alice is in example.com, bob is in other.com
	db.nodes[1].setScalar("User.domain", "example.com")
	db.nodes[2].setScalar("User.domain", "other.com")
	return db
}

// ---------------------------------------------------------------------------
// TestCascadeAuthExec_Variable_MissingJWTVar_SeesNothing
//
// Schema: cascadeAuthBaseSchema (GROUP auth uses $EMAIL, Workspace cascade also).
// JWT:    empty — no variables.
//
// Expected: empty result. With no JWT variables the closed-by-default rewriter
// collapses the query — no entity should be visible.
// ---------------------------------------------------------------------------
func TestCascadeAuthExec_Variable_MissingJWTVar_SeesNothing(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, cascadeAuthBaseSchema)
	db := baseTwoLevelFixture()

	got := resolveWithFixture(t, gqlSchema, metaInfo,
		map[string]interface{}{}, // empty JWT
		`query { queryGroup { name } }`,
		db, "Group.name",
	)
	require.Empty(t, got,
		"Empty JWT should make all groups invisible (closed-by-default)")
}

// ---------------------------------------------------------------------------
// TestCascadeAuthExec_Variable_MultiVar_BothMatch
//
// Schema: cascadeAuthMultiVarSchema (Workspace guard requires $EMAIL AND $DOMAIN).
// JWT:    EMAIL=alice@example.com, DOMAIN=example.com
//
// Fixture: alice belongs to GrpA (WsA); alice's User node has domain=example.com.
//
//	GrpA  VISIBLE   (alice is a member AND WsA has alice with matching domain)
//	GrpB  HIDDEN    (bob is a member but DOMAIN=other.com ≠ example.com)
//	GrpC  HIDDEN    (no members)
//
// ---------------------------------------------------------------------------
func TestCascadeAuthExec_Variable_MultiVar_BothMatch(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, cascadeAuthMultiVarSchema)
	db := multiVarFixture()

	got := resolveWithFixture(t, gqlSchema, metaInfo,
		map[string]interface{}{
			"EMAIL":  "alice@example.com",
			"DOMAIN": "example.com",
		},
		`query { queryGroup { name } }`,
		db, "Group.name",
	)
	require.Equal(t, []string{"GrpA"}, got,
		"With both EMAIL and DOMAIN matching alice, only GrpA should be visible")
}

// ---------------------------------------------------------------------------
// TestCascadeAuthExec_Variable_MultiVar_WrongDomain_SeesNothing
//
// Schema: cascadeAuthMultiVarSchema.
// JWT:    EMAIL=alice@example.com, DOMAIN=wrong.com
//
// Alice is a member of GrpA but her User.domain is "example.com", not "wrong.com".
// The workspace cascade guard requires BOTH email AND domain to match → no group
// passes the compound filter.
// ---------------------------------------------------------------------------
func TestCascadeAuthExec_Variable_MultiVar_WrongDomain_SeesNothing(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, cascadeAuthMultiVarSchema)
	db := multiVarFixture()

	got := resolveWithFixture(t, gqlSchema, metaInfo,
		map[string]interface{}{
			"EMAIL":  "alice@example.com",
			"DOMAIN": "wrong.com", // mismatch — alice's domain is example.com
		},
		`query { queryGroup { name } }`,
		db, "Group.name",
	)
	require.Empty(t, got,
		"Workspace cascade guard requires matching DOMAIN — wrong domain should hide all groups")
}

// ---------------------------------------------------------------------------
// TestCascadeAuthExec_Variable_MultiVar_PartialJWT_SeesNothing
//
// Schema: cascadeAuthMultiVarSchema (authority declares $DOMAIN as String!).
// JWT:    only EMAIL present — DOMAIN absent.
//
// The closed-by-default logic collapses the query when a required variable
// ($DOMAIN String!) is missing from the JWT → no entity is visible.
// ---------------------------------------------------------------------------
func TestCascadeAuthExec_Variable_MultiVar_PartialJWT_SeesNothing(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, cascadeAuthMultiVarSchema)
	db := multiVarFixture()

	got := resolveWithFixture(t, gqlSchema, metaInfo,
		map[string]interface{}{"EMAIL": "alice@example.com"}, // DOMAIN absent
		`query { queryGroup { name } }`,
		db, "Group.name",
	)
	require.Empty(t, got,
		"Missing required $DOMAIN variable should hide all entities (closed-by-default)")
}
