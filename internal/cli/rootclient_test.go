package cli

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/inspect"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pemFromServer encodes srv's TLS certificate as a CERTIFICATE PEM block -
// the same encoding test/e2e/harness_test.go uses for its TLS shim's CA material.
func pemFromServer(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

// seedRootConfig points the factory at srv with a stored token and returns
// the factory. Uses a TLS test server + its cert as --cacert material so
// the real httpx CA path is exercised.
func seedRootConfig(t *testing.T, srv *httptest.Server) *clientFactory {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(certPath, pemFromServer(t, srv), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
		"currentServer: acme\nservers:\n  acme:\n    url: "+srv.URL+"\n    ca: "+certPath+"\n"), 0o600))
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme"}, "token-abc:secret"))
	return newClientFactory(dir, store)
}

// abortAfterDone models a server that never answers: it waits for the
// client to give up on the request, then aborts the connection instead of
// returning. A handler that returns sends an empty 200, and a client whose
// deadline or cancellation lands in the same instant can decode that body
// instead of seeing its own error.
func abortAfterDone(r *http.Request) {
	<-r.Context().Done()
	panic(http.ErrAbortHandler)
}

func TestRootClientSendsBearerAndCA(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	rc, err := seedRootConfig(t, srv).Root()
	require.NoError(t, err)
	body, err := rc.GetJSON(t.Context(), "/v1-rke2-release/release")
	require.NoError(t, err)
	assert.Contains(t, string(body), `"ok"`)
	assert.Equal(t, "Bearer token-abc:secret", gotAuth)
	assert.Equal(t, "/v1-rke2-release/release", gotPath)
}

func TestRootClientRejectsUnknownCA(t *testing.T) {
	// An unknown CA is a required transport case: TLS verification must fail.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	// Seed config WITHOUT the server's cert as CA (self-signed unknown CA).
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
		"currentServer: acme\nservers:\n  acme:\n    url: "+srv.URL+"\n"), 0o600))
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme"}, "t:s"))
	rc, err := newClientFactory(dir, store).Root()
	require.NoError(t, err)
	_, err = rc.GetJSON(t.Context(), "/x")
	require.Error(t, err, "unknown CA must fail TLS verification, not fall back")
}

func TestRootClientTimesOut(t *testing.T) {
	// Timeout case. Server hangs past the client deadline; the
	// test injects a short timeout via the factory seam rather than waiting
	// out the 30s default - a package-var/option seam for the timeout
	// (mirroring keyringProbeTimeout's test-shrinkable pattern in
	// internal/config/credstore.go). Not run with t.Parallel(): it mutates
	// the shared rootTimeout package var, and this package's other
	// t.Parallel() tests must not observe a shrunk timeout mid-run.
	hang := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { <-hang }))
	defer func() { close(hang); srv.Close() }()
	f := seedRootConfig(t, srv)
	restore := setRootTimeoutForTest(t, 100*time.Millisecond)
	defer restore()
	rc, err := f.Root()
	require.NoError(t, err)
	_, err = rc.GetJSON(t.Context(), "/slow")
	require.Error(t, err)
}

func TestRootClientMapsStatusToClierr(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gone":
			http.Error(w, "nope", http.StatusNotFound)
		case "/denied":
			http.Error(w, "nope", http.StatusUnauthorized)
		case "/forbidden":
			http.Error(w, "nope", http.StatusForbidden)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	rc, err := seedRootConfig(t, srv).Root()
	require.NoError(t, err)
	var ce *clierr.Error

	_, err = rc.GetJSON(t.Context(), "/gone")
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)

	_, err = rc.GetJSON(t.Context(), "/denied")
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.Equal(t, provider.AuthStatusRemedy, ce.Hint, "a refused credential names the status check")

	_, err = rc.GetJSON(t.Context(), "/forbidden")
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.Empty(t, ce.Hint, "a 403 is not a credential problem")

	_, err = rc.GetJSON(t.Context(), "/boom")
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "boom", "generic error carries a body excerpt")
}

// argsInput is a minimal runtime.Input for driving a command's Execute
// directly, outside the urfave adapter.
type argsInput struct {
	args  []string
	bools map[string]bool
}

