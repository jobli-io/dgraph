/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package subscription

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgryski/go-farm"
	"github.com/golang-jwt/jwt/v5"
	"github.com/golang/glog"

	"github.com/dgraph-io/dgraph/v25/graphql/resolve"
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/x"
)

// TriggerPayload specifies metadata passed to a reactive poller re-evaluation.
type TriggerPayload struct {
	CommitTs uint64
}

// SubscriberResponse holds metadata about subscriber returned on successful registration.
type SubscriberResponse struct {
	BucketID       uint64
	SubscriptionID uint64
	UpdateCh       chan interface{}
}

type subscriber struct {
	expiry    time.Time
	updateCh  chan interface{}
	timer     *time.Timer
	cancel    context.CancelFunc
	closeOnce *sync.Once
}

func (s *subscriber) closeUpdateCh() {
	if s.closeOnce != nil {
		s.closeOnce.Do(func() {
			close(s.updateCh)
		})
	}
}

// Poller manages active subscription queries, coordinating reactive re-evaluations.
type Poller struct {
	sync.RWMutex
	resolver       *resolve.RequestResolver
	pollRegistry   map[uint64]map[uint64]subscriber
	activePollers  map[uint64]chan *TriggerPayload
	dependencyReg  *DependencyRegistry
	subscriptionID uint64
	globalEpoch    *uint64
	// bucketScope says where each bucket's subscribers are: see reaches.
	bucketScope map[uint64]bucketScope
}

// bucketScope is where the subscribers of a bucket read from: their namespace, if
// it could be resolved, and their claims.
type bucketScope struct {
	ns     uint64
	hasNs  bool
	claims map[string]interface{}
}

// scopeRouting: a change is only sent to the subscriptions it can have changed.
var scopeRouting atomic.Bool

// rerunsSkipped counts the re-runs scope routing saved, for the proofs.
var rerunsSkipped atomic.Uint64

// EnableScopeRouting turns scope routing on. Without it every subscription that
// mentions a changed type is run again, whichever workspace or namespace it is in.
func EnableScopeRouting() {
	scopeRouting.Store(true)
}

// anyScope is the claim value of a caller who has not picked a scope: Jobli's
// discovery mode. Such a subscriber can see records of several scopes, so it is
// sent every change. A real implementation would take this from the schema.
const anyScope = "*"

// reaches reports whether a change described by msg can have changed the result of
// the subscribers in bucket. It is only asked of buckets that mention a changed
// type or predicate; a bucket whose result holds a changed record is always run.
func (p *Poller) reaches(bucket uint64, msg *InvalidationMessage) bool {
	where, ok := p.bucketScope[bucket]
	if !ok {
		return true
	}
	if msg.HasNamespace && where.hasNs && msg.Namespace != where.ns {
		return false
	}
	for claim, changed := range msg.Scope {
		held, ok := where.claims[claim].(string)
		if ok && held != "" && held != anyScope && held != changed {
			return false
		}
	}
	return true
}

// NewPoller returns an initialized Poller.
func NewPoller(globalEpoch *uint64, resolver *resolve.RequestResolver) *Poller {
	p := &Poller{
		resolver:      resolver,
		pollRegistry:  make(map[uint64]map[uint64]subscriber),
		activePollers: make(map[uint64]chan *TriggerPayload),
		dependencyReg: NewDependencyRegistry(),
		globalEpoch:   globalEpoch,
		bucketScope:   make(map[uint64]bucketScope),
	}

	// Register poller to receive cluster-wide invalidations from the broker
	GetBroker().Subscribe(context.Background(), p.OnInvalidate)

	// Bind package resolve's InvalidationFunc to avoid circular package imports
	resolve.InvalidationFunc = func(inv resolve.Invalidation) {
		GetBroker().Publish(context.Background(), &InvalidationMessage{
			UIDs:         inv.UIDs,
			Types:        inv.Types,
			Predicates:   inv.Predicates,
			CommitTs:     inv.CommitTs,
			Namespace:    inv.Namespace,
			HasNamespace: inv.HasNamespace,
			Scope:        inv.Scope,
		})
	}

	return p
}

