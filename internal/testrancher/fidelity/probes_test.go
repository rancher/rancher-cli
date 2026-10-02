package fidelity

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/token"
)

func TestAllProbesAreValidAndUniquelyNamed(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, p := range All() {
		require.NoError(t, p.Validate(), p.Name)
		assert.False(t, seen[p.Name], "duplicate probe name %s", p.Name)
		seen[p.Name] = true
	}
}

func TestExpectedProbeSet(t *testing.T) {
	t.Parallel()
	want := []string{
		"auth/no-token", "auth/garbage-token", "auth/revoked-token", "auth/standard-create",
		"auth/forbidden-read", "auth/forbidden-write",
		"tok/create-shape", "tok/dryrun-delete", "tok/stale-precondition-delete", "tok/revoke",
		"mut/dryrun-create", "mut/dryrun-invalid", "mut/stale-rv-update",
		"mut/rv-precondition-delete", "mut/dryrun-then-apply",
		"capability/edition-and-oidc-provider-classification",
		"user-password-workflow/discovery-and-namespace",
		"access-downstream/caller-without-cluster-access", "access-downstream/impersonation-refused",
		"access-downstream/subject-without-cluster-access", "access-downstream/nonexistent-user",
		"access-downstream/nonexistent-service-account", "access-downstream/service-account-identity",
		"access-downstream/impersonate-group-dropped", "access-downstream/rules-review-complete",
		"access-downstream/sar-without-right", "access-downstream/version-field-accepted",
		"access-downstream/who-can-reason-and-binding-shape",
		"access-downstream/explain-fixture-chain", "access-downstream/explain-fixture-reason-grammar",
		"access-downstream/direct-endpoint-shapes",
		"access-downstream/member-steve-clusters", "access-downstream/member-proxy-whoami",
		"access-downstream/member-management-refusals",
		"access-downstream/member-steve-projects-and-namespaces",
		"access-downstream/member-steve-projects-by-cluster", "access-downstream/admin-steve-projects-by-cluster",
		"project-lookup/admin-local-projects",
		"access/whoami-through-proxy", "access/impersonate-user-dropped-on-local",
		"access/rules-review-standard-user-default", "access/self-access-review-with-version",
		"access/subject-access-review-right-and-anonymous",
		"access/subject-access-review-reason-grammar",
		"access/bootstrap-clusterrolebinding-shape",
		"access/explain-local-globalrole-labels", "access/explain-management-shapes",
		"discovery/aggregated-downstream", "discovery/aggregated-not-served-on-local",
		"discovery/dead-group-legacy-document",
	}
	var got []string
	for _, p := range All() {
		got = append(got, p.Name)
	}
	assert.ElementsMatch(t, want, got)
}

func TestRBACDenialProbesAreOracleOnly(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		if p.Name == "auth/forbidden-read" || p.Name == "auth/forbidden-write" {
			assert.True(t, p.OracleOnly, p.Name)
		}
	}
}

func TestOracleOnlyProbesHaveNoFacadeGolden(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		if !p.OracleOnly {
			continue
		}
		path := GoldenPath(filepath.Join("testdata", "facade"), p.Name)
		_, err := os.Stat(path)
		assert.True(t, os.IsNotExist(err),
			"oracle-only probe %s must not have a facade golden at %s", p.Name, path)
	}
}

func TestEveryProbeHasItsRecordedGoldens(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		if p.Downstream {
			continue
		}
		oraclePath := GoldenPath(filepath.Join("testdata", "oracle"), p.Name)
		_, err := os.Stat(oraclePath)
		require.NoError(t, err, "probe %s must have an oracle golden at %s", p.Name, oraclePath)
		if p.FacadeDivergence == "" {
			continue
		}
		facadePath := GoldenPath(filepath.Join("testdata", "facade"), p.Name)
		_, err = os.Stat(facadePath)
		assert.NoError(t, err,
			"divergence-pinned probe %s must have a facade golden at %s", p.Name, facadePath)
	}
}

func TestEveryDownstreamProbeHasItsRecordedGolden(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		if !p.Downstream {
			continue
		}
		path := GoldenPath(filepath.Join("testdata", "downstream"), p.Name)
		_, err := os.Stat(path)
		require.NoError(t, err, "downstream probe %s must have a recorded golden at %s", p.Name, path)
	}
}

func TestDownstreamWhoamiProbesNormalizeExtra(t *testing.T) {
	t.Parallel()
	whoamiPaths := map[string]bool{downstreamWhoami: true, localWhoami: true}
	for _, p := range All() {
		if !p.Downstream {
			continue
		}
		hasWhoami := false
		for _, o := range p.Observations {
			if whoamiPaths[o.Request.Path] {
				hasWhoami = true
			}
		}
		if !hasWhoami {
			continue
		}
		hasExtraRule := false
		for _, r := range p.Normalize {
			if r.Path == "status.userInfo.extra" {
				hasExtraRule = true
			}
		}
		assert.True(t, hasExtraRule, "%s has a whoami observation but no status.userInfo.extra normalize rule", p.Name)
	}
}

func TestAdminDoSendsBearerAndReturnsStatus(t *testing.T) {
	t.Parallel()
	var gotAuth, gotMethod, gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotMethod, gotCT = r.Header.Get("Authorization"), r.Method, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "admin-tok"}}

	status, err := adminDo(t.Context(), tgt, http.MethodPost, "/x", `{"a":1}`)
	require.NoError(t, err)
	assert.Equal(t, http.StatusCreated, status)
	assert.Equal(t, "Bearer admin-tok", gotAuth)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, `{"a":1}`, gotBody)
	assert.Equal(t, "application/json", gotCT, "a non-empty body sets the JSON content type")
}

