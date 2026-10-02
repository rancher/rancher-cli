package testrancher

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MadeFrom is a Rancher that makes a cluster token from whatever login
// credential the request bears, and names the token after that
// credential, so a test can tell which login a cached token was made
// from; the token's owner is user-<credential>. It serves nothing but the
// ext token create.
type MadeFrom struct {
	URL string

	srv     *httptest.Server
	creates atomic.Int32

	mu sync.Mutex
	// during runs inside the next create request, before the answer: the
	// exec plugin has loaded its config and read its credential by then,
	// and has not written its cache record yet.
	during func()
}

// NewMadeFrom starts a MadeFrom; the caller closes it.
func NewMadeFrom() *MadeFrom {
	m := &MadeFrom{}
	expiresAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != extTokensPath || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		m.creates.Add(1)
		m.mu.Lock()
		d := m.during
		m.during = nil
		m.mu.Unlock()
		if d != nil {
			d()
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"metadata":{"name":"from-%s"},"spec":{"userID":%q},"status":{"bearerToken":%q,"expiresAt":%q}}`, bearer, "user-"+bearer, MadeFromToken(bearer), expiresAt)
	}))
	m.URL = m.srv.URL
	return m
}

// Close stops the server.
func (m *MadeFrom) Close() { m.srv.Close() }

// Client is an HTTP client that reaches the server.
func (m *MadeFrom) Client() *http.Client { return m.srv.Client() }

// Creates is the number of create requests the server has had.
func (m *MadeFrom) Creates() int { return int(m.creates.Load()) }

// During sets what the next create request runs before it answers.
func (m *MadeFrom) During(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.during = fn
}

// MadeFromToken is the cluster token MadeFrom makes from a login
// credential.
func MadeFromToken(credential string) string { return "ext/from-" + credential + ":secret" }
