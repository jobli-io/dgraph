/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	dgoapi "github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/dql"
	"github.com/hypermodeinc/dgraph/v25/graphql/authorization"
	"github.com/hypermodeinc/dgraph/v25/graphql/dgraph"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/x"
)

const touchedUidsKey = "_total"

// Mutations come in like this with variables:
//
// mutation themutation($post: PostInput!) {
//   addPost(input: $post) { ... some query ...}
// }
// - with variable payload
// { "post":
//   { "title": "My Post",
//     "author": { authorID: 0x123 },
//     ...
//   }
// }
//
//
// Or, like this with the payload in the mutation arguments
//
// mutation themutation {
//   addPost(input: { title: ... }) { ... some query ...}
// }
//
//
// Either way we build up a Dgraph json mutation to add the object
//
// For now, all mutations are only 1 level deep (cause of how we build the
// input objects) and only create a single node (again cause of inputs)

// A MutationResolver can resolve a single mutation.
type MutationResolver interface {
	Resolve(ctx context.Context, mutation schema.Mutation) (*Resolved, bool)
}

// A MutationRewriter can transform a GraphQL mutation into a Dgraph mutation and
// can build a Dgraph dql.GraphQuery to follow a GraphQL mutation.
//
// Mutations come in like:
//
//	mutation addAuthor($auth: AuthorInput!) {
//	  addAuthor(input: $auth) {
//		   author {
//		     id
//		     name
//		   }
//	  }
//	}
//
// Where `addAuthor(input: $auth)` implies a mutation that must get run - written
// to a Dgraph mutation by Rewrite.  The GraphQL following `addAuthor(...)`implies
// a query to run and return the newly created author, so the
// mutation query rewriting is dependent on the context set up by the result of
// the mutation.
type MutationRewriter interface {
	// RewriteQueries generates and rewrites GraphQL mutation m into DQL queries which
	// check if any referenced node by XID or ID exist or not.
	// Instead of filtering on dgraph.type like @filter(type(Parrot)), we query `dgraph.type` and
	// filter it on GraphQL side. @filter(type(Parrot)) is costly in terms of memory and cpu.
	// Example existence queries:
	// 1. Parrot1(func: uid(0x127)) {
	//      uid
	//      dgraph.type
	//    }
	// 2.  Computer2(func: eq(Computer.name, "computer1")) {
	//       uid
	//       dgraph.type
	//     }
	// These query will be created in case of Add or Update Mutation which references node
	// 0x127 or Computer of name "computer1"
	RewriteQueries(ctx context.Context, m schema.Mutation) ([]*dql.GraphQuery, []string, error)
	// Rewrite rewrites GraphQL mutation m into a Dgraph mutation - that could
	// be as simple as a single DelNquads, or could be a Dgraph upsert mutation
	// with a query and multiple mutations guarded by conditions.
	Rewrite(ctx context.Context, m schema.Mutation, idExistence map[string]string) ([]*UpsertMutation, error)
	// FromMutationResult takes a GraphQL mutation and the results of a Dgraph
	// mutation and constructs a Dgraph query.  It's used to find the return
	// value from a GraphQL mutation - i.e. we've run the mutation indicated by m
	// now we need to query Dgraph to satisfy all the result fields in m.
	FromMutationResult(
		ctx context.Context,
		m schema.Mutation,
		assigned map[string]string,
		result map[string]interface{}) ([]*dql.GraphQuery, error)
	// MutatedRootUIDs returns a list of Root UIDs that were mutated as part of the mutation.
	MutatedRootUIDs(
		mutation schema.Mutation,
		assigned map[string]string,
		result map[string]interface{}) []string
	// SetOldValue stores the pre-mutation @oldValue map for a given blank-node variable.
	SetOldValue(
		key string,
		value map[string]interface{})
	// GetOldValueMap returns the variableOldValueMap populated during existence queries.
	// Keys are blank-node variable names (e.g. "Post_1", "Post_2") for add/upsert nodes,
	// or UpdateMutationFilterVar ("xx") for root-level update-filtered nodes.
	// Used by runPostValidate to expose the pre-mutation "before" state.
	GetOldValueMap() map[string]map[string]interface{}
}

// A DgraphExecutor can execute a query/mutation and returns the request response and any errors.
type DgraphExecutor interface {
	// Execute performs the actual query/mutation and returns a Dgraph response. If an error
	// occurs, that indicates that the execution failed in some way significant enough
	// way as to not continue processing this query/mutation or others in the same request.
	Execute(ctx context.Context, req *dgoapi.Request, field schema.Field) (*dgoapi.Response, error)
	CommitOrAbort(ctx context.Context, tc *dgoapi.TxnContext) (*dgoapi.TxnContext, error)
}

// An UpsertMutation is the query and mutations needed for a Dgraph upsert.
// The node types is a blank node name -> Type mapping of nodes that could
// be created by the upsert.
type UpsertMutation struct {
	Query     []*dql.GraphQuery
	Mutations []*dgoapi.Mutation
	NewNodes  map[string]schema.Type
}

// DgraphExecutorFunc is an adapter that allows us to compose dgraph execution and
// build a QueryExecuter from a function.  Based on the http.HandlerFunc pattern.
type DgraphExecutorFunc func(ctx context.Context, req *dgoapi.Request) (*dgoapi.Response, error)

// Execute calls qe(ctx, query)
func (ex DgraphExecutorFunc) Execute(
	ctx context.Context,
	req *dgoapi.Request) (*dgoapi.Response, error) {

	return ex(ctx, req)
}

// MutationResolverFunc is an adapter that allows to build a MutationResolver from
// a function.  Based on the http.HandlerFunc pattern.
type MutationResolverFunc func(ctx context.Context, m schema.Mutation) (*Resolved, bool)

// Resolve calls mr(ctx, mutation)
func (mr MutationResolverFunc) Resolve(ctx context.Context, m schema.Mutation) (*Resolved, bool) {
	return mr(ctx, m)
}

// NewDgraphResolver creates a new mutation resolver.  The resolver runs the pipeline:
// 1) rewrite the mutation using mr (return error if failed)
// 2) execute the mutation with me (return error if failed)
// 3) write a query for the mutation with mr (return error if failed)
// 4) execute the query with qe (return error if failed)
func NewDgraphResolver(mr MutationRewriter, ex DgraphExecutor) MutationResolver {
	return &dgraphResolver{
		mutationRewriter: mr,
		executor:         ex,
	}
}

// mutationResolver can resolve a single GraphQL mutation field
type dgraphResolver struct {
	mutationRewriter MutationRewriter
	executor         DgraphExecutor
}

