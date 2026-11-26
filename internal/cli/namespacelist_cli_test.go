package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ownerKubernetesNamespaces = "/k8s/clusters/c-abc/api/v1/namespaces"
	ownerSteveNamespaces      = "/k8s/clusters/c-abc/v1/namespaces"
)

// newProjectOwnerHarness is a facade for a caller whom the Kubernetes API
// refuses the list of every namespace and the get of a namespace the caller has
// no permission on, and who gets team-a by name. Steve lists the two namespaces
// of project p-1 (web), one of project p-2, and seen-only, which the caller
// cannot get and Steve returns with a few fields only.
func newProjectOwnerHarness(t *testing.T) *callerHarness {
	t.Helper()
	return newCallerHarnessWith(t, false, func(o *testrancher.Options) {
		const created = "2026-11-24T09:00:00Z"
		o.NamespaceVisibility = func(b, _ string) ([]testrancher.NamespaceFixture, bool) {
			return []testrancher.NamespaceFixture{
				{Name: "team-a", Project: "p-1", CreationTimestamp: created, Labels: map[string]string{"team": "a"}},
				{Name: "team-b", Project: "p-1", CreationTimestamp: created, Labels: map[string]string{"team": "b"}},
				{Name: "other", Project: "p-2", CreationTimestamp: created},
				{Name: "seen-only", WithoutGet: true},
			}, b == callerBearer
		}
		next := o.Fallthrough
		o.Fallthrough = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name, isNamespace := strings.CutPrefix(r.URL.Path, ownerKubernetesNamespaces)
			name = strings.TrimPrefix(name, "/")
			if !isNamespace || r.Method != http.MethodGet || strings.Contains(name, "/") {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if name == "team-a" {
				_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Namespace",
					"metadata": map[string]any{"name": "team-a", "resourceVersion": "5", "creationTimestamp": created,
						"labels": map[string]any{"kubernetes.io/metadata.name": "team-a"}},
					"status": map[string]any{"phase": "Active"}})
				return
			}
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{}, "status": "Failure",
				"message": `namespaces is forbidden: User "u-member" cannot list resource "namespaces" in API group "" at the cluster scope`, "reason": "Forbidden", "code": 403})
		})
	})
}

// namespaceNames returns the names of the namespaces in the stdout of a get
// run with -o json: the envelope's data, or the data of an object list.
func namespaceNames(t *testing.T, stdout string) []string {
	t.Helper()
	i := strings.Index(stdout, "{")
	require.GreaterOrEqual(t, i, 0, stdout)
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout[i:]), &env), stdout)
	var items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(env.Data, &items); err != nil {
		var one struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		require.NoError(t, json.Unmarshal(env.Data, &one), string(env.Data))
		return []string{one.Metadata.Name}
	}
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.Metadata.Name
	}
	return out
}

