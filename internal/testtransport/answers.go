package testtransport

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// UnavailableStatuses are the statuses a server answers when it, or a proxy
// in front of it, cannot serve a request: the ones a command must report at
// once and never wait on.
var UnavailableStatuses = []int{500, 502, 503, 504, 520, 527, 530}

// Answers is a test server that gives every request the same answer and
// counts the requests it received. A server that is down for a while
// answers this way, and the count tells a client that repeated its request
// from one that did not.
type Answers struct {
	srv   *httptest.Server
	count atomic.Int32

	mu    sync.Mutex
	paths []string
}

// NewAnswers starts a server (TLS when useTLS is true) that answers every
// request with status, the Retry-After header when retryAfter is not empty,
// the Content-Type header when contentType is not empty, and body. It reads
// and discards each request body, and never fails the test from the handler
// goroutine. The server closes when the test ends.
func NewAnswers(tb testing.TB, useTLS bool, status int, retryAfter, contentType, body string) *Answers {
	tb.Helper()
	a := &Answers{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.count.Add(1)
		a.mu.Lock()
		a.paths = append(a.paths, r.URL.Path)
		a.mu.Unlock()
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		if _, err := io.WriteString(w, body); err != nil {
			return
		}
	})
	if useTLS {
		a.srv = httptest.NewTLSServer(h)
	} else {
		a.srv = httptest.NewServer(h)
	}
	tb.Cleanup(a.srv.Close)
	return a
}

// Server is the underlying test server, for a caller that needs its
// certificate or address in the form httptest gives them.
func (a *Answers) Server() *httptest.Server { return a.srv }

// URL is the server's address.
func (a *Answers) URL() string { return a.srv.URL }

// CertPEM is the server's certificate in PEM, for a config whose CA is
// the server's own; empty for a server without TLS.
func (a *Answers) CertPEM() string {
	if a.srv.Certificate() == nil {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.srv.Certificate().Raw}))
}

// Client is the server's own HTTP client, which trusts its certificate.
func (a *Answers) Client() *http.Client { return a.srv.Client() }

// Count is the number of requests the server has received.
func (a *Answers) Count() int { return int(a.count.Load()) }

// Paths are the request paths the server has received, in arrival order.
func (a *Answers) Paths() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.paths...)
}
