package fidelity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/testtransport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortUntil shortens the Until wait bound, its interval and its read limit
// for one test. Tests that call it set package state and must not run in
// parallel; their subtests may.
func shortUntil(t *testing.T, timeout, interval, readTimeout time.Duration) {
	t.Helper()
	oldTimeout, oldInterval, oldRead := untilTimeout, untilInterval, untilReadTimeout
	untilTimeout, untilInterval, untilReadTimeout = timeout, interval, readTimeout
	t.Cleanup(func() { untilTimeout, untilInterval, untilReadTimeout = oldTimeout, oldInterval, oldRead })
}

// untilUnderTest is the Until function type of Observation.
type untilUnderTest = func(decoded map[string]any, status int, captured map[string]string) (bool, string, error)

// untilProbe is a probe with one observation that reads /x with Until.
func untilProbe(until untilUnderTest) Probe {
	return Probe{Name: "fam/until", Observations: []Observation{
		{Name: "read", Request: RequestSpec{Method: http.MethodGet, Path: "/x", Identity: IdentityAdmin}, Until: until},
	}}
}

// neverDone is an Until that reports "pending" for every answer.
func neverDone(map[string]any, int, map[string]string) (bool, string, error) {
	return false, "pending", nil
}

// storeDeadline returns a setup step that stores the time d from now under
// waitDeadlineCapture, in RFC 3339 form with nanoseconds.
func storeDeadline(d time.Duration) Step {
	return Step{Name: "store the wait deadline", Do: func(_ context.Context, _ Target, captured map[string]string) error {
		captured[waitDeadlineCapture] = time.Now().Add(d).UTC().Format(time.RFC3339Nano)
		return nil
	}}
}

// countingServer answers every request with status 202 and a JSON body, and
// counts the requests it served.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"phase":"working"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// TestUntilReadsUntilDone asserts that an observation with Until is read
// again every interval until Until reports done, that the third answer is the
// one recorded with its own status, and that Until receives the decoded body,
// the status and the probe's captures on every read.
func TestUntilReadsUntilDone(t *testing.T) {
	shortUntil(t, 5*time.Second, 10*time.Millisecond, time.Second)
	srv, n := statusSequence(t, 202, 202, 200)
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
	var seen []int
	p := untilProbe(func(decoded map[string]any, status int, captured map[string]string) (bool, string, error) {
		seen = append(seen, int(decoded["attempt"].(float64)))
		return strconv.Itoa(status) == captured["unrecorded.want"], "status " + strconv.Itoa(status), nil
	})
	p.Setup = []Step{{Name: "want", Do: func(_ context.Context, _ Target, captured map[string]string) error {
		captured["unrecorded.want"] = "200"
		return nil
	}}}
	results, err := runProbe(t.Context(), tgt, p, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, 200, results[0].Status)
	assert.InDelta(t, 3, results[0].Body["attempt"], 0, "the third answer is the one recorded")
	assert.Equal(t, int32(3), n.Load())
	assert.Equal(t, []int{1, 2, 3}, seen)
}

// TestUntilErrorEndsTheWaitAtOnce asserts that an error from Until fails the
// observation after one read, with the error in the message, and returns no
// result.
func TestUntilErrorEndsTheWaitAtOnce(t *testing.T) {
	shortUntil(t, 5*time.Second, 10*time.Millisecond, time.Second)
	srv, n := countingServer(t)
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
	p := untilProbe(func(map[string]any, int, map[string]string) (bool, string, error) {
		return false, "failed", errors.New("condition Failed is True")
	})
	results, err := runProbe(t.Context(), tgt, p, 5*time.Second)
	require.ErrorContains(t, err, "probe fam/until: observation read: condition Failed is True")
	assert.Empty(t, results)
	assert.Equal(t, int32(1), n.Load())
}

