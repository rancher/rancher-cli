package rbac

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/steve"
)

func memberBinding(name string, fields map[string]any) unstructured.Unstructured {
	obj := map[string]any{"metadata": map[string]any{"name": name}}
	maps.Copy(obj, fields)
	return unstructured.Unstructured{Object: obj}
}

// memberUserAttrObj builds a raw UserAttribute object: top-level fields
// carry no json tags, so GroupPrincipals.<provider>.Items[] is built with
// bare Go map keys (metadata.name/displayName), matching the wire shape
// resolveGroupDisplayNames reads.
func memberUserAttrObj(name string, groups map[string][]map[string]any) map[string]any {
	gp := map[string]any{}
	for provider, items := range groups {
		arr := make([]any, 0, len(items))
		for _, it := range items {
			arr = append(arr, map[string]any(it))
		}
		gp[provider] = map[string]any{"Items": arr}
	}
	return map[string]any{
		"metadata":        map[string]any{"name": name},
		"GroupPrincipals": gp,
	}
}

func TestResolveMembersUsersGroupsAndUnresolved(t *testing.T) {
	t.Parallel()
	fc := &fakeClient{
		users: []map[string]any{userObj("u-alice", "Alice Ann", "", "local://u-alice")},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"userattributes": {Items: []unstructured.Unstructured{{Object: memberUserAttrObj("u-alice", map[string][]map[string]any{
				"github": {{"metadata": map[string]any{"name": "github_org://8341"}, "displayName": "DevOps Team"}},
			})}}},
		},
	}

	rows, warns, err := ResolveMembers(t.Context(), fc, attrsOver(fc), []unstructured.Unstructured{
		memberBinding("prtb-a", map[string]any{"userName": "u-alice", "userPrincipalName": "local://u-alice", "roleTemplateName": "cluster-owner"}),
		memberBinding("prtb-b", map[string]any{"groupPrincipalName": "github_org://8341", "roleTemplateName": "cluster-member"}),
		memberBinding("prtb-c", map[string]any{"userPrincipalName": "okta_user://never-logged-in", "roleTemplateName": "cluster-member"}),
	})
	require.NoError(t, err)
	assert.Empty(t, warns)
	assert.Equal(t, []MemberRow{
		{Principal: "github_org://8341", DisplayName: "DevOps Team", Role: "cluster-member", Binding: "prtb-b"},
		{Principal: "local://u-alice", DisplayName: "Alice Ann", Role: "cluster-owner", Binding: "prtb-a"},
		{Principal: "okta_user://never-logged-in", Role: "cluster-member", Binding: "prtb-c"},
	}, rows)
}

