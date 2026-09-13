package completiontest

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// Server answers fixed bodies by request path and records every request
// URI and Accept header in order.
type Server struct {
	// HTTP is the underlying test server, for a test that drives a
	// command's own client against the same fixture.
	HTTP    *httptest.Server
	mu      sync.Mutex
	uris    []string
	accepts []string
}

// New serves bodies by path; a path with no body answers 404, so a
// source reading it fails closed.
func New(tb testing.TB, bodies map[string]string) *Server {
	tb.Helper()
	return serve(tb, func(path string) (string, bool) {
		body, ok := bodies[path]
		return body, ok
	})
}

// NewByAccept serves bodies chosen by path and Accept header: answer
// returns the body and status for a request, so a test can stand in for a
// server that refuses one media type (Rancher's local cluster answers
// 406 to the aggregated discovery header) and serves another.
func NewByAccept(tb testing.TB, answer func(path, accept string) (body string, status int)) *Server {
	tb.Helper()
	s := &Server{}
	s.HTTP = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		body, status := answer(r.URL.Path, r.Header.Get("Accept"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	tb.Cleanup(s.HTTP.Close)
	return s
}

// NewFixed serves body for every path.
func NewFixed(tb testing.TB, body string) *Server {
	tb.Helper()
	return serve(tb, func(string) (string, bool) { return body, true })
}

// NewStatus answers status with an empty body for every path.
func NewStatus(tb testing.TB, status int) *Server {
	tb.Helper()
	s := &Server{}
	s.HTTP = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.WriteHeader(status)
	}))
	tb.Cleanup(s.HTTP.Close)
	return s
}

func serve(tb testing.TB, lookup func(path string) (string, bool)) *Server {
	tb.Helper()
	s := &Server{}
	s.HTTP = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		body, ok := lookup(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	tb.Cleanup(s.HTTP.Close)
	return s
}

func (s *Server) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uris = append(s.uris, r.URL.RequestURI())
	s.accepts = append(s.accepts, r.Header.Get("Accept"))
}

// Conn returns a Connection on the server with a management token and
// the given request allowance.
func (s *Server) Conn(limit int) *completion.Connection {
	return completion.NewConnection(s.HTTP.Client(), "tok", s.HTTP.URL, false, limit)
}

// Requested returns the request URIs served so far, in order.
func (s *Server) Requested() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.uris)
}

// Accepts returns the Accept header of every request served so far, in
// order.
func (s *Server) Accepts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.accepts)
}