// TestUntilTimeoutEndsAWaitThatRunsPastTheStepTimeout asserts that the wait
// runs on a context of its own: with a step timeout shorter than untilTimeout
// the observation is read again after the step timeout has passed, ends at
// untilTimeout, and fails naming the last state, the read count, the elapsed
// time and the last status.
func TestUntilTimeoutEndsAWaitThatRunsPastTheStepTimeout(t *testing.T) {
	shortUntil(t, 400*time.Millisecond, 20*time.Millisecond, time.Second)
	srv, n := countingServer(t)
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
	start := time.Now()
	results, err := runProbe(t.Context(), tgt, untilProbe(neverDone), 50*time.Millisecond)
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.Empty(t, results)
	assert.GreaterOrEqual(t, elapsed, 350*time.Millisecond, "the wait outlasts the step timeout")
	match := regexp.MustCompile(`still pending after (\d+) reads in \S+ \(last status 202\)`).FindStringSubmatch(err.Error())
	require.NotNil(t, match, "the error names the state, the reads, the elapsed time and the status: %v", err)
	reads, convErr := strconv.Atoi(match[1])
	require.NoError(t, convErr)
	assert.Greater(t, reads, 2, "the observation is read again after the step timeout")
	assert.GreaterOrEqual(t, int(n.Load()), reads-1)
}

// TestUntilWaitDeadlineEndsTheWaitFirst asserts that a time a setup step
// stores under waitDeadlineCapture ends the wait earlier than untilTimeout.
func TestUntilWaitDeadlineEndsTheWaitFirst(t *testing.T) {
	shortUntil(t, time.Minute, 10*time.Millisecond, time.Second)
	srv, _ := countingServer(t)
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
	p := untilProbe(neverDone)
	p.Setup = []Step{storeDeadline(250 * time.Millisecond)}
	start := time.Now()
	_, err := runProbe(t.Context(), tgt, p, 5*time.Second)
	elapsed := time.Since(start)
	require.ErrorContains(t, err, "still pending after")
	require.ErrorContains(t, err, "(last status 202)")
	assert.GreaterOrEqual(t, elapsed, 200*time.Millisecond)
	assert.Less(t, elapsed, 10*time.Second, "the stored deadline, not untilTimeout, ended the wait")
}

// TestUntilWaitDeadlineAlreadyPassedSendsNothing asserts that a stored
// deadline that is not after the start of the wait fails the observation
// before any request, and that a stored value that is not an RFC 3339 time
// fails it the same way.
func TestUntilWaitDeadlineAlreadyPassedSendsNothing(t *testing.T) {
	shortUntil(t, time.Minute, 10*time.Millisecond, time.Second)
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{"passed, with nanoseconds", time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), "the wait deadline in unrecorded.waitdeadline passed before the first read"},
		{"passed, whole seconds", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "the wait deadline in unrecorded.waitdeadline passed before the first read"},
		{"not a time", "tomorrow", "the wait deadline in unrecorded.waitdeadline is not an RFC 3339 time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, n := countingServer(t)
			tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
			p := untilProbe(neverDone)
			p.Setup = []Step{{Name: "store", Do: func(_ context.Context, _ Target, captured map[string]string) error {
				captured[waitDeadlineCapture] = tc.value
				return nil
			}}}
			results, err := runProbe(t.Context(), tgt, p, 5*time.Second)
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, results)
			assert.Zero(t, n.Load(), "no request is sent")
		})
	}
}

