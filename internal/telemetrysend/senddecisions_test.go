package telemetrysend

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// answerWith answers every request with the status, the headers and the body.
func answerWith(code int, body string, headers ...string) respond {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Add(headers[i], headers[i+1])
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

// filesWaiting returns the paths of the slots and files written by a seeding
// helper that are still there.
func filesWaiting(dir string, paths []string) []string {
	var out []string
	for _, p := range paths {
		if exists(dir, p) {
			out = append(out, p)
		}
	}
	return out
}

// seedMixed writes a slot event in one hour and a compacted file in another,
// and returns every path it wrote.
func seedMixed(t *testing.T, dir string) []string {
	t.Helper()
	paths := putSlotRange(t, dir, hourAgo(3), 1, 2)
	return append(paths, putCompactedRange(t, dir, hourAgo(5), 10, 3))
}

// waitAfter returns the next attempt time of the state minus the start.
func waitAfter(t *testing.T, dir string, start time.Time) time.Duration {
	t.Helper()
	st, ok := attemptState(t, dir)
	require.True(t, ok)
	return st.NextAttempt.Sub(start)
}

func httpDate(d time.Duration, layout string) string { return time.Now().Add(d).UTC().Format(layout) }

// TestDeleteOnlyAfterSuccess asserts files are deleted only after an answer
// that covers them, for hour files and slot files alike: a 503 and a timeout
// leave every file; a 200 whose body reports refusals deletes the batch; a 429
// waits the larger of 1 hour and its Retry-After, at most 24 hours, for each
// form of the header; Retry-After counts on a 500 and not on a 2xx; three
// 503 answers in a row wait 1, 2 and 4 hours and a stored failure count of 40
// waits 24 hours; a 400 deletes and counts the events as dropped; a covered
// slot held open is waiting on Windows and deleted by the next attempt with no
// request, and covered files with nothing else to send are deleted with no
// request; a plain file or a symlink in place of telemetry/upload sends nothing
// and creates nothing; a directory in place of the lock sends nothing and
// writes the backoff state.
func TestDeleteOnlyAfterSuccess(t *testing.T) {
	t.Run("a 503 and a timeout leave every file", func(t *testing.T) {
		dir := uploadDir(t)
		paths := seedMixed(t, dir)
		srv := newTestServer(t, status(http.StatusServiceUnavailable))
		Send(t.Context(), input(dir, srv.URL))
		require.Len(t, srv.seenRequests(), 1)
		assert.Equal(t, paths, filesWaiting(dir, paths))
		st, _ := attemptState(t, dir)
		assert.Equal(t, 503, st.HTTPStatus)
		assert.Equal(t, 1, st.Failures)
		assert.Zero(t, st.Sent)

		st.NextAttempt = time.Now().Add(-time.Second)
		writeState(t, dir, st)
		clock := stopClock(t)
		in := input(dir, "")
		hang := newTestServer(t, func(_ int, _ http.ResponseWriter, r *http.Request) {
			clock.pass(in)
			testtransport.HangAndAbort(r)
		})
		in.Origin = hang.URL
		Send(clock.context(), in)
		require.Len(t, hang.seenRequests(), 1)
		assert.Equal(t, paths, filesWaiting(dir, paths))
		st, _ = attemptState(t, dir)
		assert.Equal(t, "timeout", st.NoAnswer)
		assert.Zero(t, st.HTTPStatus)
		assert.Equal(t, 2, st.Failures)
	})

	t.Run("a 200 that reports refusals deletes the batch", func(t *testing.T) {
		dir := uploadDir(t)
		paths := seedMixed(t, dir)
		srv := newTestServer(t, answerWith(200, `{"partialSuccess":{"rejectedLogRecords":"2"}}`))
		Send(t.Context(), input(dir, srv.URL))
		assert.Empty(t, filesWaiting(dir, paths))
		st, _ := attemptState(t, dir)
		assert.Equal(t, 5, st.Sent)
		require.NotNil(t, st.Refused)
		assert.Equal(t, 2, *st.Refused)
		assert.Equal(t, 0, st.Failures)
	})

	// retryAfterAfter sends one attempt after the given number of failures in a
	// row and returns the wait the state records.
	retryAfterAfter := func(failures, code int, headers ...string) time.Duration {
		dir := uploadDir(t)
		seedMixed(t, dir)
		if failures > 0 {
			writeState(t, dir, telemetry.UploadState{LastAttempt: time.Now().Add(-48 * time.Hour), Failures: failures, NextAttempt: time.Now().Add(-time.Hour)})
		}
		srv := newTestServer(t, answerWith(code, "", headers...))
		start := time.Now()
		Send(t.Context(), input(dir, srv.URL))
		return waitAfter(t, dir, start)
	}
	retryAfter := func(code int, headers ...string) time.Duration { return retryAfterAfter(0, code, headers...) }
	near := func(t *testing.T, want, got time.Duration, name string) {
		t.Helper()
		assert.InDelta(t, float64(want), float64(got), float64(3*time.Second), name)
	}

	t.Run("Retry-After on a 429", func(t *testing.T) {
		for name, tc := range map[string]struct {
			value string
			want  time.Duration
		}{
			"7200 seconds":          {"7200", 2 * time.Hour},
			"60 seconds":            {"60", time.Hour},
			"0 seconds":             {"0", time.Hour},
			"two days":              {"172800", 24 * time.Hour},
			"larger than an int":    {"99999999999999999999", 24 * time.Hour},
			"an IMF date":           {httpDate(2*time.Hour, http.TimeFormat), 2 * time.Hour},
			"an RFC 850 date":       {httpDate(2*time.Hour, time.RFC850), 2 * time.Hour},
			"an ANSI C date":        {httpDate(2*time.Hour, time.ANSIC), 2 * time.Hour},
			"a date in the past":    {httpDate(-3*time.Hour, http.TimeFormat), time.Hour},
			"a date two days ahead": {httpDate(48*time.Hour, http.TimeFormat), 24 * time.Hour},
		} {
			near(t, tc.want, retryAfter(429, "Retry-After", tc.value), name)
		}
		near(t, 2*time.Hour, retryAfter(429, "Retry-After", "7200", "Retry-After", "60"), "two headers: the first")
	})

	t.Run("a Retry-After that is ignored leaves the doubling", func(t *testing.T) {
		// After two failures the doubling gives 4 hours. A value read as
		// 0 seconds would give 1 hour, so only an ignored value shows 4.
		for name, value := range map[string]string{"negative": "-5", "a fraction": "1.5", "text": "abc", "empty": ""} {
			near(t, 4*time.Hour, retryAfterAfter(2, 429, "Retry-After", value), name)
		}
		near(t, 4*time.Hour, retryAfterAfter(2, 429), "no header")
		near(t, time.Hour, retryAfterAfter(2, 429, "Retry-After", "0"), "0 seconds is a value: the 1 hour floor")
	})

	t.Run("Retry-After on a 500 applies and on a 2xx is ignored", func(t *testing.T) {
		near(t, 2*time.Hour, retryAfter(500, "Retry-After", "7200"), "500")
		got := retryAfter(200, "Retry-After", "7200")
		assert.GreaterOrEqual(t, got, time.Hour)
		assert.Less(t, got, time.Hour+maxJitter+3*time.Second, "a 2xx ignores it")
	})

	t.Run("a 400 deletes and counts the events as dropped", func(t *testing.T) {
		dir := uploadDir(t)
		paths := seedMixed(t, dir)
		srv := newTestServer(t, status(400))
		Send(t.Context(), input(dir, srv.URL))
		assert.Empty(t, filesWaiting(dir, paths))
		st, _ := attemptState(t, dir)
		assert.Equal(t, 5, st.Dropped)
		assert.Zero(t, st.Sent)
		assert.Equal(t, 400, st.HTTPStatus)
		assert.Equal(t, 0, st.Failures)
	})

	t.Run("three 503 answers in a row wait 1, 2 and 4 hours", func(t *testing.T) {
		dir := uploadDir(t)
		seedMixed(t, dir)
		srv := newTestServer(t, status(503))
		for i, want := range []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour} {
			start := time.Now()
			Send(t.Context(), input(dir, srv.URL))
			near(t, want, waitAfter(t, dir, start), "attempt "+strconv.Itoa(i+1))
			st, _ := attemptState(t, dir)
			assert.Equal(t, i+1, st.Failures)
			st.NextAttempt = time.Now().Add(-time.Second)
			writeState(t, dir, st)
		}
		assert.Len(t, srv.seenRequests(), 3)
	})

	t.Run("a stored failure count of 40 waits 24 hours", func(t *testing.T) {
		dir := uploadDir(t)
		seedMixed(t, dir)
		writeState(t, dir, telemetry.UploadState{LastAttempt: time.Now().Add(-48 * time.Hour), Failures: 40, NextAttempt: time.Now().Add(-time.Hour)})
		srv := newTestServer(t, status(503))
		start := time.Now()
		Send(t.Context(), input(dir, srv.URL))
		near(t, 24*time.Hour, waitAfter(t, dir, start), "failures 40")
		st, _ := attemptState(t, dir)
		assert.Equal(t, 41, st.Failures)

		writeState(t, dir, telemetry.UploadState{Failures: 1 << 62, NextAttempt: time.Now().Add(-time.Hour)})
		start = time.Now()
		Send(t.Context(), input(dir, srv.URL))
		near(t, 24*time.Hour, waitAfter(t, dir, start), "a huge count")
	})

	t.Run("a covered slot held open", func(t *testing.T) {
		dir := uploadDir(t)
		paths := putSlotRange(t, dir, hourAgo(3), 1, 2)
		held, err := os.Open(filepath.Join(dir, "telemetry", filepath.FromSlash(paths[0])))
		require.NoError(t, err)
		closed := false
		t.Cleanup(func() {
			if !closed {
				_ = held.Close()
			}
		})
		srv := newTestServer(t, nil)
		Send(t.Context(), input(dir, srv.URL))
		require.Len(t, srv.seenRequests(), 1)
		if testfs.OnWindows() {
			assert.Equal(t, paths[:1], filesWaiting(dir, paths), "an open file cannot be removed")
			st, _ := attemptState(t, dir)
			assert.Equal(t, []string{uidOf(1)}, st.CoveredIDs)
			require.NoError(t, held.Close())
			closed = true
			st.NextAttempt = time.Now().Add(-time.Second)
			writeState(t, dir, st)
			Send(t.Context(), input(dir, srv.URL))
			assert.Len(t, srv.seenRequests(), 1, "the next attempt deletes it with no request")
		}
		assert.Empty(t, filesWaiting(dir, paths))
		st, _ := attemptState(t, dir)
		assert.Empty(t, st.CoveredIDs)
	})

	t.Run("covered files with nothing else to send are deleted with no request", func(t *testing.T) {
		dir := uploadDir(t)
		paths := putSlotRange(t, dir, hourAgo(3), 1, 2)
		writeState(t, dir, telemetry.UploadState{
			LastAttempt: time.Now().Add(-2 * time.Hour), NextAttempt: time.Now().Add(-time.Hour),
			HTTPStatus: 503, Failures: 3, CoveredIDs: []string{uidOf(1), uidOf(2)},
		})
		srv := newTestServer(t, nil)
		start := time.Now()
		Send(t.Context(), input(dir, srv.URL))
		assert.Empty(t, srv.seenRequests())
		assert.Empty(t, filesWaiting(dir, paths))
		st, ok := attemptState(t, dir)
		require.True(t, ok)
		assert.Empty(t, st.CoveredIDs)
		assert.Equal(t, 503, st.HTTPStatus, "the last answer stays what status shows")
		assert.Zero(t, st.Failures)
		wait := waitAfter(t, dir, start)
		assert.GreaterOrEqual(t, wait, time.Hour, "the next attempt")
		assert.Less(t, wait, time.Hour+maxJitter+3*time.Second)
	})

	t.Run("a plain file or a symlink in place of telemetry upload", func(t *testing.T) {
		for name, place := range map[string]func(t *testing.T, upload string){
			"a plain file": func(t *testing.T, upload string) {
				t.Helper()
				require.NoError(t, os.WriteFile(upload, []byte("x"), 0o600))
			},
			"a symlink": func(t *testing.T, upload string) {
				t.Helper()
				target := filepath.Join(filepath.Dir(upload), "elsewhere")
				require.NoError(t, os.MkdirAll(target, 0o700))
				testfs.Symlink(t, target, upload)
			},
		} {
			t.Run(name, func(t *testing.T) {
				dir := uploadDir(t)
				seedMixed(t, dir)
				place(t, filepath.Join(dir, "telemetry", "upload"))
				srv := newTestServer(t, nil)
				before := testfs.Snapshot(t, dir)
				Send(t.Context(), input(dir, srv.URL))
				assert.Empty(t, srv.seenRequests())
				assert.Equal(t, before, testfs.Snapshot(t, dir))
			})
		}
	})

	t.Run("a directory in place of the lock", func(t *testing.T) {
		dir := uploadDir(t)
		paths := seedMixed(t, dir)
		testfs.DirectoryInPlaceOfFile(t, filepath.Join(dir, "telemetry", "upload", "lock"))
		srv := newTestServer(t, nil)
		start := time.Now()
		Send(t.Context(), input(dir, srv.URL))
		assert.Empty(t, srv.seenRequests())
		assert.Equal(t, paths, filesWaiting(dir, paths))
		st, ok := attemptState(t, dir)
		require.True(t, ok)
		assert.Equal(t, "lock unavailable", st.NoAnswer)
		assert.Equal(t, 1, st.Failures)
		assert.Zero(t, st.HTTPStatus)
		near(t, time.Hour, waitAfter(t, dir, start), "the backoff")
	})
}

