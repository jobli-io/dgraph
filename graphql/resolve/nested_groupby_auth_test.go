/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package resolve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/dql"
	"github.com/hypermodeinc/dgraph/v25/graphql/dgraph"
	"github.com/hypermodeinc/dgraph/v25/graphql/schema"
	"github.com/hypermodeinc/dgraph/v25/graphql/test"
)

const nestedGroupByAuthSchema = `
interface WorkspaceMember @generate(query: {aggregate: false}) {
  id: ID!
  inWorkspace: Workspace! @generate(mutation: { add: false, update: false, delete: false })
}

type Workspace {
  name: String @search(by: [exact])
  ownedBy: User
}

type User {
  email: String! @id
  hasPrimaryGroup: Group
}

type Group implements WorkspaceMember @auth(
  query: { rule: """
    query($EMAIL: String!) {
      queryGroup {
        inUsers(filter: { email: { eq: $EMAIL } }) { __typename }
      }
    }
  """ }
) {
  id: ID!
  name: String @search(by: [exact])
  inUsers: [User]
}

type Company {
  name: String @search(by: [exact])
  hasPrimaryGroup: Group
}
`

func TestQueryRewritingNestedGroupByAuth(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, nestedGroupByAuthSchema)

	gqlSrc := `query {
		groupByCompany(groupBy: [{ field: { hasPrimaryGroup: { name: true } } }]) {
			count
			groupKeys {
				path
				value
			}
		}
	}`

	jwtVars := map[string]interface{}{
		"EMAIL": "user@test.com",
	}

	op, err := gqlSchema.Operation(&schema.Request{Query: gqlSrc})
	if err != nil {
		for _, qName := range gqlSchema.Queries(schema.FilterQuery) {
			t.Logf("Filter Query: %s", qName)
		}
		for _, qName := range gqlSchema.Queries(schema.GetQuery) {
			t.Logf("Get Query: %s", qName)
		}
		for _, qName := range gqlSchema.Queries(schema.GroupByQuery) {
			t.Logf("GroupBy Query: %s", qName)
		}
	}
	require.NoError(t, err)
	gqlQuery := test.GetQuery(t, op)

	metaInfo.AuthVars = jwtVars
	ctx, err := metaInfo.AddClaimsToContext(context.Background())
	require.NoError(t, err)

	rewriter := NewQueryRewriter()
	dgQuery, err := rewriter.Rewrite(ctx, gqlQuery)
	require.NoError(t, err)

	actual := dgraph.AsString(dgQuery)
	t.Logf("Generated DQL:\n%s", actual)

	// Assert that:
	// 1. Group_Auth3 helper is defined with cascade filter.
	// 2. Company.hasPrimaryGroup edge has @filter(uid(Group_Auth3)) inline!
	// 3. Root groupByCompany uses @groupby(val(__gby_0))
	require.Contains(t, actual, "Group_Auth3 as var(func: type(Group))")
	require.Contains(t, actual, "Company.hasPrimaryGroup @filter(uid(Group_Auth3))")
	require.Contains(t, actual, "@groupby(val(__gby_0))")

	// Always validate: DQL must parse cleanly with no unused variables.
	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "DQL should parse without unused-variable errors")
}

func TestQueryRewritingNestedGroupByAuthMultiLevel(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, nestedGroupByAuthSchema)

	gqlSrc := `query {
		groupByCompany(groupBy: [{ field: { hasPrimaryGroup: { inWorkspace: { name: true } } } }]) {
			count
			groupKeys {
				path
				value
			}
		}
	}`

	jwtVars := map[string]interface{}{
		"EMAIL": "user@test.com",
	}

	op, err := gqlSchema.Operation(&schema.Request{Query: gqlSrc})
	require.NoError(t, err)
	gqlQuery := test.GetQuery(t, op)

	metaInfo.AuthVars = jwtVars
	ctx, err := metaInfo.AddClaimsToContext(context.Background())
	require.NoError(t, err)

	rewriter := NewQueryRewriter()
	dgQuery, err := rewriter.Rewrite(ctx, gqlQuery)
	require.NoError(t, err)

	actual := dgraph.AsString(dgQuery)
	t.Logf("Generated DQL:\n%s", actual)

	// Assert intermediate variables and aggregations:
	// 1. Group_Auth3 as var(func: type(Group)) (auth query)
	// 2. Company.hasPrimaryGroup filtered with uid(Group_Auth3)
	// 3. __gby_0_p0 as max(val(__gby_0_c)) defined inside Company.hasPrimaryGroup
	// 4. __gby_0 as max(val(__gby_0_p0)) defined at root level
	require.Contains(t, actual, "as var(func: type(Group))")
	require.Contains(t, actual, "Company.hasPrimaryGroup @filter(uid(")
	require.Contains(t, actual, "__gby_0_p0 as max(val(__gby_0_c))")
	require.Contains(t, actual, "__gby_0 as max(val(__gby_0_p0))")
	require.Contains(t, actual, "@groupby(val(__gby_0))")

	// Verify that the generated DQL parses perfectly with no level-aggregation errors.
	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "DQL should parse perfectly with no level-aggregation or unused-variable errors")
}

