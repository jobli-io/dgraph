/*
 * SPDX-FileCopyrightText: © 2017-2026 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

// SPIKE: the question asked at the door of a partition.
//
// The claim that selects a partition is whatever the request named: the gateway
// checks it against nothing. The rules keep a caller who does not belong from
// reading or writing through GraphQL, but a DQL request has no rules, and a
// lambda resolver that reads or writes by DQL acts wherever the caller's claim
// points.
//
// So the tenant resolver asks, before it lets a request into a partition,
// whether the rules would show this caller any record that can hold a grant: a
// Workspace, a Group, or any other type implementing the grant interface. It is
// asked as a GraphQL query, so it is the schema's own rules that answer, with
// whatever the fast path does to them. A caller who holds a grant somewhere in
// the workspace sees the record that holds it. A caller who holds none sees none,
// and is not let in.

// DoorQuestion is the GraphQL query that returns one record if the caller holds
// anything in the partition it runs in, and DoorField the name of its answer.
func DoorQuestion() string {
	return "query { " + DoorField + ": query" + grantInterface + "(first: 1) { __typename } }"
}

const DoorField = "held"
