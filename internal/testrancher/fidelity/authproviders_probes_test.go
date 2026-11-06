package fidelity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	onlyLocalProbe = "authproviders/only-local-enabled"
	externalProbe  = "authproviders/external-providers-enabled"
)

var authFakeConfigs = []string{"openldap", "okta", "azuread"}

var authFakeSeeds = []string{"enabled absent", "enabled false"}

// authFaultMode is how the fake fails one request.
type authFaultMode string

const (
	// faultRefused answers a PATCH with 403 and applies nothing.
	faultRefused authFaultMode = "refused"
	// faultApplied500 applies a PATCH and answers 500.
	faultApplied500 authFaultMode = "applied and answered 500"
	// faultUnavailable503 answers a PATCH with 503 and applies nothing.
	faultUnavailable503 authFaultMode = "answered 503"
	// faultTooMany429 answers a PATCH with 429 and applies nothing.
	faultTooMany429 authFaultMode = "answered 429"
	// faultClosed applies a PATCH and closes the connection before the answer.
	faultClosed authFaultMode = "applied and connection closed"
	// fault500 answers a GET with 500.
	fault500 authFaultMode = "answered 500"
)

// authFault fails the k-th request (counted from 1) of one method.
type authFault struct {
	method string
	k      int
	mode   authFaultMode
}

// authRequest is one request the fake received.
type authRequest struct {
	method      string
	path        string
	contentType string
	phase       string
	body        map[string]any
}

// authFake models the auth configs a Rancher server holds, as JSON
// documents seeded like a fresh server's, the public list that reads their
// enabled values, an RFC 7386 merge patch, the webhook's refusal of an
// enabled LDAP config with no server, and the sync of Rancher's auth config
// controller: an enabled config marked "rancher-locked" or with no annotation
// becomes "unlocked", which the fake records as an unlock event, and a
// disabled config marked "unlocked" is reset and cleaned up, which the fake
// records as a cleanup event. The fake is stricter than Rancher in one
// state: it also records an unlock event for an enabled config marked
// "unlocked", which Rancher's controller leaves alone, so that a probe that
// ever leaves a config in that state fails. It has no features route.
type authFake struct {
	mu       sync.Mutex
	srv      *httptest.Server
	docs     map[string]map[string]any
	start    map[string]map[string]any
	requests []authRequest
	cleanups []string
	unlocks  []string
	phase    string
	fault    authFault
	patches  int
	gets     int
	injected bool
	// failRestoresOf names a config whose every PATCH in the teardown is
	// answered 503 without being applied.
	failRestoresOf string
	// version is what /rancherversion answers.
	version string
}

func deepCopyDocs(docs map[string]map[string]any) map[string]map[string]any {
	raw, err := json.Marshal(docs)
	if err != nil {
		panic(err)
	}
	var out map[string]map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

// seedAuthConfig is the stored object of a fresh server for one of the three
// configs: the generic auth config with its type, its cleanup annotation and
// no provider field.
func seedAuthConfig(name string, enabledFalse bool) map[string]any {
	annotations := map[string]any{authCleanupAnnotation: authCleanupLocked}
	typ, slo := "openLdapConfig", false
	switch name {
	case "okta":
		typ, slo = "oktaConfig", true
	case "azuread":
		typ, slo = "azureADConfig", true
		annotations["auth.cattle.io/azuread-endpoint-migrated"] = "true"
	}
	doc := map[string]any{
		"metadata":           map[string]any{"name": name, "annotations": annotations},
		"type":               typ,
		"logoutAllSupported": slo,
	}
	if enabledFalse {
		doc["enabled"] = false
	}
	return doc
}

func withGithubEnabled() func(*authFake) {
	return func(f *authFake) {
		f.docs["github"] = map[string]any{
			"metadata": map[string]any{"name": "github", "annotations": map[string]any{authCleanupAnnotation: authCleanupLocked}},
			"type":     "githubConfig",
			"enabled":  true,
		}
	}
}

func withoutConfig(name string) func(*authFake) {
	return func(f *authFake) { delete(f.docs, name) }
}

func withDoc(name string, change func(doc map[string]any)) func(*authFake) {
	return func(f *authFake) { change(f.docs[name]) }
}

func withServerVersion(version string) func(*authFake) {
	return func(f *authFake) { f.version = version }
}

func withFault(fault authFault) func(*authFake) {
	return func(f *authFake) { f.fault = fault }
}

func newAuthFake(t *testing.T, seed string, opts ...func(*authFake)) *authFake {
	t.Helper()
	f := &authFake{docs: map[string]map[string]any{}, phase: "setup", version: "v2.15.2"}
	for _, name := range authFakeConfigs {
		f.docs[name] = seedAuthConfig(name, seed == "enabled false")
	}
	for _, o := range opts {
		o(f)
	}
	f.start = deepCopyDocs(f.docs)
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *authFake) target() *fakeTarget {
	return &fakeTarget{srv: f.srv, bearers: map[Identity]string{IdentityAdmin: "admin-token"}}
}

func (f *authFake) setPhase(phase string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.phase = phase
}

// phased wraps p's steps so the fake records whether a request came from the
// setup or the teardown.
func (f *authFake) phased(p Probe) Probe {
	wrap := func(steps []Step, phase string) []Step {
		out := make([]Step, len(steps))
		for i, s := range steps {
			out[i] = Step{Name: s.Name, Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
				f.setPhase(phase)
				return s.Do(ctx, tgt, captured)
			}}
		}
		return out
	}
	p.Setup, p.Teardown = wrap(p.Setup, "setup"), wrap(p.Teardown, "teardown")
	return p
}

