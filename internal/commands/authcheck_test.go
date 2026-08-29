package commands

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
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
		dir := seedAuthedDir(t)
		cmd := &authCheckCmd{dir: dir, store: config.NewFileStore(dir), preflight: pf}
		return cmd.Execute(t.Context(), args{}, ui.Discard())
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
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "") // pin the effective policy to config/auto
	dir := seedAuthedDir(t)
	require.NoError(t, config.WritePin(dir, "file"))
	cmd := &authCheckCmd{
		dir:       dir,
		store:     config.NewFileStore(dir),
		preflight: func(context.Context, config.Effective) (string, error) { return "v1.31.0", nil },
	}
	res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
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
		dir := seedAuthedDir(t)
		cmd := &authCheckCmd{
			dir:   dir,
			store: config.NewFileStore(dir),
			preflight: func(context.Context, config.Effective) (string, error) {
				return "v1.31.0", nil
			},
			rancherVersion: rv,
		}
		return cmd.Execute(t.Context(), args{}, ui.Discard())
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

	t.Run("failed endpoint read never fails the check: both keys nil plus the same warning", func(t *testing.T) {
		t.Parallel()
		res, err := run(t, func(context.Context, config.Effective) (string, error) {
			return "", fmt.Errorf("/rancherversion: unexpected status 503")
		})
		require.NoError(t, err, "a working login must never be turned into a failure by the version probe")
		data := res.Data.(map[string]any)
		assert.Nil(t, data["rancherVersion"].(*string))
		assert.Nil(t, data["supported"].(*bool))
		require.Len(t, res.Warnings, 1)
		assert.Equal(t, config.UnknownServerVersionWarning, res.Warnings[0])
	})

	t.Run("unreachable version endpoint is folded into the same unknown posture, not an error", func(t *testing.T) {
		t.Parallel()
		res, err := run(t, func(context.Context, config.Effective) (string, error) {
			return "", fmt.Errorf("dial tcp: connection refused")
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

	dir := seedAuthedDir(t)
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme"}, "id:secret"))
	// Extract the test server's CA cert so RESTConfig can verify it (secure-by-default).
	cert := srv.Certificate()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	cfg := &config.Config{
		CurrentServer: "acme",
		Servers:       map[string]config.Server{"acme": {URL: srv.URL, CA: string(certPEM)}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	res, err := NewAuthCheck(dir, store).(runtime.ReadCommand).Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "AuthCheck", res.Kind)
}

// alwaysOKPreflight and alwaysOKVersion stand in for the preflight/rancherVersion
// probes in the PS-2 identity/token tests below, which exercise the two new
// probes and have nothing to do with PS-8's floor check.
func alwaysOKPreflight(context.Context, config.Effective) (string, error) { return "v1.31.0", nil }
func alwaysOKVersion(context.Context, config.Effective) (string, error)   { return "v2.15.0", nil }

// writeAuthCheckConfig gives Effective() a resolvable current context/server
// (needed only when a test cares about the STORED token, e.g. proving env
// wins over it; every other PS-2 test below sets RANCHER_CLI_TOKEN directly and
// skips this, matching TestAuthCheckUsesInjectedPreflight's config-free style).
func writeAuthCheckConfig(t *testing.T, dir string) {
	t.Helper()
	cfg := &config.Config{
		CurrentServer: "acme",
		Servers:       map[string]config.Server{"acme": {URL: "https://example.invalid"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
}

// TestAuthCheckIdentityAndTokenHappyPath pins PS-2's additive payload: both
// probes returning full metadata decode into the AuthCheck envelope
// alongside (never replacing) PS-8's rancherVersion/supported keys.
func TestAuthCheckIdentityAndTokenHappyPath(t *testing.T) {
	t.Setenv("RANCHER_CLI_TOKEN", "ext/derived-1:secretvalue")
	dir := seedAuthedDir(t)

	var gotName string
	cmd := &authCheckCmd{
		dir: dir, store: config.NewFileStore(dir),
		preflight:      alwaysOKPreflight,
		rancherVersion: alwaysOKVersion,
		identity: func(context.Context, config.Effective) (string, error) {
			return "u-abc123", nil
		},
		token: func(_ context.Context, _ config.Effective, name string) (provider.TokenMetadata, error) {
			gotName = name
			return provider.TokenMetadata{
				Name: name, Kind: "session", Provider: "local",
				ExpiresAt: "2030-01-01T00:00:00Z", Current: true,
			}, nil
		},
	}
	res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Empty(t, res.Warnings)

	data := res.Data.(map[string]any)
	identity, ok := data["identity"].(map[string]any)
	require.True(t, ok, "identity must be a populated object")
	assert.Equal(t, "u-abc123", identity["userID"])

	assert.Equal(t, "derived-1", gotName, "the probe is called with the id after the ext/ prefix")
	token, ok := data["token"].(map[string]any)
	require.True(t, ok, "token must be a populated object")
	assert.Equal(t, "derived-1", token["name"])
	assert.Equal(t, "session", token["kind"])
	assert.Equal(t, "local", token["provider"])
	assert.Equal(t, "2030-01-01T00:00:00Z", token["expiresAt"])
	assert.Equal(t, true, token["current"])
}

// TestAuthCheckTokenKindLabelsAnythingButSessionAsDerived pins the
// spec.kind display-label rule: "session" (Rancher's own login flow) is the
// only raw value shown verbatim; every other value (including the empty
// string createTokenCmd leaves for CLI-derived tokens) is labeled "derived".
func TestAuthCheckTokenKindLabelsAnythingButSessionAsDerived(t *testing.T) {
	t.Setenv("RANCHER_CLI_TOKEN", "ext/name1:secret")
	dir := seedAuthedDir(t)

	cmd := &authCheckCmd{
		dir: dir, store: config.NewFileStore(dir),
		preflight:      alwaysOKPreflight,
		rancherVersion: alwaysOKVersion,
		identity:       func(context.Context, config.Effective) (string, error) { return "u-1", nil },
		token: func(_ context.Context, _ config.Effective, name string) (provider.TokenMetadata, error) {
			return provider.TokenMetadata{Name: name, Kind: ""}, nil
		},
	}
	res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)
	token := res.Data.(map[string]any)["token"].(map[string]any)
	assert.Equal(t, "derived", token["kind"])
}

// TestAuthCheckTokenEnvOverrideWinsOverStore proves the token probe is asked
// for metadata on the EFFECTIVE token (post RANCHER_CLI_TOKEN override), never
// the stored credential — the two carry different ext ids here so a probe
// call on the wrong one is unambiguous.
func TestAuthCheckTokenEnvOverrideWinsOverStore(t *testing.T) {
	dir := seedAuthedDir(t)
	writeAuthCheckConfig(t, dir)
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme"}, "ext/stored-1:storedsecret"))
	t.Setenv("RANCHER_CLI_TOKEN", "ext/env-1:envsecret")

	var gotName string
	cmd := &authCheckCmd{
		dir: dir, store: store,
		preflight:      alwaysOKPreflight,
		rancherVersion: alwaysOKVersion,
		identity:       func(context.Context, config.Effective) (string, error) { return "u-1", nil },
		token: func(_ context.Context, _ config.Effective, name string) (provider.TokenMetadata, error) {
			gotName = name
			return provider.TokenMetadata{Name: name}, nil
		},
	}
	res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "env-1", gotName, "the env override, not the store, must drive the token probe")
	token := res.Data.(map[string]any)["token"].(map[string]any)
	assert.Equal(t, "env-1", token["name"])
}

// TestAuthCheckNonExtTokenCallsV3ProbeNotExtProbe pins PS-2's v3 fallback: a
// non-ext (management.cattle.io/v3) effective token routes to the v3token probe
// and never touches the ext probe. Here the v3 probe returns RBAC denial, so
// token degrades to null with the RBAC warning — the same posture the ext probe
// uses on the same error code.
func TestAuthCheckNonExtTokenCallsV3ProbeNotExtProbe(t *testing.T) {
	t.Setenv("RANCHER_CLI_TOKEN", "legacy-id:legacysecret")
	dir := seedAuthedDir(t)

	extCalled := false
	cmd := &authCheckCmd{
		dir: dir, store: config.NewFileStore(dir),
		preflight:      alwaysOKPreflight,
		rancherVersion: alwaysOKVersion,
		identity:       func(context.Context, config.Effective) (string, error) { return "u-1", nil },
		token: func(context.Context, config.Effective, string) (provider.TokenMetadata, error) {
			extCalled = true
			return provider.TokenMetadata{}, nil
		},
		v3token: func(_ context.Context, _ config.Effective, _ string) (provider.TokenMetadata, error) {
			return provider.TokenMetadata{}, clierr.New(clierr.CodeRBAC, "tokens.management.cattle.io is forbidden")
		},
	}
	res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.False(t, extCalled, "a non-ext token must never reach the ext probe")
	assert.Nil(t, res.Data.(map[string]any)["token"])
	assert.Contains(t, res.Warnings, "token metadata unavailable: denied by RBAC")
}

// TestAuthCheckNoTokenAtAllIsNotLegacy pins checkToken's distinct empty-token
// case: with no effective token at all (unreachable via a real preflight,
// which would already have failed, but reachable when a test — like this
// one — stubs preflight to bypass that), the token field stays null with NO
// warning at all, never the "legacy token" text a non-empty non-ext token
// gets, and the probe is never called.
func TestAuthCheckNoTokenAtAllIsNotLegacy(t *testing.T) {
	t.Parallel()
	dir := seedAuthedDir(t)

	called := false
	cmd := &authCheckCmd{
		dir: dir, store: config.NewFileStore(dir),
		preflight:      alwaysOKPreflight,
		rancherVersion: alwaysOKVersion,
		identity:       func(context.Context, config.Effective) (string, error) { return "u-1", nil },
		token: func(context.Context, config.Effective, string) (provider.TokenMetadata, error) {
			called = true
			return provider.TokenMetadata{}, nil
		},
	}
	res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.False(t, called, "no token at all must never reach the ext token probe")
	assert.Nil(t, res.Data.(map[string]any)["token"])
	assert.Empty(t, res.Warnings)
}

// TestAuthCheckIdentityDegradation covers PS-2's null+warning posture for
// the identity probe across every non-version failure cause: RBAC denial
// (the expected outcome under a genuinely read-only token, since SelfUser is
// a create-verb request — PS-5's recipe), a timeout, and the ext.cattle.io
// group/resource being absent (pre-2.14, or a served-but-partial install).
// None of these ever fail the check itself.
func TestAuthCheckIdentityDegradation(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		warning string
	}{
		{
			name:    "RBAC denial (read-only token; identity is create-verb)",
			err:     clierr.New(clierr.CodeRBAC, "selfusers.ext.cattle.io is forbidden: user cannot create resource"),
			warning: "identity metadata unavailable: denied by RBAC",
		},
		{
			name:    "timeout",
			err:     context.DeadlineExceeded,
			warning: "identity metadata unavailable: context deadline exceeded",
		},
		{
			name:    "missing group/resource",
			err:     clierr.New(clierr.CodeNotFound, "the server could not find the requested resource"),
			warning: "identity metadata unavailable: ext.cattle.io not served by this server (requires Rancher >= 2.14, the supported minimum)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RANCHER_CLI_TOKEN", "id:secret")
			dir := seedAuthedDir(t)
			cmd := &authCheckCmd{
				dir: dir, store: config.NewFileStore(dir),
				preflight:      alwaysOKPreflight,
				rancherVersion: alwaysOKVersion,
				identity:       func(context.Context, config.Effective) (string, error) { return "", tc.err },
				token: func(context.Context, config.Effective, string) (provider.TokenMetadata, error) {
					return provider.TokenMetadata{}, nil
				},
			}
			res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
			require.NoError(t, err, "an identity-probe failure must never fail the check")
			data := res.Data.(map[string]any)
			assert.True(t, data["ok"].(bool), "ok is unaffected by identity degradation")
			assert.Nil(t, data["identity"])
			assert.Contains(t, res.Warnings, tc.warning)
		})
	}
}

// TestAuthCheckTokenDegradation mirrors TestAuthCheckIdentityDegradation for
// the token probe, independently: an ext-shaped effective token (so the
// probe is actually invoked, not short-circuited by the legacy-token path)
// whose lookup is denied or times out nulls the token field with a naming
// warning and never fails the check. The 404 case is deliberately NOT in
// this table (finding 1): the version probe is stubbed to alwaysOKVersion
// here (supported == true), and that combination is now its own dedicated
// test below rather than one row here, since a 404 alone can't tell
// "group absent" and "object missing within a present group" apart.
func TestAuthCheckTokenDegradation(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		warning string
	}{
		{
			name:    "RBAC denial",
			err:     clierr.New(clierr.CodeRBAC, "tokens.ext.cattle.io is forbidden: user cannot get resource"),
			warning: "token metadata unavailable: denied by RBAC",
		},
		{
			name:    "timeout",
			err:     context.DeadlineExceeded,
			warning: "token metadata unavailable: context deadline exceeded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RANCHER_CLI_TOKEN", "ext/derived-1:secretvalue")
			dir := seedAuthedDir(t)
			cmd := &authCheckCmd{
				dir: dir, store: config.NewFileStore(dir),
				preflight:      alwaysOKPreflight,
				rancherVersion: alwaysOKVersion,
				identity:       func(context.Context, config.Effective) (string, error) { return "u-1", nil },
				token: func(context.Context, config.Effective, string) (provider.TokenMetadata, error) {
					return provider.TokenMetadata{}, tc.err
				},
			}
			res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
			require.NoError(t, err, "a token-probe failure must never fail the check")
			data := res.Data.(map[string]any)
			assert.True(t, data["ok"].(bool), "ok is unaffected by token degradation")
			assert.Nil(t, data["token"])
			assert.Contains(t, res.Warnings, tc.warning)
		})
	}
}

