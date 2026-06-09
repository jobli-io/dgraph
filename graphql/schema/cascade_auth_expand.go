/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/dgraph-io/gqlparser/v2/ast"
	"github.com/dgraph-io/gqlparser/v2/gqlerror"
)

// ---------------------------------------------------------------------------
// @cascadeAuth compile-down pass
// ---------------------------------------------------------------------------

// cascadeAuthIncomingEdge records a parent field that carries @cascadeAuth
// pointing to a given child type.
type cascadeAuthIncomingEdge struct {
	parentTypeName    string
	fieldName         string // e.g. "inWorkspace"
	dgraphPred        string // e.g. "WorkspaceMember.inWorkspace" (child → parent)
	inverseDgraphPred string // e.g. "Workspace.hasUser" (parent → child, via @hasInverse)
	cfg               *CascadeAuthFieldConfig
}

// expandCascadeAuth is the schema-time compile-down pass. It runs after authRules()
// has processed all @auth directives (but before interfaces are reset) and propagates
// parent @auth rules into child TypeAuth entries.
//
// Strategy: for each child type that has an incoming @cascadeAuth edge, we take the
// parent's existing TypeAuth.Rules.Query RuleNode and merge it into the child's
// TypeAuth for the configured operations. The child's query auth now requires
// satisfying the parent's auth rule — which already traverses the ownership edge.
//
// Variable substitution: when variableContext = "self", we clone the parent's Rule
// tree with the child type's @authVariables applied. For the initial implementation
// we store the parent's RuleNode directly and record the child's authVariables in a
// wrapper CascadeAuthRuleNode for later resolution by the auth rewriter.
//
// authRulesOwnOnly: snapshot of authRules taken BEFORE interface auth was AND-merged
// into concrete types. Used exclusively for childOwnQueryRule lookups in bidirectional
// contributions, ensuring only a type's own declared @auth (with $sub/$azp checks)
// is propagated bidirectionally — never the interface-level $ws-only auth.
func expandCascadeAuth(sch *schema, authRules map[string]*TypeAuth, authRulesOwnOnly map[string]*TypeAuth) error {
	s := sch.schema

	// -------------------------------------------------------------------------
	// Phase 1: build reverse index.
	// -------------------------------------------------------------------------
	incomingEdges := make(map[string][]cascadeAuthIncomingEdge)

	for _, typ := range s.Types {
		// Only process concrete Object types in Phase 1.
		// Interface types are handled by iterating their concrete implementors below.
		// Previously we processed both Object and Interface, which resulted in each
		// concrete type receiving the edge twice (once from Object iteration, once
		// from Interface fan-out) — producing duplicate uid_in filters.
		if typ.Kind != ast.Object {
			continue
		}
		for _, field := range typ.Fields {
			dir := field.Directives.ForName(cascadeAuthDirective)
			// Check interface-inherited directive when concrete type doesn't carry it directly.
			var fromInterface string // interface name where the directive was found
			if dir == nil {
				for _, ifaceName := range typ.Interfaces {
					ifaceDef := s.Types[ifaceName]
					if ifaceDef == nil {
						continue
					}
					if ifaceField := ifaceDef.Fields.ForName(field.Name); ifaceField != nil {
						if d := ifaceField.Directives.ForName(cascadeAuthDirective); d != nil {
							dir = d
							fromInterface = ifaceName
							break
						}
					}
				}
			}
			if dir == nil {
				continue
			}
			// When the directive is inherited from an interface, use the interface's
			// field definition for CascadeAuthConfig (it carries the directive args).
			// The concrete type's field carries the same args post-schema-gen, so
			// either works — but if the concrete field somehow lost the directive
			// during schema rewrite, fall back to the interface field.
			_ = fromInterface

			parentAstType := &astType{
				typ:             &ast.Type{NamedType: typ.Name},
				inSchema:        sch,
				dgraphPredicate: sch.dgraphPredicate,
			}
			fd := &fieldDefinition{
				fieldDef:        field,
				inSchema:        sch,
				dgraphPredicate: sch.dgraphPredicate,
				parentType:      parentAstType,
			}
			cfg := fd.CascadeAuthConfig()
			if cfg == nil {
				continue
			}

			// authorityTypeName: the field's return type (e.g. Workspace) — provides @auth rules.
			// dependentTypeName: the type hosting this field (e.g. Group/Company) — receives them.
			authorityTypeName := field.Type.Name()
			dependentTypeName := typ.Name
			// Use fd.DgraphPredicate() for the actual Dgraph predicate name. For fields
			// inherited from interfaces (e.g. inWorkspace defined on WorkspaceMember),
			// the Dgraph predicate is WorkspaceMember.inWorkspace — not Photo.inWorkspace.
			// Using the concrete type name produces a non-existent predicate, causing
			// Dgraph fillVars failures at query time.
			// Resolve the inverse predicate now (schema time) so the rewriter
			// can traverse parent→child when building bidirectional DQL vars.
			// fd.Inverse() is unreliable when @hasInverse is declared on the
			// authority side (e.g. Workspace.hasUserInvitation @hasInverse(field:
			// inWorkspace)) rather than on the child field itself. Use our
			// dedicated helper that scans the authority type directly.
			// Pass the concrete child type name so the helper can differentiate
			// between Workspace.hasCandidate vs Workspace.hasUser when both carry
			// @hasInverse(field: inWorkspace).
			invPred := findInversePredicate(sch, authorityTypeName, field.Name, dependentTypeName)
			edge := cascadeAuthIncomingEdge{
				parentTypeName:    authorityTypeName,
				fieldName:         field.Name,
				dgraphPred:        fd.DgraphPredicate(),
				inverseDgraphPred: invPred,
				cfg:               cfg,
			}

			incomingEdges[dependentTypeName] = append(incomingEdges[dependentTypeName], edge)
		}
	}

	// -------------------------------------------------------------------------
	// Phase 2: for each concrete child type, generate cascade auth rules and
	// merge them into TypeAuth.
	// -------------------------------------------------------------------------

	// Snapshot authRules before Phase 2 so that bidirectional writes on
	// the live authRules map (which update authority.Rules.Query mid-loop)
	// do not leak into cascade reads for other types. Without the snapshot:
	//   1. Type A (bidirectional) OR-merges its own auth into Workspace.Rules.Query
	//   2. Type B (next in iteration) calls cascadeAuthRuleForEdge and reads the
	//      now-polluted Workspace.Rules.Query, picking up all of A's rules.
	//   3. Each subsequent type accumulates every previous type's rules → 300+ vars.
	authRulesForCascade := snapshotAuthRules(authRules)

	// biDirSeen deduplicates bidirectional rules per authority type across ALL passes.
	// Key: "inversePred::ruleLeafKey" — once a leaf rule has been OR-merged into
	// an authority, subsequent identical contributions from the same or other types
	// are discarded. This keeps the rule tree O(distinct-rules) not O(types × rules).
	biDirSeen := make(map[string]bool)

	for _, typ := range s.Types {
		if typ.Kind != ast.Object {
			continue
		}
		childTypeName := typ.Name
		edges := incomingEdges[childTypeName]
		if len(edges) == 0 {
			continue
		}

		childAstType := &astType{
			typ:             &ast.Type{NamedType: childTypeName},
			inSchema:        sch,
			dgraphPredicate: sch.dgraphPredicate,
		}
		policy := childAstType.CascadeAuthPolicyConfig()

		// @cascadeAuthPolicy(skip: true) — type explicitly opts out of all cascade
		// auth expansion (e.g. audit/log types that implement WorkspaceMember only
		// for the inWorkspace field, not for auth enforcement from the authority).
		if policy.Skip {
			continue
		}

		// Build per-parent-edge RuleNodes, reading from the snapshot so that
		// cascade rules are derived only from each authority's declared @auth.
		var perEdgeRules []*RuleNode // used only for preCascadeQueryAuth / bidir setup
		for _, edge := range edges {
			// At depth=0 the immediate child IS the outermost queried child.
			ruleNode, err := buildCascadeRule(edge, childTypeName, childTypeName, "query", incomingEdges,
				authRulesForCascade, make(map[string]bool), 0, sch)
			if err != nil {
				return err
			}
			if ruleNode != nil {
				perEdgeRules = append(perEdgeRules, ruleNode)
			}
		}

		if len(perEdgeRules) == 0 {
			continue
		}

		// Combined query-cascade rule (for bidir / preCascadeQueryAuth lookup).
		var cascadeRule *RuleNode // query-op combined rule; used for bidir
		switch {
		case len(perEdgeRules) == 1:
			cascadeRule = perEdgeRules[0]
		case len(perEdgeRules) > 1 && policy.Aggregation == "or":
			cascadeRule = &RuleNode{Or: perEdgeRules}
		case len(perEdgeRules) > 1:
			cascadeRule = &RuleNode{And: perEdgeRules}
		}
		_ = cascadeRule // op-specific loop below rebuilds per-op rules

		// Ensure TypeAuth exists.
		if authRules[childTypeName] == nil {
			authRules[childTypeName] = &TypeAuth{Fields: make(map[string]*AuthContainer)}
		}
		if authRules[childTypeName].Fields == nil {
			authRules[childTypeName].Fields = make(map[string]*AuthContainer)
		}

		// Snapshot child's interface-merged auth (authRulesForCascade) so bidir
		// contributions use the full ANDed set: own declared auth AND interface auth
		// (e.g. WorkspaceMember.query's $ws scope gate + user membership check).
		// Using authRulesOwnOnly here would let purely structural arms (NOT has(managedBy))
		// propagate bidirectionally with no identity gate.
		var preCascadeQueryAuth *RuleNode
		for _, edge := range edges {
			if edge.cfg.Bidirectional {
				preCascadeQueryAuth = childOwnQueryRule(childTypeName, authRulesForCascade)
				break
			}
		}

		// Merge into each covered operation slot, using op-specific authority rules
		// where available (add→@auth(add:...) with @auth(query:...) fallback, etc.).
		ops := operationsFromEdges(edges)
		for _, op := range ops {
			// Build per-op per-edge rules (op-specific authority rule selection).
			var perOpEdgeRules []*RuleNode
			for _, edge := range edges {
				// At depth=0 the immediate child IS the outermost queried child.
				ruleNode, err := buildCascadeRule(edge, childTypeName, childTypeName, op, incomingEdges,
					authRulesForCascade, make(map[string]bool), 0, sch)
				if err != nil {
					return err
				}
				if ruleNode != nil {
					perOpEdgeRules = append(perOpEdgeRules, ruleNode)
				}
			}
			if len(perOpEdgeRules) == 0 {
				continue
			}
			// Compose: AND-aggregate cascade edges first, then merge the cascade
			// block into the child's existing (TypeAuth + InterfaceAuth) base rule.
			// Aggregation is resolved from @cascadeAuthPolicy(aggregation:) on the
			// child type; defaults to "and" when no type-level policy is set.
			composeCascadeWithBase(authRules[childTypeName], op, perOpEdgeRules, policy.Aggregation)
		}

		// Bidirectional: reverse-visibility rules on parent (Pass 0).
		// Skipped when the child's @cascadeAuthPolicy sets skipBidirectional: true —
		// this prevents types with no user-specific own auth from polluting the
		// authority's filter with a user-agnostic condition.
		if !policy.SkipBidirectional {
			for _, edge := range edges {
				if !edge.cfg.Bidirectional {
					continue
				}
				if preCascadeQueryAuth == nil {
					continue
				}
				if edge.inverseDgraphPred == "" {
					continue
				}

				biDirRule := withCascadeEdgePred(preCascadeQueryAuth, edge.inverseDgraphPred)
				if !hasNewLeaves(biDirRule, edge.inverseDgraphPred, biDirSeen) {
					continue
				}

				if authRules[edge.parentTypeName] == nil {
					authRules[edge.parentTypeName] = &TypeAuth{Fields: make(map[string]*AuthContainer)}
				}
				if authRules[edge.parentTypeName].Fields == nil {
					authRules[edge.parentTypeName].Fields = make(map[string]*AuthContainer)
				}
				if authRules[edge.parentTypeName].Rules == nil {
					authRules[edge.parentTypeName].Rules = &AuthContainer{}
				}

				authRules[edge.parentTypeName].Rules.Query =
					mergeAuthNodeWithOr(
						authRules[edge.parentTypeName].Rules.Query,
						biDirRule,
					)

				// Edge scoping: when traversing inversePred, only return authorized children.
				// OR-accumulate rather than overwrite: multiple concrete child types that
				// share the same edge field name (e.g. inWorkspace on WorkspaceMember) must
				// each contribute their own scoping rule — the last type processed must not
				// clobber rules written by earlier types.
				{
					existing := authRules[edge.parentTypeName].Fields[edge.fieldName]
					var existingQ *RuleNode
					if existing != nil {
						existingQ = existing.Query
					}
					authRules[edge.parentTypeName].Fields[edge.fieldName] = &AuthContainer{
						Query: mergeAuthNodeWithOr(existingQ, biDirRule),
					}
				}
			}
		}
	}

	// -------------------------------------------------------------------------
	// Phase 3: multi-pass bidirectional propagation for transitive chains.
	//
	// Problem: with non-deterministic schema ordering, if Group is processed
	// before Candidate in Phase 2 (both Groupables), Group's bidir contribution
	// to Workspace only sees Group's declared auth — it misses any bidir auth
	// that Candidate added to Group. A second pass fixes this.
	//
	// Critical constraint: Phase 3 MUST NOT read from the post-Phase-2
	// authRules snapshot (bidirSnapshot) as preCascadeQueryAuth. Phase 2's
	// mergeIntoOp adds cascade-forward rules (e.g. Workspace's own auth) into
	// child types. If Phase 3 reads those back as preCascadeQueryAuth and
	// contributes them bidirectionally to Workspace, Workspace's own auth
	// (including the workspace-name-only arm like queryWorkspace(name==$ws))
	// gets re-added as an OR alternative to Workspace.query — creating a
	// circular bypass that lets any user with a valid $ws token see the workspace.
	//
	// Solution: maintain a separate bidirOnlyRules accumulator that starts from
	// authRulesForCascade (pure declared auth, no cascade-forward pollution) and
	// only accumulates bidir OR-merges. Phase 3 uses bidirOnlyRules as the
	// source for preCascadeQueryAuth. bidir OR-merges are applied to BOTH
	// authRules (for Phase 3 cascade forward correctness) AND bidirOnlyRules
	// (so the next pass can see them as inputs). Cascade-forward-merged content
	// never enters bidirOnlyRules.
	// -------------------------------------------------------------------------
	const maxBidirPasses = 10

	// bidirOnlyRules starts from the interface-merged pre-cascade snapshot (authRulesForCascade)
	// so bidir contributions carry the full interface-AND gates (WorkspaceMember.$ws,
	// Manageable.managedBy check, etc.). Cascade-forward merges never enter this
	// accumulator, preventing circular leaks where Workspace's own auth could be
	// re-contributed back to itself via the cascade chain.
	bidirOnlyRules := snapshotAuthRules(authRulesForCascade)

	for pass := 0; pass < maxBidirPasses; pass++ {
		// Snapshot the bidir-only accumulator for this pass's read phase.
		// Types that received bidir additions in the previous pass expose those
		// additions here, enabling transitive propagation (Candidate→Group→Workspace).
		bidirOnlySnapshot := snapshotAuthRules(bidirOnlyRules)
		changed := false

		for _, typ := range s.Types {
			if typ.Kind != ast.Object {
				continue
			}
			childTypeName := typ.Name
			edges := incomingEdges[childTypeName]
			if len(edges) == 0 {
				continue
			}

			childAstType := &astType{
				typ:             &ast.Type{NamedType: childTypeName},
				inSchema:        sch,
				dgraphPredicate: sch.dgraphPredicate,
			}
			policy := childAstType.CascadeAuthPolicyConfig()
			if policy.SkipBidirectional {
				continue
			}

			// Read preCascadeQueryAuth from the bidir-only snapshot.
			// This contains declared auth + previous-pass bidir contributions only —
			// never cascade-forward rules, preventing circular leaks.
			var preCascadeQueryAuth *RuleNode
			hasBidir := false
			for _, edge := range edges {
				if edge.cfg.Bidirectional {
					hasBidir = true
					preCascadeQueryAuth = childOwnQueryRule(childTypeName, bidirOnlySnapshot)
					break
				}
			}
			if !hasBidir || preCascadeQueryAuth == nil {
				continue
			}

			for _, edge := range edges {
				if !edge.cfg.Bidirectional || edge.inverseDgraphPred == "" {
					continue
				}

				biDirRule := withCascadeEdgePred(preCascadeQueryAuth, edge.inverseDgraphPred)
				if !hasNewLeaves(biDirRule, edge.inverseDgraphPred, biDirSeen) {
					continue
				}

				// New leaves found — merge into parent in both maps.
				changed = true
				if authRules[edge.parentTypeName] == nil {
					authRules[edge.parentTypeName] = &TypeAuth{Fields: make(map[string]*AuthContainer)}
				}
				if authRules[edge.parentTypeName].Fields == nil {
					authRules[edge.parentTypeName].Fields = make(map[string]*AuthContainer)
				}
				if authRules[edge.parentTypeName].Rules == nil {
					authRules[edge.parentTypeName].Rules = &AuthContainer{}
				}

				authRules[edge.parentTypeName].Rules.Query =
					mergeAuthNodeWithOr(
						authRules[edge.parentTypeName].Rules.Query,
						biDirRule,
					)
				// OR-accumulate (same rationale as Phase 2 above).
				{
					existing := authRules[edge.parentTypeName].Fields[edge.fieldName]
					var existingQ *RuleNode
					if existing != nil {
						existingQ = existing.Query
					}
					authRules[edge.parentTypeName].Fields[edge.fieldName] = &AuthContainer{
						Query: mergeAuthNodeWithOr(existingQ, biDirRule),
					}
				}

				// Also accumulate into bidirOnlyRules so subsequent passes can
				// see this pass's bidir additions as inputs (transitive chain).
				if bidirOnlyRules[edge.parentTypeName] == nil {
					bidirOnlyRules[edge.parentTypeName] = &TypeAuth{Fields: make(map[string]*AuthContainer)}
				}
				if bidirOnlyRules[edge.parentTypeName].Rules == nil {
					bidirOnlyRules[edge.parentTypeName].Rules = &AuthContainer{}
				}
				bidirOnlyRules[edge.parentTypeName].Rules.Query =
					mergeAuthNodeWithOr(
						bidirOnlyRules[edge.parentTypeName].Rules.Query,
						biDirRule,
					)
			}
		}

		if !changed {
			break // Fixed point reached — no new rules were added.
		}
	}

	return nil
}