func (i argsInput) String(string) string    { return "" }
func (i argsInput) Bool(k string) bool      { return i.bools[k] }
func (i argsInput) Strings(string) []string { return nil }
func (i argsInput) IsSet(string) bool       { return false }
func (i argsInput) Args() []string          { return i.args }

// TestInspectClusterCancellationThroughProductionAdapters drives inspect
// cluster's Execute through the REAL per-invocation clientFactory - the real
// rootClient for the steve management Cluster GET and the registration
// fetch, and the real provider client for the provisioning LIST - against
// one httptest server. Both
// production adapters rebuild their errors from strings (provider.mapErr;
// rootClient.GetJSON's clierr formatting), so the context sentinel never
// survives the chain: this asserts that a canceled optional lookup still
// propagates as the command's error, degrades nothing to a warning, and
// issues no follow-on request (counted server-side).
func TestInspectClusterCancellationThroughProductionAdapters(t *testing.T) {
	const (
		mgmtGetPath  = "/v1/management.cattle.io.clusters/local"
		provListPath = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters"
		regPath      = "/v1/management.cattle.io.clusterregistrationtokens/local/default-token"
	)
	const mgmtBody = `{"id":"local","type":"management.cattle.io.cluster","metadata":{"name":"local"},"spec":{"displayName":"local"},"status":{"state":"active"}}`
	const provListBody = `{"apiVersion":"provisioning.cattle.io/v1","kind":"ClusterList","metadata":{"resourceVersion":"1"},"items":[]}`

	t.Run("canceled provisioning LIST propagates and no registration request follows", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var regRequests atomic.Int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case mgmtGetPath:
				_, _ = w.Write([]byte(mgmtBody))
			case provListPath:
				// Abort the caller mid-LIST; the client tears the request
				// down, which unblocks the handler via its request context.
				cancel()
				abortAfterDone(r)
			case regPath:
				regRequests.Add(1)
				http.NotFound(w, r)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		f := seedRootConfig(t, srv)
		cmd, ok := inspect.NewInspectCluster(f, f).(runtime.ReadCommand)
		require.True(t, ok, "inspect cluster must be a ReadCommand")
		res, err := cmd.Execute(ctx, argsInput{args: []string{"local"}}, nil)
		require.Error(t, err, "a canceled provisioning LIST must propagate, not degrade")
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce, "the structured error propagates")
		assert.Empty(t, res.Warnings, "cancellation is the command's error, never a section warning")
		assert.Zero(t, regRequests.Load(), "no registration request follows a canceled provisioning lookup")
	})

	t.Run("canceled registration fetch propagates through the root client", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		// Pass --registration so the fetch is attempted and the cancellation
		// gate is exercised (without the flag the structural gate prevents the
		// fetch and the test would never see the error).
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case mgmtGetPath:
				_, _ = w.Write([]byte(mgmtBody))
			case provListPath:
				_, _ = w.Write([]byte(provListBody))
			case regPath:
				cancel()
				abortAfterDone(r)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		f := seedRootConfig(t, srv)
		cmd, ok := inspect.NewInspectCluster(f, f).(runtime.ReadCommand)
		require.True(t, ok, "inspect cluster must be a ReadCommand")
		res, err := cmd.Execute(ctx, argsInput{args: []string{"local"}, bools: map[string]bool{"registration": true}}, nil)
		require.Error(t, err, "a canceled registration fetch must propagate, not degrade")
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce, "the structured error propagates")
		assert.Empty(t, res.Warnings, "cancellation is the command's error, never a section warning")
	})
}