// TestAuthCheckTokenNotFoundBelowFloorIsGroupAbsent pins finding 1's first
// branch: a token-probe 404 when the version probe could NOT confirm the
// server is at or above the ext.cattle.io floor (2.14) keeps the existing
// group-absent wording — the two probes disagreeing here (one says
// below-floor, the other implies the group exists) would be far more
// surprising to a caller than a shared "can't tell" posture.
func TestAuthCheckTokenNotFoundBelowFloorIsGroupAbsent(t *testing.T) {
	t.Setenv("RANCHER_CLI_TOKEN", "ext/derived-1:secretvalue")
	dir := seedAuthedDir(t)
	cmd := &authCheckCmd{
		dir: dir, store: config.NewFileStore(dir),
		preflight: alwaysOKPreflight,
		rancherVersion: func(context.Context, config.Effective) (string, error) {
			return "v2.13.0", nil // below floor: supported == false
		},
		identity: func(context.Context, config.Effective) (string, error) { return "u-1", nil },
		token: func(context.Context, config.Effective, string) (provider.TokenMetadata, error) {
			return provider.TokenMetadata{}, clierr.New(clierr.CodeNotFound, "tokens.ext.cattle.io \"derived-1\" not found")
		},
	}
	res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err, "a token-probe failure must never fail the check")
	data := res.Data.(map[string]any)
	assert.Nil(t, data["token"])
	assert.Contains(t, res.Warnings, "token metadata unavailable: ext.cattle.io not served by this server (requires Rancher >= 2.14, the supported minimum)")
}

