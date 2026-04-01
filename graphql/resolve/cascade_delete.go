/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	dgoapi "github.com/dgraph-io/dgo/v250/protos/api"
	exprlib "github.com/expr-lang/expr"
	"github.com/hypermodeinc/dgraph/v25/dql"
	"github.com/hypermodeinc/dgraph/v25/graphql/dgraph"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
)

// CascadeDeleteCollector pre-queries and collects all nodes that must be deleted
// as part of a @cascadeDelete chain rooted at the given UIDs. It runs before the
// main Dgraph delete mutation and appends additional {uid: ...} entries to the
// caller-provided deletes slice so everything is removed in one transaction.
//
// authVariables comes from the request JWT and is used to enforce authMode on
// each cascaded child type.
func CascadeDeleteCollector(
	ctx context.Context,
	executor DgraphExecutor,
	auth schema.AuthCtx,
	mutatedType schema.Type,
	rootUIDs []string,
	deletes []interface{},
) ([]interface{}, error) {
	if len(rootUIDs) == 0 {
		return deletes, nil
	}

	// visited tracks UIDs already scheduled for deletion to prevent duplicates / cycles.
	visited := make(map[string]bool, len(rootUIDs))
	for _, uid := range rootUIDs {
		visited[uid] = true
	}

	var collect func(typ schema.Type, uids []string, remainingDepth int) error
	collect = func(typ schema.Type, uids []string, remainingDepth int) error {
		if len(uids) == 0 || remainingDepth == 0 {
			return nil
		}

		fields := typ.CascadeDeleteFields()
		if len(fields) == 0 {
			return nil
		}

		// Build a DQL query to fetch linked UIDs for each @cascadeDelete field.
		// Use DgraphPredicate() (not DgraphAlias()) because the predicate stored in
		// Dgraph for interface-inherited fields uses the interface name as prefix
		// (e.g. "Recordable.hasCreateRecord"), while DgraphAlias() naively uses
		// the concrete type name ("Note.hasCreateRecord") which yields no results.
		fieldSelects := make([]string, 0, len(fields))
		for _, f := range fields {
			fieldSelects = append(fieldSelects, fmt.Sprintf("%s { uid }", f.DgraphPredicate()))
		}
		query := fmt.Sprintf("{ q(func: uid(%s)) { uid %s } }",
			strings.Join(uids, ", "),
			strings.Join(fieldSelects, " "))

		resp, err := executor.Execute(ctx, &dgoapi.Request{Query: query, ReadOnly: true}, nil)
		if err != nil {
			return fmt.Errorf("cascadeDelete pre-query failed: %w", err)
		}

		var raw struct {
			Q []map[string]interface{} `json:"q"`
		}
		if err := json.Unmarshal(resp.GetJson(), &raw); err != nil {
			return fmt.Errorf("cascadeDelete pre-query unmarshal: %w", err)
		}

		for _, f := range fields {
			cfg := f.CascadeDeleteConfig()
			if cfg == nil {
				continue
			}

			// Per-field depth: the min of the field's own depth limit and the remaining budget.
			nextDepth := remainingDepth
			if cfg.Depth > 0 && (nextDepth < 0 || cfg.Depth < nextDepth) {
				nextDepth = cfg.Depth
			}
			if nextDepth > 0 {
				nextDepth--
			}

			// Collect candidate UIDs from the query result for this field.
			var candidateUIDs []string
			dgPred := f.DgraphPredicate() // e.g. "NoteOwner.hasNote" or "Group.hasCandidate"

			// The orphan-check scope must be the type that DECLARES the predicate
			// (the interface or concrete type whose name prefixes the DQL predicate),
			// NOT the concrete type currently being iterated.  Using typ.Name() here
			// would give "Candidate" for an interface-inherited "NoteOwner.hasNote"
			// predicate, causing type(Candidate) to miss Company and other NoteOwner
			// implementors.  Extracting from dgPred gives the correct "NoteOwner".
			orphanCheckType := strings.SplitN(dgPred, ".", 2)[0]

			for _, parentNode := range raw.Q {
				// The DQL response for a singular edge (e.g. hasCreateRecord: CreateRecord)
				// is a plain map[string]interface{}, NOT []interface{}.
				// Normalise both cases so the loop below handles them uniformly.
				var arr []interface{}
				switch v := parentNode[dgPred].(type) {
				case []interface{}:
					arr = v
				case map[string]interface{}:
					arr = []interface{}{v}
				}
				for _, item := range arr {
					child, ok := item.(map[string]interface{})
					if !ok {
						continue
					}
					uid, _ := child["uid"].(string)
					if uid == "" || visited[uid] {
						continue
					}

					// Apply CEL filter if configured.
					if cfg.Filter != "" {
						env := schema.NewExprEvaluationContext(f.Type().Name(), child, nil, nil, auth, "delete")
						prog, err := exprlib.Compile(cfg.Filter, exprlib.Env(env.As()))
						if err == nil {
							result, err := exprlib.Run(prog, env.As())
							if err == nil {
								if b, ok := result.(bool); ok && !b {
									continue // filter returned false — skip this child
								}
							}
						}
					}

					// Apply orphan check if configured.
					if cfg.OnlyIfOrphan {
						parentUID, _ := parentNode["uid"].(string)
						orphan, err := isOrphanNode(ctx, executor, uid, dgPred, orphanCheckType, parentUID, cfg.OnlyIfOrphanScope)
						if err != nil {
							return err
						}
						if !orphan {
							continue
						}
					}

					candidateUIDs = append(candidateUIDs, uid)
				}
			}

			if len(candidateUIDs) == 0 {
				continue
			}

			// Apply auth mode to determine which UIDs are actually deletable.
			authorizedUIDs, err := applyAuthMode(ctx, executor, auth, f.Type(), candidateUIDs, cfg.AuthMode)
			if err != nil {
				return fmt.Errorf("authMode %q check failed for %s: %w", cfg.AuthMode, f.Type().Name(), err)
			}

			for _, uid := range authorizedUIDs {
				visited[uid] = true
				deletes = append(deletes, map[string]interface{}{"uid": uid})
			}

			// Recurse into the child type.
			if len(authorizedUIDs) > 0 {
				if err := collect(f.Type(), authorizedUIDs, nextDepth); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := collect(mutatedType, rootUIDs, -1); err != nil {
		return nil, err
	}

	return deletes, nil
}

// applyAuthMode filters candidateUIDs according to the configured authMode:
//   - "skip"    → return all UIDs unchanged (no auth check)
//   - "enforce" → check auth; return error if any UID fails
//   - "filter"  → return only UIDs the caller is authorized to delete
func applyAuthMode(
	ctx context.Context,
	executor DgraphExecutor,
	auth schema.AuthCtx,
	typ schema.Type,
	candidateUIDs []string,
	authMode string,
) ([]string, error) {
	if authMode == "skip" || authMode == "" {
		return candidateUIDs, nil
	}

	authRw := &authRewriter{
		authVariables: auth.AuthVariables,
		varGen:        NewVariableGenerator(),
		selector:      deleteAuthSelector,
		parentVarName: typ.Name() + "Root",
		hasAuthRules:  true,
	}

	// Static RBAC evaluation — no Dgraph query needed.
	rn := authRw.selector(typ)
	if rn == nil {
		// No @auth(delete:...) on this type — all UIDs pass.
		return candidateUIDs, nil
	}
	rbac := rn.EvaluateStatic(auth.AuthVariables)
	if rbac == schema.Positive {
		return candidateUIDs, nil
	}
	if rbac == schema.Negative {
		if authMode == "enforce" {
			return nil, fmt.Errorf("not authorized to delete %s nodes", typ.Name())
		}
		return nil, nil // "filter" mode: skip all
	}

	// Dynamic auth — query Dgraph.
	authorizedUIDs, err := queryAuthorizedUIDs(ctx, executor, authRw, typ, candidateUIDs)
	if err != nil {
		return nil, err
	}

	if authMode == "enforce" && len(authorizedUIDs) < len(candidateUIDs) {
		return nil, fmt.Errorf(
			"not authorized to cascade-delete all %s nodes: %d of %d passed auth rules",
			typ.Name(), len(authorizedUIDs), len(candidateUIDs))
	}

	return authorizedUIDs, nil
}

// queryAuthorizedUIDs runs a Dgraph auth query to find which of the given UIDs
// the current identity is authorized to delete, using the type's @auth(delete:...)
// rules via the delete auth selector.
func queryAuthorizedUIDs(
	ctx context.Context,
	executor DgraphExecutor,
	authRw *authRewriter,
	typ schema.Type,
	candidateUIDs []string,
) ([]string, error) {
	varName := authRw.varGen.Next(typ, "", "", false)
	authRw.varName = varName

	authQueries, authFilter := authRw.rewriteAuthQueries(typ)
	if len(authQueries) == 0 {
		return candidateUIDs, nil
	}

	// Parse UIDs to uint64 for the DQL query builder.
	uidNums := make([]uint64, 0, len(candidateUIDs))
	for _, u := range candidateUIDs {
		parsed, err := strconv.ParseUint(strings.TrimPrefix(u, "0x"), 16, 64)
		if err != nil {
			// Try decimal.
			parsed, err = strconv.ParseUint(u, 10, 64)
			if err != nil {
				continue
			}
		}
		uidNums = append(uidNums, parsed)
	}

	if len(uidNums) == 0 {
		return nil, nil
	}

	// Build the auth-filtered query:
	// TypeName(func: uid(candidates)) @filter(uid(AuthVar1) AND uid(AuthVar2)) { uid }
	// AuthVar1 as var(func: uid(candidates)) @cascade { ...auth query 1... }
	mainQry := &dql.GraphQuery{
		Attr: typ.Name(),
		Func: &dql.Function{
			Name: "uid",
			UID:  uidNums,
		},
		Filter:   authFilter,
		Children: []*dql.GraphQuery{{Attr: "uid"}},
	}
	varQry := &dql.GraphQuery{
		Var:  varName,
		Attr: "var",
		Func: &dql.Function{Name: "uid", UID: uidNums},
	}

	allQrys := append([]*dql.GraphQuery{mainQry, varQry}, authQueries...)
	queryStr := dgraph.AsString(allQrys)

	resp, err := executor.Execute(ctx, &dgoapi.Request{Query: queryStr, ReadOnly: true}, nil)
	if err != nil {
		return nil, fmt.Errorf("auth query for cascade delete failed: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(resp.GetJson(), &result); err != nil {
		return nil, err
	}

	// Collect the UIDs returned by the auth-filtered query.
	var authorized []string
	arr, _ := result[typ.Name()].([]interface{})
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if uid, ok := m["uid"].(string); ok && uid != "" {
			authorized = append(authorized, uid)
		}
	}
	return authorized, nil
}

// BuildCascadePreQuery takes the upsert query blocks produced by deleteRewriter and
// returns an augmented slice that can be executed read-only to resolve the concrete
// root UIDs before the delete runs.
//
// The delete rewriter assigns variables at two levels:
//
//	x as deleteNote(func: type(Note)) @filter(...) {    ← root var "x"
//	    uid
//	    ClientData_6 as ClientDataOwner.hasClientData   ← child var
//	    Tag_7        as Tagable.hasTag                  ← child var
//	}
//
// Running this standalone fails with "variable x defined but not used" because
// uid(x) only appears in the mutation JSON. We fix this by adding a SINGLE
// "cascadeRoots" consumer block that references ALL defined variables in its uid()
// function argument list, while a @filter(type(rootTypeName)) ensures only the
// root-type UIDs are returned. Child vars (ClientData_6, Tag_7, …) hold non-Note
// UIDs so the type filter weeds them out automatically.
//
//	cascadeRoots(func: uid(x, NoteRoot, ClientData_6, …)) @filter(type(Note)) { uid }
//
// rootVar is the DQL variable name for the delete block (MutationQueryVar = "x").
// rootTypeName is the GraphQL type being deleted (e.g. "Note").
func BuildCascadePreQuery(queryBlocks []*dql.GraphQuery, rootVar, rootTypeName string) []*dql.GraphQuery {
	// Collect ALL variable names defined anywhere in the query tree.
	var allVars []string
	var collectVars func(blocks []*dql.GraphQuery)
	collectVars = func(blocks []*dql.GraphQuery) {
		for _, b := range blocks {
			if b.Var != "" {
				allVars = append(allVars, b.Var)
			}
			collectVars(b.Children)
		}
	}
	collectVars(queryBlocks)

	if len(allVars) == 0 {
		return queryBlocks
	}

	// Build ONE consumer block that references all vars in uid(…) and filters
	// to the root type so only root UIDs are returned.
	consumerFunc := &dql.Function{Name: "uid"}
	for _, varName := range allVars {
		consumerFunc.Args = append(consumerFunc.Args, dql.Arg{Value: varName, IsDQLVar: true})
		consumerFunc.NeedsVar = append(consumerFunc.NeedsVar, dql.VarContext{Name: varName, Typ: dql.UidVar})
	}
	cascadeBlock := &dql.GraphQuery{
		Attr: "cascadeRoots",
		Func: consumerFunc,
		Filter: &dql.FilterTree{
			Func: &dql.Function{
				Name: "type",
				Args: []dql.Arg{{Value: rootTypeName}},
			},
		},
		Children: []*dql.GraphQuery{{Attr: "uid"}},
	}

	return append(queryBlocks, cascadeBlock)
}

// isOrphanNode returns true if no other node (besides the one being
// deleted, identified by parentUID) references childUID via the given predicate.
//
// predicate is the DQL predicate name (e.g. "Group.hasCandidate").
// parentTypeName is the type name of the deleting node (e.g. "Group").
// parentUID is the UID of the node currently being deleted; it is excluded
// from the incoming-reference count so we don't count the edge we're about
// to remove.
// scope="type" (default): only count references from nodes of parentTypeName.
// scope="all": count references from any node type.
func isOrphanNode(
	ctx context.Context,
	executor DgraphExecutor,
	childUID string,
	predicate string,
	parentTypeName string,
	parentUID string,
	scope string,
) (bool, error) {
	// Build a DQL query that counts how many OTHER nodes (not the one being
	// deleted) still reference childUID via the given predicate.
	//
	// uid_in(predicate, childUID) in a @filter selects nodes where the predicate
	// has childUID as one of its values — i.e. nodes that point to childUID.
	var query string
	if scope == "all" {
		// Any node type, excluding the node being deleted.
		query = fmt.Sprintf(
			`{ ref(func: has(%s)) @filter(uid_in(%s, %s) AND NOT uid(%s)) { cnt: count(uid) } }`,
			predicate, predicate, childUID, parentUID)
	} else {
		// Only nodes of the parent type, excluding the node being deleted.
		query = fmt.Sprintf(
			`{ ref(func: type(%s)) @filter(uid_in(%s, %s) AND NOT uid(%s)) { cnt: count(uid) } }`,
			parentTypeName, predicate, childUID, parentUID)
	}

	resp, err := executor.Execute(ctx, &dgoapi.Request{Query: query, ReadOnly: true}, nil)
	if err != nil {
		return false, fmt.Errorf("orphan check query failed: %w", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(resp.GetJson(), &raw); err != nil {
		return false, err
	}

	refArr, _ := raw["ref"].([]interface{})
	for _, item := range refArr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		var cnt float64
		switch v := m["cnt"].(type) {
		case float64:
			cnt = v
		case string:
			cnt, _ = strconv.ParseFloat(v, 64)
		}
		if cnt > 0 {
			return false, nil // still referenced — not an orphan
		}
	}
	return true, nil
}
