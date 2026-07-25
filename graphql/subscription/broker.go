/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package subscription

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
)

// InvalidationMessage is the payload distributed across the cluster when a mutation succeeds.
type InvalidationMessage struct {
	UIDs       []string `json:"uids,omitempty"`
	Types      []string `json:"types,omitempty"`
	Predicates []string `json:"predicates,omitempty"`
	Namespace  uint64   `json:"namespace,omitempty"`
	CommitTs   uint64   `json:"commit_ts,omitempty"`
}

// InvalidationBroker defines the interface for publishing and subscribing to cluster-wide invalidations.
type InvalidationBroker interface {
	Publish(ctx context.Context, msg *InvalidationMessage) error
	Subscribe(ctx context.Context, handler func(msg *InvalidationMessage)) error
	Close() error
}

// GlobalBrokerInstance is the active broker used by the Dgraph instance.
var (
	GlobalBrokerInstance InvalidationBroker
	brokerOnce           sync.Once
)

// GetBroker returns the active global broker instance.
func GetBroker() InvalidationBroker {
	brokerOnce.Do(func() {
		if GlobalBrokerInstance == nil {
			GlobalBrokerInstance = NewLocalBroker()
		}
	})
	return GlobalBrokerInstance
}

// SetBroker overrides the active global broker instance.
func SetBroker(b InvalidationBroker) {
	GlobalBrokerInstance = b
}

// -----------------------------------------------------------------------------
// 1. LOCAL LOOPBACK BROKER (Default)
// -----------------------------------------------------------------------------

type localBroker struct {
	sync.RWMutex
	handlers []func(msg *InvalidationMessage)
}

// NewLocalBroker returns a new in-memory local loopback broker.
func NewLocalBroker() InvalidationBroker {
	return &localBroker{
		handlers: make([]func(msg *InvalidationMessage), 0),
	}
}

func (l *localBroker) Publish(ctx context.Context, msg *InvalidationMessage) error {
	l.RLock()
	defer l.RUnlock()

	for _, handler := range l.handlers {
		go handler(msg) // non-blocking async dispatch
	}
	return nil
}

func (l *localBroker) Subscribe(ctx context.Context, handler func(msg *InvalidationMessage)) error {
	l.Lock()
	defer l.Unlock()

	l.handlers = append(l.handlers, handler)
	return nil
}

func (l *localBroker) Close() error {
	return nil
}

// -----------------------------------------------------------------------------
// 2. REDIS / NATS PLUGGABLE BROKER (Mockable / Extensible)
// -----------------------------------------------------------------------------

type externalBroker struct {
	sync.RWMutex
	addr     string
	driver   string // "redis" or "nats"
	handlers []func(msg *InvalidationMessage)
	stopCh   chan struct{}
}

// NewExternalBroker returns a new external broker driver (configured for Redis or NATS).
func NewExternalBroker(driver, addr string) InvalidationBroker {
	b := &externalBroker{
		addr:     addr,
		driver:   strings.ToLower(driver),
		handlers: make([]func(msg *InvalidationMessage), 0),
		stopCh:   make(chan struct{}),
	}
	b.startMockReceiveLoop()
	return b
}

func (e *externalBroker) Publish(ctx context.Context, msg *InvalidationMessage) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	glog.V(2).Infof("Broadcasting invalidation over %s (%s): %s", e.driver, e.addr, string(payload))
	// In production, we write payload to the Redis/NATS socket connection here.
	return nil
}

func (e *externalBroker) Subscribe(ctx context.Context, handler func(msg *InvalidationMessage)) error {
	e.Lock()
	defer e.Unlock()

	e.handlers = append(e.handlers, handler)
	return nil
}

func (e *externalBroker) Close() error {
	close(e.stopCh)
	return nil
}

func (e *externalBroker) startMockReceiveLoop() {
	go func() {
		for {
			select {
			case <-e.stopCh:
				return
			case <-time.After(10 * time.Second): // Mock status check / connection keep-alive
				glog.V(3).Infof("External broker %s connection healthy", e.driver)
			}
		}
	}()
}

// -----------------------------------------------------------------------------
// 3. NATIVE CLUSTER P2P gRPC BROKER
// -----------------------------------------------------------------------------

type nativeClusterBroker struct {
	sync.RWMutex
	handlers []func(msg *InvalidationMessage)
}

// NewNativeClusterBroker returns a new native gRPC-assisted peer cluster broker.
func NewNativeClusterBroker() InvalidationBroker {
	return &nativeClusterBroker{
		handlers: make([]func(msg *InvalidationMessage), 0),
	}
}

func (n *nativeClusterBroker) Publish(ctx context.Context, msg *InvalidationMessage) error {
	// 1. Process locally first
	n.RLock()
	for _, h := range n.handlers {
		go h(msg)
	}
	n.RUnlock()

	// 2. Broadcast to Peer Alphas using standard Go http client asynchronously.
	// Since Alpha nodes expose GraphQL /admin endpoints over HTTP/2, we can push
	// a fast lightweight POST notification to peer Alpha endpoints (e.g. /admin/subscription/invalidate).
	go n.broadcastToPeers(msg)

	return nil
}

func (n *nativeClusterBroker) Subscribe(ctx context.Context, handler func(msg *InvalidationMessage)) error {
	n.Lock()
	defer n.Unlock()

	n.handlers = append(n.handlers, handler)
	return nil
}

func (n *nativeClusterBroker) Close() error {
	return nil
}

func (n *nativeClusterBroker) broadcastToPeers(msg *InvalidationMessage) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return
	}

	// In a clustered Dgraph setup, Alphas can register internal HTTP endpoints.
	// We send a non-blocking HTTP POST request to peer nodes.
	client := &http.Client{
		Timeout: 500 * time.Millisecond,
	}

	// Mock peer address list (in real production, fetched from Zero cluster state / conn.GetPools())
	peerAddrs := []string{}

	for _, addr := range peerAddrs {
		url := fmt.Sprintf("http://%s/admin/subscription/invalidate", addr)
		req, err := http.NewRequest("POST", url, strings.NewReader(string(payload)))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			glog.Warningf("Failed to broadcast invalidation to peer %s: %s", addr, err)
			continue
		}
		resp.Body.Close()
	}
}