// TestAuthCheckTokenNotFoundAboveFloorIsRotatedToken pins finding 1's fix: once
// the version probe has confirmed the server IS at or above the floor, the
// same token-probe 404 can only mean the named token object itself was
// deleted/rotated server-side (a stale RANCHER_CLI_TOKEN or credential-store id)
// — never that ext.cattle.io itself is missing, which a >= 2.14 server
// already contradicts. Before this fix, this case wrongly reused the
// below-floor wording (see the "missing group/resource" row this replaced in
// TestAuthCheckTokenDegradation).
func TestAuthCheckTokenNotFoundAboveFloorIsRotatedToken(t *testing.T) {
	t.Setenv("RANCHER_CLI_TOKEN", "ext/derived-1:secretvalue")
	dir := seedAuthedDir(t)
	cmd := &authCheckCmd{
		dir: dir, store: config.NewFileStore(dir),
		preflight:      alwaysOKPreflight,
		rancherVersion: alwaysOKVersion, // v2.15.0: supported == true
		identity:       func(context.Context, config.Effective) (string, error) { return "u-1", nil },
		token: func(context.Context, config.Effective, string) (provider.TokenMetadata, error) {
			return provider.TokenMetadata{}, clierr.New(clierr.CodeNotFound, "tokens.ext.cattle.io \"derived-1\" not found")
		},
	}
	res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err, "a token-probe failure must never fail the check")
	data := res.Data.(map[string]any)
	assert.Nil(t, data["token"])
	assert.Contains(t, res.Warnings, "token metadata unavailable: stored token not found on the server (revoked or rotated) — re-authenticate with rancher auth login")
}