// buildCascadeRule is the new canonical cascade-auth rule builder.
//
// It replaces the cascadeAuthRuleForEdge + CascadeBundlePred mechanism with a
// cleaner two-step approach:
//
//  1. Compute the authority type's FULL auth rule — its own @auth rule (if any)
//     combined with the auth rules from ITS OWN cascade parents, merged using
//     the authority's own @cascadeAuthPolicy(aggregation).
//
//  2. Wrap the result in a CascadeWrap node so the DQL rewriter can emit:
//     var(func: type(AuthorityType)) @filter(authorityFullAuth) @cascade
//     and return uid_in(edge.dgraphPred, uid(authorityVar)) as the filter for
//     the child type.
//
// Key invariant: each type's full auth is computed from ITS OWN policy, not the
// child's. This means Group's aggregation:"or" correctly produces
// OR(G_iam_rule, uid_in(inWorkspace, Workspace_auth)) on the Group var, which
// is then referenced via uid_in(inGroup, uid(Group_var)) by JobAd.
//
// outerChildTypeName: the outermost queried type (provides @authVariables context).
// immediateChildTypeName: the type declaring @cascadeAuth at this depth.
// op: "query" | "add" | "update" | "delete".
func buildCascadeRule(
	edge cascadeAuthIncomingEdge,
	outerChildTypeName, immediateChildTypeName string,
	op string,
	incomingEdges map[string][]cascadeAuthIncomingEdge,
	authRulesSnapshot map[string]*TypeAuth,
	visited map[string]bool,
	depth int,
	sch *schema,
) (*RuleNode, error) {
	if visited[edge.parentTypeName] {
		return nil, nil
	}
	if edge.cfg.Depth != -1 && depth >= edge.cfg.Depth {
		return nil, nil
	}

	authorityDef := sch.schema.Types[edge.parentTypeName]
	if authorityDef == nil {
		return nil, nil
	}

	ta := authRulesSnapshot[edge.parentTypeName]

	// ── Case 1: authority is an interface with no direct @auth — collect implementors.
	if (ta == nil || ta.Rules == nil) && authorityDef.Kind == ast.Interface {
		implRule := interfaceImplementorAuthRules(sch, edge.parentTypeName, authRulesSnapshot)
		if implRule == nil {
			return nil, nil
		}
		implRule, err := resolveAuthVarsInRuleNode(implRule, edge, outerChildTypeName, immediateChildTypeName, incomingEdges, visited, sch)
		if err != nil {
			return nil, err
		}
		if implRule == nil {
			return nil, nil
		}
		return &RuleNode{
			CascadeWrapPred:  edge.dgraphPred,
			CascadeWrapType:  edge.parentTypeName,
			CascadeWrapInner: implRule,
		}, nil
	}

	// ── Resolve authority's own @auth rule for this op (with @authVariables substitution).
	var authorityOwnRule *RuleNode
	if ta != nil && ta.Rules != nil {
		switch op {
		case "add":
			authorityOwnRule = ta.Rules.Add
		case "update":
			authorityOwnRule = ta.Rules.Update
		case "delete":
			authorityOwnRule = ta.Rules.Delete
		}
		if authorityOwnRule == nil {
			authorityOwnRule = ta.Rules.Query // fallback
		}
	}
	if authorityOwnRule != nil {
		var err error
		authorityOwnRule, err = resolveAuthVarsInRuleNode(authorityOwnRule, edge, outerChildTypeName, immediateChildTypeName, incomingEdges, visited, sch)
		if err != nil {
			return nil, err
		}
	}

	// ── Recurse into authority's own cascade parents.
	visited[edge.parentTypeName] = true
	defer func() { visited[edge.parentTypeName] = false }()

	authorityAstType := &astType{
		typ:             &ast.Type{NamedType: edge.parentTypeName},
		inSchema:        sch,
		dgraphPredicate: sch.dgraphPredicate,
	}
	authorityPolicy := authorityAstType.CascadeAuthPolicyConfig()

	// Determine the outerChildTypeName to thread into recursive (grandparent) calls.
	// For "parent" and "propagate", the effective donor type must advance at each hop
	// so that the correct vars propagate through the full chain.
	nextOuterChildTypeName := computeNextOuterChildTypeName(edge, outerChildTypeName, immediateChildTypeName, incomingEdges, visited, sch)

	parentEdges := incomingEdges[edge.parentTypeName]
	var parentCascadeRules []*RuleNode
	for _, parentEdge := range parentEdges {
		// immediateChildTypeName for the recursive call = edge.parentTypeName:
		// the authority at this depth becomes the immediate child at depth+1.
		gpRule, err := buildCascadeRule(
			parentEdge, nextOuterChildTypeName, edge.parentTypeName, op,
			incomingEdges, authRulesSnapshot, visited, depth+1, sch,
		)
		if err != nil {
			return nil, err
		}
		if gpRule != nil {
			parentCascadeRules = append(parentCascadeRules, gpRule)
		}
	}

	// ── Combine authority's own rule + parent cascade rules per authority's OWN policy.
	var authorityFullAuth *RuleNode
	switch {
	case len(parentCascadeRules) == 0:
		// No cascade parents — just the authority's own rule.
		authorityFullAuth = authorityOwnRule

	case authorityOwnRule == nil:
		// Through-type: no own @auth, but has cascade parents.
		if len(parentCascadeRules) == 1 {
			authorityFullAuth = parentCascadeRules[0]
		} else if authorityPolicy.Aggregation == "or" {
			authorityFullAuth = &RuleNode{Or: parentCascadeRules}
		} else {
			authorityFullAuth = &RuleNode{And: parentCascadeRules}
		}

	default:
		// Both own @auth AND cascade parents — combine per authority's policy.
		var parentCascade *RuleNode
		if len(parentCascadeRules) == 1 {
			parentCascade = parentCascadeRules[0]
		} else if authorityPolicy.Aggregation == "or" {
			parentCascade = &RuleNode{Or: parentCascadeRules}
		} else {
			parentCascade = &RuleNode{And: parentCascadeRules}
		}
		if authorityPolicy.Aggregation == "or" {
			authorityFullAuth = mergeAuthNodeWithOr(authorityOwnRule, parentCascade)
		} else {
			authorityFullAuth = &RuleNode{And: []*RuleNode{authorityOwnRule, parentCascade}}
		}
	}

	if authorityFullAuth == nil {
		return nil, nil
	}

	return &RuleNode{
		CascadeWrapPred:  edge.dgraphPred,
		CascadeWrapType:  edge.parentTypeName,
		CascadeWrapInner: authorityFullAuth,
	}, nil
}

