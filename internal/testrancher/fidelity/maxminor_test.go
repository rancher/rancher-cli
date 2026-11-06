package fidelity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testtransport"
)

const headCommit = "0123456789abcdef0123456789abcdef01234567"

// maxMinorRequest is one request the server of a maxMinorTarget received.
type maxMinorRequest struct {
	method string
	path   string
	auth   string
}

// maxMinorTarget is a Target over an httptest server. The server responds to
// /rancherversion with a fixed status and body and to every other path with
// an empty object. The target records every request and every Bearer call.
type maxMinorTarget struct {
	srv        *httptest.Server
	downstream string
	mu         sync.Mutex
	requests   []maxMinorRequest
	bearers    int
}

func newMaxMinorTarget(t *testing.T, status int, body string) *maxMinorTarget {
	t.Helper()
	g := &maxMinorTarget{downstream: "c-maxminor"}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.requests = append(g.requests, maxMinorRequest{r.Method, r.URL.Path, r.Header.Get("Authorization")})
		g.mu.Unlock()
		if r.URL.Path == "/rancherversion" {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// newServerVersionTarget returns a target whose server responds to
// /rancherversion with the given version and a commit.
func newServerVersionTarget(t *testing.T, version string) *maxMinorTarget {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"Version": version, "GitCommit": headCommit})
	require.NoError(t, err)
	return newMaxMinorTarget(t, http.StatusOK, string(raw))
}

func (g *maxMinorTarget) BaseURL() string      { return g.srv.URL }
func (g *maxMinorTarget) Client() *http.Client { return g.srv.Client() }
func (g *maxMinorTarget) Downstream() string   { return g.downstream }
func (g *maxMinorTarget) Bearer(context.Context, Identity) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bearers++
	return "bearer-of-the-max-minor-target", nil
}

func (g *maxMinorTarget) paths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.requests))
	for _, r := range g.requests {
		out = append(out, r.method+" "+r.path)
	}
	return out
}

func (g *maxMinorTarget) versionRequestsHaveNoCredential(t *testing.T) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range g.requests {
		if r.path == "/rancherversion" {
			assert.Empty(t, r.auth, "the version request carries no Authorization header")
		}
	}
	assert.Zero(t, g.bearers, "no bearer is resolved for the version request")
}

// stepRuns counts the setup and teardown steps of a fixture probe that ran.
type stepRuns struct {
	setup, teardown atomic.Int32
}

// maxMinorFixture is a probe with one observation that sends no credential, a
// setup step and a teardown step that count their runs, and the given
// MaxMinor. kind is "oracle" for an OracleOnly probe or "downstream" for a
// Downstream one.
func maxMinorFixture(kind, maxMinor string, runs *stepRuns) Probe {
	return Probe{
		Name:       "maxminor/" + kind,
		OracleOnly: kind == "oracle",
		Downstream: kind == "downstream",
		MaxMinor:   maxMinor,
		Setup: []Step{{Name: "count", Do: func(context.Context, Target, map[string]string) error {
			runs.setup.Add(1)
			return nil
		}}},
		Observations: []Observation{{Name: "get", Request: RequestSpec{Method: "GET", Path: "/v1/things", Identity: IdentityNone}}},
		Teardown: []Step{{Name: "count", Do: func(context.Context, Target, map[string]string) error {
			runs.teardown.Add(1)
			return nil
		}}},
	}
}

