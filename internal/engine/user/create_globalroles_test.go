package user

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const testPassword = "correctHorseBattery1\n"

func defaultRole(id string) *unstructured.Unstructured {
	return globalRole(id, map[string]any{"newUserDefault": true, "rules": tokenRules()})
}

func plainRole(id string) *unstructured.Unstructured {
	return globalRole(id, map[string]any{"rules": tokenRules()})
}

func typedInput(roles ...string) userInput {
	in := planInput("alice")
	in.lists["global-role"] = roles
	return in
}

// bindingRoles returns the globalRoleName of every recorded binding create,
// dry-run or real as asked, in order.
func bindingRoles(c *fakeUserClient, dryRun bool) []string {
	var out []string
	for _, b := range c.bindings {
		if b.dryRun == dryRun {
			role, _, _ := unstructured.NestedString(b.obj.Object, "globalRoleName")
			out = append(out, role)
		}
	}
	return out
}

// committedDetails returns the details of a committed partial success, read
// back from their JSON form as a client of the envelope reads them.
func committedDetails(t *testing.T, err error) (*clierr.Error, map[string]any) {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	raw, mErr := json.Marshal(ce.Details)
	require.NoError(t, mErr)
	var details map[string]any
	require.NoError(t, json.Unmarshal(raw, &details))
	return ce, details
}

// TestCreateBindsTheServerDefaultGlobalRoles asserts that without
// --global-role create user binds every GlobalRole marked newUserDefault, in
// sorted order, and no other: each dry-run binding names the preview user and
// each real binding names the id the real User create returned, never the
// preview name.
func TestCreateBindsTheServerDefaultGlobalRoles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		roles []*unstructured.Unstructured
		want  []string
	}{
		{name: "one default", roles: []*unstructured.Unstructured{defaultRole("user"), plainRole("user-base")}, want: []string{"user"}},
		{name: "two defaults sorted", roles: []*unstructured.Unstructured{defaultRole("zeta"), plainRole("middle"), defaultRole("alpha")}, want: []string{"alpha", "zeta"}},
		{name: "no default", roles: []*unstructured.Unstructured{plainRole("user-base")}, want: nil},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			preview, committed := fmt.Sprintf("u-preview-%d", i), fmt.Sprintf("u-real-%d", i)
			client := &fakeUserClient{userPreviewName: preview, userRealName: committed, roles: tt.roles}
			eff, exec, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
			require.NoError(t, err)
			assert.Equal(t, tt.want, bindingRoles(client, true))
			assert.Equal(t, 1, client.roleListCalls)

			_, err = exec(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.want, bindingRoles(client, false))
			assert.Equal(t, 1, client.roleListCalls, "execute lists nothing")

			for _, b := range client.bindings {
				wantUser := committed
				if b.dryRun {
					wantUser = preview
				}
				assert.Equal(t, wantUser, b.obj.Object["userName"], "dry-run=%v", b.dryRun)
				assert.Equal(t, bindingGenerateName, b.obj.GetGenerateName())
				assert.Empty(t, b.obj.GetNamespace())
				assert.NotContains(t, b.obj.Object, "userPrincipalName")
				assert.Equal(t, "GlobalRoleBinding", b.obj.GetKind())
			}
			if len(tt.want) == 0 {
				require.Len(t, eff.Warnings, 2)
				assert.Contains(t, eff.Warnings[1], "marks no global role as a default")
			}
		})
	}
}

