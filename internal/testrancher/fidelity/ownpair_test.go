package fidelity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pairUUID   = "11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	serverUUID = "22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	ownPairReq = "GET " + installUUIDPath
)

// ownPairServer is a server that answers /rancherversion with a version, the
// install-uuid setting with a status and the two fields the checks read, and
// every other path with an empty object. It records every request.
type ownPairServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	auths    []string
}

// newOwnPairServer returns a server whose install-uuid setting answers with
// status, value and defaultValue, and whose version is version.
func newOwnPairServer(t *testing.T, version string, status int, value, defaultValue string) *ownPairServer {
	t.Helper()
	s := &ownPairServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/rancherversion":
			_, _ = fmt.Fprintf(w, `{"Version":%q,"GitCommit":%q}`, version, headCommit)
		case installUUIDPath:
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, `{"id":"install-uuid","value":%q,"default":%q}`, value, defaultValue)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *ownPairServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *ownPairServer) authOf(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auths[i]
}

// target returns an OracleTarget over the server that names uuid as the
// install id of its pair.
func (s *ownPairServer) target(uuid string) *OracleTarget {
	return &OracleTarget{baseURL: s.URL, client: s.Client(), admin: "admin-tok", pairInstallUUID: uuid}
}

// ownPairProbe is an OracleOnly probe with OwnPairOnly, one observation that
// sends no credential, and a setup and a teardown step that count their runs.
func ownPairProbe(runs *stepRuns) Probe {
	p := maxMinorFixture("oracle", "", runs)
	p.Name = "ownpair/oracle"
	p.OwnPairOnly = true
	return p
}

// TestOwnPairWithoutAnInstallIDIsNotChecked asserts that an OwnPairOnly probe
// run against an OracleTarget that names no install id, and against a target
// with no PairInstallUUID method, returns an error that is notChecked, sends
// no request at all, runs no setup or teardown step and returns no result.
func TestOwnPairWithoutAnInstallIDIsNotChecked(t *testing.T) {
	t.Parallel()
	for name, mk := range map[string]func(srv *ownPairServer) Target{
		"oracle target with an empty install id": func(srv *ownPairServer) Target { return srv.target("") },
		"target with no install id method":       func(srv *ownPairServer) Target { return &fakeTarget{srv: srv.Server} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newOwnPairServer(t, "v2.15.2", http.StatusOK, serverUUID, "")
			var runs stepRuns
			results, err := RunProbe(t.Context(), mk(srv), ownPairProbe(&runs))
			require.Error(t, err)
			_, ok := errors.AsType[notChecked](err)
			assert.True(t, ok, "the error is notChecked: %v", err)
			_, ok = errors.AsType[*notOwnPair](err)
			assert.True(t, ok)
			require.ErrorContains(t, err, "probe ownpair/oracle runs only on the Rancher scripts/e2e-fidelity booted or kept")
			assert.Nil(t, results)
			assert.Empty(t, srv.seen(), "no request is sent")
			assert.Zero(t, runs.setup.Load())
			assert.Zero(t, runs.teardown.Load())
		})
	}
}

// TestOwnPairWithAnotherInstallIDIsNotChecked asserts that a server whose
// install-uuid setting differs from the target's install id gets exactly one
// request, a GET of the setting with the admin bearer, that the probe is then
// notChecked and that no setup or teardown step ran. A non-empty value wins
// over the default: a default that matches does not save a different value.
func TestOwnPairWithAnotherInstallIDIsNotChecked(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]struct{ value, defaultValue string }{
		"value differs":                    {serverUUID, ""},
		"neither field is set":             {"", ""},
		"value differs and default equals": {serverUUID, pairUUID},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newOwnPairServer(t, "v2.15.2", http.StatusOK, answer.value, answer.defaultValue)
			var runs stepRuns
			results, err := RunProbe(t.Context(), srv.target(pairUUID), ownPairProbe(&runs))
			_, ok := errors.AsType[notChecked](err)
			require.True(t, ok, "the error is notChecked: %v", err)
			assert.Nil(t, results)
			assert.Equal(t, []string{ownPairReq}, srv.seen())
			assert.Equal(t, "Bearer admin-tok", srv.authOf(0))
			assert.Zero(t, runs.setup.Load())
			assert.Zero(t, runs.teardown.Load())
		})
	}
}