// shortBody answers with a Content-Length larger than the bytes it sends and
// closes the connection.
func shortBody(code int) respond {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 " + strconv.Itoa(code) + " X\r\nContent-Length: 100\r\n\r\nabcde")
		_ = buf.Flush()
		_ = conn.Close()
	}
}

// slowBody sends the status and part of the body, calls stall, then holds the
// request open.
func slowBody(code int, stall func()) respond {
	return func(_ int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte("abc"))
		w.(http.Flusher).Flush()
		stall()
		testtransport.HangAndAbort(r)
	}
}

// TestUploadDecisionsReadOnlyTheStatus asserts the decision for each status
// class does not depend on the body: for each of three bodies, a 2xx deletes,
// a 400, 413 and 415 delete and count dropped, every other status keeps the
// files; a redirect to the same origin or to another one is not followed and is
// no answer; a body that cannot be read, from a short length or a stall until
// the deadline, is no answer and keeps the files (a 204 has no body in the HTTP
// client whatever its length says: one that announces a body and closes early
// is still an answer and deletes); the refused count is null for an empty body
// and one that does not parse. Every row runs through the automatic attempt
// and through the command entry.
func TestUploadDecisionsReadOnlyTheStatus(t *testing.T) {
	entries := map[string]func(ctx context.Context, in Input){
		"the automatic attempt": Send,
		"the command":           func(ctx context.Context, in Input) { SendNow(ctx, in) },
	}
	for name, send := range entries {
		t.Run(name, func(t *testing.T) { uploadDecisionRows(t, send) })
	}
}