// OnInvalidate is the broker callback. It routes incoming invalidations to the correct poll buckets.
func (p *Poller) OnInvalidate(msg *InvalidationMessage) {
	holding, mentioning := p.dependencyReg.AffectedBuckets(msg.UIDs, msg.Types, msg.Predicates)
	if len(holding)+len(mentioning) == 0 {
		return
	}

	p.RLock()
	defer p.RUnlock()

	affectedBuckets := holding
	for _, bucketID := range mentioning {
		if scopeRouting.Load() && !p.reaches(bucketID, msg) {
			rerunsSkipped.Add(1)
			continue
		}
		affectedBuckets = append(affectedBuckets, bucketID)
	}

	for _, bucketID := range affectedBuckets {
		if ch, ok := p.activePollers[bucketID]; ok {
			select {
			case ch <- &TriggerPayload{CommitTs: msg.CommitTs}:
			default:
				// Channel buffer full; update is already queued. Keep non-blocking.
			}
		}
	}
}

// subscriberContext builds the context each subscription query runs under, from
// the headers the subscriber presented when it connected.
//
// It resolves identity the same way the HTTP edge does, through
// x.AttachRequestIdentity, rather than attaching only the access JWT. A poll is a
// fresh query on every tick with no request of its own behind it, so these stored
// headers are the only place a credential can come from. With the JWT alone, the
// --security auth token never reached the context and no Principal was ever
// resolved, so under --security "anonymous=none" every poll was refused -- even for
// a subscriber that had presented a valid token or ACL JWT.
//
// The synthetic request has no RemoteAddr, so no peer is attached. Nothing on this
// path reads one: a subscription runs ordinary GraphQL queries, which are
// authorized on identity and predicates, never on the caller's address.
func subscriberContext(h http.Header) context.Context {
	return x.AttachRequestIdentity(context.Background(), &http.Request{Header: h})
}

// AddSubscriber handles registration for a subscriber, switching the engine to reactive mode.
func (p *Poller) AddSubscriber(req *schema.Request) (*SubscriberResponse, error) {
	p.RLock()
	resolver := p.resolver
	p.RUnlock()

	localEpoch := atomic.LoadUint64(p.globalEpoch)
	if err := resolver.ValidateSubscription(req); err != nil {
		return nil, err
	}

	authMeta := resolver.Schema().Meta().AuthMeta()
	ctx, err := authMeta.AttachAuthorizationJwt(context.Background(), req.Header)
	if err != nil {
		return nil, err
	}
	customClaims, err := authMeta.ExtractCustomClaims(ctx)
	if err != nil {
		return nil, err
	}

	if customClaims.RegisteredClaims.ExpiresAt == nil {
		customClaims.RegisteredClaims.ExpiresAt = jwt.NewNumericDate(time.Time{})
	}

	op, err := resolver.Schema().Operation(req)
	if err != nil {
		return nil, err
	}

	// Check if this subscription operation is an SSE pass-through custom query
	var sseQuery schema.Query
	var sseConfig *schema.FieldHTTPConfig
	for _, q := range op.Queries() {
		if q.IsCustomHTTP() {
			cfg, err := q.CustomHTTPConfig()
			if err == nil && cfg != nil && cfg.Mode == schema.SSE {
				sseQuery = q
				sseConfig = cfg
				break
			}
		}
	}

	if sseQuery != nil && sseConfig != nil {
		p.Lock()
		subscriptionID := p.subscriptionID
		p.subscriptionID++
		bucketID := farm.Fingerprint64([]byte(fmt.Sprintf("sse-%d", subscriptionID)))

		updateCh := make(chan interface{}, 100)
		closeOnce := &sync.Once{}

		streamCtx, cancelStream := context.WithCancel(context.Background())

		var expiryTimer *time.Timer
		expiryTime := customClaims.RegisteredClaims.ExpiresAt.Time
		if !expiryTime.IsZero() {
			duration := time.Until(expiryTime)
			expiryTimer = time.AfterFunc(duration, func() {
				p.TerminateSubscription(bucketID, subscriptionID)
			})
		}

		sub := subscriber{
			expiry:    expiryTime,
			updateCh:  updateCh,
			timer:     expiryTimer,
			cancel:    cancelStream,
			closeOnce: closeOnce,
		}

		subscriptions := make(map[uint64]subscriber)
		subscriptions[subscriptionID] = sub
		p.pollRegistry[bucketID] = subscriptions
		p.Unlock()

		glog.Infof("Started SSE pass-through subscription ID: %d", subscriptionID)
		go p.streamCustomHTTPSubscription(streamCtx, sseQuery, sseConfig, req, updateCh, closeOnce)

		return &SubscriberResponse{
			BucketID:       bucketID,
			SubscriptionID: subscriptionID,
			UpdateCh:       updateCh,
		}, nil
	}

	buf, err := json.Marshal(req)
	x.Check(err)
	var bucketID uint64
	if customClaims.AuthVariables != nil {
		authvariables, err := json.Marshal(customClaims.AuthVariables)
		if err != nil {
			return nil, err
		}
		bucketID = farm.Fingerprint64(append(buf, authvariables...))
	} else {
		bucketID = farm.Fingerprint64(buf)
	}

	// 1. Run the initial query synchronously to return the starting state to the client
	res := resolver.Resolve(subscriberContext(req.Header), req)
	if len(res.Errors) != 0 {
		return nil, res.Errors
	}

	prevHash := farm.Fingerprint64(res.Data.Bytes())
	updateCh := make(chan interface{}, 10)
	updateCh <- res.Output()

	p.Lock()
	defer p.Unlock()

	subscriptionID := p.subscriptionID
	p.subscriptionID++

	subscriptions, ok := p.pollRegistry[bucketID]
	if !ok {
		subscriptions = make(map[uint64]subscriber)
	}

	glog.Infof("Subscription reactive routing started for ID: %d in bucket: %d", subscriptionID, bucketID)
	where := bucketScope{claims: customClaims.AuthVariables}
	if tctx, err := x.ResolveTenant(subscriberContext(req.Header)); err == nil {
		if ns, err := x.ExtractNamespace(tctx); err == nil {
			where.ns, where.hasNs = ns, true
		}
	}
	p.bucketScope[bucketID] = where

	// 2. Set up Reactive Expiry Timer
	var expiryTimer *time.Timer
	expiryTime := customClaims.RegisteredClaims.ExpiresAt.Time
	if !expiryTime.IsZero() {
		duration := time.Until(expiryTime)
		expiryTimer = time.AfterFunc(duration, func() {
			p.TerminateSubscription(bucketID, subscriptionID)
		})
	}

	subscriptions[subscriptionID] = subscriber{
		expiry:    expiryTime,
		updateCh:  updateCh,
		timer:     expiryTimer,
		closeOnce: &sync.Once{},
	}
	p.pollRegistry[bucketID] = subscriptions

	// 3. Register GraphQL AST Dependencies and dynamically resolved entities
	p.dependencyReg.Register(bucketID, subscriptionID, op)
	p.dependencyReg.UpdateResolvedEntities(bucketID, subscriptionID, res.Data.Bytes())

	if ok {
		// Existing reactive goroutine is already active for this bucket. Re-use it.
		return &SubscriberResponse{
			BucketID:       bucketID,
			SubscriptionID: subscriptionID,
			UpdateCh:       subscriptions[subscriptionID].updateCh,
		}, nil
	}

	// 4. Start a new reactive poller channel and goroutine
	triggerCh := make(chan *TriggerPayload, 50)
	p.activePollers[bucketID] = triggerCh

	pollR := &pollRequest{
		bucketID:      bucketID,
		prevHash:      prevHash,
		graphqlReq:    req,
		authVariables: customClaims.AuthVariables,
		localEpoch:    localEpoch,
		triggerCh:     triggerCh,
	}
	go p.poll(pollR)

	return &SubscriberResponse{
		BucketID:       bucketID,
		SubscriptionID: subscriptionID,
		UpdateCh:       subscriptions[subscriptionID].updateCh,
	}, nil
}

