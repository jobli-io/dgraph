/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package subscription

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hypermodeinc/dgraph/v25/graphql/resolve"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/x"
	"github.com/stretchr/testify/require"
)

func TestPoller_SSEPassThrough(t *testing.T) {
	// 1. Mock upstream SSE HTTP server
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "text/event-stream", r.Header.Get("Accept"))

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		require.True(t, ok)

		// Event 1
		fmt.Fprintf(w, "data: {\"delta\": \"Hello \", \"tokens\": 1}\n\n")
		flusher.Flush()

		time.Sleep(50 * time.Millisecond)

		// Event 2
		fmt.Fprintf(w, "data: {\"delta\": \"world!\", \"tokens\": 2}\n\n")
		flusher.Flush()

		time.Sleep(50 * time.Millisecond)

		// OpenAI / standard completion marker
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstreamServer.Close()

	// 2. Define schema with mode: SSE
	sch := fmt.Sprintf(`
		type StreamChunk @remote {
			delta: String
			tokens: Int
		}

		type Query {
			chatStream(prompt: String!): StreamChunk
				@withSubscription
				@custom(http: {
					url: "%s/stream"
					method: "POST"
					body: "{ prompt: \"$prompt\" }"
					mode: SSE
				})
		}
	`, upstreamServer.URL)

	handler, errs := schema.NewHandler(sch, false)
	require.Nil(t, errs)

	parsedSch, err := schema.FromString(handler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err)

	resolver := resolve.New(parsedSch, nil)
	globalEpoch := uint64(1)
	poller := NewPoller(&globalEpoch, resolver)

	// 3. Subscribe to chatStream
	subReq := &schema.Request{
		Query: `
			subscription {
				chatStream(prompt: "Say hello") {
					delta
					tokens
				}
			}
		`,
	}

	subResp, err := poller.AddSubscriber(subReq)
	require.NoError(t, err)
	require.NotNil(t, subResp)
	require.NotNil(t, subResp.UpdateCh)

	// 4. Receive Event 1
	select {
	case item, ok := <-subResp.UpdateCh:
		require.True(t, ok, "Expected first update")
		byts, err := json.Marshal(item)
		require.NoError(t, err)
		require.Contains(t, string(byts), `"delta":"Hello "`)
		require.Contains(t, string(byts), `"tokens":1`)
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for event 1")
	}

	// 5. Receive Event 2
	select {
	case item, ok := <-subResp.UpdateCh:
		require.True(t, ok, "Expected second update")
		byts, err := json.Marshal(item)
		require.NoError(t, err)
		require.Contains(t, string(byts), `"delta":"world!"`)
		require.Contains(t, string(byts), `"tokens":2`)
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for event 2")
	}

	// 6. Channel should close following [DONE]
	select {
	case _, ok := <-subResp.UpdateCh:
		require.False(t, ok, "Channel should be closed after [DONE]")
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for channel close")
	}
}

func TestPoller_SSEPassThroughCancellation(t *testing.T) {
	clientDisconnected := make(chan struct{})

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		require.True(t, ok)

		fmt.Fprintf(w, "data: {\"delta\": \"start\"}\n\n")
		flusher.Flush()

		// Wait for context cancellation
		<-r.Context().Done()
		close(clientDisconnected)
	}))
	defer upstreamServer.Close()

	sch := fmt.Sprintf(`
		type StreamChunk @remote {
			delta: String
		}

		type Query {
			infiniteStream: StreamChunk
				@withSubscription
				@custom(http: {
					url: "%s/stream"
					method: "GET"
					mode: SSE
				})
		}
	`, upstreamServer.URL)

	handler, errs := schema.NewHandler(sch, false)
	require.Nil(t, errs)

	parsedSch, err := schema.FromString(handler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err)

	resolver := resolve.New(parsedSch, nil)
	globalEpoch := uint64(1)
	poller := NewPoller(&globalEpoch, resolver)

	subReq := &schema.Request{
		Query: `
			subscription {
				infiniteStream {
					delta
				}
			}
		`,
	}

	subResp, err := poller.AddSubscriber(subReq)
	require.NoError(t, err)

	// Read first item
	select {
	case item, ok := <-subResp.UpdateCh:
		require.True(t, ok)
		byts, err := json.Marshal(item)
		require.NoError(t, err)
		require.Contains(t, string(byts), `"delta":"start"`)
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for first item")
	}

	// Terminate subscription (simulating client disconnect)
	poller.TerminateSubscription(subResp.BucketID, subResp.SubscriptionID)

	// Verify upstream receives cancellation
	select {
	case <-clientDisconnected:
		// Upstream context was cancelled successfully
	case <-time.After(2 * time.Second):
		t.Fatal("Upstream server did not receive cancellation")
	}

	// Verify channel is closed
	select {
	case _, ok := <-subResp.UpdateCh:
		require.False(t, ok, "UpdateCh should be closed after termination")
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for channel close")
	}
}

