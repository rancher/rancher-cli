package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestDecideCapabilityTruthTable pins the four-outcome capability truth table
// exactly, including the stated precedence and contradiction resolutions.
func TestDecideCapabilityTruthTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		enabled *bool
		prime   *bool
		edition provider.Edition
		want    runtime.CapabilityOutcome
	}{
		{"enabled true on community: proceed regardless", new(true), new(true), provider.EditionCommunity, runtime.CapabilityProceed},
		{"enabled true wins over any edition", new(true), nil, provider.EditionUnknown, runtime.CapabilityProceed},
		{"disabled on prime edition: disabled", new(false), new(true), provider.EditionPrime, runtime.CapabilityDisabled},
		{"edition prime + feature prime:false is a non-prime feature that is off", new(false), new(false), provider.EditionPrime, runtime.CapabilityDisabled},
		{"disabled + prime-classified + community: prime required", new(false), new(true), provider.EditionCommunity, runtime.CapabilityPrimeRequired},
		{"disabled + non-prime feature + community: disabled, not prime-required", new(false), new(false), provider.EditionCommunity, runtime.CapabilityDisabled},
		{"disabled + missing prime classification + community: disabled", new(false), nil, provider.EditionCommunity, runtime.CapabilityDisabled},
		{"enabled missing (forbidden/absent Feature GET): unknown", nil, new(true), provider.EditionCommunity, runtime.CapabilityUnknown},
		{"edition unknown, disabled: disabled on any edition", new(false), new(true), provider.EditionUnknown, runtime.CapabilityDisabled},
		{"everything unavailable: unknown", nil, nil, provider.EditionUnknown, runtime.CapabilityUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, decideCapability(tc.enabled, tc.prime, tc.edition))
		})
	}
}

func TestFeatureStateReadsEffectiveValueAndPrimeClassification(t *testing.T) {
	t.Parallel()

	t.Run("spec.value present", func(t *testing.T) {
		t.Parallel()
		obj := &unstructured.Unstructured{Object: map[string]any{
			"spec":   map[string]any{"value": true},
			"status": map[string]any{"prime": true, "default": false},
		}}
		enabled, prime := featureState(obj)
		require.NotNil(t, enabled)
		assert.True(t, *enabled)
		require.NotNil(t, prime)
		assert.True(t, *prime)
	})

	t.Run("spec.value absent falls back to status.default", func(t *testing.T) {
		t.Parallel()
		obj := &unstructured.Unstructured{Object: map[string]any{
			"status": map[string]any{"default": false, "prime": true},
		}}
		enabled, prime := featureState(obj)
		require.NotNil(t, enabled)
		assert.False(t, *enabled)
		require.NotNil(t, prime)
		assert.True(t, *prime)
	})

	t.Run("both absent: nil, never guessed false", func(t *testing.T) {
		t.Parallel()
		obj := &unstructured.Unstructured{Object: map[string]any{}}
		enabled, prime := featureState(obj)
		assert.Nil(t, enabled)
		assert.Nil(t, prime)
	})
}

// featureObj builds a minimal but wire-realistic Feature JSON body: kind,
// apiVersion, and metadata.name matter because client-go's unstructured
// decode path requires them, not just the status fields under test.
func featureObj(prime, defaultVal bool) string {
	return fmt.Sprintf(`{"kind":"Feature","apiVersion":"management.cattle.io/v3",
		"metadata":{"name":"widget-feature"},
		"status":{"prime":%t,"default":%t}}`, prime, defaultVal)
}

