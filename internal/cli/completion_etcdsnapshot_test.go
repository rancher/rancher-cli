package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// etcdSnapshotServer serves the rke.cattle.io/v1 etcdsnapshots list of the
// namespaces fleet-default and c-m-abc on the local management cluster.
func etcdSnapshotServer(t *testing.T) *httptest.Server {
	t.Helper()
	const base = "/k8s/clusters/local/apis/rke.cattle.io/v1/namespaces/"
	mux := http.NewServeMux()
	mux.HandleFunc(base+"fleet-default/etcdsnapshots", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"prod-etcd-snapshot-n1-1700000000","namespace":"fleet-default"}},
			{"metadata":{"name":"prod-etcd-snapshot-n2-1700000001","namespace":"fleet-default"}}
		]}`))
	})
	mux.HandleFunc(base+"c-m-abc/etcdsnapshots", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"imported-etcd-snapshot-n1-1700000002","namespace":"c-m-abc"}}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestETCDSnapshotFullCallback drives the real completion callback through
// BuildRoot with the arguments a shell completion script passes, and asserts
// that get offers the names of the default namespace fleet-default, or of the
// namespace typed with --namespace; that no names are offered with the network
// gate off; and that patch, which is always refused, and delete, which the row
// does not have, offer none. Not parallel: it sets the process environment and
// the captured command line.
func TestETCDSnapshotFullCallback(t *testing.T) {
	srv := etcdSnapshotServer(t)

	setup := func(t *testing.T, networkOn bool) {
		t.Helper()
		dir := t.TempDir()
		cfg := &config.Config{
			CurrentServer: "srv",
			Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:    config.CompletionConfig{Network: networkOn},
		}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		setStoredConfigForTest(t, cfg, dir)
		t.Setenv(config.EnvConfigDir, dir)
		t.Setenv(config.EnvToken, "mgmt-token")
		t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	}

	complete := func(t *testing.T, words ...string) string {
		t.Helper()
		origArgv, origWord, origProto := rawArgv, rawCurrentWord, rawCompleting
		t.Cleanup(func() {
			rawArgv = origArgv
			rawCurrentWord = origWord
			rawCompleting = origProto
		})
		scriptArgs := append([]string{"--rancher-completion-proto=2"}, words...)
		scriptArgs = append(scriptArgs, "--rancher-completion-cur=", "--generate-shell-completion")
		CaptureArgv(scriptArgs)
		root, err := BuildRoot("test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		_ = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
		return out.String()
	}

	t.Run("get lists the names of fleet-default", func(t *testing.T) {
		setup(t, true)
		got := complete(t, "get", "etcd-snapshot")
		assert.Contains(t, got, "prod-etcd-snapshot-n1-1700000000")
		assert.Contains(t, got, "prod-etcd-snapshot-n2-1700000001")
		assert.NotContains(t, got, "imported-etcd-snapshot")
	})

	t.Run("get lists the names of the typed namespace", func(t *testing.T) {
		setup(t, true)
		got := complete(t, "get", "etcd-snapshot", "--namespace", "c-m-abc")
		assert.Contains(t, got, "imported-etcd-snapshot-n1-1700000002")
		assert.NotContains(t, got, "prod-etcd-snapshot")
	})

	t.Run("network gate off offers nothing", func(t *testing.T) {
		setup(t, false)
		assert.Empty(t, complete(t, "get", "etcd-snapshot"))
	})

	t.Run("patch offers no names", func(t *testing.T) {
		setup(t, true)
		got := complete(t, "patch", "etcd-snapshot")
		for _, name := range []string{
			"prod-etcd-snapshot-n1-1700000000",
			"prod-etcd-snapshot-n2-1700000001",
			"imported-etcd-snapshot-n1-1700000002",
		} {
			assert.NotContains(t, got, name, "patch offered %s", name)
		}
	})

	t.Run("delete offers nothing", func(t *testing.T) {
		setup(t, true)
		assert.Empty(t, complete(t, "delete", "etcd-snapshot"))
	})
}
