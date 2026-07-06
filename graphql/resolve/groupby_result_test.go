/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompleteGroupByResult(t *testing.T) {
	tests := []struct {
		name      string
		queryName string
		raw       string
		wantJSON  string
		wantErr   bool
	}{
		{
			name:      "strips TypeName prefix from predicate keys",
			queryName: "groupByJobAd",
			raw: `{
				"groupByJobAd": [
					{
						"@groupby": [
							{"JobAd.status": "OPEN", "count": 5},
							{"JobAd.status": "DRAFT", "count": 3}
						]
					}
				]
			}`,
			wantJSON: `{"groupByJobAd":[{"count":5,"status":"OPEN"},{"count":3,"status":"DRAFT"}]}`,
		},
		{
			name:      "strips multi-segment prefix (e.g. Recordable.createdAt)",
			queryName: "groupByJobAd",
			raw: `{
				"groupByJobAd": [
					{
						"@groupby": [
							{"Recordable.createdAt": "2026-07-06T10:00:00Z", "count": 20}
						]
					}
				]
			}`,
			wantJSON: `{"groupByJobAd":[{"count":20,"createdAt":"2026-07-06T10:00:00Z"}]}`,
		},
		{
			name:      "aggregate alias fields without dots pass through unchanged",
			queryName: "groupByNote",
			raw: `{
				"groupByNote": [
					{
						"@groupby": [
							{"Note.tag": "bug", "count": 7, "scoreMax": 100, "scoreAvg": 42.5}
						]
					}
				]
			}`,
			wantJSON: `{"groupByNote":[{"count":7,"scoreAvg":42.5,"scoreMax":100,"tag":"bug"}]}`,
		},
		{
			name:      "empty outer list returns empty array",
			queryName: "groupByJobAd",
			raw:       `{"groupByJobAd": []}`,
			wantJSON:  `{"groupByJobAd":[]}`,
		},
		{
			name:      "missing @groupby key returns empty array",
			queryName: "groupByJobAd",
			raw:       `{"groupByJobAd": [{}]}`,
			wantJSON:  `{"groupByJobAd":[]}`,
		},
		{
			name:      "query name not present in response is passed through unchanged",
			queryName: "groupByFoo",
			raw:       `{"groupByBar": []}`,
			wantJSON:  `{"groupByBar":[]}`,
		},
		{
			name:      "preserves other top-level fields (e.g. extensions)",
			queryName: "groupByNote",
			raw: `{
				"groupByNote": [{"@groupby": [{"Note.x": "v", "count": 1}]}],
				"extensions": {"touched_uids": 3}
			}`,
			// We only care that groupByNote is transformed and extensions is kept.
			// Exact key ordering may vary, so we unmarshal to compare.
		},
		{
			name:      "invalid JSON returns error",
			queryName: "groupByJobAd",
			raw:       `not-json`,
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := completeGroupByResult(tc.queryName, []byte(tc.raw))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			if tc.wantJSON == "" {
				// For the "preserves other fields" case just check both keys exist.
				var m map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(out, &m))
				require.Contains(t, m, "groupByNote")
				require.Contains(t, m, "extensions")
				return
			}

			// Unmarshal both to compare structure-independently of key order.
			var got, want interface{}
			require.NoError(t, json.Unmarshal(out, &got))
			require.NoError(t, json.Unmarshal([]byte(tc.wantJSON), &want))
			require.Equal(t, want, got)
		})
	}
}
