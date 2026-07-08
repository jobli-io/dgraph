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
		name        string
		queryName   string
		raw         string
		wantJSON    string
		wantErr     bool
		notSelected bool
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
			// Direct group-by fields are now emitted as groupKeys entries.
			wantJSON: `{"groupByJobAd":[{"count":5,"groupKeys":[{"path":"status","value":"OPEN"}]},{"count":3,"groupKeys":[{"path":"status","value":"DRAFT"}]}]}`,
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
			wantJSON: `{"groupByJobAd":[{"count":20,"groupKeys":[{"path":"createdAt","value":"2026-07-06T10:00:00Z"}]}]}`,
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
			// "Note.tag" becomes a groupKeys entry; aggregates pass through.
			wantJSON: `{"groupByNote":[{"count":7,"groupKeys":[{"path":"tag","value":"bug"}],"scoreAvg":42.5,"scoreMax":100}]}`,
		},
		{
			name:      "nested spec resolved via pathMap (leaf-UID traversal)",
			queryName: "groupByApplication",
			raw: `{
				"groupByApplication": [
					{
						"@groupby": [
							{"ApplicationStatus.name": "Screened", "count": 5}
						]
					}
				]
			}`,
			// DQL groups by the leaf scalar (ApplicationStatus.name).
			// pathMap["name"] = "hasStatus.name" → full path used in groupKeys.
			wantJSON: `{"groupByApplication":[{"count":5,"groupKeys":[{"path":"hasStatus.name","value":"Screened"}]}]}`,
		},
		{
			name:      "multiple nested specs resolved via pathMap",
			queryName: "groupByCompany",
			raw: `{
				"groupByCompany": [
					{
						"@groupby": [
							{"val(__gby_0)": "Co Auth Group A", "val(__gby_1)": "test@gorillajobs.app", "count": 1}
						]
					}
				]
			}`,
			// Note: It follows the exact same order as defined in the query (groupByArg list)
			wantJSON: `{"groupByCompany":[{"count":1,"groupKeys":[{"path":"hasPrimaryGroup.name","value":"Co Auth Group A"},{"path":"createdBy.email","value":"test@gorillajobs.app"}]}]}`,
		},
		{
			name:      "nested spec resolved via pathMap (value-variable-based groupby)",
			queryName: "groupByApplication",
			raw: `{
				"groupByApplication": [
					{
						"@groupby": [
							{"val(__gby_0)": "Screened", "count": 5}
						]
					}
				]
			}`,
			wantJSON: `{"groupByApplication":[{"count":5,"groupKeys":[{"path":"hasStatus.name","value":"Screened"}]}]}`,
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
			name:      "omits groupKeys when not selected",
			queryName: "groupByJobAd",
			raw: `{
				"groupByJobAd": [
					{
						"@groupby": [
							{"JobAd.status": "OPEN", "count": 5}
						]
					}
				]
			}`,
			wantJSON:    `{"groupByJobAd":[{"count":5}]}`,
			notSelected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Build a pathMap for the nested spec test case.
			// The map is: leaf-field-name → full dot-separated GraphQL path.
			var pathMap map[string]string
			if tc.queryName == "groupByApplication" {
				pathMap = map[string]string{
					"name":         "hasStatus.name",
					"val(__gby_0)": "hasStatus.name",
				}
			} else if tc.queryName == "groupByCompany" {
				pathMap = map[string]string{
					"val(__gby_0)": "0000:hasPrimaryGroup.name",
					"val(__gby_1)": "0001:createdBy.email",
				}
			}

			out, err := completeGroupByResult(tc.queryName, []byte(tc.raw), pathMap, !tc.notSelected)

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
