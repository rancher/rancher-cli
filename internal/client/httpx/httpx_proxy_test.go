package httpx

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http/httpproxy"
)

// connectProxy is a minimal forward proxy: it answers CONNECT by hijacking
// the client connection and piping bytes to/from resolve(r.Host), recording
// every host:port it was asked to tunnel to (in the form the client sent,
// before resolution). Any other method is rejected, matching a real forward
// proxy's TLS posture — it never terminates or inspects the tunneled bytes.
type connectProxy struct {
	srv     *httptest.Server
	resolve func(hostport string) string

	mu      sync.Mutex
	targets []string
}

// newConnectProxy starts the fixture. A nil resolve tunnels to the address
// the client actually asked for; tests that route to a hostname-shaped
// (not loopback-literal) target pass a resolve func that redirects that
// hostname to the loopback address it is really listening on.
func newConnectProxy(t *testing.T, resolve func(hostport string) string) *connectProxy {
	t.Helper()
	if resolve == nil {
		resolve = func(hostport string) string { return hostport }
	}
	p := &connectProxy{resolve: resolve}
	p.srv = httptest.NewServer(http.HandlerFunc(p.handleConnect))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *connectProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "connect only", http.StatusMethodNotAllowed)
		return
	}

	p.mu.Lock()
	p.targets = append(p.targets, r.Host)
	p.mu.Unlock()

	dst, err := net.Dial("tcp", p.resolve(r.Host))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = dst.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	src, _, err := hj.Hijack()
	if err != nil {
		_ = dst.Close()
		return
	}

	_, _ = src.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(dst, src) }()
	go func() { defer wg.Done(); _, _ = io.Copy(src, dst) }()
	wg.Wait()
	_ = dst.Close()
	_ = src.Close()
}

func (p *connectProxy) tunneledTargets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

// loopbackResolver redirects hostname:port to realAddr, leaving any other
// host untouched. It backs both the proxy fixture's own dial-through and a
// client Transport's DialContext, so a hostname-shaped (not IP-literal)
// target can be used everywhere a loopback listener actually answers —
// httpproxy's "never proxy a loopback address" rule only inspects IP
// literals, so a hostname stays subject to normal proxy routing decisions.
func loopbackResolver(hostname, realAddr string) func(hostport string) string {
	return func(hostport string) string {
		host, _, err := net.SplitHostPort(hostport)
		if err != nil || host != hostname {
			return hostport
		}
		return realAddr
	}
}

// asTransportProxy adapts an httpproxy-style URL-keyed proxy func to the
// *http.Request-keyed shape http.Transport.Proxy expects.
func asTransportProxy(fn func(*url.URL) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) { return fn(req.URL) }
}

func dialWithResolve(resolve func(string) string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, resolve(addr))
	}
}

// newLoopbackTLSTarget starts an httptest TLS server bound to 127.0.0.1
// presenting a certificate valid for host, standing in for an internal
// server a real client would reach by a routable hostname.
func newLoopbackTLSTarget(t *testing.T, host string) (certPEM string, srv *httptest.Server) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))

	srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return certPEM, srv
}

// nonLoopbackIPv4 returns the machine's first non-loopback IPv4 address, or
// "" if none is usable — sandboxed CI sometimes exposes loopback only.
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		return ipnet.IP.String()
	}
	return ""
}

