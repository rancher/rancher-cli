package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

const (
	projectPath  = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-abc/projects/p-xyz"
	bindingsPath = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/p-xyz/projectroletemplatebindings"
)

// bindingVerbArgs is get, patch and delete of the binding prtb-1, each as a
// preview where it writes.
func bindingVerbArgs() map[string][]string {
	return map[string][]string{
		"get":    {"get", "projectroletemplatebinding", "prtb-1"},
		"patch":  {"patch", "projectroletemplatebinding", "prtb-1", "--patch", `[{"op":"add","path":"/metadata/labels","value":{"a":"b"}}]`, "--dry-run"},
		"delete": {"delete", "projectroletemplatebinding", "prtb-1", "--dry-run"},
	}
}

// TestProjectRoleTemplateBindingVerbsReadTheNamespaceFromTheProject asserts a
// get, patch and delete of a projectroletemplatebinding with --project and
// --cluster read the project once and address the binding in the project's
// backing namespace on the local cluster, which is the project id when the
// project's status has none.
func TestProjectRoleTemplateBindingVerbsReadTheNamespaceFromTheProject(t *testing.T) {
	h := newContractHarness(t)

	t.Run("list", func(t *testing.T) {
		r := h.run(t, "get", "projectroletemplatebindings", "--project", "web", "--cluster", "c-abc", "-o", "json")
		require.Nil(t, r.err, "%s", r.stderr)
		assert.Zero(t, r.exit)
		assert.Equal(t, 1, r.count(projectPath), "the project is read once: %v", r.requests)
		assert.Equal(t, 1, r.count(bindingsPath), "the bindings are listed in the project's namespace: %v", r.requests)
		var env struct {
			Data []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(r.stdout), &env))
		require.Len(t, env.Data, 1)
		assert.Equal(t, "prtb-1", env.Data[0].Metadata.Name)
	})

	writes := map[string]string{"get": "", "patch": "PUT", "delete": "DELETE"}
	for verb, args := range bindingVerbArgs() {
		t.Run(verb+" named", func(t *testing.T) {
			r := h.run(t, append(slices.Clone(args), "--project", "web", "--cluster", "c-abc", "-o", "json")...)
			require.Nil(t, r.err, "%s", r.stderr)
			assert.Zero(t, r.exit)
			assert.Equal(t, 1, r.count(projectPath), "the project is read once: %v", r.requests)
			assert.Equal(t, 1, requestsOf(r, "GET", bindingsPath+"/prtb-1"),
				"the binding is read in the project's namespace: %v", r.requests)
			if method := writes[verb]; method != "" {
				assert.Equal(t, 1, requestsOf(r, method, bindingsPath+"/prtb-1"),
					"the %s goes to the project's namespace: %v", verb, r.requests)
			}
			for _, line := range r.requests {
				assert.NotContains(t, line, "/namespaces/web/", "the display name is never a namespace: %v", r.requests)
			}
		})
	}

	t.Run("a namespace that is the backing namespace", func(t *testing.T) {
		r := h.run(t, "get", "projectroletemplatebindings", "--project", "p-xyz", "--cluster", "c-abc", "--namespace", "p-xyz", "-o", "json")
		require.Nil(t, r.err, "%s", r.stderr)
		assert.Equal(t, 1, r.count(bindingsPath), "%v", r.requests)
	})
}

// TestProjectRoleTemplateBindingVerbsRefuseBadClusterProjectAndNamespace asserts
// the refusals of --project, --cluster and --namespace on get, patch and
// delete of a projectroletemplatebinding: code, message, hint and exit code,
// and that no binding is read or written.
func TestProjectRoleTemplateBindingVerbsRefuseBadClusterProjectAndNamespace(t *testing.T) {
	h := newContractHarness(t)
	tests := []struct {
		name     string
		extra    []string
		code     string
		message  string
		hint     string
		requests int
	}{
		{
			name:    "--project without --cluster",
			extra:   []string{"--project", "p-xyz"},
			code:    clierr.CodeValidation,
			message: "--cluster is required with --project; the project with the id or display name 'p-xyz' is on cluster 'c-abc' - pass --cluster 'c-abc'",
			hint:    "rancher access clusters",
		},
		{
			name:    "a project that does not exist",
			extra:   []string{"--project", "nope", "--cluster", "c-abc"},
			code:    clierr.CodeNotFound,
			message: "project not found: 'nope'; it is not among the projects you can read on cluster 'c-abc'; pass a project id or display name",
			hint:    "rancher access projects --cluster 'c-abc'",
		},
		{
			name:    "a project of another cluster",
			extra:   []string{"--project", "p-def", "--cluster", "c-abc"},
			code:    clierr.CodeNotFound,
			message: "project not found: 'p-def'; it is not among the projects you can read on cluster 'c-abc'; pass a project id or display name",
			hint:    "rancher access projects --cluster 'c-abc'",
		},
		{
			name:    "--cluster without --project",
			extra:   []string{"--cluster", "c-abc"},
			code:    clierr.CodeValidation,
			message: "--cluster applies to a projectroletemplatebinding only with --project, where it names the cluster the project is in; pass --project or remove --cluster",
			hint:    "rancher access projects --cluster 'c-abc'",
		},
		{
			name:    "no --namespace and no --project",
			code:    clierr.CodeValidation,
			message: "--namespace, or --project with --cluster, is required when you give the name of one projectroletemplatebinding",
			hint:    "rancher access projects",
		},
		{
			name:    "a namespace that is not the project's",
			extra:   []string{"--project", "web", "--cluster", "c-abc", "--namespace", "other"},
			code:    clierr.CodeValidation,
			message: "--namespace 'other' is not the backing namespace of project 'p-xyz' in cluster 'c-abc'; the backing namespace is 'p-xyz'",
		},
		{
			name:    "an empty --project",
			extra:   []string{"--project", "", "--cluster", "c-abc"},
			code:    clierr.CodeValidation,
			message: "--project is empty; pass a project id or display name, or omit --project",
		},
	}
	for verb, args := range bindingVerbArgs() {
		for _, tc := range tests {
			t.Run(verb+" "+tc.name, func(t *testing.T) {
				r := h.run(t, append(append(slices.Clone(args), tc.extra...), "-o", "json")...)
				require.NotNil(t, r.err, "stdout: %s stderr: %s", r.stdout, r.stderr)
				assert.Equal(t, clierr.ExitCode(tc.code), r.exit)
				assert.Equal(t, tc.code, r.err.Code)
				assert.Equal(t, tc.message, r.err.Message)
				if tc.hint == "" {
					assert.Empty(t, r.err.Hint)
				} else {
					assert.Equal(t, withDirectory(h.cfgDir, tc.hint), r.err.Hint)
				}
				for _, line := range r.requests {
					assert.NotContains(t, line, "projectroletemplatebindings", "no binding is read or written: %v", r.requests)
				}
			})
		}
	}
}

// requestsOf is how many requests of method went to path, whatever their
// query.
func requestsOf(r contractRun, method, path string) int {
	n := 0
	for _, line := range r.requests {
		m, rest, _ := strings.Cut(line, " ")
		if p, _, _ := strings.Cut(rest, "?"); m == method && p == path {
			n++
		}
	}
	return n
}