type pollRequest struct {
	prevHash      uint64
	graphqlReq    *schema.Request
	bucketID      uint64
	localEpoch    uint64
	authVariables map[string]interface{}
	triggerCh     chan *TriggerPayload
}

func (p *Poller) poll(req *pollRequest) {
	p.RLock()
	resolver := p.resolver
	p.RUnlock()

	// Implement backpressure token-bucket rate limiter: max 5 executions per second
	const maxTokens = 5.0
	const refillInterval = 1000 * time.Millisecond
	tokens := maxTokens
	lastRefill := time.Now()

	for {
		// Await next reactive invalidation trigger
		trigger, ok := <-req.triggerCh
		if !ok {
			return // Channel closed, terminate poller
		}

		// Coalescence Debouncing: wait 50ms for consecutive rapid triggers
		time.Sleep(50 * time.Millisecond)
		// Drain any queued triggers during sleep to coalesce
		for len(req.triggerCh) > 0 {
			t := <-req.triggerCh
			if t != nil && t.CommitTs > trigger.CommitTs {
				trigger = t
			}
		}

		// Apply Token-Bucket Rate Limiting
		now := time.Now()
		elapsed := now.Sub(lastRefill)
		lastRefill = now
		tokens += float64(elapsed) / float64(refillInterval) * maxTokens
		if tokens > maxTokens {
			tokens = maxTokens
		}

		if tokens < 1.0 {
			// Throttled: Wait until next refill interval
			time.Sleep(refillInterval / maxTokens)
			tokens = 1.0
		}
		tokens -= 1.0

		// Check for global schema updates / epoch changes
		globalEpoch := atomic.LoadUint64(p.globalEpoch)
		if req.localEpoch != globalEpoch || globalEpoch == math.MaxUint64 {
			p.terminateSubscriptions(req.bucketID)
			return
		}

		// Execute consistent read (guaranteeing Tr >= Tw)
		ctx := subscriberContext(req.graphqlReq.Header)
		if trigger.CommitTs > 0 {
			// Set Dgraph consistent read context details
			ctx = context.WithValue(ctx, "read_ts", trigger.CommitTs)
		}
		res := resolver.Resolve(ctx, req.graphqlReq)

		currentHash := farm.Fingerprint64(res.Data.Bytes())
		if req.prevHash == currentHash {
			// Fast path check: verify if subscribers are still active
			p.Lock()
			subscribers, hasSubs := p.pollRegistry[req.bucketID]
			if !hasSubs || len(subscribers) == 0 {
				delete(p.pollRegistry, req.bucketID)
				delete(p.bucketScope, req.bucketID)
				delete(p.activePollers, req.bucketID)
				p.Unlock()
				return
			}
			p.Unlock()
			continue
		}

		req.prevHash = currentHash

		p.Lock()
		subscribers, hasSubs := p.pollRegistry[req.bucketID]
		if !hasSubs || len(subscribers) == 0 {
			delete(p.pollRegistry, req.bucketID)
			delete(p.bucketScope, req.bucketID)
			delete(p.activePollers, req.bucketID)
			p.Unlock()
			return
		}

		// Broadcast new consistent updates and re-evaluate nested tracking
		for subID, subscriber := range subscribers {
			p.dependencyReg.UpdateResolvedEntities(req.bucketID, subID, res.Data.Bytes())
			subscriber.updateCh <- res.Output()
		}
		p.Unlock()
	}
}

