/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

// Tests for @cascadeAuth directive validation — variableContext modes.
//
// These tests exercise cascadeAuthDirectiveValidation and the helpers it calls
// (authVarKeysFromDef, collectUnresolvedKeys).
// Each test calls NewHandler directly and asserts either success or a specific
// error message fragment.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildSchemaOK asserts that the schema compiles without errors.
func buildSchemaOK(t *testing.T, input string) {
	t.Helper()
	_, err := NewHandler(input, false)
	require.NoError(t, err)
}

// buildSchemaErr asserts that the schema fails to compile and that the error
// message contains all of the provided substrings.
func buildSchemaErr(t *testing.T, input string, wantFragments ...string) {
	t.Helper()
	_, err := NewHandler(input, false)
	require.Error(t, err, "expected schema compile error but got none")
	msg := err.Error()
	for _, frag := range wantFragments {
		assert.True(t, strings.Contains(msg, frag),
			"error message should contain %q\nfull error: %s", frag, msg)
	}
}

// baseWorkspaceType is a minimal Workspace type with @auth and @authVariables,
// reused across multiple tests.
const baseWorkspaceType = `
type Workspace
  @authVariables(vars: [{key: "PERMISSIONS", value: ["READ_WORKSPACE"]}])
  @auth(query: { rule: """
    query($sub: String!) {
      queryWorkspace(filter: {
        hasIAMBinding: { forUserMember: { userId: { eq: $sub } } }
      }) { __typename }
    }
  """ })
{
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}
`

// ─────────────────────────────────────────────────────────────────────────────
// variableContext: "self"
// ─────────────────────────────────────────────────────────────────────────────

func TestCascadeAuthValidation_Self_RequiresAuthVariablesOnChildType(t *testing.T) {
	// Group has no @authVariables but uses variableContext: self → schema error.
	const input = baseWorkspaceType + `
interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth(variableContext: self)
}

type Group implements WorkspaceMember {
  name: String
}
`
	buildSchemaErr(t, input,
		"variableContext",
		"self",
		"@authVariables",
	)
}

func TestCascadeAuthValidation_Self_PassesWhenChildHasAuthVariables(t *testing.T) {
	// Group declares @authVariables → self is valid.
	const input = baseWorkspaceType + `
interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth(variableContext: self)
}

type Group implements WorkspaceMember
  @authVariables(vars: [{key: "PERMISSIONS", value: ["READ_GROUP"]}])
{
  name: String
}
`
	buildSchemaOK(t, input)
}

func TestCascadeAuthValidation_Self_InterfaceHostExemptFromAuthVariablesCheck(t *testing.T) {
	// The @cascadeAuth is on an interface field — no @authVariables required on
	// the interface itself; concrete implementors supply them.
	const input = baseWorkspaceType + `
interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth(variableContext: self)
}

type Group implements WorkspaceMember
  @authVariables(vars: [{key: "PERMISSIONS", value: ["READ_GROUP"]}])
{
  name: String
}
`
	buildSchemaOK(t, input)
}

// ─────────────────────────────────────────────────────────────────────────────
// variableContext: "parent"
// ─────────────────────────────────────────────────────────────────────────────

func TestCascadeAuthValidation_Parent_PassesWhenAuthorityHasNoPlaceholdersAndNoAuthVars(t *testing.T) {
	// Workspace's @auth rule has no {{KEY}} placeholders — the rule is
	// self-contained. parent is valid even without @authVariables on Workspace.
	const noVarsWorkspace = `
type Workspace
  @auth(query: { rule: """
    query { queryWorkspace { __typename } }
  """ })
{
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth(variableContext: parent)
}

type Group implements WorkspaceMember {
  name: String
}
`
	buildSchemaOK(t, noVarsWorkspace)
}

func TestCascadeAuthValidation_Parent_PassesWhenImmediateAuthorityHasAuthVariables(t *testing.T) {
	// Workspace has @authVariables → parent is valid.
	const input = baseWorkspaceType + `
interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth(variableContext: parent)
}

type Group implements WorkspaceMember {
  name: String
}
`
	buildSchemaOK(t, input)
}