func (mr *dgraphResolver) Resolve(ctx context.Context, m schema.Mutation) (*Resolved, bool) {
	span := trace.SpanFromContext(ctx)
	stop := x.SpanTimer(span, "resolveMutation")
	defer stop()
	if span != nil {
		span.AddEvent("Mutation Started", trace.WithAttributes(
			attribute.String("Mutation alias", m.Alias()),
			attribute.String("Type", m.MutatedType().DgraphName()),
		))
	}

	resolverTrace := &schema.ResolverTrace{
		Path:       []interface{}{m.ResponseName()},
		ParentType: "Mutation",
		FieldName:  m.ResponseName(),
		ReturnType: m.Type().String(),
	}
	timer := newtimer(ctx, &resolverTrace.OffsetDuration)
	timer.Start()
	defer timer.Stop()

	resolved, success := mr.rewriteAndExecute(ctx, m)
	resolverTrace.Dgraph = resolved.Extensions.Tracing.Execution.Resolvers[0].Dgraph
	resolved.Extensions.Tracing.Execution.Resolvers[0] = resolverTrace
	return resolved, success
}

func getNumUids(m schema.Mutation, a map[string]string, r map[string]interface{}) int {
	switch m.MutationType() {
	case schema.AddMutation:
		return len(a)
	default:
		mutated := extractMutated(r, m.Name())
		return len(mutated)
	}
}

// idExistenceRes represents the idExistence query result object with known and arbitrary fields of oldValue
type idExistenceRes struct {
	Uid       string                 `json:"uid"`
	Types     []string               `json:"dgraph.type"`
	OldValues map[string]interface{} `json:"-"` // Use `json:"-"` to skip default unmarshaling for this field
}

// Custom UnmarshalJSON method for idExistence result
func (r *idExistenceRes) UnmarshalJSON(data []byte) error {
	// Step 1: Unmarshal the raw JSON into a temporary map[string]interface{}
	// This will capture *all* fields from the JSON object.
	var tempMap map[string]interface{}
	if err := json.Unmarshal(data, &tempMap); err != nil {
		return fmt.Errorf("failed to unmarshal Res into temp map: %w", err)
	}

	// Step 2: Extract known fields (Uid, Types) from the temporary map
	// and delete them, so only arbitrary fields remain.

	// Extract Uid
	if uidVal, ok := tempMap["uid"]; ok {
		if uidStr, isStr := uidVal.(string); isStr {
			r.Uid = uidStr
		} else {
			// Handle case where Uid exists but is not a string (e.g., return error)
			return fmt.Errorf("uid field is not a string, got %T", uidVal)
		}
	}
	// Extract Types
	if typesVal, ok := tempMap["dgraph.type"]; ok {
		if typesSlice, isSlice := typesVal.([]interface{}); isSlice {
			r.Types = make([]string, 0, len(typesSlice))
			for i, item := range typesSlice {
				if strItem, isStr := item.(string); isStr {
					r.Types = append(r.Types, strItem)
				} else {
					// Handle case where an element in Types is not a string
					return fmt.Errorf("element at index %d in Types is not a string, got %T", i, item)
				}
			}
		} else {
			// Handle case where Types exists but is not an array (e.g., return error)
			return fmt.Errorf("types field is not an array, got %T", typesVal)
		}
		delete(tempMap, "dgraph.type") // Remove Types from tempMap
	}

	// Step 3: Assign the remaining fields in tempMap to ArbitraryFields
	r.OldValues = tempMap

	return nil
}

