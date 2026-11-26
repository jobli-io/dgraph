/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package x

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/pkg/errors"
	"google.golang.org/grpc/metadata"
)

// Partition namespaces (SPIKE).
//
// A stock namespace owns a full copy of the schema and one tablet per predicate
// in Zero, so Zero's membership state grows with namespaces x predicates. A
// partition namespace owns only data: its keys still carry its namespace, but
// its schema, types and tablets are those of the root namespace, which acts as
// the template. Zero therefore holds one tablet set regardless of how many
// partitions exist.
//
// In this spike every non-root namespace is a partition when the feature flag is
// on. Where the list of partitions should live is an open design question.

// PartitionNsHeader is the HTTP header (and gRPC metadata key) the spike tenant
// resolver reads the partition namespace from. It is client-controlled, so the
// resolver is for local proof points only: the real resolver must derive the
// namespace from a verified credential.
const PartitionNsHeader = "x-dgraph-partition-ns"

var partitionNamespaces atomic.Bool

// rootNsPrefix is the namespaced-attribute prefix of the root namespace.
const rootNsPrefix = "0" + NsSeparator

// SetPartitionNamespaces turns partition namespaces on or off. Call it during
// command setup, before any listener starts serving.
func SetPartitionNamespaces(on bool) {
	partitionNamespaces.Store(on)
}

// PartitionNamespacesEnabled reports whether partition namespaces are on.
func PartitionNamespacesEnabled() bool {
	return partitionNamespaces.Load()
}

// IsPartitionNamespace reports whether ns inherits its schema and tablets from
// the root namespace.
func IsPartitionNamespace(ns uint64) bool {
	return ns != RootNamespace && partitionNamespaces.Load()
}

// TemplateNamespace returns the namespace whose schema, types, tablets and
// GraphQL schema serve ns.
func TemplateNamespace(ns uint64) uint64 {
	if IsPartitionNamespace(ns) {
		return RootNamespace
	}
	return ns
}

// IsPartitionAttr reports whether the namespaced attr belongs to a partition.
func IsPartitionAttr(attr string) bool {
	if !partitionNamespaces.Load() || strings.HasPrefix(attr, rootNsPrefix) {
		return false
	}
	return strings.Contains(attr, NsSeparator)
}

// TemplateAttr maps a namespaced attribute (or type name) of a partition to the
// same attribute in the root namespace. It is the key under which schema state
// and tablets are looked up. Attributes of the root namespace, and all
// attributes when partitions are off, are returned unchanged.
//
// It must never be used to build a storage key: data keys keep their own
// namespace.
func TemplateAttr(attr string) string {
	if !partitionNamespaces.Load() {
		return attr
	}
	return RootAttr(attr)
}

// RootAttr maps a namespaced attribute to the same attribute in the root
// namespace, whether or not partition namespaces are on in this process. It is
// for code that learns from its input that namespaces are partitions, such as a
// restore reading a backup taken from a partitioned cluster.
func RootAttr(attr string) string {
	if strings.HasPrefix(attr, rootNsPrefix) {
		return attr
	}
	i := strings.Index(attr, NsSeparator)
	if i < 0 {
		return attr
	}
	return rootNsPrefix + attr[i+len(NsSeparator):]
}

// AttrInNamespaceOf returns attr re-homed into the namespace of like. Schema
// state is stored under the template's names, so anything read out of it and
// handed back to a caller working in a partition has to be re-homed, or the
// caller would go on to read the root namespace's data.
func AttrInNamespaceOf(like, attr string) string {
	i := strings.Index(like, NsSeparator)
	j := strings.Index(attr, NsSeparator)
	if i < 0 || j < 0 {
		return attr
	}
	return like[:i] + attr[j:]
}

// TenantCredentialAttacher copies whatever the installed tenant resolver needs
// from an HTTP request into the context metadata. Each resolver brings its own,
// so a header one resolver trusts is never even read under another.
type TenantCredentialAttacher func(ctx context.Context, r *http.Request) context.Context

var tenantCredentialAttacher atomic.Pointer[TenantCredentialAttacher]

// SetTenantCredentialAttacher installs the attacher that goes with the tenant
// resolver. Call it during command setup, before any listener starts serving.
func SetTenantCredentialAttacher(a TenantCredentialAttacher) {
	if a == nil {
		tenantCredentialAttacher.Store(nil)
		return
	}
	tenantCredentialAttacher.Store(&a)
}

func attachTenantCredential(ctx context.Context, r *http.Request) context.Context {
	if a := tenantCredentialAttacher.Load(); a != nil {
		return (*a)(ctx, r)
	}
	return ctx
}

// AttachPartitionNs copies the partition header of an HTTP request into the
// context metadata, where PartitionHeaderResolver reads it.
func AttachPartitionNs(ctx context.Context, r *http.Request) context.Context {
	if !partitionNamespaces.Load() || r == nil {
		return ctx
	}
	v := r.Header.Get(PartitionNsHeader)
	if v == "" {
		return ctx
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		md = metadata.New(nil)
	} else {
		md = md.Copy()
	}
	md.Set(PartitionNsHeader, v)
	return metadata.NewIncomingContext(ctx, md)
}

// PartitionHeaderResolver is the spike TenantResolver: the request acts in the
// namespace named by PartitionNsHeader, or in the root namespace without one.
func PartitionHeaderResolver(ctx context.Context) (context.Context, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return AttachNamespace(ctx, RootNamespace), nil
	}
	v := md.Get(PartitionNsHeader)
	if len(v) == 0 {
		return AttachNamespace(ctx, RootNamespace), nil
	}
	ns, err := strconv.ParseUint(v[0], 10, 64)
	if err != nil {
		return ctx, errors.Wrapf(err, "invalid %s", PartitionNsHeader)
	}
	return AttachNamespace(ctx, ns), nil
}
