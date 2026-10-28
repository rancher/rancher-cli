package httpx

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reqTo(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	require.NoError(t, err)
	return req
}

// TestRejectCrossOriginRedirectTable asserts the policy on the origin tuple:
// equivalent spellings are followed, any scheme/host/port change is refused
// with a GENERIC error, and the hop cap matches Go's default.
func TestRejectCrossOriginRedirectTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, first, next string
		refused           bool
	}{
		{"same", "https://h/a", "https://h/b", false},
		{"host case", "https://H.example/a", "https://h.example/b", false},
		{"explicit default port", "https://h/a", "https://h:443/b", false},
		{"ipv6 default port", "https://[::1]/a", "https://[::1]:443/b", false},
		{"ipv4 same", "https://127.0.0.1:9443/a", "https://127.0.0.1:9443/b", false},
		{"scheme upgrade", "http://h/a", "https://h/a", true},
		{"scheme downgrade", "https://h/a", "http://h/a", true},
		{"host", "https://a.example/", "https://b.example/", true},
		{"port", "https://h:443/", "https://h:8443/", true},
		{"ipv6 port", "https://[::1]:443/", "https://[::1]:8443/", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := RejectCrossOriginRedirect(reqTo(t, tc.next), []*http.Request{reqTo(t, tc.first)})
			if !tc.refused {
				assert.NoError(t, err)
				return
			}
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "refusal must be a structured error, got %v", err)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.False(t, ce.Retryable)
			assert.Empty(t, ce.Hint)
			require.ErrorContains(t, err, "a redirect to another origin is not followed, to keep credentials from being sent there")
		})
	}
}

func TestRejectCrossOriginRedirectHopCap(t *testing.T) {
	t.Parallel()
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = reqTo(t, "https://h/"+strconv.Itoa(i))
	}
	err := RejectCrossOriginRedirect(reqTo(t, "https://h/x"), via)
	require.ErrorContains(t, err, "stopped after 10 redirects from https://h")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "the hop cap is a structured refusal like every other")
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.False(t, ce.Retryable)
}

// TestRejectCrossOriginRedirectUnusableTargetIsStructured asserts that a
// redirect target with no usable origin (an unsupported scheme) is refused
// with the same structured, non-retryable shape as a cross-origin target.
func TestRejectCrossOriginRedirectUnusableTargetIsStructured(t *testing.T) {
	t.Parallel()
	err := RejectCrossOriginRedirect(reqTo(t, "ftp://h/x"), []*http.Request{reqTo(t, "https://h/a")})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.False(t, ce.Retryable)
	require.ErrorContains(t, err, "refused to follow a redirect from https://h: the redirect target has no usable origin")
	assert.NotNil(t, errors.Unwrap(ce), "the origin parse error stays reachable as the cause")
}

// TestRejectCrossOriginRedirectHopCapWinsOverUnusableOrigin asserts the
// precedence: at the hop cap a loop is reported even when the first request
// has no usable origin, and the origin is named only when it parses.
func TestRejectCrossOriginRedirectHopCapWinsOverUnusableOrigin(t *testing.T) {
	t.Parallel()
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = reqTo(t, "ftp://h/"+strconv.Itoa(i))
	}
	err := RejectCrossOriginRedirect(reqTo(t, "https://h/x"), via)
	require.ErrorContains(t, err, "stopped after 10 redirects")
	assert.NotContains(t, err.Error(), " from ")
}

// twoServers returns TLS servers A and B where A answers /start with a
// redirect of the given status to B, A answers /same with a same-origin
// redirect to /final, /final echoes the body, and B counts every request.
func twoServers(t *testing.T, status int) (a, b *httptest.Server, bHits *atomic.Int32, caPEM string) {
	t.Helper()
	bHits = &atomic.Int32{}
	b = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(b.Close)
	var aURL string
	a = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, b.URL+"/landed", status)
		case "/same":
			http.Redirect(w, r, aURL+"/final", status)
		case "/final":
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(a.Close)
	aURL = a.URL
	caPEM = string(pemOf(t, a.Certificate()))
	return a, b, bHits, caPEM
}

// TestNewRefusesCrossOriginRedirects asserts, over real TLS servers, that every
// redirect status is refused before any request reaches the other origin.
func TestNewRefusesCrossOriginRedirects(t *testing.T) {
	t.Parallel()
	for _, status := range []int{301, 302, 307, 308} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			a, _, bHits, caPEM := twoServers(t, status)
			client, err := New(Options{CACert: caPEM})
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.URL+"/start", bytes.NewReader([]byte("secret")))
			require.NoError(t, err)
			_, _, err = Do(client, req)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			var uerr *url.Error
			require.ErrorAs(t, err, &uerr, "the client wraps the refusal in *url.Error")
			assert.Zero(t, bHits.Load(), "no request may reach the other origin")
		})
	}
}

// TestNewFollowsSameOriginRedirectWithBody asserts that a same-origin 307 is
// followed and replays the body.
func TestNewFollowsSameOriginRedirectWithBody(t *testing.T) {
	t.Parallel()
	a, _, _, caPEM := twoServers(t, http.StatusTemporaryRedirect)
	client, err := New(Options{CACert: caPEM})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.URL+"/same", bytes.NewReader([]byte("payload")))
	require.NoError(t, err)
	resp, body, err := Do(client, req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "payload", string(body))
}

// TestNewExternalRefusesCrossOriginRedirectWithoutServerWording asserts that
// the external client carries the same policy and that the refusal names
// only the origins, never a server entry.
func TestNewExternalRefusesCrossOriginRedirectWithoutServerWording(t *testing.T) {
	t.Parallel()
	// Plain HTTP fixtures: NewExternal trusts only OS roots, so a TLS
	// fixture cannot be reached; the redirect policy is scheme-agnostic.
	bHits := &atomic.Int32{}
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bHits.Add(1) }))
	t.Cleanup(b.Close)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+"/token", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(a.Close)
	client := NewExternal(DefaultTimeout)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.URL+"/token", bytes.NewReader([]byte("code=x")))
	require.NoError(t, err)
	_, _, err = Do(client, req)
	require.ErrorContains(t, err, "refused to follow a redirect from "+a.URL+" to "+b.URL)
	assert.NotContains(t, err.Error(), "server entry")
	assert.Zero(t, bHits.Load())
}
