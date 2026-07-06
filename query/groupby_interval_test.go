/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package query

import (
	"testing"
	"time"

	_ "time/tzdata" // embed IANA timezone database

	"github.com/stretchr/testify/require"
)

// input is 2026-07-06T14:37:52 UTC = 2026-07-07T00:37:52 AEST (UTC+10).
var testTime = time.Date(2026, 7, 6, 14, 37, 52, 0, time.UTC)

func TestFloorToInterval_UTC(t *testing.T) {
	tests := []struct {
		tok  string
		want time.Time
	}{
		{"hour", time.Date(2026, 7, 6, 14, 0, 0, 0, time.UTC)},
		{"day", time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)},
		{"month", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{"year", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range tests {
		t.Run(tc.tok, func(t *testing.T) {
			got, err := floorToInterval(testTime, tc.tok, "")
			require.NoError(t, err)
			require.True(t, tc.want.Equal(got),
				"want %v, got %v", tc.want, got)
		})
	}
}

func TestFloorToInterval_Timezone(t *testing.T) {
	// 2026-07-06T14:37:52 UTC = 2026-07-07T00:37:52 Australia/Sydney (AEST, UTC+10).
	// Flooring to day/month/year in Sydney time should cross the day boundary.
	tz := "Australia/Sydney"
	sydLoc, err := time.LoadLocation(tz)
	require.NoError(t, err)

	tests := []struct {
		tok  string
		want time.Time
	}{
		// Hour floor in Sydney: 00:00 → 00:00 on 2026-07-07.
		{"hour", time.Date(2026, 7, 7, 0, 0, 0, 0, sydLoc)},
		// Day floor in Sydney: 2026-07-07.
		{"day", time.Date(2026, 7, 7, 0, 0, 0, 0, sydLoc)},
		// Month floor in Sydney: 2026-07-01 (same month, July).
		{"month", time.Date(2026, 7, 1, 0, 0, 0, 0, sydLoc)},
		// Year floor in Sydney: 2026-01-01.
		{"year", time.Date(2026, 1, 1, 0, 0, 0, 0, sydLoc)},
	}
	for _, tc := range tests {
		t.Run(tc.tok, func(t *testing.T) {
			got, err := floorToInterval(testTime, tc.tok, tz)
			require.NoError(t, err)
			require.True(t, tc.want.Equal(got),
				"want %v, got %v", tc.want, got)
		})
	}
}

func TestFloorToInterval_DayBoundary(t *testing.T) {
	// 2024-12-31T23:59:59 UTC = 2025-01-01T10:59:59 AEST (UTC+11 in summer).
	newYearsEveUTC := time.Date(2024, 12, 31, 23, 59, 59, 0, time.UTC)
	tz := "Australia/Sydney"
	sydLoc, _ := time.LoadLocation(tz)

	got, err := floorToInterval(newYearsEveUTC, "day", tz)
	require.NoError(t, err)
	want := time.Date(2025, 1, 1, 0, 0, 0, 0, sydLoc)
	require.True(t, want.Equal(got), "want %v, got %v", want, got)

	got, err = floorToInterval(newYearsEveUTC, "year", tz)
	require.NoError(t, err)
	want = time.Date(2025, 1, 1, 0, 0, 0, 0, sydLoc)
	require.True(t, want.Equal(got), "want %v, got %v", want, got)
}

func TestFloorToInterval_UnknownTokenizer(t *testing.T) {
	_, err := floorToInterval(testTime, "minute", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown tokenizer name")
}

func TestFloorToInterval_InvalidTimezone(t *testing.T) {
	_, err := floorToInterval(testTime, "day", "Invalid/Zone")
	require.Error(t, err)
}
