package provider

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rancherVersionProxyTargetHost is a hostname no real DNS resolves, standing
// in for an internal Rancher server reached through a corporate proxy.
// httpproxy's "never proxy a loopback address" rule only inspects IP
// literals, so a hostname target stays subject to normal proxy routing even
// though sharedConnectProxy actually tunnels it to a 127.0.0.1 fixture.
const rancherVersionProxyTargetHost = "rancherversion-proxy-target.internal.test"

// connectProxy is a minimal CONNECT-only forward proxy: it hijacks the
// client connection and pipes bytes to rancherVersionProxyTargetHost's real
// loopback address, recording every host:port it was asked to tunnel to.
type connectProxy struct {
	srv *httptest.Server

	mu     sync.Mutex
	target string // real loopback address for rancherVersionProxyTargetHost, set per test
	seen   []string
}

func newConnectProxy() *connectProxy {
	p := &connectProxy{}
	p.srv = httptest.NewServer(http.HandlerFunc(p.handleConnect))
	return p
}

// setTarget points rancherVersionProxyTargetHost at addr for the duration of
// the caller's test - TestMain starts one proxy for the whole binary, and
// each test using it runs its own target server on a fresh loopback port.
func (p *connectProxy) setTarget(t *testing.T, addr string) {
	t.Helper()
	p.mu.Lock()
	p.target = addr
	p.mu.Unlock()
	t.Cleanup(func() {
		p.mu.Lock()
		p.target = ""
		p.seen = nil
		p.mu.Unlock()
	})
}

func (p *connectProxy) tunneledTargets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func (p *connectProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "connect only", http.StatusMethodNotAllowed)
		return
	}

	p.mu.Lock()
	p.seen = append(p.seen, r.Host)
	real := p.target
	p.mu.Unlock()

	host, _, err := net.SplitHostPort(r.Host)
	if err != nil || host != rancherVersionProxyTargetHost || real == "" {
		http.Error(w, "no route", http.StatusBadGateway)
		return
	}

	dst, err := net.Dial("tcp", real)
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

var sharedConnectProxyForTests *connectProxy

// startSharedEnvtest and stopSharedEnvtest are no-ops in the default build.
// The envtest-tagged test file replaces them so TestMain starts its one
// control plane before any test runs and stops it after the last one.
var (
	startSharedEnvtest = func() {}
	stopSharedEnvtest  = func() {}
)

// TestMain starts one CONNECT proxy fixture and points HTTPS_PROXY at it for
// the whole binary. net/http's ProxyFromEnvironment caches its environment
// read behind a private, unresettable sync.Once, so a per-test t.Setenv
// would arrive too late once any earlier test's request has already
// resolved a proxy decision - setting it once here, before any test runs,
// makes that first resolution the one this package wants. Every other test
// in this package targets a loopback (127.0.0.1) httptest server, which
// httpproxy never proxies regardless of this setting (see
// TestProxyFuncNeverProxiesLoopbackTarget in the httpx package), so only
// rancherVersionProxyTargetHost - a real hostname, not a loopback literal -
// actually routes through the fixture.
//
// Proxy variables inherited from the environment are cleared with
// Unsetenv, never by writing an empty value: Windows environment names are
// case-insensitive, so writing https_proxy="" after HTTPS_PROXY would blank
// the variable this test just set. Unsetting every spelling first and
// writing the uppercase value last is correct on every platform.
func TestMain(m *testing.M) {
	sharedConnectProxyForTests = newConnectProxy()
	for _, k := range []string{"https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"} {
		_ = os.Unsetenv(k)
	}
	_ = os.Setenv("HTTPS_PROXY", sharedConnectProxyForTests.srv.URL)
	startSharedEnvtest()
	code := m.Run()
	stopSharedEnvtest()
	os.Exit(code)
}

// TestRancherVersionTraversesConnectProxyWithPinnedCA asserts the login
// preflight's real network path (RancherVersion, called by every HTTPS
// `auth login`, including --token-stdin, before any credential is
// persisted): behind an HTTPS_PROXY, its GET /rancherversion tunnels through
// a CONNECT proxy and still verifies the target's pinned CA.
func TestRancherVersionTraversesConnectProxyWithPinnedCA(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: rancherVersionProxyTargetHost},
		DNSNames:     []string{rancherVersionProxyTargetHost},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != rancherVersionPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Version":"v2.15.0"}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	sharedConnectProxyForTests.setTarget(t, srv.Listener.Addr().String())

	_, targetPort, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	eff := config.Effective{
		URL:   "https://" + net.JoinHostPort(rancherVersionProxyTargetHost, targetPort),
		CA:    string(certPEM),
		Token: "id:secret",
	}

	got, err := RancherVersion(t.Context(), eff)
	require.NoError(t, err)
	assert.Equal(t, "v2.15.0", got)

	assert.Equal(t, []string{net.JoinHostPort(rancherVersionProxyTargetHost, targetPort)}, sharedConnectProxyForTests.tunneledTargets())
}