func TestPoller_SSEWithGraphQLResponseFormat(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher := w.(http.Flusher)

		// Send keepalive comment
		fmt.Fprintf(w, ": keepalive\n\n")
		flusher.Flush()

		// Send data wrapped in GraphQL {"data": {"chatStream": ...}}
		fmt.Fprintf(w, "data: {\"data\": {\"chatStream\": {\"delta\": \"chunk gql\", \"tokens\": 42}}}\n\n")
		flusher.Flush()

		fmt.Fprintf(w, "event: complete\n\n")
		flusher.Flush()
	}))
	defer upstreamServer.Close()

	sch := fmt.Sprintf(`
		type StreamChunk @remote {
			delta: String
			tokens: Int
		}

		type Query {
			chatStream: StreamChunk
				@withSubscription
				@custom(http: {
					url: "%s/stream"
					method: "GET"
					mode: SSE
				})
		}
	`, upstreamServer.URL)

	handler, errs := schema.NewHandler(sch, false)
	require.Nil(t, errs)

	parsedSch, err := schema.FromString(handler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err)

	resolver := resolve.New(parsedSch, nil)
	globalEpoch := uint64(1)
	poller := NewPoller(&globalEpoch, resolver)

	subReq := &schema.Request{
		Query: `
			subscription {
				chatStream {
					delta
					tokens
				}
			}
		`,
	}

	subResp, err := poller.AddSubscriber(subReq)
	require.NoError(t, err)

	select {
	case item, ok := <-subResp.UpdateCh:
		require.True(t, ok)
		byts, err := json.Marshal(item)
		require.NoError(t, err)
		require.Contains(t, string(byts), `"delta":"chunk gql"`)
		require.Contains(t, string(byts), `"tokens":42`)
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for gql response event")
	}

	select {
	case _, ok := <-subResp.UpdateCh:
		require.False(t, ok, "Expected channel to close after event: complete")
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for channel close")
	}
}

func TestPoller_SSEUpstreamError(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream service unavailable", http.StatusServiceUnavailable)
	}))
	defer upstreamServer.Close()

	sch := fmt.Sprintf(`
		type StreamChunk @remote {
			delta: String
		}

		type Query {
			failStream: StreamChunk
				@withSubscription
				@custom(http: {
					url: "%s/stream"
					method: "GET"
					mode: SSE
				})
		}
	`, upstreamServer.URL)

	handler, errs := schema.NewHandler(sch, false)
	require.Nil(t, errs)

	parsedSch, err := schema.FromString(handler.GQLSchema(), x.RootNamespace)
	require.NoError(t, err)

	resolver := resolve.New(parsedSch, nil)
	globalEpoch := uint64(1)
	poller := NewPoller(&globalEpoch, resolver)

	subReq := &schema.Request{
		Query: `
			subscription {
				failStream {
					delta
				}
			}
		`,
	}

	subResp, err := poller.AddSubscriber(subReq)
	require.NoError(t, err)

	select {
	case item, ok := <-subResp.UpdateCh:
		require.True(t, ok)
		byts, err := json.Marshal(item)
		require.NoError(t, err)
		require.Contains(t, string(byts), "upstream SSE service returned status 503")
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for error event")
	}

	select {
	case _, ok := <-subResp.UpdateCh:
		require.False(t, ok, "Expected channel to close after error")
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for channel close")
	}
}
