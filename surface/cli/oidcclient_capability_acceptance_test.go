package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// This file is the real-kind-table acceptance coverage for the capability
// gate, exercised through its first Capability-bearing kind row: oidcclient
// (feature "oidc-provider"). It pins the four
// detector outcomes live through the pipeline on a GENERATED verb (get
// oidcclient), and PRIME_REQUIRED before Plan on `create resource -f` with an
// oidcclient manifest — the coverage rule's gating half going non-vacuous.

// oidcAcceptanceServer serves /rancherversion, the oidc-provider Feature GET,
// and the oidcclients list/get routes, counting target-resource hits
// separately from the capability-detector requests.
func oidcAcceptanceServer(t *testing.T, rancherVersion, featureBody string, featureStatus int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var targetHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/rancherversion", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rancherVersion))
	})
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/features/oidc-provider", func(w http.ResponseWriter, r *http.Request) {
		if featureBody == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(featureStatus)
		_, _ = w.Write([]byte(featureBody))
	})
	targetHandler := func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		grWriteJSON(w, grKubeList(nil))
	}
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/oidcclients", targetHandler)
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/oidcclients/", targetHandler)
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, &targetHits
}

// oidcFeatureObj builds a minimal, wire-realistic Feature JSON body for
// "oidc-provider": status.prime classifies the feature, status.default is
// its effective value absent an explicit spec.value override.
func oidcFeatureObj(prime, defaultVal bool) string {
	return fmt.Sprintf(`{"kind":"Feature","apiVersion":"management.cattle.io/v3",
		"metadata":{"name":"oidc-provider"},
		"status":{"prime":%t,"default":%t}}`, prime, defaultVal)
}

func oidcErrEnvelope(t *testing.T, stdout, stderr []byte) grErrEnvelope {
	t.Helper()
	body := stderr
	if len(body) == 0 {
		body = stdout
	}
	var env grErrEnvelope
	require.NoError(t, json.Unmarshal(body, &env), "response: %s", body)
	return env
}

// TestOIDCClientCapabilityFourOutcomesLive pins the four detector outcomes
// live through runtime.Run on a generated verb (get oidcclient): proceed
// (target request runs), unknown (target request still runs — the
// unknown-never-blocks rule), disabled (VALIDATION, zero target requests),
// and PRIME_REQUIRED (zero target requests).
func TestOIDCClientCapabilityFourOutcomesLive(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

	t.Run("proceed: feature enabled", func(t *testing.T) {
		srv, hits := oidcAcceptanceServer(t,
			`{"Version":"v2.16.0","RancherPrime":"false"}`,
			oidcFeatureObj(true, true), http.StatusOK)
		dir := grConfigDir(t, srv)
		stdout, stderr, err := grRun(t, root, dir, "get", "oidcclient", "-o", "json")
		require.NoError(t, err, "stderr: %s", stderr)
		assert.Contains(t, string(stdout), `"OIDCClientList"`)
		assert.Equal(t, int32(1), hits.Load(), "proceed must reach the target request exactly once")
	})

	t.Run("unknown: feature absent never blocks", func(t *testing.T) {
		srv, hits := oidcAcceptanceServer(t,
			`{"Version":"v2.16.0","RancherPrime":"false"}`,
			"", http.StatusNotFound)
		dir := grConfigDir(t, srv)
		stdout, stderr, err := grRun(t, root, dir, "get", "oidcclient", "-o", "json")
		require.NoError(t, err, "stderr: %s", stderr)
		assert.Contains(t, string(stdout), `"OIDCClientList"`)
		assert.Equal(t, int32(1), hits.Load(), "unknown outcome must still reach the target request")
	})

	t.Run("disabled: prime edition with the feature off", func(t *testing.T) {
		srv, hits := oidcAcceptanceServer(t,
			`{"Version":"v2.16.0","RancherPrime":"true"}`,
			oidcFeatureObj(false, false), http.StatusOK)
		dir := grConfigDir(t, srv)
		stdout, stderr, err := grRun(t, root, dir, "get", "oidcclient", "-o", "json")
		require.Error(t, err)
		env := oidcErrEnvelope(t, stdout, stderr)
		assert.Equal(t, clierr.CodeValidation, env.Data.Error.Code)
		assert.Contains(t, env.Data.Error.Message, "oidc-provider is disabled")
		assert.Contains(t, env.Data.Error.Hint, "enable feature oidc-provider")
		assert.Zero(t, hits.Load(), "disabled must make zero target requests")
	})

	// This is the real shape a community server serves before oidc-provider
	// is ever toggled on: the Feature object already exists (200, not 404)
	// with status.prime false and status.default false. It refuses rather
	// than falling through to unknown, closing the orphan-commit path a
	// generic downstream NOT_FOUND used to leave open.
	t.Run("disabled: community edition, never-toggled feature", func(t *testing.T) {
		srv, hits := oidcAcceptanceServer(t,
			`{"Version":"v2.14.3","RancherPrime":"false"}`,
			oidcFeatureObj(false, false), http.StatusOK)
		dir := grConfigDir(t, srv)
		stdout, stderr, err := grRun(t, root, dir, "get", "oidcclient", "-o", "json")
		require.Error(t, err)
		env := oidcErrEnvelope(t, stdout, stderr)
		assert.Equal(t, clierr.CodeValidation, env.Data.Error.Code)
		assert.Contains(t, env.Data.Error.Message, "oidc-provider is disabled on this server")
		assert.Contains(t, env.Data.Error.Hint, clierr.LearnMorePrimeURL)
		assert.Zero(t, hits.Load(), "disabled must make zero target requests")
	})

	t.Run("prime required: community edition, prime-classified feature off", func(t *testing.T) {
		srv, hits := oidcAcceptanceServer(t,
			`{"Version":"v2.16.0","RancherPrime":"false"}`,
			oidcFeatureObj(true, false), http.StatusOK)
		dir := grConfigDir(t, srv)
		stdout, stderr, err := grRun(t, root, dir, "get", "oidcclient", "-o", "json")
		require.Error(t, err)
		env := oidcErrEnvelope(t, stdout, stderr)
		assert.Equal(t, clierr.CodePrimeRequired, env.Data.Error.Code)
		var details struct {
			Feature string `json:"feature"`
		}
		require.NoError(t, json.Unmarshal(env.Data.Error.Details, &details))
		assert.Equal(t, "oidc-provider", details.Feature)
		assert.Zero(t, hits.Load(), "PRIME_REQUIRED must make zero target requests")
	})
}

