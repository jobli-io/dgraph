/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

// Tests for getDefaultValue DateTime normalisation.
//
// expr-lang's built-in now() returns time.Time, while value:"$now" returns a
// plain RFC3339 string.  Without normalisation in resolveExpr, the two paths
// would leave obj[fieldName] with different Go types, breaking subsequent
// @validate / @transform expressions that compare the field value.
//
// The fix (resolveExpr normalises time.Time → string for DateTime fields) is
// verified here by calling getDefaultValue directly and asserting the returned
// value is always a string in both cases.

import (
	"testing"
	"time"

	"github.com/hypermodeinc/dgraph/v25/x"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetDefaultValue_DateTimeNormalisation(t *testing.T) {
	const gqlSchema = `
		type Booking {
			id: ID!
			name: String!
			created: DateTime! @default(add: {value: "$now"})
			exprCreated: DateTime @default(add: {expr: "now()"})
		}
	`

	handler, err := NewHandler(gqlSchema, false)
	require.NoError(t, err, "NewHandler failed")

	sch, err := FromString(handler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err, "FromString failed")

	s, ok := sch.(*schema)
	require.True(t, ok)

	astSch := s.schema
	bookingDef := astSch.Types["Booking"]
	require.NotNil(t, bookingDef, "Booking type must exist")

	createdFd := bookingDef.Fields.ForName("created")
	exprCreatedFd := bookingDef.Fields.ForName("exprCreated")
	require.NotNil(t, createdFd, "created field must exist")
	require.NotNil(t, exprCreatedFd, "exprCreated field must exist")

	auth := AuthCtx{}
	parent := map[string]interface{}{}

	t.Run("value:$now returns RFC3339 string", func(t *testing.T) {
		val, _, err := getDefaultValue(astSch, createdFd, "add", "Booking", parent, auth, nil, nil)
		require.NoError(t, err)
		require.NotNil(t, val)

		s, ok := val.(string)
		require.True(t, ok, "value:$now must return string, got %T: %v", val, val)
		_, parseErr := time.Parse(time.RFC3339, s)
		assert.NoError(t, parseErr, "value:$now result %q must be valid RFC3339", s)
	})

	t.Run("expr:now() returns RFC3339 string (not time.Time)", func(t *testing.T) {
		val, _, err := getDefaultValue(astSch, exprCreatedFd, "add", "Booking", parent, auth, nil, nil)
		require.NoError(t, err)
		require.NotNil(t, val)

		// Must be string, NOT time.Time.
		str, ok := val.(string)
		require.True(t, ok, "expr:now() must be normalised to string, got %T: %v", val, val)
		_, parseErr := time.Parse(time.RFC3339, str)
		assert.NoError(t, parseErr, "expr:now() result %q must be valid RFC3339", str)
	})

	t.Run("both paths produce the same Go type", func(t *testing.T) {
		vVal, _, err := getDefaultValue(astSch, createdFd, "add", "Booking", parent, auth, nil, nil)
		require.NoError(t, err)

		eVal, _, err := getDefaultValue(astSch, exprCreatedFd, "add", "Booking", parent, auth, nil, nil)
		require.NoError(t, err)

		// IsType checks that the two values have the same dynamic type.
		assert.IsType(t, vVal, eVal, "value:$now and expr:now() must produce the same Go type")
	})
}
