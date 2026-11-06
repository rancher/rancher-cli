package fidelity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func jsonObservation(name string, status int, body map[string]any) ObservationResult {
	return ObservationResult{Name: name, Status: status, ContentType: "application/json", Body: body}
}

func goldenOf(obs ...ObservationResult) Golden {
	return Golden{OracleImage: "image", Observations: obs}
}

// shape is a Difference with its raw values as strings, so a table compares
// them directly; an empty string is a nil value.
type shape struct {
	Pointer    string
	Want       string
	Got        string
	WaitRanOut bool
}

func shapes(ds []Difference) []shape {
	out := make([]shape, 0, len(ds))
	for _, d := range ds {
		out = append(out, shape{d.Pointer, string(d.Want), string(d.Got), d.WaitRanOut})
	}
	return out
}

// TestDifferences asserts the structural comparison: equal recordings give
// none; a changed status, nested value, text, type, a key or an observation
// on one side only, null against absent, arrays of equal and of different
// length and the pointer escaping each give the differences the format
// promises, sorted by pointer; a probe that returned an error compares only
// the observations it completed; and every difference of an observation
// whose wait ran out is flagged.
func TestDifferences(t *testing.T) {
	read := func(status int, body map[string]any) ObservationResult {
		return jsonObservation("read", status, body)
	}
	cases := []struct {
		name      string
		want, got Golden
		completed bool
		diffs     []shape
	}{
		{
			name:      "equal recordings",
			want:      goldenOf(read(200, map[string]any{"list": []any{1.0, "a", map[string]any{"k": true}}, "n": nil})),
			got:       goldenOf(read(200, map[string]any{"list": []any{1.0, "a", map[string]any{"k": true}}, "n": nil})),
			completed: true,
		},
		{
			name:      "changed status",
			want:      goldenOf(read(200, nil)),
			got:       goldenOf(read(403, nil)),
			completed: true,
			diffs:     []shape{{Pointer: "/read/status", Want: "200", Got: "403"}},
		},
		{
			name:      "changed nested value",
			want:      goldenOf(read(200, map[string]any{"spec": map[string]any{"a": map[string]any{"b": "x"}}})),
			got:       goldenOf(read(200, map[string]any{"spec": map[string]any{"a": map[string]any{"b": "y"}}})),
			completed: true,
			diffs:     []shape{{Pointer: "/read/body/spec/a/b", Want: `"x"`, Got: `"y"`}},
		},
		{
			name:      "key on the run side only",
			want:      goldenOf(read(200, map[string]any{"a": 1.0})),
			got:       goldenOf(read(200, map[string]any{"a": 1.0, "b": map[string]any{"c": 2.0}})),
			completed: true,
			diffs:     []shape{{Pointer: "/read/body/b", Got: `{"c":2}`}},
		},
		{
			name:      "key on the golden side only",
			want:      goldenOf(read(200, map[string]any{"a": 1.0, "b": "x"})),
			got:       goldenOf(read(200, map[string]any{"a": 1.0})),
			completed: true,
			diffs:     []shape{{Pointer: "/read/body/b", Want: `"x"`}},
		},
		{
			name:      "observation on the run side only",
			want:      goldenOf(read(200, nil)),
			got:       goldenOf(read(200, nil), jsonObservation("extra", 404, nil)),
			completed: true,
			diffs:     []shape{{Pointer: "/extra", Got: `{"contentType":"application/json","status":404}`}},
		},
		{
			name:      "observation on the golden side only",
			want:      goldenOf(read(200, nil), jsonObservation("extra", 404, nil)),
			got:       goldenOf(read(200, nil)),
			completed: true,
			diffs:     []shape{{Pointer: "/extra", Want: `{"contentType":"application/json","status":404}`}},
		},
		{
			name:      "null against absent",
			want:      goldenOf(read(200, map[string]any{"a": nil, "x": 1.0})),
			got:       goldenOf(read(200, map[string]any{"x": 1.0})),
			completed: true,
			diffs:     []shape{{Pointer: "/read/body/a", Want: "null"}},
		},
		{
			name:      "absent against null",
			want:      goldenOf(read(200, map[string]any{"x": 1.0})),
			got:       goldenOf(read(200, map[string]any{"a": nil, "x": 1.0})),
			completed: true,
			diffs:     []shape{{Pointer: "/read/body/a", Got: "null"}},
		},
		{
			name:      "arrays of equal length with one changed element",
			want:      goldenOf(read(200, map[string]any{"list": []any{"a", "b", "c"}})),
			got:       goldenOf(read(200, map[string]any{"list": []any{"a", "x", "c"}})),
			completed: true,
			diffs:     []shape{{Pointer: "/read/body/list/1", Want: `"b"`, Got: `"x"`}},
		},
		{
			name:      "arrays of different length",
			want:      goldenOf(read(200, map[string]any{"list": []any{"a", "b"}})),
			got:       goldenOf(read(200, map[string]any{"list": []any{"a", "b", "c"}})),
			completed: true,
			diffs:     []shape{{Pointer: "/read/body/list", Want: `["a","b"]`, Got: `["a","b","c"]`}},
		},
		{
			name:      "type change",
			want:      goldenOf(read(200, map[string]any{"v": 1.0})),
			got:       goldenOf(read(200, map[string]any{"v": "1"})),
			completed: true,
			diffs:     []shape{{Pointer: "/read/body/v", Want: "1", Got: `"1"`}},
		},
		{
			name:      "object against array",
			want:      goldenOf(read(200, map[string]any{"v": map[string]any{"a": 1.0}})),
			got:       goldenOf(read(200, map[string]any{"v": []any{1.0}})),
			completed: true,
			diffs:     []shape{{Pointer: "/read/body/v", Want: `{"a":1}`, Got: `[1]`}},
		},
		{
			name:      "text",
			want:      goldenOf(ObservationResult{Name: "read", Status: 200, ContentType: "text/plain", Text: "one"}),
			got:       goldenOf(ObservationResult{Name: "read", Status: 200, ContentType: "text/plain", Text: "two"}),
			completed: true,
			diffs:     []shape{{Pointer: "/read/text", Want: `"one"`, Got: `"two"`}},
		},
		{
			name:      "names with a slash and a tilde",
			want:      goldenOf(jsonObservation("a/b~c", 200, map[string]any{"x/y": "1", "m~n": "1"})),
			got:       goldenOf(jsonObservation("a/b~c", 201, map[string]any{"x/y": "2", "m~n": "2"})),
			completed: true,
			diffs: []shape{
				{Pointer: "/a~1b~0c/body/m~0n", Want: `"1"`, Got: `"2"`},
				{Pointer: "/a~1b~0c/body/x~1y", Want: `"1"`, Got: `"2"`},
				{Pointer: "/a~1b~0c/status", Want: "200", Got: "201"},
			},
		},
		{
			name: "an errored probe compares only the observations it completed",
			want: goldenOf(read(200, map[string]any{"a": 1.0}),
				jsonObservation("second", 200, nil), jsonObservation("third", 200, nil)),
			got:       goldenOf(read(200, map[string]any{"a": 2.0}), jsonObservation("second", 200, nil)),
			completed: false,
			diffs:     []shape{{Pointer: "/read/body/a", Want: "1", Got: "2"}},
		},
		{
			name: "a completed probe reports the missing observation",
			want: goldenOf(read(200, nil), jsonObservation("second", 200, nil)),
			got:  goldenOf(read(200, nil)), completed: true,
			diffs: []shape{{Pointer: "/second", Want: `{"contentType":"application/json","status":200}`}},
		},
		{
			name: "a run-side observation of an errored probe is still reported",
			want: goldenOf(read(200, nil)),
			got:  goldenOf(read(200, nil), jsonObservation("extra", 200, nil)), completed: false,
			diffs: []shape{{Pointer: "/extra", Got: `{"contentType":"application/json","status":200}`}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Differences("fam/probe", tc.want, tc.got, tc.completed)
			assert.Equal(t, append([]shape{}, tc.diffs...), append([]shape{}, shapes(got)...))
			for _, d := range got {
				assert.Equal(t, "fam/probe", d.Probe)
			}
		})
	}

	t.Run("a wait that ran out flags the status and the body difference", func(t *testing.T) {
		ranOut := read(200, map[string]any{"v": 2.0})
		ranOut.WaitRanOut = true
		got := Differences("fam/probe", goldenOf(read(401, map[string]any{"v": 1.0})), goldenOf(ranOut), true)
		assert.Equal(t, []shape{
			{Pointer: "/read/body/v", Want: "1", Got: "2", WaitRanOut: true},
			{Pointer: "/read/status", Want: "401", Got: "200", WaitRanOut: true},
		}, shapes(got))
	})

	t.Run("only the observation whose wait ran out is flagged", func(t *testing.T) {
		ranOut := jsonObservation("waited", 200, nil)
		ranOut.WaitRanOut = true
		got := Differences("fam/probe",
			goldenOf(jsonObservation("waited", 401, nil), jsonObservation("other", 200, nil)),
			goldenOf(ranOut, jsonObservation("other", 500, nil)), true)
		assert.Equal(t, []shape{
			{Pointer: "/other/status", Want: "200", Got: "500"},
			{Pointer: "/waited/status", Want: "401", Got: "200", WaitRanOut: true},
		}, shapes(got))
	})

	t.Run("the result is sorted by pointer", func(t *testing.T) {
		got := Differences("fam/probe",
			goldenOf(jsonObservation("b", 1, nil), jsonObservation("a", 1, nil), jsonObservation("c", 1, nil)),
			goldenOf(jsonObservation("c", 2, nil), jsonObservation("b", 2, nil), jsonObservation("a", 2, nil)), true)
		var pointers []string
		for _, d := range got {
			pointers = append(pointers, d.Pointer)
		}
		assert.Equal(t, []string{"/a/status", "/b/status", "/c/status"}, pointers)
	})

	t.Run("the golden's image is not compared", func(t *testing.T) {
		want, got := goldenOf(read(200, nil)), goldenOf(read(200, nil))
		got.OracleImage = "another"
		assert.Empty(t, Differences("fam/probe", want, got, true))
	})
}

