package httpx

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetRecords clears both records now and when the test ends. A test that
// uses the records must not call t.Parallel(): they are process-wide.
func resetRecords(t *testing.T) {
	t.Helper()
	ClearRecords()
	t.Cleanup(ClearRecords)
}

// tlsServer starts a TLS test server and returns it with its certificate as
// a PEM for the client's CA.
func tlsServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pemCA := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	return srv, pemCA
}

func originOfURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u.Scheme + "://" + u.Host
}

// stubTransport is a transport that identifies itself.
type stubTransport struct{ name string }

func (s *stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("stub %s sends nothing", s.name)
}

// TestOnlyARequestThatGotAResponseRecordsItsHost asserts a client built by
// New records the scheme and host of a request that came back with a response
// whatever its status, with the transport below the User-Agent transport, and
// records nothing for a refused port or a failed TLS handshake.
func TestOnlyARequestThatGotAResponseRecordsItsHost(t *testing.T) {
	for _, code := range []int{200, 302, 401, 404, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			resetRecords(t)
			srv, pemCA := tlsServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) })
			client, err := New(Options{CACert: pemCA})
			require.NoError(t, err)
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			resp, err := client.Get(srv.URL + "/x")
			require.NoError(t, err)
			_ = resp.Body.Close()
			hosts := AnsweredHosts()
			require.Len(t, hosts, 1)
			assert.Equal(t, originOfURL(t, srv.URL), hosts[0].Origin)
			assert.Same(t, baseTransport(t, client), hosts[0].Transport, "the transport below the User-Agent transport")
		})
	}

	t.Run("a refused port", func(t *testing.T) {
		resetRecords(t)
		client, err := New(Options{})
		require.NoError(t, err)
		_, err = client.Get("https://127.0.0.1:1/x")
		require.Error(t, err)
		assert.Empty(t, AnsweredHosts())
	})
	t.Run("a failed TLS handshake", func(t *testing.T) {
		resetRecords(t)
		srv, _ := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {})
		client, err := New(Options{})
		require.NoError(t, err)
		_, err = client.Get(srv.URL + "/x")
		var unknown x509.UnknownAuthorityError
		require.ErrorAs(t, err, &unknown)
		assert.Empty(t, AnsweredHosts())
	})
}

// TestTheLastTransportIsKeptAndTheOrderIsTheLastRecording asserts a host
// recorded again keeps its latest transport and moves to the end, hosts that
// compare equal after canonicalization stay two entries in the order they
// were last recorded, and a URL with no scheme or host records nothing.
func TestTheLastTransportIsKeptAndTheOrderIsTheLastRecording(t *testing.T) {
	resetRecords(t)
	a1, a2, b := &stubTransport{"a1"}, &stubTransport{"a2"}, &stubTransport{"b"}
	RecordAnswered(&url.URL{Scheme: "https", Host: "H:443"}, a1)
	RecordAnswered(&url.URL{Scheme: "https", Host: "h"}, b)
	RecordAnswered(&url.URL{Scheme: "https", Host: "H:443"}, a2)
	RecordAnswered(&url.URL{Scheme: "https"}, a1)
	RecordAnswered(&url.URL{Host: "x"}, a1)
	RecordAnswered(nil, a1)
	RecordAnswered(&url.URL{Scheme: "https", Host: "y"}, nil)
	assert.Equal(t, []AnsweredHost{
		{Origin: "https://h", Transport: b},
		{Origin: "https://H:443", Transport: a2},
	}, AnsweredHosts())

	got := AnsweredHosts()
	got[0].Origin = "changed"
	assert.Equal(t, "https://h", AnsweredHosts()[0].Origin, "the reader returns a copy")
}

