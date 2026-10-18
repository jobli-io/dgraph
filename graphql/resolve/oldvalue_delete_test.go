/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"encoding/json"
	"testing"

	"github.com/hypermodeinc/dgraph/v25/dql"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildOldValuePreQuery_CollectsVarsAndOldValues(t *testing.T) {
	// Simulate a delete query with root var "x" and child vars
	queryBlocks := []*dql.GraphQuery{
		{
			Var:  "x",
			Attr: "deleteAutomationScheduleTrigger",
			Func: &dql.Function{Name: "type", Args: []dql.Arg{{Value: "AutomationScheduleTrigger"}}},
			Children: []*dql.GraphQuery{
				{Attr: "uid"},
			},
		},
	}

	selections := map[string]*schema.OldValueSelection{
		"sId": {
			DgraphPredicate: "Identifiable.sId",
		},
		"cron": {
			DgraphPredicate: "AutomationScheduleTrigger.cron",
		},
	}

	result := BuildOldValuePreQuery(queryBlocks, "x", "AutomationScheduleTrigger", selections)

	require.Len(t, result, 2, "must append exactly one 'before' consumer block")

	beforeBlock := result[1]
	assert.Equal(t, "before", beforeBlock.Attr, "consumer block attr must be 'before'")

	// Must reference root var x
	require.NotNil(t, beforeBlock.Func)
	assert.Equal(t, "uid", beforeBlock.Func.Name)
	varNames := make([]string, 0)
	for _, arg := range beforeBlock.Func.Args {
		varNames = append(varNames, arg.Value)
	}
	assert.Contains(t, varNames, "x")

	// Filter by root type
	require.NotNil(t, beforeBlock.Filter)
	require.NotNil(t, beforeBlock.Filter.Func)
	assert.Equal(t, "type", beforeBlock.Filter.Func.Name)
	assert.Equal(t, "AutomationScheduleTrigger", beforeBlock.Filter.Func.Args[0].Value)

	// Children must contain uid, dgraph.type, and the @oldValue selections
	childAttrs := make([]string, 0)
	for _, ch := range beforeBlock.Children {
		childAttrs = append(childAttrs, ch.Attr)
	}
	assert.Contains(t, childAttrs, "uid")
	assert.Contains(t, childAttrs, "dgraph.type")
	assert.Contains(t, childAttrs, "sId : Identifiable.sId")
	assert.Contains(t, childAttrs, "cron : AutomationScheduleTrigger.cron")
}

func TestDeleteEvent_JSONSerialization_WithBefore(t *testing.T) {
	ev := eventPayload{
		Typename:  "AutomationScheduleTrigger",
		Operation: schema.DeleteMutation,
		CommitTs:  123456789,
		Delete: &deleteEvent{
			RootUIDs: []string{"0x123"},
			Before: []map[string]interface{}{
				{
					"uid":  "0x123",
					"sId":  "jobli::550e8400-e29b-41d4-a716-446655440000",
					"cron": "0 9 * * 1-5",
				},
			},
		},
	}

	b, err := json.Marshal(ev)
	require.NoError(t, err)

	var parsed map[string]interface{}
	err = json.Unmarshal(b, &parsed)
	require.NoError(t, err)

	del, ok := parsed["delete"].(map[string]interface{})
	require.True(t, ok, "delete must be an object")

	rootUIDs, ok := del["rootUIDs"].([]interface{})
	require.True(t, ok)
	assert.Equal(t, "0x123", rootUIDs[0])

	beforeList, ok := del["before"].([]interface{})
	require.True(t, ok, "before must be an array")
	require.Len(t, beforeList, 1)

	firstItem, ok := beforeList[0].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "0x123", firstItem["uid"])
	assert.Equal(t, "jobli::550e8400-e29b-41d4-a716-446655440000", firstItem["sId"])
	assert.Equal(t, "0 9 * * 1-5", firstItem["cron"])
}