// waitTwice is a probe whose first observation waits for a status the
// server never answers and whose second observation answers normally.
func waitTwice(status int) Probe {
	return Probe{Name: "fam/wait-twice", Observations: []Observation{
		{Name: "waits", Request: RequestSpec{Method: http.MethodGet, Path: "/x", Identity: IdentityAdmin}, WaitForStatus: status},
		{Name: "after", Request: RequestSpec{Method: http.MethodGet, Path: "/y", Identity: IdentityAdmin}},
	}}
}

func waitTwiceTarget(t *testing.T) *fakeTarget {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /x", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"steady"}`))
	})
	mux.HandleFunc("GET /y", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"after"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
}

// TestRunProbeWithKeepsTheLastAnswer asserts that with KeepLastAnswer an
// observation whose wait runs out returns its last answer with WaitRanOut
// and the probe goes on to the next observation. It fails when the wait
// error is returned instead.
func TestRunProbeWithKeepsTheLastAnswer(t *testing.T) {
	shortWait(t, 200*time.Millisecond, 20*time.Millisecond)
	tgt := waitTwiceTarget(t)
	results, err := runProbeWith(t.Context(), tgt, waitTwice(401), 5*time.Second, RunOptions{KeepLastAnswer: true})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "waits", results[0].Name)
	assert.Equal(t, http.StatusOK, results[0].Status)
	assert.True(t, results[0].WaitRanOut)
	assert.Equal(t, map[string]any{"state": "steady"}, results[0].Body)
	assert.Equal(t, "after", results[1].Name)
	assert.False(t, results[1].WaitRanOut)
}

// TestRunProbeWithKeepsAnObservationThatArrives asserts that an observation
// whose status arrives in time is not marked, with the option on.
func TestRunProbeWithKeepsAnObservationThatArrives(t *testing.T) {
	shortWait(t, 2*time.Second, 10*time.Millisecond)
	srv, _ := statusSequence(t, 200, 401)
	tgt := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}}
	results, err := runProbeWith(t.Context(), tgt, waitProbe(401), 5*time.Second, RunOptions{KeepLastAnswer: true})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, 401, results[0].Status)
	assert.False(t, results[0].WaitRanOut)
}