// TestRegisteringAFetcherKeepsTheLastOneAndNeedsAHost asserts the last
// registration wins and a registration with no host, no scheme or no function
// records nothing.
func TestRegisteringAFetcherKeepsTheLastOneAndNeedsAHost(t *testing.T) {
	resetRecords(t)
	first := func(context.Context) (string, int, error) { return "first", 200, nil }
	second := func(context.Context) (string, int, error) { return "second", 200, nil }

	_, ok := RegisteredInstallIDFetcher()
	assert.False(t, ok)
	for _, origin := range []string{"", "https://", "h", "://x", "https:///path"} {
		RegisterInstallIDFetcher(origin, first)
		_, ok = RegisteredInstallIDFetcher()
		assert.False(t, ok, "origin %q", origin)
	}
	RegisterInstallIDFetcher("https://h", nil)
	_, ok = RegisteredInstallIDFetcher()
	assert.False(t, ok, "no function")

	RegisterInstallIDFetcher("https://a", first)
	RegisterInstallIDFetcher("https://b", second)
	f, ok := RegisteredInstallIDFetcher()
	require.True(t, ok)
	assert.Equal(t, "https://b", f.Origin)
	value, status, err := f.Fetch(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "second", value)
	assert.Equal(t, 200, status)

	RegisterInstallIDFetcher("", first)
	f, _ = RegisteredInstallIDFetcher()
	assert.Equal(t, "https://b", f.Origin, "a refused registration leaves the last one")
}

// TestClearRecordsEmptiesBothRecords asserts the one clear function empties
// the hosts and the fetcher.
func TestClearRecordsEmptiesBothRecords(t *testing.T) {
	resetRecords(t)
	RecordAnswered(&url.URL{Scheme: "https", Host: "h"}, &stubTransport{"a"})
	RegisterInstallIDFetcher("https://h", func(context.Context) (string, int, error) { return "", 0, nil })
	ClearRecords()
	assert.Empty(t, AnsweredHosts())
	_, ok := RegisteredInstallIDFetcher()
	assert.False(t, ok)
}

// TestTheRecordsHoldUnderConcurrentUse asserts, under the race detector,
// that many goroutines can record, read, register and clear at once.
func TestTheRecordsHoldUnderConcurrentUse(t *testing.T) {
	resetRecords(t)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			host := fmt.Sprintf("h%d", i%4)
			for range 200 {
				RecordAnswered(&url.URL{Scheme: "https", Host: host}, &stubTransport{host})
				_ = AnsweredHosts()
				RegisterInstallIDFetcher("https://"+host, func(context.Context) (string, int, error) { return host, 200, nil })
				_, _ = RegisteredInstallIDFetcher()
				if i%8 == 0 {
					ClearRecords()
				}
			}
		})
	}
	wg.Wait()
}

// versionServer is a TLS server for the version request tests. It answers
// /rancherversion as its mode says and keeps the headers and paths it saw.
type versionServer struct {
	*httptest.Server
	ca      string
	mode    atomic.Int32
	other   *httptest.Server
	otherN  atomic.Int32
	mu      sync.Mutex
	headers []http.Header
	paths   []string
}

const (
	versionOK int32 = iota
	versionRedirect
	versionLimit
	versionOverLimit
	versionBroken
)

func newVersionServer(t *testing.T) *versionServer {
	t.Helper()
	v := &versionServer{}
	v.other, _ = tlsServer(t, func(w http.ResponseWriter, r *http.Request) { v.otherN.Add(1) })
	v.Server, v.ca = tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.headers = append(v.headers, r.Header.Clone())
		v.paths = append(v.paths, r.URL.Path)
		v.mu.Unlock()
		switch v.mode.Load() {
		case versionRedirect:
			http.Redirect(w, r, v.other.URL+"/rancherversion", http.StatusFound)
		case versionLimit:
			_, _ = w.Write([]byte(strings.Repeat("a", FactsBodyLimit)))
		case versionOverLimit:
			_, _ = w.Write([]byte(strings.Repeat("a", FactsBodyLimit+1)))
		case versionBroken:
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("short"))
			panic(http.ErrAbortHandler)
		default:
			_, _ = w.Write([]byte(`{"Version":"v2.15.2"}`))
		}
	})
	return v
}

