package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// uploadEnv makes the environment of a run of the built tree that records
// events: the hermetic environment of experimentEnv, recording turned on, the
// telemetry clock frozen, and the state the process keeps reset when the test
// ends. The frozen clock keeps the 150 ms read limit of the hourly pass from
// running out on a slow runner, which would leave an hour the pass meant to
// remove. A test that needs a due pass moves the marker file's modification
// time back, which stays real. It returns the config directory. Not for a
// parallel test.
func uploadEnv(t *testing.T) (dir string) {
	t.Helper()
	experimentEnv(t)
	t.Cleanup(telemetry.SetEnabledForTest())
	t.Cleanup(telemetry.SetFixedClockForTest())
	t.Setenv(telemetry.EnvTelemetry, "")
	t.Cleanup(telemetry.ResetServerForTest)
	t.Cleanup(resetRecordingState)
	return os.Getenv(config.EnvConfigDir)
}

// exampleEvent returns the committed full example of a command run event.
func exampleEvent(t *testing.T) map[string]any {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Join(filepath.Dir(file), "..", "..", "telemetry", "contract", telemetry.ContractVersion, "examples", "valid")
	data, err := os.ReadFile(filepath.Join(root, "command-run-full.json"))
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(data, &fields))
	return fields
}

func recordUID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

func hourStart(hoursAgo int) time.Time {
	return time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(hoursAgo) * time.Hour)
}

func hourName(h time.Time) string { return h.Format("2006010215") }

// seedEvent writes one event as slot number slot of the hour that began
// hoursAgo hours before the current one, and returns its path.
func seedEvent(t *testing.T, dir string, hoursAgo, slot int, fields map[string]any) string {
	t.Helper()
	hour := hourStart(hoursAgo)
	path := filepath.Join(dir, "telemetry", "events", hourName(hour), fmt.Sprintf("%04d.json", slot))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	b, err := json.Marshal(fields)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0o600))
	marker := filepath.Join(dir, "telemetry", "pruned")
	if _, err := os.Stat(marker); err != nil {
		require.NoError(t, os.WriteFile(marker, nil, 0o600))
	}
	return path
}

// seedFinished writes n events numbered from first into the slots of one
// finished hour and returns their paths.
func seedFinished(t *testing.T, dir string, hoursAgo, first, n int) []string {
	t.Helper()
	var paths []string
	for i := range n {
		f := exampleEvent(t)
		f["log.record.uid"] = recordUID(first + i)
		f["time"] = hourStart(hoursAgo).Format(time.RFC3339)
		paths = append(paths, seedEvent(t, dir, hoursAgo, i+1, f))
	}
	return paths
}

func anyExists(paths []string) bool {
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			return true
		}
	}
	return false
}

func uploadFilesOf(t *testing.T, dir string) []string {
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
	return out
}

// hit is one request a test server received.
type hit struct {
	method, path string
	header       http.Header
	body         []byte
}

// ids returns the record ids of the events in an OTLP body.
func (h hit) ids(t *testing.T) []string {
	t.Helper()
	var doc struct {
		ResourceLogs []struct {
			ScopeLogs []struct {
				LogRecords []struct {
					Attributes []struct {
						Key   string `json:"key"`
						Value struct {
							StringValue string `json:"stringValue"`
						} `json:"value"`
					} `json:"attributes"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	require.NoError(t, json.Unmarshal(h.body, &doc))
	var out []string
	for _, rl := range doc.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, rec := range sl.LogRecords {
				for _, a := range rec.Attributes {
					if a.Key == "log.record.uid" {
						out = append(out, a.Value.StringValue)
					}
				}
			}
		}
	}
	return out
}

// uploadServer is a local server that records every request and answers
// through respond, by default with a 200 and an empty JSON object.
type uploadServer struct {
	*httptest.Server
	mu   sync.Mutex
	hits []hit
}

type uploadAnswer func(call int, w http.ResponseWriter, r *http.Request)

func newUploadServer(t *testing.T, answer uploadAnswer) *uploadServer {
	t.Helper()
	return serveUpload(t, "127.0.0.1:0", answer)
}

// serveUpload listens on addr and skips the test when the address is not
// available.
func serveUpload(t *testing.T, addr string, answer uploadAnswer) *uploadServer {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	s := &uploadServer{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := raw
		if r.Header.Get("Content-Encoding") == "gzip" {
			if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
				body, _ = io.ReadAll(zr)
			}
		}
		s.mu.Lock()
		call := len(s.hits)
		s.hits = append(s.hits, hit{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
		s.mu.Unlock()
		if answer == nil {
			_, _ = w.Write([]byte("{}"))
			return
		}
		answer(call, w, r)
	}))
	s.Listener = l
	s.Start()
	t.Cleanup(s.Close)
	return s
}

func (s *uploadServer) requests() []hit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]hit(nil), s.hits...)
}

func (s *uploadServer) count() int { return len(s.requests()) }

func replyStatus(code int) uploadAnswer {
	return func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

// withEndpoint sets the experiment variable to an upload endpoint.
func withEndpoint(t *testing.T, url string) {
	t.Helper()
	setExperiment(t, "telemetryupload="+url, true)
}
