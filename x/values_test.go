/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package x

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValueType(t *testing.T) {
	require.Equal(t, ValueType(false, false, false), ValueUid)
	require.Equal(t, ValueType(false, false, true), ValueEmpty)
	require.Equal(t, ValueType(false, true, false), ValueUid)
	require.Equal(t, ValueType(false, true, true), ValueEmpty)
	require.Equal(t, ValueType(true, false, false), ValuePlain)
	require.Equal(t, ValueType(true, false, true), ValuePlain)
	require.Equal(t, ValueType(true, true, false), ValueMulti)
	require.Equal(t, ValueType(true, true, true), ValueMulti)
}
