/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/dgraph-io/gqlparser/v2/ast"
	"github.com/dgraph-io/gqlparser/v2/gqlerror"
	"github.com/dgraph-io/gqlparser/v2/validator"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var allowedFilters = []string{"StringHashFilter", "StringExactFilter", "StringFullTextFilter",
	"StringRegExpFilter", "StringTermFilter", "DateTimeFilter", "FloatFilter", "Int64Filter",
	"IntFilter", "PointGeoFilter", "ContainsFilter", "IntersectsFilter", "PolygonGeoFilter"}

func listInputCoercion(observers *validator.Events, addError validator.AddErrFunc) {
	observers.OnValue(func(walker *validator.Walker, value *ast.Value) {
		if value.Definition == nil || value.ExpectedType == nil {
			return
		}

		if value.Kind == ast.Variable {
			return
		}
		if value.ExpectedType.NamedType == IDType {
			value.Kind = ast.StringValue
		}
		// If the expected value is a list (ExpectedType.Elem != nil) && the value is not of list type,
		// then we need to coerce the value to a list, otherwise, we can return here as we do below.
		if !(value.ExpectedType.Elem != nil && value.Kind != ast.ListValue) {
			return
		}
		if value.ExpectedType.Elem.NamedType == IDType {
			value.Kind = ast.StringValue
		}
		val := *value
		child := &ast.ChildValue{Value: &val}
		valueNew := ast.Value{
			Children:   []*ast.ChildValue{child},
			Kind:       ast.ListValue,
			Position:   val.Position,
			Definition: val.Definition,
		}
		*value = valueNew
	})
}

func filterCheck(observers *validator.Events, addError validator.AddErrFunc) {
	observers.OnValue(func(walker *validator.Walker, value *ast.Value) {
		if value.Definition == nil {
			return
		}

		if x.HasString(allowedFilters, value.Definition.Name) && len(value.Children) > 1 {
			addError(
				validator.Message(
					"%s filter expects only one filter function, got: %d",
					value.Definition.Name,
					len(value.Children),
				),
				validator.At(value.Position),
			)
		}
	})
}

// memberTypesCheck rejects memberTypes when it appears inside and, or, or not
// clauses of any filter (at any nesting depth). memberTypes scopes the DQL
// func: type(...) predicate and has no meaning inside logical combinators.
// It is valid at:
//   - the top level of a root interface query filter
//   - the top level of a nested interface-typed field filter
//
// e.g. both of these are valid:
//
//	queryManageable(filter: { memberTypes: [Workspace] }) { ... }
//	aggregateNote(filter: { forResource: { memberTypes: [Company] } }) { ... }
func memberTypesCheck(observers *validator.Events, addError validator.AddErrFunc) {
	observers.OnField(func(walker *validator.Walker, field *ast.Field) {
		checkMemberTypesInFilter(walker, field.Arguments.ForName("filter"), false, addError)
	})
}

// checkMemberTypesInFilter recursively walks a filter value tree. insideLogical is true
// when we are inside an and/or/not clause. It reports an error if memberTypes appears
// at any depth other than the immediate top-level of a filter.
func checkMemberTypesInFilter(
	walker *validator.Walker,
	arg *ast.Argument,
	insideLogical bool,
	addError validator.AddErrFunc,
) {
	if arg == nil || arg.Value == nil {
		return
	}
	walkFilterValue(arg.Value, insideLogical, addError)
}

func walkFilterValue(val *ast.Value, insideLogical bool, addError validator.AddErrFunc) {
	if val == nil || val.Kind != ast.ObjectValue {
		return
	}
	for _, child := range val.Children {
		switch child.Name {
		case "and", "or":
			// list or single object — recurse with insideLogical = true
			if child.Value != nil {
				if child.Value.Kind == ast.ListValue {
					for _, item := range child.Value.Children {
						walkFilterValue(item.Value, true, addError)
					}
				} else {
					walkFilterValue(child.Value, true, addError)
				}
			}
		case "not":
			if child.Value != nil {
				walkFilterValue(child.Value, true, addError)
			}
		case "memberTypes":
			if insideLogical {
				addError(
					validator.Message(
						"memberTypes is only supported at the top level of an interface filter, "+
							"not inside and, or, or not clauses. "+
							"Use a top-level memberTypes to scope the query to specific implementing types.",
					),
					validator.At(val.Position),
				)
			}
		}
	}
}

