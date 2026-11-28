/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTenantAndGrantsMarkers(t *testing.T) {
	load := func(sch string) error {
		_, err := NewHandler(sch, false)
		return err
	}

	t.Run("one tenant, with grants held on it and on other types", func(t *testing.T) {
		require.NoError(t, load(`
			interface Resource @grants { id: ID! }
			type Workspace implements Resource @tenant { name: String! @id }
			type Group implements Resource { title: String }
			type Board @grants { id: ID! title: String }`))
	})

	t.Run("a schema that marks nothing is as it was", func(t *testing.T) {
		require.NoError(t, load(`type Workspace { name: String! @id }`))
	})

	t.Run("two tenants are refused", func(t *testing.T) {
		err := load(`
			type Workspace @tenant { name: String! @id }
			type Organisation @tenant { name: String! @id }`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "a schema can have only one tenant type")
	})

	t.Run("grants with no tenant to hold them in are refused", func(t *testing.T) {
		err := load(`
			interface Resource @grants { id: ID! }
			type Group implements Resource { title: String }`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "@grants needs a type with @tenant")
	})

	t.Run("a tenant is a type, not an interface", func(t *testing.T) {
		err := load(`
			interface Owner @tenant { id: ID! }
			type Workspace implements Owner { name: String! @id }`)
		require.Error(t, err)
	})

	t.Run("a tenant is stored", func(t *testing.T) {
		err := load(`
			type Workspace @tenant @remote { name: String! }
			type Other { id: ID! }`)
		require.Error(t, err)
	})
}
