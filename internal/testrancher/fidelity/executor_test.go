package fidelity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/testtransport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeTarget struct {
	srv        *httptest.Server
	bearers    map[Identity]string
	downstream string
	// client, when set, replaces the server's own client.
	client *http.Client
}

func (f *fakeTarget) BaseURL() string { return f.srv.URL }
func (f *fakeTarget) Client() *http.Client {
	if f.client != nil {
		return f.client
	}
	return f.srv.Client()
}
func (f *fakeTarget) Bearer(_ context.Context, id Identity) (string, error) {
	return f.bearers[id], nil
}
func (f *fakeTarget) Downstream() string { return f.downstream }

func TestRunProbeThreadsCapturesAndNormalizes(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /things", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{"name": "e2e-fid-1", "resourceVersion": "42"},
		})
	})
	mux.HandleFunc("GET /things/e2e-fid-1", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer admin-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "e2e-fid-1"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "admin-token"}}

	p := Probe{
		Name: "fam/capture",
		Observations: []Observation{
			{
				Name:     "create",
				Request:  RequestSpec{Method: "POST", Path: "/things", Body: `{"spec":{}}`, Identity: IdentityAdmin},
				Captures: map[string]Capture{"name": {Path: "metadata.name"}},
			},
			{
				Name:    "get",
				Request: RequestSpec{Method: "GET", Path: "/things/{name}", Identity: IdentityAdmin},
			},
		},
	}
	results, err := RunProbe(t.Context(), tgt, p)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, http.StatusCreated, results[0].Status)
	assert.Equal(t, "<rv>", results[0].Body["metadata"].(map[string]any)["resourceVersion"],
		"default normalization applies to recorded bodies")
	assert.Equal(t, http.StatusOK, results[1].Status)
}

func TestRunProbeExtBearerNameCapture(t *testing.T) {
	t.Parallel()
	var gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tokens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{"bearerToken": "ext/fac-token-7:fac-secret-7"},
		})
	})
	mux.HandleFunc("GET /tokens/fac-token-7", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "fac-token-7"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	p := Probe{
		Name: "fam/ext-capture",
		Observations: []Observation{
			{
				Name:     "create",
				Request:  RequestSpec{Method: "POST", Path: "/tokens", Body: `{"spec":{}}`, Identity: IdentityAdmin},
				Captures: map[string]Capture{"tokname": {Path: "status.bearerToken", ExtBearerName: true}},
			},
			{Name: "get", Request: RequestSpec{Method: "GET", Path: "/tokens/{tokname}", Identity: IdentityAdmin}},
		},
	}
	results, err := RunProbe(t.Context(), tgt, p)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "/tokens/fac-token-7", gotPath,
		"the named request must reach the created record via the bearer-derived name")
	assert.Equal(t, http.StatusOK, results[1].Status)
}

func TestCapturesResolveBeforeNormalizationScrubs(t *testing.T) {
	t.Parallel()
	var gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /things", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{"resourceVersion": "42"},
			"status":   map[string]any{"bearerToken": "ext/tok-9:sec-9"},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	p := Probe{
		Name: "fam/order",
		Observations: []Observation{
			{
				Name:    "create",
				Request: RequestSpec{Method: "POST", Path: "/things", Body: `{}`, Identity: IdentityAdmin},
				Captures: map[string]Capture{
					"rv":      {Path: "metadata.resourceVersion"},
					"tokname": {Path: "status.bearerToken", ExtBearerName: true},
				},
			},
			{Name: "use", Request: RequestSpec{Method: "GET", Path: "/use/{rv}/{tokname}", Identity: IdentityAdmin}},
		},
	}
	results, err := RunProbe(t.Context(), tgt, p)
	require.NoError(t, err)
	assert.Equal(t, "/use/42/tok-9", gotPath,
		"captures must read the pre-normalization values")
	meta := results[0].Body["metadata"].(map[string]any)
	st := results[0].Body["status"].(map[string]any)
	assert.Equal(t, "<rv>", meta["resourceVersion"], "the recorded body is still scrubbed")
	assert.Equal(t, "<secret>", st["bearerToken"], "the recorded body never carries the live bearer")
}