// cascadeAuthRuleForEdge builds the RuleNode for a single incoming cascade edge.
// op is the operation being processed ("query", "add", "update", "delete").
// For add/update/delete it prefers the authority's op-specific @auth rule,
// falling back to @auth(query:...) when no op-specific rule is declared.
//
// childTypeName is the OUTERMOST queried type — @authVariables from this type
// propagate down through all depths for substitution.
// immediateChildTypeName is the type declaring the @cascadeAuth field at THIS
// specific depth — used only to validate variableContext:"self" requirements.
// At depth=0 both are the same; at deeper depths immediateChildTypeName is the
// intermediate type (the authority from the calling frame).
func cascadeAuthRuleForEdge(edge cascadeAuthIncomingEdge, childTypeName, immediateChildTypeName string, op string,
	incomingEdges map[string][]cascadeAuthIncomingEdge,
	authRules map[string]*TypeAuth,
	visited map[string]bool, depth int, sch *schema) (*RuleNode, error) {

	if visited[edge.parentTypeName] {
		return nil, nil
	}
	if edge.cfg.Depth != -1 && depth >= edge.cfg.Depth {
		return nil, nil
	}

	ta := authRules[edge.parentTypeName]
	if ta == nil || ta.Rules == nil {
		// Authority has no own @auth.
		//
		// Case 1: it's an interface — collect auth rules from all concrete
		// implementors and use their union instead.
		authorityDef := sch.schema.Types[edge.parentTypeName]
		if authorityDef != nil && authorityDef.Kind == ast.Interface {
			implRule := interfaceImplementorAuthRules(sch, edge.parentTypeName, authRules)
			if implRule == nil {
				return nil, nil
			}
			// Wrap with the cascade edge pred (child→parent traversal predicate).
			return withCascadeEdgePred(implRule, edge.dgraphPred), nil
		}

		// Case 2: it's a concrete type with no own auth but it has incoming
		// cascade edges (e.g. Company has no @auth but Company←Group←Workspace).
		// Build a bundle where the PRIMARY leaf is a synthetic "pass-all" CascadeThroughType
		// node (emitting var(func: type(Company)) with no filter), and the grandparent
		// rules are applied onto that var as uid_in filters.
		// This enables 4+-level chains: AdPostRecord→Company→Group→Workspace.
		if authorityDef == nil {
			return nil, nil
		}
		parentEdgesThrough := incomingEdges[edge.parentTypeName]
		if len(parentEdgesThrough) == 0 {
			return nil, nil // no own auth and no cascade to inherit — nothing to emit
		}
		visited[edge.parentTypeName] = true
		defer func() { visited[edge.parentTypeName] = false }()
		var throughRules []*RuleNode
		for _, parentEdge := range parentEdgesThrough {
			// Pass the outermost childTypeName (not the intermediate through-type)
			// so that @authVariables substitution at every depth uses the original
			// queried type's vars — e.g. JobAd.QRY_PERMISSIONS propagates into
			// Group's and Workspace's auth rules, not Group.QRY_PERMISSIONS.
			// immediateChildTypeName = edge.parentTypeName (through-node is the immediate child).
			gpRule, err := cascadeAuthRuleForEdge(parentEdge, childTypeName, edge.parentTypeName, op,
				incomingEdges, authRules, visited, depth+1, sch)
			if err != nil {
				return nil, err
			}
			if gpRule != nil {
				throughRules = append(throughRules, gpRule)
			}
		}
		if len(throughRules) == 0 {
			return nil, nil
		}
		// Synthetic primary leaf: represents the through-node (e.g. Company)
		// with no auth filter — just a type scan. The DQL rewriter emits:
		//   Var as var(func: type(Company))  [with grandparent uid_in filters on it]
		primaryLeaf := &RuleNode{
			CascadeEdgePred:    edge.dgraphPred,
			CascadeThroughType: edge.parentTypeName,
		}
		// Build the AND bundle: [primary, ...grandparents]
		andChildren := append([]*RuleNode{primaryLeaf}, throughRules...)
		return &RuleNode{
			And:               andChildren,
			CascadeBundlePred: edge.dgraphPred,
		}, nil
	}

	// Select the op-specific authority rule, falling back to query.
	// Doc table: add→@auth(add:..) || @auth(query:..), etc.
	var authorityRule *RuleNode
	switch op {
	case "add":
		authorityRule = ta.Rules.Add
	case "update":
		authorityRule = ta.Rules.Update
	case "delete":
		authorityRule = ta.Rules.Delete
	}
	if authorityRule == nil {
		authorityRule = ta.Rules.Query // fallback / query path
	}
	if authorityRule == nil {
		return nil, nil
	}

	// Clone the authority rule and apply @authVariables substitution.
	// immediateChildTypeName (the type at THIS depth declaring the @cascadeAuth field)
	// is edge.parentTypeName of the calling frame. At depth=0 it equals childTypeName.
	// We use edge.dgraphPred to identify the field; the immediate child is whoever
	// declared it — tracked by the caller as the previous edge.parentTypeName.
	// Here we compute it: at the point of this call, childTypeName is the outermost
	// queried type. The immediate child is the type that HAS the @cascadeAuth field
	// pointing to edge.parentTypeName (the authority). That type was passed as
	// immediateChildTypeName from the outer frame.
	parentRule, err := resolveAuthVarsInRuleNode(authorityRule, edge, childTypeName, immediateChildTypeName, incomingEdges, visited, sch)
	if err != nil {
		return nil, err
	}
	if parentRule == nil {
		return nil, nil
	}

	// If the parent itself has incoming cascade edges, AND-chain the grandparent rule.
	parentEdges := incomingEdges[edge.parentTypeName]
	if len(parentEdges) > 0 {
		visited[edge.parentTypeName] = true
		defer func() { visited[edge.parentTypeName] = false }()

		var grandparentRules []*RuleNode
		for _, parentEdge := range parentEdges {
			// Pass the outermost childTypeName (not the intermediate edge.parentTypeName)
			// so substitution at every depth uses the originally queried type's
			// @authVariables — the full chain A→B→C all use A's QRY_PERMISSIONS.
			// immediateChildTypeName for the recursive call = edge.parentTypeName
			// (the authority at THIS depth becomes the immediate child at depth+1).
			gpRule, err := cascadeAuthRuleForEdge(parentEdge, childTypeName, edge.parentTypeName, op,
				incomingEdges, authRules, visited, depth+1, sch)
			if err != nil {
				return nil, err
			}
			if gpRule != nil {
				grandparentRules = append(grandparentRules, gpRule)
			}
		}
		if len(grandparentRules) > 0 {
			// Always build an And bundle node here — the CascadeBundlePred structure
			// ensures rewriteCascadeBundle is invoked, which applies grandparent
			// uid_in filters ONTO the primary authority var (not onto the child type).
			// CascadeAuthAggregation carries the authority type's @cascadeAuthPolicy
			// so rewriteCascadeBundle can use OR instead of AND where declared.
			parentAstType := &astType{
				typ:             &ast.Type{NamedType: edge.parentTypeName},
				inSchema:        sch,
				dgraphPredicate: sch.dgraphPredicate,
			}
			parentPolicy := parentAstType.CascadeAuthPolicyConfig()
			allRules := append([]*RuleNode{parentRule}, grandparentRules...)
			parentRule = &RuleNode{
				And:                    allRules,
				CascadeRootType:        edge.parentTypeName,
				CascadeAuthAggregation: parentPolicy.Aggregation,
			}
		}
	}

	// Wrap parentRule with cascade edge pred.
	parentRule = withCascadeEdgePred(parentRule, edge.dgraphPred)

	return parentRule, nil
}

