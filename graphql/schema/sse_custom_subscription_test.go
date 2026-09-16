/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"strings"
	"testing"

	"github.com/hypermodeinc/dgraph/v25/x"
	"github.com/stretchr/testify/require"
)

func TestCustomHTTPSubscription_ValidSchema(t *testing.T) {
	sch := `
		type StreamChunk @remote {
			delta: String
			tokens: Int
		}

		type Query {
			aiStream(prompt: String!): StreamChunk
				@withSubscription
				@custom(http: {
					url: "http://localhost:9999/v1/chat/stream"
					method: "POST"
					body: "{ prompt: \"$prompt\" }"
					mode: SSE
					forwardHeaders: ["Authorization"]
				})
		}
	`

	handler, errs := NewHandler(sch, false)
	require.Nil(t, errs, "Schema should be valid")
	require.NotNil(t, handler)

	gqlSchemaStr := handler.GQLSchema()
	require.True(t, strings.Contains(gqlSchemaStr, "type Subscription {"), "Generated schema should have Subscription type")
	require.True(t, strings.Contains(gqlSchemaStr, "aiStream(prompt: String!): StreamChunk"), "aiStream should be in Subscription type")

	parsed, err := FromString(gqlSchemaStr, x.RootNamespace)
	require.NoError(t, err, "Generated schema should parse cleanly")
	require.NotNil(t, parsed)
}

func TestCustomHTTPSubscription_InvalidModeOnQueryWithoutSubscription(t *testing.T) {
	sch := `
		type StreamChunk @remote {
			delta: String
		}

		type Query {
			aiStream(prompt: String!): StreamChunk
				@custom(http: {
					url: "http://localhost:9999/stream"
					method: "POST"
					mode: SSE
				})
		}
	`

	_, errs := NewHandler(sch, false)
	require.NotNil(t, errs, "Schema should fail when mode SSE is used on Query without @withSubscription")
	require.Contains(t, errs.Error(), "mode field inside @custom directive can't be present on Query/Mutation")
}

func TestCustomHTTPSubscription_InvalidSubscriptionWithSingleMode(t *testing.T) {
	sch := `
		type StreamChunk @remote {
			delta: String
		}

		type Query {
			aiStream(prompt: String!): StreamChunk
				@withSubscription
				@custom(http: {
					url: "http://localhost:9999/stream"
					method: "POST"
					mode: SINGLE
				})
		}
	`

	_, errs := NewHandler(sch, false)
	require.NotNil(t, errs, "Schema should fail when @withSubscription is used on custom http with SINGLE mode")
	require.Contains(t, errs.Error(), "custom query should have dql argument")
}