func TestAdminDoSurfacesTransportError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
	srv.Close() // requests now fail at the transport

	_, err := adminDo(t.Context(), tgt, http.MethodGet, "/x", "")
	require.Error(t, err, "a transport failure is surfaced, never swallowed")
}

func TestDeleteFixtureStatusHandling(t *testing.T) {
	t.Parallel()
	var code int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		w.WriteHeader(code)
	}))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
	ctx := t.Context()

	code = http.StatusOK
	require.NoError(t, deleteFixture("f", "/x/y").Do(ctx, tgt, map[string]string{}), "a 2xx delete is clean")
	code = http.StatusNotFound
	require.NoError(t, deleteFixture("f", "/x/y").Do(ctx, tgt, map[string]string{}), "an absent fixture (404) is clean")
	code = http.StatusForbidden
	require.Error(t, deleteFixture("f", "/x/y").Do(ctx, tgt, map[string]string{}), "a rejected delete is an error")
}

func TestDeleteFixtureUnresolvedTemplateNeverHitsServer(t *testing.T) {
	t.Parallel()
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	err := deleteFixture("f", "/x/{tokname}").Do(t.Context(), tgt, map[string]string{})
	require.Error(t, err)
	require.ErrorContains(t, err, "unresolved")
	assert.False(t, hit, "an unresolved capture must never delete against a synthetic URL")
}

func TestCreateSecretFixtureDeletesBeforeCreate(t *testing.T) {
	t.Parallel()
	var order []string
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE "+localSecretsPath+"/e2e-fid-x", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "delete")
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("POST "+localSecretsPath, func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "create")
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	require.NoError(t, createSecretFixture("e2e-fid-x", `{"kind":"Secret"}`).Do(t.Context(), tgt, map[string]string{}))
	assert.Equal(t, []string{"delete", "create"}, order,
		"an interrupted prior run's leftover is removed before the create")
}

func TestCreateSecretFixtureNon201Errors(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE "+localSecretsPath+"/e2e-fid-x", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("POST "+localSecretsPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	err := createSecretFixture("e2e-fid-x", `{"kind":"Secret"}`).Do(t.Context(), tgt, map[string]string{})
	require.Error(t, err)
	assert.ErrorContains(t, err, "status 500", "a non-201 create is surfaced with its status")
}

func TestTokenProbesUseTheProductionPathConstant(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		if !strings.HasPrefix(p.Name, "tok/") && p.Name != "auth/standard-create" {
			continue
		}
		for _, o := range p.Observations {
			assert.True(t, strings.HasPrefix(o.Request.Path, token.ExtTokensPath),
				"%s %s path %q must be rooted at token.ExtTokensPath", p.Name, o.Name, o.Request.Path)
		}
	}
}

// TestFixtureNamesCarryTheFidelityPrefix asserts every fixture a probe
// creates (a body with object metadata) is named with the fidelity prefix.
// A review body names the object it asks about under resourceAttributes,
// creates nothing, and is out of scope.
func TestFixtureNamesCarryTheFidelityPrefix(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		for _, o := range p.Observations {
			if strings.Contains(o.Request.Body, `"metadata"`) && strings.Contains(o.Request.Body, `"name"`) {
				assert.Contains(t, o.Request.Body, "e2e-fid-",
					"%s %s creates a named fixture without the e2e-fid- prefix", p.Name, o.Name)
			}
		}
	}
}

func TestCreateCapturesDeriveTheNameFromTheBearer(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		for _, o := range p.Observations {
			c, ok := o.Captures["tokname"]
			if !ok {
				continue
			}
			assert.Equal(t, Capture{Path: "status.bearerToken", ExtBearerName: true}, c,
				"%s %s: the create response carries no metadata.name; the name must derive from the ext bearer", p.Name, o.Name)
		}
	}
}

func TestRVSequencesStartWithARecordedGet(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		switch p.Name {
		case "mut/stale-rv-update", "mut/rv-precondition-delete", "mut/dryrun-then-apply":
			require.NotEmpty(t, p.Setup, "%s creates its fixture in idempotent setup", p.Name)
			first := p.Observations[0]
			assert.Equal(t, http.MethodGet, first.Request.Method,
				"%s must record the initial GET the CLI pipeline performs", p.Name)
			assert.Contains(t, first.Captures, "rv1", p.Name)
		}
	}
}

func TestDryrunThenApplySendsByteIdenticalBodies(t *testing.T) {
	t.Parallel()
	for _, p := range All() {
		if p.Name != "mut/dryrun-then-apply" {
			continue
		}
		var dryRun, apply string
		for _, o := range p.Observations {
			switch o.Name {
			case "dry-run-update":
				dryRun = o.Request.Body
			case "apply":
				apply = o.Request.Body
			}
		}
		require.NotEmpty(t, dryRun)
		assert.Equal(t, dryRun, apply,
			"execution must apply exactly the object the preview rendered")
	}
}

// TestAccessProbesAreRegistered asserts the access family is reachable
// through All, so the golden-presence gate covers it.
func TestAccessProbesAreRegistered(t *testing.T) {
	t.Parallel()
	names := map[string]bool{}
	for _, p := range All() {
		names[p.Name] = true
	}
	for _, want := range []string{
		"access/whoami-through-proxy", "access/impersonate-user-dropped-on-local",
		"access/rules-review-standard-user-default", "access/self-access-review-with-version",
		"access/subject-access-review-right-and-anonymous",
		"access/subject-access-review-reason-grammar",
		"access/bootstrap-clusterrolebinding-shape",
		"access/explain-local-globalrole-labels", "access/explain-management-shapes",
	} {
		assert.True(t, names[want], want)
	}
}
