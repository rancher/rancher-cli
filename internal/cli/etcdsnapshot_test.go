package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetETCDSnapshotDescriptionExplainsCreatedAtAgeAndSizeBytes asserts that
// the schema description of get etcd-snapshot says that CREATED-AT is when the
// snapshot was taken and AGE counts from when Rancher created the object, and
// that SIZE-BYTES is in bytes.
func TestGetETCDSnapshotDescriptionExplainsCreatedAtAgeAndSizeBytes(t *testing.T) {
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	var desc string
	for _, c := range m.Commands {
		if c.Name == "get etcd-snapshot" {
			desc = c.Desc
		}
	}
	assert.Contains(t, desc, "CREATED-AT is when the snapshot was taken; AGE counts from when Rancher created the object.")
	assert.Contains(t, desc, "SIZE-BYTES is in bytes.")
}

// TestGetETCDSnapshotSendsTheSelectorAndTheNamespace asserts that get
// etcd-snapshot lists the etcdsnapshots in the namespace fleet-default of the
// local cluster when no namespace is typed, and in the typed namespace when one
// is, and that it sends --selector to the server as the label selector. Not
// parallel: it sets the process environment and the captured command line.
func TestGetETCDSnapshotSendsTheSelectorAndTheNamespace(t *testing.T) {
	exitEnv(t)
	var mu sync.Mutex
	var requests []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "rke.cattle.io/v1", "kind": "ETCDSnapshotList",
			"metadata": map[string]any{"resourceVersion": "1"},
			"items":    []any{},
		})
	}))
	defer srv.Close()
	dir := grConfigDir(t, srv)

	list := func(t *testing.T, args ...string) string {
		t.Helper()
		mu.Lock()
		requests = nil
		mu.Unlock()
		root := grRoot(t)
		var out, errOut bytes.Buffer
		root.Writer, root.ErrWriter = &out, &errOut
		got := run(t.Context(), root, append([]string{"rancher", "--config", dir, "get", "etcd-snapshot"}, args...))
		require.Nil(t, got.err, "stderr: %s", errOut.String())
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, requests, 1)
		return requests[0]
	}

	const base = "/k8s/clusters/local/apis/rke.cattle.io/v1/namespaces/"
	selector := "rke.cattle.io/cluster-name=c1"

	got := list(t, "-l", selector, "-o", "json")
	u, err := url.Parse(got)
	require.NoError(t, err)
	assert.Equal(t, base+"fleet-default/etcdsnapshots", u.Path)
	assert.Equal(t, selector, u.Query().Get("labelSelector"))

	got = list(t, "--selector", selector, "--namespace", "c-m-abc", "-o", "json")
	u, err = url.Parse(got)
	require.NoError(t, err)
	assert.Equal(t, base+"c-m-abc/etcdsnapshots", u.Path)
	assert.Equal(t, selector, u.Query().Get("labelSelector"))
}
