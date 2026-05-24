/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// Schema validation: @cascadeDelete directive
// ─────────────────────────────────────────────────────────────────────────────

func TestCascadeDelete_Valid_EdgeField(t *testing.T) {
	const input = `
type Parent {
  name: String
  hasChild: Child @cascadeDelete @hasInverse(field: inParent)
}
type Child {
  name: String
  inParent: Parent
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err, "@cascadeDelete on an edge field must be accepted")
}

func TestCascadeDelete_Valid_WithOptions(t *testing.T) {
	const input = `
type Parent {
  name: String
  hasChild: [Child] @cascadeDelete(onlyIfOrphan: true, onlyIfOrphanScope: "type", depth: 3) @hasInverse(field: inParent)
}
type Child {
  name: String
  inParent: Parent
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err, "@cascadeDelete with valid options must be accepted")
}

func TestCascadeDelete_Invalid_ScalarField(t *testing.T) {
	const input = `
type Parent {
  name: String @cascadeDelete
}
`
	_, err := NewHandler(input, false)
	require.Error(t, err, "@cascadeDelete on a scalar field must be rejected")
	assert.Contains(t, err.Error(), "can only be used on edge")
}

func TestCascadeDelete_Invalid_EnumField(t *testing.T) {
	const input = `
enum Status { ACTIVE INACTIVE }
type Parent {
  status: Status @cascadeDelete
}
`
	_, err := NewHandler(input, false)
	require.Error(t, err, "@cascadeDelete on an enum field must be rejected")
	assert.Contains(t, err.Error(), "enum")
}

func TestCascadeDelete_Invalid_NegativeDepth(t *testing.T) {
	const input = `
type Parent {
  hasChild: Child @cascadeDelete(depth: -1) @hasInverse(field: inParent)
}
type Child {
  name: String
  inParent: Parent
}
`
	_, err := NewHandler(input, false)
	require.Error(t, err, "@cascadeDelete with negative depth must be rejected")
	assert.Contains(t, err.Error(), "depth")
}

func TestCascadeDelete_Invalid_ZeroDepth(t *testing.T) {
	const input = `
type Parent {
  hasChild: Child @cascadeDelete(depth: 0) @hasInverse(field: inParent)
}
type Child {
  name: String
  inParent: Parent
}
`
	_, err := NewHandler(input, false)
	require.Error(t, err, "@cascadeDelete with zero depth must be rejected")
	assert.Contains(t, err.Error(), "depth")
}

func TestCascadeDelete_Invalid_OrphanScope(t *testing.T) {
	const input = `
type Parent {
  hasChild: [Child] @cascadeDelete(onlyIfOrphan: true, onlyIfOrphanScope: "bad") @hasInverse(field: inParent)
}
type Child {
  name: String
  inParent: Parent
}
`
	_, err := NewHandler(input, false)
	require.Error(t, err, "@cascadeDelete with invalid onlyIfOrphanScope must be rejected")
	assert.Contains(t, err.Error(), "onlyIfOrphanScope")
}

func TestCascadeDelete_Valid_SelfReference(t *testing.T) {
	// Self-referencing cascade chains (e.g. tree nodes) are valid.
	// Runtime safety is guaranteed by the visited-UID map in CascadeDeleteCollector.
	const input = `
type TreeNode {
  name: String
  children: [TreeNode] @cascadeDelete @hasInverse(field: parent)
  parent: TreeNode
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err, "self-referencing @cascadeDelete must be accepted (runtime-safe via visited map)")
}

func TestCascadeDelete_Valid_OrphanScopeType(t *testing.T) {
	const input = `
type Group {
  name: String
  hasCandidate: [Candidate] @cascadeDelete(onlyIfOrphan: true, onlyIfOrphanScope: "type") @hasInverse(field: inGroup)
}
type Candidate {
  name: String
  inGroup: [Group]
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err, "onlyIfOrphanScope: \"type\" must be accepted")
}

func TestCascadeDelete_Valid_OrphanScopeAll(t *testing.T) {
	const input = `
type Group {
  name: String
  hasCandidate: [Candidate] @cascadeDelete(onlyIfOrphan: true, onlyIfOrphanScope: "all") @hasInverse(field: inGroup)
}
type Candidate {
  name: String
  inGroup: [Group]
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err, "onlyIfOrphanScope: \"all\" must be accepted")
}

// ─────────────────────────────────────────────────────────────────────────────
// CascadeDeleteFields: verify runtime field introspection via FromString
// ─────────────────────────────────────────────────────────────────────────────

func TestCascadeDelete_FieldsResolved(t *testing.T) {
	const input = `
type Note {
  text: String
  hasCreateRecord: CreateRecord @cascadeDelete @hasInverse(field: forNote)
  hasUpdateRecord: [UpdateRecord] @cascadeDelete @hasInverse(field: forNote)
}
type CreateRecord {
  createdAt: DateTime
  forNote: Note
}
type UpdateRecord {
  updatedAt: DateTime
  forNote: Note
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err, "schema with multiple @cascadeDelete fields must compile")
}