// TestMaxMinorOverEveryServerVersion asserts, for an OracleOnly and a
// Downstream fixture, that a probe run against a server of a minor newer than
// its MaxMinor returns an error that wraps ErrNotForMinor and names the
// probe, the MaxMinor and the server version. It asserts that the server
// received only the version request, without a credential, and that no setup
// or teardown step ran. A server of the MaxMinor or an older one runs every
// observation. Minors compare as numbers, major first: 2.9 is older than 2.15
// and 2.15 is older than 2.150.
func TestMaxMinorOverEveryServerVersion(t *testing.T) {
	rows := []struct {
		maxMinor string
		version  string
		skip     bool
	}{
		{"2.15", "v2.9.1", false},
		{"2.15", "v2.14.5", false},
		{"2.15", "v2.15.0", false},
		{"2.15", "v2.15.2", false},
		{"2.15", "v2.15-head", false},
		{"2.15", "v2.15-" + headCommit + "-head", false},
		{"2.15", "v2.16-head", true},
		{"2.15", "v2.16-" + headCommit + "-head", true},
		{"2.15", "v2.16.0-alpha1", true},
		{"2.15", "v2.16.0-rc.1", true},
		{"2.15", "v2.17.1", true},
		{"2.15", "v2.17-" + headCommit + "-head", true},
		{"2.15", "v2.150.0", true},
		{"2.15", "v3.0.0", true},
		{"2.9", "v2.8.5", false},
		{"2.9", "v2.9.1", false},
		{"2.9", "v2.15.0", true},
		{"2.0", "v2.0.3", false},
		{"2.0", "v2.1.0", true},
		{"3.0", "v2.99.0", false},
		{"3.0", "v3.0.1", false},
		{"3.0", "v3.1.0", true},
	}
	for _, kind := range []string{"oracle", "downstream"} {
		for _, row := range rows {
			t.Run(fmt.Sprintf("%s/MaxMinor %s/server %s", kind, row.maxMinor, row.version), func(t *testing.T) {
				tgt := newServerVersionTarget(t, row.version)
				var runs stepRuns
				results, err := RunProbe(t.Context(), tgt, maxMinorFixture(kind, row.maxMinor, &runs))
				tgt.versionRequestsHaveNoCredential(t)
				if row.skip {
					require.ErrorIs(t, err, ErrNotForMinor)
					require.ErrorContains(t, err, "probe maxminor/"+kind+" runs only up to Rancher "+row.maxMinor)
					require.ErrorContains(t, err, "the server is "+row.version)
					assert.Nil(t, results)
					assert.Equal(t, []string{"GET /rancherversion"}, tgt.paths(), "only the version request is sent")
					assert.Zero(t, runs.setup.Load(), "setup ran")
					assert.Zero(t, runs.teardown.Load(), "teardown ran")
					return
				}
				require.NoError(t, err)
				require.Len(t, results, 1)
				assert.Equal(t, []string{"GET /rancherversion", "GET /v1/things"}, tgt.paths())
				assert.Equal(t, int32(1), runs.setup.Load())
				assert.Equal(t, int32(1), runs.teardown.Load())
			})
		}
	}
}

// TestMaxMinorComesBeforeTheDownstreamCheck asserts that a Downstream probe
// whose MaxMinor is older than the server's minor is not checked even when
// the target has no downstream cluster, and that with a server of the
// MaxMinor it still refuses to run without one.
func TestMaxMinorComesBeforeTheDownstreamCheck(t *testing.T) {
	var runs stepRuns
	tgt := newServerVersionTarget(t, "v2.16-head")
	tgt.downstream = ""
	_, err := RunProbe(t.Context(), tgt, maxMinorFixture("downstream", "2.15", &runs))
	require.ErrorIs(t, err, ErrNotForMinor)

	tgt = newServerVersionTarget(t, "v2.15.2")
	tgt.downstream = ""
	_, err = RunProbe(t.Context(), tgt, maxMinorFixture("downstream", "2.15", &runs))
	require.ErrorContains(t, err, "target has no downstream cluster")
	assert.NotErrorIs(t, err, ErrNotForMinor)
}

