package inspect

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var userGVR = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "users"}

func memberBinding(fields map[string]any) unstructured.Unstructured {
	obj := map[string]any{"metadata": map[string]any{"name": "b"}}
	for k, v := range fields {
		obj[k] = v
	}
	return unstructured.Unstructured{Object: obj}
}

func userObj(name, display string, principalIDs ...any) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata":     map[string]any{"name": name},
		"displayName":  display,
		"principalIds": principalIDs,
	}}
}

func userAttrObj(name string, groups map[string][]map[string]any) unstructured.Unstructured {
	gp := map[string]any{}
	for provider, items := range groups {
		arr := make([]any, 0, len(items))
		for _, it := range items {
			arr = append(arr, map[string]any(it))
		}
		gp[provider] = map[string]any{"Items": arr}
	}
	return unstructured.Unstructured{Object: map[string]any{
		"metadata":        map[string]any{"name": name},
		"GroupPrincipals": gp,
	}}
}

func TestResolveMembersUsersGroupsAndUnresolved(t *testing.T) {
	t.Parallel()
	fc := newFakeClient().
		withList(userGVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			userObj("u-alice", "Alice Ann", "local://u-alice"),
		}}).
		withList(userAttrGVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			userAttrObj("u-alice", map[string][]map[string]any{
				"github": {{"metadata": map[string]any{"name": "github_org://8341"}, "displayName": "DevOps Team"}},
			}),
		}})

	rows, warns, err := resolveMembers(context.Background(), fc, []unstructured.Unstructured{
		memberBinding(map[string]any{"userName": "u-alice", "userPrincipalName": "local://u-alice", "roleTemplateName": "cluster-owner"}),
		memberBinding(map[string]any{"groupPrincipalName": "github_org://8341", "roleTemplateName": "cluster-member"}),
		memberBinding(map[string]any{"userPrincipalName": "okta_user://never-logged-in", "roleTemplateName": "cluster-member"}),
	})
	require.NoError(t, err)
	assert.Empty(t, warns)
	assert.Equal(t, []memberRow{
		{Principal: "github_org://8341", DisplayName: "DevOps Team", Role: "cluster-member"},
		{Principal: "local://u-alice", DisplayName: "Alice Ann", Role: "cluster-owner"},
		{Principal: "okta_user://never-logged-in", Role: "cluster-member"},
	}, rows)
}