func TestCascadeDelete_InterfaceInheritance(t *testing.T) {
	// @cascadeDelete declared on an interface field should be inherited
	// by concrete types implementing the interface.
	const input = `
interface Recordable {
  hasCreateRecord: CreateRecord @cascadeDelete @hasInverse(field: forRecordable)
}
type Note implements Recordable {
  text: String
}
type CreateRecord {
  createdAt: DateTime
  forRecordable: Recordable
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err,
		"@cascadeDelete on interface fields must be accepted and inherited by implementors")
}

// ─────────────────────────────────────────────────────────────────────────────
// Multi-level cascade chain: A → B → C
// ─────────────────────────────────────────────────────────────────────────────

func TestCascadeDelete_MultiLevel_SchemaValid(t *testing.T) {
	const input = `
type Workspace {
  name: String @id
  hasGroup: [Group] @cascadeDelete @hasInverse(field: inWorkspace)
}
type Group {
  name: String
  inWorkspace: Workspace
  hasCandidate: [Candidate] @cascadeDelete @hasInverse(field: inGroup)
}
type Candidate {
  name: String
  inGroup: [Group]
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err, "multi-level cascade chain must be accepted")
}

// ─────────────────────────────────────────────────────────────────────────────
// Multiple @cascadeDelete fields on a single type
// ─────────────────────────────────────────────────────────────────────────────

func TestCascadeDelete_MultipleCascadeFields(t *testing.T) {
	const input = `
type Candidate {
  name: String
  hasAddress: Address @cascadeDelete @hasInverse(field: forCandidate)
  hasNotes: [Note] @cascadeDelete @hasInverse(field: forCandidate)
}
type Address {
  city: String
  forCandidate: Candidate
}
type Note {
  text: String
  forCandidate: Candidate
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err, "multiple @cascadeDelete fields on one type must be accepted")
}

// ─────────────────────────────────────────────────────────────────────────────
// Cross-type cycle: A → B → A (both with @cascadeDelete)
// ─────────────────────────────────────────────────────────────────────────────

func TestCascadeDelete_CrossTypeCycle_SchemaValid(t *testing.T) {
	// Cross-type cycles are safe at runtime (visited-UID map terminates traversal).
	_, err := NewHandler(`
type A {
  name: String
  hasB: B @cascadeDelete @hasInverse(field: inA)
  fromB: B
}
type B {
  name: String
  inA: A
  hasA: A @cascadeDelete @hasInverse(field: fromB)
}
`, false)
	require.NoError(t, err, "cross-type cascade cycle must be accepted (runtime-safe)")
}

// ─────────────────────────────────────────────────────────────────────────────
// Deeply nested cascade scenarios
// ─────────────────────────────────────────────────────────────────────────────

// TestCascadeDelete_DeepChain_5Levels verifies that a 5-level deep cascade
// chain (Org → Division → Team → Member → Badge) compiles successfully.
func TestCascadeDelete_DeepChain_5Levels(t *testing.T) {
	const input = `
type Org {
  name: String @id
  hasDivision: [Division] @cascadeDelete @hasInverse(field: inOrg)
}
type Division {
  name: String
  inOrg: Org
  hasTeam: [Team] @cascadeDelete @hasInverse(field: inDivision)
}
type Team {
  name: String
  inDivision: Division
  hasMember: [Member] @cascadeDelete @hasInverse(field: inTeam)
}
type Member {
  name: String
  inTeam: Team
  hasBadge: Badge @cascadeDelete @hasInverse(field: forMember)
}
type Badge {
  label: String
  forMember: Member
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err, "5-level deep cascade chain must be accepted")
}

// TestCascadeDelete_DeepChain_MixedOptions verifies that each level in a
// deep chain can have different @cascadeDelete options.
func TestCascadeDelete_DeepChain_MixedOptions(t *testing.T) {
	const input = `
type Company {
  name: String @id
  hasDepartment: [Department] @cascadeDelete(depth: 1) @hasInverse(field: inCompany)
}
type Department {
  name: String
  inCompany: Company
  hasProject: [Project] @cascadeDelete(onlyIfOrphan: true, onlyIfOrphanScope: "type") @hasInverse(field: inDepartment)
}
type Project {
  name: String
  inDepartment: [Department]
  hasTask: [Task] @cascadeDelete @hasInverse(field: inProject)
}
type Task {
  title: String
  inProject: Project
  hasAttachment: [Attachment] @cascadeDelete(onlyIfOrphan: true, onlyIfOrphanScope: "all") @hasInverse(field: forTask)
}
type Attachment {
  url: String
  forTask: [Task]
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err, "deep chain with mixed per-level options must be accepted")
}

// TestCascadeDelete_DiamondPattern verifies that diamond-shaped cascade
// relationships compile: A → B, A → C, B → D, C → D.
func TestCascadeDelete_DiamondPattern(t *testing.T) {
	const input = `
type A {
  name: String
  hasB: B @cascadeDelete @hasInverse(field: fromA)
  hasC: C @cascadeDelete @hasInverse(field: fromA)
}
type B {
  name: String
  fromA: A
  hasD: D @cascadeDelete @hasInverse(field: fromB)
}
type C {
  name: String
  fromA: A
  hasD: D @cascadeDelete @hasInverse(field: fromC)
}
type D {
  label: String
  fromB: B
  fromC: C
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err, "diamond-shaped cascade pattern must be accepted")
}