func (mr *dgraphResolver) rewriteAndExecute(
	ctx context.Context,
	mutation schema.Mutation) (*Resolved, bool) {
	var mutResp, qryResp *dgoapi.Response
	req := &dgoapi.Request{}
	commit := false

	defer func() {
		if !commit && mutResp != nil && mutResp.Txn != nil {
			mutResp.Txn.Aborted = true
			_, err := mr.executor.CommitOrAbort(ctx, mutResp.Txn)
			if err != nil {
				glog.Errorf("Error occurred while aborting transaction: %s", err)
			}
		}
	}()

	dgraphPreMutationQueryDuration := &schema.LabeledOffsetDuration{Label: "preMutationQuery"}
	// dgraphDeletePreQueryDuration tracks the query-field pre-query for delete mutations.
	// This query runs BEFORE the delete so that the return payload can be captured while
	// nodes still exist.  It must never be moved to after the mutation loop.
	dgraphDeletePreQueryDuration := &schema.LabeledOffsetDuration{Label: "preQuery"}
	dgraphMutationDuration := &schema.LabeledOffsetDuration{Label: "mutation"}
	dgraphPostMutationQueryDuration := &schema.LabeledOffsetDuration{Label: "query"}
	ext := &schema.Extensions{
		Tracing: &schema.Trace{
			Execution: &schema.ExecutionTrace{
				Resolvers: []*schema.ResolverTrace{
					{
						Dgraph: []*schema.LabeledOffsetDuration{
							dgraphPreMutationQueryDuration,
							dgraphDeletePreQueryDuration,
							dgraphMutationDuration,
							dgraphPostMutationQueryDuration,
						},
					},
				},
			},
		},
	}

	emptyResult := func(err error) *Resolved {
		return &Resolved{
			// all the standard mutations are nullable objects, so Data should pretty-much be
			// {"mutAlias":null} everytime.
			Data:  mutation.NullResponse(),
			Field: mutation,
			// there is no completion down the pipeline, so error's path should be prepended with
			// mutation's alias before returning the response.
			Err:        schema.PrependPath(err, mutation.ResponseName()),
			Extensions: ext,
		}
	}

	// upserts stores rewritten []*UpsertMutation by Rewrite function. These mutations
	// are then executed and the results processed and returned.
	var upserts []*UpsertMutation
	var err error
	// queries stores rewritten []*dql.GraphQuery by RewriteQueries function. These queries
	// are then executed and the results are processed
	var queries []*dql.GraphQuery
	var filterTypes []string
	queries, filterTypes, err = mr.mutationRewriter.RewriteQueries(ctx, mutation)
	if err != nil {
		return emptyResult(schema.GQLWrapf(err, "couldn't rewrite mutation %s", mutation.Name())),
			resolverFailed
	}
	// Execute queries and parse its result into a map
	qry := dgraph.AsString(queries)
	req.Query = qry

	// The query will be empty in case there is no reference XID / UID in the mutation.
	// Don't execute the query in those cases.
	// The query will also be empty in case this is not an Add or an Update Mutation.
	if req.Query != "" {
		// Executing and processing existence queries
		queryTimer := newtimer(ctx, &dgraphPreMutationQueryDuration.OffsetDuration)
		queryTimer.Start()
		mutResp, err = mr.executor.Execute(ctx, req, nil)
		queryTimer.Stop()
		if err != nil {
			gqlErr := schema.GQLWrapLocationf(
				err, mutation.Location(), "mutation %s failed", mutation.Name())
			return emptyResult(gqlErr), resolverFailed
		}
		ext.TouchedUids += mutResp.GetMetrics().GetNumUids()[touchedUidsKey]
	}

	// Parse the result of query.
	// mutResp.Json will contain response to the query.
	// The response is parsed to existenceQueriesResult
	// dgraph.type is a list that contains types and interfaces the type implements.
	// Example Response:
	// {
	// 	Project_1 :
	//		[
	//			{
	//				"uid" : "0x123",
	// 				"dgraph.type" : ["Project", "Work"]
	// 			}
	//		],
	//	Column_2 :
	//		[
	//			{
	//				"uid": "0x234",
	// 				"dgraph.type" : ["Column"]
	// 			}
	//		]
	// }
	queryResultMap := make(map[string][]idExistenceRes)
	if mutResp != nil {
		err = json.Unmarshal(mutResp.Json, &queryResultMap)
	}
	if err != nil {
		gqlErr := schema.GQLWrapLocationf(
			err, mutation.Location(), "mutation %s failed", mutation.Name())
		return emptyResult(gqlErr), resolverFailed
	}

	x.AssertTrue(len(filterTypes) == len(queries))
	// qNameToType map contains the mapping from the query name to type/interface the query response
	// has to be filtered upon.
	qNameToType := make(map[string]string)
	for i, typ := range filterTypes {
		qNameToType[queries[i].Attr] = typ
	}
	// The above response is parsed into map[string]string as follows:
	// {
	// 		"Project_1" : "0x123",
	// 		"Column_2" : "0x234"
	// }
	// As only Add and Update mutations generate queries using RewriteQueries,
	// qNameToUID map will be non-empty only in case of Add or Update Mutation.
	qNameToUID := make(map[string]string)
	// Collect the UpdateMutationFilterVar (="xx") result separately so we can call
	// SetOldValue for update mutations after the main loop.
	var updateMutFilterResults []idExistenceRes
	for key, result := range queryResultMap {
		if key == UpdateMutationFilterVar {
			// Defer handling — must not add to qNameToUID, but do capture for SetOldValue.
			updateMutFilterResults = result
			continue
		}
		var matchedResults []idExistenceRes
		typ := qNameToType[key]
		for _, res := range result {
			if x.HasString(res.Types, typ) {
				matchedResults = append(matchedResults, res)
			}
		}

		if len(matchedResults) > 1 {
			var uids []string
			for _, res := range matchedResults {
				uids = append(uids, res.Uid)
			}
			// Found multiple UIDs for query. This should ideally not happen.
			// This indicates that there are multiple nodes with same XIDs / UIDs. Throw an error.
			err = errors.New(fmt.Sprintf("Found multiple nodes with UIDs: [%s]",
				strings.Join(uids, ", ")))
			gqlErr := schema.GQLWrapLocationf(
				err, mutation.Location(), "mutation %s failed", mutation.Name())
			return emptyResult(gqlErr), resolverFailed
		}

		if len(matchedResults) == 1 {
			res := matchedResults[0]
			qNameToUID[key] = res.Uid
			mr.mutationRewriter.SetOldValue(key, res.OldValues)
		}
	}

	// For update mutations: store old values under UpdateMutationFilterVar ("xx") so that
	// @validate and @default exprs can access `before.*` fields.
	// When the filter matches multiple nodes we merge all their old-value maps — the shape
	// is identical across rows (same predicates), so the last writer wins on value, which
	// is acceptable since expressions that reference `before.*` describe schema-level
	// invariants, not per-row diffs.
	if len(updateMutFilterResults) > 0 {
		merged := make(map[string]interface{})
		for _, res := range updateMutFilterResults {
			for k, v := range res.OldValues {
				merged[k] = v
			}
		}
		mr.mutationRewriter.SetOldValue(UpdateMutationFilterVar, merged)
	}

	// Create upserts, delete mutations, update mutations, add mutations.
	upserts, err = mr.mutationRewriter.Rewrite(ctx, mutation, qNameToUID)

	if err != nil {
		return emptyResult(schema.GQLWrapf(err, "couldn't rewrite mutation %s", mutation.Name())),
			resolverFailed
	}
	if len(upserts) == 0 {
		return &Resolved{
			Data:       completeMutationResult(mutation, nil, 0),
			Field:      mutation,
			Err:        nil,
			Extensions: ext,
		}, resolverSucceeded
	}

	// For delete mutation, if query field is requested, there will be two upserts, the second one
	// isn't needed for mutation, it only has the query to fetch the query field.
	// We need to execute this query before the mutation to find out the query field.
	var queryErrs error
	if mutation.MutationType() == schema.DeleteMutation {
		// For delete mutations, the queryField is fetched HERE — before the mutation executes —
		// so that the return payload reflects nodes that still exist in the graph.
		// deleteRewriter.Rewrite() appends upserts[1] (a query-only block) only when
		// QueryField() is non-nil, matching the guard below.
		if qryField := mutation.QueryField(); qryField != nil {
			dgQuery := upserts[1].Query
			upserts = upserts[0:1] // we don't need the second upsert anymore

			// Use the dedicated preQuery timer — this is intentionally PRE-mutation.
			queryTimer := newtimer(ctx, &dgraphDeletePreQueryDuration.OffsetDuration)
			queryTimer.Start()
			qryResp, err = mr.executor.Execute(ctx, &dgoapi.Request{Query: dgraph.AsString(dgQuery),
				ReadOnly: true}, qryField)
			queryTimer.Stop()

			if err != nil && !x.IsGqlErrorList(err) {
				return emptyResult(schema.GQLWrapf(err, "couldn't execute query for mutation %s",
					mutation.Name())), resolverFailed
			} else {
				queryErrs = err
			}
			ext.TouchedUids += qryResp.GetMetrics().GetNumUids()[touchedUidsKey]
		}

		// Collect @cascadeDelete nodes before the primary delete executes.
		//
		// Fast path (ID-based deletes): the delete rewriter creates a block like
		//   Note_1 as var(func: uid(0xb9ad8)) @filter(type(Note))
		// We scan upserts[0].Query for such blocks and read Func.UID directly — no
		// extra query needed, and edges are still live.
		//
		// Slow path (filter/bulk deletes): Func.UID is empty, so we augment the
		// upsert query with consumer blocks (BuildCascadePreQuery) and run it
		// read-only to resolve the root UIDs from cascadeRoots.
		mutatedType := mutation.MutatedType()
		cascadeFields := mutatedType.CascadeDeleteFields()
		// Note: no need to re-check MutationType — already inside the DeleteMutation block above.
		if len(cascadeFields) > 0 && len(upserts) > 0 && upserts[0].Mutations != nil {
			// --- fast path: extract UIDs from Func.UID on typed uid-function blocks ---
			typeName := mutatedType.Name()
			var rootUIDs []string
			for _, qry := range upserts[0].Query {
				if qry.Func == nil || qry.Func.Name != "uid" || len(qry.Func.UID) == 0 {
					continue
				}
				filt := qry.Filter
				if filt == nil || filt.Func == nil {
					continue
				}
				if filt.Func.Name == "type" &&
					len(filt.Func.Args) > 0 &&
					filt.Func.Args[0].Value == typeName {
					for _, u := range qry.Func.UID {
						rootUIDs = append(rootUIDs, fmt.Sprintf("0x%x", u))
					}
				}
			}

			// --- slow path: run the augmented pre-query for filter/bulk deletes ---
			if len(rootUIDs) == 0 {
				glog.V(2).Infof("CASCADE-DELETE: Initiating DQL 'SLOW-PATH' augmented Pre-Query resolution for Type -> %s", typeName)

				preQuery := BuildCascadePreQuery(upserts[0].Query, MutationQueryVar, typeName)
				preResp, preErr := mr.executor.Execute(ctx,
					&dgoapi.Request{Query: dgraph.AsString(preQuery), ReadOnly: true}, nil)
				if preErr == nil && len(preResp.GetJson()) > 0 {
					var preResult map[string]interface{}
					if json.Unmarshal(preResp.GetJson(), &preResult) == nil {
						if arr, ok := preResult["cascadeRoots"].([]interface{}); ok {
							for _, item := range arr {
								if m, ok := item.(map[string]interface{}); ok {
									if uid, ok := m["uid"].(string); ok && uid != "" {
										rootUIDs = append(rootUIDs, uid)
									}
								}
							}
						}
					}
				}
			}

			if len(rootUIDs) > 0 {
				customClaims, clErr := mutation.GetAuthMeta().ExtractCustomClaims(ctx)
				if clErr == nil {
					auth := schema.AuthCtx{AuthVariables: customClaims.AuthVariables}
					var existingDeletes []interface{}
					if json.Unmarshal(upserts[0].Mutations[0].DeleteJson, &existingDeletes) == nil {
						// Reuse the delete rewriter's VariableGenerator so
						// cascade-generated reverse-edge cleanup vars don't
						// collide with variables already emitted by the rewriter.
						var cascVarGen *VariableGenerator
						if drw, ok := mr.mutationRewriter.(*deleteRewriter); ok {
							cascVarGen = drw.VarGen
						}
						_ = cascVarGen // VarGen passed to CascadeDeleteCollector in future refactor
						extended, cascErr := CascadeDeleteCollector(
							ctx, mr.executor, auth, mutatedType, rootUIDs, existingDeletes)
						if cascErr != nil {
							return emptyResult(schema.GQLWrapf(cascErr,
									"cascade delete collection failed for mutation %s", mutation.Name())),
								resolverFailed
						}

						if b, mErr := json.Marshal(extended); mErr == nil {
							upserts[0].Mutations[0].DeleteJson = b
						}
					}
				}
			}
		}
	}

	// IMPORTANT: For delete mutations the query-field pre-query has already been executed
	// above (before this point). Do NOT move any delete query-field execution to after this
	// mutation loop — the nodes will no longer exist once the delete transaction commits.
	result := make(map[string]interface{})
	newNodes := make(map[string]schema.Type)

	mutationTimer := newtimer(ctx, &dgraphMutationDuration.OffsetDuration)
	mutationTimer.Start()

	for _, upsert := range upserts {
		req.Query = dgraph.AsString(upsert.Query)
		req.Mutations = upsert.Mutations
		mutResp, err = mr.executor.Execute(ctx, req, nil)
		if err != nil {
			gqlErr := schema.GQLWrapLocationf(
				err, mutation.Location(), "mutation %s failed", mutation.Name())
			return emptyResult(gqlErr), resolverFailed

		}

		ext.TouchedUids += mutResp.GetMetrics().GetNumUids()[touchedUidsKey]
		if req.Query != "" && len(mutResp.GetJson()) != 0 {
			if err := json.Unmarshal(mutResp.GetJson(), &result); err != nil {
				return emptyResult(
						schema.GQLWrapf(err, "Couldn't unmarshal response from Dgraph mutation")),
					resolverFailed
			}
		}
		// for update mutation, if @id field is present in set then we check that
		// in filter only one node is selected. if there are multiple nodes selected,
		// then it's not possible to update all of them with same value of @id fields.
		// In that case we return error
		if mutation.MutationType() == schema.UpdateMutation {
			inp := mutation.ArgValue(schema.InputArgName).(map[string]interface{})
			setArg := inp["set"]
			objSet, okSetArg := setArg.(map[string]interface{})
			if len(objSet) == 0 && okSetArg {
				return emptyResult(
						schema.GQLWrapf(errors.Errorf("not able to find set args"+
							" in update mutation"),
							"mutation %s failed", mutation.Name())),
					resolverFailed
			}

			mutatedType := mutation.MutatedType()
			var xidsPresent bool
			if len(objSet) != 0 {
				for _, xid := range mutatedType.XIDFields() {
					if xidVal, ok := objSet[xid.Name()]; ok && xidVal != nil {
						xidsPresent = true
					}
				}
			}
			// if @id field is present in set and there are multiple nodes returned from
			// upsert query then we return error
			if xidsPresent && len(result[mutation.Name()].([]interface{})) > 1 {
				if queryAuthSelector(mutatedType) == nil {
					return emptyResult(
							schema.GQLWrapf(errors.Errorf("only one node is allowed in"+
								" the filter while updating fields with @id directive"),
								"mutation %s failed", mutation.Name())),
						resolverFailed
				}
				return emptyResult(
						schema.GQLWrapf(errors.Errorf("GraphQL debug: only one node is"+
							" allowed in the filter while updating fields with @id directive"),
							"mutation %s failed", mutation.Name())),
					resolverFailed

			}
		}

		copyTypeMap(upsert.NewNodes, newNodes)
	}
	mutationTimer.Stop()

	authErr := authorizeNewNodes(ctx, mutation, mutResp.Uids, newNodes, mr.executor, mutResp.Txn)
	if authErr != nil {
		return emptyResult(schema.GQLWrapf(authErr, "mutation failed")), resolverFailed
	}

	// @postValidate: run type-level post-mutation validation within the same
	// uncommitted transaction. If validation fails the deferred abort fires.
	if pvErr := runPostValidate(ctx, mutation, mr.executor, mr.mutationRewriter, mutResp, result); pvErr != nil {
		return emptyResult(schema.GQLWrapf(pvErr, "post-mutation validation failed")), resolverFailed
	}

	var dgQuery []*dql.GraphQuery
	dgQuery, err = mr.mutationRewriter.FromMutationResult(ctx, mutation, mutResp.GetUids(), result)
	queryErrs = schema.AppendGQLErrs(queryErrs, schema.GQLWrapf(err,
		"couldn't rewrite query for mutation %s", mutation.Name()))
	if err != nil {
		return emptyResult(queryErrs), resolverFailed
	}

	txnCtx, err := mr.executor.CommitOrAbort(ctx, mutResp.Txn)
	if err != nil {
		return emptyResult(
				schema.GQLWrapf(err, "mutation failed, couldn't commit transaction")),
			resolverFailed
	}
	commit = true

	// once committed, send async updates to configured webhooks, if any.
	if mutation.HasLambdaOnMutate() {
		// compose the new node ids
		newUids := map[string][]string{}
		for k, v := range mutResp.GetUids() {
			t := newNodes[k].Name()
			newUids[t] = append(newUids[t], v)
		}

		rootUIDs := mr.mutationRewriter.MutatedRootUIDs(mutation, mutResp.GetUids(), result)
		go sendWebhookEvent(ctx, mutation, txnCtx.CommitTs, rootUIDs, newUids)
	}

	// For delete mutation, we would have already populated qryResp if query field was requested.
	if mutation.MutationType() != schema.DeleteMutation {
		queryTimer := newtimer(ctx, &dgraphPostMutationQueryDuration.OffsetDuration)
		queryTimer.Start()
		qryResp, err = mr.executor.Execute(ctx, &dgoapi.Request{Query: dgraph.AsString(dgQuery),
			ReadOnly: true}, mutation.QueryField())
		queryTimer.Stop()

		if !x.IsGqlErrorList(err) {
			err = schema.GQLWrapf(err, "couldn't execute query for mutation %s", mutation.Name())
		}
		queryErrs = schema.AppendGQLErrs(queryErrs, err)
		ext.TouchedUids += qryResp.GetMetrics().GetNumUids()[touchedUidsKey]
	}
	numUids := getNumUids(mutation, mutResp.Uids, result)

	return &Resolved{
		Data:  completeMutationResult(mutation, qryResp.GetJson(), numUids),
		Field: mutation,
		// the error path only contains the query field, so we prepend the mutation response name
		Err:        schema.PrependPath(queryErrs, mutation.ResponseName()),
		Extensions: ext,
	}, resolverSucceeded
}