func TestRunProbeHeadersForwardAndSubstitute(t *testing.T) {
	t.Parallel()
	var contentType, custom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		custom = r.Header.Get("X-Fixture")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "n-1"}})
	}))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	p := Probe{
		Name: "fam/headers",
		Observations: []Observation{
			{
				Name:     "seed",
				Request:  RequestSpec{Method: "POST", Path: "/x", Body: `{}`, Identity: IdentityAdmin},
				Captures: map[string]Capture{"name": {Path: "metadata.name"}},
			},
			{
				Name: "explicit",
				Request: RequestSpec{
					Method: "PUT", Path: "/x", Body: `{}`,
					Headers:  map[string]string{"Content-Type": "application/json-patch+json", "X-Fixture": "{name}"},
					Identity: IdentityAdmin,
				},
			},
		},
	}
	_, err := RunProbe(t.Context(), tgt, p)
	require.NoError(t, err)
	assert.Equal(t, "application/json-patch+json", contentType,
		"an explicit Content-Type must override the JSON default")
	assert.Equal(t, "n-1", custom, "header values substitute captures")
}

func TestRunProbeObservationTimeout(t *testing.T) {
	t.Parallel()
	stall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-stall
	}))
	defer srv.Close()
	defer close(stall)
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	p := Probe{
		Name: "fam/stall",
		Observations: []Observation{
			{Name: "hang", Request: RequestSpec{Method: "GET", Path: "/x", Identity: IdentityAdmin}},
		},
	}
	_, err := runProbe(t.Context(), tgt, p, 50*time.Millisecond)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "a stalled observation must time out, not hang the suite")
}

func TestRunProbeFixtureStepsAreBounded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	p := Probe{
		Name: "fam/stall-setup",
		Setup: []Step{{
			Name: "block",
			Do: func(ctx context.Context, _ Target, _ map[string]string) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}},
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: "GET", Path: "/x", Identity: IdentityAdmin}},
		},
	}
	_, err := runProbe(t.Context(), tgt, p, 50*time.Millisecond)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded,
		"setup and teardown steps run under the same per-step deadline as observations")
}

func TestRunProbeJoinsTeardownErrorsWithPrimary(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	primary := fmt.Errorf("setup boom")
	p := Probe{
		Name:  "fam/join",
		Setup: []Step{{Name: "explode", Do: func(context.Context, Target, map[string]string) error { return primary }}},
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: "GET", Path: "/x", Identity: IdentityAdmin}},
		},
		Teardown: []Step{
			{Name: "clean-a", Do: func(context.Context, Target, map[string]string) error { return fmt.Errorf("clean-a failed") }},
			{Name: "clean-b", Do: func(context.Context, Target, map[string]string) error { return fmt.Errorf("clean-b failed") }},
		},
	}
	_, err := RunProbe(t.Context(), tgt, p)
	require.Error(t, err)
	require.ErrorContains(t, err, "setup boom", "the primary failure survives")
	require.ErrorContains(t, err, "clean-a failed", "every teardown failure is reported")
	assert.ErrorContains(t, err, "clean-b failed", "later teardown failures are not dropped")
}

func TestIsConnResetClassifiesWrappedErrno(t *testing.T) {
	t.Parallel()
	assert.True(t, isConnReset(fmt.Errorf("Get \"x\": %w", syscall.ECONNRESET)))
	assert.False(t, isConnReset(fmt.Errorf("Get \"x\": %w", io.EOF)))
	assert.False(t, isConnReset(nil))
}

// resetRoundTripper fails its first failFirst calls with a connection reset,
// then succeeds, counting every call.
type resetRoundTripper struct {
	failFirst int
	calls     int
}

func (rt *resetRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	rt.calls++
	if rt.calls <= rt.failFirst {
		return nil, syscall.ECONNRESET
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
	}, nil
}