func TestCascadeAuthValidation_Parent_PassesWhenAuthorityRuleHasNoPlaceholders(t *testing.T) {
	// Workspace's @auth rule has no {{KEY}} placeholders — the rule is
	// self-contained. parent is valid even without @authVariables on Workspace.
	const input = `
type Workspace
  @auth(query: { rule: """
    query { queryWorkspace { __typename } }
  """ })
{
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth(variableContext: parent)
}

type Group implements WorkspaceMember {
  name: String
}
`
	buildSchemaOK(t, input)
}

func TestCascadeAuthValidation_Parent_ErrorsWhenAuthorityRuleHasUnresolvedPlaceholders(t *testing.T) {
	// Workspace's @auth rule contains <<PERMISSIONS>> — it requires substitution.
	// parent must fail because Workspace has no @authVariables to supply the value.
	const input = `
type Workspace
  @auth(query: { rule: """
    query($sub: String!) {
      queryWorkspace(filter: {
        hasIAMBinding: { forRole: { permission: { in: <<PERMISSIONS>> } } }
      }) { __typename }
    }
  """ })
{
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth(variableContext: parent)
}

type Group implements WorkspaceMember {
  name: String
}
`
	buildSchemaErr(t, input, "PERMISSIONS")
}

// ─────────────────────────────────────────────────────────────────────────────
// variableContext: "adaptive"
// ─────────────────────────────────────────────────────────────────────────────

func TestCascadeAuthValidation_Adaptive_ErrorsWhenNoAncestorHasAuthVariables(t *testing.T) {
	// Neither Group (child) nor Workspace (authority) has @authVariables.
	// Workspace's @auth rule has <<PERMISSIONS>> — substitution is needed.
	// adaptive must fail — no type in the chain supplies @authVariables.
	const input = `
type Workspace
  @auth(query: { rule: """
    query($sub: String!) {
      queryWorkspace(filter: {
        hasIAMBinding: { forRole: { permission: { in: <<PERMISSIONS>> } } }
      }) { __typename }
    }
  """ })
{
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth(variableContext: adaptive)
}

type Group implements WorkspaceMember {
  name: String
}
`
	buildSchemaErr(t, input, "adaptive", "@authVariables")
}

func TestCascadeAuthValidation_Adaptive_PassesWhenChildHasAuthVariables(t *testing.T) {
	// Group (child) has @authVariables — adaptive satisfied by the child itself.
	const input = `
type Workspace
  @auth(query: { rule: """
    query { queryWorkspace { __typename } }
  """ })
{
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth(variableContext: adaptive)
}

type Group implements WorkspaceMember
  @authVariables(vars: [{key: "PERMISSIONS", value: ["READ_GROUP"]}])
{
  name: String
}
`
	buildSchemaOK(t, input)
}

func TestCascadeAuthValidation_Adaptive_PassesWhenAuthorityHasAuthVariables(t *testing.T) {
	// Group (child) has NO @authVariables but Workspace (authority) does.
	// Adaptive walks up from the child and finds Workspace → passes.
	const input = baseWorkspaceType + `
interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth(variableContext: adaptive)
}

type Group implements WorkspaceMember {
  name: String
}
`
	buildSchemaOK(t, input)
}

func TestCascadeAuthValidation_Adaptive_PassesWhenGrandparentHasAuthVariables(t *testing.T) {
	// A→B (adaptive), B has no vars, C has vars → passes (C is in the chain).
	const input = `
type Workspace
  @authVariables(vars: [{key: "PERMISSIONS", value: ["READ_WORKSPACE"]}])
  @auth(query: { rule: """
    query { queryWorkspace { __typename } }
  """ })
{
  name: String
  hasGroups: [Group] @hasInverse(field: inWorkspace)
}

interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth(variableContext: adaptive)
}

type Group implements WorkspaceMember
  @auth(query: { rule: """
    query { queryGroup { __typename } }
  """ })
{
  name: String
  hasAds: [Ad] @hasInverse(field: inGroup)
}

interface GroupMember {
  inGroup: Group @cascadeAuth(variableContext: adaptive)
}

type Ad implements GroupMember {
  name: String
}
`
	// Ad has no vars, Group has no vars, but Workspace does → adaptive passes.
	buildSchemaOK(t, input)
}

