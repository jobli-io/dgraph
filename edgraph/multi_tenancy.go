/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package edgraph

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/dgraph-io/dgraph/v25/protos/pb"
	"github.com/dgraph-io/dgraph/v25/query"
	"github.com/dgraph-io/dgraph/v25/schema"
	"github.com/dgraph-io/dgraph/v25/worker"
	"github.com/dgraph-io/dgraph/v25/x"
)

type ResetPasswordInput struct {
	UserID    string
	Password  string
	Namespace uint64
}

func (s *Server) ResetPassword(ctx context.Context, inp *ResetPasswordInput) error {
	query := fmt.Sprintf(`{
			x as updateUser(func: eq(dgraph.xid, "%s")) @filter(type(dgraph.type.User)) {
				uid
			}
		}`, inp.UserID)

	userNQuads := []*api.NQuad{
		{
			Subject:     "uid(x)",
			Predicate:   "dgraph.password",
			ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: inp.Password}},
		},
	}
	req := &Request{
		req: &api.Request{
			CommitNow: true,
			Query:     query,
			Mutations: []*api.Mutation{
				{
					Set:  userNQuads,
					Cond: "@if(gt(len(x), 0))",
				},
			},
		},
		doAuth: NoAuthorize,
	}
	ctx = x.AttachNamespace(ctx, inp.Namespace)
	resp, err := (&Server{}).doQuery(ctx, req)
	if err != nil {
		return errors.Wrapf(err, "Reset password for user %s in namespace %d, got error:",
			inp.UserID, inp.Namespace)
	}

	type userNode struct {
		Uid string `json:"uid"`
	}

	type userQryResp struct {
		User []userNode `json:"updateUser"`
	}
	var userResp userQryResp
	if err := json.Unmarshal(resp.GetJson(), &userResp); err != nil {
		return errors.Wrap(err, "Reset password failed with error")
	}

	if len(userResp.User) == 0 {
		return errors.New("Failed to reset password, user doesn't exist")
	}
	return nil
}

// CreateNamespaceInternal creates a new namespace. Only superadmin is authorized to do so.
// Authorization is handled by middlewares.
func (s *Server) CreateNamespaceInternal(ctx context.Context, passwd string) (uint64, error) {
	return s.CreateNamedNamespace(ctx, passwd, "")
}

// CreateNamedNamespace creates a namespace and writes its record in the registry
// of namespaces, under name if one is given. A name that is taken is refused
// before anything is made.
func (s *Server) CreateNamedNamespace(ctx context.Context, passwd, name string) (uint64, error) {
	glog.V(2).Info("Got create namespace request.")
	if name != "" {
		if _, taken, err := NamespaceByName(ctx, name); err != nil {
			return 0, errors.Wrapf(err, "Creating namespace, reading the registry:")
		} else if taken {
			return 0, errors.Errorf("a namespace named %q already exists", name)
		}
	}

	num := &pb.Num{Val: 1, Type: pb.Num_NS_ID}
	ids, err := worker.AssignNsIdsOverNetwork(ctx, num)
	if err != nil {
		return 0, errors.Wrapf(err, "Creating namespace, got error:")
	}

	ns := ids.StartId
	glog.V(2).Infof("Got a lease for NsID: %d", ns)

	if x.IsPartitionNamespace(ns) {
		// A partition inherits its schema, types and tablets from the template, so
		// creating one is only the ID lease above: nothing is written, and Zero
		// gains no tablets.
		//
		// It is given no guardians group and no groot user of its own, as a stock
		// namespace is. The people who may enter a partition directly are the
		// guardians of the root namespace, under their own names (see
		// authenticateLogin), so a partition holds no ACL records and has no
		// password to keep.
		if err := RegisterNamespace(ctx, ns, name); err != nil {
			return 0, err
		}
		glog.V(2).Infof("Created partition namespace: %d", ns)
		return ns, nil
	}

	// Attach the newly leased NsID in the context in order to create guardians/groot for it.
	ctx = x.AttachNamespace(ctx, ns)
	m := &pb.Mutations{StartTs: worker.State.GetTimestamp(false)}
	m.Schema = schema.InitialSchema(ns)
	m.Types = schema.InitialTypes(ns)
	_, err = query.ApplyMutations(ctx, m)
	if err != nil {
		return 0, err
	}

	err = x.RetryUntilSuccess(10, 100*time.Millisecond, func() error {
		return createGuardianAndGroot(ctx, passwd)
	})
	if err != nil {
		return 0, errors.Wrapf(err, "Failed to create guardian and groot: ")
	}

	if err := RegisterNamespace(ctx, ns, name); err != nil {
		return 0, err
	}
	glog.V(2).Infof("Created namespace: %d", ns)
	return ns, nil
}

// This function is used while creating new namespace. New namespace creation is only allowed
// by the guardians of the galaxy group.
func createGuardianAndGroot(ctx context.Context, passwd string) error {
	if !x.WorkerConfig.AclEnabled {
		return nil
	}

	if err := upsertGuardian(ctx); err != nil {
		return errors.Wrap(err, "While creating Guardian")
	}
	if err := upsertGroot(ctx, passwd); err != nil {
		return errors.Wrap(err, "While creating Groot")
	}
	return nil
}

// DeleteNamespace deletes a new namespace. Only superadmin is authorized to do so.
// Authorization is handled by middlewares.
func (s *Server) DeleteNamespace(ctx context.Context, namespace uint64) error {
	glog.Info("Deleting namespace: ", namespace)
	// A partition has no schema entries, so it never appears in Namespaces().
	if _, ok := schema.State().Namespaces()[namespace]; !ok && !x.IsPartitionNamespace(namespace) {
		return errors.Errorf("error deleting non-existing namespace %#x", namespace)
	}
	if err := worker.ProcessDeleteNsRequest(ctx, namespace); err != nil {
		return err
	}
	return UnregisterNamespace(ctx, namespace)
}
