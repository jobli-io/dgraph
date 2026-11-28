/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package authorization

import (
	"context"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

// A name under DerivedPrefix is the engine's to set: a token cannot, wherever in
// the token it is written, and what the request derived is what the rules see.
func TestDerivedVariablesAreTheEnginesToSet(t *testing.T) {
	meta, err := ParseAuthMeta(`# Dgraph.Authorization {"VerificationKey":"secret","Header":"X-Auth",` +
		`"Namespace":"https://x/claims","Algo":"HS256"}`)
	require.NoError(t, err)

	const name = DerivedPrefix + "tenantRule.0123"
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		name:               true,
		"https://x/claims": map[string]interface{}{"ws": "acme", name: true},
	}).SignedString([]byte("secret"))
	require.NoError(t, err)
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(string(AuthJwtCtxKey), tok))

	claims, err := meta.ExtractCustomClaims(ctx)
	require.NoError(t, err)
	require.Equal(t, "acme", claims.AuthVariables["ws"])
	require.NotContains(t, claims.AuthVariables, name)

	derived := WithDerivedVariables(ctx, map[string]interface{}{name: false, "ws": "globex"})
	claims, err = meta.ExtractCustomClaims(derived)
	require.NoError(t, err)
	require.Equal(t, false, claims.AuthVariables[name])
	require.Equal(t, "acme", claims.AuthVariables["ws"], "only a derived name is taken from the request")

	more := WithDerivedVariables(derived, map[string]interface{}{DerivedPrefix + "other": true})
	claims, err = meta.ExtractCustomClaims(more)
	require.NoError(t, err)
	require.Equal(t, false, claims.AuthVariables[name])
	require.Equal(t, true, claims.AuthVariables[DerivedPrefix+"other"])
}
