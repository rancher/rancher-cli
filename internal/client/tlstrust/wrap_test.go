package tlstrust

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slotRemedy puts a distinct marker in each Remedy field, so a message shows
// which slot each kind of failure reads.
var slotRemedy = Remedy{CA: "; CA-SLOT", URL: "URL-SLOT", Fix: "FIX-SLOT"}

// wrapFixture is a TLS server presenting leaf at https://localhost:<port>.
type wrapFixture struct {
	srv    *httptest.Server
	origin httpx.Origin
	url    string
}

func startWrapFixture(t *testing.T, leaf tls.Certificate) wrapFixture {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	raw := "https://localhost:" + port
	return wrapFixture{srv: srv, origin: originOf(t, raw), url: raw}
}

// wrappedClient is httpx.New with the classifying wrapper installed for
// server, trusting caPEM: the construction every production caller uses.
func wrappedClient(t *testing.T, server httpx.Origin, caPEM string, r Remedy) *http.Client {
	t.Helper()
	client, err := httpx.New(httpx.Options{CACert: caPEM, Timeout: 5 * time.Second, WrapTransport: WrapTransport(server, r)})
	require.NoError(t, err)
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func certPEM(c *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
}

// failureKinds is one real failed handshake per kind, each against a
// localhost server: the CA is trusted except for unknown authority.
func failureKinds(t *testing.T) []struct {
	name   string
	kind   Kind
	leaf   tls.Certificate
	caPEM  string
	phrase string
} {
	t.Helper()
	now := time.Now()
	ca, caKey := genCA(t, "wrap-ca")
	other, _ := genCA(t, "another-ca")
	return []struct {
		name   string
		kind   Kind
		leaf   tls.Certificate
		caPEM  string
		phrase string
	}{
		{"unknown authority", KindUnknownAuthority, genLeaf(t, ca, caKey, "localhost", now.Add(-time.Hour), now.Add(time.Hour)), certPEM(other),
			"(or confirm the fingerprint shown here with the server's administrator); CA-SLOT; if this server worked before"},
		{"hostname mismatch", KindHostnameMismatch, genLeaf(t, ca, caKey, "other.example", now.Add(-time.Hour), now.Add(time.Hour)), certPEM(ca),
			"check that URL-SLOT matches the certificate's hostname; FIX-SLOT cannot fix a hostname mismatch"},
		{"expired", KindExpired, genLeaf(t, ca, caKey, "localhost", now.Add(-2*time.Hour), now.Add(-time.Hour)), certPEM(ca),
			"ask the server's administrator to renew it; FIX-SLOT cannot fix an expired certificate"},
		{"not yet valid", KindNotYetValid, genLeaf(t, ca, caKey, "localhost", now.Add(time.Hour), now.Add(2*time.Hour)), certPEM(ca),
			"ask the server's administrator to reissue it; FIX-SLOT cannot fix this"},
		{"invalid chain", KindInvalidChain, genClientAuthOnlyLeaf(t, ca, caKey, "localhost"), certPEM(ca),
			"the chain is malformed or incomplete; contact the server's administrator"},
	}
}

// TestWrapTransportClassifiesEachKindAndSlot asserts a certificate failure
// of a request to the server origin is a TLS_TRUST error of the right kind,
// exit 10 and not retryable, that each Remedy field lands in the message of
// the kinds that read it, and that the transport error stays reachable.
// Mutations: no origin check is not caught here; each Remedy field written
// into another kind's message fails the phrase check; dropping the cause
// fails the CertificateVerificationError check.
func TestWrapTransportClassifiesEachKindAndSlot(t *testing.T) {
	for _, tc := range failureKinds(t) {
		t.Run(tc.name, func(t *testing.T) {
			fx := startWrapFixture(t, tc.leaf)
			client := wrappedClient(t, fx.origin, tc.caPEM, slotRemedy)
			_, err := client.Get(fx.url)
			require.Error(t, err)

			ce := TypedError(err)
			require.NotNil(t, ce, "got %v", err)
			assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
			assert.Equal(t, 10, ce.ExitCode())
			assert.False(t, ce.Retryable)
			assert.Equal(t, tc.kind, ce.Details.(Details).Kind)
			assert.Contains(t, ce.Message, tc.phrase)

			_, ok := errors.AsType[*tls.CertificateVerificationError](err)
			assert.True(t, ok, "the transport error stays reachable")
			if tc.kind == KindInvalidChain {
				for _, slot := range []string{"CA-SLOT", "URL-SLOT", "FIX-SLOT"} {
					assert.NotContains(t, ce.Message, slot, "an invalid chain names no flag")
				}
			}
		})
	}
}

// TestWrapTransportLeavesOtherOriginsAlone asserts a certificate failure of
// a request to an origin other than the wrapped server's stays the
// transport error. Mutation: no origin check.
func TestWrapTransportLeavesOtherOriginsAlone(t *testing.T) {
	now := time.Now()
	ca, caKey := genCA(t, "wrap-ca")
	other, _ := genCA(t, "another-ca")
	fx := startWrapFixture(t, genLeaf(t, ca, caKey, "localhost", now.Add(-time.Hour), now.Add(time.Hour)))
	elsewhere := originOf(t, "https://rancher.example:8443")
	client := wrappedClient(t, elsewhere, certPEM(other), slotRemedy)

	_, err := client.Get(fx.url)
	require.Error(t, err)
	assert.Nil(t, TypedError(err), "another origin is never attributed to the server")
	_, ok := errors.AsType[*tls.CertificateVerificationError](err)
	assert.True(t, ok)
}

// TestWrapTransportLeavesOtherErrorsAndSuccessAlone asserts a connection
// failure that is not a certificate failure and a successful request pass
// through untouched. Mutation: classify without requiring a certificate
// failure (a non-certificate error becomes TLS_TRUST or is replaced).
func TestWrapTransportLeavesOtherErrorsAndSuccessAlone(t *testing.T) {
	now := time.Now()
	ca, caKey := genCA(t, "wrap-ca")
	fx := startWrapFixture(t, genLeaf(t, ca, caKey, "localhost", now.Add(-time.Hour), now.Add(time.Hour)))
	client := wrappedClient(t, fx.origin, certPEM(ca), slotRemedy)

	resp, err := client.Get(fx.url)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	fx.srv.Close()
	client.CloseIdleConnections()
	_, err = client.Get(fx.url)
	require.Error(t, err)
	assert.Nil(t, TypedError(err))
	_, isOpErr := errors.AsType[*net.OpError](err)
	assert.True(t, isOpErr, "a refused connection keeps its transport error")
}

// TestWrapTransportNeverClassifiesAnHTTPSProxy asserts that a failure to
// verify an https proxy, which Go reports under the Rancher request URL
// with the client's own roots, is not attributed to the server. Mutation:
// no proxy exclusion.
func TestWrapTransportNeverClassifiesAnHTTPSProxy(t *testing.T) {
	now := time.Now()
	ca, caKey := genCA(t, "wrap-ca")
	proxy := startWrapFixture(t, genLeaf(t, ca, caKey, "localhost", now.Add(-time.Hour), now.Add(time.Hour)))
	target := originOf(t, "https://rancher.example:8443")
	proxyURL, err := url.Parse(proxy.url)
	require.NoError(t, err)

	transport := &http.Transport{
		Proxy:           func(*http.Request) (*url.URL, error) { return proxyURL, nil },
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: x509.NewCertPool()},
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: WrapTransport(target, slotRemedy)(transport), Timeout: 5 * time.Second}

	_, err = client.Get(target.String())
	require.Error(t, err)
	opErr, isOpErr := errors.AsType[*net.OpError](err)
	require.True(t, isOpErr)
	require.Equal(t, "proxyconnect", opErr.Op)
	assert.Nil(t, TypedError(err), "the proxy's certificate is not the server's")
}