func variableTypeCheck(observers *validator.Events, addError validator.AddErrFunc) {
	observers.OnValue(func(walker *validator.Walker, value *ast.Value) {
		if value.Definition == nil || value.ExpectedType == nil ||
			value.VariableDefinition == nil {
			return
		}

		if value.Kind != ast.Variable {
			return
		}
		if value.VariableDefinition.Type.IsCompatible(value.ExpectedType) {
			return
		}

		addError(validator.Message("Variable type provided %s is incompatible with expected type %s",
			value.VariableDefinition.Type.String(),
			value.ExpectedType.String()), validator.At(value.Position))
	})
}

func directiveArgumentsCheck(observers *validator.Events, addError validator.AddErrFunc) {
	observers.OnDirective(func(walker *validator.Walker, directive *ast.Directive) {
		if directive.Name == cascadeDirective && len(directive.Arguments) == 1 {
			if directive.ParentDefinition == nil {
				addError(validator.Message("Schema is not set yet. Please try after sometime."))
				return
			}
			fieldArg := directive.Arguments.ForName(cascadeArg)
			if fieldArg == nil {
				return
			}
			isVariable := fieldArg.Value.Kind == ast.Variable
			fieldsVal, _ := directive.ArgumentMap(walker.Variables)[cascadeArg].([]interface{})
			if len(fieldsVal) == 0 {
				return
			}
			var validatorPath ast.Path
			if isVariable {
				validatorPath = ast.Path{ast.PathName("variables")}
				validatorPath = append(validatorPath, ast.PathName(fieldArg.Value.Raw))

			}

			typFields := directive.ParentDefinition.Fields
			typName := directive.ParentDefinition.Name

			for _, value := range fieldsVal {
				v, ok := value.(string)
				if !ok {
					continue
				}
				if typFields.ForName(v) == nil {
					err := fmt.Sprintf("Field `%s` is not present in type `%s`."+
						" You can only use fields in cascade which are in type `%s`", value, typName, typName)
					if isVariable {
						validatorPath = append(validatorPath, ast.PathName(v))
						err = gqlerror.ErrorPathf(validatorPath, "%v", err).Error()
					}
					addError(validator.Message("%v", err), validator.At(directive.Position))
					return
				}

			}

		}

	})
}

func intRangeCheck(observers *validator.Events, addError validator.AddErrFunc) {
	observers.OnValue(func(walker *validator.Walker, value *ast.Value) {
		if value.Definition == nil || value.ExpectedType == nil || value.Kind == ast.Variable ||
			value.Kind == ast.ListValue {
			return
		}

		switch value.Definition.Name {
		case "Int":
			if value.Kind == ast.NullValue {
				return
			}
			if value.Kind == ast.FloatValue {
				value.Raw = strings.Split(value.Raw, ".")[0]
			}
			_, err := strconv.ParseInt(value.Raw, 10, 32)
			if err != nil {
				if errors.Is(err, strconv.ErrRange) {
					addError(validator.Message("Out of range value '%s', for type `%s`",
						value.Raw, value.Definition.Name), validator.At(value.Position))
				} else {
					addError(validator.Message("%s", err), validator.At(value.Position))
				}
			}
		case "Int64":
			if value.Kind == ast.IntValue || value.Kind == ast.StringValue || value.Kind == ast.FloatValue {
				if value.Kind == ast.FloatValue {
					value.Raw = strings.Split(value.Raw, ".")[0]
				}
				_, err := strconv.ParseInt(value.Raw, 10, 64)
				if err != nil {
					if errors.Is(err, strconv.ErrRange) {
						addError(validator.Message("Out of range value '%s', for type `%s`",
							value.Raw, value.Definition.Name), validator.At(value.Position))
					} else {
						addError(validator.Message("%s", err), validator.At(value.Position))
					}
				}
				value.Kind = ast.StringValue
			} else {
				addError(validator.Message("Type mismatched for Value `%s`, expected: Int64, got: '%s'", value.Raw,
					valueKindToString(value.Kind)), validator.At(value.Position))
			}
		case "UInt64":
			// UInt64 exists only in admin schema
			if value.Kind == ast.IntValue || value.Kind == ast.StringValue {
				_, err := strconv.ParseUint(value.Raw, 10, 64)
				if err != nil {
					addError(validator.Message("%v", err.Error()), validator.At(value.Position))
				}
				// UInt64 values parsed from query text would be propagated as strings internally
				value.Kind = ast.StringValue
			} else {
				addError(validator.Message("Type mismatched for Value `%s`, expected: UInt64, "+
					"got: '%s'", value.Raw,
					valueKindToString(value.Kind)), validator.At(value.Position))
			}
		}
	})
}