// TestProbeWithoutMaxMinorSendsNoVersionRequest asserts that a probe with no
// MaxMinor never asks for the server version, whatever the response to that
// request would be.
func TestProbeWithoutMaxMinorSendsNoVersionRequest(t *testing.T) {
	for _, kind := range []string{"oracle", "downstream"} {
		t.Run(kind, func(t *testing.T) {
			tgt := newMaxMinorTarget(t, http.StatusInternalServerError, "no")
			var runs stepRuns
			results, err := RunProbe(t.Context(), tgt, maxMinorFixture(kind, "", &runs))
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, []string{"GET /v1/things"}, tgt.paths())
			assert.Equal(t, int32(1), runs.setup.Load())
		})
	}
}

// TestMaxMinorReadsOnlyTheVersionForEveryProbeThatSetsIt asserts, for every
// probe of All() that sets MaxMinor, that running it against a server of a
// far newer minor that responds only to /rancherversion returns
// ErrNotForMinor and sends only that request. A probe with MaxMinor must be
// OracleOnly or Downstream. When no probe in All() sets MaxMinor, the fixture
// tests above cover this and this test runs nothing.
func TestMaxMinorReadsOnlyTheVersionForEveryProbeThatSetsIt(t *testing.T) {
	withMaxMinor := 0
	for _, p := range All() {
		if p.MaxMinor == "" {
			continue
		}
		withMaxMinor++
		t.Run(p.Name, func(t *testing.T) {
			assert.True(t, p.OracleOnly || p.Downstream, "a probe with MaxMinor must be OracleOnly or Downstream: the facade has no Rancher minor")
			tgt := newServerVersionTarget(t, "v99.0.0")
			_, err := RunProbe(t.Context(), tgt, p)
			require.ErrorIs(t, err, ErrNotForMinor)
			assert.Equal(t, []string{"GET /rancherversion"}, tgt.paths())
			tgt.versionRequestsHaveNoCredential(t)
		})
	}
	t.Logf("%d probes of All() set MaxMinor", withMaxMinor)
}

// TestMaxMinorUnreadableVersionIsAnError asserts that a probe run against a
// server whose version cannot be read or has no minor returns an error other
// than ErrNotForMinor, runs no setup or teardown step and sends only the
// version request.
func TestMaxMinorUnreadableVersionIsAnError(t *testing.T) {
	type versionResponse struct {
		name   string
		status int
		body   string
		want   string
	}
	cases := []versionResponse{
		{"status 500", http.StatusInternalServerError, `{"Version":"v2.15.2","GitCommit":"abc"}`, "answered status 500"},
		{"not json", http.StatusOK, `nope`, "decoding the answer"},
		{"empty version", http.StatusOK, `{"Version":"","GitCommit":"abc"}`, "no Version or no GitCommit"},
		{"no commit", http.StatusOK, `{"Version":"v2.16-head"}`, "no Version or no GitCommit"},
	}
	versions := map[string]string{
		"head":                      "no Rancher minor in the server version",
		"dev":                       "no Rancher minor in the server version",
		"dev-v2.15.0":               "no Rancher minor in the server version",
		"xv2.16.0":                  "no Rancher minor in the server version",
		"2.15.2":                    "no Rancher minor in the server version",
		"v2":                        "no Rancher minor in the server version",
		"v2.x":                      "no Rancher minor in the server version",
		"v2.15x":                    "no Rancher minor in the server version",
		"v02.15.0":                  "no Rancher minor in the server version",
		"v2.015.0":                  "no Rancher minor in the server version",
		"v2.":                       "no Rancher minor in the server version",
		"V2.15.2":                   "no Rancher minor in the server version",
		"v2.99999999999999999999.0": "value out of range",
		"v99999999999999999999.1.0": "value out of range",
	}
	for version, want := range versions {
		raw, err := json.Marshal(map[string]string{"Version": version, "GitCommit": headCommit})
		require.NoError(t, err)
		cases = append(cases, versionResponse{"version " + version, http.StatusOK, string(raw), want})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tgt := newMaxMinorTarget(t, c.status, c.body)
			var runs stepRuns
			_, err := RunProbe(t.Context(), tgt, maxMinorFixture("oracle", "2.15", &runs))
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrNotForMinor)
			require.ErrorContains(t, err, "maxminor/oracle")
			require.ErrorContains(t, err, c.want)
			assert.Equal(t, []string{"GET /rancherversion"}, tgt.paths())
			assert.Zero(t, runs.setup.Load())
			assert.Zero(t, runs.teardown.Load())
		})
	}

	t.Run("unreachable server", func(t *testing.T) {
		tgt := &fakeTarget{srv: closedServer(t)}
		var runs stepRuns
		_, err := RunProbe(t.Context(), tgt, maxMinorFixture("oracle", "2.15", &runs))
		require.ErrorContains(t, err, "reading the server version")
		require.NotErrorIs(t, err, ErrNotForMinor)
		assert.Zero(t, runs.setup.Load())
	})
}