// completeMutationResult takes in the result returned for the query field of mutation and builds
// the JSON required for data field in GraphQL response.
// The input qryResult can either be nil or of the form:
//
//	{"qryFieldAlias":...}
//
// and the output will look like:
//
//	{"addAuthor":{"qryFieldAlias":...,"numUids":2,"msg":"Deleted"}}
func completeMutationResult(mutation schema.Mutation, qryResult []byte, numUids int) []byte {
	comma := ""
	var buf bytes.Buffer
	x.Check2(buf.WriteRune('{'))
	mutation.CompleteAlias(&buf)
	x.Check2(buf.WriteRune('{'))

	// Our standard MutationPayloads consist of only the following fields:
	//  * queryField
	//  * numUids
	//  * msg (only for DeleteMutationPayload)
	// And __typename can be present anywhere. So, build data accordingly.
	// Note that all these fields are nullable, so no need to raise non-null errors.
	for _, f := range mutation.SelectionSet() {
		x.Check2(buf.WriteString(comma))
		f.CompleteAlias(&buf)

		switch f.Name() {
		case schema.Typename:
			x.Check2(buf.WriteString(`"` + f.TypeName(nil) + `"`))
		case schema.Msg:
			if numUids == 0 {
				x.Check2(buf.WriteString(`"No nodes were deleted"`))
			} else {
				x.Check2(buf.WriteString(`"Deleted"`))
			}
		case schema.NumUid:
			// Although theoretically it is possible that numUids can be out of the int32 range but
			// we don't need to apply coercion rules here as per Int type because carrying out a
			// mutation which mutates more than 2 billion uids doesn't seem a practical case.
			// So, we are skipping coercion here.
			x.Check2(buf.WriteString(strconv.Itoa(numUids)))
		default: // this has to be queryField
			if len(qryResult) == 0 {
				// don't write null, instead write [] as query field is always a nullable list
				x.Check2(buf.Write(schema.JsonEmptyList))
			} else {
				// need to write only the value returned for query field, so need to remove the JSON
				// key till colon (:) and also the ending brace }.
				// 4 = {"":
				x.Check2(buf.Write(qryResult[4+len(f.ResponseName()) : len(qryResult)-1]))
			}
		}
		comma = ","
	}
	x.Check2(buf.WriteString("}}"))

	return buf.Bytes()
}