func (f *authFake) run(t *testing.T, p Probe, timeout time.Duration) ([]ObservationResult, error) {
	t.Helper()
	return runProbe(t.Context(), f.target(), f.phased(p), timeout)
}

func (f *authFake) log() []authRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *authFake) endState() map[string]map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return deepCopyDocs(f.docs)
}

func (f *authFake) cleanupEvents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cleanups)
}

func (f *authFake) unlockEvents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.unlocks)
}

// assertNoControllerEvent asserts Rancher's auth config controller found
// nothing to act on: no config was ever enabled while marked "rancher-locked"
// or unmarked, none was ever marked "unlocked", and none was reset and
// cleaned up.
func assertNoControllerEvent(t *testing.T, f *authFake, msgAndArgs ...any) {
	t.Helper()
	assert.Empty(t, f.unlockEvents(), msgAndArgs...)
	assert.Empty(t, f.cleanupEvents(), msgAndArgs...)
}

// mergePatchDocument applies an RFC 7386 merge patch: an object merges
// key by key, null deletes a key, anything else replaces.
func mergePatchDocument(target, patch any) any {
	patchMap, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	targetMap, ok := target.(map[string]any)
	if !ok {
		targetMap = map[string]any{}
	}
	for k, v := range patchMap {
		if v == nil {
			delete(targetMap, k)
			continue
		}
		targetMap[k] = mergePatchDocument(targetMap[k], v)
	}
	return targetMap
}

