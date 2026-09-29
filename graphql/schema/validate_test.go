/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"testing"

	"github.com/hypermodeinc/dgraph/v25/x"
	"github.com/stretchr/testify/require"
)

func TestValidate_ArrayOfRules_Add(t *testing.T) {
	const gqlSchema = `
		type Product {
			id: ID!
			code: String @validate(
				add: [
					{ rule: "required", reason: "Product code is required" },
					{ rule: "min=3", reason: "Product code must be at least 3 characters" },
					{ expr: "!(value contains ' ')", reason: "Product code cannot contain spaces" }
				]
			)
		}
	`

	handler, err := NewHandler(gqlSchema, false)
	require.NoError(t, err, "NewHandler failed")

	sch, err := FromString(handler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err, "FromString failed")

	s, ok := sch.(*schema)
	require.True(t, ok)

	prodDef := s.schema.Types["Product"]
	require.NotNil(t, prodDef)

	codeFd := prodDef.Fields.ForName("code")
	require.NotNil(t, codeFd)

	auth := AuthCtx{}

	t.Run("valid value passes all add rules", func(t *testing.T) {
		parent := map[string]interface{}{"code": "PROD1"}
		errs := validateValue(s.schema, codeFd, "add", "Product", parent, auth, nil, nil)
		require.Empty(t, errs)
	})

	t.Run("violating min rule produces specific reason", func(t *testing.T) {
		parent := map[string]interface{}{"code": "AB"}
		errs := validateValue(s.schema, codeFd, "add", "Product", parent, auth, nil, nil)
		require.Len(t, errs, 1)
		require.Equal(t, "Product code must be at least 3 characters", errs[0].Error())
	})

	t.Run("violating expr rule produces specific reason", func(t *testing.T) {
		parent := map[string]interface{}{"code": "AB CD"}
		errs := validateValue(s.schema, codeFd, "add", "Product", parent, auth, nil, nil)
		require.Len(t, errs, 1)
		require.Equal(t, "Product code cannot contain spaces", errs[0].Error())
	})

	t.Run("empty string triggers required and min rules", func(t *testing.T) {
		parent := map[string]interface{}{"code": ""}
		errs := validateValue(s.schema, codeFd, "add", "Product", parent, auth, nil, nil)
		require.GreaterOrEqual(t, len(errs), 1)
		require.Equal(t, "Product code is required", errs[0].Error())
	})
}

func TestValidate_ArrayOfRules_UpdateVsAdd(t *testing.T) {
	const gqlSchema = `
		type Employee {
			id: ID!
			salary: Int @validate(
				add: [
					{ rule: "min=1000", reason: "Starting salary must be at least 1000" }
				]
				update: [
					{ rule: "min=1500", reason: "Updated salary must be at least 1500" },
					{ expr: "before != nil && before.salary != nil ? value >= before.salary : true", reason: "Salary cannot be reduced" }
				]
			)
		}
	`

	handler, err := NewHandler(gqlSchema, false)
	require.NoError(t, err, "NewHandler failed")

	sch, err := FromString(handler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err, "FromString failed")

	s, ok := sch.(*schema)
	require.True(t, ok)

	empDef := s.schema.Types["Employee"]
	require.NotNil(t, empDef)

	salaryFd := empDef.Fields.ForName("salary")
	require.NotNil(t, salaryFd)

	auth := AuthCtx{}

	t.Run("add action executes only add rules", func(t *testing.T) {
		parent := map[string]interface{}{"salary": 1200}
		errs := validateValue(s.schema, salaryFd, "add", "Employee", parent, auth, nil, nil)
		require.Empty(t, errs)

		parentInvalid := map[string]interface{}{"salary": 500}
		errsInvalid := validateValue(s.schema, salaryFd, "add", "Employee", parentInvalid, auth, nil, nil)
		require.Len(t, errsInvalid, 1)
		require.Equal(t, "Starting salary must be at least 1000", errsInvalid[0].Error())
	})

	t.Run("update action executes update rules", func(t *testing.T) {
		oldValue := map[string]interface{}{"salary": 2000}

		// Salary reduced: violates expr rule
		parentReduced := map[string]interface{}{"salary": 1800}
		errs := validateValue(s.schema, salaryFd, "update", "Employee", parentReduced, auth, oldValue, nil)
		require.Len(t, errs, 1)
		require.Equal(t, "Salary cannot be reduced", errs[0].Error())

		// Salary < 1500: violates min=1500
		oldValueLow := map[string]interface{}{"salary": 1000}
		parentLow := map[string]interface{}{"salary": 1200}
		errsLow := validateValue(s.schema, salaryFd, "update", "Employee", parentLow, auth, oldValueLow, nil)
		require.Len(t, errsLow, 1)
		require.Equal(t, "Updated salary must be at least 1500", errsLow[0].Error())

		// Valid increase
		parentValid := map[string]interface{}{"salary": 2500}
		errsValid := validateValue(s.schema, salaryFd, "update", "Employee", parentValid, auth, oldValue, nil)
		require.Empty(t, errsValid)
	})
}