func uploadDecisionRows(t *testing.T, send func(ctx context.Context, in Input)) {
	t.Helper()
	bodies := map[string]string{
		"empty":       "",
		"refusals":    `{"partialSuccess":{"rejectedLogRecords":"1"}}`,
		"not parsing": "<html>not json",
	}
	type outcome int
	const (
		accepted outcome = iota
		dropped
		kept
	)
	rows := map[int]outcome{
		200: accepted, 202: accepted, 204: accepted,
		400: dropped, 413: dropped, 415: dropped,
		500: kept, 503: kept, 401: kept, 403: kept, 404: kept, 405: kept, 407: kept, 408: kept, 421: kept, 425: kept, 429: kept,
	}
	for code, want := range rows {
		for name, body := range bodies {
			t.Run(strconv.Itoa(code)+" "+name, func(t *testing.T) {
				dir := uploadDir(t)
				paths := seedMixed(t, dir)
				srv := newTestServer(t, answerWith(code, body))
				send(t.Context(), input(dir, srv.URL))
				require.Len(t, srv.seenRequests(), 1)
				st, ok := attemptState(t, dir)
				require.True(t, ok)
				assert.Equal(t, code, st.HTTPStatus)
				switch want {
				case accepted:
					assert.Empty(t, filesWaiting(dir, paths))
					assert.Equal(t, 5, st.Sent)
					assert.Zero(t, st.Dropped)
					assert.Zero(t, st.Failures)
					if name == "refusals" && code != 204 {
						require.NotNil(t, st.Refused)
						assert.Equal(t, 1, *st.Refused)
					} else if name != "refusals" {
						assert.Nil(t, st.Refused, "no count for an empty body or one that does not parse")
					}
				case dropped:
					assert.Empty(t, filesWaiting(dir, paths))
					assert.Equal(t, 5, st.Dropped)
					assert.Zero(t, st.Sent)
					assert.Zero(t, st.Failures)
				case kept:
					assert.Equal(t, paths, filesWaiting(dir, paths))
					assert.Equal(t, 1, st.Failures)
					assert.Zero(t, st.Sent)
					assert.Zero(t, st.Dropped)
				}
			})
		}
	}

	t.Run("redirects are never followed", func(t *testing.T) {
		other := newTestServer(t, nil)
		for name, target := range map[string]func(self string) (code int, location string){
			"301 to the same origin": func(self string) (int, string) { return 301, self + "/other" },
			"302 to another origin":  func(string) (int, string) { return 302, other.URL + "/v1/logs" },
			"303 to the same origin": func(self string) (int, string) { return 303, self + "/v1/logs" },
			"307 to the same origin": func(self string) (int, string) { return 307, self + "/other" },
			"308 to the same origin": func(self string) (int, string) { return 308, self + "/other" },
		} {
			t.Run(name, func(t *testing.T) {
				dir := uploadDir(t)
				paths := seedMixed(t, dir)
				var self string
				srv := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
					code, location := target(self)
					w.Header().Set("Location", location)
					w.WriteHeader(code)
				})
				self = srv.URL
				before := len(other.seenRequests())
				send(t.Context(), input(dir, srv.URL))
				assert.Len(t, srv.seenRequests(), 1, "no second request")
				assert.Len(t, other.seenRequests(), before)
				assert.Equal(t, paths, filesWaiting(dir, paths))
				st, _ := attemptState(t, dir)
				assert.Zero(t, st.HTTPStatus)
				assert.Equal(t, "refused redirect", st.NoAnswer)
				assert.Equal(t, 1, st.Failures)
			})
		}
	})

	t.Run("a 204 that announces a body is an answer", func(t *testing.T) {
		// The HTTP client never reads the body of a 204, whatever its length
		// says, so the answer stands and the files are deleted.
		dir := uploadDir(t)
		paths := seedMixed(t, dir)
		srv := newTestServer(t, shortBody(204))
		send(t.Context(), input(dir, srv.URL))
		require.Len(t, srv.seenRequests(), 1)
		st, ok := attemptState(t, dir)
		require.True(t, ok)
		assert.Equal(t, 204, st.HTTPStatus)
		assert.Empty(t, st.NoAnswer)
		assert.Equal(t, 5, st.Sent)
		assert.Empty(t, filesWaiting(dir, paths))
	})

	t.Run("an answer that cannot be read is no answer", func(t *testing.T) {
		short := func(code int, _ func()) respond { return shortBody(code) }
		for name, tc := range map[string]struct {
			answer  func(code int, stall func()) respond
			code    int
			noReply string
		}{
			"a 200 with a short body": {short, 200, "network error"},
			"a 400 with a short body": {short, 400, "network error"},
			"a 200 that stalls":       {slowBody, 200, "timeout"},
			"a 400 that stalls":       {slowBody, 400, "timeout"},
		} {
			t.Run(name, func(t *testing.T) {
				dir := uploadDir(t)
				paths := seedMixed(t, dir)
				clock := stopClock(t)
				in := input(dir, "")
				srv := newTestServer(t, tc.answer(tc.code, func() { clock.pass(in) }))
				in.Origin = srv.URL
				send(clock.context(), in)
				require.Len(t, srv.seenRequests(), 1)
				st, ok := attemptState(t, dir)
				require.True(t, ok)
				assert.Equal(t, paths, filesWaiting(dir, paths))
				assert.Equal(t, tc.noReply, st.NoAnswer)
				assert.Zero(t, st.HTTPStatus)
				assert.Equal(t, 1, st.Failures)
			})
		}
	})
}