// TerminateSubscriptions terminates all subscriptions in a bucket.
func (p *Poller) terminateSubscriptions(bucketID uint64) {
	p.Lock()
	defer p.Unlock()

	subscriptions, ok := p.pollRegistry[bucketID]
	if !ok {
		return
	}
	for subID, subscriber := range subscriptions {
		if subscriber.timer != nil {
			subscriber.timer.Stop()
		}
		if subscriber.cancel != nil {
			subscriber.cancel()
		}
		subscriber.closeUpdateCh()
		p.dependencyReg.Deregister(bucketID, subID)
	}
	delete(p.pollRegistry, bucketID)
	delete(p.bucketScope, bucketID)
	if ch, ok := p.activePollers[bucketID]; ok {
		close(ch)
		delete(p.activePollers, bucketID)
	}
}

// TerminateSubscription terminates a specific subscription ID.
func (p *Poller) TerminateSubscription(bucketID, subscriptionID uint64) {
	p.Lock()
	defer p.Unlock()
	p.terminateSubscription(bucketID, subscriptionID)
}

func (p *Poller) terminateSubscription(bucketID, subscriptionID uint64) {
	subscriptions, ok := p.pollRegistry[bucketID]
	if !ok {
		return
	}
	subscriber, ok := subscriptions[subscriptionID]
	if ok {
		glog.Infof("Terminating reactive subscription ID: %d", subscriptionID)
		if subscriber.timer != nil {
			subscriber.timer.Stop()
		}
		if subscriber.cancel != nil {
			subscriber.cancel()
		}
		subscriber.closeUpdateCh()
		p.dependencyReg.Deregister(bucketID, subscriptionID)
	}
	delete(subscriptions, subscriptionID)
	p.pollRegistry[bucketID] = subscriptions

	if len(subscriptions) == 0 {
		delete(p.pollRegistry, bucketID)
		delete(p.bucketScope, bucketID)
		if ch, ok := p.activePollers[bucketID]; ok {
			close(ch)
			delete(p.activePollers, bucketID)
		}
	}
}

var sseHttpClient = &http.Client{
	Timeout: 0,
	Transport: &http.Transport{
		DisableCompression: true,
	},
}

