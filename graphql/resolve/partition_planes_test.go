/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgraph/v25/dql"
	"github.com/dgraph-io/dgraph/v25/graphql/dgraph"
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
	"github.com/dgraph-io/dgraph/v25/graphql/test"
)

const planeSchema = `
type Workspace @tenant(claim: "ws", name: "name") { id: ID! name: String! @id }
type Vendor @scope(plane: control) { id: ID! name: String }
type App @scope(plane: control) { clientId: String! @id title: String madeBy: Vendor }
type Plugin { id: ID! label: String! hasApp: App notes: [Note] }
type Note { id: ID! text: String }
`

func rewritten(t *testing.T, query string) string {
	sch := test.LoadSchemaFromString(t, planeSchema)
	op, err := sch.Operation(&schema.Request{Query: query})
	require.NoError(t, err)
	out, _, err := NewQueryRewriter().Rewrite(context.Background(), op.Queries()[0])
	require.NoError(t, err)
	return strings.Join(strings.Fields(dgraph.AsString(out)), " ")
}

func TestAnEdgeIntoTheControlPlaneIsReadInTheTemplateNamespace(t *testing.T) {
	q := rewritten(t, `{ queryPlugin { label notes { text } hasApp(filter: {clientId: {eq: "web"}}) { title madeBy { name } } } }`)
	require.Contains(t, q, `Plugin.hasApp : Plugin.hasApp @filter(eq(App.clientId, "web")) @template {`,
		"the edge from a tenant's record to a control record is marked, with its filter under it")
	require.Equal(t, 1, strings.Count(q, "@template"),
		"once: what is under a marked block is read there too, and an edge between tenant types is not marked")

	// a control type asked for at the root runs in the root namespace as a whole: nothing to mark
	require.NotContains(t, rewritten(t, `{ queryApp { title madeBy { name } } }`), "@template")

	// the text the engine is sent says the same when it is read back
	res, err := dql.Parse(dql.Request{Str: q})
	require.NoError(t, err)
	var marked []string
	var walk func(gq *dql.GraphQuery)
	walk = func(gq *dql.GraphQuery) {
		if gq.Template {
			marked = append(marked, gq.Attr)
		}
		for _, c := range gq.Children {
			walk(c)
		}
	}
	for _, gq := range res.Query {
		walk(gq)
	}
	require.Equal(t, []string{"Plugin.hasApp"}, marked)
}