// authorizeNewNodes takes the new nodes (uids) actually created by a GraphQL mutation and
// the types that mutation rewriting expects those nodes to be (newNodeTypes) and checks if
// the JWT that came in with the request is authorized to create those nodes.  We can't check
// this before the mutation, because the nodes aren't linked into the graph yet.
//
// We group the nodes into their types, generate the authorization add rules for that type
// and then check that the authorized nodes for each type is equal to the nodes created
// for that type by performing an authorization query to Dgraph as part of the ongoing
// transaction (txn).  If the authorization query returns fewer nodes than we created, some
// of the new nodes failed the auth rules.
func authorizeNewNodes(
	ctx context.Context,
	m schema.Mutation,
	uids map[string]string,
	newNodeTypes map[string]schema.Type,
	queryExecutor DgraphExecutor,
	txn *dgoapi.TxnContext) error {

	customClaims, err := m.GetAuthMeta().ExtractCustomClaims(ctx)
	if err != nil {
		return schema.GQLWrapf(err, "authorization failed")
	}
	authVariables := customClaims.AuthVariables
	newRw := &authRewriter{
		authVariables: authVariables,
		varGen:        NewVariableGenerator(),
		selector:      addAuthSelector,
		hasAuthRules:  true,
	}

	// Collect all the newly created nodes in type groups

	newByType := make(map[string][]uint64)
	namesToType := make(map[string]schema.Type)
	for nodeName, nodeTyp := range newNodeTypes {
		if uidStr, created := uids[nodeName]; created {
			uid, err := strconv.ParseUint(uidStr, 0, 64)
			if err != nil {
				return schema.GQLWrapf(err, "authorization failed")
			}
			if nodeTyp.ListType() != nil {
				nodeTyp = nodeTyp.ListType()
			}
			namesToType[nodeTyp.Name()] = nodeTyp
			newByType[nodeTyp.Name()] = append(newByType[nodeTyp.Name()], uid)
		}
	}

	// sort to get a consistent query rewriting
	var createdTypes []string
	for typeName := range newByType {
		createdTypes = append(createdTypes, typeName)
	}
	sort.Strings(createdTypes)

	// Write auth queries for each set of node types

	var needsAuth []string
	authQrys := make(map[string][]*dql.GraphQuery)
	for _, typeName := range createdTypes {
		typ := namesToType[typeName]
		varName := newRw.varGen.Next(typ, "", "", false)
		newRw.varName = varName
		newRw.parentVarName = typ.Name() + "Root"
		authQueries, authFilter := newRw.rewriteAuthQueries(typ)

		rn := newRw.selector(typ)
		rbac := rn.EvaluateStatic(newRw.authVariables)

		if rbac == schema.Negative {
			return x.GqlErrorf("authorization failed")
		}

		if rbac == schema.Positive {
			continue
		}

		if len(authQueries) == 0 {
			continue
		}

		// Generate query blocks like this for each node type
		//
		// Todo(func: uid(Todo1)) @filter(uid(Todo2) AND uid(Todo3)) { uid }
		// Todo1 as var(func: uid(...new uids of this type...) )
		// Todo2 as var(func: uid(Todo1)) @cascade { ...auth query 1... }
		// Todo3 as var(func: uid(Todo1)) @cascade { ...auth query 2... }

		typQuery := &dql.GraphQuery{
			Attr: typ.Name(),
			Func: &dql.Function{
				Name: "uid",
				Args: []dql.Arg{{Value: varName}}},
			Filter:   authFilter,
			Children: []*dql.GraphQuery{{Attr: "uid"}}}

		nodes := newByType[typeName]
		sort.Slice(nodes, func(i, j int) bool { return nodes[i] < nodes[j] })
		varQry := &dql.GraphQuery{
			Var:  varName,
			Attr: "var",
			Func: &dql.Function{
				Name: "uid",
				UID:  nodes,
			},
		}

		needsAuth = append(needsAuth, typeName)
		authQrys[typeName] = append([]*dql.GraphQuery{typQuery, varQry}, authQueries...)

	}

	if len(needsAuth) == 0 {
		// no auth to apply
		return nil
	}

	// create the query in order so we get a stable query
	sort.Strings(needsAuth)
	var qs []*dql.GraphQuery
	for _, typeName := range needsAuth {
		qs = append(qs, authQrys[typeName]...)
	}

	resp, errs := queryExecutor.Execute(ctx,
		&dgoapi.Request{
			Query:   dgraph.AsString(qs),
			StartTs: txn.GetStartTs(),
		}, nil)
	if errs != nil || len(resp.Json) == 0 {
		return x.GqlErrorf("authorization request failed")
	}

	authResult := make(map[string]interface{})
	if err := json.Unmarshal(resp.Json, &authResult); err != nil {
		return x.GqlErrorf("authorization checking failed")
	}

	for _, typeName := range needsAuth {
		check, ok := authResult[typeName]
		if !ok || check == nil {
			// We needed auth on this type, but it wasn't even in the response.  That
			// means Dgraph found no matching nodes and returned nothing for this field.
			// So all the nodes failed auth.

			// FIXME: what do we actually want to return to users when auth failed?
			// Is this too much?
			return x.GqlErrorf("authorization failed")
		}

		foundUIDs, ok := check.([]interface{})
		if !ok {
			return x.GqlErrorf("authorization failed")
		}

		if len(newByType[typeName]) != len(foundUIDs) {
			// Some of the created nodes passed auth and some failed.
			return x.GqlErrorf("authorization failed")
		}
	}

	// By now either there were no types that needed auth, or all nodes passed the
	// auth checks.  So the mutation as a whole passed authorization.

	return nil
}