// TestCreateGlobalRoleFlagReplacesTheDefaults asserts typed roles replace
// the defaults, are bound once each in sorted order, and need no GlobalRole
// list; an unknown role is NOT_FOUND before any User or binding dry-run and
// before any write.
func TestCreateGlobalRoleFlagReplacesTheDefaults(t *testing.T) {
	t.Parallel()
	roles := []*unstructured.Unstructured{defaultRole("user"), plainRole("user-base"), plainRole("other")}

	t.Run("typed roles", func(t *testing.T) {
		t.Parallel()
		client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2", roles: roles}
		_, exec, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, typedInput("user-base", "other", "user-base"))
		require.NoError(t, err)
		_, err = exec(t.Context())
		require.NoError(t, err)
		assert.Equal(t, []string{"other", "user-base"}, bindingRoles(client, true))
		assert.Equal(t, []string{"other", "user-base"}, bindingRoles(client, false))
		assert.Zero(t, client.roleListCalls)
		assert.Equal(t, 2, client.roleGetCalls, "one read per distinct typed role")
	})

	t.Run("unknown role", func(t *testing.T) {
		t.Parallel()
		client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2", roles: roles}
		_, _, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, typedInput("user-bsae"))
		require.Error(t, err)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		require.ErrorContains(t, err, "role not found: user-bsae")
		assert.Equal(t, clierr.Rancher+" get globalroles", ce.Hint)
		assert.Empty(t, client.bindings)
		assert.Zero(t, client.userCreateCalls)
		assert.Empty(t, client.writes)
	})

	t.Run("empty id", func(t *testing.T) {
		t.Parallel()
		client := &fakeUserClient{userPreviewName: "u-preview1", roles: roles}
		_, _, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, typedInput(" "))
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})

	t.Run("unreadable role is bound and gets no warning", func(t *testing.T) {
		t.Parallel()
		client := &fakeUserClient{userPreviewName: "u-preview1", roleGetErr: clierr.New(clierr.CodeRBAC, "forbidden")}
		eff, _, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, typedInput("user"))
		require.NoError(t, err)
		assert.Equal(t, []string{"user"}, bindingRoles(client, true))
		assert.Len(t, eff.Warnings, 2, "the Secret note and the binding note only")
	})
}

// TestCreateTypedRolesThatCannotCreateTokens asserts the preview warns, after
// the Secret and binding notes, when none of the typed roles has a rule that
// allows create on tokens in ext.cattle.io, and that the command still runs.
func TestCreateTypedRolesThatCannotCreateTokens(t *testing.T) {
	t.Parallel()
	warning := "none of these global roles allows creating tokens; the user cannot log in with rancher-cli until granted one; preview the grant with: " +
		clierr.Rancher + " grant 'alice' --global --role user --dry-run"
	wildcard := []any{map[string]any{"apiGroups": []any{"*"}, "resources": []any{"*"}, "verbs": []any{"*"}}}
	tests := []struct {
		name string
		role *unstructured.Unstructured
		warn bool
	}{
		{name: "user-base rules", role: globalRole("r", map[string]any{"rules": tokenRules()})},
		{name: "wildcard rule", role: globalRole("r", map[string]any{"rules": wildcard})},
		{name: "namespaced rules only", role: globalRole("r", map[string]any{"namespacedRules": map[string]any{"ns": tokenRules()}}), warn: true},
		{name: "inherited cluster roles only", role: globalRole("r", map[string]any{"inheritedClusterRoles": []any{"cluster-owner"}}), warn: true},
		{name: "reads settings only", role: globalRole("r", map[string]any{"rules": []any{map[string]any{
			"apiGroups": []any{"management.cattle.io"}, "resources": []any{"settings"}, "verbs": []any{"get", "list"},
		}}}), warn: true},
		{name: "create on other resource of the group", role: globalRole("r", map[string]any{"rules": []any{map[string]any{
			"apiGroups": []any{"ext.cattle.io"}, "resources": []any{"kubeconfigs"}, "verbs": []any{"create"},
		}}}), warn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2", roles: []*unstructured.Unstructured{tt.role}}
			eff, exec, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, typedInput("r"))
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(eff.Warnings), 2)
			assert.Contains(t, eff.Warnings[0], "v1/Secret")
			assert.Equal(t, "also creates a management.cattle.io/v3 GlobalRoleBinding for global role r", eff.Warnings[1])
			if tt.warn {
				require.Len(t, eff.Warnings, 3)
				assert.Equal(t, warning, eff.Warnings[2])
			} else {
				assert.Len(t, eff.Warnings, 2)
			}
			_, err = exec(t.Context())
			require.NoError(t, err, "the command still runs")
			assert.Equal(t, []string{"r"}, bindingRoles(client, false))
		})
	}

	t.Run("one role that allows it is enough", func(t *testing.T) {
		t.Parallel()
		client := &fakeUserClient{userPreviewName: "u-preview1", roles: []*unstructured.Unstructured{
			globalRole("a", map[string]any{"inheritedClusterRoles": []any{"cluster-owner"}}), plainRole("b"),
		}}
		eff, _, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, typedInput("a", "b"))
		require.NoError(t, err)
		assert.Len(t, eff.Warnings, 3, "the Secret note and one note per role")
	})
}