// withCascadeEdgePred returns a deep clone of the RuleNode tree with
// CascadeEdgePred set on every leaf Rule node that doesn't already have one.
// Cloning is required because ta.Rules.Query is shared across all child types
// that reference the same authority; mutating in place would corrupt parallel
// cascade expansions. Skipping already-set leaves preserves the edge pred that
// was set by recursive grandparent processing.
func withCascadeEdgePred(rn *RuleNode, pred string) *RuleNode {
	if rn == nil {
		return nil
	}
	clone := *rn // shallow copy of the struct fields
	if rn.Rule != nil {
		// Leaf node: set pred only if not already set (preserves grandparent preds).
		if clone.CascadeEdgePred == "" {
			clone.CascadeEdgePred = pred
			// CascadeInversePred is the diagnostic alias for the same predicate.
			// collectCascadeInversePreds in tests uses this field for introspection.
			clone.CascadeInversePred = pred
		}
		// Always preserve type filter (set by interface-authority expansion).
		// It must survive the clone so the rewriter can emit @filter(type(X)).
		return &clone
	}
	// For composite (AND/OR/Not) nodes: set CascadeBundlePred to mark this node
	// as a cascade bundle wrapper. The bundle predicate identifies the edge that
	// uid_in will traverse to scope the child type from authority nodes.
	// This is used by collectCascadeInversePreds in tests for introspection.
	if len(rn.Or) > 0 || len(rn.And) > 0 || rn.Not != nil {
		if clone.CascadeBundlePred == "" {
			clone.CascadeBundlePred = pred
		}
	}
	if len(rn.Or) > 0 {
		clone.Or = make([]*RuleNode, len(rn.Or))
		for i, child := range rn.Or {
			clone.Or[i] = withCascadeEdgePred(child, pred)
		}
	}
	if len(rn.And) > 0 {
		clone.And = make([]*RuleNode, len(rn.And))
		for i, child := range rn.And {
			clone.And[i] = withCascadeEdgePred(child, pred)
		}
	}
	if rn.Not != nil {
		clone.Not = withCascadeEdgePred(rn.Not, pred)
	}
	return &clone
}

