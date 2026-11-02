package telemetrysend

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// valuesOf counts the attributes and array items of a decoded OTLP body, as
// the receiver counts them.
func valuesOf(export decExport) int {
	n := 0
	count := func(kvs []decKeyValue) {
		for _, kv := range kvs {
			n++
			if kv.Value.ArrayValue != nil && kv.Value.ArrayValue.Values != nil {
				n += len(*kv.Value.ArrayValue.Values)
			}
		}
	}
	for _, rl := range export.ResourceLogs {
		count(rl.Resource.Attributes)
		for _, sl := range rl.ScopeLogs {
			for _, rec := range sl.LogRecords {
				count(rec.Attributes)
			}
		}
	}
	return n
}

// sentIDs returns every record id the requests held, in order.
func sentIDs(t *testing.T, requests []seen) []string {
	t.Helper()
	var out []string
	for _, r := range requests {
		out = append(out, r.ids(t)...)
	}
	return out
}

// randomText returns n characters of hex that gzip cannot shrink much.
func randomText(r *rand.Rand, n int) string {
	raw := make([]byte, n/2)
	for i := range raw {
		raw[i] = byte(r.IntN(256))
	}
	return hex.EncodeToString(raw)
}

// TestLargeFileIsSplit asserts a compacted file of 2500 events and a finished
// hour of 2500 slot files each go in three requests of at most 1000 records,
// that every file is deleted after the last 2xx, and that a list whose
// compressed body passes 1 MiB is halved until each request fits.
func TestLargeFileIsSplit(t *testing.T) {
	t.Run("a compacted file", func(t *testing.T) {
		dir := uploadDir(t)
		file := putCompactedRange(t, dir, hourAgo(3), 1, 2500)
		srv := newTestServer(t, nil)
		Send(t.Context(), input(dir, srv.URL))
		reqs := srv.seenRequests()
		require.Len(t, reqs, 3)
		for i, want := range []int{1000, 1000, 500} {
			assert.Len(t, reqs[i].flat(t), want)
		}
		assert.False(t, exists(dir, file), "deleted after the last 2xx")
	})

	t.Run("a finished hour of slots", func(t *testing.T) {
		dir := uploadDir(t)
		paths := putSlotRange(t, dir, hourAgo(3), 1, 2500)
		srv := newTestServer(t, nil)
		Send(t.Context(), input(dir, srv.URL))
		require.Len(t, srv.seenRequests(), 3)
		requireInsideLimits(t, srv, idsUpTo(2500))
		assert.Empty(t, filesWaiting(dir, paths))
	})

	t.Run("the file stays until the last request is answered", func(t *testing.T) {
		dir := uploadDir(t)
		file := putCompactedRange(t, dir, hourAgo(3), 1, 2500)
		srv := newTestServer(t, func(call int, w http.ResponseWriter, _ *http.Request) {
			if call == 2 {
				w.WriteHeader(503)
			}
		})
		Send(t.Context(), input(dir, srv.URL))
		require.Len(t, srv.seenRequests(), 3)
		assert.True(t, exists(dir, file))
	})

	t.Run("a body that passes 1 MiB compressed", func(t *testing.T) {
		dir := uploadDir(t)
		r := rand.New(rand.NewPCG(1, 2))
		hour := hourAgo(3)
		var paths []string
		for i := range 400 {
			f := eventFields(t, i+1, hour)
			f["io.rancher.cli.command"] = randomText(r, 8000)
			paths = append(paths, putSlot(t, dir, hour, i+1, f))
		}
		srv := newTestServer(t, nil)
		Send(t.Context(), input(dir, srv.URL))
		reqs := srv.seenRequests()
		require.Greater(t, len(reqs), 1, "one request would pass 1 MiB")
		for _, req := range reqs {
			assert.LessOrEqual(t, req.sent, maxSentBytes)
		}
		ids := sentIDs(t, reqs)
		assert.Len(t, ids, 400)
		assert.Len(t, slices.Compact(slices.Sorted(slices.Values(ids))), 400, "every event once")
		assert.Empty(t, filesWaiting(dir, paths))
	})
}

