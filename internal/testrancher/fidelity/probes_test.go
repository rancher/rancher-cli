package fidelity

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
		"discovery/aggregated-downstream", "discovery/aggregated-on-local",
		"discovery/dead-group-legacy-document",
		"owner/local-creates-name-the-creator", "owner/login-body-names-no-owner",
		"owner-downstream/scoped-creates-and-reviews", "owner-downstream/cluster-login-and-review",
		"access-downstream/owner-management-gate", "access-downstream/owner-steve",
		"access-downstream/member-steve-by-id", "access-downstream/member-cluster-token",
		"access-downstream/admin-steve-cluster-shape", "access-downstream/standard-steve-empty",
		"access/standard-cloud-credential-by-name",
		"revoke/unscoped-ext-token-deletes-itself", "revoke/ext-token-deleted-by-another-token-of-its-user",
		"revoke/standard-user-v3-self-delete-forbidden", "revoke-downstream/cluster-scoped-ext-token-deletes-itself",
		"revoke/standard-user-v3-token-logout-deletes-itself", "revoke/standard-user-v3-logout-with-a-deleted-token",
		"revoke-downstream/cluster-scoped-v3-token-logout-401", "revoke/standard-user-login-token-logout-deletes-itself",
	}
	var got []string
	for _, p := range All() {
		got = append(got, p.Name)
	}
	assert.ElementsMatch(t, want, got)
}

// TestLogoutObservationsCarryOnlyTheCapturedBearer asserts every
// observation that posts to the logout endpoint, or deletes a v3 token
// through the proxy, is sent with no identity and a captured bearer.
// Logout deletes the token its bearer names, so any other identity would
// delete that identity's own token: the oracle's admin, the run's standard
// login. A v3 DELETE is checked too because one sent with another
// identity would use that identity's bearer.
func TestLogoutObservationsCarryOnlyTheCapturedBearer(t *testing.T) {
	t.Parallel()
	logouts := 0
	for _, p := range All() {
		for _, o := range p.Observations {
			deletesV3 := o.Request.Method == http.MethodDelete && strings.HasPrefix(o.Request.Path, token.V3TokensPath)
			if o.Request.Path != token.LogoutPath && !deletesV3 {
				continue
			}
			if o.Request.Path == token.LogoutPath {
				logouts++
			}
			assert.Equal(t, IdentityNone, o.Request.Identity, "%s/%s", p.Name, o.Name)
			assert.Regexp(t, `^Bearer \{[a-z0-9]+\}$`, o.Request.Headers["Authorization"], "%s/%s", p.Name, o.Name)
		}
	}
	assert.NotZero(t, logouts, "the logout probes exist")
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

// TestTokenUsesRightAfterACreateOrDeleteWaitForTheirRecordedStatus asserts
// that every observation sending a captured bearer (an Authorization header
// "Bearer {name}") or the revoked identity sets WaitForStatus to the status
// its goldens record, unless it is allowlisted with a reason; and that the
// admin reads of an ext token right after its create or delete do the
// same. Rancher reads tokens from informer caches, so without the wait such
// an observation can record the cache delay instead of the behavior.
func TestTokenUsesRightAfterACreateOrDeleteWaitForTheirRecordedStatus(t *testing.T) {
	t.Parallel()
	type key struct{ probe, observation string }
	allowed := map[key]string{
		{"revoke/standard-user-v3-self-delete-forbidden", "delete-with-its-own-bearer"}:                         "v3 bearer right after its create; a v3 cache miss falls back to a direct get",
		{"revoke/standard-user-v3-token-logout-deletes-itself", "logout-with-its-own-bearer"}:                   "v3 bearer right after its create",
		{"revoke/standard-user-login-token-logout-deletes-itself", "logout-with-its-own-bearer"}:                "login bearer right after the login",
		{"revoke/standard-user-v3-logout-with-a-deleted-token", "logout-with-its-own-bearer"}:                   "v3 bearer right after its create",
		{"owner-downstream/scoped-creates-and-reviews", "review-downstream-with-the-scoped-v3-token"}:           "v3 bearer right after its create",
		{"owner-downstream/cluster-login-and-review", "review-downstream-with-the-login-token"}:                 "login bearer right after the login",
		{"revoke-downstream/cluster-scoped-v3-token-logout-401", "logout-with-its-own-bearer"}:                  "cluster-scoped token refused outside its cluster; never deleted",
		{"revoke-downstream/cluster-scoped-v3-token-logout-401", "delete-with-its-own-bearer-after-the-logout"}: "cluster-scoped token refused outside its cluster; never deleted",
		{"owner-downstream/scoped-creates-and-reviews", "review-local-with-the-scoped-v3-token"}:                "cluster-scoped token refused outside its cluster; never deleted",
		{"revoke/ext-token-deleted-by-another-token-of-its-user", "second-delete-with-the-other-token"}:         "bearer already accepted on the previous request; the object is read by a direct get",
		{"access-downstream/member-cluster-token", "downstream-read-with-the-cluster-token"}:                    "token created in a setup step; its failures belong to the access-downstream investigation",
	}
	adminReads := map[key]bool{
		{"tok/create-shape", "get"}:          true,
		{"tok/dryrun-delete", "still-there"}: true,
		{"tok/revoke", "gone"}:               true,
	}
	capturedBearer := regexp.MustCompile(`^Bearer \{[^}]+\}$`)
	used := map[key]bool{}
	for _, p := range All() {
		for _, o := range p.Observations {
			k := key{p.Name, o.Name}
			selected := capturedBearer.MatchString(o.Request.Headers["Authorization"]) ||
				o.Request.Identity == IdentityRevoked || adminReads[k]
			if !selected {
				assert.Zero(t, o.WaitForStatus, "%s %s does not use a token right after its create or delete", p.Name, o.Name)
				continue
			}
			if reason, ok := allowed[k]; ok {
				used[k] = true
				assert.Zero(t, o.WaitForStatus, "%s %s is allowlisted: %s", p.Name, o.Name, reason)
				continue
			}
			if !assert.NotZero(t, o.WaitForStatus, "%s %s: set WaitForStatus or allowlist it with a reason", p.Name, o.Name) {
				continue
			}
			dirs := []string{"oracle"}
			if p.Downstream {
				dirs = []string{"downstream"}
			}
			if p.FacadeDivergence != "" {
				dirs = append(dirs, "facade")
			}
			for _, dir := range dirs {
				g, err := LoadGolden(GoldenPath(filepath.Join("testdata", dir), p.Name))
				require.NoError(t, err)
				i := slices.IndexFunc(g.Observations, func(r ObservationResult) bool { return r.Name == o.Name })
				require.GreaterOrEqual(t, i, 0, "%s golden of %s has no %s", dir, p.Name, o.Name)
				assert.Equal(t, g.Observations[i].Status, o.WaitForStatus, "%s %s: the %s golden records another status", p.Name, o.Name, dir)
			}
		}
	}
	for k, reason := range allowed {
		assert.True(t, used[k], "allowlist entry %s %s (%s) matches no selected observation", k.probe, k.observation, reason)
	}
}