func TestResolveMembersLazyAndEmpty(t *testing.T) {
	t.Parallel()
	fc := &fakeClient{
		listErrByGVR: map[string]error{
			"users":          errors.New("forbidden"),
			"userattributes": errors.New("forbidden"),
		},
	}

	rows, warns, err := ResolveMembers(t.Context(), fc, attrsOver(fc), nil)
	require.NoError(t, err)
	assert.Empty(t, rows)
	assert.Empty(t, warns, "empty input must issue zero requests and zero warnings")

	rows, warns, err = ResolveMembers(t.Context(), fc, attrsOver(fc), []unstructured.Unstructured{
		memberBinding("prtb-g", map[string]any{"groupPrincipalName": "github_org://1", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	assert.Equal(t, []MemberRow{{Principal: "github_org://1", Role: "r", Binding: "prtb-g"}}, rows)
	require.Len(t, warns, 1, "group-only input must not touch the users LIST")
	assert.Contains(t, warns[0], "could not list userattributes")
}

func TestResolveMembersCollisionStaysUnresolved(t *testing.T) {
	t.Parallel()
	fc := &fakeClient{
		users: []map[string]any{
			userObj("u-1", "First", "", "okta_user://dup"),
			userObj("u-2", "Second", "", "okta_user://dup"),
		},
	}

	rows, warns, err := ResolveMembers(t.Context(), fc, attrsOver(fc), []unstructured.Unstructured{
		memberBinding("prtb-1", map[string]any{"userPrincipalName": "okta_user://dup", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	assert.Equal(t, []MemberRow{{Principal: "okta_user://dup", Role: "r", Binding: "prtb-1"}}, rows, "colliding principal stays unresolved")
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "okta_user://dup")
}

func TestResolveMembersDuplicateUserOwnershipSameDisplay(t *testing.T) {
	t.Parallel()
	// Duplicate USER ownership is ambiguous even when both owners carry the
	// SAME display name - mirrors the RBAC resolver's duplicate-owner refusal.
	fc := &fakeClient{
		users: []map[string]any{
			userObj("u-1", "Same Name", "", "okta_user://dup"),
			userObj("u-2", "Same Name", "", "okta_user://dup"),
		},
	}

	rows, warns, err := ResolveMembers(t.Context(), fc, attrsOver(fc), []unstructured.Unstructured{
		memberBinding("prtb-1", map[string]any{"userPrincipalName": "okta_user://dup", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	assert.Equal(t, []MemberRow{{Principal: "okta_user://dup", Role: "r", Binding: "prtb-1"}}, rows)
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "okta_user://dup")
}

func TestResolveMembersUnrelatedCollisionsStaySilent(t *testing.T) {
	t.Parallel()
	// Directory corruption on principals NO binding references must not
	// attach warnings to a fully resolved card.
	fc := &fakeClient{
		users: []map[string]any{
			userObj("u-alice", "Alice Ann", "", "local://u-alice"),
			userObj("u-x", "X", "", "okta_user://unrelated-dup"),
			userObj("u-y", "Y", "", "okta_user://unrelated-dup"),
		},
	}

	rows, warns, err := ResolveMembers(t.Context(), fc, attrsOver(fc), []unstructured.Unstructured{
		memberBinding("prtb-1", map[string]any{"userName": "u-alice", "userPrincipalName": "local://u-alice", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	assert.Equal(t, []MemberRow{{Principal: "local://u-alice", DisplayName: "Alice Ann", Role: "r", Binding: "prtb-1"}}, rows)
	assert.Empty(t, warns, "unrelated duplicate ownership must not warn")
}

func TestResolveMembersGroupDisplayDisagreement(t *testing.T) {
	t.Parallel()
	// Cached GROUP principals collide only on DISAGREEING display names -
	// the same name cached by many users is the normal case, not a conflict.
	fc := &fakeClient{
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"userattributes": {Items: []unstructured.Unstructured{
				{Object: memberUserAttrObj("u-1", map[string][]map[string]any{
					"github": {{"metadata": map[string]any{"name": "github_org://ok"}, "displayName": "Stable"},
						{"metadata": map[string]any{"name": "github_org://flap"}, "displayName": "Old Name"}},
				})},
				{Object: memberUserAttrObj("u-2", map[string][]map[string]any{
					"github": {{"metadata": map[string]any{"name": "github_org://ok"}, "displayName": "Stable"},
						{"metadata": map[string]any{"name": "github_org://flap"}, "displayName": "New Name"}},
				})},
			}},
		},
	}

	rows, warns, err := ResolveMembers(t.Context(), fc, attrsOver(fc), []unstructured.Unstructured{
		memberBinding("prtb-flap", map[string]any{"groupPrincipalName": "github_org://flap", "roleTemplateName": "r"}),
		memberBinding("prtb-ok", map[string]any{"groupPrincipalName": "github_org://ok", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	assert.Equal(t, []MemberRow{
		{Principal: "github_org://flap", Role: "r", Binding: "prtb-flap"},
		{Principal: "github_org://ok", DisplayName: "Stable", Role: "r", Binding: "prtb-ok"},
	}, rows)
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "github_org://flap")
}

func TestResolveMembersRepeatedPrincipalIDInOneUserIsNotACollision(t *testing.T) {
	t.Parallel()
	// One malformed User repeating the same principal id is ONE owner -
	// ambiguity means multiple Users, never a duplicated slice entry.
	fc := &fakeClient{
		users: []map[string]any{
			userObj("u-1", "Alice Ann", "", "okta_user://rep", "okta_user://rep"),
		},
	}

	rows, warns, err := ResolveMembers(t.Context(), fc, attrsOver(fc), []unstructured.Unstructured{
		memberBinding("prtb-1", map[string]any{"userPrincipalName": "okta_user://rep", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	assert.Equal(t, []MemberRow{{Principal: "okta_user://rep", DisplayName: "Alice Ann", Role: "r", Binding: "prtb-1"}}, rows)
	assert.Empty(t, warns)
}

func TestResolveMembersWarningOrderDeterministic(t *testing.T) {
	t.Parallel()
	// Mixed source failures: users warning ALWAYS precedes userattributes;
	// collision warnings sort by principal regardless of input order.
	fc := &fakeClient{
		listErrByGVR: map[string]error{
			"users":          errors.New("users forbidden"),
			"userattributes": errors.New("attrs forbidden"),
		},
	}
	_, warns, err := ResolveMembers(t.Context(), fc, attrsOver(fc), []unstructured.Unstructured{
		memberBinding("prtb-g", map[string]any{"groupPrincipalName": "github_org://g", "roleTemplateName": "r"}),
		memberBinding("prtb-a", map[string]any{"userName": "u-a", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	require.Len(t, warns, 2)
	assert.Contains(t, warns[0], "could not list users")
	assert.Contains(t, warns[1], "could not list userattributes")

	fc2 := &fakeClient{
		users: []map[string]any{
			userObj("u-1", "A", "", "okta_user://zeta"),
			userObj("u-2", "B", "", "okta_user://zeta"),
			userObj("u-3", "C", "", "okta_user://alpha"),
			userObj("u-4", "D", "", "okta_user://alpha"),
		},
	}
	_, warns2, err := ResolveMembers(t.Context(), fc2, attrsOver(fc2), []unstructured.Unstructured{
		memberBinding("prtb-zeta", map[string]any{"userPrincipalName": "okta_user://zeta", "roleTemplateName": "r"}),
		memberBinding("prtb-alpha", map[string]any{"userPrincipalName": "okta_user://alpha", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	require.Len(t, warns2, 2)
	assert.Contains(t, warns2[0], "okta_user://alpha", "collision warnings sort by principal")
	assert.Contains(t, warns2[1], "okta_user://zeta")
}

func TestResolveMembersSamePrincipalTwoRoles(t *testing.T) {
	t.Parallel()
	// Two CRTBs granting different roles to the same principal → two rows,
	// display name resolved on both, sorted by (principal, role, binding),
	// no warnings.
	fc := &fakeClient{
		users: []map[string]any{userObj("u-alice", "Alice Ann", "", "local://u-alice")},
	}

	rows, warns, err := ResolveMembers(t.Context(), fc, attrsOver(fc), []unstructured.Unstructured{
		memberBinding("crtb-owner", map[string]any{"userName": "u-alice", "userPrincipalName": "local://u-alice", "roleTemplateName": "cluster-owner"}),
		memberBinding("crtb-member", map[string]any{"userName": "u-alice", "userPrincipalName": "local://u-alice", "roleTemplateName": "cluster-member"}),
	})
	require.NoError(t, err)
	assert.Empty(t, warns)
	assert.Equal(t, []MemberRow{
		{Principal: "local://u-alice", DisplayName: "Alice Ann", Role: "cluster-member", Binding: "crtb-member"},
		{Principal: "local://u-alice", DisplayName: "Alice Ann", Role: "cluster-owner", Binding: "crtb-owner"},
	}, rows)
}

// TestResolveMembersSortByBindingWhenPrincipalAndRoleTie asserts the sort
// key's third component: two bindings granting the SAME role to the SAME
// principal (an out-of-band duplicate) sort deterministically by binding
// name rather than depending on input order.
func TestResolveMembersSortByBindingWhenPrincipalAndRoleTie(t *testing.T) {
	t.Parallel()
	fc := &fakeClient{}

	rows, _, err := ResolveMembers(t.Context(), fc, attrsOver(fc), []unstructured.Unstructured{
		memberBinding("crtb-zeta", map[string]any{"userPrincipalName": "okta_user://dup", "roleTemplateName": "r"}),
		memberBinding("crtb-alpha", map[string]any{"userPrincipalName": "okta_user://dup", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "crtb-alpha", rows[0].Binding)
	assert.Equal(t, "crtb-zeta", rows[1].Binding)
}

func TestResolveMembersFlagsSystemPrincipals(t *testing.T) {
	t.Parallel()
	fc := &fakeClient{}

	rows, _, err := ResolveMembers(t.Context(), fc, attrsOver(fc), []unstructured.Unstructured{
		memberBinding("crtb-1", map[string]any{"userPrincipalName": "system://provisioning/fleet-default/lx-import-k3d", "roleTemplateName": "cluster-owner"}),
		memberBinding("crtb-2", map[string]any{"userPrincipalName": "system:serviceaccount:cattle-system:x", "roleTemplateName": "cluster-owner"}),
		memberBinding("crtb-3", map[string]any{"userName": "u-alice", "roleTemplateName": "cluster-member"}),
	})
	require.NoError(t, err)
	require.Len(t, rows, 3)
	for _, row := range rows {
		switch row.Principal {
		case "system://provisioning/fleet-default/lx-import-k3d":
			assert.True(t, row.System, "system:// principal flagged")
		case "system:serviceaccount:cattle-system:x":
			assert.True(t, row.System, "system: (no //) principal must also be flagged")
		default:
			assert.False(t, row.System)
		}
	}
}

func TestResolveMembersCancellationAborts(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fc := &fakeClient{listErrByGVR: map[string]error{"users": context.Canceled}}

	_, _, err := ResolveMembers(ctx, fc, attrsOver(fc), []unstructured.Unstructured{
		memberBinding("prtb-1", map[string]any{"userName": "u-x", "roleTemplateName": "r"}),
	})
	require.Error(t, err, "caller cancellation must abort, not degrade")
}

// cancelAfterUsersClient succeeds the users LIST but cancels the caller
// context as a side effect.
type cancelAfterUsersClient struct {
	*fakeClient
	cancel context.CancelFunc
}

func (c *cancelAfterUsersClient) List(ctx context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error) {
	list, err := c.fakeClient.List(ctx, gvr, ns, selector)
	if gvr.Resource == "users" {
		c.cancel()
	}
	return list, err
}

func TestResolveMembersNoRequestAfterMidFlightCancel(t *testing.T) {
	t.Parallel()
	// The users LIST SUCCEEDS but the caller context is canceled by the
	// time it returns - the userattributes LIST must never be issued and
	// the command must abort, not succeed on partial enrichment.
	ctx, cancel := context.WithCancel(t.Context())
	fc := &cancelAfterUsersClient{
		fakeClient: &fakeClient{
			users: []map[string]any{userObj("u-alice", "Alice Ann", "", "local://u-alice")},
			listsByGVR: map[string]*unstructured.UnstructuredList{
				"userattributes": {},
			},
		},
		cancel: cancel,
	}
	var attrRequests int
	attrs := steve.NewUserAttributes(userAttributeGetter(nil, nil, &attrRequests), listAllowedClient{allowed: true})

	_, _, err := ResolveMembers(ctx, fc, attrs, []unstructured.Unstructured{
		memberBinding("prtb-1", map[string]any{"userName": "u-alice", "roleTemplateName": "r"}),
		memberBinding("prtb-2", map[string]any{"groupPrincipalName": "github_org://g", "roleTemplateName": "r"}),
	})
	require.Error(t, err)
	assert.Zero(t, attrRequests, "no request may follow cancellation")
}

// TestResolveMembersUserAttributeRefusals asserts the group display names
// follow what the caller may read: a refused collection and a collection that
// may leave items out give no names and a warning that says why.
func TestResolveMembersUserAttributeRefusals(t *testing.T) {
	t.Parallel()
	group := []unstructured.Unstructured{memberBinding("prtb-g", map[string]any{"groupPrincipalName": "github_org://8341", "roleTemplateName": "r"})}
	items := []unstructured.Unstructured{{Object: memberUserAttrObj("u-alice", map[string][]map[string]any{
		"github": {{"metadata": map[string]any{"name": "github_org://8341"}, "displayName": "DevOps Team"}},
	})}}
	const prefix = "could not list userattributes; group display names are not shown: "
	tests := []struct {
		name     string
		get      steve.Getter
		listable bool
		display  string
		warning  string
	}{
		{name: "get and list", get: userAttributeGetter(items, nil, nil), listable: true, display: "DevOps Team"},
		{
			name: "collection 404", get: userAttributeGetter(nil, clierr.ResponseError("GET", 404, nil), nil), listable: true,
			warning: prefix + "you cannot list userattributes (GET /v1/management.cattle.io.userattributes answered 404); Rancher's API shows you no userattributes type; a GlobalRole with get and list on userattributes.management.cattle.io allows it; see: " + clierr.Rancher + " get globalroles",
		},
		{
			name: "get only", get: userAttributeGetter(nil, nil, nil), listable: false,
			warning: prefix + "you cannot list every userattribute (a SelfSubjectAccessReview for list on userattributes.management.cattle.io answered denied); GET /v1/management.cattle.io.userattributes may leave items out; a GlobalRole with get and list on userattributes.management.cattle.io allows it; see: " + clierr.Rancher + " get globalroles",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			attrs := steve.NewUserAttributes(tc.get, listAllowedClient{allowed: tc.listable})
			rows, warns, err := ResolveMembers(t.Context(), &fakeClient{}, attrs, group)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, tc.display, rows[0].DisplayName)
			if tc.warning == "" {
				assert.Empty(t, warns)
				return
			}
			assert.Equal(t, []string{tc.warning}, warns)
		})
	}
}