// TestInspectClusterRootTimeoutDegradesRegistration asserts the OTHER side of
// the cancellation boundary through the production root client: the root
// http.Client's own request timeout (rootTimeout) expiring while the CALLER's
// context is still active is an ordinary availability failure, not a
// cancellation - the command succeeds, only the registration section is
// omitted, and the warning carries the deadline wording (so agents can tell
// timeout from RBAC). Not run with t.Parallel(): it mutates the shared
// rootTimeout package var via setRootTimeoutForTest.
func TestInspectClusterRootTimeoutDegradesRegistration(t *testing.T) {
	const (
		mgmtGetPath  = "/v1/management.cattle.io.clusters/local"
		provListPath = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters"
		regPath      = "/v1/management.cattle.io.clusterregistrationtokens/local/default-token"
		crtbListPath = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/local/clusterroletemplatebindings"
		apisDiscPath = "/k8s/clusters/local/apis"
		machListPath = "/k8s/clusters/local/apis/cluster.x-k8s.io/v1beta1/namespaces/fleet-default/machines"
	)
	const mgmtBody = `{"id":"local","type":"management.cattle.io.cluster","metadata":{"name":"local"},"spec":{"displayName":"local"},"status":{"state":"active"}}`
	const provListBody = `{"apiVersion":"provisioning.cattle.io/v1","kind":"ClusterList","metadata":{"resourceVersion":"1"},"items":[{"apiVersion":"provisioning.cattle.io/v1","kind":"Cluster","metadata":{"name":"local-prov","namespace":"fleet-default"},"spec":{},"status":{"clusterName":"local","ready":true}}]}`

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case mgmtGetPath:
			_, _ = w.Write([]byte(mgmtBody))
		case provListPath:
			_, _ = w.Write([]byte(provListBody))
		case regPath:
			// Outlast the root client's (shrunk) timeout; the client aborts
			// the request at the deadline, which unblocks the handler via
			// the server-side request context.
			abortAfterDone(r)
		case crtbListPath:
			// Return an empty crtb list so members lookup succeeds silently
			// and does not add a second warning to the registration-timeout case.
			_, _ = w.Write([]byte(`{"apiVersion":"management.cattle.io/v3","kind":"ClusterRoleTemplateBindingList","items":[]}`))
		case apisDiscPath:
			// Return a minimal API group list so the machines lookup can resolve
			// the cluster.x-k8s.io group version (empty GVR → discovery) without
			// adding a warning to the registration-timeout case.
			_, _ = w.Write([]byte(`{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"cluster.x-k8s.io","versions":[{"groupVersion":"cluster.x-k8s.io/v1beta1","version":"v1beta1"}],"preferredVersion":{"groupVersion":"cluster.x-k8s.io/v1beta1","version":"v1beta1"}}]}`))
		case machListPath:
			// Return an empty machine list so the machines section succeeds
			// silently and does not add a warning to the registration-timeout case.
			_, _ = w.Write([]byte(`{"apiVersion":"cluster.x-k8s.io/v1beta1","kind":"MachineList","metadata":{},"items":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	f := seedRootConfig(t, srv)
	restore := setRootTimeoutForTest(t, 200*time.Millisecond)
	defer restore()

	cmd, ok := inspect.NewInspectCluster(f, f).(runtime.ReadCommand)
	require.True(t, ok, "inspect cluster must be a ReadCommand")
	// Pass --registration so the fetch is attempted and the timeout path is
	// exercised (without the flag the structural gate prevents the fetch and
	// no warning would be issued).
	res, err := cmd.Execute(t.Context(), argsInput{args: []string{"local"}, bools: map[string]bool{"registration": true}}, nil)
	require.NoError(t, err, "an adapter-owned timeout with an active caller context degrades, never errors")

	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "registration unavailable")
	assert.Contains(t, strings.ToLower(res.Warnings[0]), "deadline",
		"the warning carries the adapter's timeout wording so an agent can tell timeout from RBAC")

	b, merr := json.Marshal(res.Data)
	require.NoError(t, merr)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	_, hasReg := doc["registration"]
	assert.False(t, hasReg, "registration must be ABSENT from the wire on the timed-out read")
	_, hasStatus := doc["status"]
	assert.True(t, hasStatus, "the provisioning section is independent of the registration timeout")
}

// setRootTimeoutForTest shrinks the rootClient HTTP timeout for the duration
// of a test, mirroring internal/config/credstore.go's keyringProbeTimeout
// test-shrinkable pattern. Must be called before the factory's first Root()
// (the timeout is baked into the memoized http.Client at construction).
func setRootTimeoutForTest(t *testing.T, d time.Duration) (restore func()) {
	t.Helper()
	orig := rootTimeout
	rootTimeout = d
	return func() { rootTimeout = orig }
}