// runPostValidate executes the @postValidate type-level directive, if present, after
// the mutation has been written but before the transaction is committed.
//
// Expression context (mirrors @validate naming):
//
//	nodes  []map  — one entry per mutated node of this type. Each entry is:
//	               {
//	                 "uid":    string  — the Dgraph UID (e.g. "0x1a")
//	                 "before": map     — pre-mutation @oldValue fields (see note below)
//	                 "after":  map     — post-mutation @oldValue fields (post-write, pre-commit)
//	               }
//	auth   map    — JWT auth variables (same as @validate's auth).
//	action string — "add" or "update" (same as @validate's action).
//
// "before" state per node:
//
//	Add mutations (new nodes): before is always {} — new nodes have no prior state.
//	Add-upsert (existing nodes): before is the individual node's snapshot, keyed by
//	  blank-node name (e.g. "Post_1") in variableOldValueMap. Accurate per-node.
//	Nested same-type nodes (any depth): each node gets its own blank-node variable
//	  (e.g. Comment_1, Comment_2, Comment_3 for a self-referencing type), so before
//	  is accurate per-node regardless of nesting depth.
//	Update root nodes (matched by filter): stored under the shared key "xx"
//	  (UpdateMutationFilterVar) as a MERGED map. If the filter matches N nodes,
//	  their @oldValue fields are overlaid field-by-field (last-writer-wins). This
//	  means every root-updated node in `nodes` sees the same merged before map —
//	  not its individual pre-mutation state. This is the same constraint that
//	  @validate's `before` variable has for bulk update mutations.
//
// The expression is evaluated ONCE against the full nodes array.
func runPostValidate(
	ctx context.Context,
	mutation schema.Mutation,
	executor DgraphExecutor,
	rewriter MutationRewriter,
	mutResp *dgoapi.Response,
	result map[string]interface{},
) error {
	// 1. Resolve action string; skip delete mutations.
	action := ""
	switch mutation.MutationType() {
	case schema.AddMutation:
		action = "add"
	case schema.UpdateMutation:
		action = "update"
	default:
		// DeleteMutation and others: skip silently.
		return nil
	}

	// 2. Check if @postValidate applies for this action.
	typ := mutation.MutatedType()
	cfg := typ.PostValidateConfig(action)
	if cfg == nil {
		return nil
	}

	// 3. Collect all UIDs of this type across the entire mutation tree.
	//    Also returns the inverted map (UID → blank-node name) needed to join before state.
	uids, uidToBlankName := collectPostValidateUIDs(typ.DgraphName(), mutation, mutResp, result)
	if len(uids) == 0 {
		return nil
	}

	// 4. Fetch post-mutation (after) state for all UIDs in one DQL query within the
	//    same uncommitted transaction.
	parsedUIDs := make([]uint64, 0, len(uids))
	for _, uid := range uids {
		if v := mustParseUID(uid); v != 0 {
			parsedUIDs = append(parsedUIDs, v)
		}
	}
	if len(parsedUIDs) == 0 {
		return nil
	}

	qry := &dql.GraphQuery{
		Attr: "postValidateNodes",
		Func: &dql.Function{
			Name: "uid",
			UID:  parsedUIDs,
		},
		Children: getFieldsForExistsQuery(typ),
	}

	resp, err := executor.Execute(ctx, &dgoapi.Request{
		Query:    dgraph.AsString([]*dql.GraphQuery{qry}),
		ReadOnly: false, // must share the uncommitted txn
		StartTs:  mutResp.Txn.GetStartTs(),
	}, nil)
	if err != nil {
		return errors.Wrapf(err, "@postValidate: failed to fetch post-mutation state")
	}

	var rawResult map[string][]map[string]interface{}
	if len(resp.GetJson()) > 0 {
		if err := json.Unmarshal(resp.GetJson(), &rawResult); err != nil {
			return errors.Wrapf(err, "@postValidate: failed to unmarshal post-mutation query")
		}
	}

	// 5. Get the pre-mutation (before) state map from the rewriter.
	//    Keys: blank-node variable names for add/upsert nodes; "xx" for update root nodes.
	oldValueMap := rewriter.GetOldValueMap()
	// Fallback before for update root nodes: the merged "xx" entry.
	mergedUpdateBefore := oldValueMap[UpdateMutationFilterVar] // nil for add mutations

	// 6. Build the nodes array: each element is {uid, before, after}.
	//    - after:  post-mutation state from the DQL query, normalised to GQL field names.
	//    - before: pre-mutation state from the existence query, also normalised.
	//
	// Both the DQL existence query (GetOldValueMap) and the post-validate fetch use
	// Dgraph predicate names ("Review.rating", "Review.comment", …). We strip the
	// type prefix so expressions can use bare GQL names: .after.rating, .before.comment.
	dgraphPrefix := typ.DgraphName()
	// nodes MUST be []map[string]interface{} (not []interface{}) so that expr-lang's
	// type checker knows the element type and correctly binds `.` in {predicate} blocks.
	nodes := make([]map[string]interface{}, 0, len(rawResult["postValidateNodes"]))
	for _, rawAfter := range rawResult["postValidateNodes"] {
		uid, _ := rawAfter["uid"].(string)

		// Resolve before state: prefer per-node blank-node entry, fall back to merged.
		var rawBefore map[string]interface{}
		if blankName, ok := uidToBlankName[uid]; ok {
			rawBefore = oldValueMap[blankName] // nil for new nodes (empty before)
		}
		if rawBefore == nil {
			rawBefore = mergedUpdateBefore // nil for add-only mutations
		}
		if rawBefore == nil {
			rawBefore = map[string]interface{}{}
		}

		before := normalizePredicateKeys(rawBefore, dgraphPrefix)
		after := normalizePredicateKeys(rawAfter, dgraphPrefix)

		// new mirrors @validate's `new` variable: fields in `after` whose value differs
		// from `before`. For add mutations `before` is empty, so `new` == `after`.
		newFields := make(map[string]interface{})
		for k, v := range after {
			if bv, ok := before[k]; !ok || bv != v {
				newFields[k] = v
			}
		}

		nodes = append(nodes, map[string]interface{}{
			"uid":    uid,
			"before": before,
			"after":  after,
			"new":    newFields,
		})
	}

	// Backfill: in a multi-Alpha cluster the DQL post-mutation read above may be
	// routed to a different Alpha that cannot see the uncommitted write, causing
	// rawResult["postValidateNodes"] to be empty even though the node was just
	// created.  Ensure every UID collected from mutResp.GetUids() has an entry in
	// nodes, adding a stub (uid + before, empty after/new) if the DQL missed it.
	// Count-based lambda checks (User/Group quota) use nodes.length, so they
	// always see the correct batch size. Field-based checks (JobAd quota) can
	// inspect n.after and gracefully handle the empty-map case.
	foundInDQL := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if uid, ok := n["uid"].(string); ok {
			foundInDQL[uid] = true
		}
	}
	for _, uid := range uids {
		if foundInDQL[uid] {
			continue
		}
		var rawBefore map[string]interface{}
		if blankName, ok := uidToBlankName[uid]; ok {
			rawBefore = oldValueMap[blankName]
		}
		if rawBefore == nil {
			rawBefore = mergedUpdateBefore
		}
		if rawBefore == nil {
			rawBefore = map[string]interface{}{}
		}
		before := normalizePredicateKeys(rawBefore, dgraphPrefix)
		nodes = append(nodes, map[string]interface{}{
			"uid":    uid,
			"before": before,
			"after":  map[string]interface{}{},
			"new":    map[string]interface{}{},
		})
	}

	// 7. Compile and evaluate the CEL expression once against the full nodes array.
	//
	// Expression context variables:
	//   nodes   []map  — all mutated nodes; each element has {uid, before, after, new}.
	//                    Fields normalised to bare GQL names (.after.rating etc.)
	//   action  string — "add" or "update"
	//   auth    map    — JWT claim variables (empty map when unauthenticated)
	//
	// Per-node fields accessible inside {predicate} blocks:
	//   .uid     string — Dgraph UID of the node
	//   .before  map    — pre-mutation field values (empty for add)
	//   .after   map    — post-mutation field values
	//   .new     map    — fields that changed (after - before); mirrors @validate `new`
	//
	// Helper functions (same set as @validate):
	//   callLambda(name, payload) — invoke a registered lambda
	//   uuid()                    — new UUIDv4 string
	//   sha256(str)               — hex SHA-256
	//   generateEmbedding(...)    — vector embedding
	//   diffMap(a, b)             — fields in b that differ from a
	//   mapStringWithoutKeys(m,k) — m minus specified keys
	//   error(v)                  — abort expression with error
	//
	// IMPORTANT: expr.Run must receive the SAME type as expr.Env — the compiled bytecode
	// uses struct field offsets. Passing a map when the env shape is a struct causes the
	// "reflect: call of reflect.Value.Field on map Value" panic.
	type postValidateNode = map[string]interface{}
	type postValidateEnv struct {
		Nodes  []postValidateNode     `expr:"nodes"`
		Action string                 `expr:"action"`
		Auth   map[string]interface{} `expr:"auth"`
		// Helper functions — same set as @validate (NewExprEvaluationContext).
		CallLambda           func(string, map[string]interface{}) (interface{}, error)                            `expr:"callLambda"`
		UUID                 func() string                                                                        `expr:"uuid"`
		Sha256               func(string) string                                                                  `expr:"sha256"`
		GenerateEmbedding    func(string, string, string, map[string]any) []float32                               `expr:"generateEmbedding"`
		DiffMap              func(map[string]interface{}, map[string]interface{}) (map[string]interface{}, error) `expr:"diffMap"`
		MapStringWithoutKeys func(map[string]interface{}, []interface{}) map[string]interface{}                   `expr:"mapStringWithoutKeys"`
		Error                func(interface{}) (interface{}, error)                                               `expr:"error"`
	}
	prog, err := expr.Compile(cfg.Expr,
		expr.Env(postValidateEnv{}),
		expr.AllowUndefinedVariables(),
	)
	if err != nil {
		return errors.Wrapf(err, "@postValidate on type %s: failed to compile expression %q", typ.Name(), cfg.Expr)
	}

	// Build the full AuthCtx — same pattern as mutation_rewriter.go — so that
	// callLambda is wired up with the real JWT from the incoming HTTP request.
	authCtx := schema.AuthCtx{}
	authCtx.AccessJWT, _ = x.ExtractJwt(ctx)
	if idField := typ.IDField(); idField != nil {
		authCtx.AuthHeaderKey = idField.GetAuthMeta().GetHeader()
		authCtx.AuthHeaderValue = authorization.GetJwtToken(ctx)
	}
	customClaims, clErr := mutation.GetAuthMeta().ExtractCustomClaims(ctx)
	if clErr == nil && customClaims.AuthVariables != nil {
		authCtx.AuthVariables = customClaims.AuthVariables
	}
	if authCtx.AuthVariables == nil {
		authCtx.AuthVariables = map[string]interface{}{}
	}

	// Populate the struct helper functions using the exported constructor so that
	// the unexported schema-package helpers (callLambda, hashSHA256, …) are accessible.
	helpers := schema.NewPostValidateExprHelpers(authCtx)

	// Pass the populated struct — must match the postValidateEnv type used at compile time.
	evalEnv := postValidateEnv{
		Nodes:                nodes,
		Action:               action,
		Auth:                 authCtx.AuthVariables,
		CallLambda:           helpers["callLambda"].(func(string, map[string]interface{}) (interface{}, error)),
		UUID:                 helpers["uuid"].(func() string),
		Sha256:               helpers["sha256"].(func(string) string),
		GenerateEmbedding:    helpers["generateEmbedding"].(func(string, string, string, map[string]any) []float32),
		DiffMap:              helpers["diffMap"].(func(map[string]interface{}, map[string]interface{}) (map[string]interface{}, error)),
		MapStringWithoutKeys: helpers["mapStringWithoutKeys"].(func(map[string]interface{}, []interface{}) map[string]interface{}),
		Error:                helpers["error"].(func(interface{}) (interface{}, error)),
	}

	exprResult, runErr := expr.Run(prog, evalEnv)
	if runErr != nil {
		return errors.Wrapf(runErr, "@postValidate on type %s: expression error", typ.Name())
	}

	passed, ok := exprResult.(bool)
	if !ok || !passed {
		msg := fmt.Sprintf("@postValidate on type %s failed", typ.Name())
		if cfg.Reason != "" {
			msg = cfg.Reason
		}
		return errors.New(msg)
	}
	return nil
}

