package httpx

import (
	"net/http"
	"runtime"
	"sync/atomic"
)

// userAgent is the value every httpx.New and kube.HTTPClient request sends.
// It is set once, by main, before any client exists.
var userAgent atomic.Pointer[string]

// SetUserAgent sets the User-Agent value of every request the CLI sends to
// Rancher or a downstream endpoint, and returns a function that puts the
// previous value back. The process sets it once, in main, before it builds a
// client. Tests that call it must not run in parallel with tests that read
// the value or send requests.
func SetUserAgent(v string) (restore func()) {
	old := userAgent.Swap(&v)
	return func() { userAgent.Store(old) }
}

// UserAgent returns the value SetUserAgent last set, or
// "cli.rancher.io/dev (<os>/<arch>)" when none was set.
func UserAgent() string {
	if v := userAgent.Load(); v != nil {
		return *v
	}
	return "cli.rancher.io/dev (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}

// WithoutCancel returns req without its Cancel channel: a shallow copy when
// the channel is set, req itself otherwise. http.Client applies its Timeout
// to a transport it does not recognize through the deprecated Request.Cancel
// channel as well as the context deadline, and a transport that sees both
// may report either: "request canceled" instead of the deadline error the
// same client reports without a wrapper. The context carries the deadline,
// so a wrapping transport hands the inner one the request without the
// channel.
func WithoutCancel(req *http.Request) *http.Request {
	if req.Cancel == nil { //nolint:staticcheck // the channel is set by http.Client itself; this is the only way to withhold it
		return req
	}
	withoutCancel := *req
	withoutCancel.Cancel = nil //nolint:staticcheck // see above
	return &withoutCancel
}

// userAgentTransport sends every request with UserAgent(), replacing any
// User-Agent the caller set: no caller can drop or change it.
type userAgentTransport struct {
	next http.RoundTripper
}

// WrappedRoundTripper returns the transport this one wraps; client-go and
// the tests walk wrapped transports through this method.
func (t *userAgentTransport) WrappedRoundTripper() http.RoundTripper { return t.next }

// CloseIdleConnections forwards to the wrapped transport:
// http.Client.CloseIdleConnections still reaches the connection pool.
func (t *userAgentTransport) CloseIdleConnections() {
	if closer, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t *userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A transport must not modify the caller's request: the header is set
	// on a clone.
	clone := req.Clone(req.Context())
	// The clone is this transport's own: the Cancel channel is cleared in
	// place. WithoutCancel explains why.
	clone.Cancel = nil //nolint:staticcheck // the channel is set by http.Client itself; this is the only way to withhold it
	if clone.Header == nil {
		clone.Header = http.Header{}
	}
	clone.Header.Set("User-Agent", UserAgent())
	return t.next.RoundTrip(clone)
}
