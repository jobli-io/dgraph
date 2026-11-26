/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"google.golang.org/grpc/metadata"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/dgraph-io/dgraph/v25/edgraph"
	"github.com/dgraph-io/dgraph/v25/graphql/authorization"
	"github.com/dgraph-io/dgraph/v25/x"
)

// Claim-based tenant resolver for partition namespaces (SPIKE).
//
// The namespace a request acts in is derived from a verified credential and
// nothing else: the JWT the root namespace's GraphQL schema already declares in
// its `# Dgraph.Authorization` line. The token's workspace claim is looked up in
// a directory kept in the root namespace, which maps a workspace name to the
// partition that holds it.
//
//	no token                      root namespace (the control plane)
//	token fails verification      rejected
//	claim absent, "" or "*"       root namespace (discovery mode)
//	claim names a known workspace that workspace's partition
//	claim names anything else     rejected, never the root namespace
//
// A request therefore reaches a partition only by presenting a token that
// verifies and names it. The root namespace holds no tenant data, so what an
// unauthenticated caller can reach there is a question for the control plane's
// own access rules, not for this resolver.
//
// The directory is two predicates in the root namespace, named as the GraphQL
// type `WorkspaceEntry { name: String! @id, namespace: Int! }` would generate
// them, so that type can be added to the schema later without moving data.

const (
	// tenantJwtMdKey carries the request's auth JWT from the HTTP edge to the
	// resolver. It is a credential, not a claim: whatever arrives under it is
	// verified before it is believed, so it does not matter that a gRPC client
	// can set it directly.
	tenantJwtMdKey = "partition-tenant-jwt"

	dirNamePred = "WorkspaceEntry.name"
	dirNsPred   = "WorkspaceEntry.namespace"

	// A workspace that is not in the directory is re-checked after this long, so
	// one created a moment ago becomes reachable quickly.
	dirMissTTL = 2 * time.Second
	// maxCached bounds both caches; they are dropped whole when they fill up.
	maxCached = 50000
)

type dirEntry struct {
	ns    uint64
	found bool
	until time.Time
}

type tokenEntry struct {
	ns    uint64
	until time.Time
}

type jwtTenantResolver struct {
	claim  string
	dirTTL time.Duration

	mu     sync.RWMutex
	dir    map[string]dirEntry
	tokens map[string]tokenEntry
}

// InstallJWTTenantResolver makes the workspace claim of the root schema's auth
// JWT the only way to select a partition namespace. Call it during command
// setup, before any listener starts serving.
func InstallJWTTenantResolver(claim string, dirTTL time.Duration) {
	t := &jwtTenantResolver{
		claim:  claim,
		dirTTL: dirTTL,
		dir:    make(map[string]dirEntry),
		tokens: make(map[string]tokenEntry),
	}
	x.SetTenantCredentialAttacher(t.attach)
	x.SetTenantResolver(t.resolve)
}

// rootAuthMeta returns the JWT settings of the root namespace's GraphQL schema,
// or nil if the root has no schema yet or declares no authorization.
func rootAuthMeta() *authorization.AuthMeta {
	if adminServerVar == nil {
		return nil
	}
	if err := adminServerVar.lazyLoadSchema(x.RootNamespace); err != nil {
		return nil
	}
	gh, ok := adminServerVar.gqlServer.(*graphqlHandler)
	if !ok {
		return nil
	}
	gh.resolverMux.RLock()
	r := gh.resolver[x.RootNamespace]
	gh.resolverMux.RUnlock()
	if r == nil || r.Schema() == nil || r.Schema().Meta() == nil {
		return nil
	}
	return r.Schema().Meta().AuthMeta()
}

// attach copies the auth JWT of an HTTP request into the context metadata. It
// reads the header the root schema names, so there is one place that says which
// header carries the credential.
func (t *jwtTenantResolver) attach(ctx context.Context, r *http.Request) context.Context {
	if r == nil {
		return ctx
	}
	meta := rootAuthMeta()
	if meta == nil || meta.GetHeader() == "" {
		return ctx
	}
	val := r.Header.Get(meta.GetHeader())
	if val == "" {
		return ctx
	}
	if strings.HasPrefix(strings.ToLower(val), "bearer ") {
		val = strings.TrimSpace(val[len("bearer "):])
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		md = metadata.New(nil)
	} else {
		md = md.Copy()
	}
	md.Set(tenantJwtMdKey, val)
	return metadata.NewIncomingContext(ctx, md)
}