// ─────────────────────────────────────────────────────────────────────────────
// Multi-mode chain: adaptive + self interaction (user scenario)
// A→B (adaptive), B→C (adaptive), C→D (self)
// C has no @authVariables → self on C→D fails.
// ─────────────────────────────────────────────────────────────────────────────

func TestCascadeAuthValidation_SelfOnCFailsWhenCHasNoVars(t *testing.T) {
	// A→B (adaptive), B→C (adaptive): both pass (no error required for adaptive).
	// C→D (self): C has no @authVariables → schema error (self requires C to have vars).
	const input = `
type DType
  @authVariables(vars: [{key: "PERMISSIONS", value: ["READ_D"]}])
  @auth(query: { rule: """
    query { queryDType { __typename } }
  """ })
{
  name: String
  hasCs: [CType] @hasInverse(field: inD)
}

interface DMember {
  inD: DType @cascadeAuth(variableContext: self)
}

type CType implements DMember
  @auth(query: { rule: """
    query { queryCType { __typename } }
  """ })
{
  name: String
  hasBs: [BType] @hasInverse(field: inC)
}

interface CMember {
  inC: CType @cascadeAuth(variableContext: adaptive)
}

type BType implements CMember
  @auth(query: { rule: """
    query { queryBType { __typename } }
  """ })
{
  name: String
  hasAs: [AType] @hasInverse(field: inB)
}

interface BMember {
  inB: BType @cascadeAuth(variableContext: adaptive)
}

type AType implements BMember {
  name: String
}
`
	// C→D is `self` and C (CType) has no @authVariables → schema error.
	buildSchemaErr(t, input, "self", "@authVariables")
}

func TestCascadeAuthValidation_SelfOnCPassesWhenCHasVars(t *testing.T) {
	// Same chain but C now has @authVariables → all edges pass.
	const input = `
type DType
  @authVariables(vars: [{key: "PERMISSIONS", value: ["READ_D"]}])
  @auth(query: { rule: """
    query { queryDType { __typename } }
  """ })
{
  name: String
  hasCs: [CType] @hasInverse(field: inD)
}

interface DMember {
  inD: DType @cascadeAuth(variableContext: self)
}

type CType implements DMember
  @authVariables(vars: [{key: "PERMISSIONS", value: ["READ_C"]}])
  @auth(query: { rule: """
    query { queryCType { __typename } }
  """ })
{
  name: String
  hasBs: [BType] @hasInverse(field: inC)
}

interface CMember {
  inC: CType @cascadeAuth(variableContext: adaptive)
}

type BType implements CMember
  @auth(query: { rule: """
    query { queryBType { __typename } }
  """ })
{
  name: String
  hasAs: [AType] @hasInverse(field: inB)
}

interface BMember {
  inB: BType @cascadeAuth(variableContext: adaptive)
}

type AType implements BMember {
  name: String
}
`
	buildSchemaOK(t, input)
}

// ─────────────────────────────────────────────────────────────────────────────
// Invalid variableContext value
// ─────────────────────────────────────────────────────────────────────────────

func TestCascadeAuthValidation_InvalidVariableContextValue(t *testing.T) {
	const input = baseWorkspaceType + `
interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth(variableContext: grandparent)
}

type Group implements WorkspaceMember {
  name: String
}
`
	buildSchemaErr(t, input, "variableContext")
}

// ─────────────────────────────────────────────────────────────────────────────
// Second-pass re-substitution: interface stub value=[] must not lock concrete
// types into deny-all for GQL or RBAC rules.
//
// Regression test for the bug where:
//   - Interface declares @authVariables with value: [] (stub)
//   - First pass parses the rule with in: [] → sets Rule/RBACRule (deny-all stub)
//   - Second pass (force=true) must re-substitute with concrete type's real values
// ─────────────────────────────────────────────────────────────────────────────