// TestRunProbeWithoutTheOptionFailsTheWait asserts that RunProbe and
// RunProbeWith with no option keep today's behavior: a wait that runs out
// fails the probe and returns no result. It fails when the option defaults
// on.
func TestRunProbeWithoutTheOptionFailsTheWait(t *testing.T) {
	shortWait(t, 200*time.Millisecond, 20*time.Millisecond)
	tgt := waitTwiceTarget(t)
	for name, run := range map[string]func() ([]ObservationResult, error){
		"RunProbe": func() ([]ObservationResult, error) { return RunProbe(t.Context(), tgt, waitTwice(401)) },
		"RunProbeWith": func() ([]ObservationResult, error) {
			return RunProbeWith(t.Context(), tgt, waitTwice(401), RunOptions{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			results, err := run()
			require.ErrorContains(t, err, "observation waits: want status 401, still 200 after ")
			assert.Empty(t, results)
		})
	}
}

// TestRunProbeWithAContextCancelStillFails asserts that a cancelled context
// fails the probe even with KeepLastAnswer.
func TestRunProbeWithAContextCancelStillFails(t *testing.T) {
	shortWait(t, 10*time.Second, 5*time.Second)
	tgt := waitTwiceTarget(t)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	results, err := runProbeWith(ctx, tgt, waitTwice(401), 30*time.Second, RunOptions{KeepLastAnswer: true})
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, results)
}

// cancelAfterAnswer calls cancel right after the real round trip returned
// its response, so the probe's context ends while the answer is in hand.
type cancelAfterAnswer struct {
	next   http.RoundTripper
	cancel context.CancelFunc
}

func (c cancelAfterAnswer) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(r)
	c.cancel()
	return resp, err
}

// TestRunProbeWithAContextThatEndsDuringTheWaitFails asserts that a context
// that ends while the answer of a timed-out wait is in hand fails the probe
// with the wait error and keeps no result, whatever the options say. It fails
// when the answer is kept although the context ended.
func TestRunProbeWithAContextThatEndsDuringTheWaitFails(t *testing.T) {
	shortWait(t, time.Nanosecond, 20*time.Millisecond)
	tgt := waitTwiceTarget(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tgt.client = &http.Client{Transport: cancelAfterAnswer{next: tgt.srv.Client().Transport, cancel: cancel}}
	results, err := runProbeWith(ctx, tgt, waitTwice(401), 30*time.Second, RunOptions{KeepLastAnswer: true})
	require.ErrorContains(t, err, "observation waits: want status 401, still 200 after ")
	assert.Empty(t, results)
}

func entry(probe, pointer, want, got string) KnownEntry {
	e := KnownEntry{Probe: probe, Pointer: pointer, FirstSeen: "abc1234", Cause: CauseRancher, Ruling: RulingExpectedAtGA}
	if want != "" {
		e.Want = json.RawMessage(want)
	}
	if got != "" {
		e.Got = json.RawMessage(got)
	}
	return e
}

func diff(probe, pointer, want, got string) Difference {
	d := Difference{Probe: probe, Pointer: pointer}
	if want != "" {
		d.Want = json.RawMessage(want)
	}
	if got != "" {
		d.Got = json.RawMessage(got)
	}
	return d
}

// testRun is the input of Compare: the header, the probes, the CLI tests,
// the harness failures and the number of tests that ran.
type testRun struct {
	header   Report
	probes   []ProbeRun
	cliTests []CLITestRun
	harness  []HarnessFailure
	testsRan int
}

// compareRun compares the run with a known file.
func compareRun(known KnownFile, run testRun) Report {
	return Compare(known, run.header, run.probes, run.cliTests, run.harness, run.testsRan)
}

// cleanRun is a run in which both suites ran a probe, a test ran and nothing
// else happened; cases add to it.
func cleanRun(diffs ...Difference) testRun {
	probes := []ProbeRun{
		{Suite: SuiteOracle, Probe: "fam/a"},
		{Suite: SuiteDownstream, Probe: "fam/down", Downstream: true},
	}
	for _, d := range diffs {
		for i := range probes {
			if probes[i].Probe == d.Probe {
				probes[i].Differences = append(probes[i].Differences, d)
			}
		}
	}
	return testRun{
		header: Report{
			Minor: "2.16", StartedAt: "2026-10-20T19:00:00Z", GitCommit: "0123456789abcdef0123456789abcdef01234567",
			GoldenK3sImage: "k3s:golden", DownstreamImage: "k3s:golden",
		},
		probes: probes, testsRan: 3,
	}
}

func newPointers(r Report) []string {
	var out []string
	for _, n := range r.New {
		out = append(out, n.Entry.Probe+n.Entry.Pointer)
	}
	return out
}

func entryPointers(es []KnownEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Probe+e.Pointer)
	}
	return out
}

