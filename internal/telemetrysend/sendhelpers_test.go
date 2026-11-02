package telemetrysend

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/telemetry"
)

// uidOf returns a lowercase version 4 UUID unique to n.
func uidOf(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// hourAgo is the start of the UTC hour n hours before the current one.
func hourAgo(n int) time.Time {
	return time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(n) * time.Hour)
}

func hourDir(h time.Time) string { return h.UTC().Format("2006010215") }

// eventFields returns the fields of a valid stored event with record id n,
// from the committed full example, with the event time set to the hour.
func eventFields(t *testing.T, n int, hour time.Time) map[string]any {
	t.Helper()
	fields := maps.Clone(flatExamples(t, "valid")["command-run-full.json"])
	fields["log.record.uid"] = uidOf(n)
	fields["time"] = hour.UTC().Format(time.RFC3339)
	return fields
}

func eventLine(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(fields)
	require.NoError(t, err)
	return b
}

// uploadDir returns a configuration directory with an empty telemetry
// directory.
func uploadDir(t *testing.T) string {
	t.Helper()
	t.Cleanup(telemetry.SetEnabledForTest())
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "telemetry"), 0o700))
	return dir
}

// putSlot writes the event as slot number slot of the hour and returns its
// slash path below the telemetry directory.
func putSlot(t *testing.T, dir string, hour time.Time, slot int, fields map[string]any) string {
	t.Helper()
	rel := "events/" + hourDir(hour) + "/" + fmt.Sprintf("%04d.json", slot)
	file := filepath.Join(dir, "telemetry", filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
	require.NoError(t, os.WriteFile(file, eventLine(t, fields), 0o600))
	return rel
}

// putSlotRange writes events numbered first to first+n-1 as the slots 1 to n of
// the hour and returns their paths.
func putSlotRange(t *testing.T, dir string, hour time.Time, first, n int) []string {
	t.Helper()
	var out []string
	for i := range n {
		out = append(out, putSlot(t, dir, hour, i+1, eventFields(t, first+i, hour)))
	}
	return out
}

// putCompactedFile writes the events one per line as the file name of the hour,
// ages it to mtime and returns its path below the telemetry directory.
func putCompactedFile(t *testing.T, dir string, hour time.Time, name string, mtime time.Time, events ...map[string]any) string {
	t.Helper()
	var buf bytes.Buffer
	for _, ev := range events {
		buf.Write(eventLine(t, ev))
		buf.WriteByte('\n')
	}
	rel := "events/" + hourDir(hour) + "/" + name
	file := filepath.Join(dir, "telemetry", filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
	require.NoError(t, os.WriteFile(file, buf.Bytes(), 0o600))
	require.NoError(t, os.Chtimes(file, mtime, mtime))
	return rel
}

// putCompactedRange writes a compacted file of events first to first+n-1 that
// is two minutes old.
func putCompactedRange(t *testing.T, dir string, hour time.Time, first, n int) string {
	t.Helper()
	events := make([]map[string]any, n)
	for i := range n {
		events[i] = eventFields(t, first+i, hour)
	}
	return putCompactedFile(t, dir, hour, hourDir(hour)+".jsonl", time.Now().Add(-2*time.Minute), events...)
}

func exists(dir, rel string) bool {
	_, err := os.Lstat(filepath.Join(dir, "telemetry", filepath.FromSlash(rel)))
	return err == nil
}

// seen is one request a test server received.
type seen struct {
	method, path string
	header       http.Header
	// sent is the size of the body as it came; body is the body after gunzip.
	sent int
	body []byte
}

// flat returns the flat events of an OTLP body.
func (s seen) flat(t *testing.T) []map[string]any {
	t.Helper()
	return rebuildFlat(t, s.body)
}

// ids returns the record ids of an OTLP body in order.
func (s seen) ids(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, f := range s.flat(t) {
		out = append(out, f["log.record.uid"].(string))
	}
	return out
}

// testServer is a local server that records every request and answers through
// respond, which defaults to a 200 with an empty JSON object.
type testServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []seen
}

// respond answers the call-th request (from 0) after the request was
// recorded.
type respond func(call int, w http.ResponseWriter, r *http.Request)

func newTestServer(t *testing.T, answer respond) *testServer {
	t.Helper()
	s := &testServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := raw
		if r.Header.Get("Content-Encoding") == "gzip" {
			if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
				body, _ = io.ReadAll(zr)
			}
		}
		s.mu.Lock()
		call := len(s.requests)
		s.requests = append(s.requests, seen{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), sent: len(raw), body: body})
		s.mu.Unlock()
		if answer == nil {
			_, _ = w.Write([]byte("{}"))
			return
		}
		answer(call, w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// seenRequests returns a copy of the requests received so far.
func (s *testServer) seenRequests() []seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

func status(code int) respond {
	return func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

// input returns the input of an attempt against the server with a deadline
// that leaves plenty of time.
func input(dir, origin string) Input {
	return Input{
		ConfigDir:   dir,
		Version:     "dev",
		Origin:      origin,
		Encoder:     NewOTLPEncoder(),
		Deadline:    time.Now().Add(20 * time.Second),
		Floor:       time.Second,
		RecordingOn: func() bool { return true },
	}
}

// attemptState reads the state file of dir.
func attemptState(t *testing.T, dir string) (telemetry.UploadState, bool) {
	t.Helper()
	s, err := telemetry.OpenStore(dir)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	return s.ReadUploadState()
}

// readyIDs returns the record ids of the events a finished-hours listing of dir
// holds now.
func readyIDs(t *testing.T, dir string) []string {
	t.Helper()
	s, err := telemetry.OpenStore(dir)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	list, err := s.Ready(telemetry.ReadyOptions{})
	require.NoError(t, err)
	var out []string
	for _, ev := range list.Events {
		out = append(out, ev.UID)
	}
	return out
}

// uploadFiles lists the names below telemetry/upload.
func uploadFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "telemetry", "upload"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}

// writeState takes the upload lock of dir, writes the state and releases the
// lock, as an earlier attempt would have.
func writeState(t *testing.T, dir string, st telemetry.UploadState) {
	t.Helper()
	s, err := telemetry.OpenStore(dir)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	lock, err := s.TakeUploadLock()
	require.NoError(t, err)
	require.NoError(t, lock.WriteState(st))
	require.NoError(t, lock.Release())
}