// TestOwnPairWithTheSameInstallIDRuns asserts that a server whose
// install-uuid setting equals the target's install id, in value or, when
// value is empty, in default, runs the probe after the one read of the
// setting: the setup ran, the observation was sent and the teardown ran.
func TestOwnPairWithTheSameInstallIDRuns(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]struct{ value, defaultValue string }{
		"in value":                  {pairUUID, ""},
		"in value, default differs": {pairUUID, serverUUID},
		"in default, value empty":   {"", pairUUID},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newOwnPairServer(t, "v2.15.2", http.StatusOK, answer.value, answer.defaultValue)
			var runs stepRuns
			results, err := RunProbe(t.Context(), srv.target(pairUUID), ownPairProbe(&runs))
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, []string{ownPairReq, "GET /v1/things"}, srv.seen())
			assert.Equal(t, int32(1), runs.setup.Load())
			assert.Equal(t, int32(1), runs.teardown.Load())
		})
	}
}

// TestOwnPairReadFailureIsAnError asserts that a read of the install-uuid
// setting that answers 500 or cannot reach the server returns an error that
// is not notChecked, names the probe and the read, sends nothing else and
// runs no step.
func TestOwnPairReadFailureIsAnError(t *testing.T) {
	t.Parallel()
	t.Run("status 500", func(t *testing.T) {
		t.Parallel()
		srv := newOwnPairServer(t, "v2.15.2", http.StatusInternalServerError, pairUUID, "")
		var runs stepRuns
		results, err := RunProbe(t.Context(), srv.target(pairUUID), ownPairProbe(&runs))
		require.ErrorContains(t, err, "probe ownpair/oracle: reading the install-uuid setting: status 500")
		_, ok := errors.AsType[notChecked](err)
		assert.False(t, ok, "a failed read is not a skip")
		assert.Nil(t, results)
		assert.Equal(t, []string{ownPairReq}, srv.seen())
		assert.Zero(t, runs.setup.Load())
		assert.Zero(t, runs.teardown.Load())
	})

	t.Run("server unreachable", func(t *testing.T) {
		t.Parallel()
		srv := closedServer(t)
		tgt := &OracleTarget{baseURL: srv.URL, client: srv.Client(), admin: "admin-tok", pairInstallUUID: pairUUID}
		var runs stepRuns
		_, err := RunProbe(t.Context(), tgt, ownPairProbe(&runs))
		require.ErrorContains(t, err, "probe ownpair/oracle: reading the install-uuid setting")
		_, ok := errors.AsType[notChecked](err)
		assert.False(t, ok)
		assert.Zero(t, runs.setup.Load())
	})
}

// TestOwnPairSkipTextHoldsNoInstallID asserts that neither the target's
// install id nor the server's appears in the error or the reason of a probe
// that is not checked, for a mismatch and for a target with no install id.
func TestOwnPairSkipTextHoldsNoInstallID(t *testing.T) {
	t.Parallel()
	for name, uuid := range map[string]string{"another install id": pairUUID, "no install id": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newOwnPairServer(t, "v2.15.2", http.StatusOK, serverUUID, serverUUID)
			var runs stepRuns
			_, err := RunProbe(t.Context(), srv.target(uuid), ownPairProbe(&runs))
			nc, ok := errors.AsType[notChecked](err)
			require.True(t, ok, "the error is notChecked: %v", err)
			for _, text := range []string{err.Error(), nc.reason()} {
				assert.NotContains(t, text, pairUUID)
				assert.NotContains(t, text, serverUUID)
			}
		})
	}
}

// TestValidateOwnPairOnly asserts that OwnPairOnly is accepted on an
// OracleOnly probe and on a Downstream probe, and refused on a probe that is
// neither.
func TestValidateOwnPairOnly(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*Probe){
		"oracle only": func(p *Probe) { p.OracleOnly = true },
		"downstream":  func(p *Probe) { p.Downstream = true },
	} {
		p := validProbe()
		mutate(&p)
		p.OwnPairOnly = true
		require.NoError(t, p.Validate(), name)
	}

	p := validProbe()
	p.OwnPairOnly = true
	require.ErrorContains(t, p.Validate(), "OwnPairOnly can be set only on an OracleOnly or a Downstream probe")

	p.FacadeDivergence = "reason"
	require.ErrorContains(t, p.Validate(), "OwnPairOnly can be set only on an OracleOnly or a Downstream probe")
}