// TestWrapTransportForNeedsAUsableOrigin asserts a URL that has no origin
// yields no wrapper, and a usable one yields a wrapper that classifies.
// Mutation: return a wrapper for an unparsable URL, or none for a good one.
func TestWrapTransportForNeedsAUsableOrigin(t *testing.T) {
	assert.Nil(t, WrapTransportFor("", Remedy{}))
	assert.Nil(t, WrapTransportFor("ftp://rancher.example", Remedy{}))
	assert.Nil(t, WrapTransportFor("https://rancher.example:bad", Remedy{}))

	now := time.Now()
	ca, caKey := genCA(t, "wrap-ca")
	other, _ := genCA(t, "another-ca")
	fx := startWrapFixture(t, genLeaf(t, ca, caKey, "localhost", now.Add(-time.Hour), now.Add(time.Hour)))
	wrap := WrapTransportFor(fx.url, Remedy{})
	require.NotNil(t, wrap)
	client, err := httpx.New(httpx.Options{CACert: certPEM(other), Timeout: 5 * time.Second, WrapTransport: wrap})
	require.NoError(t, err)
	_, err = client.Get(fx.url)
	require.Error(t, err)
	assert.NotNil(t, TypedError(err))
}

// TestTypedErrorFindsTheTLSTrustErrorInAChain asserts TypedError returns the
// TLS_TRUST error through wrapping and nothing for any other error.
// Mutation: ignore the code, or stop at the first wrapper.
func TestTypedErrorFindsTheTLSTrustErrorInAChain(t *testing.T) {
	trust := clierr.New(clierr.CodeTLSTrust, "untrusted")
	assert.Same(t, trust, TypedError(trust))
	assert.Same(t, trust, TypedError(&url.Error{Op: "Get", URL: "https://x", Err: trust}))
	assert.Same(t, trust, TypedError(fmt.Errorf("resolving: %w", trust)))
	assert.Nil(t, TypedError(nil))
	assert.Nil(t, TypedError(errors.New("boom")))
	assert.Nil(t, TypedError(clierr.New(clierr.CodeGeneric, "boom")))
}