// featureFake stands in for a management-scope Rancher server serving both
// /rancherversion and a named Feature GET at the same path pattern
// livePositionalCandidates' fixtures use (managementClusterServer).
func featureFake(t *testing.T, rancherVersionBody, featureBody string, featureStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rancherversion":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(rancherVersionBody))
		case "/k8s/clusters/local/apis/management.cattle.io/v3/features/widget-feature":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(featureStatus)
			_, _ = w.Write([]byte(featureBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func checkerFor(t *testing.T, srv *httptest.Server) *capabilityChecker {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "acme",
		Servers:       map[string]config.Server{"acme": {URL: srv.URL}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	f := newClientFactory(dir, &countingStore{token: "id:secret"})
	return newCapabilityChecker(f)
}

// TestCapabilityCheckerPrimeRequiredEndToEnd proves the full composition —
// provider server-info probe + engine feature client — against a fake
// server: a Prime-classified, disabled feature on a community server yields
// PRIME_REQUIRED, with Server populated for the error's details.
func TestCapabilityCheckerPrimeRequiredEndToEnd(t *testing.T) {
	t.Parallel()
	srv := featureFake(t,
		`{"Version":"v2.15.0","RancherPrime":"false"}`,
		featureObj(true, false), http.StatusOK)
	c := checkerFor(t, srv)
	dec := c.Check(t.Context(), runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"})
	assert.Equal(t, runtime.CapabilityPrimeRequired, dec.Outcome)
	assert.Equal(t, srv.URL, dec.Server)
}

// TestCapabilityCheckerDisabledOnPrimeServerEndToEnd proves outcome 2: a
// disabled feature on an affirmatively Prime server, regardless of the
// feature's own classification.
func TestCapabilityCheckerDisabledOnPrimeServerEndToEnd(t *testing.T) {
	t.Parallel()
	srv := featureFake(t,
		`{"Version":"v2.15.0","RancherPrime":"true"}`,
		featureObj(true, false), http.StatusOK)
	c := checkerFor(t, srv)
	dec := c.Check(t.Context(), runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"})
	assert.Equal(t, runtime.CapabilityDisabled, dec.Outcome)
	assert.True(t, dec.EditionPrime)
}

// TestCapabilityCheckerDisabledOnCommunityServerEndToEnd proves the amended
// outcome 2: a disabled, non-prime-classified feature on a community server
// is Disabled (not Unknown) with EditionPrime false, selecting the neutral
// message variant.
func TestCapabilityCheckerDisabledOnCommunityServerEndToEnd(t *testing.T) {
	t.Parallel()
	srv := featureFake(t,
		`{"Version":"v2.15.0","RancherPrime":"false"}`,
		featureObj(false, false), http.StatusOK)
	c := checkerFor(t, srv)
	dec := c.Check(t.Context(), runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"})
	assert.Equal(t, runtime.CapabilityDisabled, dec.Outcome)
	assert.False(t, dec.EditionPrime)
}

// TestCapabilityCheckerForbiddenFeatureGetIsUnknown proves the detector
// suppresses its own failure (a forbidden Feature GET) rather than blocking
// or surfacing a detector-side error — the unknown outcome lets the real
// target request speak.
func TestCapabilityCheckerForbiddenFeatureGetIsUnknown(t *testing.T) {
	t.Parallel()
	srv := featureFake(t, `{"Version":"v2.15.0","RancherPrime":"false"}`, `{}`, http.StatusForbidden)
	c := checkerFor(t, srv)
	dec := c.Check(t.Context(), runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"})
	assert.Equal(t, runtime.CapabilityUnknown, dec.Outcome)
}

// TestCapabilityCheckerOneEditionFetchSharedWithFeatureCheck proves the
// one-fetch-per-invocation requirement using the SAME checker
// (and therefore the same underlying clientFactory) for two Check calls —
// the memoized ServerInfo must not re-fetch.
func TestCapabilityCheckerOneEditionFetchSharedAcrossChecks(t *testing.T) {
	t.Parallel()
	var versionHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rancherversion":
			versionHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Version":"v2.15.0","RancherPrime":"false"}`))
		case "/k8s/clusters/local/apis/management.cattle.io/v3/features/widget-feature":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(featureObj(true, false)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := checkerFor(t, srv)
	c.Check(t.Context(), runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"})
	c.Check(t.Context(), runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"})
	assert.Equal(t, 1, versionHits, "the checker's clientFactory must fetch /rancherversion at most once")
}

// TestCapabilityCheckerEnabledFeatureSkipsEditionFetch makes sure the common
// case — the feature is already enabled — decides on the Feature alone and
// never pays the /rancherversion round trip (truth-table outcome 1 needs no
// edition).
func TestCapabilityCheckerEnabledFeatureSkipsEditionFetch(t *testing.T) {
	t.Parallel()
	var versionHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rancherversion":
			versionHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Version":"v2.15.0","RancherPrime":"false"}`))
		case "/k8s/clusters/local/apis/management.cattle.io/v3/features/widget-feature":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(featureObj(true, true)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := checkerFor(t, srv)
	dec := c.Check(t.Context(), runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"})
	assert.Equal(t, runtime.CapabilityProceed, dec.Outcome)
	assert.Equal(t, 0, versionHits, "an enabled feature must never trigger the edition fetch")
}