// TestOwnPairCheckComesAfterMaxMinor asserts that a probe with a MaxMinor
// older than the server's minor and OwnPairOnly is skipped for its MaxMinor
// after the one request for the version, and the install-uuid setting is
// never read; with a server of the MaxMinor the version is read first, then
// the setting, then the probe runs.
func TestOwnPairCheckComesAfterMaxMinor(t *testing.T) {
	t.Parallel()
	t.Run("newer server", func(t *testing.T) {
		t.Parallel()
		srv := newOwnPairServer(t, "v99.0.0", http.StatusOK, pairUUID, "")
		var runs stepRuns
		p := ownPairProbe(&runs)
		p.MaxMinor = "2.15"
		results, err := RunProbe(t.Context(), srv.target(pairUUID), p)
		require.ErrorIs(t, err, ErrNotForMinor)
		assert.Nil(t, results)
		assert.Equal(t, []string{"GET /rancherversion"}, srv.seen(), "the install-uuid setting is never read")
		assert.Zero(t, runs.setup.Load())
	})

	t.Run("server of the MaxMinor", func(t *testing.T) {
		t.Parallel()
		srv := newOwnPairServer(t, "v2.15.2", http.StatusOK, pairUUID, "")
		var runs stepRuns
		p := ownPairProbe(&runs)
		p.MaxMinor = "2.15"
		results, err := RunProbe(t.Context(), srv.target(pairUUID), p)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, []string{"GET /rancherversion", ownPairReq, "GET /v1/things"}, srv.seen())
	})
}

// TestRunProbeOrSkipOwnPair asserts that RunProbeOrSkip records an OwnPairOnly
// probe that is not checked with the drift collector, under the suite, with
// the probe's name, its Downstream flag and the reason, and skips the test
// with a message that starts "not checked:", for a server with another
// install id and for a target with none; with a nil collector it skips
// without recording; a matching install id runs the probe and neither records
// nor skips; a failed read is returned and neither records nor skips.
func TestRunProbeOrSkipOwnPair(t *testing.T) {
	t.Parallel()
	const mismatch = "runs only on the Rancher scripts/e2e-fidelity booted or kept; the server's install-uuid setting differs from that pair's"
	const noID = "runs only on the Rancher scripts/e2e-fidelity booted or kept; the target names no install id of that pair"

	t.Run("another install id, OracleOnly probe", func(t *testing.T) {
		t.Parallel()
		srv := newOwnPairServer(t, "v2.15.2", http.StatusOK, serverUUID, "")
		var runs stepRuns
		drift := &DriftCollector{}
		results, rec, err := runOrSkip(t, srv.target(pairUUID), ownPairProbe(&runs), drift, SuiteOracle)
		require.NoError(t, err)
		assert.Nil(t, results)
		assert.Equal(t, "not checked: probe ownpair/oracle "+mismatch, rec.skipped)
		assert.Empty(t, rec.failed)
		assert.Equal(t, []ProbeRun{{Suite: SuiteOracle, Probe: "ownpair/oracle", NotCheckedReason: mismatch}}, drift.probes)
		assert.Equal(t, []string{ownPairReq}, srv.seen())
		assert.Zero(t, runs.setup.Load())
	})

	t.Run("no install id, Downstream probe", func(t *testing.T) {
		t.Parallel()
		srv := newOwnPairServer(t, "v2.15.2", http.StatusOK, serverUUID, "")
		var runs stepRuns
		p := ownPairProbe(&runs)
		p.Name = "ownpair/downstream"
		p.OracleOnly, p.Downstream = false, true
		tgt := srv.target("")
		tgt.downstream = "c-ownpair"
		drift := &DriftCollector{}
		_, rec, _ := runOrSkip(t, tgt, p, drift, SuiteDownstream)
		assert.Equal(t, "not checked: probe ownpair/downstream "+noID, rec.skipped)
		assert.Equal(t, []ProbeRun{{Suite: SuiteDownstream, Probe: "ownpair/downstream", Downstream: true, NotCheckedReason: noID}}, drift.probes)
		assert.Empty(t, srv.seen())
	})

	t.Run("another install id with no collector", func(t *testing.T) {
		t.Parallel()
		srv := newOwnPairServer(t, "v2.15.2", http.StatusOK, serverUUID, "")
		var runs stepRuns
		_, rec, _ := runOrSkip(t, srv.target(pairUUID), ownPairProbe(&runs), nil, SuiteOracle)
		assert.True(t, strings.HasPrefix(rec.skipped, "not checked: "))
		assert.Empty(t, rec.failed)
	})

	t.Run("the same install id", func(t *testing.T) {
		t.Parallel()
		srv := newOwnPairServer(t, "v2.15.2", http.StatusOK, pairUUID, "")
		var runs stepRuns
		drift := &DriftCollector{}
		results, rec, err := runOrSkip(t, srv.target(pairUUID), ownPairProbe(&runs), drift, SuiteOracle)
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Empty(t, rec.skipped)
		assert.Empty(t, rec.failed)
		assert.Empty(t, drift.probes)
	})

	t.Run("a failed read is returned", func(t *testing.T) {
		t.Parallel()
		srv := newOwnPairServer(t, "v2.15.2", http.StatusInternalServerError, "", "")
		var runs stepRuns
		drift := &DriftCollector{}
		_, rec, err := runOrSkip(t, srv.target(pairUUID), ownPairProbe(&runs), drift, SuiteOracle)
		require.ErrorContains(t, err, "reading the install-uuid setting: status 500")
		assert.Empty(t, rec.skipped)
		assert.Empty(t, drift.probes)
	})
}