func TestValidate_ArrayOfRules_RootRules(t *testing.T) {
	const gqlSchema = `
		type User {
			id: ID!
			username: String @validate(
				rules: [
					{ rule: "min=3", reason: "Username must be at least 3 chars" },
					{ rule: "alphanum", reason: "Username must be alphanumeric" }
				]
			)
		}
	`

	handler, err := NewHandler(gqlSchema, false)
	require.NoError(t, err, "NewHandler failed")

	sch, err := FromString(handler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err, "FromString failed")

	s, ok := sch.(*schema)
	require.True(t, ok)

	userDef := s.schema.Types["User"]
	usernameFd := userDef.Fields.ForName("username")
	auth := AuthCtx{}

	// Root rules apply to "add"
	t.Run("root rules apply to add", func(t *testing.T) {
		errs := validateValue(s.schema, usernameFd, "add", "User", map[string]interface{}{"username": "ab"}, auth, nil, nil)
		require.Len(t, errs, 1)
		require.Equal(t, "Username must be at least 3 chars", errs[0].Error())

		errsAlpha := validateValue(s.schema, usernameFd, "add", "User", map[string]interface{}{"username": "ab#!"}, auth, nil, nil)
		require.Len(t, errsAlpha, 1)
		require.Equal(t, "Username must be alphanumeric", errsAlpha[0].Error())

		errsValid := validateValue(s.schema, usernameFd, "add", "User", map[string]interface{}{"username": "alice123"}, auth, nil, nil)
		require.Empty(t, errsValid)
	})

	// Root rules apply to "update"
	t.Run("root rules apply to update", func(t *testing.T) {
		errs := validateValue(s.schema, usernameFd, "update", "User", map[string]interface{}{"username": "x"}, auth, nil, nil)
		require.Len(t, errs, 1)
		require.Equal(t, "Username must be at least 3 chars", errs[0].Error())
	})
}

func TestValidate_SingleObjectCoercion(t *testing.T) {
	const gqlSchema = `
		type Item {
			id: ID!
			tag: String @validate(
				add: { rule: "min=4", reason: "Tag must be at least 4 characters" }
			)
		}
	`

	handler, err := NewHandler(gqlSchema, false)
	require.NoError(t, err, "NewHandler failed")

	sch, err := FromString(handler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err, "FromString failed")

	s, ok := sch.(*schema)
	require.True(t, ok)

	tagFd := s.schema.Types["Item"].Fields.ForName("tag")
	auth := AuthCtx{}

	errs := validateValue(s.schema, tagFd, "add", "Item", map[string]interface{}{"tag": "abc"}, auth, nil, nil)
	require.Len(t, errs, 1)
	require.Equal(t, "Tag must be at least 4 characters", errs[0].Error())

	errsValid := validateValue(s.schema, tagFd, "add", "Item", map[string]interface{}{"tag": "abcd"}, auth, nil, nil)
	require.Empty(t, errsValid)
}

func TestValidate_InterfaceInheritance(t *testing.T) {
	const gqlSchema = `
		interface Contactable {
			email: String @validate(rule: "email", reason: "Invalid contact email")
		}

		type Customer implements Contactable {
			id: ID!
			email: String @validate(expr: "value endsWith '@company.com'", reason: "Customer email must end with @company.com")
		}
	`

	handler, err := NewHandler(gqlSchema, false)
	require.NoError(t, err, "NewHandler failed")

	sch, err := FromString(handler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err, "FromString failed")

	s, ok := sch.(*schema)
	require.True(t, ok)

	custDef := s.schema.Types["Customer"]
	emailFd := custDef.Fields.ForName("email")
	auth := AuthCtx{}

	t.Run("violates interface rule", func(t *testing.T) {
		parent := map[string]interface{}{"email": "not-an-email"}
		errs := validateValue(s.schema, emailFd, "add", "Customer", parent, auth, nil, nil)
		require.GreaterOrEqual(t, len(errs), 1)
		// One of the errors must be the interface's rule
		found := false
		for _, e := range errs {
			if e.Error() == "Invalid contact email" {
				found = true
				break
			}
		}
		require.True(t, found, "Expected interface error 'Invalid contact email'")
	})

	t.Run("violates concrete type rule", func(t *testing.T) {
		parent := map[string]interface{}{"email": "user@other.com"}
		errs := validateValue(s.schema, emailFd, "add", "Customer", parent, auth, nil, nil)
		require.Len(t, errs, 1)
		require.Equal(t, "Customer email must end with @company.com", errs[0].Error())
	})

	t.Run("passes both interface and concrete rules", func(t *testing.T) {
		parent := map[string]interface{}{"email": "user@company.com"}
		errs := validateValue(s.schema, emailFd, "add", "Customer", parent, auth, nil, nil)
		require.Empty(t, errs)
	})
}

func TestValidate_ExprCustomErrorWithTemplate(t *testing.T) {
	const gqlSchema = `
		type Account {
			id: ID!
			balance: Int @validate(
				rules: [
					{
						expr: "value >= 0 ? true : error('balance cannot be negative')"
						reason: "Validation failed for {{.field}}: {{.error}}"
					}
				]
			)
		}
	`

	handler, err := NewHandler(gqlSchema, false)
	require.NoError(t, err, "NewHandler failed")

	sch, err := FromString(handler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err, "FromString failed")

	s, ok := sch.(*schema)
	require.True(t, ok)

	balanceFd := s.schema.Types["Account"].Fields.ForName("balance")
	auth := AuthCtx{}

	parentInvalid := map[string]interface{}{"balance": -50}
	errs := validateValue(s.schema, balanceFd, "add", "Account", parentInvalid, auth, nil, nil)
	require.Len(t, errs, 1)
	require.Equal(t, "Validation failed for balance: balance cannot be negative", errs[0].Error())

	parentValid := map[string]interface{}{"balance": 100}
	errsValid := validateValue(s.schema, balanceFd, "add", "Account", parentValid, auth, nil, nil)
	require.Empty(t, errsValid)
}
