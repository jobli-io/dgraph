/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package edgraph

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/golang/glog"
	"github.com/pkg/errors"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/dgraph-io/dgraph/v25/x"
)

// SPIKE: the registry of namespaces.
//
// Stock Dgraph keeps no list of its namespaces. Zero knows the highest ID it has
// handed out, and an alpha takes a namespace to exist when it finds schema types
// under its number. A partition has no schema of its own, so nothing could find
// one: not a listing, not a backup of one tenant, not a migration.
//
// The root namespace's schema already declares the record for it: the type
// dgraph.namespace, with a unique dgraph.namespace.id and a unique
// dgraph.namespace.name. This file writes one such record for every namespace
// that is made, and removes it when the namespace is deleted. The record also
// says which namespace's schema and tablets the namespace uses
// (dgraph.namespace.template): itself for a stock namespace, the root for a
// partition.
//
// The name is how a tenant resolver finds a partition (NamespaceByName), so the
// mapping from a tenant's name to its namespace is the engine's own record and
// not an application's.

const (
	nsTypeName  = "dgraph.namespace"
	nsIdPred    = "dgraph.namespace.id"
	nsNamePred  = "dgraph.namespace.name"
	nsTemplPred = "dgraph.namespace.template"
)

// NamespaceEntry is one record of the registry.
type NamespaceEntry struct {
	Id       uint64 `json:"id"`
	Name     string `json:"name,omitempty"`
	Template uint64 `json:"template"`
}

// registryCtx is a context for the engine's own reads and writes of the registry:
// in the root namespace, allowed to write the reserved predicates.
func registryCtx(ctx context.Context) context.Context {
	return context.WithValue(x.AttachTrustedTenant(ctx, x.RootNamespace), IsGraphql, true)
}

// RegisterNamespace writes the record of namespace ns, or changes the name of the
// one it has. An empty name leaves the name as it is. A name another namespace
// holds is refused.
func RegisterNamespace(ctx context.Context, ns uint64, name string) error {
	if ns == x.RootNamespace {
		return errors.New("the root namespace has no entry in the registry")
	}
	query := fmt.Sprintf(`{
		e as var(func: eq(%s, %d)) @filter(type(%s))
		taken as var(func: eq(%s, %q)) @filter(type(%s) AND NOT uid(e))
	}`, nsIdPred, ns, nsTypeName, nsNamePred, name, nsTypeName)
	named := ""
	if name != "" {
		named = fmt.Sprintf("\n uid(e) <%s> %q .", nsNamePred, name)
	}
	fresh := fmt.Sprintf(`_:n <dgraph.type> %q .
 _:n <%s> "%d"^^<xs:int> .
 _:n <%s> "%d"^^<xs:int> .`, nsTypeName, nsIdPred, ns, nsTemplPred, x.TemplateNamespace(ns))
	if name != "" {
		fresh += fmt.Sprintf("\n _:n <%s> %q .", nsNamePred, name)
	}
	muts := []*api.Mutation{{SetNquads: []byte(fresh), Cond: "@if(eq(len(e), 0) AND eq(len(taken), 0))"}}
	if named != "" {
		muts = append(muts, &api.Mutation{SetNquads: []byte(named), Cond: "@if(eq(len(e), 1) AND eq(len(taken), 0))"})
	}
	// which case applied is read from the same request
	query = query[:len(query)-1] + `
		entry(func: uid(e)) { uid }
		held(func: uid(taken)) { uid }
	}`
	resp, err := (&Server{}).doQuery(registryCtx(ctx), &Request{
		req:    &api.Request{CommitNow: true, Query: query, Mutations: muts},
		doAuth: NoAuthorize,
	})
	if err != nil {
		return errors.Wrapf(err, "while writing the registry entry of namespace %d", ns)
	}
	var out struct {
		Held []struct{ Uid string } `json:"held"`
	}
	if err := json.Unmarshal(resp.GetJson(), &out); err != nil {
		return err
	}
	if len(out.Held) > 0 {
		return errors.Errorf("the name %q is held by another namespace", name)
	}
	return nil
}

