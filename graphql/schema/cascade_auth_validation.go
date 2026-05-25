/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"strconv"

	"github.com/dgraph-io/gqlparser/v2/ast"
	"github.com/dgraph-io/gqlparser/v2/gqlerror"
	"github.com/hypermodeinc/dgraph/v25/x"
)

// cascadeAuthDirectiveValidation validates the @cascadeAuth directive.
// Rules:
//   - May only appear on edge (non-scalar, non-enum) fields
//   - Not allowed on @remote types, @custom or @lambda fields
//   - depth must be ≥ 1 or -1 if supplied
//   - variableContext must be "self" or "parent" if supplied
//   - No circular cascade chains (detected via DFS over @cascadeAuth edges)
func cascadeAuthDirectiveValidation(sch *ast.Schema,
	typ *ast.Definition,
	field *ast.FieldDefinition,
	dir *ast.Directive,
	secrets map[string]x.Sensitive) gqlerror.List {

	// Placement: disallow on @remote types.
	if typ.Directives.ForName(remoteDirective) != nil {
		return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
			"Type %s; Field %s: @cascadeAuth cannot be used on a @remote type", typ.Name, field.Name)}
	}

	// Placement: disallow on scalar fields and enums (must be an edge field).
	fieldTypeName := field.Type.Name()
	if isScalar(fieldTypeName) {
		return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
			"Type %s; Field %s: @cascadeAuth can only be used on edge (non-scalar) fields, not %s",
			typ.Name, field.Name, fieldTypeName)}
	}
	if sch.Types[fieldTypeName] != nil && sch.Types[fieldTypeName].Kind == ast.Enum {
		return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
			"Type %s; Field %s: @cascadeAuth cannot be used on enum fields", typ.Name, field.Name)}
	}

	// Placement: disallow on @custom and @lambda fields.
	if field.Directives.ForName(customDirective) != nil {
		return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
			"Type %s; Field %s: @cascadeAuth cannot be used on fields with @custom directive",
			typ.Name, field.Name)}
	}
	if field.Directives.ForName(lambdaDirective) != nil {
		return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
			"Type %s; Field %s: @cascadeAuth cannot be used on fields with @lambda directive",
			typ.Name, field.Name)}
	}

	// Require @hasInverse on either side of the edge.
	//
	// @cascadeAuth emits DQL as:
	//
	//   uid_in(<childPredicate>, <authorityVar>)
	//
	// rather than the less-efficient @cascade forward traversal. The rewriter
	// needs the forward Dgraph predicate (e.g. WorkspaceMember.inWorkspace) at
	// query time, which is derived from the @hasInverse link at expand time.
	// Without @hasInverse the predicate resolves to "" and the expand pass
	// returns an error at startup — catch it earlier here for a better UX.
	//
	// Note: either direction is valid — @hasInverse may be placed on the cascade
	// field itself OR on the authority type's back-reference field.
	if !hasInverseForCascadeAuth(sch, typ, field, fieldTypeName) {
		return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
			"Type %s; Field %s: @cascadeAuth requires @hasInverse to be declared — "+
				"either on this field or on the field of type %s that points back to %s. "+
				"@cascadeAuth uses uid_in(<predicate>, <authorityVar>) for efficient index-based "+
				"filtering and needs the inverse predicate to construct the query",
			typ.Name, field.Name, fieldTypeName, typ.Name)}
	}

	// Validate that the authority type has @auth with a query rule — or, if the
	// authority is an interface with no own @auth, that at least one implementing
	// type has @auth(query:...) or @cascadeAuth. Without either, @cascadeAuth is
	// a silent no-op and the child type will be unprotected.
	authorityDef := sch.Types[fieldTypeName]
	if authorityDef != nil && (authorityDef.Kind == ast.Object || authorityDef.Kind == ast.Interface) {
		authorityAuthDir := authorityDef.Directives.ForName(authDirective)
		hasQueryRule := authorityAuthDir != nil && authorityAuthDir.Arguments.ForName("query") != nil

		if !hasQueryRule {
			// Also accept if the authority type itself has @cascadeAuth on any field
			// — auth will be derived transitively at expand time.
			if typeHasCascadeAuthEdge(authorityDef) {
				hasQueryRule = true
			}
		}

		if !hasQueryRule {
			// For interface authority: accept if any implementor has @auth(query:...)
			// or @cascadeAuth on any field.
			if authorityDef.Kind == ast.Interface {
				for _, candDef := range sch.Types {
					if candDef.Kind != ast.Object {
						continue
					}
					for _, iface := range candDef.Interfaces {
						if iface != fieldTypeName {
							continue
						}
						implDir := candDef.Directives.ForName(authDirective)
						if (implDir != nil && implDir.Arguments.ForName("query") != nil) ||
							typeHasCascadeAuthEdge(candDef) {
							hasQueryRule = true
							break
						}
					}
					if hasQueryRule {
						break
					}
				}
			}
		}

		if !hasQueryRule {
			if authorityAuthDir == nil {
				return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
					"Type %s; Field %s: @cascadeAuth target type %q has no @auth directive — "+
						"the cascade will be a no-op and %s will be unprotected",
					typ.Name, field.Name, fieldTypeName, typ.Name)}
			}
			return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
				"Type %s; Field %s: @cascadeAuth target type %q has no @auth(query:...) rule — "+
					"only query auth is propagated by @cascadeAuth",
				typ.Name, field.Name, fieldTypeName)}
		}
	}

	// Validate depth: must be ≥ 1 or -1 if supplied.
	if depthArg := dir.Arguments.ForName("depth"); depthArg != nil && depthArg.Value.Raw != "" {
		d, err := strconv.ParseInt(depthArg.Value.Raw, 10, 64)
		if err != nil || (d < 1 && d != -1) {
			return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
				"Type %s; Field %s: @cascadeAuth depth must be ≥ 1 or -1, got %q",
				typ.Name, field.Name, depthArg.Value.Raw)}
		}
	}

	// Validate variableContext: must be "self", "parent", or "adaptive" if supplied.
	if vcArg := dir.Arguments.ForName("variableContext"); vcArg != nil && vcArg.Value.Raw != "" {
		vc := vcArg.Value.Raw
		if vc != "self" && vc != "parent" && vc != "adaptive" {
			return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
				`Type %s; Field %s: @cascadeAuth variableContext must be "self", "parent", or "adaptive", got %q`,
				typ.Name, field.Name, vc)}
		}

		if vc == "self" && typ.Kind != ast.Interface {
			// Interfaces can't carry @authVariables — their concrete implementors do.
			// These checks only apply to concrete types placing @cascadeAuth directly.

			// Check: the host type must have @authVariables — without it, self-substitution
			// falls back silently to the authority's own permissions, which is almost
			// certainly wrong. Use variableContext: "adaptive" to allow this explicitly.
			if typ.Directives.ForName(authVariablesDirective) == nil {
				return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
					"Type %s; Field %s: @cascadeAuth variableContext \"self\" requires "+
						"Type %s to declare @authVariables — without it, substitution "+
						"falls back to the authority type's own permissions. "+
						"Use variableContext: \"adaptive\" to allow this fallback explicitly",
					typ.Name, field.Name, typ.Name)}
			}

			// Check: all {{KEY}} placeholders in the authority type's @auth rule
			// must be covered by the host type's @authVariables.
			authorityDef := sch.Types[fieldTypeName]
			if authorityDef != nil {
				childVarKeys := authVarKeysFromDef(typ)
				var errs gqlerror.List
				for _, authDir := range authorityDef.Directives {
					if authDir.Name != authDirective {
						continue
					}
					for _, arg := range authDir.Arguments {
						if arg.Value == nil {
							continue
						}
						collectUnresolvedKeys(arg.Value, childVarKeys, typ.Name,
							field.Name, fieldTypeName, dir, &errs)
					}
				}
				if len(errs) > 0 {
					return errs
				}
			}
		}
		// "adaptive": no additional validation — intentional best-effort mode.
		// "parent": no additional validation — uses authority's rule unchanged.
	}

	// Cycle detection: DFS over @cascadeAuth edges from this field's target type.
	visited := map[string]bool{}
	var detectCycle func(typeName string) bool
	detectCycle = func(typeName string) bool {
		if visited[typeName] {
			return true
		}
		visited[typeName] = true
		def := sch.Types[typeName]
		if def == nil {
			return false
		}
		for _, f := range def.Fields {
			if f.Directives.ForName(cascadeAuthDirective) == nil {
				continue
			}
			if detectCycle(f.Type.Name()) {
				return true
			}
		}
		visited[typeName] = false
		return false
	}
	visited[typ.Name] = true
	if detectCycle(fieldTypeName) {
		return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
			"Type %s; Field %s: @cascadeAuth forms a cycle through type %s",
			typ.Name, field.Name, fieldTypeName)}
	}

	return nil
}