// TestClassifyZeroRemedyKeepsTheLoginWording asserts the zero Remedy yields
// exactly the message each kind had before the wording became per command.
// Mutation: change any default string of Remedy.
func TestClassifyZeroRemedyKeepsTheLoginWording(t *testing.T) {
	for _, tc := range failureKinds(t) {
		t.Run(tc.name, func(t *testing.T) {
			fx := startWrapFixture(t, tc.leaf)
			plain, err := httpx.New(httpx.Options{CACert: tc.caPEM, Timeout: 5 * time.Second})
			require.NoError(t, err)
			_, err = plain.Get(fx.url)
			require.Error(t, err)
			ce := Classify(err, "rancher.test", time.Now(), Remedy{})
			require.NotNil(t, ce)

			want := map[Kind]string{
				KindUnknownAuthority: "the server presented a certificate signed by an unknown authority; " +
					"obtain its CA from a source you trust (or confirm the fingerprint shown here with the server's administrator) and pass it with --cacert" +
					"; if this server worked before, check the subject and issuer shown here - " +
					"a certificate you don't recognize can mean something between you and the server, such as a proxy, is replacing its certificate",
				KindHostnameMismatch: "the server's certificate is not valid for rancher.test" +
					"; check that --url matches the certificate's hostname; --cacert cannot fix a hostname mismatch",
				KindExpired:      "the server's certificate has expired; ask the server's administrator to renew it; --cacert cannot fix an expired certificate",
				KindNotYetValid:  "the server's certificate is not yet valid; check the local clock, or ask the server's administrator to reissue it; --cacert cannot fix this",
				KindInvalidChain: "the server's certificate chain failed validation; the chain is malformed or incomplete; contact the server's administrator",
			}
			assert.Equal(t, want[tc.kind], ce.Message)
		})
	}
}

// legacyCancelTransport waits on the request's Cancel channel when it has
// one, as a transport that sees both it and the context deadline may, and
// on the context otherwise.
type legacyCancelTransport struct{}

func (legacyCancelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cancel := req.Cancel //nolint:staticcheck // models the transport that sees the channel http.Client sets
	if cancel != nil {
		<-cancel
		return nil, errors.New("net/http: request canceled")
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// TestWrapTransportKeepsTheClientTimeoutADeadlineError asserts a client that
// times out through the wrapper reports the deadline, as it does without
// the wrapper, and not the legacy cancellation the Client would otherwise
// signal through Request.Cancel. Mutation: forward the request with its
// Cancel channel.
func TestWrapTransportKeepsTheClientTimeoutADeadlineError(t *testing.T) {
	client := &http.Client{
		Transport: WrapTransport(originOf(t, "https://rancher.example"), Remedy{})(legacyCancelTransport{}),
		Timeout:   50 * time.Millisecond,
	}
	_, err := client.Get("https://rancher.example/slow")
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), "request canceled")
}

// TestWrapTransportComparesTheWholeOrigin asserts a certificate failure of a
// request to the server's host on another port stays the transport error:
// the origin is scheme, host and port. Mutation: compare hostnames only.
func TestWrapTransportComparesTheWholeOrigin(t *testing.T) {
	now := time.Now()
	ca, caKey := genCA(t, "wrap-ca")
	other, _ := genCA(t, "another-ca")
	fx := startWrapFixture(t, genLeaf(t, ca, caKey, "localhost", now.Add(-time.Hour), now.Add(time.Hour)))
	u, err := url.Parse(fx.url)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	sameHostOtherPort := originOf(t, "https://"+net.JoinHostPort(u.Hostname(), strconv.Itoa(port+1)))
	client := wrappedClient(t, sameHostOtherPort, certPEM(other), slotRemedy)

	_, err = client.Get(fx.url)
	require.Error(t, err)
	assert.Nil(t, TypedError(err), "another port is another origin")
}
