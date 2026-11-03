package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testrancher"
)

// userAttributeProxyPaths are the local-proxy paths a read of UserAttributes
// must never take: the v3 collection, and steve's type behind the proxy.
var userAttributeProxyPaths = []string{
	"/k8s/clusters/local/apis/management.cattle.io/v3/userattributes",
	"/k8s/clusters/local/v1/management.cattle.io.userattribute",
}

// userAttributeFacade serves the facade's steve UserAttribute routes at the
// level set on the returned counter, with one object for u-1 caching one
// group, and RBAC lists that name the group, so every command that reads a
// UserAttribute has a reason to.
//
// While impersonate is set, the proxy applies an Impersonate-User header to a
// SelfSubjectReview, as it does for a caller who may impersonate; otherwise it
// drops the header, and a question about another user is a
// SubjectAccessReview.
func userAttributeFacade(level *atomic.Int32, impersonate *atomic.Bool) func(*testrancher.Options) {
	return func(o *testrancher.Options) {
		o.UserAttributeAccess = func(b string) (testrancher.UserAttributeLevel, bool) {
			return testrancher.UserAttributeLevel(level.Load()), b == callerBearer
		}
		o.UserAttributeObjects = func() []map[string]any {
			return []map[string]any{{
				"apiVersion": "management.cattle.io/v3", "kind": "UserAttribute", "metadata": map[string]any{"name": "u-1"},
				"GroupPrincipals": map[string]any{"okta": map[string]any{"Items": []any{
					map[string]any{"metadata": map[string]any{"name": "okta_group://eng"}, "displayName": "Engineering"}}}},
			}}
		}
		inner := o.Fallthrough
		o.Fallthrough = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			list := func(apiVersion, kind string, item map[string]any) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": apiVersion, "kind": kind + "List",
					"metadata": map[string]any{"resourceVersion": "1"}, "items": []any{item}})
			}
			meta := func(name, ns string) map[string]any {
				return map[string]any{"name": name, "namespace": ns, "resourceVersion": "1"}
			}
			switch {
			case impersonate.Load() && r.Method == http.MethodPost && strings.HasSuffix(p, "/authentication.k8s.io/v1/selfsubjectreviews") && r.Header.Get("Impersonate-User") != "":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview",
					"status": map[string]any{"userInfo": map[string]any{"username": r.Header.Get("Impersonate-User"), "groups": []any{"system:authenticated"}}}})
			case r.Method == http.MethodGet && strings.HasSuffix(p, "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings"):
				list("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", map[string]any{
					"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": meta("crb-eng", ""),
					"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "view"},
					"subjects": []any{map[string]any{"kind": "Group", "apiGroup": "rbac.authorization.k8s.io", "name": "okta_group://eng"}},
				})
			case r.Method == http.MethodGet && strings.HasSuffix(p, "/clusterroletemplatebindings") && strings.Contains(p, "/namespaces/c-abc/"):
				list("management.cattle.io/v3", "ClusterRoleTemplateBinding", map[string]any{
					"apiVersion": "management.cattle.io/v3", "kind": "ClusterRoleTemplateBinding", "metadata": meta("crtb-eng", "c-abc"),
					"clusterName": "c-abc", "groupPrincipalName": "okta_group://eng", "roleTemplateName": "cluster-member"})
			case r.Method == http.MethodGet && strings.HasSuffix(p, "/projectroletemplatebindings") && strings.Contains(p, "/namespaces/"):
				list("management.cattle.io/v3", "ProjectRoleTemplateBinding", map[string]any{
					"apiVersion": "management.cattle.io/v3", "kind": "ProjectRoleTemplateBinding", "metadata": meta("prtb-eng", "p-1"),
					"projectName": "c-abc:p-1", "groupPrincipalName": "okta_group://eng", "roleTemplateName": "project-member"})
			default:
				inner.ServeHTTP(w, r)
			}
		})
	}
}