// UnregisterNamespace removes the record of namespace ns, if it has one.
func UnregisterNamespace(ctx context.Context, ns uint64) error {
	query := fmt.Sprintf(`{ e as var(func: eq(%s, %d)) @filter(type(%s)) }`, nsIdPred, ns, nsTypeName)
	_, err := (&Server{}).doQuery(registryCtx(ctx), &Request{
		req: &api.Request{CommitNow: true, Query: query, Mutations: []*api.Mutation{{
			DelNquads: []byte("uid(e) * * ."), Cond: "@if(gt(len(e), 0))"}}},
		doAuth: NoAuthorize,
	})
	return errors.Wrapf(err, "while removing the registry entry of namespace %d", ns)
}

type registryRow struct {
	Id       *uint64 `json:"id"`
	Name     string  `json:"name"`
	Template *uint64 `json:"template"`
}

func readRegistry(ctx context.Context, fn string, vars map[string]string) ([]NamespaceEntry, error) {
	decl := ""
	if len(vars) > 0 {
		decl = "query q($name: string) "
	}
	q := fmt.Sprintf(`%s{ q(func: %s) @filter(type(%s)) { id: %s name: %s template: %s } }`,
		decl, fn, nsTypeName, nsIdPred, nsNamePred, nsTemplPred)
	resp, err := (&Server{}).doQuery(registryCtx(ctx), &Request{
		req:    &api.Request{Query: q, Vars: vars, ReadOnly: true},
		doAuth: NoAuthorize,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Q []registryRow `json:"q"`
	}
	if err := json.Unmarshal(resp.GetJson(), &out); err != nil {
		return nil, err
	}
	entries := make([]NamespaceEntry, 0, len(out.Q))
	for _, r := range out.Q {
		if r.Id == nil {
			continue
		}
		e := NamespaceEntry{Id: *r.Id, Name: r.Name, Template: *r.Id}
		if r.Template != nil {
			e.Template = *r.Template
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Id < entries[j].Id })
	return entries, nil
}

// RegisteredNamespaces returns every record of the registry, by ID.
func RegisteredNamespaces(ctx context.Context) ([]NamespaceEntry, error) {
	return readRegistry(ctx, "has("+nsIdPred+")", nil)
}

// NamespaceByName returns the namespace registered under name. found is false if
// none is. Two records under one name cannot be written, the predicate being
// unique; if they are found all the same, that is an error and not an answer.
func NamespaceByName(ctx context.Context, name string) (ns uint64, found bool, err error) {
	if name == "" {
		return 0, false, nil
	}
	entries, err := readRegistry(ctx, "eq("+nsNamePred+", $name)", map[string]string{"$name": name})
	if err != nil {
		return 0, false, err
	}
	switch len(entries) {
	case 0:
		return 0, false, nil
	case 1:
		if entries[0].Id == x.RootNamespace {
			return 0, false, nil
		}
		return entries[0].Id, true, nil
	}
	glog.Errorf("the namespace registry has %d entries named %q", len(entries), name)
	return 0, false, errors.Errorf("the namespace registry has more than one entry named %q", name)
}

// NamespaceName returns the name namespace ns is registered under, or "" if it has
// no record or no name.
func NamespaceName(ctx context.Context, ns uint64) (string, error) {
	entries, err := readRegistry(ctx, fmt.Sprintf("eq(%s, %d)", nsIdPred, ns), nil)
	if err != nil || len(entries) == 0 {
		return "", err
	}
	return entries[0].Name, nil
}

// NameNamespace gives namespace ns the name, writing its record if it has none.
// It is how a namespace made before the registry, or made without a name, is
// brought into it.
func (s *Server) NameNamespace(ctx context.Context, ns uint64, name string) error {
	if name == "" {
		return errors.New("a name is needed")
	}
	if ns == x.RootNamespace {
		return errors.New("the root namespace cannot be named")
	}
	if _, err := strconv.ParseUint(name, 10, 64); err == nil {
		return errors.Errorf("the name %q is a number and would be taken for a namespace ID", name)
	}
	return RegisterNamespace(ctx, ns, name)
}
