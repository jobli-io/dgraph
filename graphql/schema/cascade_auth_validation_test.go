/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

// Tests for @cascadeAuth directive validation — variableContext modes.
//
// These tests exercise cascadeAuthDirectiveValidation and the helpers it calls
// (chainHasAuthVariables, authVarKeysFromDef, collectUnresolvedKeys).
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

func TestCascadeAuthValidation_Parent_RequiresAuthVariablesOnImmediateAuthority(t *testing.T) {
	// Workspace has no @authVariables but parent requires it → schema error.
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
	buildSchemaErr(t, noVarsWorkspace,
		"parent",
		"@authVariables",
		"Workspace",
	)
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

func TestCascadeAuthValidation_Parent_SuggestsPropagateInErrorMessage(t *testing.T) {
	// Error message should suggest "propagate" as the alternative.
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
	buildSchemaErr(t, input, "propagate")
}

// ─────────────────────────────────────────────────────────────────────────────
// variableContext: "adaptive"
// ─────────────────────────────────────────────────────────────────────────────

func TestCascadeAuthValidation_Adaptive_ErrorsWhenNoAncestorHasAuthVariables(t *testing.T) {
	// Neither Group (child) nor Workspace (authority) has @authVariables.
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
// variableContext: "propagate"
// ─────────────────────────────────────────────────────────────────────────────

func TestCascadeAuthValidation_Propagate_ErrorsWhenNoAncestorHasAuthVariables(t *testing.T) {
	// Workspace (immediate authority) has no @authVariables and no further
	// ancestors → propagate must fail.
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
  inWorkspace: Workspace @cascadeAuth(variableContext: propagate)
}

type Group implements WorkspaceMember {
  name: String
}
`
	buildSchemaErr(t, input, "propagate", "@authVariables")
}

func TestCascadeAuthValidation_Propagate_PassesWhenImmediateAuthorityHasAuthVariables(t *testing.T) {
	// Workspace (immediate authority) has @authVariables → propagate satisfied.
	const input = baseWorkspaceType + `
interface WorkspaceMember {
  inWorkspace: Workspace @cascadeAuth(variableContext: propagate)
}

type Group implements WorkspaceMember {
  name: String
}
`
	buildSchemaOK(t, input)
}

func TestCascadeAuthValidation_Propagate_PassesWhenGrandparentHasAuthVariables(t *testing.T) {
	// A→B (propagate), B has no vars, C has vars → propagate finds C → passes.
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
  inWorkspace: Workspace @cascadeAuth(variableContext: propagate)
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
  inGroup: Group @cascadeAuth(variableContext: propagate)
}

type Ad implements GroupMember {
  name: String
}
`
	// Group has no vars, but Workspace does → propagate on Ad→Group walks to Workspace → passes.
	buildSchemaOK(t, input)
}

func TestCascadeAuthValidation_Propagate_ErrorsWhenChainHasNoVarsAtAll(t *testing.T) {
	// A→B→C, none has @authVariables → propagate on A→B fails.
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
  inWorkspace: Workspace @cascadeAuth(variableContext: propagate)
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
  inGroup: Group @cascadeAuth(variableContext: propagate)
}

type Ad implements GroupMember {
  name: String
}
`
	buildSchemaErr(t, input, "propagate", "@authVariables")
}

// ─────────────────────────────────────────────────────────────────────────────
// Multi-mode chain: propagate + self interaction (user scenario)
// A→B (propagate), B→C (propagate), C→D (self)
// C has no @authVariables → self on C→D fails.
// ─────────────────────────────────────────────────────────────────────────────

func TestCascadeAuthValidation_SelfOnCFailsWhenCHasNoVars_PropagateSatisfied(t *testing.T) {
	// A→B (propagate): B has no vars, D does → propagate finds D → passes.
	// B→C (propagate): C has no vars, D does → propagate finds D → passes.
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
  inC: CType @cascadeAuth(variableContext: propagate)
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
  inB: BType @cascadeAuth(variableContext: propagate)
}

type AType implements BMember {
  name: String
}
`
	// C→D is `self` and C (CType) has no @authVariables → schema error.
	buildSchemaErr(t, input, "self", "@authVariables")
}

func TestCascadeAuthValidation_SelfOnCPassesWhenCHasVars_PropagateChain(t *testing.T) {
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
  inC: CType @cascadeAuth(variableContext: propagate)
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
  inB: BType @cascadeAuth(variableContext: propagate)
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
