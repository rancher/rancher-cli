package token

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func certPEM(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

// distinctLoopbackCert issues a fresh self-signed certificate for 127.0.0.1.
// Every httptest TLS server otherwise presents the same built-in
// certificate, so two servers would share trust by accident and a
// split-origin test could pass for the wrong reason.
func distinctLoopbackCert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "idp-fixture"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// idpDeviceStub is a minimal RFC 8628 device-authorization identity
// provider, standing in for testrancher's facade IdP stub: the token
// package cannot import testrancher (it imports token for a path
// constant), so the split-origin fixtures below build their own.
type idpDeviceStub struct {
	pendingTotal int

	mu          sync.Mutex
	counter     int
	pendingLeft int
	deviceCode  string
	idToken     string
}

func (s *idpDeviceStub) handleDeviceCode(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.counter++
	n := s.counter
	s.deviceCode = fmt.Sprintf("idp-devicecode-%d", n)
	s.idToken = fmt.Sprintf("idp-idtoken-%d", n)
	s.pendingLeft = s.pendingTotal
	deviceCode := s.deviceCode
	s.mu.Unlock()

	writeTestJSON(w, http.StatusOK, map[string]any{
		"device_code":      deviceCode,
		"user_code":        fmt.Sprintf("IDP-%d", n),
		"verification_uri": "https://" + r.Host + "/idp/verify",
		"expires_in":       600,
		"interval":         1,
	})
}

func (s *idpDeviceStub) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if r.FormValue("grant_type") == "authorization_code" {
		code := r.FormValue("code")
		s.mu.Lock()
		defer s.mu.Unlock()
		if code == "" {
			writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		s.counter++
		s.idToken = fmt.Sprintf("idp-idtoken-%d", s.counter)
		writeTestJSON(w, http.StatusOK, map[string]any{
			"access_token": "idp-access-" + s.idToken,
			"token_type":   "bearer",
			"expires_in":   300,
			"id_token":     s.idToken,
		})
		return
	}

	deviceCode := r.FormValue("device_code")
	s.mu.Lock()
	defer s.mu.Unlock()
	if deviceCode == "" || deviceCode != s.deviceCode {
		writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	if s.pendingLeft > 0 {
		s.pendingLeft--
		writeTestJSON(w, http.StatusOK, map[string]any{"error": "authorization_pending"})
		return
	}
	writeTestJSON(w, http.StatusOK, map[string]any{
		"access_token": "idp-access-" + s.idToken,
		"token_type":   "bearer",
		"expires_in":   300,
		"id_token":     s.idToken,
	})
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// splitOrigin is one Rancher-on-A, IdP-on-B fixture pair: the Rancher
// client is pinned to A's CA, the IdP client to B's own certificate
// (standing in for OS roots in a unit test), and idpRequests counts only
// this fixture's IdP traffic so parallel tests never share a counter.
// rancherURL and idpURL are the origins the flow is pointed at: the
// servers' URLs on the loopback network, the logical origins in memory.
type splitOrigin struct {
	rancher, idp             *httptest.Server
	rancherURL, idpURL       string
	rancherClient, idpClient *http.Client
	// rejectLogin makes the Rancher login answer 401, as Rancher does when
	// the identity provider's token is not one it accepts.
	rejectLogin bool
	idpRequests atomic.Int32
}

// splitOriginFacade starts a minimal Rancher fixture on TLS server A,
// naming an azureADProvider document that points at TLS server B, its own
// device-code identity-provider stub with a distinct certificate.
func splitOriginFacade(t *testing.T) *splitOrigin {
	t.Helper()
	return newSplitOrigin(t, false)
}

// splitOriginFacadeInMemory is splitOriginFacade for a synctest bubble:
// both origins on the in-memory network behind one host-dispatching
// transport, so the flow's polling runs on the fake clock. The two
// clients share that transport and its trust, so the tests of TLS
// distinctness between the origins keep the loopback fixture.
func splitOriginFacadeInMemory(t *testing.T) *splitOrigin {
	t.Helper()
	return newSplitOrigin(t, true)
}

func newSplitOrigin(t *testing.T, inMemory bool) *splitOrigin {
	t.Helper()
	so := &splitOrigin{}
	idpStub := &idpDeviceStub{pendingTotal: 1}
	idpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		so.idpRequests.Add(1)
		switch r.URL.Path {
		case "/idp/devicecode":
			idpStub.handleDeviceCode(w, r)
		case "/idp/token":
			idpStub.handleToken(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	rancherHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1-public/authproviders":
			writeTestJSON(w, http.StatusOK, map[string]any{"data": []map[string]any{
				{"type": "localProvider"},
				{
					"type":          "azureADProvider",
					"clientId":      "facade-client",
					"scopes":        []string{"openid"},
					"authUrl":       so.idpURL + "/idp/authorize",
					"deviceAuthUrl": so.idpURL + "/idp/devicecode",
					"tokenUrl":      so.idpURL + "/idp/token",
				},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1-public/login":
			if so.rejectLogin {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			var body struct {
				IDToken string `json:"id_token"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			if body.IDToken == "" {
				http.Error(w, "missing id_token", http.StatusBadRequest)
				return
			}
			name := "rancher-session-" + body.IDToken
			writeTestJSON(w, http.StatusCreated, map[string]any{"id": name, "token": name + ":s"})
		default:
			http.NotFound(w, r)
		}
	})

	if inMemory {
		so.idp = httptest.NewTestServer(t, idpHandler)
		so.idpURL = idpOrigin
		so.rancher = httptest.NewTestServer(t, rancherHandler)
		so.rancherURL = rancherOrigin
		m := hostTransport{hostOf(rancherOrigin): so.rancher.Client().Transport, hostOf(idpOrigin): so.idp.Client().Transport}
		so.rancherClient, so.idpClient = m.client(), m.client()
		return so
	}

	so.idp = httptest.NewUnstartedServer(idpHandler)
	so.idp.TLS = &tls.Config{Certificates: []tls.Certificate{distinctLoopbackCert(t)}}
	so.idp.StartTLS()
	t.Cleanup(so.idp.Close)
	so.idpURL = so.idp.URL

	so.rancher = httptest.NewTLSServer(rancherHandler)
	t.Cleanup(so.rancher.Close)
	so.rancherURL = so.rancher.URL

	var err error
	so.rancherClient, err = httpx.New(httpx.Options{CACert: certPEM(t, so.rancher), Timeout: httpx.DefaultTimeout})
	require.NoError(t, err)
	so.idpClient, err = httpx.New(httpx.Options{CACert: certPEM(t, so.idp), Timeout: httpx.DefaultTimeout})
	require.NoError(t, err)
	return so
}

// interactivePrompter allows the device-code flow past oauthAuth's NoInput
// gate without reading a terminal: the flow only prints a notice, never
// reads a line.
func interactivePrompter() *ui.Prompter {
	return ui.New(io.Discard, strings.NewReader(""), false, false)
}

// TestDeviceCodeFlowSucceedsAcrossSplitOrigins asserts the topology this flow
// exists for: Rancher on one self-signed origin, the identity provider on
// another, each verified by its own client, and the flow completes.
func TestDeviceCodeFlowSucceedsAcrossSplitOrigins(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		so := splitOriginFacadeInMemory(t)
		start := time.Now()
		cred, err := AcquireLoginCredential(t.Context(), so.rancherClient, LoginRequest{
			CAFlag: "--cacert",
			Server: so.rancherURL, AuthProvider: "azureADProvider", AuthFlow: deviceAuthFlow, IdPClient: so.idpClient,
		}, 0, interactivePrompter())
		require.NoError(t, err)
		assert.NotEmpty(t, cred.Token)
		// One device-authorization request, one pending poll, one token issue.
		assert.Equal(t, int32(3), so.idpRequests.Load())
		assert.Equal(t, time.Second, time.Since(start), "the first poll is immediate and answered pending; the issuing poll came one stub interval later on the fake clock")
	})
}

// TestAuthCodeFlowSucceedsAcrossSplitOrigins asserts the same topology for
// the auth-code flow: the token exchange reaches the IdP origin through the
// injected client, the Rancher login reaches the Rancher origin, and the
// browser stub drives the loopback callback directly with the generated
// state, so no authorization endpoint is needed.
func TestAuthCodeFlowSucceedsAcrossSplitOrigins(t *testing.T) {
	t.Parallel()
	so := splitOriginFacade(t)
	openBrowserStub := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		resp, err := ownTransportClient(t).Get(q.Get("redirect_uri") + "?code=c-1&state=" + url.QueryEscape(q.Get("state")))
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}
	providers, _, _, err := getAuthProviders(t.Context(), so.rancherClient, so.rancherURL)
	require.NoError(t, err)
	provider, err := selectAuthProvider(t.Context(), providers, nil, "azureADProvider", false, ui.Discard())
	require.NoError(t, err)
	input := &loginInput{caFlag: "--cacert", server: so.rancherURL, authProvider: "azureADProvider", idpClient: so.idpClient}
	tok, err := oauthAuthCodeAuth(t.Context(), so.rancherClient, input, provider, 5*time.Second, openBrowserStub, ui.Discard())
	require.NoError(t, err)
	assert.NotEmpty(t, tok.Bearer)
	assert.Equal(t, int32(1), so.idpRequests.Load(), "exactly one token exchange reaches this fixture's IdP")
}

// TestDeviceCodeFlowWithRancherClientAsIdPClientFailsUnknownAuthority asserts
// the leak this flow closes from the other side: dialing the IdP with the
// Rancher-pinned client fails as an IdP certificate error, never TLS_TRUST.
func TestDeviceCodeFlowWithRancherClientAsIdPClientFailsUnknownAuthority(t *testing.T) {
	t.Parallel()
	so := splitOriginFacade(t)
	_, err := AcquireLoginCredential(t.Context(), so.rancherClient, LoginRequest{
		CAFlag: "--cacert",
		Server: so.rancherURL, AuthProvider: "azureADProvider", AuthFlow: deviceAuthFlow, IdPClient: so.rancherClient,
	}, 0, interactivePrompter())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	require.ErrorContains(t, err, "identity provider 127.0.0.1: certificate not trusted by the operating system trust store")
	assert.Zero(t, so.idpRequests.Load(), "the handshake fails before any request is served")
}

// TestDeviceCodeFlowNilIdPClientIsError asserts that an OAuth path without an
// IdP client fails closed instead of falling back to the Rancher client.
func TestDeviceCodeFlowNilIdPClientIsError(t *testing.T) {
	t.Parallel()
	so := splitOriginFacade(t)
	_, err := AcquireLoginCredential(t.Context(), so.rancherClient, LoginRequest{
		CAFlag: "--cacert",
		Server: so.rancherURL, AuthProvider: "azureADProvider", AuthFlow: deviceAuthFlow,
	}, 0, interactivePrompter())
	require.ErrorContains(t, err, "identity provider client is not configured")
}

// TestValidateOAuthEndpoint asserts the endpoint gate per field and case, and
// that the rendered value never carries path, query, or userinfo.
func TestValidateOAuthEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, raw, rendered string
		ok                  bool
	}{
		{"https", "https://login.example/oauth2/token?x=1", "", true},
		{"http", "http://login.example/oauth2/token", "http://login.example", false},
		{"opaque", "https:opaque", "unparseable", false},
		{"empty host", "https:///path", "unparseable", false},
		{"userinfo", "https://user:secret@login.example/token", "https://login.example", false},
		{"malformed port", "https://login.example:abc/", "unparseable", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, field := range []string{"authUrl", "deviceAuthUrl", "tokenUrl"} {
				err := validateOAuthEndpoint(field, tc.raw)
				if tc.ok {
					require.NoError(t, err)
					continue
				}
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeAuth, ce.Code)
				require.ErrorContains(t, err, "identity provider endpoint "+field+" must be an https URL with a host and no credentials, got "+tc.rendered)
				assert.NotContains(t, err.Error(), "secret")
				assert.NotContains(t, err.Error(), "/token")
			}
		})
	}
}

// ownTransportClient returns a client with a transport of its own, closed
// when the test ends. A test uses it where no httptest server's client fits:
// a plain-HTTP listener that no httptest server owns (the CLI's loopback
// OAuth callback), a target with no server, or a server whose certificate
// the client must not trust. It never uses http.DefaultTransport: closing
// any httptest server closes that transport's idle connections.
func ownTransportClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// failingTransportClient returns a client that fails the test if a request
// is sent through it.
func failingTransportClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: failingTransport{t}}
}

// countingTransport records how many requests reach it and refuses each.
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errors.New("unexpected request")
}

// TestEndpointGateStopsBothFlowsBeforeSideEffects asserts the gate's wiring
// and ordering per field and per rejected shape: exactly one endpoint is
// invalid while the other two are valid https, and neither flow opens a
// browser or sends a request.
func TestEndpointGateStopsBothFlowsBeforeSideEffects(t *testing.T) {
	t.Parallel()
	bad := map[string]string{
		"http":           "http://login.example/x",
		"opaque":         "https:opaque",
		"empty host":     "https:///path",
		"userinfo":       "https://user:secret@login.example/x",
		"malformed port": "https://login.example:abc/x",
	}
	for _, field := range []string{"authUrl", "deviceAuthUrl", "tokenUrl"} {
		for shape, value := range bad {
			t.Run(field+" "+shape, func(t *testing.T) {
				t.Parallel()
				ep := OAuthEndpoint{AuthURL: "https://a.example/authorize", DeviceAuthURL: "https://a.example/devicecode", TokenURL: "https://a.example/token"}
				switch field {
				case "authUrl":
					ep.AuthURL = value
				case "deviceAuthUrl":
					ep.DeviceAuthURL = value
				case "tokenUrl":
					ep.TokenURL = value
				}
				provider := &AzureADProvider{ClientID: "c", Scopes: []string{"openid"}, OAuthEndpoint: ep}
				rt := &countingTransport{}
				input := &loginInput{caFlag: "--cacert", server: "https://rancher.example", authProvider: "azureADProvider", idpClient: &http.Client{Transport: rt}}
				var opened atomic.Int32
				_, err := oauthAuthCodeAuth(t.Context(), &http.Client{Transport: rt}, input, provider, time.Second, func(string) error { opened.Add(1); return nil }, ui.Discard())
				require.ErrorContains(t, err, "identity provider endpoint "+field+" must be an https URL")
				_, err = oauthDeviceCodeAuth(t.Context(), &http.Client{Transport: rt}, input, provider, ui.Discard())
				require.ErrorContains(t, err, "identity provider endpoint "+field+" must be an https URL")
				assert.Zero(t, opened.Load(), "the browser must never open")
				assert.Zero(t, rt.n.Load(), "no request may be sent")
			})
		}
	}
}

// TestNewOauthConfigAcceptsEndpointsOnDifferentOrigins asserts that the three
// endpoints are not required to share an origin.
func TestNewOauthConfigAcceptsEndpointsOnDifferentOrigins(t *testing.T) {
	t.Parallel()
	provider := &AzureADProvider{
		ClientID: "c",
		AuthURL:  "https://a.example/authorize", DeviceAuthURL: "https://b.example/devicecode", TokenURL: "https://c.example/token"}
	cfg, err := newOauthConfig(provider)
	require.NoError(t, err)
	assert.Equal(t, "https://c.example/token", cfg.Endpoint.TokenURL)
}

// idpFixture starts a TLS server presenting a leaf signed by a fresh CA
// with the given name and validity, and returns the server plus a client
// whose root pool holds that CA (so the verifier reports the specific kind).
func idpFixture(t *testing.T, dnsName string, notBefore, notAfter time.Time, trustCA, clientAuthOnly bool) (srv *httptest.Server, client *http.Client) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: dnsName}, DNSNames: []string{dnsName},
		NotBefore: notBefore, NotAfter: notAfter}
	if clientAuthOnly {
		// Presented by a server, a client-auth-only leaf fails verification
		// with CertificateInvalidError{IncompatibleUsage}: the deterministic
		// invalid-chain case.
		leafTmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	require.NoError(t, err)
	srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	if trustCA {
		roots.AddCert(ca)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "idp.example"}}
	return srv, &http.Client{Transport: tr, CheckRedirect: httpx.RejectCrossOriginRedirect}
}

// TestIdPCertificateErrorPerKind asserts the per-kind wording: only unknown
// authority mentions the trust store, no kind is TLS_TRUST, and the
// remediation never says to add a CA for a failure a CA cannot fix.
func TestIdPCertificateErrorPerKind(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		name, dns      string
		nb, na         time.Time
		trust          bool
		clientAuthOnly bool
		want           string
	}{
		{"unknown authority", "idp.example", now.Add(-time.Hour), now.Add(time.Hour), false, false, "certificate not trusted by the operating system trust store"},
		{"hostname mismatch", "other.example", now.Add(-time.Hour), now.Add(time.Hour), true, false, "certificate is not valid for this host name"},
		{"expired", "idp.example", now.Add(-2 * time.Hour), now.Add(-time.Hour), true, false, "certificate has expired"},
		{"not yet valid", "idp.example", now.Add(time.Hour), now.Add(2 * time.Hour), true, false, "certificate is not yet valid"},
		{"invalid chain", "idp.example", now.Add(-time.Hour), now.Add(time.Hour), true, true, "certificate chain is invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, client := idpFixture(t, tc.dns, tc.nb, tc.na, tc.trust, tc.clientAuthOnly)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/token", nil)
			require.NoError(t, err)
			_, dialErr := client.Do(req)
			require.Error(t, dialErr)
			got := idpCertificateError(dialErr, srv.URL+"/token", "--cacert")
			ce, ok := errors.AsType[*clierr.Error](got)
			require.True(t, ok, "got %v", got)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			require.ErrorContains(t, got, "identity provider 127.0.0.1: "+tc.want)
			if tc.name != "unknown authority" {
				assert.NotContains(t, got.Error(), "trust store")
			}
		})
	}
}

// TestIdPCertificateErrorPassesThroughOtherErrors asserts that a non-certificate
// failure and a certificate failure at another origin are returned as is.
func TestIdPCertificateErrorPassesThroughOtherErrors(t *testing.T) {
	t.Parallel()
	plain := errors.New("dial tcp: connection refused")
	assert.Same(t, plain, idpCertificateError(plain, "https://idp.example/token", "--cacert"))
	srv, client := idpFixture(t, "idp.example", time.Now().Add(-time.Hour), time.Now().Add(time.Hour), false, false)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/token", nil)
	require.NoError(t, err)
	_, dialErr := client.Do(req)
	require.Error(t, dialErr)
	assert.Same(t, dialErr, idpCertificateError(dialErr, "https://other.example/token", "--cacert"))
}

// TestDeviceCodeFlowRancherRejectionIsAnAuthError asserts that an OAuth login
// Rancher rejects exits with the AUTH code, as a rejected basic login does:
// a scripted caller reading .error.code must see the same class for the
// same fact, whichever flow produced it.
func TestDeviceCodeFlowRancherRejectionIsAnAuthError(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		so := splitOriginFacadeInMemory(t)
		so.rejectLogin = true
		_, err := AcquireLoginCredential(t.Context(), so.rancherClient, LoginRequest{
			CAFlag: "--cacert",
			Server: so.rancherURL, AuthProvider: "azureADProvider", AuthFlow: deviceAuthFlow, IdPClient: so.idpClient,
		}, 0, interactivePrompter())
		require.Error(t, err)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		require.ErrorContains(t, err, "login rejected: 401")
		_, isAcquire := errors.AsType[*AcquireError](err)
		assert.False(t, isAcquire, "a deterministic rejection created nothing; there is no cleanup to report")
	})
}