// oidcClientManifest writes a minimal, spec-complete OIDCClient manifest
// (both TTL fields are CRD-required — see the kind row's doc comment) to a
// temp file and returns its path.
func oidcClientManifest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "oidcclient.yaml")
	body := "apiVersion: management.cattle.io/v3\n" +
		"kind: OIDCClient\n" +
		"metadata:\n  name: acceptance-test-client\n" +
		"spec:\n  tokenExpirationSeconds: 3600\n  refreshTokenExpirationSeconds: 86400\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// TestCreateResourceOIDCClientManifestPrimeRequired pins the create-gating
// seam's real-table acceptance: `create resource -f` with an oidcclient
// manifest resolves the row's Capability requirement via
// createCmd.ResolveCapability and answers PRIME_REQUIRED BEFORE Plan runs —
// the target oidcclients GVR sees zero requests, proving the capability
// resolver stage runs strictly before the server-side dry-run.
func TestCreateResourceOIDCClientManifestPrimeRequired(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	srv, hits := oidcAcceptanceServer(t,
		`{"Version":"v2.16.0","RancherPrime":"false"}`,
		oidcFeatureObj(true, false), http.StatusOK)
	dir := grConfigDir(t, srv)
	manifest := oidcClientManifest(t)

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	stdout, stderr, err := grRun(t, root, dir, "create", "resource", "-f", manifest, "-o", "json")
	require.Error(t, err)
	env := oidcErrEnvelope(t, stdout, stderr)
	assert.Equal(t, clierr.CodePrimeRequired, env.Data.Error.Code)
	var details struct {
		Feature string `json:"feature"`
	}
	require.NoError(t, json.Unmarshal(env.Data.Error.Details, &details))
	assert.Equal(t, "oidc-provider", details.Feature)
	assert.Zero(t, hits.Load(), "create resource -f must make zero target requests before a PRIME_REQUIRED refusal")
}

// TestCreateResourceOIDCClientManifestDisabledAfterEnabled pins the
// disabled-after-enabled orphan case: a feature that was on and is now
// explicitly off (spec.value=false) still serves its CRD — a create would
// otherwise commit against a controller that stopped reconciling. The
// capability resolver must refuse before Plan makes any target request,
// the same as the PRIME_REQUIRED path above.
func TestCreateResourceOIDCClientManifestDisabledAfterEnabled(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	srv, hits := oidcAcceptanceServer(t,
		`{"Version":"v2.14.3","RancherPrime":"false"}`,
		oidcFeatureObj(false, false), http.StatusOK)
	dir := grConfigDir(t, srv)
	manifest := oidcClientManifest(t)

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	stdout, stderr, err := grRun(t, root, dir, "create", "resource", "-f", manifest, "-o", "json")
	require.Error(t, err)
	env := oidcErrEnvelope(t, stdout, stderr)
	assert.Equal(t, clierr.CodeValidation, env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Message, "oidc-provider is disabled on this server")
	assert.Zero(t, hits.Load(), "create resource -f must make zero target requests before a disabled refusal")
}