func (f *authFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	req := authRequest{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"), phase: f.phase}
	if r.Method == http.MethodPatch {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req.body)
	}
	f.requests = append(f.requests, req)

	name, isConfig := strings.CutPrefix(r.URL.Path, authConfigsPath+"/")
	isOurs := isConfig && slices.Contains(authFakeConfigs, name)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/rancherversion":
		f.gets++
		if f.faultHits(http.MethodGet, f.gets) {
			http.Error(w, "injected", http.StatusInternalServerError)
			return
		}
		// Rancher's handler sets no Content-Type and sends three fields.
		raw, _ := json.Marshal(map[string]string{"Version": f.version, "GitCommit": "e2e-fid-commit", "RancherPrime": "false"})
		_, _ = w.Write(raw)
	case r.Method == http.MethodGet && r.URL.Path == authProvidersPath:
		f.gets++
		if f.faultHits(http.MethodGet, f.gets) {
			http.Error(w, "injected", http.StatusInternalServerError)
			return
		}
		f.writeList(w)
	case r.Method == http.MethodGet && isOurs:
		f.gets++
		if f.faultHits(http.MethodGet, f.gets) {
			http.Error(w, "injected", http.StatusInternalServerError)
			return
		}
		doc, ok := f.docs[name]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeFakeJSON(w, http.StatusOK, doc)
	case r.Method == http.MethodPatch && isConfig:
		f.patches++
		f.patch(w, name, isOurs, req)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (f *authFake) faultHits(method string, count int) bool {
	if f.fault.method == method && f.fault.k == count {
		f.injected = true
		return true
	}
	return false
}

func writeFakeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeList answers the public list: local, then every stored config that is
// enabled, as the list handler does.
func (f *authFake) writeList(w http.ResponseWriter) {
	entry := func(id, typ string) map[string]any {
		return map[string]any{"id": id, "type": typ, "logoutAllSupported": false, "logoutAllEnabled": false, "logoutAllForced": false}
	}
	data := []map[string]any{entry("local", "localProvider")}
	for _, name := range slices.Sorted(maps.Keys(f.docs)) {
		doc := f.docs[name]
		if doc["enabled"] != true {
			continue
		}
		typ, _ := doc["type"].(string)
		data = append(data, entry(name, strings.TrimSuffix(typ, "Config")+"Provider"))
	}
	writeFakeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (f *authFake) patch(w http.ResponseWriter, name string, isOurs bool, req authRequest) {
	if req.contentType != mergePatchContentType {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	doc, ok := f.docs[name]
	if !isOurs || !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	hit := f.faultHits(http.MethodPatch, f.patches)
	if hit && f.fault.mode == faultRefused {
		http.Error(w, "injected refusal", http.StatusForbidden)
		return
	}
	if hit && f.fault.mode == faultUnavailable503 {
		http.Error(w, "injected unavailable", http.StatusServiceUnavailable)
		return
	}
	if hit && f.fault.mode == faultTooMany429 {
		http.Error(w, "injected rate limit", http.StatusTooManyRequests)
		return
	}
	if f.failRestoresOf == name && req.phase == "teardown" {
		http.Error(w, "injected unavailable", http.StatusServiceUnavailable)
		return
	}
	patched, _ := mergePatchDocument(deepCopyDocs(map[string]map[string]any{name: doc})[name], req.body).(map[string]any)
	enabled := patched["enabled"] == true
	servers, _ := patched["servers"].([]any)
	if enabled && patched["type"] == "openLdapConfig" && len(servers) < 1 {
		http.Error(w, "at least one server is required", http.StatusBadRequest)
		return
	}
	f.docs[name] = patched
	meta, _ := patched["metadata"].(map[string]any)
	annotations, _ := meta["annotations"].(map[string]any)
	annotation, _ := annotations[authCleanupAnnotation].(string)
	switch {
	case enabled && (annotation == authCleanupLocked || annotation == ""):
		if annotations == nil {
			annotations = map[string]any{}
			meta["annotations"] = annotations
		}
		annotations[authCleanupAnnotation] = "unlocked"
		f.unlocks = append(f.unlocks, name)
	case enabled && annotation == "unlocked":
		f.unlocks = append(f.unlocks, name)
	case !enabled && annotation == "unlocked":
		f.cleanups = append(f.cleanups, name)
	}
	if hit {
		switch f.fault.mode {
		case faultApplied500:
			http.Error(w, "injected failure after the write", http.StatusInternalServerError)
			return
		case faultClosed:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
	}
	writeFakeJSON(w, http.StatusOK, patched)
}

func authFamily(t *testing.T) []Probe {
	t.Helper()
	var family []Probe
	for _, p := range All() {
		if strings.HasPrefix(p.Name, "authproviders/") {
			family = append(family, p)
		}
	}
	require.NotEmpty(t, family)
	return family
}

func authProbe(t *testing.T, name string) Probe {
	t.Helper()
	for _, p := range authFamily(t) {
		if p.Name == name {
			return p
		}
	}
	require.Failf(t, "no such probe", "%s", name)
	return Probe{}
}

// assertWritesStayWithinTheThreeConfigs asserts every request the fake got is
// a GET of the list or of one of the three configs, or a PATCH of one of the
// three configs with the merge patch content type, and that none touched a
// feature or any other config.
func assertWritesStayWithinTheThreeConfigs(t *testing.T, f *authFake) {
	t.Helper()
	allowed := []string{authProvidersPath}
	for _, name := range authFakeConfigs {
		allowed = append(allowed, authConfigPath(name))
	}
	allowedGets := append([]string{"/rancherversion"}, allowed...)
	for _, r := range f.log() {
		switch r.method {
		case http.MethodGet:
			assert.Contains(t, allowedGets, r.path, "GET")
		case http.MethodPatch:
			assert.Contains(t, allowed[1:], r.path, "PATCH")
			assert.Equal(t, mergePatchContentType, r.contentType, "PATCH %s", r.path)
		default:
			assert.Failf(t, "a request that is neither a GET nor a PATCH", "%s %s", r.method, r.path)
		}
		assert.NotContains(t, r.path, "/features", "no request reads or writes a feature")
	}
}

func patchCount(f *authFake) int {
	n := 0
	for _, r := range f.log() {
		if r.method == http.MethodPatch {
			n++
		}
	}
	return n
}

// TestEveryAuthProvidersProbeThatWritesIsOracleOnly asserts every probe of
// the authproviders family other than only-local-enabled is oracle-only, and
// that only-local-enabled has no teardown, is not oracle-only, and sends
// nothing but GET requests when run.
func TestEveryAuthProvidersProbeThatWritesIsOracleOnly(t *testing.T) {
	t.Parallel()
	for _, p := range authFamily(t) {
		if p.Name == onlyLocalProbe {
			continue
		}
		assert.True(t, p.OracleOnly, "%s writes to the server and the facade has no auth config to write to", p.Name)
	}
	only := authProbe(t, onlyLocalProbe)
	assert.Empty(t, only.Teardown)
	assert.False(t, only.OracleOnly)
	assert.NotEmpty(t, only.FacadeDivergence)

	f := newAuthFake(t, "enabled absent")
	results, err := f.run(t, only, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, results, 1)
	for _, r := range f.log() {
		assert.Equal(t, http.MethodGet, r.method, r.path)
	}
	assert.Equal(t, f.start, f.endState())
}

type wantRequest struct {
	method string
	path   string
	body   map[string]any
}

func annotationPatch(value string) map[string]any {
	return map[string]any{"annotations": map[string]any{authCleanupAnnotation: value}}
}

// enableBody and restoreBody are written out here, not read from the probe's
// edits, so a change to a field the probe sets shows as a difference.
func enableBody(fields map[string]any) map[string]any {
	body := map[string]any{"enabled": true, "metadata": annotationPatch(authCleanupUserLocked)}
	maps.Copy(body, fields)
	return body
}

func restoreBody(enabled any, fields ...string) map[string]any {
	body := map[string]any{"enabled": enabled, "metadata": annotationPatch(authCleanupLocked)}
	for _, field := range fields {
		body[field] = nil
	}
	return body
}

// TestTheExternalProvidersProbeSendsExactlyTheRequestsItNames asserts, for a
// config seeded with enabled absent and with enabled false, the request list
// of the probe's setup, observation and teardown against the fake: the
// request for the server version that the MaxMinor gate sends first, the
// reads of the originals, one merge patch per config that enables it, one
// that restores it in reverse order with an absent original sent as null and
// a false original sent as false, the read back, and the end state equal to
// the start with no cleanup event.
func TestTheExternalProvidersProbeSendsExactlyTheRequestsItNames(t *testing.T) {
	t.Parallel()
	for _, seed := range authFakeSeeds {
		t.Run(seed, func(t *testing.T) {
			t.Parallel()
			f := newAuthFake(t, seed)
			results, err := f.run(t, authProbe(t, externalProbe), 5*time.Second)
			require.NoError(t, err)
			require.Len(t, results, 1)

			var originalEnabled any
			if seed == "enabled false" {
				originalEnabled = false
			}
			azure := map[string]any{
				"tenantId": "e2e-fid-tenant", "applicationId": "e2e-fid-app",
				"endpoint":           "https://e2e-fid-azure.invalid/",
				"authEndpoint":       "https://e2e-fid-azure.invalid/e2e-fid-tenant/oauth2/v2.0/authorize",
				"tokenEndpoint":      "https://e2e-fid-azure.invalid/e2e-fid-tenant/oauth2/v2.0/token",
				"deviceAuthEndpoint": "https://e2e-fid-azure.invalid/e2e-fid-tenant/oauth2/v2.0/devicecode",
				"rancherUrl":         "https://e2e-fid-rancher.invalid",
			}
			azureFields := slices.Sorted(maps.Keys(azure))
			want := []wantRequest{
				{http.MethodGet, "/rancherversion", nil},
				{http.MethodGet, authProvidersPath, nil},
				{http.MethodGet, authConfigPath("openldap"), nil},
				{http.MethodGet, authConfigPath("okta"), nil},
				{http.MethodGet, authConfigPath("azuread"), nil},
				{http.MethodPatch, authConfigPath("openldap"), enableBody(map[string]any{"servers": []any{"e2e-fid-ldap.invalid"}})},
				{http.MethodPatch, authConfigPath("okta"), enableBody(map[string]any{"rancherApiHost": "https://e2e-fid-okta.invalid"})},
				{http.MethodPatch, authConfigPath("azuread"), enableBody(azure)},
				{http.MethodGet, authProvidersPath, nil},
				{http.MethodGet, authProvidersPath, nil},
				{http.MethodPatch, authConfigPath("azuread"), restoreBody(originalEnabled, azureFields...)},
				{http.MethodPatch, authConfigPath("okta"), restoreBody(originalEnabled, "rancherApiHost")},
				{http.MethodPatch, authConfigPath("openldap"), restoreBody(originalEnabled, "servers")},
				{http.MethodGet, authConfigPath("openldap"), nil},
				{http.MethodGet, authConfigPath("okta"), nil},
				{http.MethodGet, authConfigPath("azuread"), nil},
				{http.MethodGet, authProvidersPath, nil},
			}
			var got []wantRequest
			for _, r := range f.log() {
				got = append(got, wantRequest{r.method, r.path, r.body})
				if r.method == http.MethodPatch {
					assert.Equal(t, mergePatchContentType, r.contentType, r.path)
				}
			}
			assert.Equal(t, want, got)
			assert.Equal(t, f.start, f.endState())
			assertNoControllerEvent(t, f)
			assertWritesStayWithinTheThreeConfigs(t, f)

			assert.Equal(t, 200, results[0].Status)
			data, _ := results[0].Body["data"].([]any)
			var ids []string
			for _, e := range data {
				ids = append(ids, e.(map[string]any)["id"].(string))
			}
			assert.Equal(t, []string{"azuread", "local", "okta", "openldap"}, ids, "the probe sorts the recorded list by id")
		})
	}
}

type faultRow struct {
	name      string
	fault     authFault
	phase     string
	object    string
	isRestore bool
}

// faultRows computes the failure points of probe p from the requests of a
// clean run: every PATCH in each of the five modes, and every GET answered
// 500. The phase of a PATCH is the one the clean run recorded for it.
func faultRows(clean []authRequest) []faultRow {
	var rows []faultRow
	patches, gets := 0, 0
	for _, r := range clean {
		switch r.method {
		case http.MethodPatch:
			patches++
			object := strings.TrimPrefix(r.path, authConfigsPath+"/")
			for _, mode := range []authFaultMode{faultRefused, faultUnavailable503, faultTooMany429, faultApplied500, faultClosed} {
				rows = append(rows, faultRow{
					name:      fmt.Sprintf("PATCH %d %s (%s %s)", patches, mode, r.phase, object),
					fault:     authFault{http.MethodPatch, patches, mode},
					phase:     r.phase,
					object:    object,
					isRestore: r.phase == "teardown",
				})
			}
		case http.MethodGet:
			gets++
			rows = append(rows, faultRow{
				name:  fmt.Sprintf("GET %d %s (%s %s)", gets, fault500, r.phase, r.path),
				fault: authFault{http.MethodGet, gets, fault500},
				phase: r.phase,
			})
		}
	}
	return rows
}

// TestAuthProvidersProbesRestoreWhatTheyWroteAtEveryFailurePoint asserts, for
// every probe of the authproviders family and for configs seeded with enabled
// absent and with enabled false, that the end state of the fake equals its
// start state after a run in which the k-th PATCH is refused (403), answered
// 503 or 429 without being applied, applied and answered 500, or applied and
// its connection closed, for every k of a clean run, and the k-th GET is answered
// 500, for every k. The failure points are counted from a clean run, not
// listed. A restore that fails with a 503, a 429, a 500 or a closed connection is
// sent again and the run succeeds. The one exception is a restore refused:
// the run then returns an error that names the restore step, every other
// config equals its start state, and the refused config is the only
// difference. Every run records no unlock and no cleanup event and writes
// only the three configs, by merge patch.
func TestAuthProvidersProbesRestoreWhatTheyWroteAtEveryFailurePoint(t *testing.T) {
	t.Parallel()
	for _, p := range authFamily(t) {
		short := strings.TrimPrefix(p.Name, "authproviders/")
		for _, seed := range authFakeSeeds {
			clean := newAuthFake(t, seed)
			_, err := clean.run(t, p, 5*time.Second)
			assertWritesStayWithinTheThreeConfigs(t, clean)
			assertNoControllerEvent(t, clean, "%s %s: Rancher never unlocks, resets or cleans up a config", short, seed)
			require.NoError(t, err, "%s %s: a run with no failure", short, seed)
			require.Equal(t, clean.start, clean.endState(), "%s %s: a run with no failure ends where it started", short, seed)
			rows := faultRows(clean.log())
			require.NotEmpty(t, rows)

			for _, row := range rows {
				t.Run(fmt.Sprintf("%s %s %s", short, seed, row.name), func(t *testing.T) {
					t.Parallel()
					f := newAuthFake(t, seed, withFault(row.fault))
					results, err := f.run(t, p, time.Second)
					if row.isRestore && row.fault.method == http.MethodPatch && row.fault.mode != faultRefused {
						require.NoError(t, err, "a restore is sent again after a transport error or a 5xx answer")
					}
					if err == nil {
						// The observation records a 500 answer as its result; any other
						// failed request fails the run.
						require.Len(t, results, 1)
						assert.True(t, row.fault.method == http.MethodGet || (row.isRestore && row.fault.mode != faultRefused),
							"only an observation that records a 500 or a restore sent again ends without an error")
						if row.fault.method == http.MethodGet {
							assert.Equal(t, http.StatusInternalServerError, results[0].Status)
						}
					}
					assert.True(t, f.injected, "the failure point was reached")
					assertNoControllerEvent(t, f, "Rancher never unlocks, resets or cleans up a config")
					assertWritesStayWithinTheThreeConfigs(t, f)

					if row.isRestore && row.fault.mode == faultRefused {
						require.ErrorContains(t, err, "restore auth config "+row.object)
						end := f.endState()
						for _, name := range authFakeConfigs {
							if name == row.object {
								assert.NotEqual(t, f.start[name], end[name], "the config whose restore was refused stays as written")
								continue
							}
							assert.Equal(t, f.start[name], end[name], name)
						}
						return
					}
					assert.Equal(t, f.start, f.endState())
				})
			}
		}
	}
}

// TestPreconditionFailsWhenAnotherProviderIsEnabled asserts a run on a server
// that lists another provider beside local fails in the setup, names the ids
// it found, writes nothing, and gives the instruction that fits: for an id
// these probes do not enable, that they did not enable it; for one of the
// three configs they enable, to disable it again.
func TestPreconditionFailsWhenAnotherProviderIsEnabled(t *testing.T) {
	t.Parallel()
	for _, name := range []string{onlyLocalProbe, externalProbe} {
		for _, seed := range authFakeSeeds {
			t.Run(name+" "+seed+" github", func(t *testing.T) {
				t.Parallel()
				f := newAuthFake(t, seed, withGithubEnabled())
				_, err := f.run(t, authProbe(t, name), 5*time.Second)
				require.ErrorContains(t, err, `holds ["github" "local"], want local alone`)
				require.ErrorContains(t, err, `these probes did not enable ["github"]`)
				assert.NotContains(t, err.Error(), "authconfigs/")
				assert.Zero(t, patchCount(f))
				assert.Equal(t, f.start, f.endState())
				assertWritesStayWithinTheThreeConfigs(t, f)
				assertNoControllerEvent(t, f)
			})
			t.Run(name+" "+seed+" okta left enabled", func(t *testing.T) {
				t.Parallel()
				f := newAuthFake(t, seed, withDoc("okta", func(d map[string]any) { d["enabled"] = true }))
				_, err := f.run(t, authProbe(t, name), 5*time.Second)
				require.ErrorContains(t, err, `holds ["local" "okta"], want local alone`)
				require.ErrorContains(t, err, "a previous run may have left authconfigs/okta enabled: disable it again with annotation management.cattle.io/auth-provider-cleanup set to rancher-locked")
				assert.NotContains(t, err.Error(), "did not enable")
				assert.Zero(t, patchCount(f))
			})
		}
	}
}

// TestReadingTheOriginalsFailsBeforeAnyWrite asserts the external providers
// probe sends no PATCH, and leaves every config as it was, when a config is
// missing (a fresh Rancher v2.16 server creates only local), holds a value
// of enabled other than absent or false (one the list does not show),
// carries another cleanup annotation, already holds a field the probe sets,
// or is unreadable partway through the reads.
func TestReadingTheOriginalsFailsBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	for _, seed := range authFakeSeeds {
		for _, tc := range []struct {
			name string
			opt  func(*authFake)
			want string
		}{
			{"okta does not exist", withoutConfig("okta"), "auth config okta does not exist on this server (a fresh Rancher v2.16 server creates only local)"},
			{"openldap is enabled as text", withDoc("openldap", func(d map[string]any) { d["enabled"] = "true" }), `auth config openldap is not disabled: enabled is true`},
			{"okta carries user-locked", withDoc("okta", func(d map[string]any) {
				d["metadata"].(map[string]any)["annotations"].(map[string]any)[authCleanupAnnotation] = authCleanupUserLocked
			}), `auth config okta has annotation management.cattle.io/auth-provider-cleanup "user-locked"`},
			{"openldap has servers", withDoc("openldap", func(d map[string]any) { d["servers"] = []any{"ldap.example"} }), "auth config openldap already has field servers"},
			{"azuread has a tenant", withDoc("azuread", func(d map[string]any) { d["tenantId"] = "t" }), "auth config azuread already has field tenantId"},
			{"the second read fails", withFault(authFault{http.MethodGet, 4, fault500}), "reading auth config okta: status 500"},
		} {
			t.Run(seed+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				f := newAuthFake(t, seed, tc.opt)
				_, err := f.run(t, authProbe(t, externalProbe), 5*time.Second)
				require.ErrorContains(t, err, tc.want)
				assert.Zero(t, patchCount(f), "nothing is written")
				assert.Equal(t, f.start, f.endState())
				if tc.name == "okta does not exist" {
					assert.NotContains(t, f.endState(), "okta", "no config is created")
				}
			})
		}
	}
}

// TestTheTeardownSendsNothingForAConfigItDidNotWrite asserts every teardown
// step of the external providers probe returns nil and sends no request when
// no enable step ran, and that a config whose enable step ran but whose
// original was not recorded fails its restore and the read back without a
// request, while the restore of every other config still sends nothing.
func TestTheTeardownSendsNothingForAConfigItDidNotWrite(t *testing.T) {
	t.Parallel()
	p := authProbe(t, externalProbe)
	f := newAuthFake(t, "enabled absent")
	for _, s := range p.Teardown {
		require.NoError(t, s.Do(t.Context(), f.target(), map[string]string{}), s.Name)
	}
	assert.Empty(t, f.log())

	captured := map[string]string{authConfigWroteKey("okta"): "true"}
	for _, s := range p.Teardown {
		switch s.Name {
		case "restore auth config okta", "read back the auth configs":
			require.ErrorContains(t, s.Do(t.Context(), f.target(), captured), "auth config okta was written but its original was not recorded", s.Name)
		case "wait for the auth provider list to hold local alone":
		default:
			require.NoError(t, s.Do(t.Context(), f.target(), captured), s.Name)
		}
	}
	assert.Empty(t, f.log())
}

// TestTheReadBackFindsAFieldTheRestoreLeft asserts the step reads each config
// the probe wrote and fails when enabled, the annotation or a field the probe
// set differs from its original, treats an absent enabled and a false one as
// the same value, ignores every field the probe does not set, and fails at
// once on a non-200 answer.
func TestTheReadBackFindsAFieldTheRestoreLeft(t *testing.T) {
	t.Parallel()
	readBack := readBackAuthConfigs(authConfigEdits)
	original := func(t *testing.T, f *authFake) map[string]string {
		t.Helper()
		captured := map[string]string{}
		require.NoError(t, readAuthConfigOriginals(authConfigEdits).Do(t.Context(), f.target(), captured))
		captured[authConfigWroteKey("okta")] = "true"
		return captured
	}
	t.Run("a restored config passes, whatever else Rancher added", func(t *testing.T) {
		t.Parallel()
		f := newAuthFake(t, "enabled absent")
		captured := original(t, f)
		f.docs["okta"]["enabled"] = false
		f.docs["okta"]["groupsField"] = ""
		f.docs["okta"]["status"] = map[string]any{"conditions": []any{}}
		require.NoError(t, readBack.Do(t.Context(), f.target(), captured))
	})
	for name, change := range map[string]func(doc map[string]any){
		"enabled is true": func(d map[string]any) { d["enabled"] = true },
		"the annotation changed": func(d map[string]any) {
			d["metadata"].(map[string]any)["annotations"].(map[string]any)[authCleanupAnnotation] = "unlocked"
		},
		"a field was left": func(d map[string]any) { d["rancherApiHost"] = "https://e2e-fid-okta.invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newAuthFake(t, "enabled false")
			captured := original(t, f)
			change(f.docs["okta"])
			err := readBack.Do(t.Context(), f.target(), captured)
			require.ErrorContains(t, err, "auth config okta after the restore")
		})
	}
	t.Run("a non-200 answer fails at once", func(t *testing.T) {
		t.Parallel()
		f := newAuthFake(t, "enabled false")
		captured := original(t, f)
		delete(f.docs, "okta")
		require.ErrorContains(t, readBack.Do(t.Context(), f.target(), captured), "reading back auth config okta: status 404")
	})
}

// TestWaitingForTheListPollsAndNamesTheLastList asserts the wait step reads
// the list again until it holds exactly the wanted ids (a list with another
// entry besides them is not ready), fails with the last list it read when its
// context ends, adds that local is missing only when local was wanted and the
// last list lacks it, and fails at once on a status other than 200.
func TestWaitingForTheListPollsAndNamesTheLastList(t *testing.T) {
	t.Parallel()
	serve := func(t *testing.T, answers ...string) (*fakeTarget, *int) {
		t.Helper()
		var mu sync.Mutex
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			answer := answers[min(calls, len(answers)-1)]
			calls++
			if answer == "503" {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, answer)
		}))
		t.Cleanup(srv.Close)
		return &fakeTarget{srv: srv}, &calls
	}
	local := `{"data":[{"id":"local"}]}`
	both := `{"data":[{"id":"openldap"},{"id":"local"}]}`
	step := authProvidersAreListed("wait", []string{"local", "openldap"}, 5*time.Millisecond)

	t.Run("polls until the list is complete", func(t *testing.T) {
		t.Parallel()
		tgt, calls := serve(t, local, local, both)
		require.NoError(t, step.Do(t.Context(), tgt, nil))
		assert.Equal(t, 3, *calls)
	})
	superset := `{"data":[{"id":"local"},{"id":"openldap"},{"id":"github"}]}`
	t.Run("a list with another entry is not ready", func(t *testing.T) {
		t.Parallel()
		tgt, calls := serve(t, superset, both)
		require.NoError(t, step.Do(t.Context(), tgt, nil))
		assert.Equal(t, 2, *calls)
	})
	t.Run("names a list with another entry", func(t *testing.T) {
		t.Parallel()
		tgt, _ := serve(t, superset)
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		require.ErrorContains(t, step.Do(ctx, tgt, nil), `the last list held ["github" "local" "openldap"], want ["local" "openldap"]`)
	})
	t.Run("names the last list", func(t *testing.T) {
		t.Parallel()
		tgt, _ := serve(t, local)
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		err := step.Do(ctx, tgt, nil)
		require.ErrorContains(t, err, `the last list held ["local"], want ["local" "openldap"]`)
		assert.NotContains(t, err.Error(), "local is missing")
	})
	t.Run("says when local is missing", func(t *testing.T) {
		t.Parallel()
		tgt, _ := serve(t, `{"data":[{"id":"openldap"}]}`)
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		require.ErrorContains(t, step.Do(ctx, tgt, nil), "local is missing: the hide-local-auth-provider feature may be on")
	})
	t.Run("fails at once on another status", func(t *testing.T) {
		t.Parallel()
		tgt, calls := serve(t, "503")
		require.ErrorContains(t, step.Do(t.Context(), tgt, nil), "status 503, want 200")
		assert.Equal(t, 1, *calls)
	})
}