// TestUntilWaitThatRanOutIsNeverRecorded asserts that, under
// RunOptions{KeepLastAnswer: true}, a wait that ends at untilTimeout or at the
// stored deadline still fails the probe: RunProbeWith returns an error and
// the result of the observation that ran out, while the result of an earlier
// observation is kept.
func TestUntilWaitThatRanOutIsNeverRecorded(t *testing.T) {
	shortUntil(t, 200*time.Millisecond, 10*time.Millisecond, time.Second)
	for _, tc := range []struct {
		name  string
		setup []Step
	}{
		{"untilTimeout", nil},
		{"stored deadline", []Step{storeDeadline(100 * time.Millisecond)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := countingServer(t)
			tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
			p := Probe{
				Name:  "fam/until-keep",
				Setup: tc.setup,
				Observations: []Observation{
					{Name: "first", Request: RequestSpec{Method: http.MethodGet, Path: "/y", Identity: IdentityAdmin}},
					{Name: "wait", Request: RequestSpec{Method: http.MethodGet, Path: "/x", Identity: IdentityAdmin}, Until: neverDone},
				},
			}
			results, err := RunProbeWith(t.Context(), tgt, p, RunOptions{KeepLastAnswer: true})
			require.ErrorContains(t, err, "observation wait: still pending after")
			require.Len(t, results, 1)
			assert.Equal(t, "first", results[0].Name)
			assert.False(t, results[0].WaitRanOut)
		})
	}
}

// TestObservationNormalizeAppliesAfterTheProbeRules asserts that an
// observation's Normalize rules run after DefaultNormalizeRules and the
// probe's rules, and only on that observation: a field the probe replaces is
// replaced again by the observation rule, a field the observation deletes is
// gone from that observation only, and the next observation records the
// probe's values.
func TestObservationNormalizeAppliesAfterTheProbeRules(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"a":        "raw-a",
			"b":        "raw-b",
			"c":        "raw-c",
			"metadata": map[string]any{"resourceVersion": "17"},
		})
	}))
	t.Cleanup(srv.Close)
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
	read := func(name string, rules ...NormalizeRule) Observation {
		return Observation{Name: name, Request: RequestSpec{Method: http.MethodGet, Path: "/x", Identity: IdentityAdmin}, Normalize: rules}
	}
	p := Probe{
		Name: "fam/obs-normalize",
		Normalize: []NormalizeRule{
			{Path: "a", Replace: "<probe>"},
			{Path: "b", Replace: "<probe>"},
		},
		Observations: []Observation{
			read("both", NormalizeRule{Path: "a", Replace: "<observation>"}, NormalizeRule{Path: "b"}, NormalizeRule{Path: "c", Replace: "<observation>"}),
			read("none"),
		},
	}
	results, err := runProbe(t.Context(), tgt, p, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, results, 2)

	assert.Equal(t, "<observation>", results[0].Body["a"], "the observation rule runs after the probe rule")
	assert.NotContains(t, results[0].Body, "b", "the observation rule deletes a field the probe rule replaced")
	assert.Equal(t, "<observation>", results[0].Body["c"])
	assert.Equal(t, map[string]any{"resourceVersion": "<rv>"}, results[0].Body["metadata"], "the default rules still run")

	assert.Equal(t, "<probe>", results[1].Body["a"], "the next observation has only the probe's rules")
	assert.Equal(t, "<probe>", results[1].Body["b"])
	assert.Equal(t, "raw-c", results[1].Body["c"])
}

// TestValidateUntil asserts that Validate accepts Until on a GET observation
// without captures and without WaitForStatus, and refuses Until with another
// method, with captures, with WaitForStatus, and an observation Normalize
// rule that is not valid, each with its own message.
func TestValidateUntil(t *testing.T) {
	t.Parallel()
	ok := validProbe()
	ok.Observations[0].Until = neverDone
	ok.Observations[0].Normalize = []NormalizeRule{{Path: "metadata.name", Replace: "<name>"}}
	require.NoError(t, ok.Validate())

	tests := []struct {
		name    string
		mutate  func(*Probe)
		wantErr string
	}{
		{"until with POST", func(p *Probe) {
			p.Observations[0].Until = neverDone
			p.Observations[0].Request.Method = http.MethodPost
		}, `observation "get": Until sends the request again and needs method GET, not POST`},
		{"until with captures", func(p *Probe) {
			p.Observations[0].Until = neverDone
			p.Observations[0].Captures = map[string]Capture{"name": {Path: "metadata.name"}}
		}, `observation "get": an observation with captures cannot set Until`},
		{"until with WaitForStatus", func(p *Probe) {
			p.Observations[0].Until = neverDone
			p.Observations[0].WaitForStatus = 200
		}, `observation "get": Until and WaitForStatus are mutually exclusive`},
		{"observation rule with replace and keep-prefix", func(p *Probe) {
			p.Observations[0].Normalize = []NormalizeRule{{Path: "a", Replace: "x", KeepPrefix: "y"}}
		}, `observation "get": normalize rule a: Replace and KeepPrefix are mutually exclusive`},
		{"observation rule with keep-keys and an empty entry", func(p *Probe) {
			p.Observations[0].Normalize = []NormalizeRule{{Path: "a", KeepKeys: []string{""}}}
		}, `observation "get": normalize rule a: KeepKeys has an empty entry`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := validProbe()
			tt.mutate(&p)
			require.ErrorContains(t, p.Validate(), tt.wantErr)
		})
	}
}