// TestAuthCheckWarningOrderIsDeterministicDespiteConcurrentProbes pins finding
// 4's ordering contract: the version/identity/token probes now run
// concurrently (errgroup), but the warnings list must always assemble in
// this fixed order regardless of which goroutine happens to finish first.
// Run with -race, this also exercises finding 4's no-shared-mutable-state
// design (each goroutine writes only its own dedicated result variables).
func TestAuthCheckWarningOrderIsDeterministicDespiteConcurrentProbes(t *testing.T) {
	t.Setenv("RANCHER_CLI_TOKEN", "ext/derived-1:secretvalue")
	dir := seedAuthedDir(t)
	cmd := &authCheckCmd{
		dir: dir, store: config.NewFileStore(dir),
		preflight: alwaysOKPreflight,
		rancherVersion: func(context.Context, config.Effective) (string, error) {
			return "v2.13.0", nil
		},
		identity: func(context.Context, config.Effective) (string, error) {
			return "", clierr.New(clierr.CodeRBAC, "selfusers.ext.cattle.io is forbidden")
		},
		token: func(context.Context, config.Effective, string) (provider.TokenMetadata, error) {
			return provider.TokenMetadata{}, clierr.New(clierr.CodeRBAC, "tokens.ext.cattle.io is forbidden")
		},
	}
	for range 20 {
		res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
		require.NoError(t, err)
		require.Len(t, res.Warnings, 3)
		assert.Contains(t, res.Warnings[0], "below the supported minimum 2.14")
		assert.Equal(t, "identity metadata unavailable: denied by RBAC", res.Warnings[1])
		assert.Equal(t, "token metadata unavailable: denied by RBAC", res.Warnings[2])
	}
}

