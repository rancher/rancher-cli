package rbac

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// roleTemplateGet returns a role template of the given name and context with
// extra fields, for seeding fakeClient.gets.
func roleTemplateGet(name, context string, extra map[string]any) *unstructured.Unstructured {
	obj := roleTemplateObj(name, context, extra)
	return &obj
}

// TestGrantWarnsOfAHiddenRoleTemplate asserts grant plans the binding of a
// role template whose hidden field is the JSON boolean true and warns, once,
// that Rancher tells its UI not to display it; a role template
// with hidden false, a string value or no hidden field gets no warning; a
// GlobalRole gets none, whatever fields it carries, since it has no hidden
// field; and a role that cannot be read gets none and grant continues.
func TestGrantWarnsOfAHiddenRoleTemplate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		scope map[string]string
		role  string
		get   *unstructured.Unstructured
		err   error
		want  []string
	}{
		{
			name: "project hidden", scope: map[string]string{"project": "p-x", "cluster": "c-x"}, role: "admin",
			get:  roleTemplateGet("admin", "project", map[string]any{"hidden": true}),
			want: []string{`role "admin" is a hidden role template: Rancher tells its UI not to display it`},
		},
		{
			name: "cluster hidden", scope: map[string]string{"cluster": "c-x"}, role: "cluster-admin",
			get:  roleTemplateGet("cluster-admin", "cluster", map[string]any{"hidden": true}),
			want: []string{`role "cluster-admin" is a hidden role template: Rancher tells its UI not to display it`},
		},
		{
			name: "project hidden and locked", scope: map[string]string{"project": "p-x", "cluster": "c-x"}, role: "edit",
			get:  roleTemplateGet("edit", "project", map[string]any{"hidden": true, "locked": true}),
			want: []string{`role "edit" is a hidden role template: Rancher tells its UI not to display it`},
		},
		{name: "project hidden false", scope: map[string]string{"project": "p-x", "cluster": "c-x"}, role: "project-member", get: roleTemplateGet("project-member", "project", map[string]any{"hidden": false})},
		{name: "project no hidden field", scope: map[string]string{"project": "p-x", "cluster": "c-x"}, role: "project-member", get: roleTemplateGet("project-member", "project", nil)},
		{name: "project hidden as a string", scope: map[string]string{"project": "p-x", "cluster": "c-x"}, role: "project-member", get: roleTemplateGet("project-member", "project", map[string]any{"hidden": "true"})},
		{name: "cluster hidden false", scope: map[string]string{"cluster": "c-x"}, role: "cluster-owner", get: roleTemplateGet("cluster-owner", "cluster", map[string]any{"hidden": false})},
		{name: "project role not readable", scope: map[string]string{"project": "p-x", "cluster": "c-x"}, role: "admin", err: clierr.New(clierr.CodeRBAC, "roletemplates is forbidden")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{
				users:       []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
				gets:        map[string]*unstructured.Unstructured{"c-x/p-x": projectGetObj("p-x")},
				getErrs:     map[string]error{},
				allowCreate: true,
			}
			if tt.get != nil {
				client.gets["/"+tt.role] = tt.get
			}
			if tt.err != nil {
				client.getErrs[tt.role] = tt.err
			}
			in := stubInput{strings: map[string]string{"role": tt.role}, args: []string{"u-abc"}}
			maps.Copy(in.strings, tt.scope)
			eff, exec, err := planGrant(t, NewGrant(fakeFactory{c: client}), in)
			require.NoError(t, err)
			require.NotNil(t, exec)
			assert.Equal(t, tt.want, eff.Warnings)
		})
	}
}

// TestGrantGlobalRoleNeverWarnsOfAHiddenField asserts a GlobalRole that
// carries a hidden field gets no warning at global scope: the field belongs
// to RoleTemplate only.
func TestGrantGlobalRoleNeverWarnsOfAHiddenField(t *testing.T) {
	t.Parallel()
	role := roleGet("admin")
	role.Object["hidden"] = true
	client := &fakeClient{
		users:       []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		gets:        map[string]*unstructured.Unstructured{"/admin": role},
		allowCreate: true,
	}
	in := stubInput{bools: map[string]bool{"global": true}, strings: map[string]string{"role": "admin"}, args: []string{"u-abc"}}
	eff, _, err := planGrant(t, NewGrant(fakeFactory{c: client}), in)
	require.NoError(t, err)
	assert.Empty(t, eff.Warnings)
}