const nestedGroupByAuthWithDirectUserSchema = `
type User @auth(
  query: { rule: """
    query($EMAIL: String!) {
      queryUser(filter: { email: { eq: $EMAIL } }) { email }
    }
  """ }
) {
  email: String! @id
  name: String
}

type NoteType {
  xId: String! @id
  name: String
}

type Note {
  id: ID!
  title: String
  createdBy: User
  hasType: NoteType
}
`

func TestQueryRewritingNestedGroupByAuthDirectTypeScan(t *testing.T) {
	gqlSchema, metaInfo := cascadeAuthSchemaAndMeta(t, nestedGroupByAuthWithDirectUserSchema)

	gqlSrc := `query {
		groupByNote(groupBy: [{ field: { createdBy: { email: true } } }]) {
			count
			groupKeys {
				path
				value
			}
		}
	}`

	jwtVars := map[string]interface{}{
		"EMAIL": "user@test.com",
	}

	op, err := gqlSchema.Operation(&schema.Request{Query: gqlSrc})
	require.NoError(t, err)
	gqlQuery := test.GetQuery(t, op)

	metaInfo.AuthVars = jwtVars
	ctx, err := metaInfo.AddClaimsToContext(context.Background())
	require.NoError(t, err)

	rewriter := NewQueryRewriter()
	dgQuery, err := rewriter.Rewrite(ctx, gqlQuery)
	require.NoError(t, err)

	actual := dgraph.AsString(dgQuery)
	t.Logf("Generated DQL:\n%s", actual)

	// DQL must parse cleanly with no "Some variables are used but not defined" errors.
	_, parseErr := dql.Parse(dql.Request{Str: actual})
	require.NoError(t, parseErr, "DQL should parse without unused or undefined variable errors")
}