// answeredHost makes one request through a client built by New, so the host
// is recorded, and returns the recorded entry.
func (v *versionServer) answeredHost(t *testing.T) AnsweredHost {
	t.Helper()
	client, err := New(Options{CACert: v.ca})
	require.NoError(t, err)
	resp, err := client.Get(v.URL + "/seed")
	require.NoError(t, err)
	_ = resp.Body.Close()
	hosts := AnsweredHosts()
	require.Len(t, hosts, 1)
	v.mu.Lock()
	v.headers, v.paths = nil, nil
	v.mu.Unlock()
	return hosts[0]
}

// TestTheVersionRequestCarriesOnlyTheUserAgent asserts the request goes to
// /rancherversion at the host root through the given transport, with the
// CLI User-Agent and no credential header, and follows no redirect.
func TestTheVersionRequestCarriesOnlyTheUserAgent(t *testing.T) {
	resetRecords(t)
	defer SetUserAgent("cli.rancher.io/test")()
	v := newVersionServer(t)
	host := v.answeredHost(t)

	got, err := RequestVersion(t.Context(), host.Transport, host.Origin)
	require.NoError(t, err)
	assert.Equal(t, 200, got.Status)
	assert.JSONEq(t, `{"Version":"v2.15.2"}`, string(got.Body))
	assert.False(t, got.TooLong)
	v.mu.Lock()
	require.Equal(t, []string{"/rancherversion"}, v.paths)
	assert.Equal(t, "cli.rancher.io/test", v.headers[0].Get("User-Agent"))
	for _, name := range []string{"Authorization", "Impersonate-User", "Cookie"} {
		assert.Empty(t, v.headers[0].Values(name), name)
	}
	v.mu.Unlock()

	v.mode.Store(versionRedirect)
	redirected, err := RequestVersion(t.Context(), host.Transport, host.Origin)
	require.NoError(t, err)
	assert.Equal(t, http.StatusFound, redirected.Status, "the redirect is the answer; it is not followed")
	assert.Zero(t, v.otherN.Load(), "the redirect target saw no request")
}

// TestTheVersionRequestReadsThroughALimit asserts a body at the limit is
// returned, a body over it is reported as too long with no bytes, a failed
// body read is an error, a cancelled context is an error and a missing
// transport is an error.
func TestTheVersionRequestReadsThroughALimit(t *testing.T) {
	resetRecords(t)
	v := newVersionServer(t)
	host := v.answeredHost(t)

	v.mode.Store(versionLimit)
	got, err := RequestVersion(t.Context(), host.Transport, host.Origin)
	require.NoError(t, err)
	assert.Len(t, got.Body, FactsBodyLimit)
	assert.False(t, got.TooLong)

	v.mode.Store(versionOverLimit)
	got, err = RequestVersion(t.Context(), host.Transport, host.Origin)
	require.NoError(t, err)
	assert.Equal(t, 200, got.Status)
	assert.True(t, got.TooLong)
	assert.Empty(t, got.Body)

	v.mode.Store(versionBroken)
	_, err = RequestVersion(t.Context(), host.Transport, host.Origin)
	require.Error(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = RequestVersion(ctx, host.Transport, host.Origin)
	require.Error(t, err)
	_, err = RequestVersion(t.Context(), nil, host.Origin)
	require.Error(t, err)
}

// TestDoLimitedReportsALongBody asserts the bounded read the install id
// request uses: a body over the limit comes back as too long with its status.
func TestDoLimitedReportsALongBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("b", 100)))
	}))
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, body, tooLong, err := DoLimited(srv.Client(), req, 99)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.True(t, tooLong)
	assert.Empty(t, body)

	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	_, body, tooLong, err = DoLimited(srv.Client(), req, 100)
	require.NoError(t, err)
	assert.False(t, tooLong)
	assert.Len(t, body, 100)
}
