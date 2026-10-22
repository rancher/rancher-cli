package httpx

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// baseTransport returns the *http.Transport at the bottom of client's
// transport chain, walking the wrappers through WrappedRoundTripper.
func baseTransport(t *testing.T, client *http.Client) *http.Transport {
	t.Helper()
	rt := client.Transport
	for {
		if tr, ok := rt.(*http.Transport); ok {
			return tr
		}
		wrapper, ok := rt.(interface{ WrappedRoundTripper() http.RoundTripper })
		require.True(t, ok, "transport %T is neither *http.Transport nor a wrapper", rt)
		rt = wrapper.WrappedRoundTripper()
	}
}

// recordingServer records the User-Agent values of the requests it gets.
type recordingServer struct {
	*httptest.Server
	mu     sync.Mutex
	agents []string
}

func newRecordingServer(t *testing.T, handler http.HandlerFunc) *recordingServer {
	t.Helper()
	rec := &recordingServer{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.agents = append(rec.agents, r.Header.Get("User-Agent"))
		rec.mu.Unlock()
		if handler != nil {
			handler(w, r)
		}
	}))
	t.Cleanup(rec.Close)
	return rec
}

func (r *recordingServer) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	got := r.agents
	r.agents = nil
	return got
}

func get(t *testing.T, client *http.Client, url string, header http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	maps.Copy(req.Header, header)
	_, _, err = Do(client, req)
	require.NoError(t, err)
}

// TestUserAgentDefaultsToTheDevValue asserts the value before anything sets
// it, and that SetUserAgent's restore function puts the previous value back.
// Not parallel: it sets the process-wide value.
func TestUserAgentDefaultsToTheDevValue(t *testing.T) {
	before := UserAgent()
	restore := SetUserAgent("cli.rancher.io/x (a/b)")
	assert.Equal(t, "cli.rancher.io/x (a/b)", UserAgent())
	restoreInner := SetUserAgent("cli.rancher.io/y (a/b)")
	assert.Equal(t, "cli.rancher.io/y (a/b)", UserAgent())
	restoreInner()
	assert.Equal(t, "cli.rancher.io/x (a/b)", UserAgent())
	restore()
	assert.Equal(t, before, UserAgent())
	assert.Regexp(t, `^cli\.rancher\.io/dev \([a-z0-9]+/[a-z0-9]+\)$`, before)
}

// TestNewSendsTheUserAgentOnEveryRequest asserts that a request through an
// httpx.New client carries UserAgent(), whether the caller set none, set
// another one, or set it in any letter case, and that the value in force
// when the request is sent is the one sent. Not parallel: it sets the
// process-wide value.
func TestNewSendsTheUserAgentOnEveryRequest(t *testing.T) {
	t.Cleanup(SetUserAgent("cli.rancher.io/test (test/test)"))
	srv := newRecordingServer(t, nil)
	client, err := New(Options{})
	require.NoError(t, err)

	get(t, client, srv.URL, nil)
	get(t, client, srv.URL, http.Header{"User-Agent": {"caller/1.0"}})
	get(t, client, srv.URL, http.Header{"user-agent": {"lower/1.0"}})
	get(t, client, srv.URL, http.Header{"User-Agent": {"a/1", "b/2"}})
	want := "cli.rancher.io/test (test/test)"
	assert.Equal(t, []string{want, want, want, want}, srv.take())

	restore := SetUserAgent("cli.rancher.io/later (test/test)")
	defer restore()
	get(t, client, srv.URL, nil)
	assert.Equal(t, []string{"cli.rancher.io/later (test/test)"}, srv.take())
}

// TestNewSendsTheUserAgentThroughTheCallersWrapper asserts the caller's
// WrapTransport wraps the User-Agent transport: the wrapper sees the
// caller's request, and the server still sees the value. Not parallel: it
// sets the process-wide value.
func TestNewSendsTheUserAgentThroughTheCallersWrapper(t *testing.T) {
	t.Cleanup(SetUserAgent("cli.rancher.io/test (test/test)"))
	srv := newRecordingServer(t, nil)
	var seen []string
	client, err := New(Options{WrapTransport: func(next http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			seen = append(seen, req.Header.Get("User-Agent"))
			return next.RoundTrip(req)
		})
	}})
	require.NoError(t, err)
	get(t, client, srv.URL, http.Header{"User-Agent": {"caller/1.0"}})
	assert.Equal(t, []string{"caller/1.0"}, seen)
	assert.Equal(t, []string{"cli.rancher.io/test (test/test)"}, srv.take())
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestNewDoesNotChangeTheCallersRequest asserts the transport sets the
// header on a copy: the request the caller built keeps its own header and
// its Cancel channel.
func TestNewDoesNotChangeTheCallersRequest(t *testing.T) {
	srv := newRecordingServer(t, nil)
	client, err := New(Options{})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req.Header.Set("User-Agent", "caller/1.0")
	cancel := make(chan struct{})
	req.Cancel = cancel //nolint:staticcheck // the test asserts the transport leaves the caller's request alone
	_, _, err = Do(client, req)
	require.NoError(t, err)
	assert.Equal(t, "caller/1.0", req.Header.Get("User-Agent"))
	assert.NotNil(t, req.Cancel) //nolint:staticcheck // see above
}

