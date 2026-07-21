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

	"github.com/golang/glog"
	"github.com/pkg/errors"

	"github.com/hypermodeinc/dgraph/v25/dql"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/x"
)

type queryRewriter struct{}

type cascadeCacheKey struct {
	rn      *schema.RuleNode
	typName string
}

type authRewriter struct {
	authVariables map[string]interface{}
	isWritingAuth bool
	// forceForward instructs the compiler to dynamically override CascadeWrapReverse
	// and force the FORWARD strategy (uid_in) for single-resource contexts (e.g. mutations, get).
	forceForward bool
	// `filterByUid` is used to when we have to rewrite top level query with uid function. The
	// variable name is passed in `varName`. If true it will rewrite as following:
	// queryType(uid(varName)) {
	// Once such case is when we perform query in delete mutation.
	filterByUid bool
	selector    func(t schema.Type) *schema.RuleNode
	varGen      *VariableGenerator
	varName     string
	// `parentVarName` is used to link a query with it's previous level.
	parentVarName string
	// `hasAuthRules` indicates if any of fields in the complete query hierarchy has auth rules.
	hasAuthRules bool
	// `hasCascade` indicates if any of fields in the complete query hierarchy has cascade directive.
	hasCascade bool
	// `cascadeVarCache` deduplicates cascade authority vars within a single request.
	// Key: cascadeCacheKey (combining RuleNode pointer and type name to avoid cross-type collisions).
	// Value: DQL var name of the already-generated cascade authority var block.
	// On a cache hit, the cascade edge returns a uid_in filter referencing the existing var.
	// Initialized once at the root Rewrite() call and shared (by reference) across all
	// derived authRewriter instances within the same request.
	cascadeVarCache map[cascadeCacheKey]string
	// `mutVarCache` caches predicate → DQL variable name mappings used during mutation
	// rewriting to avoid emitting duplicate auth var blocks within a single mutation pass.
	// Distinct from cascadeVarCache: this is mutation-specific and uses a string key.
	mutVarCache *map[string]string
	// `cascadeAuthorityType` is set when rewriteRuleNode is processing the inner OR/AND tree
	// of a CascadeWrap node (Case C). It holds the Dgraph type name of the cascade authority
	// (e.g. "Group" for a CascadeWrap{pred: "Groupable.inGroup", type: "Group"}).
	//
	// Plain Rule-leaf nodes compiled inside this context must use
	//   var(func: type(cascadeAuthorityType))
	// rather than the default
	//   var(func: uid(parentVarName))
	// because the authority type's auth rules (e.g. Group's IAMResource merge) must be
	// evaluated against Group nodes — not against the child type's nodes (JobAd).
	// Group UIDs and JobAd UIDs are disjoint, so using uid(JobAd_1) would always produce
	// zero results when used as a filter inside var(func: type(Group)).
	//
	// This field is reset to the new CascadeWrapType each time a nested CascadeWrap is
	// entered, ensuring each level of the cascade chain uses the correct Dgraph type.
	cascadeAuthorityType string
}

// The struct is used as a return type for buildCommonAuthQueries function.
type commonAuthQueryVars struct {
	// Stores queries of the form
	// var(func: uid(Ticket)) {
	//		User as Ticket.assignedTo
	// }
	parentQry *dql.GraphQuery
	// Stores queries which aggregate filters and auth rules. Eg.
	// // User6 as var(func: uid(User2), orderasc: ...) @filter((eq(User.username, "User1") AND (...Auth Filter))))
	selectionQry *dql.GraphQuery
}

// NewQueryRewriter returns a new QueryRewriter.
func NewQueryRewriter() QueryRewriter {
	return &queryRewriter{}
}

func hasAuthRules(field schema.Field, authRw *authRewriter) bool {
	if field == nil {
		return false
	}

	rn := authRw.selector(field.ConstructedFor())
	if rn != nil {
		return true
	}

	for _, childField := range field.SelectionSet() {
		if authRules := hasAuthRules(childField, authRw); authRules {
			return true
		}
	}
	return false
}

func hasCascadeDirective(field schema.Field) bool {
	if c := field.Cascade(); c != nil {
		return true
	}

	for _, childField := range field.SelectionSet() {
		if res := hasCascadeDirective(childField); res {
			return true
		}
	}
	return false
}

// Returns the auth selector to be used depending on the query type.
func getAuthSelector(queryType schema.QueryType) func(t schema.Type) *schema.RuleNode {
	if queryType == schema.PasswordQuery {
		return passwordAuthSelector
	}
	return queryAuthSelector
}

// Rewrite rewrites a GraphQL query into DQL.
func (qr *queryRewriter) Rewrite(
	ctx context.Context,
	gqlQuery schema.Query) ([]*dql.GraphQuery, error) {

	customClaims, err := gqlQuery.GetAuthMeta().ExtractCustomClaims(ctx)
	if err != nil {
		return nil, err
	}

	authRw := &authRewriter{
		authVariables:   customClaims.AuthVariables,
		varGen:          NewVariableGenerator(),
		selector:        getAuthSelector(gqlQuery.QueryType()),
		parentVarName:   gqlQuery.ConstructedFor().Name() + "Root",
		cascadeVarCache: make(map[cascadeCacheKey]string),
		forceForward:    gqlQuery.QueryType() == schema.GetQuery || gqlQuery.QueryType() == schema.SimilarByIdQuery,
	}
	authRw.hasAuthRules = hasAuthRules(gqlQuery, authRw)
	authRw.hasCascade = hasCascadeDirective(gqlQuery)

	switch gqlQuery.QueryType() {
	case schema.GetQuery:

		// TODO: The only error that can occur in query rewriting is if an ID argument
		// can't be parsed as a uid: e.g. the query was something like:
		//
		// getT(id: "HI") { ... }
		//
		// But that's not a rewriting error!  It should be caught by validation
		// way up when the query first comes in.  All other possible problems with
		// the query are caught by validation.
		// ATM, I'm not sure how to hook into the GraphQL validator to get that to happen
		xid, uid, err := gqlQuery.IDArgValue()
		if err != nil {
			return nil, err
		}

		dgQuery := rewriteAsGet(gqlQuery, uid, xid, authRw)
		return dgQuery, nil
	case schema.SimilarByIdQuery:
		xid, uid, err := gqlQuery.IDArgValue()
		if err != nil {
			return nil, err
		}
		return rewriteAsSimilarByIdQuery(gqlQuery, uid, xid, authRw), nil
	case schema.SimilarByEmbeddingQuery:
		return rewriteAsSimilarByEmbeddingQuery(gqlQuery, authRw), nil
	case schema.FilterQuery:
		return rewriteAsQuery(gqlQuery, authRw, gqlQuery.Alias()), nil
	case schema.PasswordQuery:
		return passwordQuery(gqlQuery, authRw)
	case schema.AggregateQuery:
		return aggregateQuery(gqlQuery, authRw), nil
	case schema.GroupByQuery:
		return groupByQuery(gqlQuery, authRw)
	case schema.EntitiesQuery:
		return entitiesQuery(gqlQuery, authRw)
	default:
		return nil, errors.Errorf("unimplemented query type %s", gqlQuery.QueryType())
	}
}

// entitiesQuery rewrites the Apollo `_entities` Query which is sent from the Apollo gateway to a DQL query.
// This query is sent to the Dgraph service to resolve types `extended` and defined by this service.
func entitiesQuery(field schema.Query, authRw *authRewriter) ([]*dql.GraphQuery, error) {

	// Input Argument to the Query is a List of "__typename" and "keyField" pair.
	// For this type Extension:-
	// 	extend type Product @key(fields: "upc") {
	// 		upc: String @external
	// 		reviews: [Review]
	// 	}
	// Input to the Query will be
	// "_representations": [
	// 		{
	// 		  "__typename": "Product",
	// 	 	 "upc": "B00005N5PF"
	// 		},
	// 		...
	//   ]

	parsedRepr, err := field.RepresentationsArg()
	if err != nil {
		return nil, err
	}

	typeDefn := parsedRepr.TypeDefn
	rbac := authRw.evaluateStaticRules(typeDefn)

	dgQuery := &dql.GraphQuery{
		Attr: field.Name(),
	}

	if rbac == schema.Negative {
		dgQuery.Attr = dgQuery.Attr + "()"
		return []*dql.GraphQuery{dgQuery}, nil
	}

	// Construct Filter at Root Func.
	// if keyFieldsIsID = true and keyFieldValueList = {"0x1", "0x2"}
	// then query will be formed as:-
	// 	_entities(func: uid("0x1", "0x2") {
	//		...
	//	}
	// if keyFieldsIsID = false then query will be like:-
	// 	_entities(func: eq(keyFieldName,"0x1", "0x2") {
	//		...
	//	}

	// If the key field is of ID type and is not an external field
	// then we query it using the `uid` otherwise we treat it as string
	// and query using `eq` function.
	// We also don't need to add Order to the query as the results are
	// automatically returned in the ascending order of the uids.
	if parsedRepr.KeyField.IsID() && !parsedRepr.KeyField.IsExternal() {
		addUIDFunc(dgQuery, convertIDs(parsedRepr.KeyVals))
	} else {
		addEqFunc(dgQuery, typeDefn.DgraphPredicate(parsedRepr.KeyField.Name()), parsedRepr.KeyVals)
		// Add the  ascending Order of the keyField in the query.
		// The result will be converted into the exact in the resultCompletion step.
		dgQuery.Order = append(dgQuery.Order,
			&pb.Order{Attr: typeDefn.DgraphPredicate(parsedRepr.KeyField.Name())})
	}
	// AddTypeFilter in as the Filter to the Root the Query.
	// Query will be like :-
	// 	_entities(func: ...) @filter(type(typeName)) {
	//		...
	// 	}
	addTypeFilter(dgQuery, typeDefn)

	selectionAuth := addSelectionSetFrom(dgQuery, field, authRw)
	addUID(dgQuery)

	dgQueries, authVarSubst := authRw.addAuthQueries(typeDefn, []*dql.GraphQuery{dgQuery}, rbac)
	// Dedup field-level auth var blocks (selectionAuth) and merge any new substitutions
	// into authVarSubst so the combined map covers both root-auth and field-auth dedup.
	selectionAuth, authVarSubst = deduplicateSelectionAuth(selectionAuth, dgQueries, authVarSubst)
	applyAuthVarSubstToQueries(selectionAuth, authVarSubst)
	return append(dgQueries, selectionAuth...), nil

}

func aggregateQuery(query schema.Query, authRw *authRewriter) []*dql.GraphQuery {

	// Get the type which the count query is written for
	mainType := query.ConstructedFor()

	dgQuery, rbac := addCommonRules(query, mainType, authRw)
	if rbac == schema.Negative {
		return dgQuery
	}

	// Add filter
	filter, _ := query.ArgValue("filter").(map[string]interface{})
	_, varQry := addFilter(dgQuery[0], mainType, filter, authRw, query.Alias())
	dgQuery = append(dgQuery, varQry...)

	dgQuery, _ = authRw.addAuthQueries(mainType, dgQuery, rbac)

	// dgQuery[0] is the main var block (func: uid(XRoot) once auth is injected).
	// dgQuery[1:] contains filter path var blocks, rootQry (defines XRoot),
	// varQry (defines X_N), and fldAuthQueries.
	//
	// DQL requires variable definitions before uses. Since mainQuery uses XRoot
	// (via uid(XRoot)), and rootQry defines XRoot, the mainQuery MUST come
	// after the rest of dgQuery[1:] in the output.
	//
	// Correct output order:
	//   finalMainQuery    ← non-var aggregation result collector
	//   dgQuery[1:]...    ← filterVarQrys + rootQry + varQry + fldAuthQueries (defines XRoot)
	//   dgQuery[0]        ← mainQuery var block (uses XRoot)
	mainQuery := dgQuery[0]

	// Changing mainQuery Attr name to var. This is used in the final aggregate<Type> query.
	mainQuery.Attr = "var"

	finalMainQuery := &dql.GraphQuery{
		Attr: query.DgraphAlias() + "()",
	}
	// Add selection set to mainQuery and finalMainQuery.
	isAggregateVarAdded := make(map[string]bool)
	isCountVarAdded := false

	for _, f := range query.SelectionSet() {
		// fldName stores Name of the field f.
		fldName := f.Name()
		if fldName == "count" {
			if !isCountVarAdded {
				child := &dql.GraphQuery{
					Var:  "countVar",
					Attr: "count(uid)",
				}
				mainQuery.Children = append(mainQuery.Children, child)
				isCountVarAdded = true
			}
			finalQueryChild := &dql.GraphQuery{
				Alias: f.DgraphAlias(),
				Attr:  "max(val(countVar))",
			}
			finalMainQuery.Children = append(finalMainQuery.Children, finalQueryChild)
			continue
		}

		// Handle other aggregate functions than count
		aggregateFunctions := []string{"Max", "Min", "Sum", "Avg"}

		for _, function := range aggregateFunctions {
			// A field can have at maximum one of the aggregation functions as suffix
			if strings.HasSuffix(fldName, function) {
				// constructedForDgraphPredicate stores the Dgraph predicate for which aggregate function has been queried.
				constructedForDgraphPredicate := f.DgraphPredicateForAggregateField()
				// constructedForField contains the field for which aggregate function has been queried.
				// As all aggregate functions have length 3, removing last 3 characters from fldName.
				constructedForField := fldName[:len(fldName)-3]
				// isAggregateVarAdded ensures that a field is added to Var query at maximum once.
				// If a field has already been added to the var query, don't add it again.
				// Eg. Even if scoreMax and scoreMin are queried, the query will contain only one expression
				// of the from, "scoreVar as Tweets.score"
				if !isAggregateVarAdded[constructedForField] {
					child := &dql.GraphQuery{
						Var:  constructedForField + "Var",
						Attr: constructedForDgraphPredicate,
					}
					// The var field is added to mainQuery. This adds the following DQL query.
					// var(func: type(Tweets)) {
					//        scoreVar as Tweets.score
					// }

					mainQuery.Children = append(mainQuery.Children, child)
					isAggregateVarAdded[constructedForField] = true
				}
				finalQueryChild := &dql.GraphQuery{
					Alias: f.DgraphAlias(),
					Attr:  strings.ToLower(function) + "(val(" + constructedForField + "Var))",
				}
				// This adds the following DQL query
				// aggregateTweets() {
				//        TweetsAggregateResult.scoreMin : min(val(scoreVar))
				// }
				finalMainQuery.Children = append(finalMainQuery.Children, finalQueryChild)
				break
			}
		}
	}

	// Emit: [finalMainQuery, dgQuery[1:]...(defines XRoot), dgQuery[0]/mainQuery(uses XRoot)]
	// This ensures XRoot is defined before it is used in the main var block.
	authAndFilterQrys := dgQuery[1:] // rootQry, varQry, fldAuthQueries, filterVarQrys
	result := make([]*dql.GraphQuery, 0, 1+len(authAndFilterQrys)+1)
	result = append(result, finalMainQuery)
	result = append(result, authAndFilterQrys...)
	result = append(result, mainQuery)
	return result
}