// TestTheFakeAppliesRanchersRules asserts the fake the guards stand on: an
// enabled config still marked rancher-locked becomes unlocked, an enabled
// config with no annotation becomes unlocked, a disabled config marked
// unlocked records a cleanup event, an enabled LDAP config with no server is
// refused and left as it was, and a PATCH with another content type or to
// another config is refused. It also asserts the one state where the fake is
// stricter than Rancher: an enabled config marked unlocked, which Rancher's
// controller leaves alone, records an unlock event.
func TestTheFakeAppliesRanchersRules(t *testing.T) {
	t.Parallel()
	f := newAuthFake(t, "enabled absent", withGithubEnabled())
	tgt := f.target()
	patch := func(name string, body map[string]any) int {
		status, _, err := mergePatchAdmin(t.Context(), tgt, authConfigPath(name), body)
		require.NoError(t, err)
		return status
	}
	annotation := func(name string) any {
		return f.endState()[name]["metadata"].(map[string]any)["annotations"].(map[string]any)[authCleanupAnnotation]
	}

	assert.Equal(t, 200, patch("okta", enableBody(map[string]any{"rancherApiHost": "h"})))
	assert.Equal(t, authCleanupUserLocked, annotation("okta"))
	assert.Empty(t, f.cleanupEvents())
	assert.Empty(t, f.unlockEvents())

	assert.Equal(t, 200, patch("azuread", map[string]any{"enabled": true}))
	assert.Equal(t, "unlocked", annotation("azuread"), "enabled and rancher-locked becomes unlocked")
	assert.Equal(t, []string{"azuread"}, f.unlockEvents())
	assert.Equal(t, 200, patch("azuread", map[string]any{"enabled": nil}))
	assert.Equal(t, []string{"azuread"}, f.cleanupEvents(), "disabled and unlocked is cleaned up")

	before := f.endState()["openldap"]
	assert.Equal(t, 400, patch("openldap", map[string]any{"enabled": true}))
	assert.Equal(t, before, f.endState()["openldap"])
	assert.Equal(t, 200, patch("openldap", enableBody(map[string]any{"servers": []any{"s"}})))

	assert.Equal(t, 200, patch("okta", map[string]any{"metadata": map[string]any{"annotations": map[string]any{authCleanupAnnotation: nil}}}))
	assert.Equal(t, "unlocked", annotation("okta"), "enabled with no annotation becomes unlocked")
	assert.Equal(t, []string{"azuread", "okta"}, f.unlockEvents())
	assert.Equal(t, 200, patch("openldap", map[string]any{"metadata": map[string]any{"annotations": map[string]any{authCleanupAnnotation: "unlocked"}}}))
	assert.Equal(t, []string{"azuread", "okta", "openldap"}, f.unlockEvents(), "enabled and unlocked records an unlock event (stricter than Rancher)")

	assert.Equal(t, 404, patch("github", map[string]any{"enabled": nil}))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, f.srv.URL+authConfigPath("okta"), strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.srv.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)
}