func TestDoResilientRetriesIdempotentButNeverPost(t *testing.T) {
	t.Parallel()
	build := func(method string) func() (*http.Request, error) {
		return func() (*http.Request, error) {
			return http.NewRequest(method, "https://example.test/x", strings.NewReader("body"))
		}
	}

	t.Run("GET retries once and succeeds", func(t *testing.T) {
		t.Parallel()
		rt := &resetRoundTripper{failFirst: 1}
		resp, err := doResilient(&http.Client{Transport: rt}, build(http.MethodGet))
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, 2, rt.calls, "one reset then one successful retry")
	})

	t.Run("POST never retries", func(t *testing.T) {
		t.Parallel()
		rt := &resetRoundTripper{failFirst: 1}
		_, err := doResilient(&http.Client{Transport: rt}, build(http.MethodPost))
		require.Error(t, err)
		require.ErrorIs(t, err, syscall.ECONNRESET)
		assert.Equal(t, 1, rt.calls,
			"a POST reset surfaces after exactly one attempt so a committed mutation is never double-fired")
	})

	t.Run("second reset on GET surfaces", func(t *testing.T) {
		t.Parallel()
		rt := &resetRoundTripper{failFirst: 2}
		_, err := doResilient(&http.Client{Transport: rt}, build(http.MethodGet))
		require.Error(t, err)
		require.ErrorIs(t, err, syscall.ECONNRESET)
		assert.Equal(t, 2, rt.calls, "the single retry is exhausted; the second reset is returned")
	})
}

func TestIsIdempotent(t *testing.T) {
	t.Parallel()
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodHead} {
		assert.True(t, isIdempotent(m), m)
	}
	assert.False(t, isIdempotent(http.MethodPost))
	assert.False(t, isIdempotent(http.MethodPatch))
}

func TestRunProbeTeardownRunsAfterSetupFailureWithCaptures(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	var tornDown bool
	p := Probe{
		Name: "fam/teardown",
		Setup: []Step{{
			Name: "explode",
			Do: func(context.Context, Target, map[string]string) error {
				return fmt.Errorf("setup boom")
			},
		}},
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: "GET", Path: "/x", Identity: IdentityAdmin}},
		},
		Teardown: []Step{{
			Name: "clean",
			Do: func(_ context.Context, _ Target, captured map[string]string) error {
				tornDown = true
				assert.NotNil(t, captured)
				return nil
			},
		}},
	}
	_, err := RunProbe(t.Context(), tgt, p)
	require.ErrorContains(t, err, "setup boom")
	assert.True(t, tornDown, "teardown must run even when setup fails partway")
}

func TestRunProbeMissingCaptureFailsAndTearsDown(t *testing.T) {
	t.Parallel()
	var dependentRan bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /things", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		dependentRan = true
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	var tornDown bool
	p := Probe{
		Name: "fam/missing",
		Observations: []Observation{
			{
				Name:     "create",
				Request:  RequestSpec{Method: "POST", Path: "/things", Body: `{}`, Identity: IdentityAdmin},
				Captures: map[string]Capture{"name": {Path: "metadata.name"}},
			},
			{Name: "get", Request: RequestSpec{Method: "GET", Path: "/things/{name}", Identity: IdentityAdmin}},
		},
		Teardown: []Step{{
			Name: "clean",
			Do: func(context.Context, Target, map[string]string) error {
				tornDown = true
				return nil
			},
		}},
	}
	results, err := RunProbe(t.Context(), tgt, p)
	require.ErrorContains(t, err, `capture "name"`,
		"a declared capture that cannot be resolved is a probe execution error")
	assert.Len(t, results, 1, "the recorded observation survives for diagnostics")
	assert.False(t, dependentRan, "capture-dependent requests must never hit a synthetic URL")
	assert.True(t, tornDown, "teardown still runs after a capture failure")
}

func TestRunProbeExtBearerCaptureTransformFailureFails(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"bearerToken": "token-abc:secret"}})
	}))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	p := Probe{
		Name: "fam/badbearer",
		Observations: []Observation{
			{
				Name:     "create",
				Request:  RequestSpec{Method: "POST", Path: "/t", Body: `{}`, Identity: IdentityAdmin},
				Captures: map[string]Capture{"tokname": {Path: "status.bearerToken", ExtBearerName: true}},
			},
			{Name: "get", Request: RequestSpec{Method: "GET", Path: "/t/{tokname}", Identity: IdentityAdmin}},
		},
	}
	_, err := RunProbe(t.Context(), tgt, p)
	require.ErrorContains(t, err, "not ext-shaped")
	assert.NotContains(t, err.Error(), "token-abc:secret",
		"a malformed credential is still a live secret; errors never embed it")
}