// authVarKeysFromDef returns the set of key names declared in @authVariables on a type.
func authVarKeysFromDef(typ *ast.Definition) map[string]bool {
	keys := make(map[string]bool)
	dir := typ.Directives.ForName(authVariablesDirective)
	if dir == nil {
		return keys
	}
	varsArg := dir.Arguments.ForName("vars")
	if varsArg == nil {
		return keys
	}
	for _, item := range varsArg.Value.Children {
		for _, kv := range item.Value.Children {
			if kv.Name == "key" {
				keys[kv.Value.Raw] = true
			}
		}
	}
	return keys
}

// collectUnresolvedKeys walks a rule arg value tree and appends an error for any
// {{KEY}} placeholder found in a rule string that is not covered by childVarKeys.
func collectUnresolvedKeys(val *ast.Value, childVarKeys map[string]bool,
	typName, fieldName, authorityTypeName string,
	dir *ast.Directive, errs *gqlerror.List) {

	if val == nil {
		return
	}
	// Check rule string leaves.
	if val.Raw != "" {
		raw := val.Raw
		for i := 0; i < len(raw)-3; i++ {
			if raw[i] == '{' && raw[i+1] == '{' {
				end := i + 2
				for end < len(raw)-1 && !(raw[end] == '}' && raw[end+1] == '}') {
					end++
				}
				if end < len(raw)-1 {
					key := raw[i+2 : end]
					if !childVarKeys[key] {
						*errs = append(*errs, gqlerror.ErrorPosf(dir.Position,
							"Type %s; Field %s: @cascadeAuth variableContext \"self\" — "+
								"authority type %s uses {{%s}} in its @auth rule but "+
								"Type %s does not declare key %q in @authVariables",
							typName, fieldName, authorityTypeName, key, typName, key))
					}
				}
			}
		}
	}
	// Recurse into children.
	for _, child := range val.Children {
		collectUnresolvedKeys(child.Value, childVarKeys, typName, fieldName,
			authorityTypeName, dir, errs)
	}
}