// TestARestoreIsSentAgainOnlyAfterATransportErrorOrA5xxAnswer asserts a
// restore refused with 403 is sent once and fails the run, and one answered
// 503, 429 or 500, or whose connection closes, is sent a second time and
// succeeds, with the config back as it started.
func TestARestoreIsSentAgainOnlyAfterATransportErrorOrA5xxAnswer(t *testing.T) {
	t.Parallel()
	// The fourth PATCH of a clean run is the first restore.
	for _, tc := range []struct {
		mode     authFaultMode
		sends    int
		wantFail bool
	}{
		{faultRefused, 1, true},
		{faultUnavailable503, 2, false},
		{faultTooMany429, 2, false},
		{faultApplied500, 2, false},
		{faultClosed, 2, false},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Parallel()
			f := newAuthFake(t, "enabled absent", withFault(authFault{http.MethodPatch, 4, tc.mode}))
			_, err := f.run(t, authProbe(t, externalProbe), time.Second)
			sends := 0
			for _, r := range f.log() {
				if r.method == http.MethodPatch && r.phase == "teardown" && r.path == authConfigPath("azuread") {
					sends++
				}
			}
			assert.Equal(t, tc.sends, sends)
			if tc.wantFail {
				require.ErrorContains(t, err, "restore auth config azuread")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, f.start, f.endState())
		})
	}
}

