package commands

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuthCheckUsesInjectedPreflight exercises Execute with a stub preflight so
// the command's result-shaping and error propagation are covered without a
// live server.
func TestAuthCheckUsesInjectedPreflight(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, pf func(context.Context, config.Effective) (string, error)) (runtime.Result, error) {
		t.Helper()
		dir := t.TempDir()
		cmd := &authCheckCmd{dir: dir, store: config.NewFileStore(dir), preflight: pf}
		return cmd.Execute(context.Background(), args{}, ui.Discard())
	}

	t.Run("success returns an AuthCheck result", func(t *testing.T) {
		t.Parallel()
		res, err := run(t, func(context.Context, config.Effective) (string, error) {
			return "v1.31.0", nil
		})
		require.NoError(t, err)
		assert.Equal(t, "AuthCheck", res.Kind)
		assert.Equal(t, "v1.31.0", res.Data.(map[string]any)["serverVersion"])
	})

	t.Run("auth failure propagates the AUTH code", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, func(context.Context, config.Effective) (string, error) {
			return "", clierr.New(clierr.CodeAuth, "bad token")
		})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
	})

	t.Run("unreachable server propagates GENERIC retryable", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, func(context.Context, config.Effective) (string, error) {
			return "", clierr.New(clierr.CodeGeneric, "cannot reach server").WithRetryable(true)
		})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable)
	})
}

func TestAuthCheckSurfacesCredentialStorePin(t *testing.T) {
	t.Setenv("RANCHER_CREDENTIAL_STORE", "") // pin the effective policy to config/auto
	dir := t.TempDir()
	require.NoError(t, config.WritePin(dir, "file"))
	cmd := &authCheckCmd{
		dir:       dir,
		store:     config.NewFileStore(dir),
		preflight: func(context.Context, config.Effective) (string, error) { return "v1.31.0", nil },
	}
	res, err := cmd.Execute(context.Background(), args{}, ui.Discard())
	require.NoError(t, err)

	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	cs, ok := data["credentialStore"].(map[string]any)
	require.True(t, ok, "result carries credentialStore")
	assert.Equal(t, "auto", cs["policy"])
	assert.Equal(t, "file", cs["pinned"])
}

// TestAuthCheckRancherVersionSupportPayload pins PS-8's additive auth-check
// payload fields: rancherVersion and supported, alongside (never replacing)
// the existing serverVersion k8s-discovery key.
func TestAuthCheckRancherVersionSupportPayload(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, rv func(context.Context, config.Effective) (string, error)) (runtime.Result, error) {
		t.Helper()
		dir := t.TempDir()
		cmd := &authCheckCmd{
			dir:   dir,
			store: config.NewFileStore(dir),
			preflight: func(context.Context, config.Effective) (string, error) {
				return "v1.31.0", nil
			},
			rancherVersion: rv,
		}
		return cmd.Execute(context.Background(), args{}, ui.Discard())
	}

	t.Run("above floor: supported true, no warning, both keys present", func(t *testing.T) {
		t.Parallel()
		res, err := run(t, func(context.Context, config.Effective) (string, error) {
			return "v2.15.0", nil
		})
		require.NoError(t, err)
		data := res.Data.(map[string]any)
		assert.Equal(t, "v1.31.0", data["serverVersion"], "the k8s discovery key must stay untouched")
		rv, ok := data["rancherVersion"].(*string)
		require.True(t, ok)
		require.NotNil(t, rv)
		assert.Equal(t, "v2.15.0", *rv)
		supported, ok := data["supported"].(*bool)
		require.True(t, ok)
		require.NotNil(t, supported)
		assert.True(t, *supported)
		assert.Empty(t, res.Warnings)
	})

	t.Run("below floor: supported false plus a warning", func(t *testing.T) {
		t.Parallel()
		res, err := run(t, func(context.Context, config.Effective) (string, error) {
			return "v2.13.0", nil
		})
		require.NoError(t, err)
		data := res.Data.(map[string]any)
		supported := data["supported"].(*bool)
		require.NotNil(t, supported)
		assert.False(t, *supported)
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], "below the supported minimum 2.14")
	})

	t.Run("unparseable dev build: supported nil, cannot-determine warning, check still succeeds", func(t *testing.T) {
		t.Parallel()
		res, err := run(t, func(context.Context, config.Effective) (string, error) {
			return "dev", nil
		})
		require.NoError(t, err)
		data := res.Data.(map[string]any)
		assert.Nil(t, data["supported"].(*bool))
		require.Len(t, res.Warnings, 1)
		assert.Equal(t, config.UnknownServerVersionWarning, res.Warnings[0])
	})

	t.Run("RBAC-denied version read never fails the check: both keys nil plus the same warning", func(t *testing.T) {
		t.Parallel()
		res, err := run(t, func(context.Context, config.Effective) (string, error) {
			return "", clierr.New(clierr.CodeRBAC, "settings.management.cattle.io is forbidden")
		})
		require.NoError(t, err, "a working login must never be turned into a failure by the version probe")
		data := res.Data.(map[string]any)
		assert.Nil(t, data["rancherVersion"].(*string))
		assert.Nil(t, data["supported"].(*bool))
		require.Len(t, res.Warnings, 1)
		assert.Equal(t, config.UnknownServerVersionWarning, res.Warnings[0])
	})

	t.Run("missing setting is folded into the same unknown posture, not an error", func(t *testing.T) {
		t.Parallel()
		res, err := run(t, func(context.Context, config.Effective) (string, error) {
			return "", clierr.New(clierr.CodeNotFound, "settings.management.cattle.io \"server-version\" not found")
		})
		require.NoError(t, err)
		require.Len(t, res.Warnings, 1)
		assert.Equal(t, config.UnknownServerVersionWarning, res.Warnings[0])
	})
}

func TestAuthCheckHitsServerVersion(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/k8s/clusters/local/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"major":"1","minor":"31","gitVersion":"v1.31.0"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken("acme", "id:secret"))
	// Extract the test server's CA cert so RESTConfig can verify it (secure-by-default).
	cert := srv.Certificate()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	cfg := &config.Config{
		CurrentContext: "acme",
		Contexts:       map[string]config.Context{"acme": {Server: "acme", Cluster: "local"}},
		Servers:        map[string]config.Server{"acme": {URL: srv.URL, CA: string(certPEM)}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	res, err := NewAuthCheck(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "AuthCheck", res.Kind)
}