// TestGetNamespacesAProjectOwnerThroughTheFacade asserts the commands a
// project owner runs end to end: the list as objects, as a text table, under
// --project and under --selector, one namespace the owner may get, and two the
// owner may not (one Steve lists with a few fields only, one it does not
// list); each reads from Steve after the Kubernetes API refused, with the
// warnings of such a read, and sends the Steve read to the cluster the
// Kubernetes list went to.
// Not parallel: it sets the process environment.
func TestGetNamespacesAProjectOwnerThroughTheFacade(t *testing.T) {
	h := newProjectOwnerHarness(t)
	const refusal = "the Kubernetes API refused the list of every namespace (403); showing the namespaces the Steve API lists for you; list the projects you can see with "

	run := h.run(t, []string{"get", "namespaces", "--cluster", "prod", "-o", "json"})
	assert.Nil(t, run.err, run.stderr)
	assert.Equal(t, []string{"other", "seen-only", "team-a", "team-b"}, namespaceNames(t, run.stdout))
	require.Len(t, run.warnings, 2, "%v", run.warnings)
	assert.True(t, strings.HasPrefix(run.warnings[0], refusal+"rancher"), run.warnings[0])
	assert.True(t, strings.HasSuffix(run.warnings[0], " access projects --cluster 'c-abc'"), run.warnings[0])
	assert.Contains(t, run.warnings[0], "; list the projects you can see with rancher")
	assert.Equal(t, "1 namespace is shown with only the fields the Steve API returns for a namespace you have no permission to get", run.warnings[1])
	paths := []string{}
	for _, q := range run.requests {
		if strings.HasPrefix(q.path, "/k8s/clusters/c-abc/") && (strings.Contains(q.path, "/namespaces") && !strings.Contains(q.path, "/selfsubject")) {
			paths = append(paths, q.method+" "+q.path)
		}
	}
	assert.Equal(t, []string{"GET " + ownerKubernetesNamespaces, "GET " + ownerSteveNamespaces}, paths)

	run = h.run(t, []string{"get", "namespaces", "--cluster", "prod"})
	assert.Nil(t, run.err, run.stderr)
	lines := strings.Split(strings.TrimSpace(run.stdout), "\n")
	require.Len(t, lines, 5, "the header and one row for each of the four namespaces: %q", run.stdout)
	assert.Equal(t, []string{"NAME", "STATUS", "AGE"}, strings.Fields(lines[0]))
	for i, name := range []string{"other", "seen-only", "team-a", "team-b"} {
		cells := strings.Fields(lines[i+1])
		assert.Equal(t, []string{name, "Active"}, cells[:2], "the name and status cells of row %d", i)
		if name == "seen-only" {
			assert.Len(t, cells, 2, "Steve returns no creation time for it, so its age is blank")
		} else {
			assert.Len(t, cells, 3, "%s has an age", name)
		}
	}
	assert.Contains(t, strings.Join(run.warnings, "\n")+run.stderr, "the Kubernetes API refused the list of every namespace (403)")

	run = h.run(t, []string{"get", "namespaces", "--cluster", "prod", "--project", "web", "-o", "json"})
	assert.Nil(t, run.err, run.stderr)
	assert.Equal(t, []string{"team-a", "team-b"}, namespaceNames(t, run.stdout))
	assert.Empty(t, run.warnings, "the list for --project is the project's namespaces")

	run = h.run(t, []string{"get", "namespaces", "--cluster", "prod", "--selector", "team=a", "-o", "json"})
	assert.Nil(t, run.err, run.stderr)
	assert.Equal(t, []string{"team-a"}, namespaceNames(t, run.stdout))
	assert.Contains(t, run.warnings, "1 namespace is left out of the list: the Steve API returns only a few fields for a namespace you have no permission to get, and --selector needs its labels")

	run = h.run(t, []string{"get", "namespace", "team-a", "--cluster", "prod", "-o", "json"})
	assert.Nil(t, run.err, run.stderr)
	assert.Equal(t, []string{"team-a"}, namespaceNames(t, run.stdout))
	assert.Empty(t, run.warnings)
	for _, q := range run.requests {
		assert.NotEqual(t, ownerSteveNamespaces+"/team-a", q.path, "a namespace the Kubernetes API returns is not read through Steve")
	}

	for _, name := range []string{"seen-only", "nobody-sees-this"} {
		run = h.run(t, []string{"get", "namespace", name, "--cluster", "prod", "-o", "json"})
		require.NotNil(t, run.err, name)
		assert.Equal(t, "RBAC", run.err.Code, "%s: both APIs refuse, and the Kubernetes API's error is returned", name)
		require.ErrorContains(t, errors.New(run.err.Message), `cannot list resource "namespaces"`, name)
		steveGets := 0
		for _, q := range run.requests {
			if q.method == http.MethodGet && q.path == ownerSteveNamespaces+"/"+name {
				steveGets++
			}
		}
		assert.Equal(t, 1, steveGets, "%s: the named get is read through Steve of the cluster the Kubernetes get went to", name)
	}
}