// TestARestoreThatAlwaysFailsIsSentAtMostFourTimes asserts a restore the
// server answers 503 every time is sent exactly authRestoreAttempts times,
// fails the run with an error that names the restore step, leaves the other
// two configs restored, and causes no unlock or cleanup event.
func TestARestoreThatAlwaysFailsIsSentAtMostFourTimes(t *testing.T) {
	t.Parallel()
	require.Equal(t, 4, authRestoreAttempts)
	for _, seed := range authFakeSeeds {
		t.Run(seed, func(t *testing.T) {
			t.Parallel()
			f := newAuthFake(t, seed, func(f *authFake) { f.failRestoresOf = "okta" })
			_, err := f.run(t, authProbe(t, externalProbe), 3*time.Second)
			require.ErrorContains(t, err, "restore auth config okta")
			sends := 0
			for _, r := range f.log() {
				if r.method == http.MethodPatch && r.phase == "teardown" && r.path == authConfigPath("okta") {
					sends++
				}
			}
			assert.Equal(t, authRestoreAttempts, sends)
			end := f.endState()
			for _, name := range authFakeConfigs {
				if name == "okta" {
					assert.NotEqual(t, f.start[name], end[name], "the config whose restore never succeeded stays as written")
					continue
				}
				assert.Equal(t, f.start[name], end[name], name)
			}
			assertNoControllerEvent(t, f)
		})
	}
}