// buildPostValidateEnv is intentionally not used at compile time (see runPostValidate).
// Kept as documentation of the runtime variable shape passed to expr.Run.
//
//	nodes  []map[string]interface{}  — each element: {uid, before, after}
//	auth   map[string]interface{}    — JWT claim variables; {} if no auth
//	action string                    — "add" or "update"
func buildPostValidateEnv() map[string]interface{} {
	nodeShape := map[string]interface{}{
		"uid":    "",
		"before": map[string]interface{}{},
		"after":  map[string]interface{}{},
	}
	return map[string]interface{}{
		"nodes":  []interface{}{nodeShape},
		"auth":   map[string]interface{}{},
		"action": "",
	}
}

// normalizePredicateKeys converts Dgraph predicate-namespaced keys in a map to bare
// GraphQL field names so that CEL expressions can use n.after.rating / n.before.comment
// instead of n.after["Review.rating"]. This applies to both the pre-mutation (before)
// state from the existence query and the post-mutation (after) state fetched by
// runPostValidate.
//
// Transformation rules:
//   - "<dgraphTypeName>.<field>"  →  "<field>"   (e.g. "Review.rating" → "rating")
//   - "uid", "dgraph.type", etc. are kept unchanged (no matching prefix)
func normalizePredicateKeys(m map[string]interface{}, dgraphTypeName string) map[string]interface{} {
	if len(m) == 0 {
		return m
	}
	prefix := dgraphTypeName + "."
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		if strings.HasPrefix(k, prefix) {
			out[k[len(prefix):]] = v
		} else {
			out[k] = v
		}
	}
	return out
}