// idsUpTo returns the record ids of the events numbered 1 to n.
func idsUpTo(n int) []string {
	var out []string
	for i := range n {
		out = append(out, uidOf(i+1))
	}
	return out
}

// requireInsideLimits asserts that the requests the server received hold at
// most 1000 records, 250000 attributes and array items, 8 MiB decoded and 1 MiB
// as sent, and one schema version each, and that the want record ids are in
// exactly one request. It decodes each body once.
func requireInsideLimits(t *testing.T, srv *testServer, want []string) {
	t.Helper()
	reqs := srv.seenRequests()
	require.NotEmpty(t, reqs)
	var ids []string
	for i, r := range reqs {
		export := decodeExport(t, r.body)
		flat := flatEvents(t, export)
		assert.LessOrEqual(t, len(flat), 1000, "request %d records", i)
		assert.LessOrEqual(t, valuesOf(export), 250000, "request %d values", i)
		assert.LessOrEqual(t, len(r.body), 8<<20, "request %d decoded", i)
		assert.LessOrEqual(t, r.sent, 1<<20, "request %d as sent", i)
		versions := map[any]bool{}
		for _, f := range flat {
			versions[f["schema_version"]] = true
			ids = append(ids, f["log.record.uid"].(string))
		}
		assert.Len(t, versions, 1, "request %d holds one contract version", i)
	}
	assert.ElementsMatch(t, want, ids, "every event is in exactly one request")
}

// TestRequestsStayInsideTheReceiverLimits asserts, for 1000 events of 15000
// characters of repeated text (compressed under 1 MiB, over 8 MiB decoded) and
// for 1000 events with 256-item lists, that every request holds at most 1000
// records, at most 250000 attributes and array items, at most 8 MiB decoded and
// 1 MiB as sent, and one schema version, and that every event is in exactly one
// request. Events of two versions are never mixed. The 2500 valid events that
// make three full requests are the slot row of TestLargeFileIsSplit, which
// asserts the same limits.
func TestRequestsStayInsideTheReceiverLimits(t *testing.T) {
	t.Run("1000 events of repeated text", func(t *testing.T) {
		dir := uploadDir(t)
		hour := hourAgo(3)
		for i := range 1000 {
			f := eventFields(t, i+1, hour)
			f["io.rancher.cli.command"] = strings.Repeat("a", 15000)
			putSlot(t, dir, hour, i+1, f)
		}
		srv := newTestServer(t, nil)
		Send(t.Context(), input(dir, srv.URL))
		reqs := srv.seenRequests()
		require.Greater(t, len(reqs), 1, "the decoded size is over 8 MiB")
		requireInsideLimits(t, srv, idsUpTo(1000))
		assert.Less(t, reqs[0].sent, 1<<20, "the compressed size alone would allow one request")
	})

	t.Run("1000 events with 256-item lists", func(t *testing.T) {
		dir := uploadDir(t)
		hour := hourAgo(3)
		items := make([]any, 256)
		for i := range items {
			items[i] = "f"
		}
		for i := range 1000 {
			f := eventFields(t, i+1, hour)
			f["io.rancher.cli.command.flags"] = items
			putSlot(t, dir, hour, i+1, f)
		}
		srv := newTestServer(t, nil)
		Send(t.Context(), input(dir, srv.URL))
		reqs := srv.seenRequests()
		require.Greater(t, len(reqs), 1, "1000 events of 256 items pass 250000 values, and stay under 8 MiB")
		assert.Less(t, len(reqs[0].body), 8<<20, "the value count alone makes the cut")
		requireInsideLimits(t, srv, idsUpTo(1000))
	})

	t.Run("two contract versions are never mixed", func(t *testing.T) {
		dir := uploadDir(t)
		hour := hourAgo(3)
		for i := range 6 {
			f := eventFields(t, i+1, hour)
			if i%2 == 1 {
				f["schema_version"] = "1.1.0"
			}
			putSlot(t, dir, hour, i+1, f)
		}
		srv := newTestServer(t, nil)
		Send(t.Context(), input(dir, srv.URL))
		reqs := srv.seenRequests()
		require.Len(t, reqs, 2)
		assert.Equal(t, []string{uidOf(1), uidOf(3), uidOf(5)}, reqs[0].ids(t), "the group of the first event goes first")
		assert.Equal(t, []string{uidOf(2), uidOf(4), uidOf(6)}, reqs[1].ids(t))
		requireInsideLimits(t, srv, idsUpTo(6))
	})
}