// TestCreateBindingDryRunRefusedCommitsNothing asserts a binding the server
// refuses at the dry-run (the webhook's escalation check) fails the Plan with
// RBAC, no hint and the way out in the message, before anything is written.
func TestCreateBindingDryRunRefusedCommitsNothing(t *testing.T) {
	t.Parallel()
	webhook := `admission webhook "rancher.cattle.io.globalrolebindings.management.cattle.io" denied the request: errors due to escalation: user "u-op" ... is attempting to grant RBAC permissions not currently held: ...`
	client := &fakeUserClient{
		userPreviewName: "u-preview1", userRealName: "u-real2",
		roles:            []*unstructured.Unstructured{defaultRole("user")},
		bindingDryRunErr: map[string]error{"user": clierr.New(clierr.CodeRBAC, webhook)},
	}
	_, _, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, 6, ce.ExitCode())
	assert.Empty(t, ce.Hint)
	require.ErrorContains(t, err, "you may not bind global role user to a new user: ")
	require.ErrorContains(t, err, "errors due to escalation")
	require.ErrorContains(t, err, "--global-role")
	assert.Empty(t, client.writes, "no real create of any kind")
}

// TestCreateBindingDryRunOtherErrorPassesThrough asserts a binding dry-run
// error that is not a permission refusal is returned unchanged.
func TestCreateBindingDryRunOtherErrorPassesThrough(t *testing.T) {
	t.Parallel()
	boom := clierr.New(clierr.CodeValidation, "bad binding")
	client := &fakeUserClient{
		userPreviewName:  "u-preview1",
		roles:            []*unstructured.Unstructured{defaultRole("user")},
		bindingDryRunErr: map[string]error{"user": boom},
	}
	_, _, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
	assert.Same(t, boom, err)
}

// TestCreateBindingFailuresAfterUserCommittedNameEachGrant asserts that after
// a binding create fails the remaining roles are still attempted, then one
// committed partial success carries the code and message of the first failure
// and a recovery with one grant clause per role not bound; no delete is sent.
func TestCreateBindingFailuresAfterUserCommittedNameEachGrant(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		realErr    map[string]error
		wantCode   string
		wantMsg    string
		wantUnbond []string
	}{
		{
			name: "both fail with different codes",
			realErr: map[string]error{
				"alpha": clierr.New(clierr.CodeRBAC, "alpha refused"),
				"beta":  clierr.New(clierr.CodeConflict, "beta conflicts"),
			},
			wantCode: clierr.CodeRBAC, wantMsg: "alpha refused", wantUnbond: []string{"alpha", "beta"},
		},
		{
			name:       "only the first fails",
			realErr:    map[string]error{"alpha": clierr.New(clierr.CodeConflict, "alpha conflicts")},
			wantCode:   clierr.CodeConflict,
			wantMsg:    "alpha conflicts",
			wantUnbond: []string{"alpha"},
		},
		{
			name:       "only the second fails",
			realErr:    map[string]error{"beta": clierr.New(clierr.CodeNotFound, "beta is gone")},
			wantCode:   clierr.CodeNotFound,
			wantMsg:    "beta is gone",
			wantUnbond: []string{"beta"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeUserClient{
				userPreviewName: "u-preview1", userRealName: "u-real2",
				roles:          []*unstructured.Unstructured{defaultRole("alpha"), defaultRole("beta")},
				bindingRealErr: tt.realErr,
			}
			_, exec, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
			require.NoError(t, err)
			_, err = exec(t.Context())
			require.Error(t, err)
			ce, details := committedDetails(t, err)
			assert.Equal(t, tt.wantCode, ce.Code)
			assert.Equal(t, clierr.ExitCode(tt.wantCode), ce.ExitCode())
			require.ErrorContains(t, err, tt.wantMsg)
			assert.Equal(t, true, details["committed"])
			assert.Equal(t, "u-real2", details["resourceName"])
			recovery, _ := details["recovery"].(string)
			assert.Contains(t, recovery, "the user 'u-real2' was created; ")
			for _, role := range []string{"alpha", "beta"} {
				clause := "global role " + role + " was not bound; preview the grant with: " + clierr.Rancher + " grant 'u-real2' --global --role '" + role + "' --dry-run"
				if slices.Contains(tt.wantUnbond, role) {
					assert.Contains(t, recovery, clause)
				} else {
					assert.NotContains(t, recovery, "global role "+role+" was not bound")
				}
			}
			assert.Equal(t, []string{"alpha", "beta"}, bindingRoles(client, false), "every role is attempted")
			assert.Zero(t, client.deleteCalls)
		})
	}
}