// presentedToken returns the auth JWT on ctx. The HTTP edge files it under
// tenantJwtMdKey; the GraphQL layer files the same header under its own key. If
// both are there they must agree, or the request could be routed by one token
// and authorized by another.
func presentedToken(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", nil
	}
	token := ""
	for _, key := range []string{tenantJwtMdKey, string(authorization.AuthJwtCtxKey)} {
		for _, v := range md.Get(key) {
			if v == "" {
				continue
			}
			if token != "" && token != v {
				return "", errors.New("request carries more than one auth token")
			}
			token = v
		}
	}
	return token, nil
}

func (t *jwtTenantResolver) resolve(ctx context.Context) (context.Context, error) {
	// Whatever namespace the client put on the request has no standing.
	ctx = x.ClearIncomingNamespace(ctx)

	token, err := presentedToken(ctx)
	if err != nil {
		return ctx, err
	}
	if token == "" {
		return x.AttachNamespace(ctx, x.RootNamespace), nil
	}

	now := time.Now()
	t.mu.RLock()
	cached, ok := t.tokens[token]
	t.mu.RUnlock()
	if ok && now.Before(cached.until) {
		return x.AttachNamespace(ctx, cached.ns), nil
	}

	meta := rootAuthMeta()
	if meta == nil {
		return ctx, errors.New("cannot verify the auth token: the root namespace's GraphQL " +
			"schema declares no Dgraph.Authorization")
	}
	// ExtractCustomClaims reads the token from context metadata; hand it a context
	// that carries this token and nothing else.
	vctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(string(authorization.AuthJwtCtxKey), token))
	claims, err := meta.ExtractCustomClaims(vctx)
	if err != nil {
		return ctx, errors.Wrap(err, "auth token rejected")
	}

	ws, _ := claims.AuthVariables[t.claim].(string)
	ns := x.RootNamespace
	if ws != "" && ws != "*" {
		found, ok, err := t.lookup(ws, now)
		if err != nil {
			return ctx, errors.Wrap(err, "workspace directory lookup failed")
		}
		if !ok {
			return ctx, errors.Errorf("workspace %q is not in the directory", ws)
		}
		ns = found
	}

	// Believe this token for as long as the directory entry, and no longer than
	// the token itself is valid.
	until := now.Add(t.dirTTL)
	if claims.ExpiresAt != nil && claims.ExpiresAt.Time.Before(until) {
		until = claims.ExpiresAt.Time
	}
	t.mu.Lock()
	if len(t.tokens) >= maxCached {
		t.tokens = make(map[string]tokenEntry)
	}
	t.tokens[token] = tokenEntry{ns: ns, until: until}
	t.mu.Unlock()

	return x.AttachNamespace(ctx, ns), nil
}

// lookup returns the namespace the directory files ws under.
func (t *jwtTenantResolver) lookup(ws string, now time.Time) (uint64, bool, error) {
	t.mu.RLock()
	e, ok := t.dir[ws]
	t.mu.RUnlock()
	if ok && now.Before(e.until) {
		return e.ns, e.found, nil
	}

	req := &api.Request{
		Query: `query q($name: string) { q(func: eq(` + dirNamePred + `, $name), first: 2) { ns: ` +
			dirNsPred + ` } }`,
		Vars:     map[string]string{"$name": ws},
		ReadOnly: true,
	}
	// The directory is read on the resolver's own authority, in the root namespace.
	qctx := x.AttachTrustedTenant(context.Background(), x.RootNamespace)
	resp, err := (&edgraph.Server{}).QueryNoAuth(qctx, req)
	if err != nil {
		return 0, false, err
	}
	var out struct {
		Q []struct {
			Ns *uint64 `json:"ns"`
		} `json:"q"`
	}
	if err := json.Unmarshal(resp.GetJson(), &out); err != nil {
		return 0, false, err
	}

	e = dirEntry{until: now.Add(dirMissTTL)}
	switch {
	case len(out.Q) > 1:
		// Two entries for one name: there is no safe answer.
		glog.Errorf("workspace directory has more than one entry for %q", ws)
	case len(out.Q) == 1 && out.Q[0].Ns != nil && *out.Q[0].Ns != x.RootNamespace:
		e = dirEntry{ns: *out.Q[0].Ns, found: true, until: now.Add(t.dirTTL)}
	}
	t.mu.Lock()
	if len(t.dir) >= maxCached {
		t.dir = make(map[string]dirEntry)
	}
	t.dir[ws] = e
	t.mu.Unlock()
	return e.ns, e.found, nil
}