// TestSplitFileResumesAfterTheCoveredIDs asserts the ids of the requests
// answered in an attempt that ran out of time are kept in the state and left
// out of the next attempt, which deletes the file after its own requests; that
// a file of 1025 events (the largest a pass writes) finishes this way; and
// that a file of 4000 events, over attempts that each send 2000 and then get a
// 503, keeps the 1280 most recent ids and the state under 64 KiB, and is sent
// whole and deleted by an attempt that finishes.
func TestSplitFileResumesAfterTheCoveredIDs(t *testing.T) {
	t.Run("1025 events", func(t *testing.T) {
		dir := uploadDir(t)
		file := putCompactedRange(t, dir, hourAgo(3), 1, 1025)
		hang := newTestServer(t, func(call int, w http.ResponseWriter, r *http.Request) {
			if call == 1 {
				testtransport.HangAndAbort(r)
			}
		})
		Send(t.Context(), shortInput(dir, hang.URL, 3*time.Second))
		reqs := hang.seenRequests()
		require.Len(t, reqs, 2)
		assert.Len(t, reqs[0].ids(t), 1000)
		st, ok := attemptState(t, dir)
		require.True(t, ok)
		assert.Equal(t, reqs[0].ids(t), st.CoveredIDs)
		assert.True(t, exists(dir, file), "the file stays: 25 events were not sent")
		assert.Equal(t, "timeout", st.NoAnswer)

		st.NextAttempt = time.Now().Add(-time.Second)
		writeState(t, dir, st)
		srv := newTestServer(t, nil)
		Send(t.Context(), input(dir, srv.URL))
		next := srv.seenRequests()
		require.Len(t, next, 1)
		var want []string
		for i := 1001; i <= 1025; i++ {
			want = append(want, uidOf(i))
		}
		assert.Equal(t, want, next[0].ids(t), "only the 25 others")
		assert.False(t, exists(dir, file))
		st, _ = attemptState(t, dir)
		assert.Empty(t, st.CoveredIDs)
	})

	t.Run("4000 events", func(t *testing.T) {
		dir := uploadDir(t)
		file := putCompactedRange(t, dir, hourAgo(3), 1, 4000)
		all := func(from, to int) []string {
			var out []string
			for i := from; i <= to; i++ {
				out = append(out, uidOf(i))
			}
			return out
		}
		partial := newTestServer(t, func(call int, w http.ResponseWriter, _ *http.Request) {
			if call%3 == 2 {
				w.WriteHeader(503)
			}
		})
		sentOverall := map[string]bool{}
		for attempt := range 3 {
			before := len(partial.seenRequests())
			Send(t.Context(), input(dir, partial.URL))
			for _, id := range sentIDs(t, partial.seenRequests()[before:]) {
				sentOverall[id] = true
			}
			st, ok := attemptState(t, dir)
			require.True(t, ok)
			assert.True(t, exists(dir, file), "attempt %d", attempt+1)
			assert.Len(t, st.CoveredIDs, maxCoveredIDs)
			switch attempt {
			case 0:
				assert.Equal(t, all(721, 2000), st.CoveredIDs, "the 1280 most recent of the 2000 sent")
			case 1:
				assert.Equal(t, all(2001, 3280), st.CoveredIDs)
			}
			info, err := os.Stat(filepath.Join(dir, "telemetry", "upload", "state.json"))
			require.NoError(t, err)
			assert.Less(t, info.Size(), int64(64*1024))
			st.NextAttempt = time.Now().Add(-time.Second)
			writeState(t, dir, st)
			require.Len(t, partial.seenRequests()[before:], 3, "two answered requests and a 503")
		}

		srv := newTestServer(t, nil)
		Send(t.Context(), input(dir, srv.URL))
		for _, id := range sentIDs(t, srv.seenRequests()) {
			sentOverall[id] = true
		}
		assert.Len(t, sentOverall, 4000, "every event is sent at least once")
		assert.False(t, exists(dir, file), "the file is deleted by the attempt that finishes")
		st, _ := attemptState(t, dir)
		assert.Empty(t, st.CoveredIDs)
	})

	t.Run("ids no longer listed are dropped", func(t *testing.T) {
		dir := uploadDir(t)
		paths := putSlotRange(t, dir, hourAgo(3), 1, 3)
		writeState(t, dir, telemetry.UploadState{
			LastAttempt: time.Now().Add(-2 * time.Hour), NextAttempt: time.Now().Add(-time.Hour),
			CoveredIDs: []string{uidOf(1), uidOf(999)},
		})
		srv := newTestServer(t, nil)
		Send(t.Context(), input(dir, srv.URL))
		reqs := srv.seenRequests()
		require.Len(t, reqs, 1)
		assert.Equal(t, []string{uidOf(2), uidOf(3)}, reqs[0].ids(t), "a covered id is left out")
		assert.Empty(t, filesWaiting(dir, paths), "its slot is deleted too: it is covered")
		st, _ := attemptState(t, dir)
		assert.Empty(t, st.CoveredIDs)
	})
}