// TestCreateSecretFailureNamesTheRolesNotBound asserts a Secret failure
// stops before any binding, and its recovery is the Secret recovery plus one
// grant clause per role; an id that is not an object name gets prose only.
func TestCreateSecretFailureNamesTheRolesNotBound(t *testing.T) {
	t.Parallel()
	roles := []*unstructured.Unstructured{defaultRole("alpha"), defaultRole("beta")}

	t.Run("object name id", func(t *testing.T) {
		t.Parallel()
		client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2", roles: roles,
			secretCreateErr: clierr.New(clierr.CodeGeneric, "boom")}
		_, exec, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
		require.NoError(t, err)
		_, err = exec(t.Context())
		require.Error(t, err)
		_, details := committedDetails(t, err)
		recovery, _ := details["recovery"].(string)
		assert.Contains(t, recovery, clierr.Rancher+" create resource -f -")
		assert.Contains(t, recovery, "--dry-run; global role alpha was not bound; preview the grant with: "+clierr.Rancher+" grant 'u-real2' --global --role 'alpha' --dry-run; global role beta was not bound; preview the grant with: "+clierr.Rancher+" grant 'u-real2' --global --role 'beta' --dry-run")
		assert.Empty(t, bindingRoles(client, false), "no binding is attempted")
	})

	t.Run("id that is not an object name", func(t *testing.T) {
		t.Parallel()
		client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "a'b", roles: roles,
			secretCreateErr: clierr.New(clierr.CodeGeneric, "boom")}
		_, exec, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
		require.NoError(t, err)
		_, err = exec(t.Context())
		_, details := committedDetails(t, err)
		assert.Equal(t, "the user 'a'\\''b' was created without a password; the CLI prints no command for an id it cannot quote; global role alpha was not bound; global role beta was not bound", details["recovery"])
		assert.Empty(t, bindingRoles(client, false))
	})

	t.Run("no roles leaves today's recovery", func(t *testing.T) {
		t.Parallel()
		client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "a'b",
			secretCreateErr: clierr.New(clierr.CodeGeneric, "boom")}
		_, exec, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
		require.NoError(t, err)
		_, err = exec(t.Context())
		_, details := committedDetails(t, err)
		assert.Equal(t, "the user 'a'\\''b' was created without a password; the CLI prints no command for an id it cannot quote", details["recovery"])
	})
}

// TestCreateBindingFailureWithAnIdThatIsNotAnObjectNameNamesNoCommand asserts
// the binding recovery keeps the clause and prints no command for an id that
// is not an object name.
func TestCreateBindingFailureWithAnIdThatIsNotAnObjectNameNamesNoCommand(t *testing.T) {
	t.Parallel()
	client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "a'b",
		roles:          []*unstructured.Unstructured{defaultRole("alpha")},
		bindingRealErr: map[string]error{"alpha": clierr.New(clierr.CodeGeneric, "boom")}}
	_, exec, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
	require.NoError(t, err)
	_, err = exec(t.Context())
	_, details := committedDetails(t, err)
	assert.Equal(t, "the user 'a'\\''b' was created; global role alpha was not bound", details["recovery"])
}

// TestCreateGlobalRoleListRefused asserts a refused GlobalRole list fails the
// Plan as RBAC, naming --global-role, with nothing written.
func TestCreateGlobalRoleListRefused(t *testing.T) {
	t.Parallel()
	client := &fakeUserClient{userPreviewName: "u-preview1",
		roleListErr: clierr.New(clierr.CodeRBAC, `globalroles.management.cattle.io is forbidden`)}
	_, _, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	require.ErrorContains(t, err, "cannot list global roles")
	require.ErrorContains(t, err, "globalroles.management.cattle.io is forbidden")
	require.ErrorContains(t, err, "--global-role")
	assert.Empty(t, client.writes)
	assert.Zero(t, client.userCreateCalls)
}