func TestNewRoutesThroughConnectProxyWithPinnedCA(t *testing.T) {
	t.Parallel()
	const targetHost = "proxy-target.internal.test"
	certPEM, target := newLoopbackTLSTarget(t, targetHost)
	targetAddr := target.Listener.Addr().String()
	_, targetPort, err := net.SplitHostPort(targetAddr)
	require.NoError(t, err)

	resolve := loopbackResolver(targetHost, targetAddr)
	proxy := newConnectProxy(t, resolve)

	client, err := New(Options{CACert: certPEM})
	require.NoError(t, err)
	tr := client.Transport.(*http.Transport)
	tr.DisableKeepAlives = true
	tr.DialContext = dialWithResolve(resolve)
	tr.Proxy = asTransportProxy((&httpproxy.Config{HTTPSProxy: proxy.srv.URL}).ProxyFunc())

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://%s:%s/", targetHost, targetPort), nil)
	require.NoError(t, err)
	resp, body, err := Do(client, req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ok", string(body))

	assert.Equal(t, []string{net.JoinHostPort(targetHost, targetPort)}, proxy.tunneledTargets())
}

func TestNewNoProxyBypassesFixtureForNamedTarget(t *testing.T) {
	t.Parallel()
	const targetHost = "proxy-target.internal.test"
	certPEM, target := newLoopbackTLSTarget(t, targetHost)
	targetAddr := target.Listener.Addr().String()
	_, targetPort, err := net.SplitHostPort(targetAddr)
	require.NoError(t, err)

	resolve := loopbackResolver(targetHost, targetAddr)
	proxy := newConnectProxy(t, resolve)

	client, err := New(Options{CACert: certPEM})
	require.NoError(t, err)
	tr := client.Transport.(*http.Transport)
	tr.DisableKeepAlives = true
	tr.DialContext = dialWithResolve(resolve)
	tr.Proxy = asTransportProxy((&httpproxy.Config{HTTPSProxy: proxy.srv.URL, NoProxy: targetHost}).ProxyFunc())

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://%s:%s/", targetHost, targetPort), nil)
	require.NoError(t, err)
	resp, _, err := Do(client, req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Empty(t, proxy.tunneledTargets(), "a NO_PROXY-listed host must never reach the proxy fixture")
}

func TestNewProxyUnreachableErrorNamesProxyAddress(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	proxyAddr := l.Addr().String()
	require.NoError(t, l.Close())

	client, err := New(Options{})
	require.NoError(t, err)
	tr := client.Transport.(*http.Transport)
	tr.Proxy = asTransportProxy((&httpproxy.Config{HTTPSProxy: "http://" + proxyAddr}).ProxyFunc())

	req, err := http.NewRequest(http.MethodGet, "https://example.test/", nil)
	require.NoError(t, err)
	_, _, err = Do(client, req)
	require.Error(t, err)
	require.ErrorContains(t, err, proxyAddr, "a proxy-dial failure must name the proxy address so it reads distinctly from a server failure")
}

// TestProxyFuncNeverProxiesLoopbackTarget asserts the documented Go stdlib
// behavior httpx.New's proxy wiring relies on — golang.org/x/net/http/httpproxy
// treats an IP-literal loopback host as never-proxied regardless of any
// configured proxy — so a future refactor of that wiring cannot silently
// change it.
func TestProxyFuncNeverProxiesLoopbackTarget(t *testing.T) {
	t.Parallel()
	fn := (&httpproxy.Config{HTTPSProxy: "http://proxy.example:8080"}).ProxyFunc()

	for _, host := range []string{"127.0.0.1:443", "[::1]:443", "localhost:443"} {
		u, err := fn(&url.URL{Scheme: "https", Host: host})
		require.NoError(t, err)
		assert.Nil(t, u, "loopback target %q must never be proxied", host)
	}
}

// TestNewHTTPSProxyEnvVarRoutesThroughFixture exercises real HTTPS_PROXY/
// NO_PROXY values end to end against httpx.New's TLS/CA/dialer setup. It
// does NOT exercise httpx.New's own Proxy: http.ProxyFromEnvironment field —
// that assignment is overwritten below, deliberately, because
// http.ProxyFromEnvironment caches its environment read process-wide behind
// a private, unresettable sync.Once inside net/http: the first httpx client
// anywhere in this test binary to actually dial through that field locks in
// whatever HTTPS_PROXY/NO_PROXY were set at that moment for the rest of the
// process, making a second t.Setenv-driven test unreliable.
// httpproxy.FromEnvironment() parses the identical env vars with no such
// cache, so it stands in here for a fresh-per-test read of real env vars.
// The regression guard for httpx.New's own Proxy field assignment is
// internal/client/provider's TestRancherVersionTraversesConnectProxyWithPinnedCA,
// which calls production code without touching that field.
// Skips when the host exposes no non-loopback interface, since the target
// must not be a loopback literal (see TestProxyFuncNeverProxiesLoopbackTarget).
func TestNewHTTPSProxyEnvVarRoutesThroughFixture(t *testing.T) {
	ip := nonLoopbackIPv4(t)
	if ip == "" {
		t.Skip("no non-loopback IPv4 interface available in this environment")
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: ip},
		IPAddresses:  []net.IP{net.ParseIP(ip)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	l, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	require.NoError(t, err)
	target.Listener = l
	target.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	target.StartTLS()
	t.Cleanup(target.Close)

	proxy := newConnectProxy(t, nil)

	// The uppercase value is written LAST: Windows environment names are
	// case-insensitive, so writing https_proxy="" after HTTPS_PROXY would
	// blank the variable this test just set. t.Setenv has no unset form, so
	// the empty writes come first and the real value wins on every platform.
	t.Setenv("https_proxy", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	t.Setenv("HTTPS_PROXY", proxy.srv.URL)

	client, err := New(Options{CACert: string(certPEM)})
	require.NoError(t, err)
	tr := client.Transport.(*http.Transport)
	tr.DisableKeepAlives = true
	tr.Proxy = asTransportProxy(httpproxy.FromEnvironment().ProxyFunc())

	// Bounded so a sandboxed runner that reports a non-loopback IPv4 address
	// but firewalls self-connects to it fails fast instead of hanging on a
	// silently dropped SYN.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	require.NoError(t, err)
	resp, body, err := Do(client, req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ok", string(body))

	assert.Equal(t, []string{target.Listener.Addr().String()}, proxy.tunneledTargets())
}