// resolveAuthVarsInRuleNode applies the variableContext strategy to produce the
// rule template with the appropriate @authVariables applied.
//
//   - variableContext: ""         — unset; treated as "adaptive" (the doc default).
//   - variableContext: "self"     — re-substitutes the child's own @authVariables
//     into the authority's raw rule template. All {{KEY}} values required by the
//     template must be declared in the child's @authVariables.
//   - variableContext: "parent"   — no re-substitution. Child inherits the authority's
//     compiled rule: concrete @auth (if already compiled, Rule!=nil) AND the
//     cascade chain (parentCascadeRules from the step-3 recursion). If the authority's
//     own @auth is an uncompiled template (Rule==nil), that compilation happens during
//     the authority's OWN cascade processing — return nil here so that only
//     parentCascadeRules contributes. The child never checks or provides vars.
//   - variableContext: "adaptive" — try self first: if the child (or its immediate
//     ancestor at this depth) has @authVariables, re-substitute them into the template.
//     If self fails (child has no @authVariables), fall back to parent's compiled rule:
//     pass through if already compiled (Rule!=nil), return nil if uncompiled template
//     so that parentCascadeRules provides cascade protection.
func resolveAuthVarsInRuleNode(parentRule *RuleNode, edge cascadeAuthIncomingEdge,
	childTypeName, immediateChildTypeName string,
	incomingEdges map[string][]cascadeAuthIncomingEdge,
	visited map[string]bool,
	sch *schema) (*RuleNode, error) {

	switch edge.cfg.VariableContext {
	case "self":
		// Validate that the immediate child has @authVariables.
		immediateTypeDef := sch.schema.Types[immediateChildTypeName]
		if immediateTypeDef == nil || immediateTypeDef.Directives.ForName(authVariablesDirective) == nil {
			return nil, gqlerror.Errorf(
				"Type %s: @cascadeAuth field %s has variableContext: self but "+
					"type %s has no @authVariables directive. "+
					"Either add @authVariables to %s or change variableContext to adaptive.",
				immediateChildTypeName, edge.fieldName, immediateChildTypeName, immediateChildTypeName)
		}
		// Re-substitute using the outermost child's vars (A→B→C all use A's vars).
		return resubstituteRuleNode(parentRule, edge, childTypeName, sch)

	case "parent":
		// No re-substitution. No vars check. The child inherits whatever
		// compiled rule the authority has.
		//
		// If the authority's own @auth is already compiled (self-contained rule,
		// Rule!=nil), pass it through as authorityOwnRule — it contributes to
		// authorityFullAuth alongside parentCascadeRules.
		//
		// If the rule is an uncompiled template (Rule==nil), the authority's
		// compilation happens during its own cascade processing, not here.
		// Return nil so that only parentCascadeRules (from the step-3 recursion)
		// contributes to authorityFullAuth.
		if parentRule != nil && parentRule.Rule != nil {
			return parentRule, nil
		}
		return nil, nil

	default: // "adaptive" or "" (unset — treated as adaptive)
		if hasAuthVariables(sch, childTypeName) {
			return resubstituteRuleNode(parentRule, edge, childTypeName, sch)
		}
		if hasAuthVariables(sch, immediateChildTypeName) {
			return resubstituteRuleNode(parentRule, edge, immediateChildTypeName, sch)
		}
		// Self failed — neither child has @authVariables.
		// Fall back to parent's compiled rule. For adaptive, we return parentRule
		// as-is (unlike parent which skips uncompiled templates). The template rule
		// node is still valid here: step-3 (grandparent recursion) will compose it
		// with grandparent cascade rules. If the authority has no cascade chain and
		// its rule is an uncompiled template (Rule==nil), there's no protection —
		// but validation blocks that configuration anyway.
		return parentRule, nil
	}
}

// computeNextOuterChildTypeName determines what outerChildTypeName to pass to
// recursive grandparent calls in buildCascadeRule, based on the current edge's
// variableContext:
//
//   - "parent":  advance to edge.parentTypeName so B's vars propagate further.
//   - others:   pass outerChildTypeName unchanged.
func computeNextOuterChildTypeName(
	edge cascadeAuthIncomingEdge,
	outerChildTypeName, immediateChildTypeName string,
	incomingEdges map[string][]cascadeAuthIncomingEdge,
	visited map[string]bool,
	sch *schema,
) string {
	if edge.cfg.VariableContext == "parent" {
		// Advance to the immediate authority so its vars propagate further.
		return edge.parentTypeName
	}
	return outerChildTypeName
}

// resubstituteRuleNode recursively walks a parent RuleNode tree, and for each
// leaf rule node re-substitutes the child type's @authVariables into the raw
// RuleTemplate before re-parsing into a new RuleNode.
func resubstituteRuleNode(rn *RuleNode, edge cascadeAuthIncomingEdge,
	childTypeName string, sch *schema) (*RuleNode, error) {

	if rn == nil {
		return nil, nil
	}

	childAstType := &astType{
		typ:             &ast.Type{NamedType: childTypeName},
		inSchema:        sch,
		dgraphPredicate: sch.dgraphPredicate,
	}
	childVars := childAstType.AuthVariables()

	// Leaf rule node: re-substitute and re-parse.
	if rn.RuleTemplate != "" {
		// Resolve the authority type's own compile-time @authVariables FIRST
		// (e.g. {{QRY_PERMISSIONS}} defined on Workspace), then the child's.
		// This prevents spurious "unresolved placeholder" errors when the
		// authority has constant-valued placeholders the child never declares.
		authorityAstType := &astType{
			typ:             &ast.Type{NamedType: edge.parentTypeName},
			inSchema:        sch,
			dgraphPredicate: sch.dgraphPredicate,
		}
		authorityVars := authorityAstType.AuthVariables()
		child, err := parseRuleNodeFromTemplate(rn.RuleTemplate, authorityVars, childVars,
			childTypeName, edge.parentTypeName, sch)
		if err != nil {
			// Re-substitution failed. Propagate the error always — silently falling
			// back to the authority's pre-compiled rule (rn.Rule) would cause a
			// concrete type with e.g. value:[] in @authVariables to inherit the
			// authority's own permissions rather than enforcing deny-all, which is
			// a silent security misconfiguration. Schema load must fail instead.
			return nil, err
		}
		if child != nil {
			return child, nil
		}
		// RBAC or nil — fall through to parent rule.
		return rn, nil
	}

	// Composite nodes: recurse.
	if len(rn.Or) > 0 {
		newNode := &RuleNode{}
		for _, child := range rn.Or {
			resub, err := resubstituteRuleNode(child, edge, childTypeName, sch)
			if err != nil {
				return nil, err
			}
			newNode.Or = append(newNode.Or, resub)
		}
		return newNode, nil
	}
	if len(rn.And) > 0 {
		newNode := &RuleNode{}
		for _, child := range rn.And {
			resub, err := resubstituteRuleNode(child, edge, childTypeName, sch)
			if err != nil {
				return nil, err
			}
			newNode.And = append(newNode.And, resub)
		}
		return newNode, nil
	}
	if rn.Not != nil {
		resub, err := resubstituteRuleNode(rn.Not, edge, childTypeName, sch)
		if err != nil {
			return nil, err
		}
		return &RuleNode{Not: resub}, nil
	}
	// RBAC rules are static — return as-is.
	return rn, nil
}