// resolveGroupByPath walks a parsed XxxGroupByField object value to find the selected leaf.
// At each level exactly one key should be set (multiple are technically allowed by GraphQL
// but the validator rejects them before execution reaches here).
//
// Returns:
//   - pathSegments: GraphQL field-name segments, e.g. ["hasStatus", "stage", "name"]
//   - dgraphPreds: DQL predicate strings for each segment,
//     e.g. ["Application.hasStatus", "ApplicationStatus.stage", "ApplicationStage.name"]
//   - err: non-nil if the path is invalid (unknown field, empty object)
func resolveGroupByPath(
	fieldObj map[string]interface{},
	typeDef schema.Type,
) (pathSegments []string, dgraphPreds []string, traversedTypes []schema.Type, err error) {
	currentTypeObj := typeDef
	current := fieldObj
	for {
		if len(current) > 1 {
			var keys []string
			for k := range current {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return nil, nil, nil, errors.Errorf("groupBy field object must contain exactly one field, but found %d fields: %s", len(current), strings.Join(keys, ", "))
		}

		var chosenKey string
		var chosenVal interface{}
		for k, v := range current {
			chosenKey = k
			chosenVal = v
			break // take the first (and only valid) key
		}
		if chosenKey == "" {
			return nil, nil, nil, errors.Errorf("groupBy field object is empty")
		}

		pathSegments = append(pathSegments, chosenKey)
		dgPred := currentTypeObj.DgraphPredicate(chosenKey)
		dgraphPreds = append(dgraphPreds, dgPred)

		// Is the value a boolean? → this is the terminal leaf selector.
		switch v := chosenVal.(type) {
		case bool:
			if !v {
				return nil, nil, nil, errors.Errorf("groupBy field %q selector must be true", chosenKey)
			}
			return pathSegments, dgraphPreds, traversedTypes, nil
		case map[string]interface{}:
			// Navigate into the next type via FieldDefinition.Type().
			fldDef := currentTypeObj.Field(chosenKey)
			if fldDef == nil {
				return nil, nil, nil, errors.Errorf("groupBy: unknown field %q on type %s",
					chosenKey, currentTypeObj.Name())
			}
			nextType := fldDef.Type()
			if nextType == nil {
				return nil, nil, nil, errors.Errorf("groupBy: field %q has no resolvable type", chosenKey)
			}
			currentTypeObj = nextType
			current = v
			traversedTypes = append(traversedTypes, currentTypeObj)
		default:
			return nil, nil, nil, errors.Errorf("groupBy: unexpected value type for field %q", chosenKey)
		}
	}
}

// buildLeafUIDVarBlock builds the DQL var() block that collects the leaf-type UIDs
// by traversing the edge path from the root set. The emitted DQL looks like:
//
//	var(func: uid(CompanyRoot)) {
//	  Company.hasStatus {
//	    __gby_0_leafUIDs as uid
//	  }
//	}
//
// The variable __gby_0_leafUIDs then becomes the func: uid() argument of the main query.
func buildLeafUIDVarBlock(rootVar string, edgePath []string, leafUIDVarName string) *dql.GraphQuery {
	// Build from the inside out.
	innermost := &dql.GraphQuery{
		Var:  leafUIDVarName,
		Attr: "uid",
	}

	current := innermost
	for i := len(edgePath) - 1; i >= 0; i-- {
		wrapper := &dql.GraphQuery{
			Attr:     edgePath[i],
			Children: []*dql.GraphQuery{current},
		}
		current = wrapper
	}

	return &dql.GraphQuery{
		Attr:     "var",
		Func:     &dql.Function{Name: "uid", Args: []dql.Arg{{Value: rootVar}}},
		Children: []*dql.GraphQuery{current},
	}
}

// buildValueVarBlock builds the DQL var() block that collects the scalar leaf value
// by traversing the edge path from the root set. In DQL, a value variable defined inside
// a child block is mapped by child UIDs. To group parent nodes by this value, we must
// aggregate it up to the root parent level using an aggregate variable assignment (e.g. max).
//
// The emitted DQL looks like:
//
//	var(func: uid(CompanyRoot)) {
//	  Company.hasStatus {
//	    __gby_0_c as StatusIfc.name
//	  }
//	  __gby_0 as max(val(__gby_0_c))
//	}
//
// The parent-level variable __gby_0 is then used in @groupby(val(__gby_0)) in the main query.
func buildValueVarBlock(
	rootFunc *dql.Function,
	edgePath []string,
	edgeTypes []schema.Type,
	leafPred string,
	varName string,
	auth *authRewriter,
	nestedAuthQrys *[]*dql.GraphQuery,
) *dql.GraphQuery {
	childVar := varName + "_c"

	// Build from the inside out.
	innermost := &dql.GraphQuery{
		Var:  childVar,
		Attr: leafPred,
	}

	current := innermost
	for i := len(edgePath) - 1; i >= 0; i-- {
		var children []*dql.GraphQuery
		children = append(children, current)

		var nextChildVar string
		if i < len(edgePath)-1 {
			nextChildVar = fmt.Sprintf("%s_p%d", varName, i)
			aggregator := &dql.GraphQuery{
				Var:  nextChildVar,
				Attr: fmt.Sprintf("max(val(%s))", childVar),
			}
			children = append(children, aggregator)
		}

		wrapper := &dql.GraphQuery{
			Attr:     edgePath[i],
			Children: children,
		}

		// 🔒 Apply Child Type Auth on this Edge!
		if auth != nil && !auth.isWritingAuth && i < len(edgeTypes) {
			edgeType := edgeTypes[i]
			rbac := auth.evaluateStaticRules(edgeType)
			if rbac == schema.Negative {
				wrapper.Filter = &dql.FilterTree{
					Func: &dql.Function{
						Name: "uid",
						UID:  []uint64{0},
					},
				}
			} else if rbac == schema.Uncertain {
				oldVarName := auth.varName
				auth.varName = auth.varGen.Next(edgeType, "", "", auth.isWritingAuth)

				oldCascadeAuthType := auth.cascadeAuthorityType
				auth.cascadeAuthorityType = edgeType.Name()

				authQrys, authFilter := auth.rewriteAuthQueries(edgeType)
				if authFilter != nil {
					wrapper.Filter = authFilter
				}
				if len(authQrys) > 0 {
					*nestedAuthQrys = append(*nestedAuthQrys, authQrys...)
				}

				auth.cascadeAuthorityType = oldCascadeAuthType
				auth.varName = oldVarName
			}
		}

		current = wrapper
		if i < len(edgePath)-1 {
			childVar = nextChildVar
		}
	}

	finalAggregator := &dql.GraphQuery{
		Var:  varName,
		Attr: fmt.Sprintf("max(val(%s))", childVar),
	}

	return &dql.GraphQuery{
		Attr:     "var",
		Func:     rootFunc,
		Children: []*dql.GraphQuery{current, finalAggregator},
	}
}

// groupByQuery rewrites a groupByXxx GraphQL query into a DQL @groupby query.
//
// For direct-field specs the generated DQL is:
//
//	groupByNote(func: uid(<root>)) @filter(...) @groupby(Note.status, Note.createdAt@month) {
//	    count(uid)
//	    titleMin: min(Note.title)
//	}
//
// For nested-field specs (or a mix of direct + nested) value variables are used to pull
// the leaf value back up to the root level, enabling all aggregates:
//
//	var(func: uid(<root>)) {
//	    Application.hasStatus { __gby_1 as ApplicationStatus.name }
//	}
//	groupByApplication(func: uid(<root>)) @filter(...) @groupby(Application.createdAt@month, val(__gby_1)) {
//	    count(uid)
//	    ratingAvg: avg(Application.rating)
//	}
//
// Key fields (the fields listed in the groupBy argument) are NOT added as DQL children;
// they appear automatically as predicate-keyed entries inside the DQL @groupby JSON array.
// Only aggregate functions (count, Min, Max, Sum, Avg) are added as children.
func groupByQuery(query schema.Query, authRw *authRewriter) ([]*dql.GraphQuery, error) {
	// mainType is the concrete type being grouped (e.g. Note for groupByNote).
	mainType := query.ConstructedFor()

	dgQuery, rbac := addCommonRules(query, mainType, authRw)
	if rbac == schema.Negative {
		return dgQuery, nil
	}

	// Add user filter.
	filter, _ := query.ArgValue("filter").(map[string]interface{})
	_, varQry := addFilter(dgQuery[0], mainType, filter, authRw, query.Alias())
	dgQuery = append(dgQuery, varQry...)

	// Apply auth — after this, dgQuery[0] may have its Func replaced with uid(XRoot)
	// where XRoot is a variable defined by the auth var blocks in dgQuery[1:].
	dgQuery, _ = authRw.addAuthQueries(mainType, dgQuery, rbac)

	mainQuery := dgQuery[0]
	mainQuery.IsGroupby = true

	// Parse the groupBy argument.
	// pathMap tracks spec index → dot-separated GraphQL path for nested specs,
	// so completeGroupByResult can label groupKeys correctly.
	groupByArg, _ := query.ArgValue("groupBy").([]interface{})
	pathMap := make(map[int]string) // spec index → "hasStatus.name" etc.
	groupedDirectFields := make(map[string]bool)

	// varBlocks accumulates var() queries for nested specs; emitted before mainQuery.
	var varBlocks []*dql.GraphQuery
	var nestedAuthQrys []*dql.GraphQuery

	for i, spec := range groupByArg {
		specMap, _ := spec.(map[string]interface{})
		fieldObj, _ := specMap["field"].(map[string]interface{})
		by, _ := specMap["by"].(string)
		tz, _ := specMap["tz"].(string)

		pathSegments, dgPreds, edgeTypes, err := resolveGroupByPath(fieldObj, mainType)
		if err != nil {
			return nil, err
		}

		if len(dgPreds) == 1 {
			// Direct field — existing behaviour.
			leafPred := dgPreds[0]
			mainQuery.GroupbyAttrs = append(mainQuery.GroupbyAttrs, dql.GroupByAttr{
				Attr:          leafPred,
				TokenizerName: by,
				Timezone:      tz,
			})
			groupedDirectFields[pathSegments[0]] = true
		} else {
			// Nested path — the correct DQL strategy is:
			//   1. Collect the leaf scalar values mapped to root UIDs in a var() block.
			//   2. Group by val(__gby_N) in the main query block (rooted at rootVar).
			//
			// Example (Company → hasStatus → name):
			//   var(func: uid(CompanyRoot)) {
			//     Company.hasStatus {
			//       __gby_0 as StatusIfc.name
			//     }
			//   }
			//   groupByCompany(func: uid(CompanyRoot)) @groupby(val(__gby_0)) {
			//     count(uid)
			//   }
			//
			// Multiple nested specs are fully supported.
			// Each nested spec is translated to its own auxiliary var() block and aggregated.
			edgePath := dgPreds[:len(dgPreds)-1]
			leafPred := dgPreds[len(dgPreds)-1]
			varName := fmt.Sprintf("__gby_%d", i)

			varBlocks = append(varBlocks, buildValueVarBlock(mainQuery.Func, edgePath, edgeTypes, leafPred, varName, authRw, &nestedAuthQrys))

			mainQuery.GroupbyAttrs = append(mainQuery.GroupbyAttrs, dql.GroupByAttr{
				VarName:       varName,
				IsValueVar:    true,
				TokenizerName: by,
				Timezone:      tz,
			})
			pathMap[i] = strings.Join(pathSegments, ".")
		}
	}

	// Build DQL aggregate-function children from the GraphQL selection set.
	// All aggregates are valid for both direct and nested specs because @groupby
	// runs at root level in both cases.
	isCountAdded := false
	isAggAdded := make(map[string]bool)
	for _, f := range query.SelectionSet() {
		fldName := f.Name()
		if f.Skip() || !f.Include() || fldName == schema.Typename {
			continue
		}
		// groupKeys is assembled in completeGroupByResult, not via DQL children.
		if fldName == "groupKeys" {
			continue
		}
		// Direct key fields appear in the DQL @groupby response automatically;
		// they must not be added as DQL aggregate children.
		if groupedDirectFields[fldName] {
			continue
		}
		if fldName == "count" {
			if !isCountAdded {
				mainQuery.Children = append(mainQuery.Children, &dql.GraphQuery{
					// Use bare count(uid) — NOT "count as count(uid)".
					// Dgraph only allows the variable-assignment form when
					// @groupby is on a UID/edge attribute; for scalar predicates
					// (strings, ints, etc.) bare count(uid) is required and still
					// produces {"count": N} entries inside the @groupby envelope.
					Attr: "count(uid)",
				})
				isCountAdded = true
			}
			continue
		}
		for _, fn := range []string{"Max", "Min", "Sum", "Avg"} {
			if strings.HasSuffix(fldName, fn) {
				if !isAggAdded[fldName] {
					baseName := fldName[:len(fldName)-len(fn)]
					dgPred := mainType.DgraphPredicate(baseName)
					mainQuery.Children = append(mainQuery.Children, &dql.GraphQuery{
						Alias: fldName,
						Attr:  strings.ToLower(fn) + "(" + dgPred + ")",
					})
					isAggAdded[fldName] = true
				}
				break
			}
		}
	}

	// Emit in dependency order:
	//   1. auth/filter var blocks (define root variable)
	//   2. nested-type auth queries
	//   3. nested-spec var() blocks (define __gby_N variables)
	//   4. main @groupby query
	authAndFilterQrys := dgQuery[1:]
	result := make([]*dql.GraphQuery, 0, len(authAndFilterQrys)+len(nestedAuthQrys)+len(varBlocks)+1)
	result = append(result, authAndFilterQrys...)
	result = append(result, nestedAuthQrys...)
	result = append(result, varBlocks...)
	result = append(result, mainQuery)
	return result, nil
}

func passwordQuery(m schema.Query, authRw *authRewriter) ([]*dql.GraphQuery, error) {
	xid, uid, err := m.IDArgValue()
	if err != nil {
		return nil, err
	}

	dgQuery := rewriteAsGet(m, uid, xid, authRw)

	// Handle empty dgQuery
	if strings.HasSuffix(dgQuery[0].Attr, "()") {
		return dgQuery, nil
	}

	// mainQuery is the query with check<Type>Password as Attr.
	// It is the first in the list of dgQuery.
	mainQuery := dgQuery[0]

	queriedType := m.Type()
	name := queriedType.PasswordField().Name()
	predicate := queriedType.DgraphPredicate(name)
	password := m.ArgValue(name).(string)

	// This adds the checkPwd function
	op := &dql.GraphQuery{
		Attr:   "checkPwd",
		Func:   mainQuery.Func,
		Filter: mainQuery.Filter,
		Children: []*dql.GraphQuery{{
			Var: "pwd",
			Attr: fmt.Sprintf(`checkpwd(%s, "%s")`, predicate,
				password),
		}},
	}

	ft := &dql.FilterTree{
		Op: "and",
		Child: []*dql.FilterTree{{
			Func: &dql.Function{
				Name: "eq",
				Args: []dql.Arg{
					{
						Value: "val(pwd)",
					},
					{
						Value: "1",
					},
				},
			},
		}},
	}

	if mainQuery.Filter != nil {
		ft.Child = append(ft.Child, mainQuery.Filter)
	}

	mainQuery.Filter = ft

	return append(dgQuery, op), nil
}

func intersection(a, b []uint64) []uint64 {
	m := make(map[uint64]bool)
	var c []uint64

	for _, item := range a {
		m[item] = true
	}

	for _, item := range b {
		if _, ok := m[item]; ok {
			c = append(c, item)
		}
	}

	return c
}

// addUID adds UID for every node that we query. Otherwise we can't tell the
// difference in a query result between a node that's missing and a node that's
// missing a single value.  E.g. if we are asking for an Author and only the
// 'text' of all their posts e.g. getAuthor(id: 0x123) { posts { text } }
// If the author has 10 posts but three of them have a title, but no text,
// then Dgraph would just return 7 posts.  And we'd have no way of knowing if
// there's only 7 posts, or if there's more that are missing 'text'.
// But, for GraphQL, we want to know about those missing values.
func addUID(dgQuery *dql.GraphQuery) {
	if len(dgQuery.Children) == 0 {
		return
	}
	hasUid := false
	for _, c := range dgQuery.Children {
		if c.Attr == "uid" {
			hasUid = true
		}
		addUID(c)
	}

	// If uid was already requested by the user then we don't need to add it again.
	if hasUid {
		return
	}
	uidChild := &dql.GraphQuery{
		Attr:  "uid",
		Alias: "dgraph.uid",
	}
	dgQuery.Children = append(dgQuery.Children, uidChild)
}

func rewriteAsQueryByIds(
	field schema.Field,
	uids []uint64,
	authRw *authRewriter,
	queryName string) []*dql.GraphQuery {
	if field == nil {
		return nil
	}

	rbac := authRw.evaluateStaticRules(field.Type())
	dgQuery := []*dql.GraphQuery{{
		Attr: field.DgraphAlias(),
	}}

	if rbac == schema.Negative {
		dgQuery[0].Attr = dgQuery[0].Attr + "()"
		return dgQuery
	}

	dgQuery[0].Func = &dql.Function{
		Name: "uid",
		UID:  uids,
	}

	if ids := idFilter(extractQueryFilter(field), field.Type().IDField()); ids != nil {
		addUIDFunc(dgQuery[0], intersection(ids, uids))
	}

	oldVarName := authRw.varName
	authRw.varName = "__ROOT_VAR_PLACEHOLDER__"

	includedQueries := addArgumentsToField(dgQuery[0], field, authRw, queryName)
	dgQuery = append(dgQuery, includedQueries...)

	// The function getQueryByIds is called for passwordQuery or fetching query result types
	// after making a mutation. In both cases, we want the selectionSet to use the `query` auth
	// rule. queryAuthSelector function is used as selector before calling addSelectionSetFrom function.
	// The original selector function of authRw is stored in oldAuthSelector and used after returning
	// from addSelectionSetFrom function.
	oldAuthSelector := authRw.selector
	authRw.selector = queryAuthSelector
	selectionAuth := addSelectionSetFrom(dgQuery[0], field, authRw)
	authRw.selector = oldAuthSelector

	addUID(dgQuery[0])
	addCascadeDirective(dgQuery[0], field)

	dgQuery, authVarSubst := authRw.addAuthQueries(field.Type(), dgQuery, rbac)

	generatedVarName := authRw.varName
	authRw.varName = oldVarName

	// Substitute the generated root variable name in place of the __ROOT_VAR_PLACEHOLDER__ placeholder.
	// Since addAuthQueries has now run, it has generated a real, sequential variable name for the root.
	if generatedVarName != "" && generatedVarName != "__ROOT_VAR_PLACEHOLDER__" {
		placeholderSubst := map[string]string{"__ROOT_VAR_PLACEHOLDER__": generatedVarName}
		applyAuthVarSubstToQueries(selectionAuth, placeholderSubst)
		applyAuthVarSubstToQueries(dgQuery, placeholderSubst)
	}

	if len(selectionAuth) > 0 {
		// Dedup field-level auth var blocks and propagate new substitutions
		// into authVarSubst before applying it to selectionAuth.
		selectionAuth, authVarSubst = deduplicateSelectionAuth(selectionAuth, dgQuery, authVarSubst)
		applyAuthVarSubstToQueries(selectionAuth, authVarSubst)
		dgQuery = append(dgQuery, selectionAuth...)
	}

	return dgQuery
}

// addArgumentsToField adds various different arguments to a field, such as
// filter, order and pagination.
func addArgumentsToField(dgQuery *dql.GraphQuery,
	field schema.Field,
	auth *authRewriter,
	queryName string) []*dql.GraphQuery {
	filter, _ := field.ArgValue("filter").(map[string]interface{})
	_, varQry := addFilter(dgQuery, field.Type(), filter, auth, queryName)
	addOrder(dgQuery, field)
	addPagination(dgQuery, field)
	return varQry
}

func addTopLevelTypeFilter(query *dql.GraphQuery, field schema.Field) {
	addTypeFilter(query, field.Type())
}

func rewriteAsGet(
	query schema.Query,
	uid uint64,
	xidArgToVal map[string]string,
	auth *authRewriter) []*dql.GraphQuery {

	var dgQuery []*dql.GraphQuery
	rbac := auth.evaluateStaticRules(query.Type())

	// If Get query is for Type and none of the authrules are satisfied, then it is
	// caught here but in case of interface, we need to check validity on each
	// implementing type as Rules for the interface are made empty.
	if rbac == schema.Negative {
		return []*dql.GraphQuery{{Attr: query.DgraphAlias() + "()"}}
	}

	// For interface, empty query should be returned if Auth rules are
	// not satisfied even for a single implementing type
	if query.Type().IsInterface() {
		implementingTypesHasFailedRules := false
		implementingTypes := query.Type().ImplementingTypes()
		for _, typ := range implementingTypes {
			if auth.evaluateStaticRules(typ) != schema.Negative {
				implementingTypesHasFailedRules = true
			}
		}

		if !implementingTypesHasFailedRules {
			return []*dql.GraphQuery{{Attr: query.Name() + "()"}}
		}
	}

	if len(xidArgToVal) == 0 {
		dgQuery = rewriteAsQueryByIds(query, []uint64{uid}, auth, query.Alias())

		// Add the type filter to the top level get query. When the auth has been written into the
		// query the top level get query may be present in query's children.
		addTopLevelTypeFilter(dgQuery[0], query)

		return dgQuery
	}
	// iterate over map in sorted order to ensure consistency
	xids := make([]string, len(xidArgToVal))
	i := 0
	for k := range xidArgToVal {
		xids[i] = k
		i++
	}
	sort.Strings(xids)
	xidArgNameToDgPredMap := query.XIDArgs()
	var flt []*dql.FilterTree
	for _, xid := range xids {
		eqXidFuncTemp := &dql.Function{
			Name: "eq",
			Args: []dql.Arg{
				{Value: xidArgNameToDgPredMap[xid]},
				{Value: maybeQuoteArg("eq", xidArgToVal[xid])},
			},
		}
		flt = append(flt, &dql.FilterTree{
			Func: eqXidFuncTemp,
		})
	}
	if uid > 0 {
		dgQuery = []*dql.GraphQuery{{
			Attr: query.DgraphAlias(),
			Func: &dql.Function{
				Name: "uid",
				UID:  []uint64{uid},
			},
		}}
		dgQuery[0].Filter = &dql.FilterTree{
			Op:    "and",
			Child: flt,
		}

	} else {
		dgQuery = []*dql.GraphQuery{{
			Attr: query.DgraphAlias(),
			Func: flt[0].Func,
		}}
		if len(flt) > 1 {
			dgQuery[0].Filter = &dql.FilterTree{
				Op:    "and",
				Child: flt[1:],
			}
		}
	}

	// Apply query auth rules even for password query
	oldAuthSelector := auth.selector
	auth.selector = queryAuthSelector
	selectionAuth := addSelectionSetFrom(dgQuery[0], query, auth)
	auth.selector = oldAuthSelector

	addUID(dgQuery[0])
	addTypeFilter(dgQuery[0], query.Type())
	addCascadeDirective(dgQuery[0], query)

	dgQuery, authVarSubst := auth.addAuthQueries(query.Type(), dgQuery, rbac)

	if len(selectionAuth) > 0 {
		// Dedup field-level auth var blocks and propagate new substitutions
		// into authVarSubst before applying it to selectionAuth.
		selectionAuth, authVarSubst = deduplicateSelectionAuth(selectionAuth, dgQuery, authVarSubst)
		applyAuthVarSubstToQueries(selectionAuth, authVarSubst)
		dgQuery = append(dgQuery, selectionAuth...)
	}

	return dgQuery
}

// rewriteAsSimilarByIdQuery
//
// rewrites SimilarById graphQL query to nested DQL query blocks
// Example rewrittern query:
//
//			query {
//			    var(func: eq(Product.id, "0528012398")) @filter(type(Product)) {
//			        vec as Product.embedding
//			    }
//			    var() {
//			        v1 as max(val(vec))
//			    }
//			    var(func: similar_to(Product.embedding, 8, val(v1))) {
//			        v2 as Product.embedding
//			        distance as math((v2 - v1) dot (v2 - v1))
//			    }
//			    querySimilarProductById(func: uid(distance)
//	             @filter(Product.id != "0528012398"), orderasc: val(distance)) {
//			        Product.id : Product.id
//			        Product.description : Product.description
//			        Product.title : Product.title
//			        Product.imageUrl : Product.imageUrl
//			        Product.vector_distance : val(distance)
//			        dgraph.uid : uid
//			    }
//		 }
func rewriteAsSimilarByIdQuery(
	query schema.Query,
	uid uint64,
	xidArgToVal map[string]string,
	auth *authRewriter) []*dql.GraphQuery {

	// Get graphQL arguments
	typ := query.Type()
	similarBy := query.ArgValue(schema.SimilarByArgName).(string)
	pred := typ.DgraphPredicate(similarBy)
	topK := query.ArgValue(schema.SimilarTopKArgName)
	similarByField := typ.Field(similarBy)
	metric := similarByField.EmbeddingSearchMetric()
	distanceFormula := "math(sqrt((v2 - v1) dot (v2 - v1)))" // default - euclidean

	if metric == schema.SimilarSearchMetricDotProduct {
		distanceFormula = "math((1.0 - (v1 dot v2)) /2.0)"
	} else if metric == schema.SimilarSearchMetricCosine {
		distanceFormula = "math((1.0 - ((v1 dot v2) / sqrt( (v1 dot v1) * (v2 dot v2) ) )) / 2.0)"
	}

	// First generate the query to fetch the uid
	// for the given id. For Example,
	// var(func: eq(Product.id, "0528012398")) @filter(type(Product)) {
	// 	vec as Product.embedding
	// }
	dgQuery := rewriteAsGet(query, uid, xidArgToVal, auth)
	lastQuery := dgQuery[len(dgQuery)-1]
	// Turn the root query into "var"
	lastQuery.Attr = "var"
	// Save the result to be later used for the last query block, sortQuery
	result := lastQuery.Children

	// define the variable "vec" for the search vector
	lastQuery.Children = []*dql.GraphQuery{{
		Attr: pred,
		Var:  "vec",
	}}

	// Turn the variable into a "const" by
	// remembering the  max of it.
	// The lookup is going to return exactly one uid
	// anyway. For example,
	// var() {
	//	 v1 as max(val(vec))
	// }
	aggQuery := &dql.GraphQuery{
		Attr: "var" + "()",
		Children: []*dql.GraphQuery{
			{
				Var:  "v1",
				Attr: "max(val(vec))",
			},
		},
	}

	// Similar_to query, computes the distance for
	// ordering the result later.
	// Example:
	// var(func: similar_to(Product.embedding, 8, val(v1))) {
	//	  v2 as Product.embedding
	//	  distance as math((v2 - v1) dot (v2 - v1))
	//  }
	similarQuery := &dql.GraphQuery{
		Attr: "var",
		Children: []*dql.GraphQuery{
			{
				Var:  "v2",
				Attr: pred,
			},
			{
				Var:  "distance",
				Attr: distanceFormula,
			},
		},
		Func: &dql.Function{
			Name: "similar_to",
			Args: []dql.Arg{
				{
					Value: pred,
				},
				{
					Value: fmt.Sprintf("%v", topK),
				},
				{
					Value: "val(v1)",
				},
			},
		},
	}

	// Rename the distance as <Type>.vector_distance
	distance := &dql.GraphQuery{
		Alias: typ.Name() + "." + schema.SimilarQueryDistanceFieldName,
		Attr:  "val(distance)",
	}

	var found bool = false
	for _, child := range result {
		if child.Alias == typ.Name()+"."+schema.SimilarQueryDistanceFieldName {
			child.Attr = "val(distance)"
			found = true
			break
		}
	}
	if !found {
		result = append(result, distance)
	}

	// order the result by euclidean distance, For example,
	//	 querySimilarProductById(func: uid(distance), orderasc: val(distance)) {
	//	     Product.id : Product.id
	//	     Product.description : Product.description
	//	     Product.title : Product.title
	//	     Product.imageUrl : Product.imageUrl
	//	     Product.vector_distance : val(distance)
	//	     dgraph.uid : uid
	//	  }
	//	 }
	sortQuery := &dql.GraphQuery{
		Attr:     query.DgraphAlias(),
		Children: result,
		Func: &dql.Function{
			Name: "uid",
			Args: []dql.Arg{{Value: "distance"}},
		},
		Order: []*pb.Order{{Attr: "val(distance)", Desc: false}},
	}
	addArgumentsToField(sortQuery, query, auth, query.Alias())

	dgQuery = append(dgQuery, aggQuery, similarQuery, sortQuery)
	return dgQuery
}

// rewriteAsSimilarByEmbeddingQuery
//
// rewrites SimilarByEmbedding graphQL query to nested DQL query blocks
// Example rewrittern query:
//
//		query gQLTodQL($search_vector: float32vector = "<json array of float>") {
//		    var(func: similar_to(Product.embedding, 8, $search_vector)) {
//		        v2 as Product.embedding
//		        distance as math((v2 - $search_vector) dot (v2 - $search_vector))
//		    }
//		    querySimilarProductById(func: uid(distance),
//	             @filter(Product.id != "0528012398"), orderasc: val(distance)) {
//		        Product.id : Product.id
//		        Product.description : Product.description
//		        Product.title : Product.title
//		        Product.imageUrl : Product.imageUrl
//		        Product.vector_distance : val(distance)
//		        dgraph.uid : uid
//		     }
//		 }
func rewriteAsSimilarByEmbeddingQuery(
	query schema.Query, auth *authRewriter) []*dql.GraphQuery {

	dgQuery := rewriteAsQuery(query, auth, query.Alias())

	// Remember dgQuery[0].Children as result type for the last block
	// in the rewritten query
	result := dgQuery[0].Children
	typ := query.Type()

	// Get all the arguments from graphQL query
	similarBy := query.ArgValue(schema.SimilarByArgName).(string)
	pred := typ.DgraphPredicate(similarBy)
	topK := query.ArgValue(schema.SimilarTopKArgName)
	distanceArg := query.ArgValue("distance")
	var distanceThreshold float32
	if distanceArg != nil {
		var err error
		dist, err := strconv.ParseFloat(fmt.Sprintf("%v", distanceArg), 32)
		if err == nil {
			distanceThreshold = float32(dist)
		}
	}

	similarByField := typ.Field(similarBy)
	var vecStr []byte
	if similarByField.HasEmbeddingProvider() {
		// Text input, need to generate embedding
		text, ok := query.ArgValue(schema.SimilarTextArgName).(string)
		if !ok {
			// This should not happen if validation is correct.
			return []*dql.GraphQuery{}
		}
		embedding, err := similarByField.GenerateEmbedding(text)
		if err != nil {
			// How to handle this error?
			// For now, I will just log it and return an empty query.
			glog.Errorf("Failed to generate embedding: %v", err)
			return []*dql.GraphQuery{}
		}
		vecStr, _ = json.Marshal(embedding)
	} else {
		// Vector input
		vecArg := query.ArgValue(schema.SimilarVectorArgName)
		if vecArg != nil {
			vec := vecArg.([]interface{})
			vecStr, _ = json.Marshal(vec)
		}
	}

	metric := similarByField.EmbeddingSearchMetric()
	distanceFormula := "math(sqrt((v2 - $search_vector) dot (v2 - $search_vector)))" // default = euclidean

	if metric == schema.SimilarSearchMetricDotProduct {
		distanceFormula = "math(( 1.0 - (($search_vector) dot v2)) /2.0)"
	} else if metric == schema.SimilarSearchMetricCosine {
		distanceFormula = "math((1.0 - ( (($search_vector) dot v2) / sqrt( (($search_vector) dot ($search_vector))" +
			" * (v2 dot v2) ) )) / 2.0)"
	}

	// Save vectorString as a query variable, $search_vector
	if dgQuery[0].Args == nil {
		dgQuery[0].Args = make(map[string]string)
	}
	dgQuery[0].Args["$search_vector"] = " float32vector = \"" + string(vecStr) + "\""
	thisFilter := &dql.FilterTree{
		Func: dgQuery[0].Func,
	}

	// create the similar_to function and move existing root function
	// to the filter tree
	addToFilterTree(dgQuery[0], thisFilter)

	// Create similar_to as the root function, passing $search_vector as
	// the search vector
	dgQuery[0].Attr = "var"
	similarToArgs := []dql.Arg{
		{Value: pred},
		{Value: fmt.Sprintf("%v", topK)},
		{Value: "$search_vector"},
	}
	// Only include the distance threshold when explicitly set by the user (non-zero).
	if distanceThreshold != 0 {
		similarToArgs = append(similarToArgs, dql.Arg{Value: fmt.Sprintf("%v", distanceThreshold)})
	}
	dgQuery[0].Func = &dql.Function{
		Name: "similar_to",
		Args: similarToArgs,
	}

	// Compute the euclidean distance between the neighbor
	// and the search vector
	dgQuery[0].Children = []*dql.GraphQuery{
		{
			Var:  "v2",
			Attr: pred,
		},
		{
			Var:  "distance",
			Attr: distanceFormula,
		},
	}

	// Rename distance as <Type>.vector_distance
	distance := &dql.GraphQuery{
		Alias: typ.Name() + "." + schema.SimilarQueryDistanceFieldName,
		Attr:  "val(distance)",
	}

	var found bool = false
	for _, child := range result {
		if child.Alias == typ.Name()+"."+schema.SimilarQueryDistanceFieldName {
			child.Attr = "val(distance)"
			found = true
			break
		}
	}
	if !found {
		result = append(result, distance)
	}

	// order by distance
	sortQuery := &dql.GraphQuery{
		Attr:     query.DgraphAlias(),
		Children: result,
		Func: &dql.Function{
			Name: "uid",
			Args: []dql.Arg{{Value: "distance"}},
		},
		Order: []*pb.Order{{Attr: "val(distance)", Desc: false}},
	}

	dgQuery = append(dgQuery, sortQuery)
	return dgQuery
}

// Adds common RBAC and UID, Type rules to DQL query.
// This function is used by rewriteAsQuery and aggregateQuery functions
func addCommonRules(
	field schema.Field,
	fieldType schema.Type,
	authRw *authRewriter) ([]*dql.GraphQuery, schema.RuleResult) {
	rbac := authRw.evaluateStaticRules(fieldType)
	dgQuery := &dql.GraphQuery{
		Attr: field.DgraphAlias(),
	}

	if rbac == schema.Negative {
		dgQuery.Attr = dgQuery.Attr + "()"
		return []*dql.GraphQuery{dgQuery}, rbac
	}

	// When rewriting auth rules, they always start like
	// Todo2 as var(func: uid(Todo1)) @cascade {
	// Where Todo1 is the variable generated from the filter of the field
	// we are adding auth to.
	// Except for the case in which filter in auth rules is on field of
	// ID type. In this situation we write it as:
	// Todo2 as var(func: uid(0x5....)) @cascade {
	// We first check ids in the query filter and rewrite accordingly.
	ids := idFilter(extractQueryFilter(field), fieldType.IDField())

	// Todo: Add more comments to this block.
	// OPTIMIZATION: If we are a standard read query with a pre-computed Auth variable (varName != ""),
	// rewrite the root query using the uid() function to completely bypass the O(N) full class scan.
	if authRw != nil && (authRw.isWritingAuth || authRw.filterByUid || authRw.varName != "") &&
		(authRw.varName != "" || authRw.parentVarName != "") && ids == nil {
		authRw.addVariableUIDFunc(dgQuery)
		// This is executed when querying while performing delete mutation request since
		// in case of delete mutation we already have variable `MutationQueryVar` at root level.
		if authRw.filterByUid || (!authRw.isWritingAuth && authRw.varName != "") {
			// Since the variable is only added at the top level we reset the `authRW` variables.
			authRw.varName = ""
			authRw.filterByUid = false
		}
	} else if ids != nil {
		addUIDFunc(dgQuery, ids)
	} else {
		addTypeFunc(dgQuery, fieldType.DgraphName())
	}
	return []*dql.GraphQuery{dgQuery}, rbac
}

func rewriteAsQuery(field schema.Field, authRw *authRewriter, queryName string) []*dql.GraphQuery {
	dgQuery, rbac := addCommonRules(field, field.Type(), authRw)
	if rbac == schema.Negative {
		return dgQuery
	}

	oldVarName := authRw.varName
	authRw.varName = "__ROOT_VAR_PLACEHOLDER__"

	varQry := addArgumentsToField(dgQuery[0], field, authRw, queryName)
	dgQuery = append(dgQuery, varQry...)

	selectionAuth := addSelectionSetFrom(dgQuery[0], field, authRw)
	// we don't need to query uid for auth queries, as they always have at least one field in their
	// selection set.
	if !authRw.writingAuth() {
		addUID(dgQuery[0])
	}
	addCascadeDirective(dgQuery[0], field)

	dgQuery, authVarSubst := authRw.addAuthQueries(field.Type(), dgQuery, rbac)

	generatedVarName := authRw.varName
	authRw.varName = oldVarName

	// Substitute the generated root variable name in place of the __ROOT_VAR_PLACEHOLDER__ placeholder.
	// Since addAuthQueries has now run, it has generated a real, sequential variable name for the root.
	if generatedVarName != "" && generatedVarName != "__ROOT_VAR_PLACEHOLDER__" {
		placeholderSubst := map[string]string{"__ROOT_VAR_PLACEHOLDER__": generatedVarName}
		applyAuthVarSubstToQueries(selectionAuth, placeholderSubst)
		applyAuthVarSubstToQueries(dgQuery, placeholderSubst)
	}

	if len(selectionAuth) > 0 {
		// Dedup field-level auth var blocks (selectionAuth was never included in the
		// addAuthQueries dedup pass). Merge any new substitutions into authVarSubst
		// so the combined map covers both root-auth and field-auth deduplication.
		// This also propagates new substitutions to dgQuery filter-path var blocks
		// so that uid() refs to deduplicated-away vars (e.g. Workspace_Auth16) are
		// resolved before the query is submitted to Dgraph.
		selectionAuth, authVarSubst = deduplicateSelectionAuth(selectionAuth, dgQuery, authVarSubst)
		applyAuthVarSubstToQueries(selectionAuth, authVarSubst)
		return append(dgQuery, selectionAuth...)
	}

	dgQuery = rootQueryOptimization(dgQuery)
	return dgQuery
}

func rootQueryOptimization(dgQuery []*dql.GraphQuery) []*dql.GraphQuery {
	if dgQuery[0].Filter != nil && dgQuery[0].Filter.Func != nil &&
		dgQuery[0].Filter.Func.Name == "eq" && dgQuery[0].Func.Name == "type" {
		rootFunc := dgQuery[0].Func
		dgQuery[0].Func = dgQuery[0].Filter.Func
		dgQuery[0].Filter.Func = rootFunc
	}
	return dgQuery
}

func (authRw *authRewriter) writingAuth() bool {
	return authRw != nil && authRw.isWritingAuth

}

// addAuthQueries takes a field and the GraphQuery that has so far been constructed for
// the field and builds any auth queries that are need to restrict the result to only
// the nodes authorized to be queried, returning a new graphQuery that does the
// original query and the auth.
func (authRw *authRewriter) addAuthQueries(
	typ schema.Type,
	dgQuery []*dql.GraphQuery,
	rbacEval schema.RuleResult) ([]*dql.GraphQuery, map[string]string) {

	// There's no need to recursively inject auth queries into other auth queries, so if
	// we are already generating an auth query, there's nothing to add.
	if authRw == nil || authRw.isWritingAuth {
		return dgQuery, nil
	}

	if authRw.varName == "" || authRw.varName == "__ROOT_VAR_PLACEHOLDER__" {
		authRw.varName = authRw.varGen.Next(typ, "", "", authRw.isWritingAuth)
	}

	fldAuthQueries, filter := authRw.rewriteAuthQueries(typ)

	// If We are adding AuthRules on an Interfaces's operation,
	// we need to construct auth filters by verifying Auth rules on the
	// implementing types.

	if typ.IsInterface() {
		// First we fetch the list of Implementing types here
		implementingTypes := make([]schema.Type, 0)
		implementingTypes = append(implementingTypes, typ.ImplementingTypes()...)

		var qrys []*dql.GraphQuery
		var filts []*dql.FilterTree
		implementingTypesHasAuthRules := false
		for _, object := range implementingTypes {

			// It could be the case that None of implementing Types have Auth Rules, which clearly
			// indicates that neither the interface, nor any of the implementing type has its own
			// Auth rules.
			// ImplementingTypeHasAuthRules is set to true even if one of the implemented type have
			// Auth rules or Interface has its own auth rule, in the latter case, all the
			// implemented types must have inherited those auth rules.
			if object.AuthRules().Rules != nil {
				implementingTypesHasAuthRules = true
			}

			// First Check if the Auth Rules of the given type are satisfied or not.
			// It might be possible that auth rule inherited from some other interface
			// is not being satisfied. In that case we have to Drop this type
			rbac := authRw.evaluateStaticRules(object)
			if rbac == schema.Negative {
				continue
			}

			// Form Query Like Todo_1 as var(func: type(Todo)).
			// NOTE: the variable name is allocated now so that rewriteAuthQueries
			// can pass it as varName to auth sub-rules, but the var block itself is
			// NOT emitted yet. We defer the emit until we know whether the resulting
			// auth queries actually consume it (see below).
			//
			// Background: Dgraph rejects queries where a variable is defined but
			// never referenced. For types whose @auth rules traverse from
			// type(Workspace) (e.g. Note, EmailOutbound), none of the generated DQL
			// auth blocks are rooted at uid(queryVar), so emitting the root
			// type-scan var unconditionally would create a phantom variable.
			queryVar := authRw.varGen.Next(object, "", "", authRw.isWritingAuth)
			varQry := &dql.GraphQuery{
				Attr: "var",
				Var:  queryVar,
				Func: &dql.Function{
					Name: "type",
					Args: []dql.Arg{{Value: object.Name()}},
				},
			}

			// Form Auth Queries for the given object
			objAuthQueries, objfilter := (&authRewriter{
				authVariables:   authRw.authVariables,
				varGen:          authRw.varGen,
				varName:         queryVar,
				selector:        authRw.selector,
				parentVarName:   authRw.parentVarName,
				hasAuthRules:    authRw.hasAuthRules,
				cascadeVarCache: authRw.cascadeVarCache,
				forceForward:    authRw.forceForward,
			}).rewriteAuthQueries(object)

			// 1. If there is no Auth Query for the Given type then it means that
			// neither the inherited interface, nor this type has any Auth rules.
			// In this case the query must return all the nodes of this type.
			// Then we put uid(queryVar) with OR in the main query filter, and
			// rootQry will reference it — so the var block must be emitted.
			// 2. If rbac evaluates to `Positive` which means RBAC rule is satisfied.
			// Either it is the only auth rule, or it is present with `OR`, which means
			// query must return all the nodes of this type. rootQry will reference
			// uid(queryVar) directly — so the var block must be emitted.
			if len(objAuthQueries) == 0 || rbac == schema.Positive {
				// Both paths produce uid(queryVar) in rootQry's OR-filter; emit the block.
				qrys = append(qrys, varQry)
				objfilter = &dql.FilterTree{
					Func: &dql.Function{
						Name: "uid",
						Args: []dql.Arg{{Value: queryVar, IsValueVar: false, IsDQLVar: false}},
					},
				}
				filts = append(filts, objfilter)
			} else {
				// Auth queries exist. Only emit the root type-scan var block when
				// the auth queries are actually rooted at uid(queryVar) — i.e. they
				// use direct entity traversal (Job, Candidate, Company, Contact…).
				// Workspace-traversal auth rules (Note, EmailOutbound…) start from
				// type(Workspace) and never reference the entity root var, so
				// emitting varQry for them would produce a phantom variable that
				// causes Dgraph to reject the query with "defined but not used".
				if authQueriesReferenceVar(objAuthQueries, queryVar) {
					qrys = append(qrys, varQry)
				}
				qrys = append(qrys, objAuthQueries...)
				filts = append(filts, objfilter)
			}
		}

		// For an interface having Auth rules in some of the implementing types, len(qrys) = 0
		// indicates that None of the type satisfied the Auth rules, We must return Empty Query here.
		if implementingTypesHasAuthRules && len(qrys) == 0 {
			return []*dql.GraphQuery{{
				Attr: dgQuery[0].Attr + "()",
			}}, nil
		}

		// Join all the queries in qrys using OR filter and
		// append these queries into fldAuthQueries
		fldAuthQueries = append(fldAuthQueries, qrys...)
		objOrfilter := &dql.FilterTree{
			Op:    "or",
			Child: filts,
		}

		// if filts is non empty, which means it was a query on interface
		// having Either any of the types satisfying auth rules or having
		// some type with no Auth rules, In this case, the query will be different
		// and will look somewhat like this:
		// PostRoot as var(func: uid(Post1)) @filter((uid(QuestionAuth2) OR uid(AnswerAuth4)))
		if len(filts) > 0 {
			filter = objOrfilter
		}

		// Adding the case of Query on interface in which None of the implementing type have
		// Auth Query Rules, in that case, we also return simple query.
		if typ.IsInterface() && !implementingTypesHasAuthRules {
			return dgQuery, nil
		}

	}

	if len(fldAuthQueries) == 0 && !authRw.hasAuthRules {
		return dgQuery, nil
	}

	// If static evaluation already determined the result is Positive,
	// no dynamic auth queries or scaffolding are needed. Return the original query.
	// However, we must NOT skip the scaffolding (e.g. ContactRoot / Contact_N vars)
	// when child fields have their own auth rules (hasAuthRules == true): those
	// nested auth queries reference parentVarName which must be defined even though
	// the top-level RBAC has no restriction.
	// In that case we fall through and build the scaffolding with filter=nil.
	if rbacEval == schema.Positive && !authRw.hasAuthRules {
		return dgQuery, nil
	}

	// The original code handled this partially, but continued execution.
	// We are replacing it with a definitive early exit.
	if rbacEval == schema.Negative {
		// This should theoretically be handled by the caller, but as a safeguard.
		dgQuery[0].Attr = dgQuery[0].Attr + "()"
		// We can return an empty query, but the original dgQuery already has a `()`
		// suffix, so we can return that.
		return dgQuery, nil
	}

	// If we've made it this far, it means rbacEval was Uncertain and we have dynamic auth
	// rules to apply. Now, and only now, do we build the varQry and rootQry DQL variables.

	// build a query like
	//   Todo1 as var(func: ... ) @filter(...)
	// that has the filter from the user query in it.  This is then used as
	// the starting point for other auth queries.
	//
	// We already have the query, so just copy it and modify the original
	varQry := &dql.GraphQuery{
		Var:    authRw.varName,
		Attr:   "var",
		Func:   dgQuery[0].Func,
		Filter: dgQuery[0].Filter,
	}

	// build the root auth query like
	//   TodoRoot as var(func: uid(Todo1), orderasc: ..., first: ..., offset: ...) @filter(... type auth queries ...)
	// that has the order and pagination params from user query in it and filter set to auth
	// queries built for this type. This is then used as the starting point for user query and
	// auth queries for children.
	// if @cascade directive is present in the user query then pagination and order are applied only
	// on the user query and not on root query.
	rootQry := &dql.GraphQuery{
		Var:  authRw.parentVarName,
		Attr: "var",
		Func: &dql.Function{
			Name: "uid",
			Args: []dql.Arg{{Value: authRw.varName}},
		},
		Filter: filter,
	}

	// The user query doesn't need the filter parameter anymore,
	// as it has been taken care of by the var and root queries generated above.
	// But, it still needs the order parameter, even though it is also applied in root query.
	// So, not setting order to nil.
	dgQuery[0].Filter = nil

	// if @cascade is not applied on the user query at root then shift pagination arguments
	// from user query to root query for optimization and copy the order arguments for paginated
	// query to work correctly.
	if len(dgQuery[0].Cascade) == 0 {
		rootQry.Args = dgQuery[0].Args
		dgQuery[0].Args = nil
		rootQry.Order = dgQuery[0].Order
	}

	// The user query starts from the root query generated above and so gets filtered
	// input from auth processing, so now we build
	//   queryTodo(func: uid(TodoRoot), ...) { ... }
	dgQuery[0].Func = &dql.Function{
		Name: "uid",
		Args: []dql.Arg{{Value: authRw.parentVarName}},
	}

	// Deduplicate semantically identical auth var blocks (e.g. multiple
	// branches each emitting the same IAMRole / User / Workspace filter).
	// The substitution map returned here is applied to `filter` so that
	// any root auth var names that were deduplicated are also updated in
	// the top-level filter before it is attached to rootQry.
	fldAuthQueries, authVarSubst := deduplicateAuthVarBlocks(fldAuthQueries)
	if len(authVarSubst) > 0 {
		applyAuthVarSubst(filter, authVarSubst)
		// Also apply the substitution to all filter-path var blocks that were
		// added to dgQuery by addArgumentsToField / addFilter BEFORE this
		// function was called (dgQuery[0] is the main selection query — its
		// filter has already been cleared above, so only [1:] need updating).
		// Without this step, a @filter on blocks like
		//   data_and_0_and_2_inGroup_inWorkspaceRoot as var(…) @filter(uid(Workspace_Auth14)…)
		// still references the deduplicated-away var name (e.g. Workspace_Auth14)
		// even after the definition block has been substituted out, causing Dgraph
		// to report "Some variables are used but not defined".
		applyAuthVarSubstToQueries(dgQuery[1:], authVarSubst)
		// Also update cascadeVarCache so future cache hits return the surviving
		// canonical var name, not the deduplicated-away one. Without this, a
		// selectionAuth call that hits a cached entry referencing the removed var
		// (e.g. Workspace_Auth15) emits uid(Workspace_Auth15) in its filter with
		// no corresponding definition block, causing "used but not defined".
		for key, varName := range authRw.cascadeVarCache {
			if canonical, ok := authVarSubst[varName]; ok {
				authRw.cascadeVarCache[key] = canonical
			}
		}
	}

	// The final query that includes the user's filter and auth processing is thus like
	//
	// queryTodo(func: uid(Todo1)) @filter(uid(Todo2) AND uid(Todo3)) { ... }
	// Todo1 as var(func: ... ) @filter(...)
	// Todo2 as var(func: uid(Todo1)) @cascade { ...auth query 1... }
	// Todo3 as var(func: uid(Todo1)) @cascade { ...auth query 2... }
	// Ordering: [dgQuery(main+filterVars), rootQry(defines parentVarName), varQry(defines varName), fldAuthQueries...]
	// DQL requires that rootQry comes BEFORE any block that uses uid(parentVarName).
	// fldAuthQueries are already correct (they reference varName, not parentVarName directly).
	ret := append(dgQuery, rootQry, varQry)
	ret = append(ret, fldAuthQueries...)
	return ret, authVarSubst
}

func (authRw *authRewriter) addVariableUIDFunc(q *dql.GraphQuery) {
	varName := authRw.parentVarName
	if authRw.varName != "" {
		varName = authRw.varName
	}

	q.Func = &dql.Function{
		Name: "uid",
		Args: []dql.Arg{{Value: varName}},
	}
}

func (authRw *authRewriter) getRootFunc(cascadeWrapType string) *dql.Function {
	if authRw != nil && authRw.forceForward && authRw.varName != "" {
		return &dql.Function{
			Name: "uid",
			Args: []dql.Arg{{Value: authRw.varName}},
		}
	}
	return &dql.Function{
		Name: "type",
		Args: []dql.Arg{{Value: cascadeWrapType}},
	}
}

// authQueriesReferenceVar reports whether any of the given DQL auth var blocks
// use uid(varName) as their root traversal function.
//
// Direct entity auth rules (e.g. for Job, Candidate, Company, Contact) anchor
// their auth var at the entity root:
//
//	Job_Auth2 as var(func: uid(Job_1)) @filter(...) @cascade
//
// Workspace-traversal auth rules (e.g. for Note, EmailOutbound) start from a
// workspace or owner type instead:
//
//	Note_Auth18 as var(func: type(Workspace)) @filter(...) @cascade
//
// Only when at least one auth query IS rooted at uid(varName) should the
// corresponding root type-scan block (e.g. "Note_17 as var(func: type(Note))")
// be emitted. If none of the auth queries reference varName, emitting the
// type-scan block would produce a phantom variable that Dgraph rejects with
// "Some variables are defined but not used".
func authQueriesReferenceVar(queries []*dql.GraphQuery, varName string) bool {
	for _, q := range queries {
		if q == nil || q.Func == nil || q.Func.Name != "uid" {
			continue
		}
		for _, arg := range q.Func.Args {
			if arg.Value == varName {
				return true
			}
		}
	}
	return false
}

// deduplicateSelectionAuth deduplicates auth var blocks within selectionAuth and
// propagates any new substitutions to dgQuery filter-path var blocks (those added by
// addArgumentsToField / addFilter before the auth pass).
//
// Background: addAuthQueries only deduplicates the root type's own auth var blocks
// (fldAuthQueries). Field-level auth blocks (Group, Contact, UpdateRecord …) are
// appended as selectionAuth AFTER that dedup runs, so identical leaf vars such as
//
//	Group_Auth3_hasIAMBinding_forRole  (type(IAMRole) @filter(eq(IAMRole.permission,…)))
//	Group_Auth4_hasIAMBinding_forRole  (identical filter, different var name)
//	Group_Auth8_hasIAMBinding_forRole  (identical, inside workspace cascade)
//
// are never merged. This helper fixes that by running deduplicateAuthVarBlocks over
// selectionAuth, then merging the resulting substitution map (selSubst) into the
// existing rootSubst returned by addAuthQueries so that the combined map can be
// applied to selectionAuth in one final applyAuthVarSubstToQueries call.
//
// Returns the pruned selectionAuth and the merged substitution map.
func deduplicateSelectionAuth(
	selectionAuth []*dql.GraphQuery,
	dgQuery []*dql.GraphQuery,
	rootSubst map[string]string,
) ([]*dql.GraphQuery, map[string]string) {
	if len(selectionAuth) == 0 {
		return selectionAuth, rootSubst
	}
	selectionAuth, selSubst := deduplicateAuthVarBlocks(selectionAuth)
	if len(selSubst) == 0 {
		return selectionAuth, rootSubst
	}
	// Propagate new substitutions to ALL dgQuery blocks, including dgQuery[0]
	// (the main query). Child nodes in dgQuery[0] carry @filter(uid(AuthVar))
	// trees that reference auth vars generated by addSelectionSetFrom. When
	// deduplication drops e.g. Tag_51 and maps it to Tag_44, those child
	// filters must be updated or Dgraph will report "variable used but not
	// defined". Previously only dgQuery[1:] (filter-path var blocks added by
	// addFilter) were updated, leaving dgQuery[0]'s child filters stale.
	if len(dgQuery) > 0 {
		applyAuthVarSubstToQueries(dgQuery, selSubst)
	}
	// Merge selSubst into rootSubst so callers have one unified map.
	if rootSubst == nil {
		return selectionAuth, selSubst
	}
	for k, v := range selSubst {
		rootSubst[k] = v
	}
	return selectionAuth, rootSubst
}

func queryAuthSelector(t schema.Type) *schema.RuleNode {
	auth := t.AuthRules()
	if auth == nil || auth.Rules == nil {
		return nil
	}

	return auth.Rules.Query
}

// passwordAuthSelector is used as auth selector for checkPassword queries
func passwordAuthSelector(t schema.Type) *schema.RuleNode {
	auth := t.AuthRules()
	if auth == nil || auth.Rules == nil {
		return nil
	}

	return auth.Rules.Password
}

func (authRw *authRewriter) rewriteAuthQueries(typ schema.Type) ([]*dql.GraphQuery, *dql.FilterTree) {
	if authRw == nil || authRw.isWritingAuth {
		return nil, nil
	}

	return (&authRewriter{
		authVariables:        authRw.authVariables,
		varGen:               authRw.varGen,
		isWritingAuth:        true,
		varName:              authRw.varName,
		selector:             authRw.selector,
		parentVarName:        authRw.parentVarName,
		hasAuthRules:         authRw.hasAuthRules,
		cascadeVarCache:      authRw.cascadeVarCache,
		cascadeAuthorityType: authRw.cascadeAuthorityType,
		forceForward:         authRw.forceForward,
	}).rewriteRuleNode(typ, authRw.selector(typ))
}

func (authRw *authRewriter) evaluateStaticRules(typ schema.Type) schema.RuleResult {
	if authRw == nil || authRw.isWritingAuth {
		return schema.Uncertain
	}

	rn := authRw.selector(typ)
	return rn.EvaluateStatic(authRw.authVariables)
}

// rewriteCascadeBundle handles a multi-level cascade AND node where
// CascadeBundlePred is set. This node is produced by cascadeAuthRuleForEdge
// when an authority type (e.g. Group) itself has incoming cascade edges
// (e.g. Group→Workspace). The AND children are:
//
//   - The PRIMARY leaf: CascadeEdgePred == CascadeBundlePred (e.g. GroupMember.inGroup).
//     Its Rule is the authority type's own @auth query (e.g. queryGroup {...}).
//   - GRANDPARENT leaves: each has its own CascadeEdgePred (e.g. WorkspaceMember.inWorkspace).
//     These must be applied as uid_in filters ON the primary authority var, NOT on the
//     child type (Company has no WorkspaceMember.inWorkspace edge).
//
// Correct DQL for Company→Group→Workspace:
//
//	CompanyRoot @filter(uid_in(GroupMember.inGroup, uid(Company_Auth2)))
//	Company_Auth2 as var(func: type(Group)) @filter(uid(Group_Auth) AND uid_in(WorkspaceMember.inWorkspace, uid(Workspace_Auth))) @cascade {...}
//
// Instead of the flat (incorrect):
//
//	CompanyRoot @filter(uid_in(GroupMember.inGroup, uid(A)) AND uid_in(WorkspaceMember.inWorkspace, uid(B)))
func (authRw *authRewriter) rewriteCascadeBundle(
	typ schema.Type,
	rn *schema.RuleNode,
) ([]*dql.GraphQuery, *dql.FilterTree) {

	if rn.EvaluateStatic(authRw.authVariables) == schema.Negative {
		return nil, nil
	}

	bundlePred := rn.CascadeBundlePred

	// Use the authority type's @cascadeAuthPolicy(aggregation) stamped on the bundle
	// node at expansion time. When Group declares aggregation:"or", grandparent uid_in
	// filters (e.g. inWorkspace→Workspace) are ORed onto the Group authority var's
	// filter instead of ANDed, correctly mirroring Group's own access policy.
	grandparentOp := "and"
	if rn.CascadeAuthAggregation == "or" {
		grandparentOp = "or"
	}

	// Separate the primary leaf (CascadeEdgePred == bundlePred) from grandparent leaves.
	var primaryLeaf *schema.RuleNode
	var grandparentLeaves []*schema.RuleNode
	for _, child := range rn.And {
		if child.CascadeEdgePred == bundlePred {
			primaryLeaf = child
		} else {
			grandparentLeaves = append(grandparentLeaves, child)
		}
	}

	if primaryLeaf == nil {
		// Fallback: no primary leaf found — process as regular AND.
		var qrys []*dql.GraphQuery
		var filts []*dql.FilterTree
		for _, child := range rn.And {
			q, f := authRw.rewriteRuleNode(typ, child)
			qrys = append(qrys, q...)
			if f != nil {
				filts = append(filts, f)
			}
		}
		if len(filts) == 0 {
			return qrys, nil
		}
		if len(filts) == 1 {
			return qrys, filts[0]
		}
		return qrys, &dql.FilterTree{Op: "and", Child: filts}
	}

	// CascadeThroughType: this is a synthetic pass-all leaf for a through-node
	// (e.g. Company with no own @auth). Emit var(func: type(Company)) with no
	// cascade filter; grandparent uid_in filters will be applied onto it below.
	if primaryLeaf.CascadeThroughType != "" {
		varName := authRw.varGen.Next(typ, "", "", authRw.isWritingAuth)
		r1 := []*dql.GraphQuery{{
			Var:  varName,
			Attr: "var",
			Func: authRw.getRootFunc(primaryLeaf.CascadeThroughType),
		}}
		// Apply grandparent uid_in filters onto this through-node var.
		for _, gpLeaf := range grandparentLeaves {
			if gpLeaf.EvaluateStatic(authRw.authVariables) == schema.Negative {
				continue
			}
			gpQrys, gpFilter := authRw.rewriteRuleNode(typ, gpLeaf)
			r1 = append(r1, gpQrys...)
			if gpFilter != nil {
				if r1[0].Filter == nil {
					r1[0].Filter = gpFilter
				} else {
					r1[0].Filter = &dql.FilterTree{
						Op:    "and",
						Child: []*dql.FilterTree{r1[0].Filter, gpFilter},
					}
				}
			}
		}
		if authRw.cascadeVarCache != nil {
			authRw.cascadeVarCache[cascadeCacheKey{rn: primaryLeaf, typName: typ.Name()}] = varName
		}
		return r1, &dql.FilterTree{
			Func: &dql.Function{
				Name: "uid_in",
				Args: []dql.Arg{
					{Value: bundlePred},
					{Value: "uid(" + varName + ")"},
				},
			},
		}
	}

	// Check static evaluation of primary leaf (normal auth rule leaf).
	if primaryLeaf.EvaluateStatic(authRw.authVariables) == schema.Negative {
		return nil, nil
	}

	// Generate the primary authority var from primaryLeaf.Rule.
	qry := primaryLeaf.Rule.AuthFor(authRw.authVariables)
	if qry == nil {
		return nil, nil
	}

	// Cache check — skip if already cached (fall through to generate fresh var).
	if cached, ok := authRw.cascadeVarCache[cascadeCacheKey{rn: primaryLeaf, typName: typ.Name()}]; ok {
		_ = cached
	}

	varName := authRw.varGen.Next(typ, "", "", authRw.isWritingAuth)
	r1 := rewriteAsQuery(qry, authRw, varName)
	r1[0].Var = varName
	r1[0].Attr = "var"
	r1[0].Func = authRw.getRootFunc(qry.Type().DgraphName())

	// Apply grandparent uid_in filters ONTO the primary authority var's filter.
	// Each grandparent leaf has CascadeEdgePred for a predicate on the AUTHORITY type
	// (e.g. WorkspaceMember.inWorkspace on Group), not on the child type (Company).
	// The grandparentOp ("and" or "or") is read from the authority type's
	// @cascadeAuthPolicy(aggregation): when Group declares aggregation:"or", the
	// workspace uid_in filter is ORed with Group's own IAMResource filter on the
	// Group var, correctly mirroring Group's own access policy.
	for _, gpLeaf := range grandparentLeaves {
		if gpLeaf.EvaluateStatic(authRw.authVariables) == schema.Negative {
			continue
		}
		gpQrys, gpFilter := authRw.rewriteRuleNode(typ, gpLeaf)
		r1 = append(r1, gpQrys...)
		if gpFilter != nil {
			if r1[0].Filter == nil {
				r1[0].Filter = gpFilter
			} else {
				r1[0].Filter = &dql.FilterTree{
					Op:    grandparentOp,
					Child: []*dql.FilterTree{r1[0].Filter, gpFilter},
				}
			}
		}
	}

	if len(r1[0].Cascade) == 0 {
		r1[0].Cascade = append(r1[0].Cascade, "__all__")
	}

	if authRw.cascadeVarCache != nil {
		authRw.cascadeVarCache[cascadeCacheKey{rn: primaryLeaf, typName: typ.Name()}] = varName
	}

	return r1, &dql.FilterTree{
		Func: &dql.Function{
			Name: "uid_in",
			Args: []dql.Arg{
				{Value: bundlePred},
				{Value: "uid(" + varName + ")"},
			},
		},
	}
}

func (authRw *authRewriter) rewriteRuleNode(
	typ schema.Type,
	rn *schema.RuleNode) ([]*dql.GraphQuery, *dql.FilterTree) {

	if typ == nil || rn == nil {
		return nil, nil
	}

	nodeList := func(
		typ schema.Type,
		rns []*schema.RuleNode) ([]*dql.GraphQuery, []*dql.FilterTree) {

		var qrys []*dql.GraphQuery
		var filts []*dql.FilterTree
		for _, orRn := range rns {
			q, f := authRw.rewriteRuleNode(typ, orRn)
			qrys = append(qrys, q...)
			if f != nil {
				filts = append(filts, f)
			}
		}
		return qrys, filts
	}

	switch {
	case rn.CascadeWrapPred != "":
		// CascadeWrap node: uid_in(CascadeWrapPred, uid(authorityVar)).
		//
		// The authority type's full auth is in CascadeWrapInner.
		// Three cases based on the shape of the inner tree:
		//
		//  (A) Leaf inner: CascadeWrapInner has a single Rule leaf with no
		//      CascadeWrapPred / CascadeEdgePred. Build the authority var directly:
		//        authorityVar as var(func: type(CascadeWrapType)) @cascade { <rule body> }
		//
		//  (B) And inner with a Rule leaf + CascadeWrap children: build the
		//      authority var from the Rule leaf (inline @cascade body) and apply
		//      the CascadeWrap uid_in filters as @filter on the authority block:
		//        authorityVar as var(func: type(CascadeWrapType))
		//          @filter(uid_in(cascPred, uid(inner_var)) [AND ...])
		//          @cascade { <rule body> }
		//      This produces the same DQL format as the old rewriteCascadeBundle.
		//
		//  (C) Or/pure-compound inner: rewrite the inner tree to get
		//      (supportVars, innerFilter) then emit:
		//        authorityVar as var(func: type(CascadeWrapType)) @filter(innerFilter) @cascade
		//
		// Each nested CascadeWrap resets cascadeAuthorityType to its own CascadeWrapType so
		// that plain Rule-leaf nodes inside the authority's OR/AND tree produce
		// var(func: type(Group)) rather than the incorrect var(func: uid(JobAd_1)).
		if rn.EvaluateStatic(authRw.authVariables) == schema.Negative {
			return nil, nil
		}
		if rn.CascadeWrapInner == nil {
			return nil, nil
		}

		inner := rn.CascadeWrapInner

		// Cache check: reuse authority var if already generated in this request.
		// Note: We use typ.Name() (the concrete parent type being authorized, e.g. User or NoteType)
		// rather than rn.CascadeWrapType (the destination type, e.g. Workspace) to prevent
		// interface cache collisions across sibling types implementing the same interface.
		// Collisions would result in circular variable dependency cycles and runtime DQL execution failures.
		if authRw.cascadeVarCache != nil {
			if cached, ok := authRw.cascadeVarCache[cascadeCacheKey{rn: inner, typName: typ.Name()}]; ok {
				if rn.CascadeWrapReverse && !authRw.forceForward {
					reverseVar := authRw.varGen.Next(typ, "", "", authRw.isWritingAuth)
					invPred := rn.CascadeInversePred
					if invPred == "" {
						invPred = "~" + rn.CascadeWrapPred
					}
					revQry := &dql.GraphQuery{
						Var:  "",
						Attr: "var",
						Func: &dql.Function{
							Name: "uid",
							Args: []dql.Arg{{Value: cached}},
						},
						Children: []*dql.GraphQuery{
							{
								Attr: invPred,
								Var:  reverseVar + "_uids",
							},
						},
					}
					return []*dql.GraphQuery{revQry}, &dql.FilterTree{
						Func: &dql.Function{
							Name: "uid",
							Args: []dql.Arg{{Value: reverseVar + "_uids"}},
						},
					}
				}
				return nil, &dql.FilterTree{
					Func: &dql.Function{
						Name: "uid_in",
						Args: []dql.Arg{
							{Value: rn.CascadeWrapPred},
							{Value: "uid(" + cached + ")"},
						},
					},
				}
			}
		}

		varName := authRw.varGen.Next(typ, "", "", authRw.isWritingAuth)

		var subsetQrys []*dql.GraphQuery
		innerAuthRw := *authRw
		innerAuthRw.cascadeAuthorityType = rn.CascadeWrapType

		if authRw.forceForward && authRw.varName != "" {
			groupSubsetVar := authRw.varGen.Next(typ, "subset", "", authRw.isWritingAuth)
			subsetQry := &dql.GraphQuery{
				Attr: "var",
				Func: &dql.Function{
					Name: "uid",
					Args: []dql.Arg{{Value: authRw.varName}},
				},
				Children: []*dql.GraphQuery{{
					Attr: rn.CascadeWrapPred,
					Var:  groupSubsetVar,
				}},
			}
			subsetQrys = append(subsetQrys, subsetQry)
			innerAuthRw.varName = groupSubsetVar
		}

		// ── Case A: simple Rule leaf (no CascadeWrapPred / CascadeEdgePred). ──
		if inner.Rule != nil && inner.CascadeWrapPred == "" && inner.CascadeEdgePred == "" {
			if inner.EvaluateStatic(authRw.authVariables) == schema.Negative {
				return nil, nil
			}
			qry := inner.Rule.AuthFor(authRw.authVariables)
			if qry == nil {
				return nil, nil
			}
			r1 := rewriteAsQuery(qry, &innerAuthRw, varName)
			r1[0].Var = varName
			r1[0].Attr = "var"
			r1[0].Func = innerAuthRw.getRootFunc(rn.CascadeWrapType)
			if len(r1[0].Cascade) == 0 {
				r1[0].Cascade = append(r1[0].Cascade, "__all__")
			}
			if authRw.cascadeVarCache != nil {
				authRw.cascadeVarCache[cascadeCacheKey{rn: inner, typName: typ.Name()}] = varName
			}
			if rn.CascadeWrapReverse && !authRw.forceForward {
				reverseVar := authRw.varGen.Next(typ, "", "", authRw.isWritingAuth)
				invPred := rn.CascadeInversePred
				if invPred == "" {
					invPred = "~" + rn.CascadeWrapPred
				}
				revQry := &dql.GraphQuery{
					Var:  "",
					Attr: "var",
					Func: &dql.Function{
						Name: "uid",
						Args: []dql.Arg{{Value: varName}},
					},
					Children: []*dql.GraphQuery{
						{
							Attr: invPred,
							Var:  reverseVar + "_uids",
						},
					},
				}
				r1 = append(r1, revQry)
				if len(subsetQrys) > 0 {
					r1 = append(subsetQrys, r1...)
				}
				return r1, &dql.FilterTree{
					Func: &dql.Function{
						Name: "uid",
						Args: []dql.Arg{{Value: reverseVar + "_uids"}},
					},
				}
			}
			if len(subsetQrys) > 0 {
				r1 = append(subsetQrys, r1...)
			}
			return r1, &dql.FilterTree{
				Func: &dql.Function{
					Name: "uid_in",
					Args: []dql.Arg{
						{Value: rn.CascadeWrapPred},
						{Value: "uid(" + varName + ")"},
					},
				},
			}
		}

		// ── Case B: And inner with a Rule leaf + CascadeWrap children. ──
		// Build the authority var from the primary Rule leaf (inline @cascade body),
		// and apply the CascadeWrap children as uid_in @filter entries.
		// This matches the output format the old rewriteCascadeBundle produced.
		if len(inner.And) > 0 {
			// Split And children into: primary Rule leaf vs. CascadeWrap nodes.
			var ruleLeaf *schema.RuleNode
			var cascadeWrapChildren []*schema.RuleNode
			var otherChildren []*schema.RuleNode
			for _, child := range inner.And {
				if child.Rule != nil && child.CascadeWrapPred == "" && child.CascadeEdgePred == "" {
					if ruleLeaf == nil {
						ruleLeaf = child
					} else {
						otherChildren = append(otherChildren, child)
					}
				} else if child.CascadeWrapPred != "" {
					cascadeWrapChildren = append(cascadeWrapChildren, child)
				} else {
					otherChildren = append(otherChildren, child)
				}
			}

			if ruleLeaf != nil && len(cascadeWrapChildren) > 0 && len(otherChildren) == 0 {
				// Perfect case B: exactly one Rule leaf + CascadeWrap children.
				if inner.EvaluateStatic(authRw.authVariables) == schema.Negative {
					return nil, nil
				}
				if ruleLeaf.EvaluateStatic(authRw.authVariables) == schema.Negative {
					return nil, nil
				}
				qry := ruleLeaf.Rule.AuthFor(authRw.authVariables)
				if qry == nil {
					return nil, nil
				}
				// Build base var from the rule leaf.
				r1 := rewriteAsQuery(qry, &innerAuthRw, varName)
				r1[0].Var = varName
				r1[0].Attr = "var"
				r1[0].Func = innerAuthRw.getRootFunc(rn.CascadeWrapType)

				// Rewrite each CascadeWrap child and apply uid_in as @filter on r1[0].
				for _, cwChild := range cascadeWrapChildren {
					if cwChild.EvaluateStatic(authRw.authVariables) == schema.Negative {
						continue
					}
					cwQrys, cwFilter := innerAuthRw.rewriteRuleNode(typ, cwChild)
					r1 = append(r1, cwQrys...)
					if cwFilter != nil {
						if r1[0].Filter == nil {
							r1[0].Filter = cwFilter
						} else {
							r1[0].Filter = &dql.FilterTree{
								Op:    "and",
								Child: []*dql.FilterTree{r1[0].Filter, cwFilter},
							}
						}
					}
				}

				if len(r1[0].Cascade) == 0 {
					r1[0].Cascade = append(r1[0].Cascade, "__all__")
				}
				if authRw.cascadeVarCache != nil {
					authRw.cascadeVarCache[cascadeCacheKey{rn: inner, typName: typ.Name()}] = varName
				}
				if rn.CascadeWrapReverse && !authRw.forceForward {
					reverseVar := authRw.varGen.Next(typ, "", "", authRw.isWritingAuth)
					invPred := rn.CascadeInversePred
					if invPred == "" {
						invPred = "~" + rn.CascadeWrapPred
					}
					revQry := &dql.GraphQuery{
						Var:  "",
						Attr: "var",
						Func: &dql.Function{
							Name: "uid",
							Args: []dql.Arg{{Value: varName}},
						},
						Children: []*dql.GraphQuery{
							{
								Attr: invPred,
								Var:  reverseVar + "_uids",
							},
						},
					}
					r1 = append(r1, revQry)
					if len(subsetQrys) > 0 {
						r1 = append(subsetQrys, r1...)
					}
					return r1, &dql.FilterTree{
						Func: &dql.Function{
							Name: "uid",
							Args: []dql.Arg{{Value: reverseVar + "_uids"}},
						},
					}
				}
				if len(subsetQrys) > 0 {
					r1 = append(subsetQrys, r1...)
				}
				return r1, &dql.FilterTree{
					Func: &dql.Function{
						Name: "uid_in",
						Args: []dql.Arg{
							{Value: rn.CascadeWrapPred},
							{Value: "uid(" + varName + ")"},
						},
					},
				}
			}
		}

		// ── Case C: Or/pure-compound inner. ──
		// Rewrite the inner tree to get support vars and a filter expression.
		//
		// Create a copy of the rewriter with cascadeAuthorityType set to this wrap's type.
		// This propagates to plain Rule-leaf nodes inside the inner OR/AND tree so they
		// produce var(func: type(CascadeWrapType)) — e.g. type(Group) — instead of the
		// default var(func: uid(parentVarName)) = var(func: uid(JobAd_1)).
		// Group UIDs and JobAd UIDs are disjoint, so the old code always produced zero
		// results when the inner filter was used on var(func: type(Group)).
		innerQrys, innerFilter := innerAuthRw.rewriteRuleNode(typ, inner)
		if innerFilter == nil && len(innerQrys) == 0 {
			return nil, nil
		}

		// Build: varName as var(func: type(CascadeWrapType)) @filter(innerFilter) @cascade
		authBlock := &dql.GraphQuery{
			Var:     varName,
			Attr:    "var",
			Func:    innerAuthRw.getRootFunc(rn.CascadeWrapType),
			Filter:  innerFilter,
			Cascade: []string{"__all__"},
		}

		if authRw.cascadeVarCache != nil {
			authRw.cascadeVarCache[cascadeCacheKey{rn: inner, typName: typ.Name()}] = varName
		}

		// Place authBlock first so it is rendered before its support vars.
		allQrys := append([]*dql.GraphQuery{authBlock}, innerQrys...)
		if len(subsetQrys) > 0 {
			allQrys = append(subsetQrys, allQrys...)
		}
		if rn.CascadeWrapReverse && !authRw.forceForward {
			reverseVar := authRw.varGen.Next(typ, "", "", authRw.isWritingAuth)
			invPred := rn.CascadeInversePred
			if invPred == "" {
				invPred = "~" + rn.CascadeWrapPred
			}
			revQry := &dql.GraphQuery{
				Var:  "",
				Attr: "var",
				Func: &dql.Function{
					Name: "uid",
					Args: []dql.Arg{{Value: varName}},
				},
				Children: []*dql.GraphQuery{
					{
						Attr: invPred,
						Var:  reverseVar + "_uids",
					},
				},
			}
			allQrys = append(allQrys, revQry)
			return allQrys, &dql.FilterTree{
				Func: &dql.Function{
					Name: "uid",
					Args: []dql.Arg{{Value: reverseVar + "_uids"}},
				},
			}
		}
		return allQrys, &dql.FilterTree{
			Func: &dql.Function{
				Name: "uid_in",
				Args: []dql.Arg{
					{Value: rn.CascadeWrapPred},
					{Value: "uid(" + varName + ")"},
				},
			},
		}
	case len(rn.And) > 0:
		// if there is atleast one RBAC rule which is false, then this
		// whole And block needs to be ignored.
		andStatic := rn.EvaluateStatic(authRw.authVariables)
		if andStatic == schema.Negative {
			return nil, nil
		}

		// CascadeBundlePred marks a multi-level cascade bundle created by
		// cascadeAuthRuleForEdge when a parent type itself has incoming cascade
		// edges (e.g. Company→Group→Workspace). The primary leaf has
		// CascadeEdgePred == CascadeBundlePred; grandparent leaves have their
		// own CascadeEdgePred values. To produce correct DQL, grandparent
		// uid_in filters must be applied to the PRIMARY authority var (e.g.
		// the Group var), NOT to the child type (Company) directly — Company
		// has no WorkspaceMember.inWorkspace edge.
		if rn.CascadeBundlePred != "" {
			return authRw.rewriteCascadeBundle(typ, rn)
		}

		qrys, filts := nodeList(typ, rn.And)
		if len(filts) == 0 {
			return qrys, nil
		}
		if len(filts) == 1 {
			return qrys, filts[0]
		}
		return qrys, &dql.FilterTree{
			Op:    "and",
			Child: filts,
		}
	case len(rn.Or) > 0:
		// First, check if any of the children are statically Positive.
		// If so, this OR condition is already satisfied and needs no filter.
		orStatic := rn.EvaluateStatic(authRw.authVariables)
		if orStatic == schema.Positive {
			return nil, nil
		}
		qrys, filts := nodeList(typ, rn.Or)
		if len(filts) == 0 {
			return qrys, nil
		}
		if len(filts) == 1 {
			return qrys, filts[0]
		}
		return qrys, &dql.FilterTree{
			Op:    "or",
			Child: filts,
		}
	case rn.Not != nil:
		qrys, filter := authRw.rewriteRuleNode(typ, rn.Not)
		if filter == nil {
			return qrys, nil
		}
		return qrys, &dql.FilterTree{
			Op:    "not",
			Child: []*dql.FilterTree{filter},
		}
	case rn.Rule != nil && rn.CascadeEdgePred != "":
		// CascadeEdgePred is set by cascade_auth_expand.go withCascadeEdgePred.
		// It means this rule is a cascade scoping rule: the authority type
		// (e.g. Workspace) is queried as a flat var and the child type (e.g.
		// Group) is scoped using uid_in(WorkspaceMember.inWorkspace, uid(AuthVar)).
		//
		// DQL output:
		//   Group_Auth3 as var(func: type(Workspace)) @cascade {
		//     Workspace.inUsers @filter(eq(User.email, "u@example.com"))
		//   }
		//   @filter( uid_in(WorkspaceMember.inWorkspace, uid(Group_Auth3)) )
		if rn.EvaluateStatic(authRw.authVariables) == schema.Negative {
			return nil, nil
		}

		qry := rn.Rule.AuthFor(authRw.authVariables)
		if qry == nil {
			// AuthFor returned nil — rule cannot be evaluated (e.g. required JWT
			// variable missing). Skip this cascade arm.
			return nil, nil
		}

		// Cache hit: this exact cascade rule was already processed in this request.
		// Reuse the previously generated cascade authority var — no new DQL blocks needed.
		if cached, ok := authRw.cascadeVarCache[cascadeCacheKey{rn: rn, typName: typ.Name()}]; ok {
			return nil, &dql.FilterTree{
				Func: &dql.Function{
					Name: "uid_in",
					Args: []dql.Arg{
						{Value: rn.CascadeEdgePred},
						{Value: "uid(" + cached + ")"},
					},
				},
			}
		}

		// Build the authority var query using a nested auth rewriter — the same
		// pattern as addSelectionSetFrom uses for nested field auth (line ~2285).
		// The outer authRw has isWritingAuth=true which causes addAuthQueries inside
		// rewriteAsQuery to skip (early-return at line 1041). That leaves the
		// authority type's own @auth rules unevaluated through the addAuthQueries
		// path, producing disconnected sub-var blocks or missing filters.
		//
		// Using isWritingAuth: false lets rewriteAsQuery → addAuthQueries run fully
		// for the authority type, generating a self-contained var query with correct
		// @filter references (e.g. @filter(uid(User_Auth6_hasIAMBinding))).
		varName := authRw.varGen.Next(typ, "", "", authRw.isWritingAuth)
		r1 := rewriteAsQuery(qry, authRw, varName)
		r1[0].Var = varName
		r1[0].Attr = "var"
		// Override the func to type(AuthorityType) — a flat type scan independent
		// of the child root.
		if qry != nil {
			r1[0].Func = &dql.Function{
				Name: "type",
				Args: []dql.Arg{{Value: qry.Type().DgraphName()}},
			}
		}

		// Apply the authority type's own @auth rules using the nested filter rewriter
		// pattern (matching addSelectionSetFrom ~line 2285). The outer authRw has
		// isWritingAuth=true which short-circuits addAuthQueries, so we create a fresh
		// rewriter with isWritingAuth=false to evaluate the authority's @auth rules.
		//
		// rewriteAuthQueries returns (authSubVarBlocks, filter):
		//   authSubVarBlocks — extra var blocks like "User_Auth6_hasIAMBinding as IAMBinding.forResource"
		//   filter           — @filter(uid(User_Auth6_hasIAMBinding)) to apply on r1[0]
		//
		// We apply the filter to r1[0].Filter (AND with any inline filter from the
		// cascade rule's own @cascade predicate selection) and append the sub-var blocks.
		if qry != nil {
			authorityType := qry.Type()
			cascadeAuthRw := &authRewriter{
				authVariables:   authRw.authVariables,
				varGen:          authRw.varGen,
				selector:        queryAuthSelector, // always query-auth for cascade authority
				varName:         varName,
				parentVarName:   varName,
				isWritingAuth:   true, // prevent recursive addAuthQueries wrapping
				hasAuthRules:    authRw.hasAuthRules,
				cascadeVarCache: authRw.cascadeVarCache, // share the cache
				forceForward:    authRw.forceForward,
			}
			authSubVars, authFilter := cascadeAuthRw.rewriteAuthQueries(authorityType)
			if authFilter != nil {
				if r1[0].Filter == nil {
					r1[0].Filter = authFilter
				} else {
					r1[0].Filter = &dql.FilterTree{
						Op:    "and",
						Child: []*dql.FilterTree{r1[0].Filter, authFilter},
					}
				}
			}
			r1 = append(r1, authSubVars...)
		}

		if len(r1[0].Cascade) == 0 {
			r1[0].Cascade = append(r1[0].Cascade, "__all__")
		}

		// Store in cache so subsequent calls for the same cascade edge rule
		// reuse this var and skip redundant DQL block generation.
		if authRw.cascadeVarCache != nil {
			authRw.cascadeVarCache[cascadeCacheKey{rn: rn, typName: typ.Name()}] = varName
		}

		// The filter on the child is uid_in(pred, uid(AuthVar)) — not uid(AuthVar).
		// This correctly scopes Group to only those nodes where the cascade edge
		// (WorkspaceMember.inWorkspace) points to an authorized Workspace node.
		return r1, &dql.FilterTree{
			Func: &dql.Function{
				Name: "uid_in",
				Args: []dql.Arg{
					{Value: rn.CascadeEdgePred},
					{Value: "uid(" + varName + ")"},
				},
			},
		}
	case rn.Rule != nil:
		ruleStatic := rn.EvaluateStatic(authRw.authVariables)
		if ruleStatic == schema.Negative {
			return nil, nil
		}

		// create a copy of the auth query that's specialized for the values from the JWT
		qry := rn.Rule.AuthFor(authRw.authVariables)

		varName := authRw.varGen.Next(typ, "", "", authRw.isWritingAuth)
		r1 := rewriteAsQuery(qry, authRw, varName)
		r1[0].Var = varName
		r1[0].Attr = "var"

		if authRw.cascadeAuthorityType != "" {
			// REVERTED: always use cascadeAuthorityType as the var root.
			r1[0].Func = &dql.Function{
				Name: "type",
				Args: []dql.Arg{{Value: authRw.cascadeAuthorityType}},
			}
			if len(r1[0].Cascade) == 0 {
				r1[0].Cascade = append(r1[0].Cascade, "__all__")
			}
		} else {
			// Default: the rule belongs to the queried type itself.
			// build
			// Todo2 as var(func: uid(Todo1)) @cascade { ...auth query 1... }
			if len(r1[0].Cascade) == 0 {
				r1[0].Cascade = append(r1[0].Cascade, "__all__")
			}
		}

		// return all queries, including the nested var queries.
		return r1, &dql.FilterTree{
			Func: &dql.Function{
				Name: "uid",
				Args: []dql.Arg{{Value: varName}},
			},
		}
	case rn.DQLRule != nil:
		return []*dql.GraphQuery{rn.DQLRule}, &dql.FilterTree{
			Func: &dql.Function{
				Name: "uid",
				Args: []dql.Arg{{Value: rn.DQLRule.Var}},
			},
		}
	}
	return nil, nil
}

func addTypeFilter(q *dql.GraphQuery, typ schema.Type) {
	thisFilter := &dql.FilterTree{
		Func: buildTypeFunc(typ.DgraphName()),
	}
	addToFilterTree(q, thisFilter)
}

func addToFilterTree(q *dql.GraphQuery, filter *dql.FilterTree) {
	if q.Filter == nil {
		q.Filter = filter
	} else {
		q.Filter = &dql.FilterTree{
			Op:    "and",
			Child: []*dql.FilterTree{q.Filter, filter},
		}
	}
}

func addUIDFunc(q *dql.GraphQuery, uids []uint64) {
	q.Func = &dql.Function{
		Name: "uid",
		UID:  uids,
	}
}

func addEqFunc(q *dql.GraphQuery, dgPred string, values []interface{}) {
	args := []dql.Arg{{Value: dgPred}}
	for _, v := range values {
		args = append(args, dql.Arg{Value: maybeQuoteArg("eq", v)})
	}
	q.Func = &dql.Function{
		Name: "eq",
		Args: args,
	}
}

func addTypeFunc(q *dql.GraphQuery, typ string) {
	q.Func = buildTypeFunc(typ)
}

func buildTypeFunc(typ string) *dql.Function {
	return &dql.Function{
		Name: "type",
		Args: []dql.Arg{{Value: typ}},
	}
}

// Builds parentQry for auth rules and selectionQry to aggregate all filter and
// auth rules. This is used to build common auth rules by addSelectionSetFrom and
// buildAggregateFields function.
func buildCommonAuthQueries(
	f schema.Field,
	auth *authRewriter,
	parentSelectionName string) commonAuthQueryVars {
	// This adds the following query.
	//	var(func: uid(Ticket1)) {
	//		User4 as Ticket.assignedTo
	//	}
	// where `Ticket1` is the nodes selected at parent level after applying auth and `User4` is the
	// nodes we need on the current level.
	parentQry := &dql.GraphQuery{
		Func: &dql.Function{
			Name: "uid",
			Args: []dql.Arg{{Value: parentSelectionName}},
		},
		Attr:     "var",
		Children: []*dql.GraphQuery{{Attr: f.ConstructedForDgraphPredicate(), Var: auth.varName}},
	}

	// This query aggregates all filters and auth rules and is used by root query to filter
	// the final nodes for the current level.
	// User3 as var(func: uid(User4)) @filter((eq(User.username, "User1") AND (...Auth Filter))))
	selectionQry := &dql.GraphQuery{
		Var:  auth.parentVarName,
		Attr: "var",
		Func: &dql.Function{
			Name: "uid",
			Args: []dql.Arg{{Value: auth.varName}},
		},
	}

	return commonAuthQueryVars{
		parentQry:    parentQry,
		selectionQry: selectionQry,
	}
}

// buildAggregateFields builds DQL queries for aggregate fields like count, avg, max etc.
// It returns related DQL fields and Auth Queries which are then added to the final DQL query
// by the caller.
func buildAggregateFields(
	f schema.Field,
	auth *authRewriter) ([]*dql.GraphQuery, []*dql.GraphQuery) {
	constructedForType := f.ConstructedFor()
	constructedForDgraphPredicate := f.ConstructedForDgraphPredicate()

	// aggregateChildren contains the count query field and mainField (described below).
	// otherAggregateChildren contains other min,max,sum,avg fields.
	// These fields are considered separately as filters (auth and other filters) need to
	// be added to count fields and mainFields but not for other aggregate fields.
	var aggregateChildren []*dql.GraphQuery
	var otherAggregateChildren []*dql.GraphQuery
	// mainField contains the queried Aggregate Field and has all var fields inside it.
	// Eg. the mainQuery for
	// postsAggregate {
	//   titleMin
	// }
	// is
	// Author.postsAggregate : Author.posts {
	//   Author.postsAggregate_titleVar as Post.title
	//   ... other queried aggregate fields
	// }
	mainField := &dql.GraphQuery{
		Alias: f.DgraphAlias(),
		Attr:  constructedForDgraphPredicate,
	}

	// Filter for aggregate Fields. This is added to all count aggregate fields
	// and mainField
	fieldFilter, _ := f.ArgValue("filter").(map[string]interface{})
	_, varQry := addFilter(mainField, constructedForType, fieldFilter, auth, f.Alias())

	// Add type filter in case the Dgraph predicate for which the aggregate
	// field belongs to is a reverse edge
	if strings.HasPrefix(constructedForDgraphPredicate, "~") {
		addTypeFilter(mainField, f.ConstructedFor())
	}

	// isAggregateVarAdded is a map from field name to boolean. It is used to
	// ensure that a field is added to Var query at maximum once.
	// Eg. Even if scoreMax and scoreMin are queried, the corresponding field will
	// contain "scoreVar as Tweets.score" only once.
	isAggregateVarAdded := make(map[string]bool)

	// Iterate over fields queried inside aggregate.
	for _, aggregateField := range f.SelectionSet() {

		// Handle count fields inside aggregate fields.
		if aggregateField.Name() == "count" {
			aggregateChild := &dql.GraphQuery{
				Alias: aggregateField.DgraphAlias() + "_" + f.DgraphAlias(),
				Attr:  "count(" + constructedForDgraphPredicate + ")",
			}
			// Add filter to count aggregation field.
			addFilter(aggregateChild, constructedForType, fieldFilter, auth, f.Alias())

			// Add type filter in case the Dgraph predicate for which the aggregate
			// field belongs to is a reverse edge
			if strings.HasPrefix(constructedForDgraphPredicate, "~") {
				addTypeFilter(aggregateChild, f.ConstructedFor())
			}

			aggregateChildren = append(aggregateChildren, aggregateChild)
			continue
		}
		// Handle other aggregate functions than count
		aggregateFunctions := []string{"Max", "Min", "Sum", "Avg"}
		for _, function := range aggregateFunctions {
			aggregateFldName := aggregateField.Name()
			// A field can have at maximum one aggregation function as suffix.
			if strings.HasSuffix(aggregateFldName, function) {
				// constructedForField contains the field name for which aggregate function
				// has been queried. Eg. name for nameMax. Removing last 3 characters as all
				// aggregation functions have length 3
				constructedForField := aggregateFldName[:len(aggregateFldName)-3]
				// constructedForDgraphPredicate stores the Dgraph predicate for which aggregate function
				// has been queried. Eg. Post.name for nameMin
				constructedForDgraphPredicateField := aggregateField.DgraphPredicateForAggregateField()
				// Adding the corresponding var field if it has not been added before. isAggregateVarAdded
				// ensures that a var queried is added at maximum once.
				if !isAggregateVarAdded[constructedForField] {
					child := &dql.GraphQuery{
						Var:  f.DgraphAlias() + "_" + constructedForField + "Var",
						Attr: constructedForDgraphPredicateField,
					}
					// The var field is added to mainQuery. This adds the following DQL query.
					// Author.postsAggregate : Author.posts {
					//   Author.postsAggregate_nameVar as Post.name
					// }
					mainField.Children = append(mainField.Children, child)
					isAggregateVarAdded[constructedForField] = true
				}
				aggregateChild := &dql.GraphQuery{
					Alias: aggregateField.DgraphAlias() + "_" + f.DgraphAlias(),
					Attr: strings.ToLower(function) +
						"(val(" + "" + f.DgraphAlias() + "_" + constructedForField + "Var))",
				}
				// This adds the following DQL query
				// PostAggregateResult.nameMin_Author.postsAggregate : min(val(Author.postsAggregate_nameVar))
				otherAggregateChildren = append(otherAggregateChildren, aggregateChild)
				break
			}
		}
	}
	// mainField is only added as an aggregate child if it has any children fields inside it.
	// This ensures that if only count aggregation field is there, the mainField is not added.
	// As mainField contains only var fields. It is not needed in case of count.
	if len(mainField.Children) > 0 {
		aggregateChildren = append([]*dql.GraphQuery{mainField}, aggregateChildren...)
	}
	rbac := auth.evaluateStaticRules(constructedForType)
	if rbac == schema.Negative {
		return nil, nil
	}
	var parentVarName, parentQryName string
	if len(f.SelectionSet()) > 0 && !auth.isWritingAuth && auth.hasAuthRules {
		parentVarName = auth.parentVarName
		parentQryName = auth.varName
		auth.parentVarName = auth.varGen.Next(f.Type(), "", "", auth.isWritingAuth)
		auth.varName = auth.varGen.Next(f.Type(), "", "", auth.isWritingAuth)
	}
	var fieldAuth, retAuthQueries []*dql.GraphQuery
	var authFilter *dql.FilterTree
	if rbac == schema.Uncertain {
		fieldAuth, authFilter = auth.rewriteAuthQueries(constructedForType)
	}
	// At this stage aggregateChildren only contains the count aggregate fields and
	// possibly mainField. Auth filters are added to count aggregation fields and
	// mainField. Adding filters only for mainField is sufficient for other aggregate
	// functions as the aggregation functions use var from mainField.

	// Adds auth queries. The variable authQueriesAppended ensures that auth queries are
	// appended only once. This also merges auth filters and any other filters of count
	// aggregation fields / mainField.
	if len(f.SelectionSet()) > 0 && !auth.isWritingAuth && auth.hasAuthRules {
		commonAuthQueryVars := buildCommonAuthQueries(f, auth, parentVarName)
		// add child filter to parent query, auth filters to selection query and
		// selection query as a filter to child
		commonAuthQueryVars.selectionQry.Filter = authFilter
		var authQueriesAppended = false
		for _, aggregateChild := range aggregateChildren {
			if !authQueriesAppended {
				commonAuthQueryVars.parentQry.Children[0].Filter = aggregateChild.Filter
				// DQL requires definitions before uses. The dependency chain is:
				//   parentQry  → defines auth.varName (aggregate result var)
				//   fieldAuth  → uses auth.varName (auth var blocks for the field type)
				//   selectionQry → uses auth.varName AND authFilter vars from fieldAuth
				// Emit in this order so every var is defined before it is used.
				retAuthQueries = append(retAuthQueries, commonAuthQueryVars.parentQry)
				retAuthQueries = append(retAuthQueries, fieldAuth...)
				retAuthQueries = append(retAuthQueries, commonAuthQueryVars.selectionQry)
				authQueriesAppended = true
			}
			aggregateChild.Filter = &dql.FilterTree{
				Func: &dql.Function{
					Name: "uid",
					Args: []dql.Arg{{Value: commonAuthQueryVars.selectionQry.Var}},
				},
			}
		}
		// Restore the auth state after processing is done.
		auth.parentVarName = parentVarName
		auth.varName = parentQryName
	} else {
		retAuthQueries = append(retAuthQueries, fieldAuth...)
	}
	// otherAggregation Children are appended to aggregationChildren to return them.
	// This step is performed at the end to ensure that auth and other filters are
	// not added to them.
	aggregateChildren = append(aggregateChildren, otherAggregateChildren...)
	retAuthQueries = append(retAuthQueries, varQry...)
	return aggregateChildren, retAuthQueries
}

// TODO(GRAPHQL-874), Optimise Query rewriting in case of multiple alias with same filter.
// addSelectionSetFrom adds all the selections from field into q, and returns a list
// of extra queries needed to satisfy auth requirements
func addSelectionSetFrom(
	q *dql.GraphQuery,
	field schema.Field,
	auth *authRewriter) []*dql.GraphQuery {

	var authQueries []*dql.GraphQuery

	selSet := field.SelectionSet()
	if len(selSet) > 0 {
		// Only add dgraph.type as a child if this field is an abstract type and has some children.
		// dgraph.type would later be used in CompleteObject as different objects in the resulting
		// JSON would return different fields based on their concrete type.
		if field.AbstractType() {
			q.Children = append(q.Children, &dql.GraphQuery{
				Attr: "dgraph.type",
			})

		} else if !auth.writingAuth() &&
			len(selSet) == 1 &&
			selSet[0].Name() == schema.Typename {
			q.Children = append(q.Children, &dql.GraphQuery{
				// we don't need this for auth queries because they are added by us used for internal purposes.
				// Querying it for them would just add an overhead which we can avoid.
				Attr:  "uid",
				Alias: "dgraph.uid",
			})
		}
	}

	// These fields might not have been requested by the user directly as part of the query but
	// are required in the body template for other @custom fields requested within the query.
	// We must fetch them from Dgraph.
	requiredFields := make(map[string]schema.FieldDefinition)
	// fieldAdded is a map from field's dgraph alias to bool.
	// It tells whether a field with that dgraph alias has been added to DQL query or not.
	fieldAdded := make(map[string]bool)

	for _, f := range field.SelectionSet() {
		if f.IsCustomHTTP() {
			for dgAlias, fieldDef := range f.CustomRequiredFields() {
				requiredFields[dgAlias] = fieldDef
			}
			// This field is resolved through a custom directive so its selection set doesn't need
			// to be part of query rewriting.
			continue
		}
		// We skip typename because we can generate the information from schema or
		// dgraph.type depending upon if the type is interface or not. For interface type
		// we always query dgraph.type and can pick up the value from there.
		if f.Skip() || !f.Include() || f.Name() == schema.Typename {
			continue
		}

		// Handle aggregation queries
		if f.IsAggregateField() {
			aggregateChildren, aggregateAuthQueries := buildAggregateFields(f, auth)

			authQueries = append(authQueries, aggregateAuthQueries...)
			q.Children = append(q.Children, aggregateChildren...)
			// As all child fields inside aggregate have been looked at. We can continue
			fieldAdded[f.DgraphAlias()] = true
			continue
		}

		child := &dql.GraphQuery{
			Alias: f.DgraphAlias(),
		}

		// if field of IDType has @external directive then it means that
		// it stored as String with Hash index internally in the dgraph.
		if f.Type().Name() == schema.IDType && !f.IsExternal() {
			child.Attr = "uid"
		} else {
			child.Attr = f.DgraphPredicate()
		}

		filter, _ := f.ArgValue("filter").(map[string]interface{})
		// if this field has been filtered out by the filter, then don't add it in DQL query
		includeField, varQry := addFilter(child, f.Type(), filter, auth, f.Alias())
		if !includeField {
			continue
		}

		authQueries = append(authQueries, varQry...)

		// Add type filter in case the Dgraph predicate is a reverse edge
		if strings.HasPrefix(f.DgraphPredicate(), "~") {
			addTypeFilter(child, f.Type())
		}

		addOrder(child, f)
		addPagination(child, f)
		addCascadeDirective(child, f)
		rbac := auth.evaluateStaticRules(f.Type())

		// Since the recursion processes the query in bottom up way, we store the state of the so
		// that we can restore it later.
		var parentVarName, parentQryName string
		if len(f.SelectionSet()) > 0 && !auth.isWritingAuth && auth.hasAuthRules {
			parentVarName = auth.parentVarName
			parentQryName = auth.varName
			auth.parentVarName = auth.varGen.Next(f.Type(), "", "", auth.isWritingAuth)
			auth.varName = auth.varGen.Next(f.Type(), "", "", auth.isWritingAuth)
		}

		var selectionAuth []*dql.GraphQuery
		if !f.Type().IsGeo() {
			selectionAuth = addSelectionSetFrom(child, f, auth)
		}

		restoreAuthState := func() {
			if len(f.SelectionSet()) > 0 && !auth.isWritingAuth && auth.hasAuthRules {
				// Restore the auth state after processing is done.
				auth.parentVarName = parentVarName
				auth.varName = parentQryName
			}
		}

		fieldAdded[f.DgraphAlias()] = true

		if rbac == schema.Positive || rbac == schema.Uncertain {
			q.Children = append(q.Children, child)
		}

		var fieldAuth []*dql.GraphQuery
		var authFilter *dql.FilterTree
		if rbac == schema.Negative && auth.hasAuthRules && auth.hasCascade && !auth.isWritingAuth {
			// If RBAC rules are evaluated to Negative but we have cascade directive we continue
			// to write the query and add a dummy filter that doesn't return anything.
			// Example: AdminTask5 as var(func: uid())
			q.Children = append(q.Children, child)
			varName := auth.varGen.Next(f.Type(), "", "", auth.isWritingAuth)
			fieldAuth = append(fieldAuth, &dql.GraphQuery{
				Var:  varName,
				Attr: "var",
				Func: &dql.Function{
					Name: "uid",
				},
			})
			authFilter = &dql.FilterTree{
				Func: &dql.Function{
					Name: "uid",
					Args: []dql.Arg{{Value: varName}},
				},
			}
			rbac = schema.Positive
		} else if rbac == schema.Negative {
			// If RBAC rules are evaluated to Negative, we don't write queries for deeper levels.
			// Hence we don't need to do any further processing for this field.
			restoreAuthState()
			continue
		}

		// If RBAC rules are evaluated to `Uncertain` then we add the Auth rules.
		if rbac == schema.Uncertain {
			fieldAuth, authFilter = auth.rewriteAuthQueries(f.Type())
		}

		if len(f.SelectionSet()) > 0 && !auth.isWritingAuth && auth.hasAuthRules {
			commonAuthQueryVars := buildCommonAuthQueries(f, auth, parentVarName)
			// add child filter to parent query, auth filters to selection query and
			// selection query as a filter to child
			commonAuthQueryVars.parentQry.Children[0].Filter = child.Filter
			commonAuthQueryVars.selectionQry.Filter = authFilter
			child.Filter = &dql.FilterTree{
				Func: &dql.Function{
					Name: "uid",
					Args: []dql.Arg{{Value: commonAuthQueryVars.selectionQry.Var}},
				},
			}
			// DQL requires definitions before uses. The dependency chain is:
			//   parentQry  → defines auth.varName (e.g. TaskOccurrence_4)
			//   fieldAuth  → uses auth.varName (e.g. TaskOccurrence_Auth5)
			//   selectionQry → uses auth.varName AND authFilter vars from fieldAuth
			// Emit in this order so every var is defined before it is used.
			authQueries = append(authQueries, commonAuthQueryVars.parentQry)
			authQueries = append(authQueries, fieldAuth...)
			authQueries = append(authQueries, commonAuthQueryVars.selectionQry)
		} else {
			authQueries = append(authQueries, fieldAuth...)
		}
		authQueries = append(authQueries, selectionAuth...)
		restoreAuthState()
	}

	// Sort the required fields before adding them to q.Children so that the query produced after
	// rewriting has a predictable order.
	rfset := make([]string, 0, len(requiredFields))
	for dgAlias := range requiredFields {
		rfset = append(rfset, dgAlias)
	}
	sort.Strings(rfset)

	// Add fields required by other custom fields which haven't already been added as a
	// child to be fetched from Dgraph.
	for _, dgAlias := range rfset {
		if !fieldAdded[dgAlias] {
			f := requiredFields[dgAlias]
			child := &dql.GraphQuery{
				Alias: f.DgraphAlias(),
			}

			if f.Type().Name() == schema.IDType && !f.IsExternal() {
				child.Attr = "uid"
			} else {
				child.Attr = f.DgraphPredicate()
			}
			q.Children = append(q.Children, child)
		}
	}

	return authQueries
}

func addOrder(q *dql.GraphQuery, field schema.Field) {
	orderArg := field.ArgValue("order")
	order, ok := orderArg.(map[string]interface{})
	for ok {
		ascArg := order["asc"]
		descArg := order["desc"]
		thenArg := order["then"]

		if asc, ok := ascArg.(string); ok {
			q.Order = append(q.Order,
				&pb.Order{Attr: field.Type().DgraphPredicate(asc)})
		} else if desc, ok := descArg.(string); ok {
			q.Order = append(q.Order,
				&pb.Order{Attr: field.Type().DgraphPredicate(desc), Desc: true})
		}

		order, ok = thenArg.(map[string]interface{})
	}
}

func addPagination(q *dql.GraphQuery, field schema.Field) {
	q.Args = make(map[string]string)

	first := field.ArgValue("first")
	if first != nil {
		q.Args["first"] = fmt.Sprintf("%v", first)
	}

	offset := field.ArgValue("offset")
	if offset != nil {
		q.Args["offset"] = fmt.Sprintf("%v", offset)
	}
}

func addCascadeDirective(q *dql.GraphQuery, field schema.Field) {
	q.Cascade = field.Cascade()
}

func convertIDs(idsSlice []interface{}) []uint64 {
	ids := make([]uint64, 0, len(idsSlice))
	for _, id := range idsSlice {
		uid, err := strconv.ParseUint(id.(string), 0, 64)
		if err != nil {
			// Skip sending the is part of the query to Dgraph.
			continue
		}
		ids = append(ids, uid)
	}
	return ids
}

func extractQueryFilter(f schema.Field) map[string]interface{} {
	filter, _ := f.ArgValue("filter").(map[string]interface{})
	return filter
}

func idFilter(filter map[string]interface{}, idField schema.FieldDefinition) []uint64 {
	if filter == nil || idField == nil {
		return nil
	}

	idsFilter := filter[idField.Name()]
	if idsFilter == nil {
		return nil
	}
	var idsSlice []interface{}
	// idsFilter can be an single string value (most common) or
	// an interface{} slice
	switch f := idsFilter.(type) {
	case string:
		idsSlice = append(idsSlice, f)
	case []interface{}:
		idsSlice = f
	default:
		// if an unexpected type is encountered, fail silently
		return nil
	}
	return convertIDs(idsSlice)
}

// addFilter adds a filter to the input DQL query. It returns false if the field for which the
// filter was specified should not be included in the DQL query.
// Currently, it would only be false for a union field when no memberTypes are queried.
func addFilter(q *dql.GraphQuery,
	typ schema.Type,
	filter map[string]interface{},
	auth *authRewriter,
	queryName string) (bool, []*dql.GraphQuery) {

	varQry := []*dql.GraphQuery{}

	if len(filter) == 0 {
		return true, varQry
	}

	// There are two cases here.
	// 1. It could be the case of a filter at root.  In this case we would have added a uid
	// function at root. Lets delete the ids key so that it isn't added in the filter.
	// Also, we need to add a dgraph.type filter.
	// 2. This could be a deep filter. In that case we don't need to do anything special.
	idField := typ.IDField()
	idName := ""
	if idField != nil {
		idName = idField.Name()
	}

	_, hasIDsFilter := filter[idName]
	filterAtRoot := hasIDsFilter && q.Func != nil && q.Func.Name == "uid"
	if filterAtRoot {
		// If id was present as a filter,
		delete(filter, idName)
	}

	if typ.IsUnion() {
		if filter, varq, includeField := buildUnionFilter(typ, filter, auth, queryName); includeField {
			q.Filter = filter
			varQry = varq
		} else {
			return false, varQry
		}
	} else {
		// For interface types: intercept memberTypes before calling buildFilter.
		// memberTypes scopes the root func: type(...) to only the requested implementors.
		// It is only honoured at the top-level filter (not inside and/or/not) and is
		// consumed here so that buildFilter never sees it as a regular predicate.
		//
		// Empty list (memberTypes: []) → deny-all: replace func: type(...) with
		// uid(0x0) so the query returns no results, consistent with union filter
		// behaviour on an empty memberTypes list.
		if typ.IsInterface() {
			if mt, ok := filter["memberTypes"]; ok {
				delete(filter, "memberTypes")
				if names, ok := mt.([]interface{}); ok && q.Func != nil && q.Func.Name == "type" {
					if len(names) == 0 {
						// empty list → match nothing
						q.Func = &dql.Function{Name: "uid", UID: []uint64{0}}
					} else {
						args := make([]dql.Arg, 0, len(names))
						for _, n := range names {
							if s, ok := n.(string); ok && s != "" {
								args = append(args, dql.Arg{Value: s})
							}
						}
						if len(args) > 0 {
							q.Func.Args = args
						}
					}
				}
			}
		}
		q.Filter, varQry = buildFilter(typ, filter, auth, queryName)
	}
	if filterAtRoot {
		addTypeFilter(q, typ)
	}
	return true, varQry
}

// buildFilter builds a Dgraph dql.FilterTree from a GraphQL 'filter' arg.
//
// All the 'filter' args built by the GraphQL layer look like
// filter: { title: { anyofterms: "GraphQL" }, ... }
// or
// filter: { title: { anyofterms: "GraphQL" }, isPublished: true, ... }
// or
// filter: { title: { anyofterms: "GraphQL" }, and: { not: { ... } } }
// or
// filter: { <nested-field>: { ... }, ... }
// etc
//
// typ is the GraphQL type we are filtering on, and is needed to turn for example
// title (the GraphQL field) into Post.title (to Dgraph predicate).
//
// buildFilter turns any one filter object into a conjunction
// eg:
// filter: { title: { anyofterms: "GraphQL" }, isPublished: true }
// into:
// @filter(anyofterms(Post.title, "GraphQL") AND eq(Post.isPublished, true))
//
// Filters with `or:` and `not:` get translated to Dgraph OR and NOT.
//
// TODO: There's cases that don't make much sense like
// filter: { or: { title: { anyofterms: "GraphQL" } } }
// ATM those will probably generate junk that might cause a Dgraph error.  And
// bubble back to the user as a GraphQL error when the query fails. Really,
// they should fail query validation and never get here.
func buildFilter(typ schema.Type,
	filter map[string]interface{},
	auth *authRewriter,
	queryName string) (*dql.FilterTree, []*dql.GraphQuery) {

	var varQry []*dql.GraphQuery
	var ands []*dql.FilterTree
	var or *dql.FilterTree
	// Get a stable ordering so we generate the same thing each time.
	var keys []string
	for key := range filter {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// Each key in filter is either "and", "or", "not" or the field name it
	// applies to such as "title" in: `title: { anyofterms: "GraphQL" }``
	for _, field := range keys {
		if filter[field] == nil {
			continue
		}

		// derive variable name for nested objects
		qn := queryName + "_" + field

		switch field {

		// In 'and', 'or' and 'not' cases, filter[field] must be a map[string]interface{}
		// or it would have failed GraphQL validation - e.g. 'filter: { and: 10 }'
		// would have failed validation.

		case "and":
			// title: { anyofterms: "GraphQL" }, and: { ... }
			//                       we are here ^^
			// ->
			// @filter(anyofterms(Post.title, "GraphQL") AND ... )

			// The value of the and argument can be either an object or an array, hence we handle
			// both.
			// ... and: {}
			// ... and: [{}]
			switch v := filter[field].(type) {
			case map[string]interface{}:
				ft, qs := buildFilter(typ, v, auth, qn)
				ands = append(ands, ft)
				varQry = append(varQry, qs...)
			case []interface{}:
				for i, obj := range v {
					// For a single-element list (GraphQL coerces bare objects to [obj])
					// keep the parent qn unchanged to preserve stable variable names like
					// queryNested_X_and_y instead of queryNested_X_and_0_y.
					// Multiple elements get a numeric suffix for uniqueness.
					var childQueryName string
					if len(v) == 1 {
						childQueryName = qn
					} else {
						childQueryName = fmt.Sprintf("%s_%d", qn, i)
					}
					ft, qs := buildFilter(typ, obj.(map[string]interface{}), auth, childQueryName)
					ands = append(ands, ft)
					varQry = append(varQry, qs...)
				}
			}
		case "or":
			// title: { anyofterms: "GraphQL" }, or: { ... }
			//                       we are here ^^
			// ->
			// @filter(anyofterms(Post.title, "GraphQL") OR ... )

			// The value of the or argument can be either an object or an array, hence we handle
			// both.
			// ... or: {}
			// ... or: [{}]
			switch v := filter[field].(type) {
			case map[string]interface{}:
				cond, qs := buildFilter(typ, v, auth, qn)
				or = cond
				varQry = append(varQry, qs...)
			case []interface{}:
				ors := make([]*dql.FilterTree, 0, len(v))
				for i, obj := range v {
					// For a single-element list (GraphQL coerces bare objects to [obj])
					// keep the parent qn unchanged to preserve stable variable names like
					// queryNested_X_or_y instead of queryNested_X_or_0_y.
					var childQueryName string
					if len(v) == 1 {
						childQueryName = qn
					} else {
						childQueryName = fmt.Sprintf("%s_%d", qn, i)
					}
					ft, qs := buildFilter(typ, obj.(map[string]interface{}), auth, childQueryName)
					ors = append(ors, ft)
					varQry = append(varQry, qs...)
				}
				or = &dql.FilterTree{
					Child: ors,
					Op:    "or",
				}
			}
		case "not":
			// title: { anyofterms: "GraphQL" }, not: { isPublished: true}
			//                       we are here ^^
			// ->
			// @filter(anyofterms(Post.title, "GraphQL") AND NOT eq(Post.isPublished, true))
			not, qs := buildFilter(typ, filter[field].(map[string]interface{}), auth, qn)
			ands = append(ands,
				&dql.FilterTree{
					Op:    "not",
					Child: []*dql.FilterTree{not},
				})
			varQry = append(varQry, qs...)
		default:
			fd := typ.Field(field)
			// memberTypes is consumed at the top-level addFilter for interface types.
			// If it somehow reaches buildFilter (e.g. nested inside and/or/not which is
			// not supported), skip it to avoid a nil-pointer panic on fd.IsExternal().
			if field == "memberTypes" {
				continue
			}
			if fd != nil && fd.HasEmbeddingDirective() {
				embeddingFilter, ok := filter[field].(map[string]interface{})
				if !ok {
					continue
				}
				fn, val := first(embeddingFilter)

				var similarToFunc *dql.Function
				var qs []*dql.GraphQuery
				var err error

				// qn is queryName + "_" + field, e.g. "queryPost_embedding"
				similarVar := qn + "_similar"

				switch fn {
				case "similarToVector":
					similarToFunc, qs = buildSimilarToVectorFilter(typ, fd, val.(map[string]interface{}))
				case "similarToText":
					similarToFunc, qs, err = buildSimilarToTextFilter(typ, fd, val.(map[string]interface{}))
					if err != nil {
						glog.Errorf("Error in buildSimilarToTextFilter: %v", err)
						continue
					}
				case "similarToId":
					similarToFunc, qs = buildSimilarToIdFilter(typ, fd, val.(map[string]interface{}), auth, qn)
				}

				if similarToFunc != nil {
					similarQuery := &dql.GraphQuery{
						Attr: "var",
						Func: similarToFunc,
						Children: []*dql.GraphQuery{{
							Attr: "uid",
							Var:  similarVar,
						}},
					}
					qs = append(qs, similarQuery)

					ands = append(ands, &dql.FilterTree{
						Func: &dql.Function{
							Name: "uid",
							Args: []dql.Arg{{Value: similarVar}},
						},
					})
				}

				if len(qs) > 0 {
					varQry = append(varQry, qs...)
				}
				continue
			}
			// Handle nested object filtering
			//
			// filter: { <nested-field>: { ... }, ... }
			//     we are here ^^
			// ->
			// var() @filter(<nested-field-filter>){
			// 		nested_field_name as <inverse field>
			// }
			// root() @filter(var(nested_field_name))
			if fd != nil && fd.HasSearchDirective() {

				if inv := fd.Inverse(); inv != nil {

					nestedFilter := filter[field].(map[string]interface{})

					// For interface-typed nested fields: extract memberTypes before calling
					// buildFilter so it scopes the nested var query's func: type(...) rather
					// than being silently dropped.  Mirrors the same logic in addFilter for
					// the root query case.
					nestedFuncArgs := []dql.Arg{{Value: fd.Type().DgraphName()}} // default: full interface
					nestedDenyAll := false
					if fd.Type().IsInterface() {
						if mt, ok := nestedFilter["memberTypes"]; ok {
							delete(nestedFilter, "memberTypes")
							if names, ok := mt.([]interface{}); ok {
								if len(names) == 0 {
									nestedDenyAll = true
								} else {
									args := make([]dql.Arg, 0, len(names))
									for _, n := range names {
										if s, ok := n.(string); ok && s != "" {
											args = append(args, dql.Arg{Value: s})
										}
									}
									if len(args) > 0 {
										nestedFuncArgs = args
									}
								}
							}
						}
					}

					fil, qs := buildFilter(fd.Type(), nestedFilter, auth, qn)
					varQry = append(varQry, qs...)

					// add the uids of the nested object
					ands = append(ands, &dql.FilterTree{
						Op: "and",
						Child: []*dql.FilterTree{{
							Func: &dql.Function{
								Name: "uid",
								Args: []dql.Arg{{Value: qn}},
							},
						}},
					})

					// generate filter var query for nested object
					var nestedFunc *dql.Function
					if nestedDenyAll {
						nestedFunc = &dql.Function{Name: "uid", UID: []uint64{0}}
					} else {
						nestedFunc = &dql.Function{Name: "type", Args: nestedFuncArgs}
					}
					nestedQry := &dql.GraphQuery{
						Attr:   "var",
						Func:   nestedFunc,
						Filter: fil,
						Children: []*dql.GraphQuery{{
							Attr: inv.DgraphPredicate(),
							Var:  qn,
						}},
					}

					// add auth queries to nested field
					nestedQrys := []*dql.GraphQuery{nestedQry}

					if !auth.isWritingAuth {
						wr := &authRewriter{
							authVariables:   auth.authVariables,
							varGen:          auth.varGen,
							selector:        auth.selector,
							parentVarName:   qn + "Root",
							isWritingAuth:   auth.isWritingAuth,
							cascadeVarCache: auth.cascadeVarCache,
							forceForward:    auth.forceForward,
						}

						rbac := wr.evaluateStaticRules(fd.Type())
						if rbac == schema.Uncertain {
							// addAuthQueries returns:
							//   [0] nestedQry  – consumer: var(func:uid(qnRoot)){qn as inv}
							//   [1] rootQry    – defines qnRoot, USES varName
							//   [2] varQry     – defines varName (e.g. Group_N, Workspace_N)
							//   [3+] authVars  – auth var blocks (use varName / qnRoot)
							//
							// DQL var blocks must be emitted in definition-before-use order.
							// The required sequence is:
							//   varQry → rootQry → authVars → nestedQry (consumer last)
							authQrys, nestedAuthVarSubst := wr.addAuthQueries(fd.Type(), nestedQrys, rbac)
							// The deduplication that happens inside addAuthQueries may remove
							// auth var blocks and produce a substitution map.  Apply it to the
							// filter trees that already reference those var names so we don't
							// leave dangling uid(Workspace_AuthN) references after the
							// definition block has been dropped.
							if len(nestedAuthVarSubst) > 0 {
								applyAuthVarSubst(fil, nestedAuthVarSubst)
							}
							if len(authQrys) >= 3 {
								// Full auth scaffolding present: reorder so definitions
								// always precede uses and the consumer block comes last.
								nestedQrys = make([]*dql.GraphQuery, 0, len(authQrys))
								nestedQrys = append(nestedQrys, authQrys[2])     // varQry first
								nestedQrys = append(nestedQrys, authQrys[1])     // rootQry second
								nestedQrys = append(nestedQrys, authQrys[3:]...) // authVars
								nestedQrys = append(nestedQrys, authQrys[0])     // consumer last
							} else {
								nestedQrys = authQrys
							}
						} else if rbac == schema.Negative {
							nestedQry.Attr = "var()"
							nestedQry.Var = qn
							nestedQry.Func = nil
							nestedQry.Filter = nil
							nestedQry.Children = nil
						}
					}

					varQry = append(varQry, nestedQrys...)
					continue
				}
			}

			//// It's a base case like:
			//// title: { anyofterms: "GraphQL" } ->  anyofterms(Post.title: "GraphQL")
			//// numLikes: { between : { min : 10,  max:100 }}
			switch dgFunc := filter[field].(type) {
			case map[string]interface{}:
				// title: { anyofterms: "GraphQL" } ->  anyofterms(Post.title, "GraphQL")
				// OR
				// numLikes: { le: 10 } -> le(Post.numLikes, 10)

				fn, val := first(dgFunc)
				if val == nil {
					// If it is `eq` filter for eg: {filter: { title: {eq: null }}} then
					// it will be interpreted as {filter: {not: {has: title}}}, rest of
					// the filters with null values will be ignored in query rewriting.
					if fn == "eq" {
						hasFilterMap := map[string]interface{}{"not": map[string]interface{}{"has": []interface{}{field}}}
						ft, qs := buildFilter(typ, hasFilterMap, auth, qn)
						ands = append(ands, ft)
						varQry = append(varQry, qs...)
					}
					continue
				}
				args := []dql.Arg{{Value: typ.DgraphPredicate(field)}}
				switch fn {
				// in takes List of Scalars as argument, for eg:
				// code : { in: ["abc", "def", "ghi"] } -> eq(State.code,"abc","def","ghi")
				case "in":
					// in: ["abc", "def"] -> eq(State.code, "abc", "def")
					//
					// DQL's eq() requires at least one value argument (in addition to the
					// predicate). An empty list — in: [] — would produce eq(pred) which Dgraph
					// rejects with "eq expects atleast 1 argument".
					//
					// Semantically, in: [] means "value must be a member of ∅", which is always
					// false.  We short-circuit to uid(0x0): UID 0 never exists in Dgraph, so
					// the filter matches nothing. This correctly produces deny-all behaviour for
					// both hardcoded in: [] and @authVariables substitutions that resolve to an
					// empty permissions array.
					vals := val.([]interface{})
					if len(vals) == 0 {
						ands = append(ands, &dql.FilterTree{
							Func: &dql.Function{
								Name: "uid",
								UID:  []uint64{0},
							},
						})
						continue
					}
					fn = "eq"

					for _, v := range vals {
						args = append(args, dql.Arg{Value: maybeQuoteArg(fn, v)})
					}
				case "between":
					// numLikes: { between : { min : 10,  max:100 }} should be rewritten into
					// 	between(numLikes,10,20). Order of arguments (min,max) is neccessary or
					// it will return empty
					vals := val.(map[string]interface{})
					args = append(args, dql.Arg{Value: maybeQuoteArg(fn, vals["min"])},
						dql.Arg{Value: maybeQuoteArg(fn, vals["max"])})
				case "near":
					// For Geo type we have `near` filter which is written as follows:
					// { near: { distance: 33.33, coordinate: { latitude: 11.11, longitude: 22.22 } } }
					near := val.(map[string]interface{})
					coordinate := near["coordinate"].(map[string]interface{})
					var buf bytes.Buffer
					buildPoint(coordinate, &buf)
					args = append(args, dql.Arg{Value: buf.String()},
						dql.Arg{Value: fmt.Sprintf("%v", near["distance"])})
				case "within":
					// For Geo type we have `within` filter which is written as follows:
					// { within: { polygon: { coordinates: [ { points: [
					// { latitude: 11.11, longitude: 22.22}, { latitude: 15.15, longitude: 16.16} ,
					// { latitude: 20.20, longitude: 21.21} ]}] } } }
					within := val.(map[string]interface{})
					polygon := within["polygon"].(map[string]interface{})
					var buf bytes.Buffer
					buildPolygon(polygon, &buf)
					args = append(args, dql.Arg{Value: buf.String()})
				case "contains":
					// For Geo type we have `contains` filter which is either point or polygon and is written
					// as follows:
					// For point: { contains: { point: { latitude: 11.11, longitude: 22.22 }}}
					// For polygon: { contains: { polygon: { coordinates: [ { points: [
					// { latitude: 11.11, longitude: 22.22}, { latitude: 15.15, longitude: 16.16} ,
					// { latitude: 20.20, longitude: 21.21} ]}] } } }
					contains := val.(map[string]interface{})
					var buf bytes.Buffer
					if polygon, ok := contains["polygon"].(map[string]interface{}); ok {
						buildPolygon(polygon, &buf)
					} else if point, ok := contains["point"].(map[string]interface{}); ok {
						buildPoint(point, &buf)
					}
					args = append(args, dql.Arg{Value: buf.String()})
					// TODO: for both contains and intersects, we should use @oneOf in the inbuilt
					// schema. Once we have variable validation hook available in gqlparser, we can
					// do this. So, if either both the children are given or none of them is given,
					// we should get an error at parser level itself. Right now, if both "polygon"
					// and "point" are given, we only use polygon. If none of them are given,
					// an incorrect DQL query will be formed and will error out from Dgraph.
				case "intersects":
					// For Geo type we have `intersects` filter which is either multi-polygon or polygon and is written
					// as follows:
					// For polygon: { intersect: { polygon: { coordinates: [ { points: [
					// { latitude: 11.11, longitude: 22.22}, { latitude: 15.15, longitude: 16.16} ,
					// { latitude: 20.20, longitude: 21.21} ]}] } } }
					// For multi-polygon : { intersect: { multiPolygon: { polygons: [{ coordinates: [ { points: [
					// { latitude: 11.11, longitude: 22.22}, { latitude: 15.15, longitude: 16.16} ,
					// { latitude: 20.20, longitude: 21.21} ]}] }] } } }
					intersects := val.(map[string]interface{})
					var buf bytes.Buffer
					if polygon, ok := intersects["polygon"].(map[string]interface{}); ok {
						buildPolygon(polygon, &buf)
					} else if multiPolygon, ok := intersects["multiPolygon"].(map[string]interface{}); ok {
						buildMultiPolygon(multiPolygon, &buf)
					}
					args = append(args, dql.Arg{Value: buf.String()})
				default:
					args = append(args, dql.Arg{Value: maybeQuoteArg(fn, val)})
				}
				ands = append(ands, &dql.FilterTree{
					Func: &dql.Function{
						Name: fn,
						Args: args,
					},
				})
			case []interface{}:
				// has: [comments, text] -> has(comments) AND has(text)
				// ids: [ 0x123, 0x124]
				switch field {
				case "has":
					ands = append(ands, buildHasFilterList(typ, dgFunc)...)
				default:
					// If ids is an @external field then it gets rewritten just like `in` filter
					//  ids: [0x123, 0x124] -> eq(typeName.ids, "0x123", 0x124)
					if typ.Field(field).IsExternal() {
						fn := "eq"
						args := []dql.Arg{{Value: typ.DgraphPredicate(field)}}
						for _, v := range dgFunc {
							args = append(args, dql.Arg{Value: maybeQuoteArg(fn, v)})
						}
						ands = append(ands, &dql.FilterTree{
							Func: &dql.Function{
								Name: fn,
								Args: args,
							},
						})
					} else {
						// if it is not an @external field then it is rewritten as uid filter.
						// ids: [ 0x123, 0x124 ] -> uid(0x123, 0x124)
						ids := convertIDs(dgFunc)
						ands = append(ands, &dql.FilterTree{
							Func: &dql.Function{
								Name: "uid",
								UID:  ids,
							},
						})
					}
				}
			case interface{}:
				// isPublished: true -> eq(Post.isPublished, true)
				// OR an enum case
				// postType: Question -> eq(Post.postType, "Question")

				fn := "eq"
				ands = append(ands, &dql.FilterTree{
					Func: &dql.Function{
						Name: fn,
						Args: []dql.Arg{
							{Value: typ.DgraphPredicate(field)},
							{Value: fmt.Sprintf("%v", dgFunc)},
						},
					},
				})
			}
		}
	}

	var andFt *dql.FilterTree
	if len(ands) == 0 {
		return or, varQry
	} else if len(ands) == 1 {
		andFt = ands[0]
	} else if len(ands) > 1 {
		andFt = &dql.FilterTree{
			Op:    "and",
			Child: ands,
		}
	}

	if or == nil {
		return andFt, varQry
	}

	return &dql.FilterTree{
		Op:    "or",
		Child: []*dql.FilterTree{andFt, or},
	}, varQry
}

func buildHasFilterList(typ schema.Type, fieldsSlice []interface{}) []*dql.FilterTree {
	var ands []*dql.FilterTree
	fn := "has"
	for _, fieldName := range fieldsSlice {
		ands = append(ands, &dql.FilterTree{
			Func: &dql.Function{
				Name: fn,
				Args: []dql.Arg{
					{Value: typ.DgraphPredicate(fieldName.(string))},
				},
			},
		})
	}
	return ands
}

func buildPoint(point map[string]interface{}, buf *bytes.Buffer) {
	x.Check2(buf.WriteString(fmt.Sprintf("[%v,%v]", point[schema.Longitude],
		point[schema.Latitude])))
}

func buildPolygon(polygon map[string]interface{}, buf *bytes.Buffer) {
	coordinates, _ := polygon[schema.Coordinates].([]interface{})
	comma1 := ""

	x.Check2(buf.WriteString("["))
	for _, r := range coordinates {
		ring, _ := r.(map[string]interface{})
		points, _ := ring[schema.Points].([]interface{})
		comma2 := ""

		x.Check2(buf.WriteString(comma1))
		x.Check2(buf.WriteString("["))
		for _, p := range points {
			x.Check2(buf.WriteString(comma2))
			point, _ := p.(map[string]interface{})
			buildPoint(point, buf)
			comma2 = ","
		}
		x.Check2(buf.WriteString("]"))
		comma1 = ","
	}
	x.Check2(buf.WriteString("]"))
}

func buildMultiPolygon(multipolygon map[string]interface{}, buf *bytes.Buffer) {
	polygons, _ := multipolygon[schema.Polygons].([]interface{})
	comma := ""

	x.Check2(buf.WriteString("["))
	for _, p := range polygons {
		polygon, _ := p.(map[string]interface{})
		x.Check2(buf.WriteString(comma))
		buildPolygon(polygon, buf)
		comma = ","
	}
	x.Check2(buf.WriteString("]"))
}

func buildUnionFilter(typ schema.Type,
	filter map[string]interface{},
	auth *authRewriter,
	queryName string) (*dql.FilterTree, []*dql.GraphQuery, bool) {

	var varQry []*dql.GraphQuery
	memberTypesList, ok := filter["memberTypes"].([]interface{})
	// if memberTypes was specified to be an empty list like: { memberTypes: [], ...},
	// then we don't need to include the field, on which the filter was specified, in the query.
	if ok && len(memberTypesList) == 0 {
		return nil, varQry, false
	}

	ft := &dql.FilterTree{
		Op: "or",
	}

	// now iterate over the filtered member types for this union and build FilterTree for them
	for _, memberType := range typ.UnionMembers(memberTypesList) {
		memberTypeFilter, _ := filter[schema.CamelCase(memberType.Name())+"Filter"].(map[string]interface{})
		var memberTypeFt *dql.FilterTree
		if len(memberTypeFilter) == 0 {
			// if the filter for a member type wasn't specified, was null, or was specified as {};
			// then we need to query all nodes of that member type for the field on which the filter
			// was specified.
			memberTypeFt = &dql.FilterTree{Func: buildTypeFunc(memberType.DgraphName())}
		} else {
			// else we need to query only the nodes which match the filter for that member type
			ft, qs := buildFilter(memberType, memberTypeFilter, auth, queryName)
			varQry = qs
			memberTypeFt = &dql.FilterTree{
				Op: "and",
				Child: []*dql.FilterTree{
					{Func: buildTypeFunc(memberType.DgraphName())},
					ft,
				},
			}
		}
		ft.Child = append(ft.Child, memberTypeFt)
	}

	// return true because we want to include the field with filter in query
	return ft, varQry, true
}

func maybeQuoteArg(fn string, arg interface{}) string {
	switch arg := arg.(type) {
	case string: // dateTime also parsed as string
		if fn == "regexp" {
			return arg
		}
		return fmt.Sprintf("%q", arg)
	case float64, float32:
		return fmt.Sprintf("\"%v\"", arg)
	default:
		return fmt.Sprintf("%v", arg)
	}
}

// first returns the first element it finds in a map - we bump into lots of one-element
// maps like { "anyofterms": "GraphQL" }.  fst helps extract that single mapping.
func first(aMap map[string]interface{}) (string, interface{}) {
	for key, val := range aMap {
		return key, val
	}
	return "", nil
}

func buildSimilarToVectorFilter(
	typ schema.Type,
	field schema.FieldDefinition,
	filter map[string]interface{},
) (*dql.Function, []*dql.GraphQuery) {
	topK, ok := filter["topK"]
	if !ok {
		return nil, nil
	}
	vector := filter["vector"]

	maxDistance, ok := filter["distance"]
	if !ok {
		maxDistance = 0
	}

	vec, err := json.Marshal(vector)
	if err != nil {
		// should not happen with proper validation
		return nil, nil
	}

	return &dql.Function{
		Name: "similar_to",
		Args: []dql.Arg{
			{Value: field.DgraphPredicate()},
			{Value: fmt.Sprintf("%v", topK)},
			{Value: fmt.Sprintf("%q", string(vec))},
			{Value: fmt.Sprintf("%v", maxDistance)},
		},
	}, nil
}

func buildSimilarToTextFilter(
	typ schema.Type,
	field schema.FieldDefinition,
	filter map[string]interface{},
) (*dql.Function, []*dql.GraphQuery, error) {
	topK, ok := filter["topK"]
	if !ok {
		return nil, nil, errors.Errorf("topK not found in similarToText filter")
	}
	text, ok := filter["text"].(string)
	if !ok {
		return nil, nil, errors.Errorf("text not found in similarToText filter")
	}

	embedding, err := field.GenerateEmbedding(text)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "failed to generate embedding for text filter")
	}

	vec, err := json.Marshal(embedding)
	if err != nil {
		// This is an internal error, should not happen.
		return nil, nil, errors.Wrapf(err, "failed to marshal generated embedding")
	}

	maxDistance, ok := filter["distance"]
	if !ok {
		maxDistance = 0
	}

	return &dql.Function{
		Name: "similar_to",
		Args: []dql.Arg{
			{Value: field.DgraphPredicate()},
			{Value: fmt.Sprintf("%v", topK)},
			{Value: fmt.Sprintf("%q", string(vec))},
			{Value: fmt.Sprintf("%v", maxDistance)},
		},
	}, nil, nil
}