func TestRunProbeIdentityHeaders(t *testing.T) {
	t.Parallel()
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{}}

	p := Probe{
		Name: "fam/idents",
		Observations: []Observation{
			{Name: "none", Request: RequestSpec{Method: "GET", Path: "/x", Identity: IdentityNone}},
			{Name: "garbage", Request: RequestSpec{Method: "GET", Path: "/x", Identity: IdentityGarbage}},
		},
	}
	_, err := RunProbe(t.Context(), tgt, p)
	require.NoError(t, err)
	require.Len(t, auth, 2)
	assert.Empty(t, auth[0], "IdentityNone sends no Authorization header")
	assert.Equal(t, "Bearer not-a-real-token", auth[1])
}

func TestNameFromExtBearer(t *testing.T) {
	t.Parallel()
	name, ok := NameFromExtBearer("ext/fac-token-3:fac-secret-3")
	require.True(t, ok)
	assert.Equal(t, "fac-token-3", name)
	_, ok = NameFromExtBearer("not-an-ext-bearer")
	assert.False(t, ok)
}

func TestRunProbeSeedsDownstreamCapture(t *testing.T) {
	t.Parallel()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}, downstream: "c-abc123"}

	p := Probe{
		Name:       "fam/downstream",
		Downstream: true,
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: "GET", Path: "/k8s/clusters/{downstream}/things", Identity: IdentityAdmin}},
		},
	}
	_, err := RunProbe(t.Context(), tgt, p)
	require.NoError(t, err)
	assert.Equal(t, "/k8s/clusters/c-abc123/things", gotPath)
}

func TestRunProbeRecordsTrimmedTextForNonJSONBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("  unable to create token: boom  \n"))
	}))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	p := Probe{
		Name: "fam/text",
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: "GET", Path: "/x", Identity: IdentityAdmin}},
		},
	}
	results, err := RunProbe(t.Context(), tgt, p)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "unable to create token: boom", results[0].Text, "the raw body is trimmed")
	assert.Nil(t, results[0].Body, "a non-JSON body is never also decoded")
}

func TestRunProbeRecordsBodyNotTextForJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"a": "b"})
	}))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	p := Probe{
		Name: "fam/json",
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: "GET", Path: "/x", Identity: IdentityAdmin}},
		},
	}
	results, err := RunProbe(t.Context(), tgt, p)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "b", results[0].Body["a"])
	assert.Empty(t, results[0].Text, "a JSON body is never also recorded as text")
}

func TestRunProbeRefusesDownstreamProbeWithoutTarget(t *testing.T) {
	t.Parallel()
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	p := Probe{
		Name:       "fam/downstream",
		Downstream: true,
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: "GET", Path: "/k8s/clusters/{downstream}/things", Identity: IdentityAdmin}},
		},
	}
	_, err := RunProbe(t.Context(), tgt, p)
	require.ErrorContains(t, err, "no downstream cluster")
	assert.False(t, hit, "a downstream probe without a target must never record against a synthetic URL")
}

// TestSubstituteCapturedRewritesKeysAndValues asserts captured values are
// replaced by their placeholder in map keys, string values and nested
// arrays, longest value first.
func TestSubstituteCapturedRewritesKeysAndValues(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"metadata": map[string]any{"labels": map[string]any{
			"c-m-746z46nj-p-e2e-fid-explain_e2e-fid-explain-prtb": "owner-user",
			"authz.cluster.cattle.io/rtb-owner-updated":           "c-m-746z46nj_e2e-fid-explain-crtb",
		}},
		"items": []any{"c-m-746z46nj:p-e2e-fid-explain"},
	}
	substituteCaptured(body, map[string]string{"downstream": "c-m-746z46nj", "short": "c-m"})
	labels := body["metadata"].(map[string]any)["labels"].(map[string]any)
	assert.Equal(t, "owner-user", labels["{downstream}-p-e2e-fid-explain_e2e-fid-explain-prtb"])
	assert.Equal(t, "{downstream}_e2e-fid-explain-crtb", labels["authz.cluster.cattle.io/rtb-owner-updated"])
	assert.Equal(t, []any{"{downstream}:p-e2e-fid-explain"}, body["items"])
}

