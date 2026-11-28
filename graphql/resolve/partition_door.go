/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"github.com/dgraph-io/dgraph/v25/graphql/schema"
)

// SPIKE: the questions asked at the door of a partition.
//
// The claim that selects a partition is whatever the request named: the gateway
// checks it against nothing. The rules keep a caller who does not belong from
// reading or writing through GraphQL, but a DQL request has no rules, and a
// lambda resolver that reads or writes by DQL acts wherever the caller's claim
// points.
//
// So the tenant resolver asks, before it lets a request into a partition,
// whether the rules would show this caller any record that can hold a grant: the
// Workspace, a Group, or a record of any other type implementing the grant
// interface. Each is asked as a GraphQL query, so it is the schema's own rules
// that answer, with whatever the fast path does to them. A caller who holds a
// grant somewhere in the workspace sees the record that holds it. A caller who
// holds none sees none, and is not let in.

// DoorField is the name each door question gives its answer.
const DoorField = "held"

// DoorQuestions returns the GraphQL queries that each return one record if the
// caller can see a record of one grant-holding type, in the order to ask them. A
// caller is let in on the first that returns a record.
//
// The partition's authority is asked about last. Its own rule is the dearest of
// them: a Workspace is also visible through every record that points at it, so
// asking for it walks those records (47 blocks and 133,000 UIDs on the dev
// workspace, about 200 ms). A Group's rule ends at the Workspace's grants, which
// the fast path asks about once and the request that follows needs anyway.
//
// It returns nil if the schema has no grant-holding type with a query of its
// own, and then nobody can be let in.
func DoorQuestions(sch schema.Schema) []string {
	if sch == nil {
		return nil
	}
	op, err := sch.Operation(&schema.Request{
		Query: "query { " + DoorField + ": query" + grantInterface + "(first: 1) { __typename } }"})
	if err != nil || len(op.Queries()) != 1 {
		return nil
	}
	queryable := make(map[string]bool)
	for _, q := range sch.Queries(schema.FilterQuery) {
		queryable[q] = true
	}
	var last, rest []string
	for _, typ := range op.Queries()[0].Type().ImplementingTypes() {
		field := "query" + typ.Name()
		if !queryable[field] {
			continue
		}
		q := "query { " + DoorField + ": " + field + "(first: 1) { __typename } }"
		if typ.Name() == partitionAuthority {
			last = append(last, q)
		} else {
			rest = append(rest, q)
		}
	}
	return append(rest, last...)
}
