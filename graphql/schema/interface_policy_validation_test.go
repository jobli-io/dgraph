/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

// Tests for validateInterfacePolicy — the typeValidation that enforces
// well-formedness of @auth(interfacePolicy: [...]) on concrete types and
// @auth(mergeInto: ...) on interface types.
//
// Rules covered:
//  1. mergeInto on an INTERFACE must be "and" or "or".
//  2. interfacePolicy on an INTERFACE type is rejected.
//  3. Each interfacePolicy entry's merge must be "and" or "or".
//  4. Each interfacePolicy entry's interface must name a real interface in schema.
//  5. The concrete type must implement the referenced interface.
//  6. No interface may appear more than once in the list.

import "testing"

// ─── Shared fixture SDL ───────────────────────────────────────────────────────

// minimalIface is a minimal interface with @auth so mergeInto has something to
// apply to.  Concrete types can implement it to test interfacePolicy entries.
const minimalIface = `
interface IProtected
  @auth(
    mergeInto: "and"
    query: { rule: "{ $scope: { eq: \"admin\" } }" }
  )
{
  name: String
}
`

// minimalMember is a second interface used to test multiple-interface scenarios.
const minimalMember = `
interface IMember
  @auth(
    mergeInto: "and"
    query: { rule: "{ $scope: { eq: \"member\" } }" }
  )
{
  role: String
}
`

// concreteBase builds a concrete type that implements IProtected (and optionally
// IMember) plus a minimal Workspace required so @cascadeAuth has a valid anchor.
func concreteWith(interfacePolicyArg string, implements ...string) string {
	implStr := "IProtected"
	if len(implements) > 0 {
		implStr = ""
		for i, s := range implements {
			if i > 0 {
				implStr += " & "
			}
			implStr += s
		}
	}
	return minimalIface + minimalMember + `
type Widget implements ` + implStr + `
  @auth(interfacePolicy: [` + interfacePolicyArg + `])
{
  id: ID!
  name: String
  role: String
}
`
}

// ─── Rule 1: mergeInto on INTERFACE must be "and" or "or" ────────────────────

func TestValidateInterfacePolicy_MergeInto_Valid_And(t *testing.T) {
	buildSchemaOK(t, `
interface IFoo @auth(mergeInto: "and" query: { rule: "{ $x: { eq: \"y\" } }" }) {
  id: ID!
  name: String
}
type Bar implements IFoo { id: ID! name: String }
`)
}

func TestValidateInterfacePolicy_MergeInto_Valid_Or(t *testing.T) {
	buildSchemaOK(t, `
interface IFoo @auth(mergeInto: "or" query: { rule: "{ $x: { eq: \"y\" } }" }) {
  id: ID!
  name: String
}
type Bar implements IFoo { id: ID! name: String }
`)
}

func TestValidateInterfacePolicy_MergeInto_Invalid_Value(t *testing.T) {
	buildSchemaErr(t, `
interface IFoo @auth(mergeInto: "OR" query: { rule: "{ $x: { eq: \"y\" } }" }) {
  id: ID!
  name: String
}
type Bar implements IFoo { id: ID! name: String }
`,
		"IFoo", `mergeInto: "OR"`, `"and" or "or"`,
	)
}

func TestValidateInterfacePolicy_MergeInto_Invalid_Typo(t *testing.T) {
	buildSchemaErr(t, `
interface IFoo @auth(mergeInto: "ror" query: { rule: "{ $x: { eq: \"y\" } }" }) {
  id: ID!
  name: String
}
type Bar implements IFoo { id: ID! name: String }
`,
		"IFoo", "ror",
	)
}

// ─── Rule 2: interfacePolicy on INTERFACE type is rejected ────────────────────

func TestValidateInterfacePolicy_OnInterface_IsRejected(t *testing.T) {
	buildSchemaErr(t, `
interface IFoo
  @auth(
    interfacePolicy: [{ interface: "IBar", merge: "or" }]
    query: { rule: "{ $x: { eq: \"y\" } }" }
  )
{
  id: ID!
}
interface IBar @auth(query: { rule: "{ $x: { eq: \"y\" } }" }) {
  id: ID!
}
type Widget implements IFoo & IBar { id: ID! }
`,
		"IFoo", "interfacePolicy", "concrete object types",
	)
}