func valueKindToString(valKind ast.ValueKind) string {
	switch valKind {
	case ast.Variable:
		return "Variable"
	case ast.StringValue:
		return "String"
	case ast.IntValue:
		return "Int"
	case ast.FloatValue:
		return "Float"
	case ast.BlockValue:
		return "Block"
	case ast.BooleanValue:
		return "Boolean"
	case ast.NullValue:
		return "Null"
	case ast.EnumValue:
		return "Enum"
	case ast.ListValue:
		return "List"
	case ast.ObjectValue:
		return "Object"
	}
	return ""
}

// walkGroupByFieldValue walks the ast.Value of an XxxGroupByField input object and
// returns the (path, leaf ast.FieldDefinition, ok). At each level it expects exactly
// one key set. The path is the list of field names from root to leaf.
// Returns ok=false if the structure is invalid (empty, non-object intermediate, etc.).
func walkGroupByAstValue(
	val *ast.Value,
	typeDef *ast.Definition,
	schemaTypes map[string]*ast.Definition,
	path []string,
) (leafPath []string, leafFieldDef *ast.FieldDefinition, ok bool) {
	if val == nil || val.Kind != ast.ObjectValue {
		return nil, nil, false
	}
	for _, child := range val.Children {
		if child.Value == nil {
			continue
		}
		fieldName := child.Name
		fldDef := typeDef.Fields.ForName(fieldName)
		if fldDef == nil {
			return nil, nil, false
		}
		newPath := append(path, fieldName)

		switch child.Value.Kind {
		case ast.BooleanValue:
			// Terminal leaf selector.
			return newPath, fldDef, true
		case ast.ObjectValue:
			// Navigate into the next type.
			if fldDef.Type.NamedType == "" {
				return nil, nil, false // list edge — not allowed
			}
			nextType := schemaTypes[fldDef.Type.NamedType]
			if nextType == nil || nextType.Kind != ast.Object {
				return nil, nil, false
			}
			return walkGroupByAstValue(child.Value, nextType, schemaTypes, newPath)
		}
	}
	return nil, nil, false
}

