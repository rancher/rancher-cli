package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

type Options struct {
	CACert   string
	Insecure bool
	Timeout  time.Duration
	// WrapTransport, when set, wraps the client's transport. A caller that
	// must see every transport error of the client (to classify it) installs
	// its wrapper here; httpx cannot import the classifier itself.
	WrapTransport func(http.RoundTripper) http.RoundTripper
}

// DefaultTimeout is the client-level request bound callers reach for when
// they have no tighter, already-established timeout of their own (auth and
// token-creation round trips run unattended under kubectl's exec-credential
// plugin, so a caller must never rely on dial/TLS-handshake timeouts alone
// to cap an unresponsive server). Callers that already bound the request
// via a shorter context deadline (a per-path timeout var, tested and
// shrinkable) pass that value instead - it always wins since it fires
// first, and DefaultTimeout serves those callers only as a backstop against
// a future refactor that drops the context wrap. A package var, not a
// constant, so tests can shrink it via SetDefaultTimeoutForTest.
var DefaultTimeout = 30 * time.Second

// SetDefaultTimeoutForTest shrinks DefaultTimeout and returns a restore
// func. Callers must not run in parallel with tests issuing real requests.
func SetDefaultTimeoutForTest(d time.Duration) (restore func()) {
	old := DefaultTimeout
	DefaultTimeout = d
	return func() { DefaultTimeout = old }
}

// New builds the client for Rancher, downstream endpoints and the telemetry
// upload endpoint. Every request it sends carries UserAgent(), whatever the
// caller set.
func New(o Options) (*http.Client, error) {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	if o.Insecure {
		tlsConfig.InsecureSkipVerify = true
	} else if o.CACert != "" {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(o.CACert)) {
			return nil, errors.New("could not read any CA certificate from the PEM text")
		}
		tlsConfig.RootCAs = roots
	}

	// The User-Agent transport sits inside the caller's wrapper, so a
	// wrapper that classifies errors still sees every request, and the
	// header is already on the request when the wrapper hands it down.
	var transport http.RoundTripper = &userAgentTransport{next: newTransport(tlsConfig)}
	if o.WrapTransport != nil {
		transport = o.WrapTransport(transport)
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       o.Timeout,
		CheckRedirect: RejectCrossOriginRedirect,
	}, nil
}

// NewExternal builds the client for third-party origins the login flow
// must reach (the identity provider's OAuth endpoints). Verification uses
// the operating system trust store only: no pinned CA, no insecure mode,
// because the Rancher entry's trust must never extend to another origin.
// Proxy environment, dialer, TLS minimum, and the same-origin redirect
// policy match New.
func NewExternal(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport:     newTransport(&tls.Config{MinVersion: tls.VersionTLS12}),
		Timeout:       timeout,
		CheckRedirect: RejectCrossOriginRedirect,
	}
}

// newTransport is the one transport shape both constructors share, so the
// Rancher and external clients cannot drift in proxy or timeout behavior.
func newTransport(tlsConfig *tls.Config) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSClientConfig:       tlsConfig,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext:           dialer.DialContext,
	}
}

// Do executes req and reads the full response body, closing it before
// returning - the one request/response shape every hand-rolled Rancher API
// caller in this codebase needs (token's provider dance, login's ext-token
// create/revoke), so it lives here once instead of once per package.
func Do(client *http.Client, req *http.Request) (*http.Response, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return resp, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, err
	}
	return resp, body, nil
}