// logRecorderTB is a testing.TB that records Log lines instead of logging
// them.
type logRecorderTB struct {
	testing.TB
	mu    sync.Mutex
	lines []string
}

func (r *logRecorderTB) Log(args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprint(args...))
}

func (r *logRecorderTB) logged() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// TestStepLogReachesTheTestLog asserts that a setup step and a teardown step
// that call stepLog, run through RunProbeOrSkip, reach tb.Log with the line,
// in the order the steps ran, also when an observation or a setup step later
// fails.
func TestStepLogReachesTheTestLog(t *testing.T) {
	t.Parallel()
	logStep := func(name, format string, args ...any) Step {
		return Step{Name: name, Do: func(ctx context.Context, _ Target, _ map[string]string) error {
			stepLog(ctx, fmt.Sprintf(format, args...))
			return nil
		}}
	}
	t.Run("setup and teardown", func(t *testing.T) {
		t.Parallel()
		srv := newOwnPairServer(t, "v2.15.2", http.StatusOK, pairUUID, "")
		p := maxMinorFixture("oracle", "", new(stepRuns))
		p.Setup = []Step{logStep("log", "created %s in %d tries", "fixture-a", 2)}
		p.Teardown = []Step{logStep("log", "removed %s", "fixture-a")}
		rec := &logRecorderTB{TB: t}
		_, err := RunProbeOrSkip(t.Context(), rec, srv.target(""), p, RunOptions{}, nil, SuiteOracle)
		require.NoError(t, err)
		assert.Equal(t, []string{"created fixture-a in 2 tries", "removed fixture-a"}, rec.logged())
	})

	t.Run("teardown after a failed setup step", func(t *testing.T) {
		t.Parallel()
		srv := newOwnPairServer(t, "v2.15.2", http.StatusOK, pairUUID, "")
		p := maxMinorFixture("oracle", "", new(stepRuns))
		p.Setup = []Step{
			logStep("log", "created %s", "fixture-b"),
			{Name: "fail", Do: func(context.Context, Target, map[string]string) error { return errors.New("boom") }},
		}
		p.Teardown = []Step{logStep("log", "removed %s", "fixture-b")}
		rec := &logRecorderTB{TB: t}
		_, err := RunProbeOrSkip(t.Context(), rec, srv.target(""), p, RunOptions{}, nil, SuiteOracle)
		require.ErrorContains(t, err, "boom")
		assert.Equal(t, []string{"created fixture-b", "removed fixture-b"}, rec.logged())
	})
}

// TestStepLogWithoutALogFunctionLogsNothing asserts that RunProbe sets no
// log function on the context its steps get, and that stepLog, with no
// function on the context or a nil one, does nothing and does not panic.
func TestStepLogWithoutALogFunctionLogsNothing(t *testing.T) {
	t.Parallel()
	srv := newOwnPairServer(t, "v2.15.2", http.StatusOK, pairUUID, "")
	var withoutFunc, tornDown bool
	p := maxMinorFixture("oracle", "", new(stepRuns))
	p.Setup = []Step{{Name: "log", Do: func(ctx context.Context, _ Target, _ map[string]string) error {
		withoutFunc = ctx.Value(logKey{}) == nil
		stepLog(ctx, "created fixture-c")
		return nil
	}}}
	p.Teardown = []Step{{Name: "log", Do: func(ctx context.Context, _ Target, _ map[string]string) error {
		tornDown = ctx.Value(logKey{}) == nil
		stepLog(ctx, "removed fixture-c")
		return nil
	}}}
	_, err := RunProbe(t.Context(), srv.target(""), p)
	require.NoError(t, err)
	assert.True(t, withoutFunc, "setup gets no log function")
	assert.True(t, tornDown, "teardown gets no log function")

	assert.NotPanics(t, func() { stepLog(t.Context(), "nothing 1") })
	assert.NotPanics(t, func() { stepLog(withLog(t.Context(), nil), "nothing 2") })
}
