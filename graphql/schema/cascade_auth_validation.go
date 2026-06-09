/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/dgraph-io/gqlparser/v2/ast"
	"github.com/dgraph-io/gqlparser/v2/gqlerror"
	"github.com/hypermodeinc/dgraph/v25/x"
)

// cascadeAuthDirectiveValidation validates the @cascadeAuth directive.
// Rules:
//   - May only appear on edge (non-scalar, non-enum) fields
//   - Not allowed on @remote types, @custom or @lambda fields
//   - depth must be ≥ 1 or -1 if supplied
//   - variableContext must be "self", "parent", or "adaptive" if supplied
//   - self:      host type must have @authVariables
//   - parent:    immediate authority must be resolvable: either declare its own
//     @authVariables, OR be protected by its own @cascadeAuth chain
//     (in which case the expand phase walks that chain for vars)
//   - adaptive:  child type, a concrete implementor of child (if interface),
//     OR at least one type reachable from the authority must have @authVariables
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

		switch vc {
		case "self":
			if typ.Kind == ast.Interface {
				break // Interfaces carry @cascadeAuth but not @authVariables.
			}
			// Check: the host type must have @authVariables.
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

		case "parent":
			// parent: the child uses the authority's compiled rule as-is.
			// No @authVariables check — the authority compiles its own rule during
			// its own cascade processing (when it is itself a cascade child).
			// The child never provides or checks vars for parent.
			//
			// The only case to reject: authority has a {{KEY}} template AND no cascade
			// chain. Without cascade, the template can never be compiled, leaving the
			// child with zero protection.
			authDef := sch.Types[fieldTypeName]
			if authDef == nil {
				break
			}
			// Authority is cascade-protected → its compilation is handled during its
			// own cascade processing; the child inherits the result.
			if typeHasCascadeAuthEdge(authDef) {
				break
			}
			// Interface authority: check each concrete implementor.
			// Only error if an implementor has a {{KEY}} template AND no cascade edge.
			if authDef.Kind == ast.Interface {
				for _, candDef := range sch.Types {
					if candDef.Kind != ast.Object {
						continue
					}
					implementsThis := false
					for _, iface := range candDef.Interfaces {
						if iface == fieldTypeName {
							implementsThis = true
							break
						}
					}
					if !implementsThis {
						continue
					}
					authDir := candDef.Directives.ForName(authDirective)
					if authDir == nil || authDir.Arguments.ForName("query") == nil {
						continue
					}
					if typeHasCascadeAuthEdge(candDef) {
						continue // cascade-protected — its compilation will succeed
					}
					if keys := collectAuthVarKeysFromTemplate(candDef); len(keys) > 0 {
						return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
							"Type %s; Field %s: @cascadeAuth variableContext \"parent\" — "+
								"interface %s implementor %s has @auth rule with keys [%s] "+
								"but no @cascadeAuth edge to compile them. "+
								"Add a @cascadeAuth field to %s, or use variableContext: \"adaptive\"",
							typ.Name, field.Name, fieldTypeName, candDef.Name,
							strings.Join(keys, ", "), candDef.Name)}
					}
				}
				break
			}
			// Concrete authority with no cascade: error if {{KEY}} present (uncompilable).
			if keys := collectAuthVarKeysFromTemplate(authDef); len(keys) > 0 {
				return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
					"Type %s; Field %s: @cascadeAuth variableContext \"parent\" — "+
						"%s has @auth rule with keys [%s] but no @cascadeAuth edge to compile them. "+
						"Add a @cascadeAuth field to %s so its rule is compiled via cascade, "+
						"or use variableContext: \"adaptive\"",
					typ.Name, field.Name, fieldTypeName, strings.Join(keys, ", "), fieldTypeName)}
			}

		case "adaptive":
			// adaptive: try self first (child has @authVariables → re-substitute).
			// If self fails, fall back to parent's compiled rule.
			//
			// Only error when BOTH paths are blocked:
			//   self  blocked: child has no @authVariables
			//   parent blocked: authority has {{KEY}} template with no cascade chain
			//                   (template uncompilable, zero protection)

			// -- Self path: check if child (or its implementors) has @authVariables. --
			selfOK := typ.Directives.ForName(authVariablesDirective) != nil
			if !selfOK && typ.Kind == ast.Interface {
				for _, candDef := range sch.Types {
					if candDef.Kind != ast.Object {
						continue
					}
					for _, iface := range candDef.Interfaces {
						if iface == typ.Name && candDef.Directives.ForName(authVariablesDirective) != nil {
							selfOK = true
							break
						}
					}
					if selfOK {
						break
					}
				}
			}
			if selfOK {
				break // self path works — no further check needed
			}

			// -- Parent fallback path: same check as the "parent" validation case. --
			// If authority is cascade-protected or has a self-contained rule, the
			// parent's compiled rule is usable. Only error if authority has {{KEY}}
			// template AND no cascade chain.
			authDef := sch.Types[fieldTypeName]
			if authDef == nil {
				break
			}
			if typeHasCascadeAuthEdge(authDef) {
				break // cascade-protected authority — parent fallback is usable
			}
			if authDef.Kind == ast.Interface {
				for _, candDef := range sch.Types {
					if candDef.Kind != ast.Object {
						continue
					}
					implementsThis := false
					for _, iface := range candDef.Interfaces {
						if iface == fieldTypeName {
							implementsThis = true
							break
						}
					}
					if !implementsThis {
						continue
					}
					authDir := candDef.Directives.ForName(authDirective)
					if authDir == nil || authDir.Arguments.ForName("query") == nil {
						continue
					}
					if typeHasCascadeAuthEdge(candDef) {
						continue // cascade-protected implementor — fine
					}
					if keys := collectAuthVarKeysFromTemplate(candDef); len(keys) > 0 {
						return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
							"Type %s; Field %s: @cascadeAuth variableContext \"adaptive\" — "+
								"child has no @authVariables and interface %s implementor %s "+
								"has @auth rule with keys [%s] but no @cascadeAuth edge to compile them. "+
								"Add @authVariables to %s, add a @cascadeAuth field to %s, "+
								"or add @authVariables to %s",
							typ.Name, field.Name, fieldTypeName, candDef.Name,
							strings.Join(keys, ", "), typ.Name, candDef.Name, candDef.Name)}
					}
				}
				break
			}
			// Concrete authority with no cascade: both paths blocked if {{KEY}} present.
			if keys := collectAuthVarKeysFromTemplate(authDef); len(keys) > 0 {
				return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
					"Type %s; Field %s: @cascadeAuth variableContext \"adaptive\" — "+
						"child has no @authVariables and %s has @auth rule with keys [%s] "+
						"but no @cascadeAuth edge to compile them. "+
						"Add @authVariables to %s or add a @cascadeAuth field to %s",
					typ.Name, field.Name, fieldTypeName, strings.Join(keys, ", "),
					typ.Name, fieldTypeName)}
			}
		} // end switch vc

	} // end if vcArg

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
			if raw[i] == '<' && raw[i+1] == '<' {
				end := i + 2
				for end < len(raw)-1 && !(raw[end] == '>' && raw[end+1] == '>') {
					end++
				}
				if end < len(raw)-1 {
					key := raw[i+2 : end]
					if !childVarKeys[key] {
						*errs = append(*errs, gqlerror.ErrorPosf(dir.Position,
							"Type %s; Field %s: @cascadeAuth variableContext \"self\" — "+
								"authority type %s uses <<%s>> in its @auth rule but "+
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

// chainHasAuthVariables DFS-walks the cascade chain starting from typeName
// and returns true if the chain is fully resolvable — meaning every type in
// the chain whose @auth rule contains {{KEY}} placeholders has @authVariables
// to supply them. Types whose rules contain no {{KEY}} are self-contained and
// do not need @authVariables.
func chainHasAuthVariables(sch *ast.Schema, typeName string, visited map[string]bool) bool {
	if visited[typeName] {
		return false
	}
	visited[typeName] = true
	def := sch.Types[typeName]
	if def == nil {
		return false
	}
	if def.Directives.ForName(authVariablesDirective) != nil {
		return true // has explicit vars — fully covered
	}
	// Check whether this type's @auth rule actually has {{KEY}} placeholders.
	// If not, the rule is self-contained and no vars are needed.
	if len(collectAuthVarKeysFromTemplate(def)) == 0 {
		return true // self-contained rule, no substitution needed
	}
	// Has {{KEY}} but no @authVariables yet. Check if an interface implementor
	// or a higher cascade hop can provide vars.
	if def.Kind == ast.Interface {
		// Collect the interface's own {{KEY}} requirements.
		ifaceKeys := collectAuthVarKeysFromTemplate(def)
		hasParticipants := false
		for _, candDef := range sch.Types {
			if candDef.Kind != ast.Object {
				continue
			}
			implementsThis := false
			for _, iface := range candDef.Interfaces {
				if iface == typeName {
					implementsThis = true
					break
				}
			}
			if !implementsThis {
				continue
			}
			authDir := candDef.Directives.ForName(authDirective)
			implHasOwnAuth := authDir != nil && authDir.Arguments.ForName("query") != nil
			// Implementor participates if the interface has its own auth rule OR
			// the implementor has its own auth rule.
			if !implHasOwnAuth && len(ifaceKeys) == 0 {
				continue
			}
			hasParticipants = true
			// Determine if this implementor needs vars.
			var implKeys []string
			if implHasOwnAuth {
				implKeys = collectAuthVarKeysFromTemplate(candDef)
			}
			needsVars := len(ifaceKeys) > 0 || len(implKeys) > 0
			if needsVars && candDef.Directives.ForName(authVariablesDirective) == nil {
				return false // unresolvable: needs vars but has none
			}
		}
		return hasParticipants
	}
	// Walk to any authority type reachable via @cascadeAuth fields.
	for _, f := range def.Fields {
		if f.Directives.ForName(cascadeAuthDirective) == nil {
			continue
		}
		if chainHasAuthVariables(sch, f.Type.Name(), visited) {
			return true
		}
	}
	return false
}

// collectCascadeChain DFS-walks the cascade chain starting from typeName and
// returns an ordered list of type names reachable via @cascadeAuth edges.
// For interface types, only concrete implementors that have @auth(query:...)
// rules are listed — those are the ones that participate in cascade expansion
// and must supply @authVariables.
func collectCascadeChain(sch *ast.Schema, typeName string, visited map[string]bool) []string {
	if visited[typeName] {
		return nil
	}
	visited[typeName] = true
	def := sch.Types[typeName]
	if def == nil {
		return []string{typeName}
	}
	// For interfaces, show only the auth'd concrete implementors — those are
	// the types that will actually produce cascade rules and need @authVariables.
	label := typeName
	if def.Kind == ast.Interface {
		var authdImpls []string
		for name, d := range sch.Types {
			if d.Kind != ast.Object {
				continue
			}
			for _, iface := range d.Interfaces {
				if iface != typeName {
					continue
				}
				authDir := d.Directives.ForName(authDirective)
				if authDir != nil && authDir.Arguments.ForName("query") != nil {
					authdImpls = append(authdImpls, name)
				}
				break
			}
		}
		if len(authdImpls) > 0 {
			sort.Strings(authdImpls)
			label = fmt.Sprintf("%s (auth'd implementors: %s)", typeName, strings.Join(authdImpls, ", "))
		}
	}
	result := []string{label}
	for _, f := range def.Fields {
		if f.Directives.ForName(cascadeAuthDirective) == nil {
			continue
		}
		result = append(result, collectCascadeChain(sch, f.Type.Name(), visited)...)
	}
	return result
}

// collectAuthVarKeysFromTemplate extracts {{KEY}} placeholder names from all
// @auth rule strings on the given type definition. Returns deduplicated keys
// in sorted order for stable error messages.
func collectAuthVarKeysFromTemplate(def *ast.Definition) []string {
	seen := map[string]bool{}
	for _, d := range def.Directives {
		if d.Name != authDirective {
			continue
		}
		for _, arg := range d.Arguments {
			extractTemplateKeys(arg.Value, seen)
		}
	}
	var keys []string
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// extractTemplateKeys recursively walks an ast.Value tree and collects any
// {{KEY}} placeholders found in raw string leaves.
func extractTemplateKeys(val *ast.Value, out map[string]bool) {
	if val == nil {
		return
	}
	if val.Raw != "" {
		raw := val.Raw
		for i := 0; i < len(raw)-3; i++ {
			if raw[i] == '<' && raw[i+1] == '<' {
				end := i + 2
				for end < len(raw)-1 && !(raw[end] == '>' && raw[end+1] == '>') {
					end++
				}
				if end < len(raw)-1 {
					out[raw[i+2:end]] = true
				}
			}
		}
	}
	for _, child := range val.Children {
		extractTemplateKeys(child.Value, out)
	}
}

// buildAdaptiveMissingVarsError produces a detailed validation error for the
// adaptive case when chainHasAuthVariables returned false. It walks the
// authority type (which may be an interface) and reports:
//   - which auth'd implementors have @authVariables (covered)
//   - which auth'd implementors are missing @authVariables (need fixing)
//
// For concrete authorities it falls back to a plain chain-display error.
func buildAdaptiveMissingVarsError(
	sch *ast.Schema,
	dir *ast.Directive,
	childName, fieldName, authorityName string,
) gqlerror.List {
	authDef := sch.Types[authorityName]
	if authDef != nil && authDef.Kind == ast.Interface {
		present, missing := interfaceAuthVarStatus(sch, authorityName)
		var msg string
		switch {
		case len(missing) == 0:
			// All covered — chainHasAuthVariables should have returned true;
			// this shouldn't happen, but handle gracefully.
			return nil
		case len(present) == 0:
			neededKeys := collectAuthVarKeysFromTemplate(sch.Types[authorityName])
			keysLine := ""
			if len(neededKeys) > 0 {
				keysLine = fmt.Sprintf("\n  Rule requires keys: [%s]", strings.Join(neededKeys, ", "))
			}
			msg = fmt.Sprintf(
				"Type %s; Field %s: @cascadeAuth variableContext \"adaptive\" — "+
					"%s is an interface and none of its implementors declare "+
					"@authVariables.%s\n"+
					"  Missing @authVariables: [%s]\n"+
					"  Fix: add @authVariables to each listed type, or add @authVariables "+
					"to %s so it covers all implementors",
				childName, fieldName, authorityName, keysLine,
				strings.Join(missing, ", "),
				childName)
		default:
			neededKeys := collectAuthVarKeysFromTemplate(sch.Types[authorityName])
			keysLine := ""
			if len(neededKeys) > 0 {
				keysLine = fmt.Sprintf("\n  Rule requires keys: [%s]", strings.Join(neededKeys, ", "))
			}
			msg = fmt.Sprintf(
				"Type %s; Field %s: @cascadeAuth variableContext \"adaptive\" — "+
					"%s is an interface but not all implementors declare "+
					"@authVariables (partial coverage leaves some instances unprotected).%s\n"+
					"  Have @authVariables: [%s]\n"+
					"  Missing @authVariables: [%s]\n"+
					"  Fix: add @authVariables to the missing types, or add @authVariables "+
					"to %s so it covers all implementors",
				childName, fieldName, authorityName, keysLine,
				strings.Join(present, ", "),
				strings.Join(missing, ", "),
				childName)
		}
		return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position, "%s", msg)}
	}
	// Concrete authority (or unknown) — fall back to chain display.
	chain := collectCascadeChain(sch, authorityName, map[string]bool{})
	return []*gqlerror.Error{gqlerror.ErrorPosf(dir.Position,
		"Type %s; Field %s: @cascadeAuth variableContext \"adaptive\" requires "+
			"at least one type in the cascade chain to declare @authVariables, "+
			"but none were found.\n"+
			"  Chain searched: %s → [%s]\n"+
			"  Fix: add @authVariables to %s, to %s, or to any type in the chain",
		childName, fieldName,
		childName, strings.Join(chain, " → "),
		childName, authorityName)}
}

// interfaceAuthVarStatus checks each relevant concrete implementor of the given
// interface and returns two sorted slices:
//   - present: implementors that are "covered" — either have @authVariables, or
//     their auth rule has no {{KEY}} placeholders (self-contained).
//   - missing: implementors whose auth rule has {{KEY}} placeholders that require
//     substitution, but have no @authVariables to supply them.
//
// An implementor only participates (is checked at all) when:
//   - the interface has its own @auth rule with {{KEY}} placeholders, OR
//   - the implementor has its own @auth rule.
//
// Self-contained implementors (auth rule exists but has no {{KEY}}) are counted
// as "present" without needing @authVariables.
func interfaceAuthVarStatus(sch *ast.Schema, ifaceName string) (present, missing []string) {
	ifaceDef := sch.Types[ifaceName]

	// Collect {{KEY}} requirements from the interface's own @auth rule (if any).
	var ifaceKeys []string
	ifaceHasOwnAuth := false
	if ifaceDef != nil {
		authDir := ifaceDef.Directives.ForName(authDirective)
		if authDir != nil && authDir.Arguments.ForName("query") != nil {
			ifaceHasOwnAuth = true
			ifaceKeys = collectAuthVarKeysFromTemplate(ifaceDef)
		}
	}

	for name, def := range sch.Types {
		if def.Kind != ast.Object {
			continue
		}
		implementsIface := false
		for _, iface := range def.Interfaces {
			if iface == ifaceName {
				implementsIface = true
				break
			}
		}
		if !implementsIface {
			continue
		}
		authDir := def.Directives.ForName(authDirective)
		implHasOwnAuth := authDir != nil && authDir.Arguments.ForName("query") != nil

		// Implementor participates only if the interface has own auth OR the
		// implementor has its own auth.
		if !ifaceHasOwnAuth && !implHasOwnAuth {
			continue
		}

		// Short-circuit: if it has @authVariables it is always covered.
		if def.Directives.ForName(authVariablesDirective) != nil {
			present = append(present, name)
			continue
		}

		// No @authVariables. Check whether vars are actually needed:
		// needed if the interface's rule OR the implementor's own rule has {{KEY}}.
		var implKeys []string
		if implHasOwnAuth {
			implKeys = collectAuthVarKeysFromTemplate(def)
		}
		needsVars := len(ifaceKeys) > 0 || len(implKeys) > 0

		if needsVars {
			missing = append(missing, name)
		} else {
			// Self-contained rule (no {{KEY}}) — no vars needed.
			present = append(present, name)
		}
	}
	sort.Strings(present)
	sort.Strings(missing)
	return
}