// TestAuthCheckIdentityDegradesUnderReadOnlyRBACToken pins the PS-5
// read-only-recipe note explicitly (not just as one row of the degradation
// table above): SelfUser identity is a create-verb request, so a genuinely
// read-only RBAC token degrades identity to null with the RBAC warning —
// expected, not a bug, and the check still exits 0 with ok unaffected.
func TestAuthCheckIdentityDegradesUnderReadOnlyRBACToken(t *testing.T) {
	t.Setenv("RANCHER_CLI_TOKEN", "id:secret")
	dir := seedAuthedDir(t)
	cmd := &authCheckCmd{
		dir: dir, store: config.NewFileStore(dir),
		preflight:      alwaysOKPreflight,
		rancherVersion: alwaysOKVersion,
		identity: func(context.Context, config.Effective) (string, error) {
			return "", clierr.New(clierr.CodeRBAC, "selfusers.ext.cattle.io is forbidden: user cannot create resource \"selfusers\"")
		},
		token: func(context.Context, config.Effective, string) (provider.TokenMetadata, error) {
			return provider.TokenMetadata{}, nil
		},
	}
	res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.True(t, data["ok"].(bool))
	assert.Nil(t, data["identity"])
	assert.Contains(t, res.Warnings, "identity metadata unavailable: denied by RBAC")
}

// TestAuthCheckV3TokenFoundReturnsMetadata pins the v3 fallback happy path: a
// non-ext effective token whose management.cattle.io/v3 Token object is present
// returns the same five-field shape as the ext probe — name, kind, provider,
// expiresAt, current — mapped 1:1 from the root-level v3 fields.
func TestAuthCheckV3TokenFoundReturnsMetadata(t *testing.T) {
	t.Setenv("RANCHER_CLI_TOKEN", "token-nmfmf:secret")
	dir := seedAuthedDir(t)

	var gotName string
	cmd := &authCheckCmd{
		dir: dir, store: config.NewFileStore(dir),
		preflight:      alwaysOKPreflight,
		rancherVersion: alwaysOKVersion,
		identity:       func(context.Context, config.Effective) (string, error) { return "u-1", nil },
		v3token: func(_ context.Context, _ config.Effective, name string) (provider.TokenMetadata, error) {
			gotName = name
			return provider.TokenMetadata{
				Name: name, Kind: "derived", Provider: "local",
				ExpiresAt: "2026-10-19T14:45:05-04:00", Current: true,
			}, nil
		},
	}
	res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Empty(t, res.Warnings)
	assert.Equal(t, "token-nmfmf", gotName)

	token, ok := res.Data.(map[string]any)["token"].(map[string]any)
	require.True(t, ok, "token must be a populated object")
	assert.Equal(t, "token-nmfmf", token["name"])
	assert.Equal(t, "derived", token["kind"])
	assert.Equal(t, "local", token["provider"])
	assert.Equal(t, "2026-10-19T14:45:05-04:00", token["expiresAt"])
	assert.Equal(t, true, token["current"])
}