// TestNewSetsTheUserAgentOnARequestWithNoHeaderMap asserts a hand-built
// request whose Header map is nil still gets the value.
func TestNewSetsTheUserAgentOnARequestWithNoHeaderMap(t *testing.T) {
	srv := newRecordingServer(t, nil)
	client, err := New(Options{})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req.Header = nil
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, []string{UserAgent()}, srv.take())
}

// TestNewExternalSendsNoUserAgentOfOurs asserts a NewExternal client sends
// Go's default User-Agent and its transport is not wrapped: identity
// providers and other third parties get nothing new.
func TestNewExternalSendsNoUserAgentOfOurs(t *testing.T) {
	srv := newRecordingServer(t, nil)
	client := NewExternal(DefaultTimeout)
	_, ok := client.Transport.(*http.Transport)
	assert.True(t, ok, "the external client's transport is %T", client.Transport)
	get(t, client, srv.URL, nil)
	got := srv.take()
	require.Len(t, got, 1)
	assert.Regexp(t, `^Go-http-client/`, got[0])
	assert.NotContains(t, got[0], "cli.rancher.io")
}

// TestNewTimeoutStillReportsTheDeadline asserts a client with a Timeout
// reports the context deadline when the server does not answer in time:
// the User-Agent transport hands the inner transport the request without
// http.Client's Cancel channel, which would otherwise report "request
// canceled".
func TestNewTimeoutStillReportsTheDeadline(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := newRecordingServer(t, func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })
	client, err := New(Options{Timeout: 50 * time.Millisecond})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	_, err = client.Do(req) //nolint:bodyclose // the request fails
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestNewForwardsCloseIdleConnections asserts http.Client's
// CloseIdleConnections reaches the pooled transport through the wrapper, and
// that WrappedRoundTripper returns the transport New built.
func TestNewForwardsCloseIdleConnections(t *testing.T) {
	t.Parallel()
	client, err := New(Options{})
	require.NoError(t, err)
	wrapper, ok := client.Transport.(*userAgentTransport)
	require.True(t, ok, "transport is %T", client.Transport)
	var closed int
	wrapper.next = closeCounter{RoundTripper: wrapper.next, closed: &closed}
	client.CloseIdleConnections()
	assert.Equal(t, 1, closed)
	assert.IsType(t, closeCounter{}, wrapper.WrappedRoundTripper())
}

type closeCounter struct {
	http.RoundTripper
	closed *int
}

func (c closeCounter) CloseIdleConnections() { *c.closed++ }

// TestWithoutCancel asserts the helper returns the request itself when it
// has no Cancel channel and a copy without it otherwise.
func TestWithoutCancel(t *testing.T) {
	t.Parallel()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://x.invalid", nil)
	require.NoError(t, err)
	assert.Same(t, req, WithoutCancel(req))
	req.Cancel = make(chan struct{}) //nolint:staticcheck // the test builds the channel http.Client sets
	got := WithoutCancel(req)
	assert.NotSame(t, req, got)
	assert.Nil(t, got.Cancel)    //nolint:staticcheck // see above
	assert.NotNil(t, req.Cancel) //nolint:staticcheck // see above
}

// cancelAwareTransport waits on the request's Cancel channel when it has
// one, as a transport that sees both it and the context deadline may, and
// on the context otherwise.
type cancelAwareTransport struct{}

func (cancelAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if cancel := req.Cancel; cancel != nil { //nolint:staticcheck // models the transport that sees the channel http.Client sets
		<-cancel
		return nil, errors.New("net/http: request canceled")
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// TestUserAgentTransportKeepsTheClientTimeoutADeadlineError asserts a client
// that times out through the User-Agent transport reports the deadline and
// not the legacy cancellation http.Client signals through Request.Cancel to
// a transport it does not recognize. Mutation: forward the clone with its
// Cancel channel.
func TestUserAgentTransportKeepsTheClientTimeoutADeadlineError(t *testing.T) {
	t.Parallel()
	client := &http.Client{
		Transport: &userAgentTransport{next: cancelAwareTransport{}},
		Timeout:   50 * time.Millisecond,
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://rancher.example/slow", nil)
	require.NoError(t, err)
	_, err = client.Do(req) //nolint:bodyclose // the request fails
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), "request canceled")
}