// TestOnlyTheExternalProvidersProbeDeclaresMaxMinor asserts the probe that
// writes to the server declares the newest Rancher minor it runs on, 2.15
// (a fresh Rancher v2.16 server creates the auth config of local only), and
// the probe that only reads sets none.
func TestOnlyTheExternalProvidersProbeDeclaresMaxMinor(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "2.15", authProbe(t, externalProbe).MaxMinor)
	assert.Empty(t, authProbe(t, onlyLocalProbe).MaxMinor)
	for _, p := range authFamily(t) {
		require.NoError(t, p.Validate(), p.Name)
	}
}

// TestExternalProvidersProbeDoesNotRunAgainstRancher216 asserts a server that
// reports v2.16 makes the probe return an error that wraps ErrNotForMinor,
// send only the request for the server version, and leave every config
// unchanged, with no unlock or cleanup event.
func TestExternalProvidersProbeDoesNotRunAgainstRancher216(t *testing.T) {
	t.Parallel()
	for _, seed := range authFakeSeeds {
		for _, version := range []string{"v2.16.0", "v2.16.0-alpha1", "v3.0.0", "v2.16-0123456789abcdef0123456789abcdef01234567-head"} {
			t.Run(seed+" "+version, func(t *testing.T) {
				t.Parallel()
				f := newAuthFake(t, seed, withServerVersion(version))
				_, err := f.run(t, authProbe(t, externalProbe), 5*time.Second)
				require.ErrorIs(t, err, ErrNotForMinor)
				var got []string
				for _, r := range f.log() {
					got = append(got, r.method+" "+r.path)
				}
				assert.Equal(t, []string{"GET /rancherversion"}, got)
				assert.Equal(t, f.start, f.endState())
				assertNoControllerEvent(t, f)
			})
		}
	}
}