// TestCompare asserts the matching of differences against known entries and
// the report's lists, counts and clean flag.
func TestCompare(t *testing.T) {
	t.Run("a difference no entry matches is new, with the run's commit and an empty ruling", func(t *testing.T) {
		r := compareRun(KnownFile{}, cleanRun(diff("fam/a", "/read/status", "200", "403")))
		require.Len(t, r.New, 1)
		n := r.New[0].Entry
		assert.Equal(t, "fam/a", n.Probe)
		assert.Equal(t, "/read/status", n.Pointer)
		assert.JSONEq(t, "200", string(n.Want))
		assert.JSONEq(t, "403", string(n.Got))
		assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", n.FirstSeen)
		assert.Equal(t, CauseRancher, n.Cause)
		assert.Empty(t, n.Ruling)
		assert.False(t, r.Clean)
		assert.Equal(t, 1, r.Counts.New)
	})

	t.Run("an entry with every value equal is known", func(t *testing.T) {
		known := KnownFile{Minor: "2.16", Entries: []KnownEntry{entry("fam/a", "/read/status", "200", "403")}}
		r := compareRun(known, cleanRun(diff("fam/a", "/read/status", "200", "403")))
		assert.Empty(t, r.New)
		assert.Len(t, r.Known, 1)
		assert.Empty(t, r.KnownNoRuling)
		assert.Empty(t, r.NoLongerSeen)
		assert.True(t, r.Clean)
		assert.Equal(t, DriftCounts{Known: 1}, r.Counts)
	})

	t.Run("a known entry with no ruling is listed apart", func(t *testing.T) {
		e := entry("fam/a", "/read/status", "200", "403")
		e.Ruling = ""
		r := compareRun(KnownFile{Entries: []KnownEntry{e}}, cleanRun(diff("fam/a", "/read/status", "200", "403")))
		assert.Empty(t, r.Known)
		assert.Len(t, r.KnownNoRuling, 1)
		assert.Equal(t, 1, r.Counts.KnownNoRuling)
		assert.Zero(t, r.Counts.Known)
		assert.True(t, r.Clean)
	})

	t.Run("the same probe and pointer with another got is new and the entry is no longer seen", func(t *testing.T) {
		known := KnownFile{Entries: []KnownEntry{entry("fam/a", "/read/status", "200", "403")}}
		r := compareRun(known, cleanRun(diff("fam/a", "/read/status", "200", "500")))
		assert.Equal(t, []string{"fam/a/read/status"}, newPointers(r))
		assert.Equal(t, []string{"fam/a/read/status"}, entryPointers(r.NoLongerSeen))
		assert.Empty(t, r.Known)
		assert.False(t, r.Clean)
	})

	t.Run("the same probe and pointer with another want is new", func(t *testing.T) {
		known := KnownFile{Entries: []KnownEntry{entry("fam/a", "/read/status", "200", "403")}}
		r := compareRun(known, cleanRun(diff("fam/a", "/read/status", "201", "403")))
		assert.Len(t, r.New, 1)
		assert.Len(t, r.NoLongerSeen, 1)
	})

	t.Run("gotVaries matches any present value and not an absent one", func(t *testing.T) {
		e := entry("fam/a", "/edition/body/GitCommit", `"abc"`, "")
		e.GotVaries = true
		known := KnownFile{Entries: []KnownEntry{e}}

		r := compareRun(known, cleanRun(diff("fam/a", "/edition/body/GitCommit", `"abc"`, `"def"`)))
		assert.Empty(t, r.New)
		assert.Len(t, r.Known, 1)

		r = compareRun(known, cleanRun(diff("fam/a", "/edition/body/GitCommit", `"abc"`, "")))
		assert.Len(t, r.New, 1, "nothing at the pointer on the run's side is not a value")
		assert.Len(t, r.NoLongerSeen, 1)

		r = compareRun(known, cleanRun(diff("fam/a", "/edition/body/GitCommit", `"other"`, `"def"`)))
		assert.Len(t, r.New, 1, "the golden's value still has to match")
	})

	t.Run("object and array values written by the file's encoder match", func(t *testing.T) {
		e := entry("fam/a", "/read/body/spec", `{"b": [1, 2, {"z": null, "a": "x"}], "a": "\u00b7"}`, `{"list": ["x", "y"]}`)
		raw, err := encodeDriftJSON(KnownFile{Minor: "2.16", Entries: []KnownEntry{e}})
		require.NoError(t, err)
		fromFile, err := ParseKnownFile(raw)
		require.NoError(t, err)
		d := diff("fam/a", "/read/body/spec",
			"{\"a\":\"\u00b7\",\"b\":[1,2,{\"a\":\"x\",\"z\":null}]}", `{"list":["x","y"]}`)
		r := compareRun(fromFile, cleanRun(d))
		assert.Empty(t, r.New, "the file's values are canonical once parsed: indentation and key order never matter")
		assert.Len(t, r.Known, 1)
	})

	t.Run("an entry of an errored probe is not checked", func(t *testing.T) {
		run := cleanRun()
		run.probes[0].Err = "boom"
		r := compareRun(KnownFile{Entries: []KnownEntry{entry("fam/a", "/read/status", "200", "403")}}, run)
		assert.Len(t, r.NotChecked, 1)
		assert.Empty(t, r.NoLongerSeen)
		assert.Equal(t, []ErroredProbe{{Probe: "fam/a", Error: "boom"}}, r.Errored)
		assert.False(t, r.Clean)
	})

	t.Run("a probe whose MaxMinor is older than the server's minor is listed in NotCheckedProbes and is not an error", func(t *testing.T) {
		run := cleanRun()
		run.probes = append(run.probes, ProbeRun{Suite: SuiteOracle, Probe: "fam/maxminor", NotCheckedReason: "probe fam/maxminor: reason"})
		known := KnownFile{Entries: []KnownEntry{
			entry("fam/maxminor", "/read/status", "200", "404"),
			entry("fam/a", "/read/status", "200", "403"),
		}}
		r := compareRun(known, run)
		assert.Equal(t, []NotCheckedProbe{{Probe: "fam/maxminor", Reason: "probe fam/maxminor: reason"}}, r.NotCheckedProbes)
		assert.Empty(t, r.Errored, "a probe that is not checked is not an error")
		assert.True(t, r.Clean, "the report stays clean when a probe is not checked")
		assert.Equal(t, []string{"fam/maxminor/read/status"}, entryPointers(r.NotChecked), "its known entry is not checked")
		assert.Equal(t, []string{"fam/a/read/status"}, entryPointers(r.NoLongerSeen), "an entry of a probe that ran is still no longer seen")
		assert.Equal(t, []SuiteResult{{Name: SuiteOracle, Probes: 1}, {Name: SuiteDownstream, Probes: 1}}, r.Suites,
			"the suite's probe count has only the probes that ran")
		assert.Equal(t, DriftCounts{NoLongerSeen: 1, NotChecked: 1, NotCheckedProbes: 1}, r.Counts)
	})

	t.Run("a suite whose probes were all not checked is not counted in SuitesWithNoProbe", func(t *testing.T) {
		run := cleanRun()
		run.probes[1] = ProbeRun{Suite: SuiteDownstream, Probe: "fam/down", Downstream: true, NotCheckedReason: "reason"}
		r := compareRun(KnownFile{}, run)
		assert.Equal(t, []SuiteResult{{Name: SuiteOracle, Probes: 1}, {Name: SuiteDownstream, Probes: 0}}, r.Suites)
		assert.Zero(t, r.Counts.SuitesWithNoProbe)
		assert.Equal(t, 1, r.Counts.NotCheckedProbes)
		assert.True(t, r.Clean)

		run.probes = run.probes[:1]
		r = compareRun(KnownFile{}, run)
		assert.Equal(t, 1, r.Counts.SuitesWithNoProbe, "a suite with no probe record is counted in SuitesWithNoProbe")
		assert.False(t, r.Clean)
	})

	t.Run("probes not checked are sorted by name", func(t *testing.T) {
		run := cleanRun()
		run.probes = append(run.probes,
			ProbeRun{Suite: SuiteOracle, Probe: "fam/z", NotCheckedReason: "r"},
			ProbeRun{Suite: SuiteOracle, Probe: "fam/b", NotCheckedReason: "r"})
		r := compareRun(KnownFile{}, run)
		assert.Equal(t, []NotCheckedProbe{{Probe: "fam/b", Reason: "r"}, {Probe: "fam/z", Reason: "r"}}, r.NotCheckedProbes)
	})

	t.Run("an entry of a probe that did not run is not checked", func(t *testing.T) {
		r := compareRun(KnownFile{Entries: []KnownEntry{entry("fam/other", "/read/status", "200", "403")}}, cleanRun())
		assert.Len(t, r.NotChecked, 1)
		assert.Empty(t, r.NoLongerSeen)
		assert.True(t, r.Clean)
	})

	t.Run("an errored probe's completed observations are compared", func(t *testing.T) {
		run := cleanRun(diff("fam/a", "/read/status", "200", "403"))
		run.probes[0].Err = "boom"
		r := compareRun(KnownFile{}, run)
		assert.Equal(t, []string{"fam/a/read/status"}, newPointers(r))
		assert.Equal(t, 1, r.Counts.Errored)
	})

	t.Run("no file means every difference is new", func(t *testing.T) {
		r := compareRun(KnownFile{}, cleanRun(
			diff("fam/a", "/read/status", "200", "403"), diff("fam/a", "/read/body/x", `"a"`, `"b"`)))
		assert.Len(t, r.New, 2)
		assert.False(t, r.KnownFileExists)
		assert.Empty(t, r.Known)
	})

	t.Run("a wait that ran out is new, flagged and counted on its own", func(t *testing.T) {
		status := diff("fam/a", "/read/status", "401", "200")
		status.WaitRanOut = true
		r := compareRun(KnownFile{}, cleanRun(diff("fam/a", "/read/body/x", "1", "2"), status))
		assert.Equal(t, 2, r.Counts.New)
		assert.Equal(t, 1, r.Counts.WaitRanOut)
		flags := map[string]bool{}
		for _, n := range r.New {
			flags[n.Entry.Pointer] = n.WaitRanOut
		}
		assert.Equal(t, map[string]bool{"/read/body/x": false, "/read/status": true}, flags)
	})

	t.Run("a wait that ran out and matches an entry is known", func(t *testing.T) {
		status := diff("fam/a", "/read/status", "401", "200")
		status.WaitRanOut = true
		known := KnownFile{Entries: []KnownEntry{entry("fam/a", "/read/status", "401", "200")}}
		r := compareRun(known, cleanRun(status))
		assert.Empty(t, r.New)
		assert.Len(t, r.Known, 1)
		assert.Zero(t, r.Counts.WaitRanOut)
		assert.True(t, r.Clean)
	})

	t.Run("clean and not clean", func(t *testing.T) {
		assert.True(t, compareRun(KnownFile{}, cleanRun()).Clean)

		run := cleanRun()
		run.probes = run.probes[:1]
		r := compareRun(KnownFile{}, run)
		assert.False(t, r.Clean, "a suite that ran no probe")
		assert.Equal(t, 1, r.Counts.SuitesWithNoProbe)
		assert.Equal(t, []SuiteResult{{Name: SuiteOracle, Probes: 1}, {Name: SuiteDownstream, Probes: 0}}, r.Suites)

		run = cleanRun()
		run.testsRan = 0
		assert.False(t, compareRun(KnownFile{}, run).Clean, "no test ran")

		run = cleanRun()
		run.cliTests = []CLITestRun{{Name: "TestCLI", Started: true, Passed: false}}
		r = compareRun(KnownFile{}, run)
		assert.False(t, r.Clean, "a failed CLI test")
		assert.Equal(t, 1, r.Counts.FailedCLITests)
		assert.Empty(t, r.HarnessFailures)

		run = cleanRun()
		run.cliTests = []CLITestRun{{Name: "TestCLI", Started: true, Passed: true}}
		assert.True(t, compareRun(KnownFile{}, run).Clean)
	})

	t.Run("a harness failure is listed and is not part of clean", func(t *testing.T) {
		run := cleanRun()
		run.harness = []HarnessFailure{{Name: "TestOracle", Reason: "the test failed"}}
		r := compareRun(KnownFile{}, run)
		assert.Equal(t, []HarnessFailure{{Name: "TestOracle", Reason: "the test failed"}}, r.HarnessFailures)
		assert.Equal(t, 1, r.Counts.HarnessFailures)
		assert.True(t, r.Clean)
	})

	t.Run("a suite with no probe and a harness failure lists both", func(t *testing.T) {
		run := cleanRun()
		run.probes = run.probes[:1]
		run.harness = []HarnessFailure{{Name: SuiteDownstream, Reason: "the test failed"}}
		r := compareRun(KnownFile{}, run)
		assert.Equal(t, []HarnessFailure{{Name: SuiteDownstream, Reason: "the test failed"}}, r.HarnessFailures)
		assert.Equal(t, 1, r.Counts.SuitesWithNoProbe)
		assert.False(t, r.Clean)
	})

	t.Run("a CLI test that failed before its command is a harness failure and after it a failed test", func(t *testing.T) {
		c := &DriftCollector{}
		c.CLITest("TestEarly", false)
		c.CLIStarted("TestLate")
		c.CLITest("TestLate", false)
		c.CLIStarted("TestFine")
		c.CLITest("TestFine", true)
		run := cleanRun()
		run.cliTests = c.cliTests
		r := compareRun(KnownFile{}, run)
		assert.Equal(t, []HarnessFailure{{Name: "TestEarly", Reason: "the CLI test failed before its CLI command ran"}}, r.HarnessFailures)
		assert.Equal(t, 1, r.Counts.FailedCLITests)
		assert.Equal(t, []CLITestRun{
			{Name: "TestEarly"},
			{Name: "TestFine", Started: true, Passed: true},
			{Name: "TestLate", Started: true},
		}, r.CLITests)
	})
}