func (p *Poller) streamCustomHTTPSubscription(
	ctx context.Context,
	q schema.Query,
	fconf *schema.FieldHTTPConfig,
	req *schema.Request,
	updateCh chan interface{},
	closeOnce *sync.Once,
) {
	defer func() {
		closeOnce.Do(func() {
			close(updateCh)
		})
	}()

	var reqBody io.Reader
	if fconf.Template != nil {
		bodyBytes, err := json.Marshal(fconf.Template)
		if err == nil && len(bodyBytes) > 0 {
			reqBody = bytes.NewReader(bodyBytes)
		}
	}
	if reqBody == nil {
		reqBody = http.NoBody
	}

	httpReq, err := http.NewRequestWithContext(ctx, fconf.Method, fconf.URL, reqBody)
	if err != nil {
		errResp := &schema.Response{
			Errors: []*x.GqlError{
				x.GqlErrorf("failed to create upstream SSE request: %v", err),
			},
		}
		select {
		case <-ctx.Done():
		case updateCh <- errResp.Output():
		}
		return
	}

	for k, vv := range fconf.ForwardHeaders {
		for _, v := range vv {
			httpReq.Header.Add(k, v)
		}
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")

	resp, err := sseHttpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() == nil {
			errResp := &schema.Response{
				Errors: []*x.GqlError{
					x.GqlErrorf("failed to connect to upstream SSE endpoint: %v", err),
				},
			}
			select {
			case <-ctx.Done():
			case updateCh <- errResp.Output():
			}
		}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		errResp := &schema.Response{
			Errors: []*x.GqlError{
				x.GqlErrorf("upstream SSE service returned status %d: %s", resp.StatusCode, string(body)),
			},
		}
		select {
		case <-ctx.Done():
		case updateCh <- errResp.Output():
		}
		return
	}

	dispatchPayload := func(payload string) {
		var decoded interface{}
		if err := schema.Unmarshal([]byte(payload), &decoded); err != nil {
			decoded = payload
		}

		var resMap map[string]interface{}
		var extraErrors []*x.GqlError

		if decodedMap, ok := decoded.(map[string]interface{}); ok {
			if errsVal, hasErrs := decodedMap["errors"]; hasErrs {
				if errList, isList := errsVal.([]interface{}); isList {
					for _, item := range errList {
						if errItem, isMap := item.(map[string]interface{}); isMap {
							if msg, ok := errItem["message"].(string); ok {
								extraErrors = append(extraErrors, x.GqlErrorf("%s", msg))
							}
						}
					}
				}
			}

			if dataVal, hasData := decodedMap["data"]; hasData {
				if dataObj, isObj := dataVal.(map[string]interface{}); isObj {
					if fieldVal, hasField := dataObj[q.Name()]; hasField {
						resMap = map[string]interface{}{q.RemoteResponseName(): fieldVal}
					} else if fieldVal, hasField := dataObj[q.RemoteResponseName()]; hasField {
						resMap = map[string]interface{}{q.RemoteResponseName(): fieldVal}
					} else {
						resMap = map[string]interface{}{q.RemoteResponseName(): dataObj}
					}
				} else {
					resMap = map[string]interface{}{q.RemoteResponseName(): dataVal}
				}
			} else {
				resMap = map[string]interface{}{q.RemoteResponseName(): decodedMap}
			}
		} else {
			resMap = map[string]interface{}{q.RemoteResponseName(): decoded}
		}

		completedBytes, compErrs := schema.CompleteObject(q.PreAllocatePathSlice(), []schema.Field{q}, resMap)
		allErrors := append(compErrs, extraErrors...)
		var outResp *schema.Response
		if len(allErrors) > 0 {
			outResp = &schema.Response{
				Errors: allErrors,
				Data:   *bytes.NewBuffer(completedBytes),
			}
		} else {
			outResp = &schema.Response{
				Data: *bytes.NewBuffer(completedBytes),
			}
		}

		select {
		case <-ctx.Done():
			return
		case updateCh <- outResp.Output():
		}
	}

	reader := bufio.NewReader(resp.Body)
	var currentData strings.Builder

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				glog.Warningf("SSE stream read error: %v", err)
			}
			break
		}

		line = strings.TrimRight(line, "\r\n")

		if strings.HasPrefix(line, ":") {
			// SSE comment / heartbeat ping
			continue
		}

		if line == "event: complete" {
			break
		}

		if line == "" {
			if currentData.Len() > 0 {
				payload := currentData.String()
				currentData.Reset()
				if payload == "[DONE]" {
					break
				}
				dispatchPayload(payload)
			}
			continue
		}

		if strings.HasPrefix(line, "data:") {
			dataContent := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if currentData.Len() > 0 {
				currentData.WriteString("\n")
			}
			currentData.WriteString(dataContent)
		}
	}

	if currentData.Len() > 0 {
		payload := currentData.String()
		if payload != "[DONE]" {
			dispatchPayload(payload)
		}
	}
}