// parseRuleNodeFromTemplate substitutes childVars into the raw rule template,
// then parses the result into a RuleNode (GraphQL rule leaf only — not RBAC).
// Returns (nil, nil) for RBAC rules (caller uses parent as-is).
// Returns (nil, err) when substitution leaves unresolved placeholders or parse fails.
//
// Important: the rule is validated against the AUTHORITY type definition, not the
// child type. A cascaded rule like queryWorkspace(...) queries the authority; validating
// it against Contact (the child) would always fail with "expected queryContact, found queryWorkspace".
//
// authorityVars are the compile-time @authVariables constants from the authority type
// (e.g. {{QRY_PERMISSIONS}} on Workspace). They are resolved FIRST so that authority-level
// constants do not show up as unresolved when the child's own @authVariables are applied.
// childVars are the child type's own @authVariables (may include overlapping keys; child
// values take precedence so a child can override an authority default if needed).
func parseRuleNodeFromTemplate(template string, authorityVars, childVars map[string]string,
	childTypeName, authorityTypeName string, sch *schema) (*RuleNode, error) {

	// Step 1: apply child @authVariables FIRST so child-specific values take
	// precedence over the authority's own values for overlapping keys.
	// Example: JobAd.QRY_PERMISSIONS = [_ALL _JOBAD ...] must override
	// Group.QRY_PERMISSIONS = [_ALL _GROUP ...] when JobAd cascades through Group.
	//
	// value:[] is substituted verbatim as "[]" — when the template contains
	// in: {{KEY}}, this produces in: [] which buildFilter converts to uid(0x0)
	// (deny-all). This is the intended behaviour for a child that explicitly
	// declares value:[] to block access through a particular cascade arm.
	substituted, _ := substitutAuthVars(template, childVars)
	// Step 2: resolve any remaining authority compile-time constants that the
	// child did NOT declare at all (e.g. <<ADM_PERMISSIONS>> if child lacks that
	// key entirely). Uses the skip-on-empty variant so that authority stubs don't
	// clobber placeholders the child already resolved in step 1.
	substituted, _ = substitutAuthVars(substituted, authorityVars)
	if strings.HasPrefix(substituted, RBACQueryPrefix) {
		return nil, nil
	}
	// Unresolved placeholders produce a cryptic "Expected Name, found {" from the
	// GraphQL parser. Intercept and emit a meaningful error naming the missing keys.
	if unresolved := unresolvedAuthVarKeys(substituted); len(unresolved) > 0 {
		return nil, fmt.Errorf(
			"Type %s: @cascadeAuth: expanding from authority type %s: "+
				"unresolved @authVariables placeholders in cascaded rule: %v. "+
				"Check that @authVariables on Type %s declares all keys used in %s's @auth rule",
			childTypeName, authorityTypeName, unresolved, childTypeName, authorityTypeName)
	}
	node := &RuleNode{RuleTemplate: template}
	// IMPORTANT: use gqlValidateRule (not gqlParseRuleForCascade) to compile the rule.
	// gqlValidateRule calls validator.Validate() which populates ast.Field.Definition
	// on every field in the AST. This is REQUIRED — without it, field.Arguments()
	// panics at query time when ArgumentMap() is called on a nil Definition.
	//
	// The rule may query a different type than the authority type. For example, Group's
	// @auth rule might say `queryIAMResource(...)` because the auth was defined on the
	// IAMResource interface that Group implements. gqlValidateRule checks
	// f.Name == "query"+typ.Name — so we need to pass the type that the rule actually
	// queries, not necessarily the authority type.
	//
	// Strategy: infer the queried type from the root query field name in the substituted
	// rule, look it up in the schema, and validate against that type. Fall back to the
	// authority type def if the inferred type is not found.
	typeDef := inferQueriedTypeDef(sch, substituted, authorityTypeName)
	if err := gqlValidateRule(sch, typeDef, substituted, node); err != nil {
		return nil, fmt.Errorf(
			"Type %s: @cascadeAuth: expanding from authority type %s: %w",
			childTypeName, authorityTypeName, err)
	}
	return node, nil
}

// inferQueriedTypeDef parses the root query field name from a GQL rule string
// (e.g. "query { queryIAMResource(...) {...} }") and returns the *ast.Definition
// for the type it queries (e.g. sch.schema.Types["IAMResource"]). Falls back to
// sch.schema.Types[authorityTypeName] if the inferred type is not in the schema.
//
// This is needed because authority types can have @auth rules that query an interface
// they implement (the auth was defined on the interface). gqlValidateRule checks
// f.Name == "query"+typ.Name — so we must pass the type the rule actually queries.
func inferQueriedTypeDef(sch *schema, rule, authorityTypeName string) *ast.Definition {
	fallback := sch.schema.Types[authorityTypeName]

	// Fast path: extract the root query field name with a simple string scan.
	// Rules have the shape: "query(...) {\n  queryFoo(...) { ... }\n}"
	// Find "query" keyword at the field position (after the outer "query {" block).
	// We look for a token starting with "query" followed by a non-lowercase letter
	// (to distinguish the operation keyword from field names like "queryFoo").
	// Simple approach: find the second occurrence of "query" that is followed by
	// an uppercase letter — that's the root field name.
	const prefix = "query"
	remaining := rule
	foundOp := false
	for {
		idx := strings.Index(remaining, prefix)
		if idx == -1 {
			break
		}
		token := remaining[idx:]
		remaining = remaining[idx+len(prefix):]
		// Skip the operation keyword "query" (followed by space, newline, '(', or '{').
		if !foundOp {
			if len(remaining) == 0 || remaining[0] == ' ' || remaining[0] == '\n' ||
				remaining[0] == '\t' || remaining[0] == '(' || remaining[0] == '{' {
				foundOp = true
				continue
			}
		}
		// This "query" is followed by a name character — it's a field like "queryFoo".
		end := len(prefix)
		for end < len(token) && (token[end] >= 'A' && token[end] <= 'Z' ||
			token[end] >= 'a' && token[end] <= 'z' ||
			token[end] >= '0' && token[end] <= '9' || token[end] == '_') {
			end++
		}
		fieldName := token[:end] // e.g. "queryIAMResource"
		// The type name is fieldName with "query" prefix stripped.
		typeName := fieldName[len(prefix):] // e.g. "IAMResource"
		if def := sch.schema.Types[typeName]; def != nil {
			return def
		}
		break
	}
	return fallback
}

// childOwnQueryRule returns the child type's own @auth(query:...) RuleNode.
func childOwnQueryRule(childTypeName string, authRules map[string]*TypeAuth) *RuleNode {
	ta := authRules[childTypeName]
	if ta == nil || ta.Rules == nil {
		return nil
	}
	return ta.Rules.Query
}

// interfaceImplementorAuthRules returns an OR RuleNode covering every concrete type
// that implements the given interface. Each implementor's RuleNode has
// CascadeEdgePredTypeFilter set to the implementor's type name, so the DQL rewriter
// can emit @filter(type(X)) on the edge traversal child block.
//
// This is used when @cascadeAuth points to an interface authority (e.g. Recordable)
// that has no own @auth — the union of all implementing types' auth is used instead.
// Returns nil if no implementor has auth.
func interfaceImplementorAuthRules(sch *schema, ifaceName string, authRules map[string]*TypeAuth) *RuleNode {
	var or []*RuleNode
	for _, typ := range sch.schema.Types {
		if typ.Kind != ast.Object {
			continue
		}
		isImpl := false
		for _, iface := range typ.Interfaces {
			if iface == ifaceName {
				isImpl = true
				break
			}
		}
		if !isImpl {
			continue
		}
		ta := authRules[typ.Name]
		if ta == nil || ta.Rules == nil || ta.Rules.Query == nil {
			continue
		}
		// Clone the implementor's rule tree and tag every leaf with the type filter.
		tagged := tagRuleNodeWithTypeFilter(ta.Rules.Query, typ.Name)
		if tagged != nil {
			or = append(or, tagged)
		}
	}
	if len(or) == 0 {
		return nil
	}
	if len(or) == 1 {
		return or[0]
	}
	return &RuleNode{Or: or}
}