// collectPostValidateUIDs gathers all UIDs of the validated type from the mutation response
// and returns both the UID list and an inverted map (UID → blank-node variable name).
//
// The inverted map is used to join each fetched after-node to its per-node before state
// in variableOldValueMap.
//
// Blank nodes follow the naming scheme "TypeName_N" (e.g. "Post_1", "Post_2") generated
// by VariableGenerator.Next. Filtering mutResp.GetUids() by this prefix captures every
// node of the type created anywhere in the mutation tree (root or nested) without
// tree traversal — including nested same-type objects at any depth.
//
// For update mutations, root-level updated UIDs (existing nodes matched by the filter,
// not in GetUids()) are also collected from the Dgraph result map. Their before state
// is NOT individually keyed — it lives under the shared "xx" key as a merged map.
func collectPostValidateUIDs(
	dgraphTypeName string,
	mutation schema.Mutation,
	mutResp *dgoapi.Response,
	result map[string]interface{},
) ([]string, map[string]string) {
	seenUID := make(map[string]bool)
	var uids []string
	// uidToBlankName: inverted map for before-state join.
	uidToBlankName := make(map[string]string)

	// Blank-node prefix for this type (e.g. "Post_" for type Post).
	prefix := dgraphTypeName + "_"

	// Newly-assigned UIDs: covers add (root + nested) and nested-add-within-update.
	// This correctly handles same-type objects at any nesting depth because the
	// VariableGenerator assigns a unique blank-node name per object regardless of depth.
	for blankName, uid := range mutResp.GetUids() {
		if uid != "" && strings.HasPrefix(blankName, prefix) && !seenUID[uid] {
			seenUID[uid] = true
			uids = append(uids, uid)
			uidToBlankName[uid] = blankName
		}
	}

	// For update mutations, root nodes are existing (not in GetUids()).
	// Their before state is stored under "xx" (merged across all filter-matched nodes).
	if mutation.MutationType() == schema.UpdateMutation {
		for _, uid := range extractMutated(result, mutation.Name()) {
			if uid != "" && !seenUID[uid] {
				seenUID[uid] = true
				uids = append(uids, uid)
				// No individual blank-node name — before falls back to merged "xx" entry.
			}
		}
	}

	return uids, uidToBlankName
}

// mustParseUID converts a hex-string UID (e.g. "0x1a") to uint64.
// Returns 0 on parse failure (the block will simply return no results).
func mustParseUID(uid string) uint64 {
	uid = strings.TrimPrefix(uid, "0x")
	v, _ := strconv.ParseUint(uid, 16, 64)
	return v
}