// TestAddNotCheckedProbe asserts that the collector records a probe
// that is not checked with its reason and no error or difference, that it
// reaches the report through FinishDriftRun, and that a probe recorded
// through Probe with an error is an error instead.
func TestAddNotCheckedProbe(t *testing.T) {
	c := &DriftCollector{}
	c.AddNotCheckedProbe(SuiteOracle, "fam/maxminor", false, "the reason")
	require.Len(t, c.probes, 1)
	assert.Equal(t, ProbeRun{Suite: SuiteOracle, Probe: "fam/maxminor", NotCheckedReason: "the reason"}, c.probes[0])

	in := finishInputs(t)
	in.Collector.AddNotCheckedProbe(SuiteOracle, "fam/maxminor", false, "the reason")
	in.Collector.AddNotCheckedProbe(SuiteDownstream, "fam/down", true, "another reason")
	r, code := FinishDriftRun(in)
	assert.Equal(t, []NotCheckedProbe{{Probe: "fam/down", Reason: "another reason"}, {Probe: "fam/maxminor", Reason: "the reason"}}, r.NotCheckedProbes)
	assert.Empty(t, r.Errored)
	assert.Equal(t, 2, r.Counts.NotCheckedProbes)
	assert.Zero(t, r.Counts.SuitesWithNoProbe)
	assert.True(t, r.Clean)
	assert.Equal(t, 0, code)

	in = finishInputs(t)
	in.Collector.Probe(SuiteOracle, "fam/maxminor", false, nil, errors.New("the reason"))
	r, _ = FinishDriftRun(in)
	assert.Empty(t, r.NotCheckedProbes)
	assert.Len(t, r.Errored, 1)
	assert.False(t, r.Clean)
}