// TestSecondPassResubstitution_GQLRule verifies that a GQL rule on an interface
// using a <<KEY>> placeholder is correctly re-substituted with the concrete
// type's @authVariables in the second pass, even when the interface stub
// (value: []) causes the first pass to parse an in: [] rule and set rn.Rule.
func TestSecondPassResubstitution_GQLRule(t *testing.T) {
	// Interface carries the stub; concrete type overrides with real values.
	const input = `
interface IResource
  @authVariables(vars: [{ key: "QRY_PERMISSIONS", value: [] }])
  @auth(query: { rule: """
    query($sub: String!) {
      queryIResource(filter: {
        permissions: { in: <<QRY_PERMISSIONS>> }
      }) { __typename }
    }
  """ })
{
  id: ID!
  permissions: [String] @search(by: [hash])
}

type ConcreteResource implements IResource
  @authVariables(vars: [{ key: "QRY_PERMISSIONS", value: ["READ", "WRITE"] }])
{
  id: ID!
  permissions: [String] @search(by: [hash])
}
`
	// Schema must compile without error — concrete type's values override stub.
	buildSchemaOK(t, input)
}

// TestSecondPassResubstitution_RBACRule verifies that an RBAC rule using the
// toStrings|lower pipeline is correctly re-substituted with the concrete type's
// values when the interface stub (value: []) parses as in: [] in the first pass.
func TestSecondPassResubstitution_RBACRule(t *testing.T) {
	const input = `
interface IResource
  @authVariables(vars: [{ key: "QRY_PERMISSIONS", value: [] }])
  @auth(query: { rule: "{ $scope: { in: <<QRY_PERMISSIONS | toStrings | lower>> } }" })
{
  id: ID!
  name: String
}

type ConcreteResource implements IResource
  @authVariables(vars: [{ key: "QRY_PERMISSIONS", value: ["READ", "WRITE"] }])
{
  id: ID!
  name: String
}
`
	buildSchemaOK(t, input)
}

// ─────────────────────────────────────────────────────────────────────────────
// RBAC cascade re-substitution with variableContext: adaptive
//
// Regression test for parseRuleNodeFromTemplate returning (nil, nil) for RBAC
// rules, causing the cascade path to always use the authority's (Workspace's)
// pre-compiled RBAC operand regardless of the child type's @authVariables.
//
// With the fix, a concrete type's QRY_PERMISSIONS are correctly substituted
// into the cascaded RBAC rule (e.g. { $scope: { in: [...] } }) so that
// adaptive variableContext uses the child's values, not the authority's.
// ─────────────────────────────────────────────────────────────────────────────

func TestCascadeAdaptive_RBACResubstitution(t *testing.T) {
	// Authority (Workspace) has a parameterised RBAC scope rule.
	// Child (Resource) has different QRY_PERMISSIONS — with adaptive the
	// cascade must pick up the child's values, not Workspace's.
	const input = `
interface WorkspaceMember {
  inWorkspace: Workspace! @cascadeAuth(variableContext: adaptive)
}

type Workspace
  @authVariables(vars: [{ key: "QRY_PERMISSIONS", value: ["_all", "_workspace", "read", "read_workspace"] }])
  @auth(query: { and: [
    { rule: """
      query($sub: String!) {
        queryWorkspace(filter: { name: { eq: $sub } }) { __typename }
      }
    """ }
    { rule: "{ $scope: { in: <<QRY_PERMISSIONS>> } }" }
  ] })
{
  name: String
  hasResource: [Resource] @hasInverse(field: inWorkspace)
}

type Resource implements WorkspaceMember
  @authVariables(vars: [{ key: "QRY_PERMISSIONS", value: ["_all", "_resource", "read_resource"] }])
{
  id: ID!
  name: String
}
`
	// Schema must compile: the cascaded Workspace RBAC rule is re-substituted
	// with Resource's QRY_PERMISSIONS, not Workspace's.
	buildSchemaOK(t, input)
}