// TestCaptureValueIndexesArraysAndBracketedKeys asserts a capture path may
// index an array with an integer segment and name a dotted key in
// brackets.
func TestCaptureValueIndexesArraysAndBracketedKeys(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"data":     []any{map[string]any{"id": "grb-1"}, map[string]any{"id": "grb-2"}},
		"metadata": map[string]any{"annotations": map[string]any{"authz.cluster.cattle.io/globalrolebinding": "grb-9"}},
	}
	v, err := captureValue(body, "data.1.id")
	require.NoError(t, err)
	assert.Equal(t, "grb-2", v)
	v, err = captureValue(body, "metadata.annotations.[authz.cluster.cattle.io/globalrolebinding]")
	require.NoError(t, err)
	assert.Equal(t, "grb-9", v)
	_, err = captureValue(body, "data.7.id")
	require.ErrorContains(t, err, `no element "7"`)
}

// TestRunProbeSetupRecordsValuesForRequestsAndTeardown asserts a value a
// setup step records reaches the observations' requests as a {name}
// placeholder, is substituted out of the recorded body like a capture,
// and reaches the teardown.
func TestRunProbeSetupRecordsValuesForRequestsAndTeardown(t *testing.T) {
	t.Parallel()
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"owner":"u-fixture1"}`))
	}))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	var tornDownWith string
	p := Probe{
		Name: "fam/setup-records",
		Setup: []Step{{Name: "create", Do: func(_ context.Context, _ Target, captured map[string]string) error {
			captured["fixtureid"] = "u-fixture1"
			return nil
		}}},
		Observations: []Observation{
			{Name: "post", Request: RequestSpec{Method: http.MethodPost, Path: "/x", Body: `{"user":"{fixtureid}"}`, Identity: IdentityAdmin}},
		},
		SubstituteCaptures: true,
		Teardown: []Step{{Name: "delete", Do: func(_ context.Context, _ Target, captured map[string]string) error {
			tornDownWith = captured["fixtureid"]
			return nil
		}}},
	}
	results, err := RunProbe(t.Context(), tgt, p)
	require.NoError(t, err)
	assert.JSONEq(t, `{"user":"u-fixture1"}`, gotBody)
	require.Len(t, results, 1)
	assert.Equal(t, "{fixtureid}", results[0].Body["owner"])
	assert.Equal(t, "u-fixture1", tornDownWith)
}

// TestRunProbeSendsTheClusterOwnerBearerItsSetupRecorded asserts the
// executor sends the bearer a setup step recorded for the cluster-owner
// identity, and fails the observation when no step recorded one.
func TestRunProbeSendsTheClusterOwnerBearerItsSetupRecorded(t *testing.T) {
	t.Parallel()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv}
	obs := []Observation{{Name: "read", Request: RequestSpec{Method: http.MethodGet, Path: "/x", Identity: IdentityClusterOwner}}}

	p := Probe{Name: "fam/owner", Observations: obs, Setup: []Step{{Name: "owner", Do: func(_ context.Context, _ Target, c map[string]string) error {
		c[clusterOwnerBearer] = "owner-token"
		return nil
	}}}}
	_, err := RunProbe(t.Context(), tgt, p)
	require.NoError(t, err)
	assert.Equal(t, "Bearer owner-token", got)

	_, err = RunProbe(t.Context(), tgt, Probe{Name: "fam/no-owner", Observations: obs})
	require.ErrorContains(t, err, "the probe's setup must create the cluster owner")
}

// shortWait shortens the WaitForStatus bound and interval for one test.
// Tests that call it set package state and must not run in parallel.
func shortWait(t *testing.T, timeout, interval time.Duration) {
	t.Helper()
	oldTimeout, oldInterval := waitForStatusTimeout, waitForStatusInterval
	waitForStatusTimeout, waitForStatusInterval = timeout, interval
	t.Cleanup(func() { waitForStatusTimeout, waitForStatusInterval = oldTimeout, oldInterval })
}

// statusSequence answers each request with the next status of seq and
// repeats the last one; it counts the requests it served.
func statusSequence(t *testing.T, seq ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(n.Add(1)) - 1
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(seq[min(i, len(seq)-1)])
		_, _ = fmt.Fprintf(w, `{"attempt":%d}`, i+1)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func waitProbe(status int) Probe {
	return Probe{Name: "fam/wait", Observations: []Observation{
		{Name: "read", Request: RequestSpec{Method: http.MethodGet, Path: "/x", Identity: IdentityAdmin}, WaitForStatus: status},
	}}
}

// TestWaitForStatusResendsUntilTheStatusMatches asserts that an observation
// with WaitForStatus is re-sent until the answer has exactly that status,
// in either direction, and that the matching answer is the one recorded.
// It fails when the loop is removed, when it stops on any status change,
// or when it waits only while the answer is 200.
func TestWaitForStatusResendsUntilTheStatusMatches(t *testing.T) {
	shortWait(t, 2*time.Second, 10*time.Millisecond)
	for _, tc := range []struct {
		name string
		seq  []int
		want int
	}{
		{"accepted then refused", []int{200, 200, 401}, 401},
		{"another status is not the one waited for", []int{200, 500, 401}, 401},
		{"a status of the same class is not the one waited for", []int{200, 404, 401}, 401},
		{"refused then accepted", []int{401, 401, 200}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, n := statusSequence(t, tc.seq...)
			tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
			results, err := runProbe(t.Context(), tgt, waitProbe(tc.want), 5*time.Second)
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, tc.want, results[0].Status)
			assert.InDelta(t, 3, results[0].Body["attempt"], 0, "the third answer is the one recorded")
			assert.Equal(t, int32(3), n.Load())
		})
	}
}

// TestWaitForStatusSendsOnceWhenTheFirstAnswerMatches asserts that a
// matching first answer is recorded at once. It fails when the loop
// sleeps before the first send or sends again after a match.
func TestWaitForStatusSendsOnceWhenTheFirstAnswerMatches(t *testing.T) {
	shortWait(t, 2*time.Second, 500*time.Millisecond)
	srv, n := statusSequence(t, 401)
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
	start := time.Now()
	results, err := runProbe(t.Context(), tgt, waitProbe(401), 5*time.Second)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 500*time.Millisecond, "no wait before the first send")
	assert.Equal(t, 401, results[0].Status)
	assert.Equal(t, int32(1), n.Load())
}

// TestWaitForStatusFailsAtItsBound asserts that a status that never comes
// fails the probe at the wait bound, not at the longer step deadline, and
// names the status waited for, the last status, the attempt count and the
// bound; no result is returned for a golden. It fails when the bound is
// removed (the step deadline would end it with a context error instead)
// or when the last answer is returned as the result.
func TestWaitForStatusFailsAtItsBound(t *testing.T) {
	shortWait(t, 300*time.Millisecond, 20*time.Millisecond)
	srv, n := statusSequence(t, 200)
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
	start := time.Now()
	results, err := runProbe(t.Context(), tgt, waitProbe(401), 5*time.Second)
	elapsed := time.Since(start)
	require.ErrorContains(t, err, "observation read: want status 401, still 200 after ")
	require.ErrorContains(t, err, " attempts in 300ms")
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, results)
	assert.GreaterOrEqual(t, elapsed, 300*time.Millisecond)
	assert.Less(t, elapsed, 600*time.Millisecond, "ends at the wait bound, within twice the bound")
	assert.Greater(t, n.Load(), int32(2))
}

type countingTransport struct {
	next http.RoundTripper
	n    atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.next.RoundTrip(r)
}

// TestWaitForStatusStopsOnATransportError asserts that a transport error
// ends the wait at once with that error. It fails when the loop retries a
// request that got no answer.
func TestWaitForStatusStopsOnATransportError(t *testing.T) {
	shortWait(t, 2*time.Second, 10*time.Millisecond)
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // nothing listens on its address any more
	ct := &countingTransport{next: &http.Transport{}}
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}, client: &http.Client{Transport: ct}}
	_, err := runProbe(t.Context(), tgt, waitProbe(401), 5*time.Second)
	require.ErrorIs(t, err, syscall.ECONNREFUSED)
	assert.Equal(t, int32(1), ct.n.Load(), "a GET that got no answer is sent once")
}

// failAfterFirst passes the first request through and fails every later one.
type failAfterFirst struct {
	next http.RoundTripper
	n    atomic.Int32
}

var errLaterSend = errors.New("later send failed")

func (f *failAfterFirst) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.n.Add(1) > 1 {
		return nil, errLaterSend
	}
	return f.next.RoundTrip(r)
}

// TestWaitForStatusStopsOnATransportErrorAfterAnAnswer asserts that a
// transport error on a later send also ends the wait at once. It fails when
// the loop retries transport errors after its first send.
func TestWaitForStatusStopsOnATransportErrorAfterAnAnswer(t *testing.T) {
	shortWait(t, 2*time.Second, 10*time.Millisecond)
	srv, _ := statusSequence(t, 200)
	ft := &failAfterFirst{next: srv.Client().Transport}
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}, client: &http.Client{Transport: ft}}
	_, err := runProbe(t.Context(), tgt, waitProbe(401), 5*time.Second)
	require.ErrorIs(t, err, errLaterSend)
	assert.Equal(t, int32(2), ft.n.Load())
}

// closeTracker wraps every response body and counts the bodies read to the
// end and the bodies closed.
type closeTracker struct {
	next    http.RoundTripper
	sent    atomic.Int32
	drained atomic.Int32
	closed  atomic.Int32
}

type trackedBody struct {
	io.ReadCloser
	drained, closed *atomic.Int32
}

func (b trackedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.drained.Add(1)
	}
	return n, err
}

func (b trackedBody) Close() error {
	b.closed.Add(1)
	return b.ReadCloser.Close()
}

func (c *closeTracker) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	c.sent.Add(1)
	resp.Body = trackedBody{ReadCloser: resp.Body, drained: &c.drained, closed: &c.closed}
	return resp, nil
}

// TestWaitForStatusClosesEveryDiscardedBody asserts that the answers the
// wait discards are read to the end and closed, as is the one it records.
// It fails when the drain or the close of a discarded answer is removed.
func TestWaitForStatusClosesEveryDiscardedBody(t *testing.T) {
	shortWait(t, 2*time.Second, 10*time.Millisecond)
	srv, _ := statusSequence(t, 200, 200, 401)
	ct := &closeTracker{next: srv.Client().Transport}
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}, client: &http.Client{Transport: ct}}
	_, err := runProbe(t.Context(), tgt, waitProbe(401), 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, int32(3), ct.sent.Load())
	assert.Equal(t, int32(3), ct.drained.Load())
	assert.Equal(t, int32(3), ct.closed.Load())
}

// TestWaitForStatusEndsWithTheStepDeadline asserts that a step deadline
// reached between sends ends the wait with that deadline and the status it
// waited for. It fails when the wait ignores the step context.
func TestWaitForStatusEndsWithTheStepDeadline(t *testing.T) {
	shortWait(t, 10*time.Second, 5*time.Second)
	srv, n := statusSequence(t, 200)
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
	start := time.Now()
	_, err := runProbe(t.Context(), tgt, waitProbe(401), 300*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "want status 401, last 200 after 1 attempts")
	assert.Less(t, time.Since(start), 3*time.Second, "the wait ends with the step deadline, not the interval")
	assert.Equal(t, int32(1), n.Load())
}

// requestLog records the method and path of every request a test server gets.
type requestLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *requestLog) add(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, r.Method+" "+r.URL.Path)
}

func (l *requestLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

// withMethod returns the recorded entries that start with method.
func (l *requestLog) withMethod(method string) []string {
	var out []string
	for _, e := range l.all() {
		if strings.HasPrefix(e, method+" ") {
			out = append(out, e)
		}
	}
	return out
}

// callerValueKey is the key of a value a test stores in the caller's context.
type callerValueKey struct{}

// TestRunProbeTeardownRunsAfterTheCallerContextEnds asserts every teardown step
// reaches the server, in order, when the caller's context was cancelled before
// the call, by the last setup step, or by the observation, or had already
// passed its deadline. The returned error carries the caller's context error
// and no teardown error, a teardown step sees a context that has not ended,
// and the values of the caller's context stay visible to it. When the caller's
// context ended before the setup started, the setup and observation requests
// never reach the server.
func TestRunProbeTeardownRunsAfterTheCallerContextEnds(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name            string
		start           func(parent context.Context) (context.Context, context.CancelFunc)
		cancelInSetup   bool
		cancelInObserve bool
		want            error
		endedBeforeRun  bool
	}{
		{
			name: "cancelled before the call",
			start: func(parent context.Context) (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(parent)
				cancel()
				return ctx, cancel
			},
			want:           context.Canceled,
			endedBeforeRun: true,
		},
		{
			name:          "cancelled by the last setup step",
			start:         func(parent context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(parent) },
			cancelInSetup: true,
			want:          context.Canceled,
		},
		{
			name:            "cancelled during the observation",
			start:           func(parent context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(parent) },
			cancelInObserve: true,
			want:            context.Canceled,
		},
		{
			name: "deadline in the past",
			start: func(parent context.Context) (context.Context, context.CancelFunc) {
				return context.WithDeadline(parent, time.Now().Add(-time.Second))
			},
			want:           context.DeadlineExceeded,
			endedBeforeRun: true,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := row.start(t.Context())
			defer cancel()
			ctx = context.WithValue(ctx, callerValueKey{}, "from the caller")

			var log requestLog
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				log.add(r)
				if row.cancelInObserve && r.URL.Path == "/observe" {
					cancel()
					testtransport.HangAndAbort(r)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

			var sawErr error
			var sawValue any
			p := Probe{
				Name: "fam/teardown-context",
				Setup: []Step{{
					Name: "create",
					Do: func(ctx context.Context, tgt Target, _ map[string]string) error {
						if _, err := adminDo(ctx, tgt, http.MethodPost, "/setup", ""); err != nil {
							return err
						}
						if row.cancelInSetup {
							cancel()
						}
						return nil
					},
				}},
				Observations: []Observation{
					{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: "/observe", Identity: IdentityAdmin}},
				},
				Teardown: []Step{
					{Name: "record", Do: func(ctx context.Context, _ Target, _ map[string]string) error {
						sawErr = ctx.Err()
						sawValue = ctx.Value(callerValueKey{})
						return nil
					}},
					deleteFixture("delete-a", "/del/a"),
					deleteFixture("delete-b", "/del/b"),
					deleteFixture("delete-c", "/del/c"),
				},
			}
			_, err := runProbe(ctx, tgt, p, 5*time.Second)
			require.Error(t, err)
			require.ErrorIs(t, err, row.want)
			assert.NotContains(t, err.Error(), ": teardown ", "no teardown step failed")
			assert.Equal(t, []string{"DELETE /del/a", "DELETE /del/b", "DELETE /del/c"}, log.withMethod(http.MethodDelete),
				"every teardown request reached the server, in order")
			require.NoError(t, sawErr, "a teardown step runs on a context that has not ended")
			assert.Equal(t, "from the caller", sawValue, "the caller's context values reach the teardown step")
			if row.endedBeforeRun {
				require.ErrorContains(t, err, "setup create")
				assert.NotContains(t, log.all(), "POST /setup", "the setup step sent nothing on an ended context")
				assert.NotContains(t, log.all(), "GET /observe", "the observation sent nothing on an ended context")
			}
		})
	}
}

// TestRunProbeTeardownStepsAreEachBounded asserts a teardown step that waits
// for its context ends at the per-step time limit with a deadline error, not
// the cancellation of the caller's context, and that the steps after it still
// send their requests.
func TestRunProbeTeardownStepsAreEachBounded(t *testing.T) {
	t.Parallel()
	var log requestLog
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}

	var blockedErr error
	p := Probe{
		Name: "fam/teardown-bound",
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: http.MethodGet, Path: "/observe", Identity: IdentityAdmin}},
		},
		Teardown: []Step{
			{Name: "block", Do: func(ctx context.Context, _ Target, _ map[string]string) error {
				select {
				case <-ctx.Done():
					blockedErr = ctx.Err()
				case <-time.After(2 * time.Second):
					blockedErr = errors.New("step context has no deadline")
				}
				return blockedErr
			}},
			deleteFixture("delete-a", "/del/a"),
			deleteFixture("delete-b", "/del/b"),
		},
	}
	caller, cancel := context.WithCancel(t.Context())
	cancel()

	type outcome struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	go func() {
		start := time.Now()
		_, err := runProbe(caller, tgt, p, 50*time.Millisecond)
		done <- outcome{err, time.Since(start)}
	}()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(3 * time.Second):
		require.FailNow(t, "the probe did not return within the watchdog")
	}
	require.ErrorIs(t, blockedErr, context.DeadlineExceeded, "the step ends at its own time limit")
	require.NotErrorIs(t, blockedErr, context.Canceled)
	assert.Less(t, got.elapsed, time.Second, "each step is bounded by the per-step limit")
	assert.Equal(t, []string{"DELETE /del/a", "DELETE /del/b"}, log.withMethod(http.MethodDelete),
		"the steps after a step that ran out of time still send their requests")
	require.ErrorContains(t, got.err, "teardown block")
	assert.NotContains(t, got.err.Error(), "teardown delete-")
}