// TestCompareFillsTheCause asserts that a difference of a probe that is not
// downstream is rancher, a downstream difference on a run with the goldens'
// k3s is rancher, and a downstream difference on any other k3s is
// undetermined.
func TestCompareFillsTheCause(t *testing.T) {
	cases := []struct {
		name       string
		probe      string
		downstream string
		want       string
	}{
		{"not a downstream probe on another k3s", "fam/a", "k3s:other", CauseRancher},
		{"downstream probe on the goldens' k3s", "fam/down", "k3s:golden", CauseRancher},
		{"downstream probe on another k3s", "fam/down", "k3s:other", CauseUndetermined},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := cleanRun(diff(tc.probe, "/read/status", "200", "403"))
			run.header.DownstreamImage = tc.downstream
			r := compareRun(KnownFile{}, run)
			require.Len(t, r.New, 1)
			assert.Equal(t, tc.want, r.New[0].Entry.Cause)
		})
	}
}

func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func allDriftVars() map[string]string {
	return map[string]string{
		EnvDriftReport: "/tmp/report.json", EnvDriftKnown: "/tmp/known.json",
		EnvDriftStarted: "2026-10-20T19:00:00Z", EnvGoldenImage: "rancher/rancher:golden@sha256:aa",
		EnvGoldenK3sImage: "rancher/k3s:golden@sha256:bb", EnvDownstreamImage: "rancher/k3s:next@sha256:cc",
		envFidelityMinor: "2.16", envFidelityOracleIm: "rancher/rancher:head@sha256:dd",
	}
}

// TestDriftConfigFromEnv asserts that none of the six drift variables set
// is off, all set is on, each missing alone is an error that names it, an
// empty minor is an error, and -update with any of them is an error.
func TestDriftConfigFromEnv(t *testing.T) {
	t.Run("none set is off", func(t *testing.T) {
		cfg, on, err := DriftConfigFromEnv(env(map[string]string{envFidelityMinor: "2.16"}), false)
		require.NoError(t, err)
		assert.False(t, on)
		assert.Equal(t, DriftConfig{}, cfg)
	})
	t.Run("none set with update is off", func(t *testing.T) {
		_, on, err := DriftConfigFromEnv(env(nil), true)
		require.NoError(t, err)
		assert.False(t, on)
	})
	t.Run("all set is on", func(t *testing.T) {
		cfg, on, err := DriftConfigFromEnv(env(allDriftVars()), false)
		require.NoError(t, err)
		assert.True(t, on)
		assert.Equal(t, DriftConfig{
			Minor: "2.16", ReportPath: "/tmp/report.json", KnownPath: "/tmp/known.json",
			StartedAt: "2026-10-20T19:00:00Z", GoldenImage: "rancher/rancher:golden@sha256:aa",
			GoldenK3sImage: "rancher/k3s:golden@sha256:bb", DownstreamImage: "rancher/k3s:next@sha256:cc",
			OracleImage: "rancher/rancher:head@sha256:dd",
		}, cfg)
	})
	for _, name := range driftVariables {
		t.Run("missing "+name, func(t *testing.T) {
			vars := allDriftVars()
			delete(vars, name)
			_, on, err := DriftConfigFromEnv(env(vars), false)
			require.ErrorContains(t, err, "drift mode needs every drift variable; missing: "+name+"; scripts/e2e-fidelity sets them when E2E_FIDELITY_DRIFT=1")
			assert.False(t, on)
		})
	}
	t.Run("missing several lists each", func(t *testing.T) {
		vars := allDriftVars()
		delete(vars, EnvDriftKnown)
		delete(vars, EnvGoldenImage)
		_, _, err := DriftConfigFromEnv(env(vars), false)
		require.ErrorContains(t, err, "missing: "+EnvDriftKnown+", "+EnvGoldenImage)
	})
	t.Run("an empty minor is an error", func(t *testing.T) {
		vars := allDriftVars()
		vars[envFidelityMinor] = ""
		_, on, err := DriftConfigFromEnv(env(vars), false)
		require.ErrorContains(t, err, "drift mode needs E2E_FIDELITY_MINOR")
		assert.False(t, on)
	})
	t.Run("update is an error", func(t *testing.T) {
		_, on, err := DriftConfigFromEnv(env(allDriftVars()), true)
		require.ErrorContains(t, err, "drift mode compares only; it never records goldens (-update is set)")
		assert.False(t, on)
	})
	t.Run("update with one variable set is an error", func(t *testing.T) {
		_, _, err := DriftConfigFromEnv(env(map[string]string{EnvDriftReport: "/tmp/r.json"}), true)
		require.ErrorContains(t, err, "(-update is set)")
	})
}

