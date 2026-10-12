// Package testtransport is test support that keeps a test binary off
// http.DefaultTransport. Tests only; no production package imports it.
//
// httptest.Server.Close closes the idle connections of
// http.DefaultTransport, so a request that goes through it fails with
// "http: CloseIdleConnections called" whenever another test closes its
// server at the wrong moment. A test must send its requests through the
// server's own client (srv.Client()), or give a client-go rest.Config the
// server's Transport. A test binary that starts servers calls
// GuardDefaultTransport from its TestMain so a request that still reaches
// the default transport fails at once, on every run, instead of on a rare
// one.
package testtransport

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// guard is the stand-in for http.DefaultTransport. It must stay a
// comparable type: client-go compares a config's transport with
// http.DefaultTransport, and a func type or a struct holding a slice or
// map panics in that comparison.
type guard struct {
	mu   sync.Mutex
	urls []string
}

// RoundTrip closes the request body, as a transport must when it fails a
// request, records the request and fails it. Requests come from parallel
// tests, so the record is under a mutex. http.Client adds the URL to the
// error.
func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	g.mu.Lock()
	g.urls = append(g.urls, req.URL.String())
	g.mu.Unlock()
	return nil, errors.New("this test reached a server through http.DefaultTransport; send it through the server's own client (srv.Client()) or give the rest.Config a Transport")
}

// recorded returns the URLs the guard has failed, in arrival order.
func (g *guard) recorded() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.urls)
}

// listedURLs is how many URLs the error from restore lists.
const listedURLs = 20

// GuardDefaultTransport replaces http.DefaultTransport with a transport
// that fails every request and records its URL. Call it from TestMain
// before m.Run, never from an init function or a package variable:
// client-go and apimachinery read http.DefaultTransport during package
// initialization.
//
// The returned restore puts the original transport back and returns an
// error with the number of failed requests and the first 20 of their
// URLs, so a test that accepted the guard's error (it asserted only that
// some error happened) still fails the test binary. Call restore after
// m.Run and exit non-zero when it returns an error.
func GuardDefaultTransport() (restore func() error) {
	original := http.DefaultTransport
	g := &guard{}
	http.DefaultTransport = g
	return func() error {
		http.DefaultTransport = original
		urls := g.recorded()
		if len(urls) == 0 {
			return nil
		}
		total, more := len(urls), ""
		if total > listedURLs {
			more = fmt.Sprintf(" and %d more", total-listedURLs)
			urls = urls[:listedURLs]
		}
		return fmt.Errorf("http.DefaultTransport was used by %d request(s): %s%s; send each through the server's own client (srv.Client()) or give the rest.Config a Transport", total, strings.Join(urls, ", "), more)
	}
}