// TestGrantHiddenRoleWarningKeepsTheOtherWarnings asserts the hidden role
// warning comes with the warning for a principal Rancher provisions a user
// for, and neither replaces the other: the provisioning warning is first, the
// hidden role warning next.
func TestGrantHiddenRoleWarningKeepsTheOtherWarnings(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		gets: map[string]*unstructured.Unstructured{
			"c-x/p-x": projectGetObj("p-x"),
			"/admin":  roleTemplateGet("admin", "project", map[string]any{"hidden": true}),
		},
		allowCreate: true,
	}
	in := stubInput{
		strings: map[string]string{"project": "p-x", "cluster": "c-x", "role": "admin"},
		args:    []string{"okta_user://ghost"},
	}
	eff, _, err := planGrant(t, NewGrant(fakeFactory{c: client}), in)
	require.NoError(t, err)
	assert.Equal(t, []string{
		fmt.Sprintf("no user exists for principal %q; Rancher will provision user %q when the binding is created",
			"okta_user://ghost", ProvisionedUserID("okta_user://ghost")),
		hiddenRoleWarning("admin"),
	}, eff.Warnings)
}

// roleTemplateGetCounter counts the reads of role templates, by name.
type roleTemplateGetCounter struct {
	*fakeClient
	mu    sync.Mutex
	reads map[string]int
}

func (c *roleTemplateGetCounter) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if gvr.Resource == "roletemplates" {
		c.mu.Lock()
		c.reads[name]++
		c.mu.Unlock()
	}
	return c.fakeClient.Get(ctx, gvr, ns, name)
}

// TestGrantReadsAHiddenRoleTemplateOnce asserts grant takes the hidden field
// from the role template it already reads to check the role: planning a
// binding to a hidden role template sends exactly one read of it, at project
// and at cluster scope.
func TestGrantReadsAHiddenRoleTemplateOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		scope map[string]string
		role  string
		get   *unstructured.Unstructured
	}{
		{name: "project", scope: map[string]string{"project": "p-x", "cluster": "c-x"}, role: "admin", get: roleTemplateGet("admin", "project", map[string]any{"hidden": true})},
		{name: "cluster", scope: map[string]string{"cluster": "c-x"}, role: "cluster-admin", get: roleTemplateGet("cluster-admin", "cluster", map[string]any{"hidden": true})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &roleTemplateGetCounter{
				fakeClient: &fakeClient{
					users:       []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
					gets:        map[string]*unstructured.Unstructured{"c-x/p-x": projectGetObj("p-x"), "/" + tt.role: tt.get},
					allowCreate: true,
				},
				reads: map[string]int{},
			}
			in := stubInput{strings: map[string]string{"role": tt.role}, args: []string{"u-abc"}}
			maps.Copy(in.strings, tt.scope)
			eff, _, err := planGrant(t, NewGrant(fakeFactory{c: client}), in)
			require.NoError(t, err)
			assert.Equal(t, []string{hiddenRoleWarning(tt.role)}, eff.Warnings)
			assert.Equal(t, map[string]int{tt.role: 1}, client.reads)
		})
	}
}

// TestGrantKeepsTheHiddenRoleWarningWhenTheServerRefusesTheCreate asserts a
// server-side dry-run create that Rancher refuses returns its error with the
// hidden role warning; a user who reads only the error still sees it.
func TestGrantKeepsTheHiddenRoleWarningWhenTheServerRefusesTheCreate(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		gets: map[string]*unstructured.Unstructured{
			"c-x/p-x": projectGetObj("p-x"),
			"/admin":  roleTemplateGet("admin", "project", map[string]any{"hidden": true}),
		},
		createFn: func(schema.GroupVersionResource) (*unstructured.Unstructured, error) {
			return nil, clierr.New(clierr.CodeValidation, "admission webhook refused the binding")
		},
	}
	in := stubInput{
		strings: map[string]string{"project": "p-x", "cluster": "c-x", "role": "admin"},
		args:    []string{"u-abc"},
	}
	_, _, err := planGrant(t, NewGrant(fakeFactory{c: client}), in)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a *clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, []string{hiddenRoleWarning("admin")}, ce.Warnings())
}