func finishInputs(t *testing.T) DriftInputs {
	t.Helper()
	c := &DriftCollector{}
	c.Test("TestOracle", false)
	c.Test("TestOracle/fam/a", false)
	c.Test("TestDownstream", false)
	c.Test("TestDownstream/fam/down", false)
	c.Probe(SuiteOracle, "fam/a", false, nil, nil)
	c.Probe(SuiteDownstream, "fam/down", true, nil, nil)
	return DriftInputs{
		Config: DriftConfig{
			Minor: "2.16", KnownPath: "/known/v2.16.json", StartedAt: "2026-10-20T19:00:00Z",
			GoldenImage: "golden", GoldenK3sImage: "k3s:golden", DownstreamImage: "k3s:golden", OracleImage: "head",
		},
		Collector: c,
		Version:   RancherVersion{Version: "v2.16-head", GitCommit: "0123456789abcdef"},
	}
}

func harnessNames(r Report) []string {
	var out []string
	for _, h := range r.HarnessFailures {
		out = append(out, h.Name)
	}
	return out
}

// TestFinishDriftRun asserts the rules that run after m.Run: each harness
// failure gives exit 1 and is listed, a clean run exits 0, a difference
// exits 1 with no harness failure, and an unexplained non-zero m.Run is a
// harness failure only when nothing recorded explains it.
func TestFinishDriftRun(t *testing.T) {
	t.Run("a clean run", func(t *testing.T) {
		r, code := FinishDriftRun(finishInputs(t))
		assert.Equal(t, 0, code)
		assert.True(t, r.Clean)
		assert.Empty(t, r.HarnessFailures)
		assert.Equal(t, "v2.16-head", r.RancherVersion)
		assert.Equal(t, "0123456789abcdef", r.GitCommit)
		assert.Equal(t, "head", r.OracleImage)
		assert.Equal(t, "golden", r.GoldenOracleImage)
		assert.Equal(t, "/known/v2.16.json", r.KnownFile)
		assert.False(t, r.Complete, "only WriteReport completes a report")
	})

	t.Run("a failed rancherversion fetch", func(t *testing.T) {
		in := finishInputs(t)
		in.Version, in.VersionErr = RancherVersion{}, errors.New("connection refused")
		r, code := FinishDriftRun(in)
		assert.Equal(t, 1, code)
		assert.Equal(t, []HarnessFailure{{Name: "/rancherversion", Reason: "connection refused"}}, r.HarnessFailures)
		assert.Empty(t, r.GitCommit)
	})

	t.Run("a failure to build the oracle target", func(t *testing.T) {
		in := finishInputs(t)
		in.Version, in.TargetErr = RancherVersion{}, errors.New("E2E_FIDELITY_CACERT: no certificates parsed")
		r, code := FinishDriftRun(in)
		assert.Equal(t, 1, code)
		assert.Equal(t, []HarnessFailure{{Name: "oracle target", Reason: "E2E_FIDELITY_CACERT: no certificates parsed"}}, r.HarnessFailures)
	})

	t.Run("a guard failure", func(t *testing.T) {
		in := finishInputs(t)
		in.GuardErr = errors.New("a request used the default transport")
		r, code := FinishDriftRun(in)
		assert.Equal(t, 1, code)
		assert.Equal(t, []HarnessFailure{{Name: "default transport guard", Reason: "a request used the default transport"}}, r.HarnessFailures)
	})

	t.Run("m.Run non-zero with nothing recorded", func(t *testing.T) {
		in := finishInputs(t)
		in.RunCode = 1
		r, code := FinishDriftRun(in)
		assert.Equal(t, 1, code)
		assert.Equal(t, []HarnessFailure{{Name: "go test", Reason: "unexplained test failure"}}, r.HarnessFailures)
	})

	t.Run("m.Run non-zero explained by a failed CLI test", func(t *testing.T) {
		in := finishInputs(t)
		in.RunCode = 1
		in.Collector.CLIStarted("TestCLIKubernetesVersions")
		in.Collector.CLITest("TestCLIKubernetesVersions", false)
		r, code := FinishDriftRun(in)
		assert.Equal(t, 1, code)
		assert.Empty(t, r.HarnessFailures)
		assert.False(t, r.Clean)
		assert.Equal(t, 1, r.Counts.FailedCLITests)
	})

	t.Run("m.Run non-zero explained by a failed test", func(t *testing.T) {
		in := finishInputs(t)
		in.RunCode = 1
		in.Collector.Test("TestOracle/fam/b", true)
		r, code := FinishDriftRun(in)
		assert.Equal(t, 1, code)
		assert.Equal(t, []HarnessFailure{{Name: "TestOracle/fam/b", Reason: "failed; see the go test output"}}, r.HarnessFailures)
	})

	t.Run("a failed test is a harness failure even when m.Run is zero", func(t *testing.T) {
		in := finishInputs(t)
		in.Collector.Test("TestDownstream", true)
		r, code := FinishDriftRun(in)
		assert.Equal(t, 1, code)
		assert.Equal(t, []string{"TestDownstream"}, harnessNames(r))
	})

	t.Run("a failed suite with no probe is a harness failure and not only a suite with no probe", func(t *testing.T) {
		c := &DriftCollector{}
		c.Test("TestOracle", false)
		c.Test("TestDownstream", true)
		c.Probe(SuiteOracle, "fam/a", false, nil, nil)
		in := finishInputs(t)
		in.Collector = c
		in.RunCode = 1
		r, code := FinishDriftRun(in)
		assert.Equal(t, 1, code)
		assert.Equal(t, []string{"TestDownstream"}, harnessNames(r))
		assert.Equal(t, 1, r.Counts.SuitesWithNoProbe)
	})

	t.Run("a new difference exits 1 with no harness failure", func(t *testing.T) {
		in := finishInputs(t)
		in.Collector.Probe(SuiteOracle, "fam/b", false, []Difference{diff("fam/b", "/read/status", "200", "403")}, nil)
		r, code := FinishDriftRun(in)
		assert.Equal(t, 1, code)
		assert.False(t, r.Clean)
		assert.Empty(t, r.HarnessFailures)
		assert.Equal(t, 1, r.Counts.New)
	})

	t.Run("a known difference keeps the run clean", func(t *testing.T) {
		in := finishInputs(t)
		in.Collector.Probe(SuiteOracle, "fam/b", false, []Difference{diff("fam/b", "/read/status", "200", "403")}, nil)
		in.Known = KnownFileLoad{Exists: true, File: KnownFile{Minor: "2.16", Entries: []KnownEntry{entry("fam/b", "/read/status", "200", "403")}}}
		r, code := FinishDriftRun(in)
		assert.Equal(t, 0, code)
		assert.True(t, r.Clean)
		assert.True(t, r.KnownFileExists)
		assert.Len(t, r.Known, 1)
	})

	t.Run("a missing known file is no file", func(t *testing.T) {
		r, code := FinishDriftRun(finishInputs(t))
		assert.Equal(t, 0, code)
		assert.False(t, r.KnownFileExists)
	})

	t.Run("an invalid known file is a harness failure", func(t *testing.T) {
		in := finishInputs(t)
		in.Known = KnownFileLoad{Exists: true, Err: errors.New("decoding the known-differences file: boom")}
		r, code := FinishDriftRun(in)
		assert.Equal(t, 1, code)
		require.Len(t, r.HarnessFailures, 1)
		assert.Equal(t, "known file", r.HarnessFailures[0].Name)
		assert.Contains(t, r.HarnessFailures[0].Reason, "boom")
		assert.True(t, r.KnownFileExists)
	})

	t.Run("a known file for another minor is a harness failure", func(t *testing.T) {
		in := finishInputs(t)
		in.Collector.Probe(SuiteOracle, "fam/b", false, []Difference{diff("fam/b", "/read/status", "200", "403")}, nil)
		in.Known = KnownFileLoad{Exists: true, File: KnownFile{Minor: "2.17", Entries: []KnownEntry{entry("fam/b", "/read/status", "200", "403")}}}
		r, code := FinishDriftRun(in)
		assert.Equal(t, 1, code)
		require.Len(t, r.HarnessFailures, 1)
		assert.Contains(t, r.HarnessFailures[0].Reason, "describes minor 2.17, not 2.16")
		assert.Equal(t, 1, r.Counts.New, "the entries of the wrong file are not used")
	})
}