func TestUserDQLQueryFromIssue(t *testing.T) {
	dqlQuery := `query {
  var(func: uid(0x1a5)) @filter(type(Workspace)) {
    data_and_1_inWorkspace as Workspace.hasNote
  }
  var(func: type(Candidate)) {
    data_and_3_forResource as NoteOwner.hasNote
  }
  NoteType_1 as var(func: uid(0x83df1f, 0x83df21, 0xef23d0, 0xb72e02, 0xbabbd4, 0x6c8002, 0x83eba1, 0xbcb11a, 0x6c70d5, 0x6c85a1, 0x6c70d8, 0x6c8c2f, 0x731170, 0x83dfe5, 0x67f6eb, 0xb762e9, 0x6f87e8, 0x6c7ff8, 0xa609d2, 0x83ec38, 0x6c8010, 0x67f6e9, 0x83ebae, 0x83ebb4, 0x944384, 0x6c9058, 0xb7640e, 0x83ec3c, 0x75c9fc, 0xbd77ae, 0x83e177, 0x83ec3d, 0x6c92bb, 0x83ec3f, 0xba4849, 0xbfbfd1, 0x6c8c58, 0xb7cb3c, 0x6c7dd6, 0x6c8311, 0xb7671d, 0x6c8078, 0xf0fb4a)) @filter(type(NoteType))
  data_and_4_hasTypeRoot as var(func: uid(NoteType_1)) @filter(((((uid(NoteType_Auth2) OR uid_in(WorkspaceMember.inWorkspace, uid(NoteType_Auth3))) AND uid(NoteType_Auth9)) AND uid(NoteType_Auth10)) AND (uid(NoteType_Auth11) OR uid(NoteType_Auth12))))
  NoteType_Auth2 as var(func: uid(NoteType_1)) @cascade {
    dgraph.type
    WorkspaceMember.inWorkspace : WorkspaceMember.inWorkspace {
      Workspace.hasIAMRoleBinding : Workspace.hasIAMRoleBinding {
        IAMBinding.forUserMember : IAMBinding.forUserMember @filter((eq(User.status, "ACTIVE") AND eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")))
        IAMBinding.forRole : IAMBinding.forRole @filter(eq(IAMRole.permission, "_ALL", "_NOTE", "CREATE", "UPDATE", "READ", "UPDATE_NOTE", "READ_NOTE"))
      }
    }
  }
  var(func: uid(NoteType_1)) {
    NoteType_Auth4 as WorkspaceMember.inWorkspace
  }
  NoteType_Auth3 as var(func: uid(NoteType_Auth4)) @filter((uid(NoteType_Auth5) OR ((uid(NoteType_Auth6) OR uid(NoteType_Auth7)) AND uid(NoteType_Auth8)))) @cascade
  NoteType_Auth5 as var(func: uid(NoteType_Auth4)) @filter((eq(Workspace.name, "dc98a028") AND (uid(NoteType_Auth5_ownedBy)))) @cascade
  var(func: eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")) @filter(type(User)) {
    NoteType_Auth5_ownedBy as User.ownsResource
  }
  NoteType_Auth6 as var(func: uid(NoteType_Auth4)) @filter((uid(NoteType_Auth6_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(Plugin.clientId, "jobli-web")) @filter((((eq(Recordable.deleted, false) OR (NOT (has(Recordable.deleted))))) AND type(Plugin))) {
    NoteType_Auth6_hasIAMBinding_forAppMember as Plugin.hasIAMBinding
  }
  var(func: eq(IAMRole.permission, "_ALL", "_NOTE", "CREATE", "UPDATE", "READ", "UPDATE_NOTE", "READ_NOTE")) @filter(type(IAMRole)) {
    NoteType_Auth6_hasIAMBinding_forRole as IAMRole.hasBinding
  }
  var(func: uid(NoteType_Auth6_hasIAMBinding_forAppMember)) @filter(((uid(NoteType_Auth6_hasIAMBinding_forRole)) AND type(IAMBinding))) {
    NoteType_Auth6_hasIAMBinding as IAMBinding.forResource
  }
  NoteType_Auth7 as var(func: uid(NoteType_Auth4)) @filter((uid(NoteType_Auth7_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(User.status, "ACTIVE")) @filter(((eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")) AND type(User))) {
    NoteType_Auth7_hasIAMBinding_forUserMember as User.hasIAMBinding
  }
  var(func: uid(NoteType_Auth6_hasIAMBinding_forRole)) @filter(((uid(NoteType_Auth7_hasIAMBinding_forUserMember)) AND type(IAMBinding))) {
    NoteType_Auth7_hasIAMBinding as IAMBinding.forResource
  }
  NoteType_Auth8 as var(func: uid(NoteType_Auth4)) @filter((uid(NoteType_Auth8_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(Workspace.name, "dc98a028")) @filter(type(Workspace)) {
    NoteType_Auth8_hasIAMBinding_inWorkspace as Workspace.hasIAMRoleBinding
  }
  var(func: uid(NoteType_Auth8_hasIAMBinding_inWorkspace)) @filter(type(IAMBinding)) {
    NoteType_Auth8_hasIAMBinding as IAMBinding.forResource
  }
  NoteType_Auth9 as var(func: uid(NoteType_1)) @cascade {
    dgraph.type
    WorkspaceMember.inWorkspace : WorkspaceMember.inWorkspace @filter(eq(Workspace.name, "dc98a028"))
  }
  NoteType_Auth10 as var(func: uid(NoteType_1)) @filter(NOT (eq(Recordable.deleted, true))) @cascade {
    dgraph.type
  }
  NoteType_Auth11 as var(func: uid(NoteType_1)) @filter(NOT (has(Manageable.managedBy))) @cascade {
    dgraph.type
  }
  NoteType_Auth12 as var(func: uid(NoteType_1)) @filter((uid_in(Manageable.managedBy, uid(NoteType_Auth12_managedBy)))) @cascade {
    dgraph.type
  }
  NoteType_Auth12_managedBy as var(func: type(Plugin)) @filter((eq(Recordable.deleted, false) OR (NOT (has(Recordable.deleted)))))
  var(func: uid(data_and_4_hasTypeRoot)) {
    data_and_4_hasType as NoteType.hasNote
  }
  User_13 as var(func: uid(0x7a33, 0x27e2, 0x1a2, 0x2d20, 0x393f, 0x30c685, 0x57289, 0x67c4e4, 0x120498d, 0x4d200d)) @filter(type(User))
  data_and_5_createdByRoot as var(func: uid(User_13)) @filter((((((uid(User_Auth14) OR uid(User_Auth15)) OR uid(User_Auth16)) OR uid_in(WorkspaceMember.inWorkspace, uid(User_Auth17))) AND uid(User_Auth23)) AND uid(User_Auth24)))
  User_Auth14 as var(func: uid(User_13)) @filter(eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")) @cascade
  User_Auth15 as var(func: uid(User_13)) @filter((eq(User.email, "idowu@gorillajobs.com.au") AND (uid(User_Auth15_hasInvitation)))) @cascade
  var(func: eq(UserInvitation.status, "PENDING")) @filter(type(UserInvitation)) {
    User_Auth15_hasInvitation as UserInvitation.forUser
  }
  User_Auth16 as var(func: uid(User_13)) @cascade {
    dgraph.type
    WorkspaceMember.inWorkspace : WorkspaceMember.inWorkspace {
      Workspace.hasIAMRoleBinding : Workspace.hasIAMRoleBinding {
        IAMBinding.forUserMember : IAMBinding.forUserMember @filter((eq(User.status, "ACTIVE") AND eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")))
        IAMBinding.forRole : IAMBinding.forRole @filter(eq(IAMRole.permission, "_ALL", "_USER", "CREATE", "UPDATE", "UPDATE_USER", "READ", "READ_USER"))
      }
    }
  }
  var(func: uid(User_13)) {
    User_Auth18 as WorkspaceMember.inWorkspace
  }
  User_Auth17 as var(func: uid(User_Auth18)) @filter((uid(User_Auth19) OR ((uid(User_Auth20) OR uid(User_Auth21)) AND uid(User_Auth22)))) @cascade
  User_Auth19 as var(func: uid(User_Auth18)) @filter((eq(Workspace.name, "dc98a028") AND (uid(User_Auth19_ownedBy)))) @cascade
  var(func: eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")) @filter(type(User)) {
    User_Auth19_ownedBy as User.ownsResource
  }
  User_Auth20 as var(func: uid(User_Auth18)) @filter((uid(User_Auth20_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(Plugin.clientId, "jobli-web")) @filter((((eq(Recordable.deleted, false) OR (NOT (has(Recordable.deleted))))) AND type(Plugin))) {
    User_Auth20_hasIAMBinding_forAppMember as Plugin.hasIAMBinding
  }
  var(func: eq(IAMRole.permission, "_ALL", "_USER", "CREATE", "UPDATE", "UPDATE_USER", "READ", "READ_USER")) @filter(type(IAMRole)) {
    User_Auth20_hasIAMBinding_forRole as IAMRole.hasBinding
  }
  var(func: uid(User_Auth20_hasIAMBinding_forAppMember)) @filter(((uid(User_Auth20_hasIAMBinding_forRole)) AND type(IAMBinding))) {
    User_Auth20_hasIAMBinding as IAMBinding.forResource
  }
  User_Auth21 as var(func: uid(User_Auth18)) @filter((uid(User_Auth21_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(User.status, "ACTIVE")) @filter(((eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")) AND type(User))) {
    User_Auth21_hasIAMBinding_forUserMember as User.hasIAMBinding
  }
  var(func: uid(User_Auth20_hasIAMBinding_forRole)) @filter(((uid(User_Auth21_hasIAMBinding_forUserMember)) AND type(IAMBinding))) {
    User_Auth21_hasIAMBinding as IAMBinding.forResource
  }
  User_Auth22 as var(func: uid(User_Auth18)) @filter((uid(User_Auth22_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(Workspace.name, "dc98a028")) @filter(type(Workspace)) {
    User_Auth22_hasIAMBinding_inWorkspace as Workspace.hasIAMRoleBinding
  }
  var(func: uid(User_Auth22_hasIAMBinding_inWorkspace)) @filter(type(IAMBinding)) {
    User_Auth22_hasIAMBinding as IAMBinding.forResource
  }
  User_Auth23 as var(func: uid(User_13)) @cascade {
    dgraph.type
    WorkspaceMember.inWorkspace : WorkspaceMember.inWorkspace @filter(eq(Workspace.name, "dc98a028"))
  }
  User_Auth24 as var(func: uid(User_13)) @filter(NOT (eq(Recordable.deleted, true))) @cascade {
    dgraph.type
  }
  var(func: uid(data_and_5_createdByRoot)) {
    data_and_5_createdBy as User.hasCreated
  }
  NoteRoot as var(func: uid(Note_25)) @filter((((uid_in(WorkspaceMember.inWorkspace, uid(Note_Auth26)) OR uid_in(Note.forResource, uid(Note_Auth32))) AND uid(Note_Auth80)) AND uid(Note_Auth81)))
  Note_25 as var(func: uid(data_and_1_inWorkspace)) @filter(((eq(Recordable.deleted, false) AND between(Recordable.createdAt, "2026-08-31T14:00:00.000Z", "2026-09-03T23:59:38.669Z") AND (uid(data_and_3_forResource)) AND (uid(data_and_4_hasType)) AND (uid(data_and_5_createdBy))) AND type(Note)))
  var(func: uid(Note_25)) {
    Note_Auth27 as WorkspaceMember.inWorkspace
  }
  Note_Auth26 as var(func: uid(Note_Auth27)) @filter((uid(Note_Auth28) OR ((uid(Note_Auth29) OR uid(Note_Auth30)) AND uid(Note_Auth31)))) @cascade
  Note_Auth28 as var(func: uid(Note_Auth27)) @filter((eq(Workspace.name, "dc98a028") AND (uid(Note_Auth28_ownedBy)))) @cascade
  var(func: eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")) @filter(type(User)) {
    Note_Auth28_ownedBy as User.ownsResource
  }
  Note_Auth29 as var(func: uid(Note_Auth27)) @filter((uid(Note_Auth29_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(Plugin.clientId, "jobli-web")) @filter((((eq(Recordable.deleted, false) OR (NOT (has(Recordable.deleted))))) AND type(Plugin))) {
    Note_Auth29_hasIAMBinding_forAppMember as Plugin.hasIAMBinding
  }
  var(func: eq(IAMRole.permission, "_ALL", "_NOTE", "CREATE", "UPDATE", "READ", "UPDATE_NOTE", "READ_NOTE")) @filter(type(IAMRole)) {
    Note_Auth29_hasIAMBinding_forRole as IAMRole.hasBinding
  }
  var(func: uid(Note_Auth29_hasIAMBinding_forAppMember)) @filter(((uid(Note_Auth29_hasIAMBinding_forRole)) AND type(IAMBinding))) {
    Note_Auth29_hasIAMBinding as IAMBinding.forResource
  }
  Note_Auth30 as var(func: uid(Note_Auth27)) @filter((uid(Note_Auth30_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(User.status, "ACTIVE")) @filter(((eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")) AND type(User))) {
    Note_Auth30_hasIAMBinding_forUserMember as User.hasIAMBinding
  }
  var(func: uid(Note_Auth29_hasIAMBinding_forRole)) @filter(((uid(Note_Auth30_hasIAMBinding_forUserMember)) AND type(IAMBinding))) {
    Note_Auth30_hasIAMBinding as IAMBinding.forResource
  }
  Note_Auth31 as var(func: uid(Note_Auth27)) @filter((uid(Note_Auth31_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(Workspace.name, "dc98a028")) @filter(type(Workspace)) {
    Note_Auth31_hasIAMBinding_inWorkspace as Workspace.hasIAMRoleBinding
  }
  var(func: uid(Note_Auth31_hasIAMBinding_inWorkspace)) @filter(type(IAMBinding)) {
    Note_Auth31_hasIAMBinding as IAMBinding.forResource
  }
  var(func: uid(Note_25)) {
    Note_Auth33 as Note.forResource
  }
  Note_Auth32 as var(func: uid(Note_Auth33)) @filter(((uid(Note_Auth34) OR ((uid(Note_Auth35) OR uid(Note_Auth36)) AND uid(Note_Auth37))) OR (uid_in(Groupable.inGroup, uid(Note_Auth38)) OR uid_in(WorkspaceMember.inWorkspace, uid(Note_Auth49)) OR uid_in(Candidate.hasApplication, uid(Note_Auth55))))) @cascade
  Note_Auth34 as var(func: uid(Note_Auth33)) @cascade {
    dgraph.type
    WorkspaceMember.inWorkspace : WorkspaceMember.inWorkspace {
      Workspace.hasIAMRoleBinding : Workspace.hasIAMRoleBinding {
        IAMBinding.forUserMember : IAMBinding.forUserMember @filter((eq(User.status, "ACTIVE") AND eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")))
        IAMBinding.forRole : IAMBinding.forRole @filter(eq(IAMRole.permission, "_ALL", "CREATE", "_CANDIDATE", "UPDATE", "UPDATE_CANDIDATE", "READ", "READ_CANDIDATE", "_APPLICATION", "READ_APPLICATION"))
      }
    }
  }
  Note_Auth35 as var(func: uid(Note_Auth33)) @filter((uid(Note_Auth29_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth36 as var(func: uid(Note_Auth33)) @filter((uid(Note_Auth30_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth37 as var(func: uid(Note_Auth33)) @filter((uid(Note_Auth31_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: uid(Note_Auth33)) {
    Note_Auth39 as Groupable.inGroup
  }
  Note_Auth38 as var(func: uid(Note_Auth39)) @filter((((uid(Note_Auth40) OR uid(Note_Auth41)) AND uid(Note_Auth42)) OR uid_in(WorkspaceMember.inWorkspace, uid(Note_Auth43)))) @cascade
  Note_Auth40 as var(func: uid(Note_Auth39)) @filter((uid(Note_Auth29_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth41 as var(func: uid(Note_Auth39)) @filter((uid(Note_Auth30_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth42 as var(func: uid(Note_Auth39)) @filter((uid(Note_Auth31_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: uid(Note_Auth39)) {
    Note_Auth44 as WorkspaceMember.inWorkspace
  }
  Note_Auth43 as var(func: uid(Note_Auth44)) @filter((uid(Note_Auth45) OR ((uid(Note_Auth46) OR uid(Note_Auth47)) AND uid(Note_Auth48)))) @cascade
  Note_Auth45 as var(func: uid(Note_Auth44)) @filter((eq(Workspace.name, "dc98a028") AND (uid(Note_Auth28_ownedBy)))) @cascade
  Note_Auth46 as var(func: uid(Note_Auth44)) @filter((uid(Note_Auth29_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth47 as var(func: uid(Note_Auth44)) @filter((uid(Note_Auth30_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth48 as var(func: uid(Note_Auth44)) @filter((uid(Note_Auth31_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: uid(Note_Auth33)) {
    Note_Auth50 as WorkspaceMember.inWorkspace
  }
  Note_Auth49 as var(func: uid(Note_Auth50)) @filter((uid(Note_Auth51) OR ((uid(Note_Auth52) OR uid(Note_Auth53)) AND uid(Note_Auth54)))) @cascade
  Note_Auth51 as var(func: uid(Note_Auth50)) @filter((eq(Workspace.name, "dc98a028") AND (uid(Note_Auth28_ownedBy)))) @cascade
  Note_Auth52 as var(func: uid(Note_Auth50)) @filter((uid(Note_Auth29_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth53 as var(func: uid(Note_Auth50)) @filter((uid(Note_Auth30_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth54 as var(func: uid(Note_Auth50)) @filter((uid(Note_Auth31_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: uid(Note_Auth33)) {
    Note_Auth56 as Candidate.hasApplication
  }
  Note_Auth55 as var(func: uid(Note_Auth56)) @filter((uid_in(Groupable.inGroup, uid(Note_Auth57)) OR uid_in(Application.forJobBoard, uid(Note_Auth68)))) @cascade
  var(func: uid(Note_Auth56)) {
    Note_Auth58 as Groupable.inGroup
  }
  Note_Auth57 as var(func: uid(Note_Auth58)) @filter((((uid(Note_Auth59) OR uid(Note_Auth60)) AND uid(Note_Auth61)) OR uid_in(WorkspaceMember.inWorkspace, uid(Note_Auth62)))) @cascade
  Note_Auth59 as var(func: uid(Note_Auth58)) @filter((uid(Note_Auth29_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth60 as var(func: uid(Note_Auth58)) @filter((uid(Note_Auth30_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth61 as var(func: uid(Note_Auth58)) @filter((uid(Note_Auth31_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: uid(Note_Auth58)) {
    Note_Auth63 as WorkspaceMember.inWorkspace
  }
  Note_Auth62 as var(func: uid(Note_Auth63)) @filter((uid(Note_Auth64) OR ((uid(Note_Auth65) OR uid(Note_Auth66)) AND uid(Note_Auth67)))) @cascade
  Note_Auth64 as var(func: uid(Note_Auth63)) @filter((eq(Workspace.name, "dc98a028") AND (uid(Note_Auth28_ownedBy)))) @cascade
  Note_Auth65 as var(func: uid(Note_Auth63)) @filter((uid(Note_Auth29_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth66 as var(func: uid(Note_Auth63)) @filter((uid(Note_Auth30_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth67 as var(func: uid(Note_Auth63)) @filter((uid(Note_Auth31_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: uid(Note_Auth56)) {
    Note_Auth69 as Application.forJobBoard
  }
  Note_Auth68 as var(func: uid(Note_Auth69)) @filter(((uid(Note_Auth70) OR ((uid(Note_Auth71) OR uid(Note_Auth72)) AND uid(Note_Auth73))) OR uid_in(WorkspaceMember.inWorkspace, uid(Note_Auth74)))) @cascade
  Note_Auth70 as var(func: uid(Note_Auth69)) @cascade {
    dgraph.type
    WorkspaceMember.inWorkspace : WorkspaceMember.inWorkspace {
      Workspace.hasIAMRoleBinding : Workspace.hasIAMRoleBinding {
        IAMBinding.forUserMember : IAMBinding.forUserMember @filter((eq(User.status, "ACTIVE") AND eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")))
        IAMBinding.forRole : IAMBinding.forRole @filter(eq(IAMRole.permission, "_ALL", "_JOBBOARD", "CREATE", "UPDATE", "UPDATE_JOBBOARD", "READ", "READ_JOBBOARD"))
      }
    }
  }
  Note_Auth71 as var(func: uid(Note_Auth69)) @filter((uid(Note_Auth29_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth72 as var(func: uid(Note_Auth69)) @filter((uid(Note_Auth30_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth73 as var(func: uid(Note_Auth69)) @filter((uid(Note_Auth31_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: uid(Note_Auth69)) {
    Note_Auth75 as WorkspaceMember.inWorkspace
  }
  Note_Auth74 as var(func: uid(Note_Auth75)) @filter((uid(Note_Auth76) OR ((uid(Note_Auth77) OR uid(Note_Auth78)) AND uid(Note_Auth79)))) @cascade
  Note_Auth76 as var(func: uid(Note_Auth75)) @filter((eq(Workspace.name, "dc98a028") AND (uid(Note_Auth28_ownedBy)))) @cascade
  Note_Auth77 as var(func: uid(Note_Auth75)) @filter((uid(Note_Auth29_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth78 as var(func: uid(Note_Auth75)) @filter((uid(Note_Auth30_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth79 as var(func: uid(Note_Auth75)) @filter((uid(Note_Auth31_hasIAMBinding))) @cascade {
    dgraph.type
  }
  Note_Auth80 as var(func: uid(Note_25)) @cascade {
    dgraph.type
    WorkspaceMember.inWorkspace : WorkspaceMember.inWorkspace @filter(eq(Workspace.name, "dc98a028"))
  }
  Note_Auth81 as var(func: uid(Note_25)) @filter(NOT (eq(Recordable.deleted, true))) @cascade {
    dgraph.type
  }
  User_82 as var(func: type(User))
  User_Auth83 as var(func: uid(User_82)) @filter(eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")) @cascade
  User_Auth84 as var(func: uid(User_82)) @filter((eq(User.email, "idowu@gorillajobs.com.au") AND (uid(User_Auth84_hasInvitation)))) @cascade
  var(func: eq(UserInvitation.status, "PENDING")) @filter(type(UserInvitation)) {
    User_Auth84_hasInvitation as UserInvitation.forUser
  }
  User_Auth85 as var(func: uid(User_82)) @cascade {
    dgraph.type
    WorkspaceMember.inWorkspace : WorkspaceMember.inWorkspace {
      Workspace.hasIAMRoleBinding : Workspace.hasIAMRoleBinding {
        IAMBinding.forUserMember : IAMBinding.forUserMember @filter((eq(User.status, "ACTIVE") AND eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")))
        IAMBinding.forRole : IAMBinding.forRole @filter(eq(IAMRole.permission, "_ALL", "_USER", "CREATE", "UPDATE", "UPDATE_USER", "READ", "READ_USER"))
      }
    }
  }
  var(func: uid(User_82)) {
    User_Auth87 as WorkspaceMember.inWorkspace
  }
  User_Auth86 as var(func: uid(User_Auth87)) @filter((uid(User_Auth88) OR ((uid(User_Auth89) OR uid(User_Auth90)) AND uid(User_Auth91)))) @cascade
  User_Auth88 as var(func: uid(User_Auth87)) @filter((eq(Workspace.name, "dc98a028") AND (uid(User_Auth88_ownedBy)))) @cascade
  var(func: eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")) @filter(type(User)) {
    User_Auth88_ownedBy as User.ownsResource
  }
  User_Auth89 as var(func: uid(User_Auth87)) @filter((uid(User_Auth89_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(Plugin.clientId, "jobli-web")) @filter((((eq(Recordable.deleted, false) OR (NOT (has(Recordable.deleted))))) AND type(Plugin))) {
    User_Auth89_hasIAMBinding_forAppMember as Plugin.hasIAMBinding
  }
  var(func: eq(IAMRole.permission, "_ALL", "_USER", "CREATE", "UPDATE", "UPDATE_USER", "READ", "READ_USER")) @filter(type(IAMRole)) {
    User_Auth89_hasIAMBinding_forRole as IAMRole.hasBinding
  }
  var(func: uid(User_Auth89_hasIAMBinding_forAppMember)) @filter(((uid(User_Auth89_hasIAMBinding_forRole)) AND type(IAMBinding))) {
    User_Auth89_hasIAMBinding as IAMBinding.forResource
  }
  User_Auth90 as var(func: uid(User_Auth87)) @filter((uid(User_Auth90_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(IAMRole.permission, "_ALL", "_USER", "CREATE", "UPDATE", "UPDATE_USER", "READ", "READ_USER")) @filter(type(IAMRole)) {
    User_Auth90_hasIAMBinding_forRole as IAMRole.hasBinding
  }
  var(func: eq(User.status, "ACTIVE")) @filter(((eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")) AND type(User))) {
    User_Auth90_hasIAMBinding_forUserMember as User.hasIAMBinding
  }
  var(func: uid(User_Auth90_hasIAMBinding_forRole)) @filter(((uid(User_Auth90_hasIAMBinding_forUserMember)) AND type(IAMBinding))) {
    User_Auth90_hasIAMBinding as IAMBinding.forResource
  }
  User_Auth91 as var(func: uid(User_Auth87)) @filter((uid(User_Auth91_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(Workspace.name, "dc98a028")) @filter(type(Workspace)) {
    User_Auth91_hasIAMBinding_inWorkspace as Workspace.hasIAMRoleBinding
  }
  var(func: uid(User_Auth91_hasIAMBinding_inWorkspace)) @filter(type(IAMBinding)) {
    User_Auth91_hasIAMBinding as IAMBinding.forResource
  }
  User_Auth92 as var(func: uid(User_82)) @cascade {
    dgraph.type
    WorkspaceMember.inWorkspace : WorkspaceMember.inWorkspace @filter(eq(Workspace.name, "dc98a028"))
  }
  User_Auth93 as var(func: uid(User_82)) @filter(NOT (eq(Recordable.deleted, true))) @cascade {
    dgraph.type
  }
  NoteType_94 as var(func: type(NoteType))
  NoteType_Auth95 as var(func: uid(NoteType_94)) @cascade {
    dgraph.type
    WorkspaceMember.inWorkspace : WorkspaceMember.inWorkspace {
      Workspace.hasIAMRoleBinding : Workspace.hasIAMRoleBinding {
        IAMBinding.forUserMember : IAMBinding.forUserMember @filter((eq(User.status, "ACTIVE") AND eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")))
        IAMBinding.forRole : IAMBinding.forRole @filter(eq(IAMRole.permission, "_ALL", "_NOTE", "CREATE", "UPDATE", "READ", "UPDATE_NOTE", "READ_NOTE"))
      }
    }
  }
  var(func: uid(NoteType_94)) {
    NoteType_Auth97 as WorkspaceMember.inWorkspace
  }
  NoteType_Auth96 as var(func: uid(NoteType_Auth97)) @filter((uid(NoteType_Auth98) OR ((uid(NoteType_Auth99) OR uid(NoteType_Auth100)) AND uid(NoteType_Auth101)))) @cascade
  NoteType_Auth98 as var(func: uid(NoteType_Auth97)) @filter((eq(Workspace.name, "dc98a028") AND (uid(NoteType_Auth98_ownedBy)))) @cascade
  var(func: eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")) @filter(type(User)) {
    NoteType_Auth98_ownedBy as User.ownsResource
  }
  NoteType_Auth99 as var(func: uid(NoteType_Auth97)) @filter((uid(NoteType_Auth99_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(Plugin.clientId, "jobli-web")) @filter((((eq(Recordable.deleted, false) OR (NOT (has(Recordable.deleted))))) AND type(Plugin))) {
    NoteType_Auth99_hasIAMBinding_forAppMember as Plugin.hasIAMBinding
  }
  var(func: eq(IAMRole.permission, "_ALL", "_NOTE", "CREATE", "UPDATE", "READ", "UPDATE_NOTE", "READ_NOTE")) @filter(type(IAMRole)) {
    NoteType_Auth99_hasIAMBinding_forRole as IAMRole.hasBinding
  }
  var(func: uid(NoteType_Auth99_hasIAMBinding_forAppMember)) @filter(((uid(NoteType_Auth99_hasIAMBinding_forRole)) AND type(IAMBinding))) {
    NoteType_Auth99_hasIAMBinding as IAMBinding.forResource
  }
  NoteType_Auth100 as var(func: uid(NoteType_Auth97)) @filter((uid(NoteType_Auth100_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(IAMRole.permission, "_ALL", "_NOTE", "CREATE", "UPDATE", "READ", "UPDATE_NOTE", "READ_NOTE")) @filter(type(IAMRole)) {
    NoteType_Auth100_hasIAMBinding_forRole as IAMRole.hasBinding
  }
  var(func: eq(User.status, "ACTIVE")) @filter(((eq(User.userId, "e8CLjFdOVJeCYirbV5SDQ6yfUg03")) AND type(User))) {
    NoteType_Auth100_hasIAMBinding_forUserMember as User.hasIAMBinding
  }
  var(func: uid(NoteType_Auth100_hasIAMBinding_forRole)) @filter(((uid(NoteType_Auth100_hasIAMBinding_forUserMember)) AND type(IAMBinding))) {
    NoteType_Auth100_hasIAMBinding as IAMBinding.forResource
  }
  NoteType_Auth101 as var(func: uid(NoteType_Auth97)) @filter((uid(NoteType_Auth101_hasIAMBinding))) @cascade {
    dgraph.type
  }
  var(func: eq(Workspace.name, "dc98a028")) @filter(type(Workspace)) {
    NoteType_Auth101_hasIAMBinding_inWorkspace as Workspace.hasIAMRoleBinding
  }
  var(func: uid(NoteType_Auth101_hasIAMBinding_inWorkspace)) @filter(type(IAMBinding)) {
    NoteType_Auth101_hasIAMBinding as IAMBinding.forResource
  }
  NoteType_Auth102 as var(func: uid(NoteType_94)) @cascade {
    dgraph.type
    WorkspaceMember.inWorkspace : WorkspaceMember.inWorkspace @filter(eq(Workspace.name, "dc98a028"))
  }
  NoteType_Auth103 as var(func: uid(NoteType_94)) @filter(NOT (eq(Recordable.deleted, true))) @cascade {
    dgraph.type
  }
  NoteType_Auth104 as var(func: uid(NoteType_94)) @filter(NOT (has(Manageable.managedBy))) @cascade {
    dgraph.type
  }
  NoteType_Auth105 as var(func: uid(NoteType_94)) @filter((uid_in(Manageable.managedBy, uid(NoteType_Auth105_managedBy)))) @cascade {
    dgraph.type
  }
  NoteType_Auth105_managedBy as var(func: type(Plugin)) @filter((eq(Recordable.deleted, false) OR (NOT (has(Recordable.deleted)))))
  var(func: uid(NoteRoot)) {
    Recordable.createdBy @filter((((((uid(User_Auth83) OR uid(User_Auth84)) OR uid(User_Auth85)) OR uid_in(WorkspaceMember.inWorkspace, uid(User_Auth86))) AND uid(User_Auth92)) AND uid(User_Auth93))) {
      __gby_0_c as User.email
    }
    __gby_0 as max(val(__gby_0_c))
  }
  var(func: uid(NoteRoot)) {
    Note.hasType @filter(((((uid(NoteType_Auth95) OR uid_in(WorkspaceMember.inWorkspace, uid(NoteType_Auth96))) AND uid(NoteType_Auth102)) AND uid(NoteType_Auth103)) AND (uid(NoteType_Auth104) OR uid(NoteType_Auth105)))) {
      __gby_1_c as NoteType.xId
    }
    __gby_1 as max(val(__gby_1_c))
  }
  groupByNote(func: uid(NoteRoot)) @groupby(val(__gby_0), val(__gby_1)) {
    count(uid)
  }
}`

	_, err := dql.Parse(dql.Request{Str: dqlQuery})
	require.NoError(t, err)
}
