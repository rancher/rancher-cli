package fidelity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeTarget struct {
	srv     *httptest.Server
	bearers map[Identity]string
}

func (f *fakeTarget) BaseURL() string      { return f.srv.URL }
func (f *fakeTarget) Client() *http.Client { return f.srv.Client() }
func (f *fakeTarget) Bearer(_ context.Context, id Identity) (string, error) {
	return f.bearers[id], nil
}

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
	results, err := RunProbe(context.Background(), tgt, p)
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
				Name:     "mint",
				Request:  RequestSpec{Method: "POST", Path: "/tokens", Body: `{"spec":{}}`, Identity: IdentityAdmin},
				Captures: map[string]Capture{"tokname": {Path: "status.bearerToken", ExtBearerName: true}},
			},
			{Name: "get", Request: RequestSpec{Method: "GET", Path: "/tokens/{tokname}", Identity: IdentityAdmin}},
		},
	}
	results, err := RunProbe(context.Background(), tgt, p)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "/tokens/fac-token-7", gotPath,
		"the named request must reach the minted record via the bearer-derived name")
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
	results, err := RunProbe(context.Background(), tgt, p)
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
	_, err := RunProbe(context.Background(), tgt, p)
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
	_, err := runProbe(context.Background(), tgt, p, 50*time.Millisecond)
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
	_, err := runProbe(context.Background(), tgt, p, 50*time.Millisecond)
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
	_, err := RunProbe(context.Background(), tgt, p)
	require.Error(t, err)
	assert.ErrorContains(t, err, "setup boom", "the primary failure survives")
	assert.ErrorContains(t, err, "clean-a failed", "every teardown failure is reported")
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
		assert.ErrorIs(t, err, syscall.ECONNRESET)
		assert.Equal(t, 1, rt.calls,
			"a POST reset surfaces after exactly one attempt so a committed mutation is never double-fired")
	})

	t.Run("second reset on GET surfaces", func(t *testing.T) {
		t.Parallel()
		rt := &resetRoundTripper{failFirst: 2}
		_, err := doResilient(&http.Client{Transport: rt}, build(http.MethodGet))
		require.Error(t, err)
		assert.ErrorIs(t, err, syscall.ECONNRESET)
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
	_, err := RunProbe(context.Background(), tgt, p)
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
	results, err := RunProbe(context.Background(), tgt, p)
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
				Name:     "mint",
				Request:  RequestSpec{Method: "POST", Path: "/t", Body: `{}`, Identity: IdentityAdmin},
				Captures: map[string]Capture{"tokname": {Path: "status.bearerToken", ExtBearerName: true}},
			},
			{Name: "get", Request: RequestSpec{Method: "GET", Path: "/t/{tokname}", Identity: IdentityAdmin}},
		},
	}
	_, err := RunProbe(context.Background(), tgt, p)
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
	_, err := RunProbe(context.Background(), tgt, p)
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