func buildSimilarToIdFilter(
	typ schema.Type,
	field schema.FieldDefinition,
	filter map[string]interface{},
	auth *authRewriter,
	queryName string,
) (*dql.Function, []*dql.GraphQuery) {
	topK, ok := filter["topK"]
	if !ok {
		// validation should catch this.
		return nil, nil
	}
	maxDistance, ok := filter["distance"]
	if !ok {
		maxDistance = 0
	}

	vecVar := queryName + "_vec"

	var idFilters []*dql.FilterTree
	var idFunc *dql.Function

	for key, val := range filter {
		if key == "topK" {
			continue
		}

		idField := typ.Field(key)
		if idField == nil {
			continue
		}

		if idField.IsID() && !idField.IsExternal() {
			uid, err := strconv.ParseUint(val.(string), 0, 64)
			if err == nil {
				idFunc = &dql.Function{Name: "uid", UID: []uint64{uid}}
			}
		} else {
			idFilters = append(idFilters, &dql.FilterTree{
				Func: &dql.Function{
					Name: "eq",
					Args: []dql.Arg{
						{Value: typ.DgraphPredicate(key)},
						{Value: maybeQuoteArg("eq", val)},
					},
				},
			})
		}
	}

	var finalFilter *dql.FilterTree
	if len(idFilters) > 1 {
		finalFilter = &dql.FilterTree{Op: "and", Child: idFilters}
	} else if len(idFilters) == 1 {
		finalFilter = idFilters[0]
	}

	varQry := &dql.GraphQuery{
		Attr:   "var",
		Func:   idFunc,
		Filter: finalFilter,
		Children: []*dql.GraphQuery{{
			Attr: field.DgraphPredicate(),
			Var:  vecVar,
		}},
	}

	if idFunc == nil {
		varQry.Func = buildTypeFunc(typ.DgraphName())
	}

	similarToFunc := &dql.Function{
		Name: "similar_to",
		Args: []dql.Arg{
			{Value: field.DgraphPredicate()},
			{Value: fmt.Sprintf("%v", topK)},
			{Value: fmt.Sprintf("val(%s)", vecVar)},
			{Value: fmt.Sprintf("%v", maxDistance)},
		},
	}

	return similarToFunc, []*dql.GraphQuery{varQry}
}
