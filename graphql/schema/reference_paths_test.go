/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"testing"

	"github.com/dgraph-io/gqlparser/v2/ast"
	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/x"
)

// An @oldValue path may walk through an interface into a field that the
// interface does not declare and its implementors do, as long as every
// implementor that has the field stores it under the same predicate.
const oldValueImplementorSchema = `
type Space {
	id: ID!
	name: String! @search(by: [hash])
}

interface Member {
	inSpace: Space
}

interface Holder {
	id: ID!
}

type Person implements Holder & Member {
	nick: String
	%s
}

type Firm implements Holder & Member {
	title: String
	%s
}

type Bot implements Holder {
	model: String
}

type File {
	id: ID!
	heldBy: [Holder!]! @oldValue(fields: [%s])
}
`

func loadOldValueSchema(personField, firmField, paths string) (Schema, error) {
	// fmt is avoided so the schema text above stays readable in failures.
	input := oldValueImplementorSchema
	for _, v := range []string{personField, firmField, paths} {
		i := indexOf(input, "%s")
		input = input[:i] + v + input[i+2:]
	}
	h, err := NewHandler(input, false)
	if err != nil {
		return nil, err
	}
	return FromString(h.GQLSchema(), x.RootNamespace)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestOldValuePathThroughImplementorField(t *testing.T) {
	t.Run("a field the implementors share through another interface is accepted", func(t *testing.T) {
		sch, err := loadOldValueSchema("", "", `"inSpace.name"`)
		require.NoError(t, err)
		in, ok := sch.(*schema)
		require.True(t, ok)
		file := &astType{inSchema: in, dgraphPredicate: in.dgraphPredicate, typ: &ast.Type{NamedType: "File"}}
		sel := file.GetOldValueFieldsForQuery()["heldBy"]
		require.NotNil(t, sel)
		space := sel.SubFields["inSpace"]
		require.NotNil(t, space, "the path through the implementors' field is selected")
		require.Equal(t, "Member.inSpace", space.DgraphPredicate)
		require.NotNil(t, space.SubFields["name"])
		require.Equal(t, "Space.name", space.SubFields["name"].DgraphPredicate)
	})

	t.Run("a field each implementor stores under its own predicate is refused", func(t *testing.T) {
		_, err := loadOldValueSchema("label: String", "label: String", `"label"`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "stored differently")
	})

	t.Run("a field only one implementor has is accepted", func(t *testing.T) {
		_, err := loadOldValueSchema("", "", `"model"`)
		require.NoError(t, err)
	})

	t.Run("a field no implementor has is still unknown", func(t *testing.T) {
		_, err := loadOldValueSchema("", "", `"nowhere"`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown field")
	})
}

func TestReferenceScopeDirectiveValidation(t *testing.T) {
	load := func(directive string) error {
		h, err := NewHandler(`
type Space {
	id: ID!
	name: String!
	parent: Space
}

type Team `+directive+` {
	id: ID!
	title: String
	inSpace: Space
}`, false)
		if err != nil {
			return err
		}
		_, err = FromString(h.GQLSchema(), x.RootNamespace)
		return err
	}
	require.NoError(t, load(`@referenceScope(claim: "ws", path: "inSpace.name")`))
	require.NoError(t, load(`@referenceScope(claim: "ws", path: "inSpace.parent.name")`))
	require.NoError(t, load(`@referenceScope(claim: "ws", path: "title")`))
	for path, want := range map[string]string{
		"inSpace":         "must end on a scalar or enum field",
		"inSpace.nowhere": "unknown field",
		"title.name":      "cannot walk through",
		"":                "needs a claim and a path",
	} {
		err := load(`@referenceScope(claim: "ws", path: "` + path + `")`)
		require.Error(t, err, path)
		require.Contains(t, err.Error(), want, path)
	}
	err := load(`@referenceScope(claim: "", path: "title")`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "needs a claim and a path")
}