// groupByArgumentsCheck validates the arguments of groupByXxx queries at request-parse
// time, before any DQL is generated. It enforces:
//
//  1. The `field` object must resolve to a single leaf via single-valued object edges;
//     list edges are not allowed as intermediate steps.
//
//  2. The `by` (DateTimeGranularity) argument is only valid when the resolved leaf field
//     is of type DateTime.
//
//  3. When `by` is provided the leaf DateTime field must have at least one DateTime
//     @search index (year | month | day | hour).
//
//  4. The `tz` argument is only meaningful when `by` is also set.
func groupByArgumentsCheck(observers *validator.Events, addError validator.AddErrFunc) {
	observers.OnField(func(walker *validator.Walker, field *ast.Field) {
		// Only intercept groupByXxx query fields.
		const prefix = "groupBy"
		if !strings.HasPrefix(field.Name, prefix) || len(field.Name) <= len(prefix) {
			return
		}

		groupByArg := field.Arguments.ForName("groupBy")
		if groupByArg == nil || groupByArg.Value == nil {
			return
		}
		gv := groupByArg.Value

		// Skip variable arguments — they are validated at execution time.
		if gv.Kind == ast.Variable || gv.Kind != ast.ListValue {
			return
		}

		// Derive the concrete type name: "groupByNote" → "Note".
		typeName := field.Name[len(prefix):]
		typeDef := walker.Schema.Types[typeName]
		if typeDef == nil {
			return // unknown type — existing schema validation handles this
		}

		for _, specChild := range gv.Children {
			spec := specChild.Value
			if spec == nil || spec.Kind != ast.ObjectValue {
				continue
			}

			// Extract field value, by, tz from the spec input object.
			var fieldVal *ast.Value
			var by, tz string
			var byPos, tzPos *ast.Position
			for _, c := range spec.Children {
				if c.Value == nil || c.Value.Kind == ast.NullValue || c.Value.Kind == ast.Variable {
					continue
				}
				switch c.Name {
				case "field":
					fieldVal = c.Value
				case "by":
					by = c.Value.Raw
					byPos = c.Value.Position
				case "tz":
					tz = c.Value.Raw
					tzPos = c.Value.Position
				}
			}

			if fieldVal == nil {
				continue
			}

			// Constraint 4: tz requires by.
			if tz != "" && by == "" {
				addError(
					validator.Message("groupBy: `tz` is only valid when `by` is also specified — "+
						"did you forget to add `by: day` (or similar)?"),
					validator.At(tzPos),
				)
			}

			// Walk the field object to find the leaf field definition.
			_, leafFieldDef, ok := walkGroupByAstValue(fieldVal, typeDef, walker.Schema.Types, nil)
			if !ok {
				// Object structure invalid — the GraphQL type system will catch unknown fields;
				// we just skip further checks for this spec.
				continue
			}

			if by == "" {
				continue // no further DateTime constraints to check
			}

			// Constraint 2: by is only valid for DateTime leaf fields.
			fldType := leafFieldDef.Type.NamedType
			if fldType != "DateTime" {
				addError(
					validator.Message("groupBy: `by` is only valid for DateTime fields; "+
						"the resolved leaf field has type %s. Remove `by` or choose a DateTime field.",
						fldType),
					validator.At(byPos),
				)
				continue
			}

			// Constraint 3: leaf DateTime field must have at least one DateTime @search index.
			dir := leafFieldDef.Directives.ForName(searchDirective)
			if dir == nil {
				addError(
					validator.Message("groupBy: `by: %s` on DateTime field %q requires "+
						"@search(by: [%s]) to be declared on the field in the schema. "+
						"Without this index the query will perform a full scan.",
						by, leafFieldDef.Name, by),
					validator.At(byPos),
				)
				continue
			}
			byArgVal := dir.Arguments.ForName(searchArgs)
			if byArgVal == nil {
				continue // @search without explicit by → default index applies; skip check
			}
			var searchTokenizers []string
			if byArgVal.Value.Kind == ast.ListValue {
				for _, item := range byArgVal.Value.Children {
					searchTokenizers = append(searchTokenizers, item.Value.Raw)
				}
			} else {
				searchTokenizers = []string{byArgVal.Value.Raw}
			}
			// The specific granularity requested (by) does not need to match any indexed
			// tokenizer exactly. Dgraph resolves @groupby(Pred@day) with any DateTime index,
			// and floorToInterval in query/groupby.go applies the correct granularity.
			dateTimeTokenizers := map[string]bool{
				"year": true, "month": true, "day": true, "hour": true,
			}
			hasDateTimeIndex := false
			for _, tok := range searchTokenizers {
				if dateTimeTokenizers[tok] {
					hasDateTimeIndex = true
					break
				}
			}
			if !hasDateTimeIndex {
				addError(
					validator.Message("groupBy: `by: %s` on DateTime field %q requires "+
						"at least one DateTime index (@search(by: [year|month|day|hour])) but "+
						"the field only has @search(by: %v). "+
						"Add a DateTime tokenizer to the field's @search directive.",
						by, leafFieldDef.Name, searchTokenizers),
					validator.At(byPos),
				)
			}
		}
	})
}