// failingTransport answers every request with a transport error.
type failingTransport struct{ n *atomic.Int32 }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.n.Add(1)
	return nil, errors.New("connection refused by the test transport")
}

// TestUntilReadsAgainAfterAReadRunsPastItsLimit asserts that a read that runs
// past untilReadTimeout is sent again, and the wait passes when the next
// answer is done.
func TestUntilReadsAgainAfterAReadRunsPastItsLimit(t *testing.T) {
	shortUntil(t, 5*time.Second, 10*time.Millisecond, 100*time.Millisecond)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			testtransport.HangAndAbort(r)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"phase":"done"}`))
	}))
	t.Cleanup(srv.Close)
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
	results, err := runProbe(t.Context(), tgt, untilProbe(func(decoded map[string]any, status int, _ map[string]string) (bool, string, error) {
		return status == http.StatusOK && decoded["phase"] == "done", "not done", nil
	}), 5*time.Second)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, http.StatusOK, results[0].Status)
	assert.GreaterOrEqual(t, n.Load(), int32(2), "the read that ran past its limit is sent again")
}

// TestUntilReadsThatAlwaysFailEndAtTheDeadline asserts that reads that always
// get a transport error are sent again, with the pause between reads, until
// the wait deadline, and the error at the deadline names the last read error.
func TestUntilReadsThatAlwaysFailEndAtTheDeadline(t *testing.T) {
	const timeout, interval = 300 * time.Millisecond, 20 * time.Millisecond
	shortUntil(t, timeout, interval, 100*time.Millisecond)
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	var n atomic.Int32
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}, client: &http.Client{Transport: failingTransport{n: &n}}}
	start := time.Now()
	_, err := runProbe(t.Context(), tgt, untilProbe(neverDone), 5*time.Second)
	require.ErrorContains(t, err, "still without an answer after")
	require.ErrorContains(t, err, "last read error:")
	require.ErrorContains(t, err, "connection refused by the test transport")
	assert.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond, "the wait runs to its deadline")
	assert.Greater(t, n.Load(), int32(2), "each failed read is sent again")
	assert.LessOrEqual(t, n.Load(), int32(timeout/interval)+2, "a failed read is sent again only after the pause")
}

// TestUntilRequestErrorEndsTheWaitAtOnce asserts that an error resolving the
// observation's bearer, which sending again cannot change, fails the wait at
// once with no request sent.
func TestUntilRequestErrorEndsTheWaitAtOnce(t *testing.T) {
	shortUntil(t, 5*time.Second, 10*time.Millisecond, time.Second)
	srv, n := countingServer(t)
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
	p := untilProbe(neverDone)
	p.Observations[0].Request.Identity = IdentityClusterOwner
	start := time.Now()
	_, err := runProbe(t.Context(), tgt, p, 5*time.Second)
	require.ErrorContains(t, err, "no clusterowner bearer")
	assert.NotContains(t, err.Error(), "still")
	assert.Less(t, time.Since(start), time.Second, "the wait ends at once")
	assert.Zero(t, n.Load(), "nothing is sent")
}