// TestCreateExecuteDoesNotListGlobalRolesAgain asserts the real writes follow
// the set Plan resolved: a change to the GlobalRoles between Plan and execute
// changes nothing, and execute reads no GlobalRole.
func TestCreateExecuteDoesNotListGlobalRolesAgain(t *testing.T) {
	t.Parallel()
	client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2",
		roles: []*unstructured.Unstructured{defaultRole("user")}}
	_, exec, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
	require.NoError(t, err)
	lists, gets := client.roleListCalls, client.roleGetCalls

	client.roles = []*unstructured.Unstructured{defaultRole("other"), defaultRole("another")}
	client.onRoleList = func() { t.Error("execute listed the global roles") }
	_, err = exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"user"}, bindingRoles(client, false))
	assert.Equal(t, lists, client.roleListCalls)
	assert.Equal(t, gets, client.roleGetCalls)
}

// TestCreatePreviewNamesEachBinding asserts the preview notes: the Secret
// note first, one note per role in order, and for an empty role set a note
// that names the username in the grant command.
func TestCreatePreviewNamesEachBinding(t *testing.T) {
	t.Parallel()
	t.Run("two roles", func(t *testing.T) {
		t.Parallel()
		client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2",
			roles: []*unstructured.Unstructured{defaultRole("zeta"), defaultRole("alpha")}}
		eff, _, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
		require.NoError(t, err)
		assert.Equal(t, []string{
			"also creates a v1/Secret in cattle-local-user-passwords to hold the password",
			"also creates a management.cattle.io/v3 GlobalRoleBinding for global role alpha",
			"also creates a management.cattle.io/v3 GlobalRoleBinding for global role zeta",
		}, eff.Warnings)
		for _, w := range eff.Warnings {
			assert.NotContains(t, w, "u-preview1")
		}
	})

	t.Run("no default names the username quoted", func(t *testing.T) {
		t.Parallel()
		client := &fakeUserClient{userPreviewName: "u-preview1"}
		eff, _, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("o'neil"))
		require.NoError(t, err)
		require.Len(t, eff.Warnings, 2)
		assert.Equal(t, "the server marks no global role as a default for new users; the user cannot log in until granted one; preview the grant with: "+
			clierr.Rancher+` grant 'o'\''neil' --global --role user --dry-run`, eff.Warnings[1])
	})
}

// TestCreateResultReportsBindings asserts the result lists each binding the
// real create returned, in data and in the text.
func TestCreateResultReportsBindings(t *testing.T) {
	t.Parallel()
	t.Run("two bindings", func(t *testing.T) {
		t.Parallel()
		client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2",
			roles: []*unstructured.Unstructured{defaultRole("alpha"), defaultRole("beta")}}
		_, exec, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
		require.NoError(t, err)
		res, err := exec(t.Context())
		require.NoError(t, err)
		raw, err := json.Marshal(res.Data)
		require.NoError(t, err)
		var data struct {
			Bindings []struct {
				Name           string `json:"name"`
				GlobalRoleName string `json:"globalRoleName"`
			} `json:"globalRoleBindings"`
		}
		require.NoError(t, json.Unmarshal(raw, &data))
		require.Len(t, data.Bindings, 2)
		assert.Equal(t, "alpha", data.Bindings[0].GlobalRoleName)
		assert.Equal(t, "grb-alpha-created", data.Bindings[0].Name)
		assert.Equal(t, "beta", data.Bindings[1].GlobalRoleName)
		assert.Contains(t, res.Text, "global role: alpha (binding grb-alpha-created)")
		assert.Contains(t, res.Text, "global role: beta (binding grb-beta-created)")
		assert.Equal(t, "UserCreated", res.Kind)
	})

	t.Run("no bindings is an empty list", func(t *testing.T) {
		t.Parallel()
		client := &fakeUserClient{userPreviewName: "u-preview1", userRealName: "u-real2"}
		_, exec, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, planInput("alice"))
		require.NoError(t, err)
		res, err := exec(t.Context())
		require.NoError(t, err)
		raw, err := json.Marshal(res.Data)
		require.NoError(t, err)
		var data map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &data))
		assert.JSONEq(t, "[]", string(data["globalRoleBindings"]))
		assert.NotContains(t, res.Text, "global role:")
	})
}

// TestCandidateLimitMatchesGrant asserts the number of global roles a
// NOT_FOUND error lists is the number grant lists: checkGlobalRole follows
// grant's role check and keeps its own copy of the limit.
func TestCandidateLimitMatchesGrant(t *testing.T) {
	t.Parallel()
	assert.Equal(t, rbac.CandidateLimit, candidateLimit)
}
