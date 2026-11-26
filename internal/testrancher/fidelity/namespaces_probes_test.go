package fidelity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNamespaceProbeRecordsTheGeneratedProjectNameAsACapture asserts the probe
// reads the member's project before the namespaces, and that a run against a
// server whose project and namespaces carry a generated project name records
// {project} and no literal name, the presence of the creation time and of the
// Steve fields, and the phase.
func TestNamespaceProbeRecordsTheGeneratedProjectNameAsACapture(t *testing.T) {
	t.Parallel()
	var probe Probe
	for _, p := range namespaceProbes() {
		if p.Name == "namespaces-downstream/member-steve-namespaces" {
			probe = p
		}
	}
	require.NotEmpty(t, probe.Name)
	require.True(t, probe.Downstream)
	require.True(t, probe.SubstituteCaptures)
	require.NotEmpty(t, probe.Observations)
	first := probe.Observations[0]
	assert.Equal(t, "/v1/management.cattle.io.projects", first.Request.Path)
	assert.Equal(t, IdentityMember, first.Request.Identity)
	assert.Equal(t, map[string]Capture{"project": {Path: "data.0.metadata.name"}}, first.Captures)
	for _, o := range probe.Observations {
		assert.Equal(t, IdentityMember, o.Request.Identity, o.Name)
		assert.Equal(t, http.MethodGet, o.Request.Method, o.Name)
	}

	const generated = "p-x7k2q"
	namespace := map[string]any{
		"id": "default", "type": "namespace",
		"metadata": map[string]any{
			"name": "default", "resourceVersion": "9", "uid": "u-1", "creationTimestamp": "2026-11-24T09:00:00Z",
			"fields": []any{"default", "Active", "3h"}, "state": map[string]any{"name": "active"},
			"labels":      map[string]any{"kubernetes.io/metadata.name": "default", "field.cattle.io/projectId": generated},
			"annotations": map[string]any{"field.cattle.io/projectId": "c-abc123:" + generated, "note": "x"},
		},
		"spec": map[string]any{"finalizers": []any{"kubernetes"}}, "status": map[string]any{"phase": "Active"},
	}
	mux := http.NewServeMux()
	serve := func(w http.ResponseWriter, body any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
	mux.HandleFunc("GET /v1/management.cattle.io.projects", func(w http.ResponseWriter, _ *http.Request) {
		serve(w, map[string]any{"type": "collection", "revision": "3", "count": 1, "data": []any{map[string]any{
			"id": "c-abc123/" + generated, "type": "management.cattle.io.project", "kind": "Project", "apiVersion": "management.cattle.io/v3",
			"metadata": map[string]any{"name": generated, "namespace": "c-abc123", "generateName": "p-", "finalizers": []any{"controller.cattle.io/mgmt-project-rbac-remove"}},
			"spec":     map[string]any{"clusterName": "c-abc123", "displayName": "Default"},
			"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "lastUpdateTime": "2026-11-24T09:00:00Z"}}},
		}}})
	})
	mux.HandleFunc("GET /k8s/clusters/c-abc123/v1/namespaces", func(w http.ResponseWriter, _ *http.Request) {
		serve(w, map[string]any{"type": "collection", "revision": "4", "count": 1, "data": []any{namespace}})
	})
	mux.HandleFunc("GET /k8s/clusters/c-abc123/v1/namespaces/default", func(w http.ResponseWriter, _ *http.Request) { serve(w, namespace) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityMember: "member"}, downstream: "c-abc123"}

	results, err := RunProbe(t.Context(), tgt, probe)
	require.NoError(t, err)
	encoded, err := json.Marshal(results)
	require.NoError(t, err)
	raw := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(string(encoded))
	assert.NotContains(t, raw, generated, "no literal project name is recorded")
	assert.NotContains(t, raw, "u-1")
	assert.Contains(t, raw, `"field.cattle.io/projectId":"{project}"`)
	assert.Contains(t, raw, `"field.cattle.io/projectId":"{downstream}:{project}"`)
	assert.Contains(t, raw, `"creationTimestamp":"<ts>"`)
	assert.Contains(t, raw, `"fields":"<fields>"`)
	assert.Contains(t, raw, `"phase":"Active"`)
	assert.Contains(t, raw, `"id":"{downstream}/{project}"`)
}