// typeHasCascadeAuthEdge returns true if the type definition has at least one
// field carrying @cascadeAuth. Used by the authority-validation check: if the
// authority type is itself cascade-protected (no own @auth but has @cascadeAuth
// fields), child auth will be derived transitively at expand time — not a no-op.
func typeHasCascadeAuthEdge(def *ast.Definition) bool {
	for _, f := range def.Fields {
		if f.Directives.ForName(cascadeAuthDirective) != nil {
			return true
		}
	}
	return false
}

// hasInverseForCascadeAuth checks (AST-only, no schema wrapper needed) that
// @hasInverse is declared on either side of the cascade edge:
//
//   - Forward direction: field itself carries @hasInverse(field: X)
//   - Backward direction: some field on the authority type carries
//     @hasInverse(field: cascadeFieldName) AND the field references
//     childTypeDef, an interface it implements, or (when childTypeDef is
//     an interface) a concrete type that implements it.
//
// Either placement is acceptable — @cascadeAuth will locate the predicate at
// expand time via findInversePredicate, which covers both cases.
func hasInverseForCascadeAuth(
	sch *ast.Schema,
	childTypeDef *ast.Definition,
	cascadeField *ast.FieldDefinition,
	authorityTypeName string,
) bool {
	// Direction 1: @hasInverse declared on the cascade field itself.
	if cascadeField.Directives.ForName("hasInverse") != nil {
		return true
	}

	// Direction 2: @hasInverse declared on any field of the authority type
	// that points back to childTypeDef (or a related type).
	authorityDef := sch.Types[authorityTypeName]
	if authorityDef == nil {
		return false
	}

	// If childTypeDef is an interface, build the set of concrete types that
	// implement it, so we can accept back-fields that point to any implementor.
	// Example: WorkspaceMember (interface) ← Workspace.hasGroups: [Group]
	//   where Group implements WorkspaceMember.
	childIsInterface := childTypeDef.Kind == ast.Interface
	implementorsOfChild := map[string]bool{childTypeDef.Name: true}
	if childIsInterface {
		for name, def := range sch.Types {
			for _, iface := range def.Interfaces {
				if iface == childTypeDef.Name {
					implementorsOfChild[name] = true
				}
			}
		}
	}

	// Collect the set of interface names that childTypeDef implements
	// (for the non-interface child case).
	childInterfaces := make(map[string]bool, len(childTypeDef.Interfaces))
	for _, iface := range childTypeDef.Interfaces {
		childInterfaces[iface] = true
	}

	for _, f := range authorityDef.Fields {
		hiDir := f.Directives.ForName("hasInverse")
		if hiDir == nil {
			continue
		}
		arg := hiDir.Arguments.ForName("field")
		if arg == nil || arg.Value.Raw != cascadeField.Name {
			continue
		}
		elemType := f.Type.Name()
		// Accept if the back-field references:
		//   (a) the child type itself
		//   (b) an interface that the child implements
		//   (c) a concrete type that implements the child (when child is interface)
		if elemType == childTypeDef.Name || childInterfaces[elemType] || implementorsOfChild[elemType] {
			return true
		}
	}

	return false
}