func TestResolveMembersLazyAndEmpty(t *testing.T) {
	t.Parallel()
	fc := newFakeClient().
		withListErr(userGVR, errors.New("forbidden")).
		withListErr(userAttrGVR, errors.New("forbidden"))

	rows, warns, err := resolveMembers(context.Background(), fc, nil)
	require.NoError(t, err)
	assert.Empty(t, rows)
	assert.Empty(t, warns, "empty input must issue zero requests and zero warnings")

	rows, warns, err = resolveMembers(context.Background(), fc, []unstructured.Unstructured{
		memberBinding(map[string]any{"groupPrincipalName": "github_org://1", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	assert.Equal(t, []memberRow{{Principal: "github_org://1", Role: "r"}}, rows)
	require.Len(t, warns, 1, "group-only input must not touch the users LIST")
	assert.Contains(t, warns[0], "userattributes unavailable")
}

func TestResolveMembersCollisionStaysUnresolved(t *testing.T) {
	t.Parallel()
	fc := newFakeClient().
		withList(userGVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			userObj("u-1", "First", "okta_user://dup"),
			userObj("u-2", "Second", "okta_user://dup"),
		}})

	rows, warns, err := resolveMembers(context.Background(), fc, []unstructured.Unstructured{
		memberBinding(map[string]any{"userPrincipalName": "okta_user://dup", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	assert.Equal(t, []memberRow{{Principal: "okta_user://dup", Role: "r"}}, rows, "colliding principal stays unresolved")
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "okta_user://dup")
}

func TestResolveMembersDuplicateUserOwnershipSameDisplay(t *testing.T) {
	t.Parallel()
	// Duplicate USER ownership is ambiguous even when both owners carry the
	// SAME display name — mirrors the RBAC resolver's duplicate-owner refusal.
	fc := newFakeClient().
		withList(userGVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			userObj("u-1", "Same Name", "okta_user://dup"),
			userObj("u-2", "Same Name", "okta_user://dup"),
		}})

	rows, warns, err := resolveMembers(context.Background(), fc, []unstructured.Unstructured{
		memberBinding(map[string]any{"userPrincipalName": "okta_user://dup", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	assert.Equal(t, []memberRow{{Principal: "okta_user://dup", Role: "r"}}, rows)
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "okta_user://dup")
}

func TestResolveMembersUnrelatedCollisionsStaySilent(t *testing.T) {
	t.Parallel()
	// Directory corruption on principals NO binding references must not
	// attach warnings to a fully resolved card.
	fc := newFakeClient().
		withList(userGVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			userObj("u-alice", "Alice Ann", "local://u-alice"),
			userObj("u-x", "X", "okta_user://unrelated-dup"),
			userObj("u-y", "Y", "okta_user://unrelated-dup"),
		}})

	rows, warns, err := resolveMembers(context.Background(), fc, []unstructured.Unstructured{
		memberBinding(map[string]any{"userName": "u-alice", "userPrincipalName": "local://u-alice", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	assert.Equal(t, []memberRow{{Principal: "local://u-alice", DisplayName: "Alice Ann", Role: "r"}}, rows)
	assert.Empty(t, warns, "unrelated duplicate ownership must not warn")
}

func TestResolveMembersGroupDisplayDisagreement(t *testing.T) {
	t.Parallel()
	// Cached GROUP principals collide only on DISAGREEING display names —
	// the same name cached by many users is the normal case, not a conflict.
	fc := newFakeClient().
		withList(userAttrGVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			userAttrObj("u-1", map[string][]map[string]any{
				"github": {{"metadata": map[string]any{"name": "github_org://ok"}, "displayName": "Stable"},
					{"metadata": map[string]any{"name": "github_org://flap"}, "displayName": "Old Name"}},
			}),
			userAttrObj("u-2", map[string][]map[string]any{
				"github": {{"metadata": map[string]any{"name": "github_org://ok"}, "displayName": "Stable"},
					{"metadata": map[string]any{"name": "github_org://flap"}, "displayName": "New Name"}},
			}),
		}})

	rows, warns, err := resolveMembers(context.Background(), fc, []unstructured.Unstructured{
		memberBinding(map[string]any{"groupPrincipalName": "github_org://flap", "roleTemplateName": "r"}),
		memberBinding(map[string]any{"groupPrincipalName": "github_org://ok", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	assert.Equal(t, []memberRow{
		{Principal: "github_org://flap", Role: "r"},
		{Principal: "github_org://ok", DisplayName: "Stable", Role: "r"},
	}, rows)
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "github_org://flap")
}

func TestResolveMembersRepeatedPrincipalIDInOneUserIsNotACollision(t *testing.T) {
	t.Parallel()
	// One malformed User repeating the same principal id is ONE owner —
	// ambiguity means multiple Users, never a duplicated slice entry.
	fc := newFakeClient().
		withList(userGVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			userObj("u-1", "Alice Ann", "okta_user://rep", "okta_user://rep"),
		}})

	rows, warns, err := resolveMembers(context.Background(), fc, []unstructured.Unstructured{
		memberBinding(map[string]any{"userPrincipalName": "okta_user://rep", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	assert.Equal(t, []memberRow{{Principal: "okta_user://rep", DisplayName: "Alice Ann", Role: "r"}}, rows)
	assert.Empty(t, warns)
}

func TestResolveMembersWarningOrderDeterministic(t *testing.T) {
	t.Parallel()
	// Mixed source failures: users warning ALWAYS precedes userattributes;
	// collision warnings sort by principal regardless of input order.
	fc := newFakeClient().
		withListErr(userGVR, errors.New("users forbidden")).
		withListErr(userAttrGVR, errors.New("attrs forbidden"))
	_, warns, err := resolveMembers(context.Background(), fc, []unstructured.Unstructured{
		memberBinding(map[string]any{"groupPrincipalName": "github_org://g", "roleTemplateName": "r"}),
		memberBinding(map[string]any{"userName": "u-a", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	require.Len(t, warns, 2)
	assert.Contains(t, warns[0], "users unavailable")
	assert.Contains(t, warns[1], "userattributes unavailable")

	fc2 := newFakeClient().
		withList(userGVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			userObj("u-1", "A", "okta_user://zeta"),
			userObj("u-2", "B", "okta_user://zeta"),
			userObj("u-3", "C", "okta_user://alpha"),
			userObj("u-4", "D", "okta_user://alpha"),
		}})
	_, warns2, err := resolveMembers(context.Background(), fc2, []unstructured.Unstructured{
		memberBinding(map[string]any{"userPrincipalName": "okta_user://zeta", "roleTemplateName": "r"}),
		memberBinding(map[string]any{"userPrincipalName": "okta_user://alpha", "roleTemplateName": "r"}),
	})
	require.NoError(t, err)
	require.Len(t, warns2, 2)
	assert.Contains(t, warns2[0], "okta_user://alpha", "collision warnings sort by principal")
	assert.Contains(t, warns2[1], "okta_user://zeta")
}

func TestResolveMembersSamePrincipalTwoRoles(t *testing.T) {
	t.Parallel()
	// Two CRTBs granting different roles to the same principal → two rows,
	// display name resolved on both, sorted by (principal, role), no warnings.
	fc := newFakeClient().
		withList(userGVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			userObj("u-alice", "Alice Ann", "local://u-alice"),
		}})

	rows, warns, err := resolveMembers(context.Background(), fc, []unstructured.Unstructured{
		memberBinding(map[string]any{"userName": "u-alice", "userPrincipalName": "local://u-alice", "roleTemplateName": "cluster-owner"}),
		memberBinding(map[string]any{"userName": "u-alice", "userPrincipalName": "local://u-alice", "roleTemplateName": "cluster-member"}),
	})
	require.NoError(t, err)
	assert.Empty(t, warns)
	assert.Equal(t, []memberRow{
		{Principal: "local://u-alice", DisplayName: "Alice Ann", Role: "cluster-member"},
		{Principal: "local://u-alice", DisplayName: "Alice Ann", Role: "cluster-owner"},
	}, rows)
}

func TestResolveMembersCancellationAborts(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fc := newFakeClient().withListErr(userGVR, context.Canceled)

	_, _, err := resolveMembers(ctx, fc, []unstructured.Unstructured{
		memberBinding(map[string]any{"userName": "u-x", "roleTemplateName": "r"}),
	})
	require.Error(t, err, "caller cancellation must abort, not degrade")
}

// cancelAfterUsersClient succeeds the users LIST but cancels the caller
// context as a side effect, and counts userattributes LIST attempts.
type cancelAfterUsersClient struct {
	*fakeClient
	cancel    context.CancelFunc
	attrLists int
}

func (c *cancelAfterUsersClient) List(ctx context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error) {
	if gvr == userAttrGVR {
		c.attrLists++
	}
	list, err := c.fakeClient.List(ctx, gvr, ns, selector)
	if gvr == userGVR {
		c.cancel()
	}
	return list, err
}

func TestResolveMembersNoRequestAfterMidFlightCancel(t *testing.T) {
	t.Parallel()
	// The users LIST SUCCEEDS but the caller context is canceled by the
	// time it returns — the userattributes LIST must never be issued and
	// the command must abort, not succeed on partial enrichment.
	ctx, cancel := context.WithCancel(context.Background())
	fc := &cancelAfterUsersClient{
		fakeClient: newFakeClient().
			withList(userGVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
				userObj("u-alice", "Alice Ann", "local://u-alice"),
			}}).
			withList(userAttrGVR, &unstructured.UnstructuredList{}),
		cancel: cancel,
	}

	_, _, err := resolveMembers(ctx, fc, []unstructured.Unstructured{
		memberBinding(map[string]any{"userName": "u-alice", "roleTemplateName": "r"}),
		memberBinding(map[string]any{"groupPrincipalName": "github_org://g", "roleTemplateName": "r"}),
	})
	require.Error(t, err)
	assert.Zero(t, fc.attrLists, "no request may follow cancellation")
}