// TestAuthCheckV3TokenNotFoundDegradesToNullWithWarning pins the v3 fallback
// 404 path: when the management.cattle.io/v3 Token object is absent (revoked
// or rotated) the token field degrades to null with the "re-authenticate"
// advisory. Unlike the ext 404 disambiguation this warning is unconditional —
// the management API is always present, so 404 unambiguously means a stale id.
func TestAuthCheckV3TokenNotFoundDegradesToNullWithWarning(t *testing.T) {
	t.Setenv("RANCHER_CLI_TOKEN", "token-stale:secret")
	dir := seedAuthedDir(t)

	cmd := &authCheckCmd{
		dir: dir, store: config.NewFileStore(dir),
		preflight:      alwaysOKPreflight,
		rancherVersion: alwaysOKVersion,
		identity:       func(context.Context, config.Effective) (string, error) { return "u-1", nil },
		v3token: func(_ context.Context, _ config.Effective, _ string) (provider.TokenMetadata, error) {
			return provider.TokenMetadata{}, clierr.New(clierr.CodeNotFound, `tokens.management.cattle.io "token-stale" not found`)
		},
	}
	res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Nil(t, res.Data.(map[string]any)["token"])
	assert.Contains(t, res.Warnings, "token metadata unavailable: stored token not found on the server (revoked or rotated) — re-authenticate with rancher auth login")
}

// TestAuthCheckExtTokenProbeUnchanged pins that an ext/ effective token still
// routes to the ext probe only, and the v3token probe is never called — FIX 2
// must not disturb the pre-existing ext path.
func TestAuthCheckExtTokenProbeUnchanged(t *testing.T) {
	t.Setenv("RANCHER_CLI_TOKEN", "ext/derived-1:secretvalue")
	dir := seedAuthedDir(t)

	v3Called := false
	cmd := &authCheckCmd{
		dir: dir, store: config.NewFileStore(dir),
		preflight:      alwaysOKPreflight,
		rancherVersion: alwaysOKVersion,
		identity:       func(context.Context, config.Effective) (string, error) { return "u-1", nil },
		token: func(_ context.Context, _ config.Effective, name string) (provider.TokenMetadata, error) {
			return provider.TokenMetadata{
				Name: name, Kind: "derived", Provider: "local",
				ExpiresAt: "2030-01-01T00:00:00Z", Current: true,
			}, nil
		},
		v3token: func(_ context.Context, _ config.Effective, _ string) (provider.TokenMetadata, error) {
			v3Called = true
			return provider.TokenMetadata{}, nil
		},
	}
	res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.False(t, v3Called, "an ext token must never reach the v3 probe")
	token, ok := res.Data.(map[string]any)["token"].(map[string]any)
	require.True(t, ok, "token must be a populated object")
	assert.Equal(t, "derived-1", token["name"])
}

// TestAuthCheckTextViewHappyPath pins the TTY text view sections and key fields
// for a full-data result: AUTH STATUS, VERSIONS, IDENTITY, TOKEN, CREDENTIAL STORE.
func TestAuthCheckTextViewHappyPath(t *testing.T) {
	t.Parallel()
	ver := "v2.15.0"
	supported := true
	text := renderAuthCheckText(
		true, "my-server", "v1.31.0",
		&ver, &supported,
		map[string]any{"userID": "u-abc"},
		map[string]any{"name": "tok-1", "kind": "derived", "provider": "local", "expiresAt": "2030-01-01T00:00:00Z", "current": true},
		"auto", "file",
	)
	assert.Contains(t, text, "ok: true")
	assert.Contains(t, text, "server: my-server")
	assert.Contains(t, text, "serverVersion: v1.31.0")
	assert.Contains(t, text, "rancherVersion: v2.15.0")
	assert.Contains(t, text, "supported: true")
	assert.Contains(t, text, "userID: u-abc")
	assert.Contains(t, text, "name: tok-1")
	assert.Contains(t, text, "kind: derived")
	assert.Contains(t, text, "provider: local")
	assert.Contains(t, text, "expiresAt: 2030-01-01T00:00:00Z")
	assert.Contains(t, text, "current: true")
	assert.Contains(t, text, "policy: auto")
	assert.Contains(t, text, "pinned: file")
}