// fakeEncoder writes a request of its own format.
type fakeEncoder struct{}

func (fakeEncoder) Encode(events []telemetry.StoredEvent) (Request, error) {
	return Request{Path: "/other/ingest", ContentType: "application/x-test", Body: []byte(fmt.Sprintf("events=%d", len(events)))}, nil
}

func (fakeEncoder) RefusedCount([]byte) (int, bool) { return 0, true }

// TestRequestNamesItsFormat asserts every request carries its encoder's
// content type, the gzip content encoding, a body that gunzips to the encoder's
// body, and goes to the origin plus the encoder's path, for a test encoder and
// the OTLP encoder.
func TestRequestNamesItsFormat(t *testing.T) {
	otlp, err := NewOTLPEncoder().Encode([]telemetry.StoredEvent{{Fields: map[string]any{}}})
	require.NoError(t, err)
	for name, tc := range map[string]struct {
		encoder     Encoder
		path, ctype string
		body        func(n int) []byte
	}{
		"a test encoder":   {fakeEncoder{}, "/other/ingest", "application/x-test", func(n int) []byte { return fmt.Appendf(nil, "events=%d", n) }},
		"the OTLP encoder": {NewOTLPEncoder(), otlp.Path, otlp.ContentType, nil},
	} {
		t.Run(name, func(t *testing.T) {
			dir := uploadDir(t)
			putSlotRange(t, dir, hourAgo(3), 1, 3)
			srv := newTestServer(t, nil)
			in := input(dir, srv.URL)
			in.Encoder = tc.encoder
			Send(t.Context(), in)
			reqs := srv.seenRequests()
			require.Len(t, reqs, 1)
			r := reqs[0]
			assert.Equal(t, http.MethodPost, r.method)
			assert.Equal(t, tc.path, r.path)
			assert.Equal(t, tc.ctype, r.header.Get("Content-Type"))
			assert.Equal(t, "gzip", r.header.Get("Content-Encoding"))
			if tc.body != nil {
				assert.True(t, bytes.Equal(tc.body(3), r.body), string(r.body))
			} else {
				assert.Len(t, r.ids(t), 3)
			}
		})
	}
}