// tagRuleNodeWithTypeFilter clones a RuleNode tree, setting CascadeEdgePredTypeFilter
// to typeName on every leaf Rule node. This causes the DQL rewriter to emit
// @filter(type(typeName)) on the edge traversal block for that leaf's auth var.
func tagRuleNodeWithTypeFilter(rn *RuleNode, typeName string) *RuleNode {
	if rn == nil {
		return nil
	}
	clone := *rn
	if rn.Rule != nil {
		clone.CascadeEdgePredTypeFilter = typeName
		return &clone
	}
	if len(rn.Or) > 0 {
		clone.Or = make([]*RuleNode, len(rn.Or))
		for i, child := range rn.Or {
			clone.Or[i] = tagRuleNodeWithTypeFilter(child, typeName)
		}
	}
	if len(rn.And) > 0 {
		clone.And = make([]*RuleNode, len(rn.And))
		for i, child := range rn.And {
			clone.And[i] = tagRuleNodeWithTypeFilter(child, typeName)
		}
	}
	if rn.Not != nil {
		clone.Not = tagRuleNodeWithTypeFilter(rn.Not, typeName)
	}
	return &clone
}

// snapshotAuthRules returns a copy of authRules where each TypeAuth.Rules
// pointer is replaced with a fresh copy of the AuthContainer struct.
// This prevents bidirectional writes (which overwrite Rules.Query on the live
// authRules map) from affecting cascade reads that should only see each
// authority type's declared @auth rules.
func snapshotAuthRules(src map[string]*TypeAuth) map[string]*TypeAuth {
	dst := make(map[string]*TypeAuth, len(src))
	for k, v := range src {
		if v == nil {
			dst[k] = nil
			continue
		}
		cp := *v // copy TypeAuth struct by value
		if v.Rules != nil {
			rCp := *v.Rules // copy AuthContainer so Rules.Query stays stable
			cp.Rules = &rCp
		}
		dst[k] = &cp
	}
	return dst
}

// operationsFromEdges returns the union of operations from all incoming edges.
// When an edge has no explicit operations list (the @cascadeAuth directive was
// written without the operations: argument), it defaults to all four operations:
// query, add, update, delete — matching the directive documentation.
// When operations: [] is explicitly provided (OperationsProvided=true, empty
// slice), no operations are covered and the cascade rule is skipped for that edge.
func operationsFromEdges(edges []cascadeAuthIncomingEdge) []string {
	seen := make(map[string]bool)
	for _, e := range edges {
		if !e.cfg.OperationsProvided {
			// No explicit operations: arg — default to all operations.
			for _, op := range []string{"query", "add", "update", "delete"} {
				seen[op] = true
			}
		} else {
			// Explicitly provided list — may be empty (operations: []) to opt out.
			for _, op := range e.cfg.Operations {
				seen[op] = true
			}
		}
	}
	result := make([]string, 0, len(seen))
	for op := range seen {
		result = append(result, op)
	}
	return result
}

// mergeIntoOp merges a RuleNode into the appropriate TypeAuth slot using AND or OR.
func mergeIntoOp(ta *TypeAuth, op string, rule *RuleNode, aggregation string) {
	merge := mergeAuthNodeWithAnd
	if aggregation == "or" {
		merge = mergeAuthNodeWithOr
	}
	if ta.Rules == nil {
		ta.Rules = &AuthContainer{}
	}
	switch op {
	case "query":
		ta.Rules.Query = merge(ta.Rules.Query, rule)
	case "add":
		ta.Rules.Add = merge(ta.Rules.Add, rule)
	case "update":
		ta.Rules.Update = merge(ta.Rules.Update, rule)
	case "delete":
		ta.Rules.Delete = merge(ta.Rules.Delete, rule)
	}
}

// composeCascadeWithBase is the Stage-3 composition step for auth rule building.
// It combines cascade edge rules and merges the result into the child type's
// existing base rule (which already contains Stage-1 TypeAuth + Stage-2 InterfaceAuth).
//
// Composition order (AND-before-OR):
//  1. AND-aggregate all cascade edge rules when aggregation is "and".
//     OR-aggregate all cascade edge rules when aggregation is "or".
//     This forms the cascade block from the raw per-edge contributions.
//  2. Merge the cascade block into the base rule using the same aggregation:
//     "and" → final = AND(base, cascadeBlock)  — cascade restricts access
//     "or"  → final = OR(base,  cascadeBlock)  — cascade adds an access path
//
// The aggregation value comes from @cascadeAuthPolicy(aggregation:) on the child
// type. When no type-level policy is declared, CascadeAuthPolicyConfig defaults
// to "and" — this is the effective per-edge fallback.
func composeCascadeWithBase(ta *TypeAuth, op string, edgeRules []*RuleNode, aggregation string) {
	if len(edgeRules) == 0 {
		return
	}

	// Step 1 — aggregate cascade edge rules (AND first, OR second).
	// When aggregation is "and": AND-combine all edges into a single cascade block.
	// When aggregation is "or":  OR-combine  all edges into a single cascade block.
	var cascadeBlock *RuleNode
	switch {
	case len(edgeRules) == 1:
		cascadeBlock = edgeRules[0]
	case aggregation == "or":
		cascadeBlock = &RuleNode{Or: edgeRules}
	default: // "and" — AND-aggregate first
		cascadeBlock = &RuleNode{And: edgeRules}
	}

	// Step 2 — merge cascade block into the base rule per aggregation policy.
	mergeIntoOp(ta, op, cascadeBlock, aggregation)
}

// mergeAuthNodeWithOr is the OR counterpart to mergeAuthNodeWithAnd.
func mergeAuthNodeWithOr(a, b *RuleNode) *RuleNode {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return &RuleNode{Or: []*RuleNode{a, b}}
}

// authVarFuncMap builds a text/template FuncMap from a vars map plus the
// built-in transformation functions. Each authVariables key is registered as
// a zero-argument function returning its string value, so the <<KEY>> template
// action calls KEY() rather than accessing a data field — preserving the
// existing placeholder syntax without requiring a dot prefix (<<.KEY>>).
//
// Built-in transforms (usable as pipeline stages):
//
//	<<QRY_PERMISSIONS | toStrings>>         — enum list → JSON string array (case preserved)
//	<<QRY_PERMISSIONS | toStrings | lower>>  — enum list → lowercase JSON string array
func authVarFuncMap(vars map[string]string, typeName string) template.FuncMap {
	fm := template.FuncMap{
		"toStrings": enumListToJSONStrings,
		"lower":     strings.ToLower,
	}
	for key, val := range vars {
		v := val // capture loop variable
		fm[key] = func() string { return v }
	}
	if typeName != "" {
		fm["TYPE"] = func() string { return typeName }
	}
	return fm
}

// substitutAuthVars performs compile-time <<KEY>> → verbatim substitution using
// Go's text/template engine with custom << >> delimiters.
//
// Each key in vars is registered as a zero-argument FuncMap function so that
// the existing <<KEY>> placeholder syntax is preserved (no dot prefix needed).
// Pipelines are supported: <<QRY_PERMISSIONS | lower>> lowercases enum values
// to a JSON string array suitable for RBAC `in` rules.
//
// Returns (substituted, nil) on success. Returns ("", err) if the template
// fails to parse or execute — callers should treat this as a deferred
// substitution (interface stub pattern) and leave the rule node unchanged.
// Genuine typos are caught by the post-pass scanUnresolvedInNode sweep.
func substitutAuthVars(ruleStr string, vars map[string]string) (string, error) {
	if len(vars) == 0 {
		return ruleStr, nil
	}
	tmpl, err := template.New("rule").Delims("<<", ">>").Funcs(authVarFuncMap(vars, "")).Parse(ruleStr)
	if err != nil {
		// Template parse error (e.g. undefined function for interface stub key).
		// Return the original string so callers can detect it is still unresolved.
		return ruleStr, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, nil); err != nil {
		return ruleStr, err
	}
	return buf.String(), nil
}

// resolveAuthVariables is the richer variant that also substitutes <<TYPE>>
// via the template FuncMap — kept for use outside the parse pipeline.
func resolveAuthVariables(ruleStr string, vars map[string]string, typeName string) string {
	tmpl, err := template.New("rule").Delims("<<", ">>").Funcs(authVarFuncMap(vars, typeName)).Parse(ruleStr)
	if err != nil {
		return ruleStr
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, nil); err != nil {
		return ruleStr
	}
	return buf.String()
}