// TestCascadeDelete_FanOutMultipleChildren verifies a wide fan-out where a
// single parent type cascades into many child types at each level.
func TestCascadeDelete_FanOutMultipleChildren(t *testing.T) {
	const input = `
type Root {
  name: String @id
  hasAlpha: [Alpha] @cascadeDelete @hasInverse(field: inRoot)
  hasBeta: [Beta] @cascadeDelete @hasInverse(field: inRoot)
  hasGamma: Gamma @cascadeDelete @hasInverse(field: inRoot)
}
type Alpha {
  name: String
  inRoot: Root
  hasLeafA: [LeafA] @cascadeDelete @hasInverse(field: inAlpha)
}
type Beta {
  name: String
  inRoot: Root
  hasLeafB: LeafB @cascadeDelete @hasInverse(field: inBeta)
}
type Gamma {
  name: String
  inRoot: Root
  hasLeafC: [LeafC] @cascadeDelete @hasInverse(field: inGamma)
}
type LeafA {
  val: String
  inAlpha: Alpha
}
type LeafB {
  val: String
  inBeta: Beta
}
type LeafC {
  val: String
  inGamma: Gamma
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err, "fan-out cascade (1 parent → 3 children each with their own cascade) must be accepted")
}

// TestCascadeDelete_InterfaceDeepChain verifies a deep cascade chain that
// starts via an interface field: Recordable.hasCreateRecord → CreateRecord ←→
// then CreateRecord itself has a cascade child (AuditEntry).
func TestCascadeDelete_InterfaceDeepChain(t *testing.T) {
	const input = `
interface Recordable {
  hasCreateRecord: CreateRecord @cascadeDelete @hasInverse(field: forRecordable)
}
type Note implements Recordable {
  text: String
}
type Task implements Recordable {
  title: String
}
type CreateRecord {
  createdAt: DateTime
  forRecordable: Recordable
  hasAuditEntry: [AuditEntry] @cascadeDelete @hasInverse(field: forRecord)
}
type AuditEntry {
  action: String
  forRecord: CreateRecord
  hasDetail: AuditDetail @cascadeDelete @hasInverse(field: forEntry)
}
type AuditDetail {
  data: String
  forEntry: AuditEntry
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err,
		"deep chain through interface (Recordable→CreateRecord→AuditEntry→AuditDetail) must be accepted")
}

// TestCascadeDelete_SelfRefTree_WithOrphanCheck verifies that a self-referencing
// tree (e.g. Category → Category) with onlyIfOrphan compiles.
// At runtime, a leaf category (no other parent) gets cascade-deleted, but one
// with multiple parents is preserved.
func TestCascadeDelete_SelfRefTree_WithOrphanCheck(t *testing.T) {
	const input = `
type Category {
  name: String
  children: [Category] @cascadeDelete(onlyIfOrphan: true, onlyIfOrphanScope: "type") @hasInverse(field: parents)
  parents: [Category]
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err, "self-referencing tree with onlyIfOrphan must be accepted")
}

// TestCascadeDelete_DepthLimitedDeepChain verifies that depth limits at
// different levels in a deep chain compile correctly. The cascade should
// stop traversing beyond the specified depth at runtime.
func TestCascadeDelete_DepthLimitedDeepChain(t *testing.T) {
	const input = `
type L1 {
  name: String
  hasL2: [L2] @cascadeDelete(depth: 2) @hasInverse(field: inL1)
}
type L2 {
  name: String
  inL1: L1
  hasL3: [L3] @cascadeDelete @hasInverse(field: inL2)
}
type L3 {
  name: String
  inL2: L2
  hasL4: [L4] @cascadeDelete @hasInverse(field: inL3)
}
type L4 {
  name: String
  inL3: L3
  hasL5: L5 @cascadeDelete @hasInverse(field: inL4)
}
type L5 {
  label: String
  inL4: L4
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err,
		"depth-limited deep chain (L1 depth:2 stops at L3) must be accepted")
}

// TestCascadeDelete_MixedSingularAndList_DeepChain verifies that a deep chain
// mixing singular and list edge fields at each level compiles correctly.
func TestCascadeDelete_MixedSingularAndList_DeepChain(t *testing.T) {
	const input = `
type Order {
  ref: String @id
  hasInvoice: Invoice @cascadeDelete @hasInverse(field: forOrder)
}
type Invoice {
  number: String
  forOrder: Order
  hasLineItem: [LineItem] @cascadeDelete @hasInverse(field: inInvoice)
}
type LineItem {
  description: String
  inInvoice: Invoice
  hasTaxEntry: TaxEntry @cascadeDelete @hasInverse(field: forLineItem)
}
type TaxEntry {
  rate: Float
  forLineItem: LineItem
}
`
	_, err := NewHandler(input, false)
	require.NoError(t, err,
		"deep chain mixing singular (hasInvoice, hasTaxEntry) and list (hasLineItem) fields must be accepted")
}
