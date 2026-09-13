package provider

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// hangingServer is a TLS server that holds every request open until the
// request's own context ends, the shape of an unreachable or stalled
// server as seen by a client whose transport has connected.
func hangingServer(t *testing.T) (srv *httptest.Server, caPEM string) {
	t.Helper()
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	return srv, caPEM
}

// cancelSoon returns a context that is cancelled shortly after the call,
// and fails the test if fn has not returned well before the request
// timeout would have ended the request instead.
func cancelSoon(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the request outlived its cancelled context")
		return nil
	}
}

// TestPreflightEndsWithItsContext asserts that cancelling the context
// handed to Preflight ends the version request at once, and that the
// caller's own cancellation is returned as such - never dressed as the
// retryable "cannot reach server" failure.
func TestPreflightEndsWithItsContext(t *testing.T) {
	t.Parallel()
	srv, caPEM := hangingServer(t)
	eff := config.Effective{URL: srv.URL, CA: caPEM, Token: "id:secret"}
	err := cancelSoon(t, func(ctx context.Context) error {
		_, err := Preflight(ctx, eff)
		return err
	})
	require.ErrorIs(t, err, context.Canceled)
	_, structured := errors.AsType[*clierr.Error](err)
	assert.False(t, structured, "cancellation passes through unmapped: %v", err)
	assert.NotContains(t, err.Error(), "cannot reach server")
}

// TestVersionResolutionEndsWithItsContext asserts the same for the API
// group discovery that fills an empty GVR version on the generic client:
// the caller's context ends the request, and the cancellation is returned
// as such.
func TestVersionResolutionEndsWithItsContext(t *testing.T) {
	t.Parallel()
	srv, caPEM := hangingServer(t)
	rc := &rest.Config{Host: srv.URL, BearerToken: "id:secret"}
	rc.CAData = []byte(caPEM)
	c, err := NewClusterClient(rc, "local")
	require.NoError(t, err)
	err = cancelSoon(t, func(ctx context.Context) error {
		_, err := c.List(ctx, schema.GroupVersionResource{Group: "cluster.x-k8s.io", Resource: "machines"}, "fleet-default", "")
		return err
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), "discovering version for group")
}

// discoveryServer serves a JSON /version and an unaggregated /apis document
// under any prefix (Preflight aims at the local cluster's proxy path).
func discoveryServer(t *testing.T) (srv *httptest.Server, caPEM string) {
	t.Helper()
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/version"):
			_, _ = w.Write([]byte(`{"major":"1","minor":"33","gitVersion":"v1.33.4+k3s1"}`))
		case strings.HasSuffix(r.URL.Path, "/apis"):
			_, _ = w.Write([]byte(`{"kind":"APIGroupList","apiVersion":"v1","groups":[
				{"name":"cluster.x-k8s.io","versions":[{"groupVersion":"cluster.x-k8s.io/v1beta2","version":"v1beta2"},{"groupVersion":"cluster.x-k8s.io/v1beta1","version":"v1beta1"}],"preferredVersion":{"groupVersion":"cluster.x-k8s.io/v1beta1","version":"v1beta1"}},
				{"name":"ext.cattle.io","versions":[{"groupVersion":"ext.cattle.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"ext.cattle.io/v1","version":"v1"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	return srv, caPEM
}

// TestPreflightDecodesTheServedVersion asserts that the version document
// read with the caller's context decodes to the same GitVersion the
// discovery client's own call returned.
func TestPreflightDecodesTheServedVersion(t *testing.T) {
	t.Parallel()
	srv, caPEM := discoveryServer(t)
	ver, err := Preflight(t.Context(), config.Effective{URL: srv.URL, CA: caPEM, Token: "id:secret"})
	require.NoError(t, err)
	assert.Equal(t, "v1.33.4+k3s1", ver)
}

// TestServerGroupsDecodesThePreferredVersion asserts that the group list
// read with the caller's context carries the server's preferredVersion for
// a named group, not the first version listed.
func TestServerGroupsDecodesThePreferredVersion(t *testing.T) {
	t.Parallel()
	srv, caPEM := discoveryServer(t)
	rc := &rest.Config{Host: srv.URL, BearerToken: "id:secret"}
	rc.CAData = []byte(caPEM)
	disc, err := kube.Discovery(discoveryConfig(rc))
	require.NoError(t, err)
	groups, err := serverGroups(t.Context(), disc)
	require.NoError(t, err)
	byName := map[string]string{}
	for _, g := range groups.Groups {
		byName[g.Name] = g.PreferredVersion.Version
	}
	assert.Equal(t, map[string]string{"cluster.x-k8s.io": "v1beta1", "ext.cattle.io": "v1"}, byName)
}