// ─── Rule 3: merge must be "and" or "or" in interfacePolicy ──────────────────

func TestValidateInterfacePolicy_Merge_Valid_Or(t *testing.T) {
	buildSchemaOK(t, concreteWith(`{ interface: "IProtected", merge: "or" }`))
}

func TestValidateInterfacePolicy_Merge_Valid_And(t *testing.T) {
	buildSchemaOK(t, concreteWith(`{ interface: "IProtected", merge: "and" }`))
}

func TestValidateInterfacePolicy_Merge_Invalid_Uppercase(t *testing.T) {
	buildSchemaErr(t,
		concreteWith(`{ interface: "IProtected", merge: "OR" }`),
		"Widget", "IProtected", "merge", `"and" or "or"`, "OR",
	)
}

func TestValidateInterfacePolicy_Merge_Invalid_Typo(t *testing.T) {
	buildSchemaErr(t,
		concreteWith(`{ interface: "IProtected", merge: "AND" }`),
		"Widget", "AND",
	)
}

func TestValidateInterfacePolicy_Merge_EmptyString(t *testing.T) {
	buildSchemaErr(t,
		concreteWith(`{ interface: "IProtected", merge: "" }`),
		"Widget", "IProtected",
	)
}

// ─── Rule 4: referenced interface must exist in the schema ───────────────────

func TestValidateInterfacePolicy_Interface_DoesNotExist(t *testing.T) {
	buildSchemaErr(t, minimalIface+`
type Widget implements IProtected
  @auth(interfacePolicy: [{ interface: "Nonexistent", merge: "or" }])
{
  id: ID!
  name: String
}
`,
		"Widget", "Nonexistent", "not a defined interface type",
	)
}

func TestValidateInterfacePolicy_Interface_NamesConcreteType_NotInterface(t *testing.T) {
	buildSchemaErr(t, minimalIface+`
type Other { value: String }
type Widget implements IProtected
  @auth(interfacePolicy: [{ interface: "Other", merge: "or" }])
{
  id: ID!
  name: String
}
`,
		"Widget", "Other", "not a defined interface type",
	)
}

// ─── Rule 5: concrete type must implement the referenced interface ─────────────

func TestValidateInterfacePolicy_Interface_NotImplementedByType(t *testing.T) {
	buildSchemaErr(t, minimalIface+minimalMember+`
type Widget implements IProtected
  @auth(interfacePolicy: [{ interface: "IMember", merge: "or" }])
{
  id: ID!
  name: String
}
`,
		"Widget", "IMember", "does not implement",
	)
}

func TestValidateInterfacePolicy_Interface_Implemented_Passes(t *testing.T) {
	buildSchemaOK(t, concreteWith(
		`{ interface: "IProtected", merge: "or" } { interface: "IMember", merge: "and" }`,
		"IProtected", "IMember",
	))
}

// ─── Rule 6: no duplicate interface entries ───────────────────────────────────

func TestValidateInterfacePolicy_Duplicate_Interface(t *testing.T) {
	buildSchemaErr(t,
		concreteWith(`{ interface: "IProtected", merge: "or" } { interface: "IProtected", merge: "and" }`),
		"Widget", "IProtected", "more than once",
	)
}

// ─── Happy-path: valid interfacePolicy with multiple entries ─────────────────

func TestValidateInterfacePolicy_MultipleInterfaces_AllValid(t *testing.T) {
	buildSchemaOK(t, concreteWith(
		`{ interface: "IProtected", merge: "or" } { interface: "IMember", merge: "and" }`,
		"IProtected", "IMember",
	))
}

// ─── Edge: empty interfacePolicy list is valid ────────────────────────────────

func TestValidateInterfacePolicy_EmptyList_Valid(t *testing.T) {
	buildSchemaOK(t, minimalIface+`
type Widget implements IProtected
  @auth(interfacePolicy: [])
{
  id: ID!
  name: String
}
`)
}

// ─── Edge: type with @auth but no interfacePolicy is valid ───────────────────

func TestValidateInterfacePolicy_NoInterfacePolicy_Valid(t *testing.T) {
	buildSchemaOK(t, minimalIface+`
type Widget implements IProtected
  @auth(query: { rule: "{ $scope: { eq: \"user\" } }" })
{
  id: ID!
  name: String
}
`)
}