// enumListToJSONStrings converts a GraphQL enum list or unquoted identifier list
// to a JSON string array, preserving the original casing. Scalar values are
// JSON-quoted. Use in a pipeline with lower to also lowercase:
//
//	<<QRY_PERMISSIONS | toStrings>>         → ["_ALL","_EMAIL","READ"]
//	<<QRY_PERMISSIONS | toStrings | lower>>  → ["_all","_email","read"]
//
// Examples:
//
//	[_ALL, _EMAIL, READ]   → ["_ALL","_EMAIL","READ"]
//	["_all", "read"]       → ["_all","read"]   (already quoted, no-op)
//	_ALL                   → "_ALL"
func enumListToJSONStrings(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "[") {
		// Scalar — strip any existing quotes and re-encode as JSON string.
		b, _ := json.Marshal(strings.Trim(raw, `"`))
		return string(b)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]")
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if p == "" {
			continue
		}
		b, _ := json.Marshal(p)
		out = append(out, string(b))
	}
	return "[" + strings.Join(out, ",") + "]"
}

// findInversePredicate scans the authorityTypeName's AST definition for the
// field that declares @hasInverse(field: childFieldName) AND whose element type
// matches concreteChildTypeName (or is an interface that concreteChildTypeName
// implements). It returns the field's fully-qualified Dgraph predicate name
// (e.g. "Workspace.hasCandidate" for a Candidate child, "Workspace.hasUser"
// for a User child — both declare @hasInverse(field: inWorkspace)).
//
// An exact type match is preferred over an interface match. If no exact match
// exists but the authority field targets an interface the child implements,
// the first such interface match is returned.
//
// This is necessary because @hasInverse is commonly declared on the *authority*
// side of a relationship (e.g. Workspace.hasCandidate @hasInverse(field:
// inWorkspace)) rather than on the child field itself. Without type-filtering,
// the first-match would be non-deterministic when multiple authority fields share
// the same @hasInverse target field name (e.g. hasUser, hasCandidate, hasGroup
// all declare field: inWorkspace on Workspace).
func findInversePredicate(sch *schema, authorityTypeName, childFieldName, concreteChildTypeName string) string {
	authDef := sch.schema.Types[authorityTypeName]
	if authDef == nil {
		return ""
	}

	buildPred := func(f *ast.FieldDefinition) string {
		authAstType := &astType{
			typ:             &ast.Type{NamedType: authorityTypeName},
			inSchema:        sch,
			dgraphPredicate: sch.dgraphPredicate,
		}
		fd := &fieldDefinition{
			fieldDef:        f,
			inSchema:        sch,
			dgraphPredicate: sch.dgraphPredicate,
			parentType:      authAstType,
		}
		return fd.DgraphPredicate()
	}

	// interfaceMatchPred holds the first candidate where the authority field's
	// element type is an interface implemented by concreteChildTypeName.
	// Used as fallback if no exact type match is found.
	var interfaceMatchPred string

	for _, f := range authDef.Fields {
		dir := f.Directives.ForName("hasInverse")
		if dir == nil {
			continue
		}
		arg := dir.Arguments.ForName("field")
		if arg == nil || arg.Value.Raw != childFieldName {
			continue
		}

		fieldElemType := f.Type.Name()

		// Priority 1: exact type match — this authority field points directly
		// to the concrete child type (e.g. Workspace.hasCandidate → [Candidate]).
		if fieldElemType == concreteChildTypeName {
			return buildPred(f)
		}

		// Priority 2: interface match — the authority field targets an interface
		// (e.g. WorkspaceMember) that concreteChildTypeName implements.
		// Record the first such match; continue scanning for a possible exact match.
		if interfaceMatchPred == "" {
			targetDef := sch.schema.Types[fieldElemType]
			if targetDef != nil && targetDef.Kind == ast.Interface {
				childDef := sch.schema.Types[concreteChildTypeName]
				if childDef != nil {
					for _, iface := range childDef.Interfaces {
						if iface == fieldElemType {
							interfaceMatchPred = buildPred(f)
							break
						}
					}
				}
			}
		}
	}

	return interfaceMatchPred
}

// ruleNodeKey returns a canonical string fingerprint of a RuleNode tree.
// It is used by the biDirSeen deduplication map in expandCascadeAuth to detect
// when multiple child types contribute structurally identical bidirectional rules.
//
// Design notes:
//   - Leaf nodes: keyed by RuleTemplate (pre-substitution rule string) + CascadeEdgePred.
//     RuleTemplate is stable across child types that share the same authority @auth rule.
//   - Or/And nodes: children are sorted so that `A OR B` and `B OR A` produce the same key.
//   - Not nodes: prefixed with "NOT".
//   - nil node: gives the empty string (safe to call).
func ruleNodeKey(rn *RuleNode) string {
	if rn == nil {
		return ""
	}
	// Leaf: DQL rule (pre-built query node)
	if rn.DQLRule != nil {
		return "DQL:" + rn.DQLRule.Var + rn.CascadeEdgePred
	}
	// Leaf: RBAC rule
	if rn.RBACRule != nil {
		return fmt.Sprintf("RBAC:%s%s%v%s", rn.RBACRule.Variable, rn.RBACRule.Operator, rn.RBACRule.Operand, rn.CascadeEdgePred)
	}
	// Leaf: GraphQL auth rule — use the pre-substitution template for stability.
	if rn.Rule != nil {
		tpl := rn.RuleTemplate
		if tpl == "" {
			// DQLQuery() is the stable compiled DQL string for this auth rule.
			// Avoid fmt.Sprintf("%v", rn.Rule) — for interface types that returns
			// a non-deterministic representation (often a pointer address).
			tpl = rn.Rule.DQLQuery()
		}
		return "RULE:" + tpl + "|" + rn.CascadeEdgePred
	}
	// Composite: Or
	if len(rn.Or) > 0 {
		parts := make([]string, len(rn.Or))
		for i, child := range rn.Or {
			parts[i] = ruleNodeKey(child)
		}
		// Sort children for order-insensitive comparison.
		for i := 0; i < len(parts)-1; i++ {
			for j := i + 1; j < len(parts); j++ {
				if parts[i] > parts[j] {
					parts[i], parts[j] = parts[j], parts[i]
				}
			}
		}
		return "OR[" + strings.Join(parts, ",") + "]"
	}
	// Composite: And
	if len(rn.And) > 0 {
		parts := make([]string, len(rn.And))
		for i, child := range rn.And {
			parts[i] = ruleNodeKey(child)
		}
		for i := 0; i < len(parts)-1; i++ {
			for j := i + 1; j < len(parts); j++ {
				if parts[i] > parts[j] {
					parts[i], parts[j] = parts[j], parts[i]
				}
			}
		}
		return "AND[" + strings.Join(parts, ",") + "]"
	}
	// Not
	if rn.Not != nil {
		return "NOT[" + ruleNodeKey(rn.Not) + "]"
	}
	return ""
}

// collectRuleNodeLeafKeys recursively extracts the ruleNodeKey of every LEAF
// RuleNode in the tree (i.e., nodes with a Rule, DQLRule, or RBACRule).
// Composite Or/And/Not wrappers are traversed transparently.
func collectRuleNodeLeafKeys(rn *RuleNode) []string {
	if rn == nil {
		return nil
	}
	// Leaf: has a concrete auth predicate.
	if rn.Rule != nil || rn.DQLRule != nil || rn.RBACRule != nil {
		return []string{ruleNodeKey(rn)}
	}
	var keys []string
	for _, child := range rn.Or {
		keys = append(keys, collectRuleNodeLeafKeys(child)...)
	}
	for _, child := range rn.And {
		keys = append(keys, collectRuleNodeLeafKeys(child)...)
	}
	if rn.Not != nil {
		keys = append(keys, collectRuleNodeLeafKeys(rn.Not)...)
	}
	return keys
}

// hasNewLeaves returns true if biDirRule contains at least one leaf whose
// (inversePred :: leafKey) pair has not been seen before, and records all
// leaves as seen regardless.  This enables leaf-level deduplication:
// if every auth condition a new type would contribute is already present from
// a previous type's merge (even as part of a larger AND node), the merge is
// skipped entirely.
func hasNewLeaves(biDirRule *RuleNode, inversePred string, seen map[string]bool) bool {
	leafKeys := collectRuleNodeLeafKeys(biDirRule)
	hasNew := false
	for _, lk := range leafKeys {
		fullKey := inversePred + "::" + lk
		if !seen[fullKey] {
			hasNew = true
			seen[fullKey] = true
		}
	}
	return hasNew
}