// TestMaxMinorVersionRequestHasAStepTimeout asserts that the version request
// ends at the step timeout, not at the end of the caller's context: against
// a server that never responds, the probe returns an error that is not
// ErrNotForMinor soon after the step timeout, and runs no setup or teardown
// step.
func TestMaxMinorVersionRequestHasAStepTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		testtransport.HangAndAbort(r)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var runs stepRuns
	start := time.Now()
	_, err := runProbe(ctx, &fakeTarget{srv: srv}, maxMinorFixture("oracle", "2.15", &runs), 50*time.Millisecond)
	elapsed := time.Since(start)
	require.ErrorContains(t, err, "reading the server version")
	require.NotErrorIs(t, err, ErrNotForMinor)
	assert.Less(t, elapsed, 5*time.Second, "the request ends at the step timeout")
	assert.Zero(t, runs.setup.Load())
	assert.Zero(t, runs.teardown.Load())
}

// TestMaxMinorThatIsNotMajorMinorIsAnErrorBeforeAnyRequest asserts that
// checkMaxMinor returns an error, and sends no request, for a MaxMinor that
// Validate would refuse, instead of panicking on a nil match.
func TestMaxMinorThatIsNotMajorMinorIsAnErrorBeforeAnyRequest(t *testing.T) {
	for _, maxMinor := range []string{"2", "v2.15", "2.015", "2.x"} {
		tgt := newServerVersionTarget(t, "v2.15.2")
		var runs stepRuns
		err := checkMaxMinor(t.Context(), tgt, maxMinorFixture("oracle", maxMinor, &runs), time.Second)
		require.ErrorContains(t, err, "not in the form major.minor", maxMinor)
		assert.Empty(t, tgt.paths())
	}
}

// skipRecorderTB is a testing.TB that records Skipf, Fatalf and Errorf
// instead of skipping or failing the test. Skipf and Fatalf stop the calling
// goroutine, as testing.TB does; Errorf does not.
type skipRecorderTB struct {
	testing.TB
	skipped string
	failed  string
}