func envelopeOf(t *testing.T, out string) (data map[string]any, warnings []string) {
	t.Helper()
	i := strings.Index(out, "{")
	require.GreaterOrEqual(t, i, 0, out)
	var env struct {
		Data     map[string]any `json:"data"`
		Warnings []string       `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal([]byte(out[i:]), &env), out)
	return env.Data, env.Warnings
}

// uaCheck asserts what one command shows at one level of access, from the
// decoded data and warnings of its envelope.
type uaCheck func(t *testing.T, level testrancher.UserAttributeLevel, data map[string]any, warnings []string)

func anyWarning(warnings []string, fragment string) bool {
	return slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, fragment) })
}

const (
	uaByIDNoType    = "you cannot read userattributes (GET /v1/management.cattle.io.userattributes/%s answered 404); Rancher's API shows you no userattributes type"
	uaCollectNoType = "you cannot list userattributes (GET /v1/management.cattle.io.userattributes answered 404); Rancher's API shows you no userattributes type"
	uaCollectGetNo  = "you cannot list every userattribute (a SelfSubjectAccessReview for list on userattributes.management.cattle.io answered denied)"
	uaRemedyWords   = "a GlobalRole with get and list on userattributes.management.cattle.io allows it"
)

// aliasesFor asserts the groupAliases of an answer about a user whose
// UserAttribute has count groups where it can be read, and is unavailable
// where it cannot.
func aliasesFor(t *testing.T, level testrancher.UserAttributeLevel, data map[string]any, count float64) {
	t.Helper()
	aliases, ok := data["groupAliases"].(map[string]any)
	require.True(t, ok, "groupAliases in %v", data)
	if level == testrancher.UserAttributesNone {
		assert.Equal(t, "unavailable", aliases["status"])
		assert.Nil(t, aliases["count"])
		return
	}
	assert.Equal(t, "available", aliases["status"])
	assert.Equal(t, count, aliases["count"])
}

func aliasCheck(count float64) uaCheck {
	return func(t *testing.T, l testrancher.UserAttributeLevel, d map[string]any, _ []string) {
		t.Helper()
		aliasesFor(t, l, d, count)
	}
}

// membersCheck asserts the group display names of a member list: shown with
// get and list, otherwise missing with a warning that names why.
func membersCheck(t *testing.T, l testrancher.UserAttributeLevel, d map[string]any, w []string) {
	t.Helper()
	const prefix = "could not list userattributes; group display names are not shown: "
	var display any
	rows, _ := d["members"].([]any)
	require.NotEmpty(t, rows, "members in %v", d)
	for _, r := range rows {
		if row := r.(map[string]any); row["principal"] == "okta_group://eng" {
			display = row["displayName"]
		}
	}
	switch l {
	case testrancher.UserAttributesNone:
		assert.Nil(t, display)
		assert.True(t, anyWarning(w, prefix+uaCollectNoType), "%v", w)
	case testrancher.UserAttributesGet:
		assert.Nil(t, display)
		assert.True(t, anyWarning(w, prefix+uaCollectGetNo), "%v", w)
	default:
		assert.Equal(t, "Engineering", display)
		assert.False(t, anyWarning(w, prefix), "%v", w)
	}
}

// TestUserAttributeReadsGoThroughSteveForEveryCommand asserts, for a caller
// the local proxy refuses and each of no right, get, and get and list on
// userattributes, what every command that reads a UserAttribute shows, and
// that none of them sends a UserAttribute request through the local proxy.
func TestUserAttributeReadsGoThroughSteveForEveryCommand(t *testing.T) {
	rows := []struct {
		name        string
		args        string
		impersonate bool
		check       uaCheck
	}{
		{"access can", "access can get pods --cluster prod --namespace default --as u-1 -o json", false, func(t *testing.T, l testrancher.UserAttributeLevel, d map[string]any, w []string) {
			t.Helper()
			aliasesFor(t, l, d, 1)
			assert.Equal(t, "allowed", d["decision"])
			none := l == testrancher.UserAttributesNone
			assert.Equal(t, none, anyWarning(w, fmt.Sprintf(uaByIDNoType, "u-1")), "%v", w)
			assert.Equal(t, none, anyWarning(w, uaRemedyWords), "%v", w)
		}},
		{"access explain", "access explain get pods --cluster prod --namespace default --as u-1 -o json", false, func(t *testing.T, l testrancher.UserAttributeLevel, d map[string]any, w []string) {
			t.Helper()
			aliasesFor(t, l, d, 1)
			var found bool
			for _, i := range d["chainIssues"].([]any) {
				issue := i.(map[string]any)
				if issue["object"] == "userattribute u-1" {
					found = true
					assert.Equal(t, "managementRead", issue["stage"])
					assert.Equal(t, "RBAC", issue["code"])
					assert.Contains(t, issue["message"], fmt.Sprintf(uaByIDNoType, "u-1"))
				}
			}
			assert.Equal(t, l == testrancher.UserAttributesNone, found)
			if l == testrancher.UserAttributesNone {
				assert.NotEqual(t, "complete", d["chain"])
			}
		}},
		{"access who-can", "access who-can get pods --cluster prod --namespace default -o json", false, func(t *testing.T, l testrancher.UserAttributeLevel, d map[string]any, w []string) {
			t.Helper()
			switch l {
			case testrancher.UserAttributesNone:
				assert.True(t, anyWarning(w, "group members not expanded: "+uaCollectNoType), "%v", w)
			case testrancher.UserAttributesGet:
				assert.True(t, anyWarning(w, "group members not expanded: "+uaCollectGetNo), "%v", w)
			default:
				assert.False(t, anyWarning(w, "group members not expanded"), "%v", w)
				var members any
				for _, s := range d["subjects"].([]any) {
					if row := s.(map[string]any); row["name"] == "okta_group://eng" {
						members = row["knownMembers"]
					}
				}
				assert.Equal(t, []any{"u-1"}, members)
			}
		}},
		{"access clusters as", "access clusters --as u-1 -o json", true, aliasCheck(1)},
		{"access clusters", "access clusters -o json", false, aliasCheck(0)},
		{"access projects as", "access projects --cluster prod --as u-1 -o json", true, aliasCheck(1)},
		{"access projects", "access projects --cluster prod -o json", false, aliasCheck(0)},
		{"inspect user", "inspect user u-1 -o json", false, func(t *testing.T, l testrancher.UserAttributeLevel, d map[string]any, w []string) {
			t.Helper()
			_, hasGroups := d["user"].(map[string]any)["groups"]
			if l == testrancher.UserAttributesNone {
				assert.False(t, hasGroups, "the groups are left out when UserAttributes are refused")
				assert.NotEmpty(t, d["degraded"])
				assert.True(t, anyWarning(w, fmt.Sprintf(uaByIDNoType, "u-1")), "%v", w)
				return
			}
			assert.True(t, hasGroups)
			assert.Empty(t, d["degraded"])
		}},
		{"inspect cluster", "inspect cluster prod -o json", false, membersCheck},
		{"inspect project", "inspect project web --cluster prod -o json", false, membersCheck},
	}
	levels := []struct {
		name  string
		level testrancher.UserAttributeLevel
	}{
		{"none", testrancher.UserAttributesNone},
		{"get", testrancher.UserAttributesGet},
		{"get and list", testrancher.UserAttributesGetList},
	}
	for _, lv := range levels {
		t.Run(lv.name, func(t *testing.T) {
			var level atomic.Int32
			var impersonate atomic.Bool
			level.Store(int32(lv.level))
			h := newCallerHarnessWith(t, false, userAttributeFacade(&level, &impersonate))
			for _, row := range rows {
				t.Run(row.name, func(t *testing.T) {
					impersonate.Store(row.impersonate)
					r := h.run(t, strings.Fields(row.args))
					require.Nil(t, r.err, "%s: %s", row.args, r.stdout)
					data, warnings := envelopeOf(t, r.stdout)
					row.check(t, lv.level, data, warnings)
					for _, q := range r.requests {
						for _, prefix := range userAttributeProxyPaths {
							assert.False(t, strings.HasPrefix(q.path, prefix), "%s went through the local proxy: %s", row.args, q.path)
						}
					}
					var steveReads int
					for _, q := range r.requests {
						if strings.HasPrefix(q.path, testrancher.UserAttributesCollectionPath) {
							steveReads++
						}
					}
					assert.Positive(t, steveReads, "%s read no UserAttribute through steve", row.args)
				})
			}
		})
	}
}
