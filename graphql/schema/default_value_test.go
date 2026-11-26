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
	"encoding/json"
	"testing"
	"time"

	"github.com/dgraph-io/dgraph/v25/x"
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

// TestGetTransformValue_DateTimeNormalisation verifies the same invariant for
// @transform: expr:"now()" must return an RFC3339 string, not time.Time, so
// that the value stored into obj[fieldName] during mutation rewriting is
// consistent with the value:"$now" path.  Without this, @validate expressions
// that run after @transform would receive time.Time for DateTime fields and
// panic when calling date() on them.
func TestGetTransformValue_DateTimeNormalisation(t *testing.T) {
	const gqlSchema = `
		type Booking {
			id: ID!
			name: String!
			updated: DateTime! @transform(expr: "now()")
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
	require.NotNil(t, bookingDef)

	updatedFd := bookingDef.Fields.ForName("updated")
	require.NotNil(t, updatedFd, "updated field must exist")

	auth := AuthCtx{}
	parent := map[string]interface{}{"id": "0x1", "name": "Test"}

	t.Run("@transform expr:now() returns RFC3339 string (not time.Time)", func(t *testing.T) {
		val, err := getTransformValue(astSch, updatedFd, "update", "Booking", parent, auth, nil, nil, "now()")
		require.NoError(t, err)
		require.NotNil(t, val)

		str, ok := val.(string)
		require.True(t, ok, "@transform expr:now() must be normalised to string, got %T: %v", val, val)
		_, parseErr := time.Parse(time.RFC3339, str)
		assert.NoError(t, parseErr, "@transform expr:now() result %q must be valid RFC3339", str)
	})
}

// TestValidateDirective_DateTimeExprNoSchemaLoadPanic verifies that a @validate
// expr expression using date() on a DateTime field does not panic at schema
// load time.  The bug was that validateDirectiveValidation used time.Now()
// (time.Time) as the dry-run test value for DateTime fields; date() in
// expr-lang expects a string and panics with "interface {} is time.Time, not
// string" when it receives time.Time.
func TestValidateDirective_DateTimeExprNoSchemaLoadPanic(t *testing.T) {
	const gqlSchema = `
		type JobAd {
			id: ID!
			status: String
			postDate: DateTime
			expiryDate: DateTime @validate(
				update: {
					expr: """
						!(before.status == "ACTIVE" && rawInput.expiryDate != nil)
						&& (after.expiryDate == nil || date(after.expiryDate) > now())
						&& (after.expiryDate == nil || after.postDate == nil || date(after.expiryDate) > date(after.postDate))
					"""
					reason: "expiryDate must be in the future and after postDate"
				}
			)
		}
	`
	// This must not panic or return a schema-load error for the @validate expr.
	handler, err := NewHandler(gqlSchema, false)
	require.NoError(t, err, "NewHandler must succeed — @validate with date() on DateTime must not fail at schema load")
	require.NotNil(t, handler)
}

func TestGetDefaultValue_TokenUsageCreditsExpr(t *testing.T) {
	const gqlSchema = `
		type TokenUsage {
			id: ID!
			promptTokens: Int!
			cachedTokens: Int
			completionTokens: Int!
			totalTokens: Int!
			model: String!
			credits: Float
				@generate(mutation: { add: false, update: false, delete: false })
				@default(add: {
					expr: """
					let m = lower(trim(string(after.model ?? "")));
					let rates = (m contains "embedding") ? [0.0, 0.0]
						: ((m contains "carbon") || (m contains "-pro")) ? [3.5, 22.0]
						: ((m contains "flash-lite") || (m contains "hydrogen")) ? [0.2, 0.8]
						: [0.6, 5.0];
					let p = float(string(after.promptTokens ?? 0));
					let rawCached = float(string(after.cachedTokens ?? 0));
					let cached = rawCached < 0.0 ? 0.0 : (rawCached > p ? p : rawCached);
					let uncached = p - cached;
					let c = float(string(after.completionTokens ?? 0));
					(((uncached * rates[0]) + (cached * rates[0] * 0.25) + (c * rates[1])) / 1000.0)
					""",
					evaluationOrder: 10
				})
		}
	`

	handler, err := NewHandler(gqlSchema, false)
	require.NoError(t, err, "NewHandler must load TokenUsage credits @default expr cleanly")

	sch, err := FromString(handler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err)

	astSch := sch.(*schema).schema
	tuDef := astSch.Types["TokenUsage"]
	require.NotNil(t, tuDef)
	creditsFd := tuDef.Fields.ForName("credits")
	require.NotNil(t, creditsFd)

	cases := []struct {
		name     string
		parent   map[string]interface{}
		expected float64
	}{
		{
			name:     "standard flash model (json.Number from GraphQL HTTP decoder)",
			parent:   map[string]interface{}{"model": "gemini-3.5-flash", "promptTokens": json.Number("5000"), "completionTokens": json.Number("400")},
			expected: (5000.0*0.6 + 400.0*5.0) / 1000.0, // 5.0 credits
		},
		{
			name:     "standard flash model with 80% cached prompt prefix (75% cache discount)",
			parent:   map[string]interface{}{"model": "gemini-3.5-flash", "promptTokens": json.Number("10000"), "cachedTokens": json.Number("8000"), "completionTokens": json.Number("200")},
			expected: (2000.0*0.6 + 8000.0*0.6*0.25 + 200.0*5.0) / 1000.0, // 3.4 credits instead of 7.0
		},
		{
			name:     "cachedTokens clamped safely when exceeding promptTokens",
			parent:   map[string]interface{}{"model": "gemini-3.5-flash", "promptTokens": json.Number("1000"), "cachedTokens": json.Number("1500"), "completionTokens": json.Number("100")},
			expected: (1000.0*0.6*0.25 + 100.0*5.0) / 1000.0, // 0.65 credits
		},
		{
			name:     "standard flash model (float64 JSON numbers)",
			parent:   map[string]interface{}{"model": "gemini-3.5-flash", "promptTokens": float64(5000), "completionTokens": float64(400)},
			expected: (5000.0*0.6 + 400.0*5.0) / 1000.0, // 5.0 credits
		},
		{
			name:     "flagship carbon model (int64 numbers)",
			parent:   map[string]interface{}{"model": "gemini-4-carbon", "promptTokens": int64(2000), "completionTokens": int64(1000)},
			expected: (2000.0*3.5 + 1000.0*22.0) / 1000.0, // 29.0 credits
		},
		{
			name:     "flash-lite / hydrogen model",
			parent:   map[string]interface{}{"model": "gemini-3.5-flash-lite", "promptTokens": 1000, "completionTokens": 500},
			expected: (1000.0*0.2 + 500.0*0.8) / 1000.0, // 0.6 credits
		},
		{
			name:     "embedding model defaults to zero credits",
			parent:   map[string]interface{}{"model": "models/gemini-embedding-001", "promptTokens": json.Number("4000"), "completionTokens": json.Number("0")},
			expected: 0.0,
		},
		{
			name:     "nil token fields default to zero credits",
			parent:   map[string]interface{}{"model": "gemini-3.5-flash"},
			expected: 0.0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val, _, err := getDefaultValue(astSch, creditsFd, "add", "TokenUsage", tc.parent, AuthCtx{}, nil, nil)
			require.NoError(t, err)
			f, ok := val.(float64)
			require.True(t, ok, "credits @default must return float64, got %T", val)
			assert.InDelta(t, tc.expected, f, 1e-9)
		})
	}
}