// TestAuthCheckTextViewNullFields pins the honest-null convention: nil pointer
// fields render as "-", nil sub-objects render as "<field>: unavailable", and
// plain empty strings (cluster, pinned) also render as "-" — all three
// "nothing here" states use the same marker within this view.
func TestAuthCheckTextViewNullFields(t *testing.T) {
	t.Parallel()
	text := renderAuthCheckText(
		true, "s", "v1.31.0",
		nil, nil,
		nil, nil,
		"keychain", "",
	)
	assert.Contains(t, text, "rancherVersion: -")
	assert.Contains(t, text, "supported: -")
	assert.Contains(t, text, "identity: unavailable")
	assert.Contains(t, text, "token: unavailable")
	assert.Contains(t, text, "pinned: -", "empty pinned renders as dash, not blank")
}

// TestStyleAuthCheckTextStripInvariant pins auth status's bold spans: every
// section header is bold, whole line, and a healthy "ok: true" row stays
// plain — plus the strip invariant and the inactive-style identity.
func TestStyleAuthCheckTextStripInvariant(t *testing.T) {
	t.Parallel()
	text := renderAuthCheckText(true, "my-server", "v1.31.0", nil, nil, nil, nil, "auto", "")
	enabled := style.Style{Enabled: true}

	styled := styleAuthCheckText(text, true, enabled)
	assert.NotEqual(t, text, styled)
	assert.Equal(t, text, style.Strip(styled))
	assert.Equal(t, text, styleAuthCheckText(text, true, style.Style{}), "disabled style returns text unchanged")

	for _, header := range []string{"AUTH STATUS", "VERSIONS", "IDENTITY", "TOKEN", "CREDENTIAL STORE"} {
		assert.Contains(t, styled, "\x1b[1m"+header+"\x1b[0m\n", "%s must be bold, whole line", header)
	}
	assert.NotContains(t, styled, "\x1b[33m", "a healthy ok: true row must not be yellow")
	assert.NotContains(t, styled, "\x1b[1m  ok: true", "ok: true is not bold either")
}

// TestStyleAuthCheckTextAccentsFalseOK pins the yellow accent: only the
// unhealthy "ok: false" row turns yellow, never "ok: true".
func TestStyleAuthCheckTextAccentsFalseOK(t *testing.T) {
	t.Parallel()
	text := renderAuthCheckText(false, "my-server", "v1.31.0", nil, nil, nil, nil, "auto", "")
	enabled := style.Style{Enabled: true}

	styled := styleAuthCheckText(text, false, enabled)
	assert.Equal(t, text, style.Strip(styled))
	assert.Contains(t, styled, "\x1b[33m  ok: false\x1b[0m\n")
}

// TestAuthCheckTextViewWarningsReachStderr drives the real pipeline (runtime.Run's
// Text branch) and pins that warnings still reach stderr as "# warning: ..."
// lines — not stdout — when the text view is active. A non-ext token whose
// v3 lookup 404s (revoked or rotated) is the real-world case: the Text view
// renders on stdout while the re-authenticate advisory arrives only on stderr.
func TestAuthCheckTextViewWarningsReachStderr(t *testing.T) {
	t.Setenv("RANCHER_CLI_TOKEN", "legacy-id:legacysecret")
	dir := seedAuthedDir(t)
	cmd := &authCheckCmd{
		dir: dir, store: config.NewFileStore(dir),
		preflight:      alwaysOKPreflight,
		rancherVersion: alwaysOKVersion,
		identity:       func(context.Context, config.Effective) (string, error) { return "u-1", nil },
		v3token: func(_ context.Context, _ config.Effective, _ string) (provider.TokenMetadata, error) {
			return provider.TokenMetadata{}, clierr.New(clierr.CodeNotFound, `tokens.management.cattle.io "legacy-id" not found`)
		},
	}
	var out, errBuf bytes.Buffer
	_, ce := runtime.Run(t.Context(), cmd, args{},
		runtime.IO{Out: &out, Err: &errBuf, Format: ""}, safety.Binding{})
	require.Nil(t, ce)

	stdout, stderr := out.String(), errBuf.String()
	assert.Contains(t, stdout, "AUTH STATUS")
	assert.NotContains(t, stdout, "# warning:", "warnings must never appear on stdout")
	assert.Contains(t, stderr, "# warning: token metadata unavailable: stored token not found on the server (revoked or rotated) — re-authenticate with rancher auth login")
	assert.NotContains(t, stderr, "AUTH STATUS", "text view must never appear on stderr")
}