// TestExternalProvidersProbeRunsAgainstRancher215Builds asserts a server that
// reports a v2.15 build other than a release (a head build, a release
// candidate) is a server the probe runs on: the request for the version is
// followed by the read of the list in the precondition, and the run ends with
// every config unchanged.
func TestExternalProvidersProbeRunsAgainstRancher215Builds(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"v2.15.3-rc1", "v2.15-0123456789abcdef0123456789abcdef01234567-head"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			f := newAuthFake(t, "enabled absent", withServerVersion(version))
			_, err := f.run(t, authProbe(t, externalProbe), 5*time.Second)
			require.NoError(t, err)
			log := f.log()
			require.GreaterOrEqual(t, len(log), 2)
			assert.Equal(t, "GET /rancherversion", log[0].method+" "+log[0].path)
			assert.Equal(t, "GET "+authProvidersPath, log[1].method+" "+log[1].path)
			assert.Equal(t, f.start, f.endState())
		})
	}
}

// TestAuthProvidersProbeRestoresWhenTheCallerContextIsCancelled asserts the
// probe that enables the three auth configs puts every one back when the
// caller's context is cancelled right after each enable step and after the
// step that waits for the list: the end state equals the start state, Rancher's
// controller finds nothing to act on, only the three configs are written, the
// error carries the cancellation, and no restore or other teardown step fails.
func TestAuthProvidersProbeRestoresWhenTheCallerContextIsCancelled(t *testing.T) {
	t.Parallel()
	const enablePrefix = "enable auth config "
	const listStep = "wait for the enabled auth providers to be listed"
	p := authProbe(t, "authproviders/external-providers-enabled")
	var points []string
	for _, s := range p.Setup {
		if strings.HasPrefix(s.Name, enablePrefix) || s.Name == listStep {
			points = append(points, s.Name)
		}
	}
	require.Len(t, points, len(authFakeConfigs)+1, "three enable steps and the wait for the list")

	for _, point := range points {
		for _, seed := range authFakeSeeds {
			t.Run(point+" "+seed, func(t *testing.T) {
				t.Parallel()
				f := newAuthFake(t, seed)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cancelling := p
				cancelling.Setup = slices.Clone(p.Setup)
				for i, s := range cancelling.Setup {
					if s.Name != point {
						continue
					}
					cancelling.Setup[i] = Step{Name: s.Name, Do: func(ctx context.Context, tgt Target, captured map[string]string) error {
						err := s.Do(ctx, tgt, captured)
						cancel()
						return err
					}}
				}
				_, err := runProbe(ctx, f.target(), f.phased(cancelling), 5*time.Second)
				require.ErrorIs(t, err, context.Canceled)
				assert.NotContains(t, err.Error(), ": teardown ", "no teardown step failed")
				assert.NotContains(t, err.Error(), "restore auth config")
				assert.Equal(t, f.start, f.endState(), "every config is back where it started")
				assertNoControllerEvent(t, f, "Rancher never unlocks, resets or cleans up a config")
				assertWritesStayWithinTheThreeConfigs(t, f)
			})
		}
	}
}