// TestWriteReport asserts that the report is written through a temporary
// file and a rename with complete set, decodes back into the same report,
// and that a missing directory is an error that leaves no file.
func TestWriteReport(t *testing.T) {
	r, _ := FinishDriftRun(finishInputs(t))
	require.False(t, r.Complete)

	t.Run("writes a complete report in place of the old file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "report.json")
		require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
		before, err := os.Stat(path)
		require.NoError(t, err)

		require.NoError(t, WriteReport(path, r))

		after, err := os.Stat(path)
		require.NoError(t, err)
		assert.False(t, os.SameFile(before, after), "the report is renamed into place, not written over the old file")
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		var got Report
		require.NoError(t, json.Unmarshal(raw, &got))
		assert.True(t, got.Complete)
		assert.True(t, got.Clean)
		assert.Equal(t, "2.16", got.Minor)
		assert.Equal(t, "2026-10-20T19:00:00Z", got.StartedAt)
		entries, err := os.ReadDir(filepath.Dir(path))
		require.NoError(t, err)
		assert.Len(t, entries, 1, "no temporary file is left")
		assert.Equal(t, byte('\n'), raw[len(raw)-1])
	})

	t.Run("lists are arrays and never null", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "report.json")
		require.NoError(t, WriteReport(path, r))
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		var generic map[string]any
		require.NoError(t, json.Unmarshal(raw, &generic))
		for _, k := range []string{"new", "known", "knownNoRuling", "noLongerSeen", "notChecked", "notCheckedProbes", "errored", "cliTests", "suites", "harnessFailures"} {
			assert.IsTypef(t, []any{}, generic[k], "%s", k)
		}
	})

	t.Run("a missing directory is an error and leaves no file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "missing", "report.json")
		require.Error(t, WriteReport(path, r))
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Empty(t, entries)
	})
}

// TestFetchRancherVersion asserts that /rancherversion is read through the
// target's client and that an answer without Version or GitCommit, a non-200
// answer and an unreachable server are errors, never an empty field.
func TestFetchRancherVersion(t *testing.T) {
	serve := func(t *testing.T, status int, body string) *fakeTarget {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/rancherversion", r.URL.Path)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return &fakeTarget{srv: srv}
	}
	v, err := FetchRancherVersion(t.Context(), serve(t, 200, `{"Version":"v2.16-head","GitCommit":"abcdef0","RancherPrime":"false"}`))
	require.NoError(t, err)
	assert.Equal(t, RancherVersion{Version: "v2.16-head", GitCommit: "abcdef0"}, v)

	for name, c := range map[string]struct {
		tgt  *fakeTarget
		want string
	}{
		"no commit":   {serve(t, 200, `{"Version":"v2.16-head"}`), "the answer has no Version or no GitCommit"},
		"no version":  {serve(t, 200, `{"GitCommit":"abcdef0"}`), "the answer has no Version or no GitCommit"},
		"not json":    {serve(t, 200, `nope`), "decoding the answer: "},
		"status 500":  {serve(t, 500, `{"Version":"v","GitCommit":"c"}`), "answered status 500"},
		"unreachable": {&fakeTarget{srv: closedServer(t)}, "connection refused"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := FetchRancherVersion(t.Context(), c.tgt)
			require.ErrorContains(t, err, c.want)
			assert.NotContains(t, err.Error(), "fetching /rancherversion", "the harness failure names the path, not the error")
		})
	}
}

func closedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv
}
