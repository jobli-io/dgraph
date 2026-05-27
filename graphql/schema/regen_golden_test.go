/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

// Package schema golden-file regeneration helper.
// Run with: go test -run TestRegenGolden -regenGolden ./graphql/schema/...
package schema

import (
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

var regenGolden = flag.Bool("regenGolden", false, "Regenerate golden output files for TestSchemaString and TestApolloServiceQueryResult")

// TestRegenGolden regenerates the golden output files for TestSchemaString and
// TestApolloServiceQueryResult when run with -regenGolden.
// Usage: go test -run TestRegenGolden -regenGolden ./graphql/schema/...
func TestRegenGolden(t *testing.T) {
	if !*regenGolden {
		t.Skip("pass -regenGolden to regenerate golden files")
	}

	// Regenerate testdata/schemagen/output/
	t.Run("SchemaString", func(t *testing.T) {
		inputDir := "testdata/schemagen/input/"
		outputDir := "testdata/schemagen/output/"

		files, err := os.ReadDir(inputDir)
		require.NoError(t, err)

		for _, f := range files {
			input, err := os.ReadFile(inputDir + f.Name())
			require.NoError(t, err)

			schHandler, errs := NewHandler(string(input), false)
			require.NoError(t, errs, "schema error for %s", f.Name())

			generated := schHandler.GQLSchema()
			err = os.WriteFile(outputDir+f.Name(), []byte(generated), 0644)
			require.NoError(t, err)
			t.Logf("regenerated: %s", f.Name())
		}
	})

	// Regenerate testdata/apolloservice/output/
	t.Run("ApolloService", func(t *testing.T) {
		inputDir := "testdata/apolloservice/input/"
		outputDir := "testdata/apolloservice/output/"

		files, err := os.ReadDir(inputDir)
		require.NoError(t, err)

		for _, f := range files {
			input, err := os.ReadFile(inputDir + f.Name())
			require.NoError(t, err)

			schHandler, errs := NewHandler(string(input), true)
			require.NoError(t, errs, "schema error for %s", f.Name())

			generated := schHandler.GQLSchemaWithoutApolloExtras()
			err = os.WriteFile(outputDir+f.Name(), []byte(generated), 0644)
			require.NoError(t, err)
			t.Logf("regenerated: %s", f.Name())
		}
	})
}