// TestAuthCheckDataPayloadUnchanged pins that adding Result.Text does not alter
// the Result.Data map — every JSON key and type must survive byte-identical,
// matching TestContextCurrentTextView's parallel assertion for current-context.
func TestAuthCheckDataPayloadUnchanged(t *testing.T) {
	t.Setenv("RANCHER_CLI_TOKEN", "ext/derived-1:secretvalue")
	dir := seedAuthedDir(t)
	ver := "v2.15.0"
	cmd := &authCheckCmd{
		dir: dir, store: config.NewFileStore(dir),
		preflight:      alwaysOKPreflight,
		rancherVersion: func(context.Context, config.Effective) (string, error) { return ver, nil },
		identity: func(context.Context, config.Effective) (string, error) {
			return "u-abc", nil
		},
		token: func(_ context.Context, _ config.Effective, name string) (provider.TokenMetadata, error) {
			return provider.TokenMetadata{Name: name, Kind: "session", Provider: "local"}, nil
		},
	}
	res, err := cmd.Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)

	data, ok := res.Data.(map[string]any)
	require.True(t, ok, "Data must be map[string]any")

	assert.Equal(t, true, data["ok"])
	_, hasServer := data["server"]
	assert.True(t, hasServer, "server key must be present")
	_, hasSV := data["serverVersion"]
	assert.True(t, hasSV, "serverVersion key must be present")
	rv, hasRV := data["rancherVersion"]
	assert.True(t, hasRV, "rancherVersion key must be present")
	assert.NotNil(t, rv, "rancherVersion must be non-nil with a responding stub")
	_, hasSupported := data["supported"]
	assert.True(t, hasSupported, "supported key must be present")
	identity, hasIdentity := data["identity"]
	assert.True(t, hasIdentity, "identity key must be present")
	assert.NotNil(t, identity, "identity must be populated by the stub")
	token, hasToken := data["token"]
	assert.True(t, hasToken, "token key must be present")
	assert.NotNil(t, token, "token must be populated by the stub")
	cs, hasCS := data["credentialStore"].(map[string]any)
	assert.True(t, hasCS, "credentialStore must be a nested map")
	_, hasPolicy := cs["policy"]
	assert.True(t, hasPolicy, "credentialStore.policy key must be present")
	_, hasPinned := cs["pinned"]
	assert.True(t, hasPinned, "credentialStore.pinned key must be present")
}

// seedAuthedDir returns a config dir carrying a current context and server,
// so authCheckCmd.Execute reaches the injected preflight stub: the
// not-logged-in guard (cfg.ErrNotLoggedIn) fires before preflight on an
// empty dir, which is the correct behavior for a blank machine but not the
// state these result-shaping tests exercise.
func seedAuthedDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers = map[string]config.Server{"default": {URL: "https://rancher.test.invalid"}}
	cfg.CurrentServer = "default"
	require.NoError(t, cfg.Write())
	return dir
}

// TestAuthCheckNotLoggedInNamesKnownServers makes sure the answer is
// state, not plumbing: with server entries in config but no current
// pointer, auth status reports "not logged in" and the hint names the
// re-login shortcut with the stored server name — never the generic
// "no current context and no RANCHER_CLI_URL set" that implies a blank
// machine.
func TestAuthCheckNotLoggedInNamesKnownServers(t *testing.T) {
	dir := seedAuthedDir(t)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.CurrentServer = "" // an empty pointer (auth logout never touches it)
	require.NoError(t, cfg.Write())

	cmd := &authCheckCmd{dir: dir, store: config.NewFileStore(dir),
		preflight: func(context.Context, config.Effective) (string, error) {
			t.Fatal("preflight must not run when not logged in")
			return "", nil
		}}
	_, err = cmd.Execute(t.Context(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
	assert.Contains(t, ce.Message, "not logged in")
	// The hinted command must be complete and valid verbatim: a concrete,
	// shell-quoted name, executable with no substitution.
	assert.Equal(t, "rancher auth login 'default'", ce.Hint)
}