func (r *skipRecorderTB) Skipf(format string, args ...any) {
	r.skipped = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (r *skipRecorderTB) Fatalf(format string, args ...any) {
	r.failed = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (r *skipRecorderTB) Errorf(format string, args ...any) {
	r.failed = fmt.Sprintf(format, args...)
}

// runOrSkip calls RunProbeOrSkip with a skipRecorderTB in its own goroutine,
// as a test function runs, and returns what it returned and what the
// skipRecorderTB recorded.
func runOrSkip(t *testing.T, tgt Target, p Probe, drift *DriftCollector, suite string) (results []ObservationResult, rec *skipRecorderTB, err error) {
	t.Helper()
	rec = &skipRecorderTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		results, err = RunProbeOrSkip(t.Context(), rec, tgt, p, RunOptions{}, drift, suite)
	}()
	<-done
	return results, rec, err
}

// TestRunProbeOrSkip asserts that RunProbeOrSkip, against a server of a minor
// newer than the probe's MaxMinor, records the probe with the drift collector
// under the suite, with the probe's name, its Downstream flag and the reason,
// and skips the test with a message that starts "not checked:", with no
// result and no failure. With a nil collector it skips without recording.
// Against a server of the MaxMinor it runs the probe and neither records nor
// skips. Any other error is returned to the caller and neither records nor
// skips.
func TestRunProbeOrSkip(t *testing.T) {
	const reason = "runs only up to Rancher 2.15; the server is v2.16-head"

	t.Run("newer server, Downstream probe", func(t *testing.T) {
		tgt := newServerVersionTarget(t, "v2.16-head")
		var runs stepRuns
		drift := &DriftCollector{}
		results, rec, err := runOrSkip(t, tgt, maxMinorFixture("downstream", "2.15", &runs), drift, SuiteDownstream)
		require.NoError(t, err, "the test stops at the skip: RunProbeOrSkip returns no error and no results")
		assert.Nil(t, results)
		assert.Equal(t, "not checked: probe maxminor/downstream "+reason, rec.skipped)
		assert.Empty(t, rec.failed)
		assert.Equal(t, []ProbeRun{{
			Suite: SuiteDownstream, Probe: "maxminor/downstream", Downstream: true, NotCheckedReason: reason,
		}}, drift.probes)
		assert.Equal(t, []string{"GET /rancherversion"}, tgt.paths())
	})

	t.Run("newer server, OracleOnly probe", func(t *testing.T) {
		var runs stepRuns
		drift := &DriftCollector{}
		_, rec, _ := runOrSkip(t, newServerVersionTarget(t, "v2.16-head"), maxMinorFixture("oracle", "2.15", &runs), drift, SuiteOracle)
		assert.True(t, strings.HasPrefix(rec.skipped, "not checked: "))
		assert.Equal(t, []ProbeRun{{Suite: SuiteOracle, Probe: "maxminor/oracle", NotCheckedReason: reason}}, drift.probes)
	})

	t.Run("newer server with no collector", func(t *testing.T) {
		var runs stepRuns
		_, rec, _ := runOrSkip(t, newServerVersionTarget(t, "v2.17.1"), maxMinorFixture("downstream", "2.15", &runs), nil, SuiteDownstream)
		assert.True(t, strings.HasPrefix(rec.skipped, "not checked: "))
		assert.Empty(t, rec.failed)
	})

	t.Run("server of the MaxMinor", func(t *testing.T) {
		var runs stepRuns
		drift := &DriftCollector{}
		results, rec, err := runOrSkip(t, newServerVersionTarget(t, "v2.15.2"), maxMinorFixture("oracle", "2.15", &runs), drift, SuiteOracle)
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Empty(t, rec.skipped)
		assert.Empty(t, rec.failed)
		assert.Empty(t, drift.probes)
	})

	t.Run("another error is returned", func(t *testing.T) {
		var runs stepRuns
		drift := &DriftCollector{}
		_, rec, err := runOrSkip(t, newMaxMinorTarget(t, http.StatusInternalServerError, "no"), maxMinorFixture("oracle", "2.15", &runs), drift, SuiteOracle)
		require.ErrorContains(t, err, "answered status 500")
		assert.Empty(t, rec.skipped)
		assert.Empty(t, rec.failed)
		assert.Empty(t, drift.probes)
	})

	t.Run("a probe with no MaxMinor that fails returns its error", func(t *testing.T) {
		tgt := newMaxMinorTarget(t, http.StatusOK, "")
		p := maxMinorFixture("oracle", "", new(stepRuns))
		p.Setup = []Step{{Name: "fail", Do: func(context.Context, Target, map[string]string) error { return fmt.Errorf("boom") }}}
		drift := &DriftCollector{}
		_, rec, err := runOrSkip(t, tgt, p, drift, SuiteOracle)
		require.ErrorContains(t, err, "boom")
		assert.Empty(t, rec.skipped)
		assert.Empty(t, drift.probes)
	})
}
