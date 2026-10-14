package tlstrust

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// genCA returns a self-signed CA certificate and its key.
func genCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert, priv
}

// genLeaf issues a server leaf certificate signed by ca/caKey, valid for dns.
func genLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dns string, notBefore, notAfter time.Time) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dns},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		DNSNames:     []string{dns},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &priv.PublicKey, caKey)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: priv}
}

// selfSignedLeaf returns a leaf certificate that signs itself (no CA), the
// dynamiclistener-style shape. san is its one subject alternative name: an
// IP address becomes an IP SAN, anything else a DNS SAN.
func selfSignedLeaf(t *testing.T, san string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: san},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	if ip := net.ParseIP(san); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{san}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, cert
}

// dialFail starts an httptest TLS server presenting leafCert, issues a GET
// against it with an empty trust store (or explicit roots) and an explicit
// SNI/hostname-check name, and returns the resulting client.Do error - the
// real failed-handshake error this package classifies. serverName is set
// explicitly (rather than left to default to the dial address's literal IP)
// so hostname verification checks against a cert's DNSNames rather than
// tripping the unrelated "no IP SANs" case.
func dialFail(t *testing.T, leafCert tls.Certificate, roots *x509.CertPool, serverName string) error {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leafCert}}
	srv.StartTLS()
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: serverName,
	}}}
	_, err := client.Get(srv.URL)
	require.Error(t, err)
	return err
}

func TestClassifyNotATLSFailure(t *testing.T) {
	client := &http.Client{Transport: &http.Transport{}}
	_, err := client.Get("http://127.0.0.1:0")
	require.Error(t, err)
	assert.Nil(t, Classify(err, "example.com", time.Now(), Remedy{}))
}

func TestClassifyUnknownAuthority(t *testing.T) {
	leaf, cert := selfSignedLeaf(t, "rancher.example")
	err := dialFail(t, leaf, x509.NewCertPool(), "rancher.example")
	ce := Classify(err, "rancher.example", time.Now(), Remedy{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Contains(t, ce.Message, "--cacert")
	details, ok := ce.Details.(Details)
	require.True(t, ok)
	assert.Equal(t, KindUnknownAuthority, details.Kind)
	assert.Equal(t, Fingerprint(cert), details.Fingerprint)
	assert.NotEmpty(t, details.Subject)
}

func TestClassifyHostnameMismatch(t *testing.T) {
	ca, caKey := genCA(t, "test-ca")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	leaf := genLeaf(t, ca, caKey, "wronghost.example", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	err := dialFail(t, leaf, roots, "rancher.example")
	ce := Classify(err, "rancher.example", time.Now(), Remedy{})
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindHostnameMismatch, details.Kind)
	assert.Contains(t, ce.Message, "cannot fix a hostname mismatch")
}

func TestClassifyExpired(t *testing.T) {
	ca, caKey := genCA(t, "test-ca")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	leaf := genLeaf(t, ca, caKey, "rancher.example", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	err := dialFail(t, leaf, roots, "rancher.example")
	ce := Classify(err, "rancher.example", time.Now(), Remedy{})
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindExpired, details.Kind)
	assert.Contains(t, ce.Message, "cannot fix an expired certificate")
}

func TestClassifyNotYetValid(t *testing.T) {
	ca, caKey := genCA(t, "test-ca")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	leaf := genLeaf(t, ca, caKey, "rancher.example", time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))
	err := dialFail(t, leaf, roots, "rancher.example")
	ce := Classify(err, "rancher.example", time.Now(), Remedy{})
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindNotYetValid, details.Kind)
	assert.Contains(t, ce.Message, "check the local clock")
}

// TestClassifyNullableDetailsOnEmptyChain asserts the nullable-details
// contract directly: when a certificate-verification failure carries no
// presented certificates at all (the handshake failed before any could be
// captured), the classified error still carries its Kind but no fingerprint
// fields.
func TestClassifyNullableDetailsOnEmptyChain(t *testing.T) {
	err := &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
	ce := Classify(err, "example.com", time.Now(), Remedy{})
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindUnknownAuthority, details.Kind)
	assert.Empty(t, details.Fingerprint)
	assert.Empty(t, details.Subject)
	assert.Empty(t, details.NotAfter)
}

// TestDetailsTextDetailsAllFieldsPresent asserts the full projection order:
// certificate, issuer, fingerprint, valid - each only when the underlying
// field is present.
func TestDetailsTextDetailsAllFieldsPresent(t *testing.T) {
	d := Details{
		Kind:        KindExpired,
		Subject:     "CN=rancher.example",
		Issuer:      "CN=test-ca",
		Fingerprint: "deadbeef",
		NotBefore:   "2020-01-01T00:00:00Z",
		NotAfter:    "2020-02-01T00:00:00Z",
	}
	assert.Equal(t, []string{
		"certificate: CN=rancher.example",
		"issuer: CN=test-ca",
		"fingerprint: deadbeef",
		"valid: 2020-01-01T00:00:00Z to 2020-02-01T00:00:00Z",
	}, d.TextDetails())
}

// TestDetailsTextDetailsNullableFieldsOmitLines asserts the nullable-details
// case (classifyKind's empty-chain path): every field but Kind may be
// absent, and an absent field prints no line - never an empty label.
func TestDetailsTextDetailsNullableFieldsOmitLines(t *testing.T) {
	assert.Empty(t, Details{Kind: KindInvalidChain}.TextDetails())
}

func TestClassifyInvalidChainDefaultsUnrecognizedReason(t *testing.T) {
	err := &tls.CertificateVerificationError{
		Err: x509.CertificateInvalidError{Reason: x509.TooManyIntermediates},
	}
	ce := Classify(err, "example.com", time.Now(), Remedy{})
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindInvalidChain, details.Kind)
}

func TestCandidateAnchorLeafOnly(t *testing.T) {
	_, leaf := selfSignedLeaf(t, "127.0.0.1")
	got := CandidateAnchor([]*x509.Certificate{leaf})
	assert.Same(t, leaf, got)
}

func TestCandidateAnchorIntermediateOnlyRootAbsent(t *testing.T) {
	// Rancher commonly omits the root: the presented chain is leaf +
	// intermediate CA, with no root certificate at all.
	ca, caKey := genCA(t, "intermediate-ca")
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(4),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"leaf"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &priv.PublicKey, caKey)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	got := CandidateAnchor([]*x509.Certificate{leaf, ca})
	assert.Same(t, ca, got)
}

func TestCandidateAnchorMalformedGapNoValidIssuer(t *testing.T) {
	_, leaf := selfSignedLeaf(t, "leaf-host")
	unrelated, _ := genCA(t, "unrelated")
	got := CandidateAnchor([]*x509.Certificate{leaf, unrelated})
	assert.Same(t, leaf, got, "an unrelated certificate must never become the candidate")
}

func TestCandidateAnchorExtraneousCertIgnored(t *testing.T) {
	ca, caKey := genCA(t, "real-ca")
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(5),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"leaf"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &priv.PublicKey, caKey)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	extraneous, _ := genCA(t, "extraneous")

	got := CandidateAnchor([]*x509.Certificate{leaf, ca, extraneous})
	assert.Same(t, ca, got)
}

func TestCandidateAnchorEmptyChain(t *testing.T) {
	assert.Nil(t, CandidateAnchor(nil))
}

func TestFingerprintIsSHA256HexOfDER(t *testing.T) {
	_, cert := selfSignedLeaf(t, "leaf-host")
	want := sha256.Sum256(cert.Raw)
	assert.Equal(t, hex.EncodeToString(want[:]), Fingerprint(cert))
}

// TestClassifyProxyObservedChainDiffersFromDirect proves the classification
// reflects whatever chain the real client actually verified against - here
// standing in for an intercepting proxy by simply using a distinct CA/leaf
// pair from the "direct" test above, since Classify never re-dials: it can
// only ever report the chain its caller's own failed request observed.
func TestClassifyProxyObservedChainDiffersFromDirect(t *testing.T) {
	proxyLeaf, proxyCert := selfSignedLeaf(t, "proxy-observed")
	err := dialFail(t, proxyLeaf, x509.NewCertPool(), "proxy-observed")
	ce := Classify(err, "proxy-observed", time.Now(), Remedy{})
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, Fingerprint(proxyCert), details.Fingerprint)
}

// TestClassifyExpiredDetailsDescribeTheFailingCertificate: for every kind
// except unknown authority, the details must describe the certificate that
// failed verification - never the candidate anchor, which for an expired
// leaf under a valid CA would be the (not expired, differently named) CA.
func TestClassifyExpiredDetailsDescribeTheFailingCertificate(t *testing.T) {
	ca, caKey := genCA(t, "test-ca")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	leaf := genLeaf(t, ca, caKey, "rancher.example", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	err := dialFail(t, leaf, roots, "rancher.example")
	ce := Classify(err, "rancher.example", time.Now(), Remedy{})
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindExpired, details.Kind)
	assert.Contains(t, details.Subject, "rancher.example", "details must describe the expired leaf")
	assert.NotContains(t, details.Subject, "test-ca", "details must never describe the CA for an expired leaf")
	parsed, perr := x509.ParseCertificate(leaf.Certificate[0])
	require.NoError(t, perr)
	assert.Equal(t, parsed.NotAfter.UTC().Format(time.RFC3339), details.NotAfter,
		"notAfter must be the failing certificate's, not the anchor's")
}

// TestClassifyUnknownAuthorityDetailsDescribeTheCandidateAnchor asserts the
// one deliberate exception: unknown authority shows the certificate a user
// could pin, which for a chain [leaf, CA] is the CA.
func TestClassifyUnknownAuthorityDetailsDescribeTheCandidateAnchor(t *testing.T) {
	ca, caKey := genCA(t, "pinnable-ca")
	leaf := genLeaf(t, ca, caKey, "rancher.example", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	// Present the CA alongside the leaf so the candidate walk has an
	// anchor above the leaf to find.
	leaf.Certificate = append(leaf.Certificate, ca.Raw)
	err := dialFail(t, leaf, x509.NewCertPool(), "rancher.example")
	ce := Classify(err, "rancher.example", time.Now(), Remedy{})
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindUnknownAuthority, details.Kind)
	assert.Contains(t, details.Subject, "pinnable-ca", "unknown authority shows the pinnable anchor")
}

// TestClassifyNotYetValidIntermediate: the verifier flags the certificate
// whose validity actually failed - a future-dated INTERMEDIATE under a
// currently-valid leaf must classify as not-yet-valid (check the clock),
// never as expired (renew the cert), which chain[0]'s dates would imply.
func TestClassifyNotYetValidIntermediate(t *testing.T) {
	now := time.Now()
	future := &x509.Certificate{
		SerialNumber:          big.NewInt(9),
		Subject:               pkix.Name{CommonName: "future-intermediate"},
		NotBefore:             now.Add(time.Hour),
		NotAfter:              now.Add(48 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	err := &tls.CertificateVerificationError{
		UnverifiedCertificates: []*x509.Certificate{
			{SerialNumber: big.NewInt(8), Subject: pkix.Name{CommonName: "valid-leaf"},
				NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)},
			future,
		},
		Err: x509.CertificateInvalidError{Cert: future, Reason: x509.Expired},
	}
	ce := Classify(err, "rancher.example", now, Remedy{})
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindNotYetValid, details.Kind,
		"the flagged certificate's dates decide, not the leaf's")
	assert.Contains(t, details.Subject, "future-intermediate")
}

// synthURLErr builds the error shape a failed TLS handshake produces for a
// request aimed at terminal: a *url.Error wrapping a certificate
// verification error whose typed cause is x509 unknown authority.
func synthURLErr(t *testing.T, terminal string, chain ...*x509.Certificate) error {
	t.Helper()
	return &url.Error{Op: "Get", URL: terminal, Err: &tls.CertificateVerificationError{
		UnverifiedCertificates: chain,
		Err:                    x509.UnknownAuthorityError{},
	}}
}

func originOf(t *testing.T, raw string) httpx.Origin {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	o, err := httpx.OriginOf(u)
	require.NoError(t, err)
	return o
}

// TestClassifyForOriginAttribution asserts the attribution rule shared by
// ClassifyForOrigin and KindOf: the failed request's terminal URL is
// compared to the expected origin as a normalized tuple, so a same-origin
// redirect to a differently spelled URL is still attributed, while a
// different scheme, host, or port is not.
func TestClassifyForOriginAttribution(t *testing.T) {
	_, leaf := selfSignedLeaf(t, "idp.example")
	cases := []struct {
		name, want, terminal string
		attributed           bool
	}{
		{"host case and explicit port", "https://IDP.example:443", "https://idp.example/token", true},
		{"ipv6 explicit port", "https://[::1]:443", "https://[::1]/token", true},
		{"same host different scheme", "https://idp.example", "http://idp.example/token", false},
		{"different host", "https://idp.example", "https://other.example/token", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := synthURLErr(t, tc.terminal, leaf)
			want := originOf(t, tc.want)
			ce := ClassifyForOrigin(err, want, time.Now(), Remedy{})
			kind, ok := KindOf(err, want, time.Now())
			if !tc.attributed {
				assert.Nil(t, ce)
				assert.False(t, ok)
				return
			}
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
			assert.True(t, ok)
			assert.Equal(t, KindUnknownAuthority, kind)
		})
	}
}

// TestClassifyForOriginAfterSameOriginRedirect asserts attribution through
// a real client: the first request is spelled with an upper-case host, the
// server redirects to the lower-case spelling (same origin, so the redirect
// policy follows it), and the second connection's handshake fails. The
// error's terminal URL is the redirected spelling; both ClassifyForOrigin
// and KindOf must still attribute it to the original spelling's origin.
func TestClassifyForOriginAfterSameOriginRedirect(t *testing.T) {
	ca, caKey := genCA(t, "fixture-ca")
	trusted := genLeaf(t, ca, caKey, "idp.example", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	untrusted, _ := selfSignedLeaf(t, "idp.example")
	roots := x509.NewCertPool()
	roots.AddCert(ca)

	var connections atomic.Int32
	// port is the listener's port, known only after StartTLS; the handler
	// closes over it because a server-side r.URL carries only the path.
	var port string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			// Lower-case spelling of the same origin.
			http.Redirect(w, r, "https://idp.example:"+port+"/token", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		// First connection: trusted, so the redirect is received. Second
		// connection (the redirected hop): untrusted, so its handshake fails.
		if connections.Add(1) == 1 {
			return &trusted, nil
		}
		return &untrusted, nil
	}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	var err error
	_, port, err = net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)

	// Both spellings of the fixture host resolve to the loopback listener.
	dialer := &net.Dialer{}
	client := &http.Client{
		CheckRedirect: httpx.RejectCrossOriginRedirect,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				_, p, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", p))
			},
		},
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://IDP.example:"+port+"/start", nil)
	require.NoError(t, err)
	_, err = client.Do(req)
	require.Error(t, err)
	urlErr, ok := errors.AsType[*url.Error](err)
	require.True(t, ok)
	assert.Equal(t, "https://idp.example:"+port+"/token", urlErr.URL, "the error names the redirected hop")
	assert.Equal(t, int32(2), connections.Load(), "the redirect was followed and re-dialed")

	want := originOf(t, "https://IDP.example:"+port)
	ce := ClassifyForOrigin(err, want, time.Now(), Remedy{})
	require.NotNil(t, ce, "the failure at the redirected spelling belongs to the original origin")
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	kind, ok := KindOf(err, want, time.Now())
	require.True(t, ok)
	assert.Equal(t, KindUnknownAuthority, kind)
	assert.Nil(t, ClassifyForOrigin(err, originOf(t, "https://other.example:"+port), time.Now(), Remedy{}))
}

// TestClassifyForOriginRejectsInvalidInputs asserts the nil-returning
// guards: no *url.Error in the chain, an unparseable terminal URL, and an
// invalid (zero) expected origin.
func TestClassifyForOriginRejectsInvalidInputs(t *testing.T) {
	_, leaf := selfSignedLeaf(t, "idp.example")
	want := originOf(t, "https://idp.example")
	assert.Nil(t, ClassifyForOrigin(&tls.CertificateVerificationError{UnverifiedCertificates: []*x509.Certificate{leaf}, Err: x509.UnknownAuthorityError{}}, want, time.Now(), Remedy{}))
	assert.Nil(t, ClassifyForOrigin(synthURLErr(t, "::not a url", leaf), want, time.Now(), Remedy{}))
	assert.Nil(t, ClassifyForOrigin(synthURLErr(t, "https://idp.example/", leaf), httpx.Origin{}, time.Now(), Remedy{}))
	_, ok := KindOf(errors.New("dial tcp: connection refused"), want, time.Now())
	assert.False(t, ok)
}

// genClientAuthOnlyLeaf issues a leaf whose only extended key usage is
// client authentication. Presented by a server, Go's verifier rejects it
// with CertificateInvalidError{IncompatibleUsage}, the deterministic way
// to reach the invalid-chain kind through a real handshake.
func genClientAuthOnlyLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dns string) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(4),
		Subject:      pkix.Name{CommonName: dns},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{dns},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &priv.PublicKey, caKey)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: priv}
}

// TestKindOfRealHandshakePerKind asserts KindOf over real failed handshakes
// for every kind, using a fixture whose root is trusted so the specific
// kind (not unknown authority) is what the verifier reports.
func TestKindOfRealHandshakePerKind(t *testing.T) {
	now := time.Now()
	ca, caKey := genCA(t, "fixture-ca")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	cases := []struct {
		name  string
		leaf  tls.Certificate
		roots *x509.CertPool
		sni   string
		kind  Kind
	}{
		{"unknown authority", genLeaf(t, ca, caKey, "idp.example", now.Add(-time.Hour), now.Add(time.Hour)), x509.NewCertPool(), "idp.example", KindUnknownAuthority},
		{"hostname mismatch", genLeaf(t, ca, caKey, "other.example", now.Add(-time.Hour), now.Add(time.Hour)), roots, "idp.example", KindHostnameMismatch},
		{"expired", genLeaf(t, ca, caKey, "idp.example", now.Add(-2*time.Hour), now.Add(-time.Hour)), roots, "idp.example", KindExpired},
		{"not yet valid", genLeaf(t, ca, caKey, "idp.example", now.Add(time.Hour), now.Add(2*time.Hour)), roots, "idp.example", KindNotYetValid},
		{"invalid chain", genClientAuthOnlyLeaf(t, ca, caKey, "idp.example"), roots, "idp.example", KindInvalidChain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := dialFail(t, tc.leaf, tc.roots, tc.sni)
			urlErr, ok := errors.AsType[*url.Error](err)
			require.True(t, ok)
			want := originOf(t, urlErr.URL)
			kind, ok := KindOf(err, want, now)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, tc.kind, kind)
		})
	}
}

// TestClassifyUnknownAuthorityLeafOnlyLabelsServingCertificate asserts:
// when the presented chain carries no valid issuer, the
// fingerprint describes the server's own certificate, never a certificate
// authority - the label and the message wording must say so, and
// Certificate()/IsLeafCertificate() must expose the fact so a consent flow
// never mislabels what it is asking the operator to trust.
func TestClassifyUnknownAuthorityLeafOnlyLabelsServingCertificate(t *testing.T) {
	leaf, cert := selfSignedLeaf(t, "rancher.example")
	err := dialFail(t, leaf, x509.NewCertPool(), "rancher.example")
	ce := Classify(err, "rancher.example", time.Now(), Remedy{})
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.True(t, details.IsLeafCertificate())
	require.NotNil(t, details.Certificate())
	assert.Equal(t, cert.Raw, details.Certificate().Raw)
	assert.Contains(t, details.TextDetails(), "serving certificate: "+details.Subject)
	assert.NotContains(t, ce.Message, "obtain its CA", "a bare leaf is never called the server's CA")
	assert.Contains(t, ce.Message, "no separate certificate authority")
	assert.Contains(t, ce.Message, "intercepting TLS",
		"an unrecognized certificate on a previously trusted server can mean a middlebox; the message says so")
}

// TestClassifyUnknownAuthorityWithAnchorLabelsCertificateAuthority asserts
// the non-leaf counterpart: a chain with a valid issuer labels the
// candidate a certificate authority, and IsLeafCertificate is false.
func TestClassifyUnknownAuthorityWithAnchorLabelsCertificateAuthority(t *testing.T) {
	ca, caKey := genCA(t, "pinnable-ca")
	leaf := genLeaf(t, ca, caKey, "rancher.example", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	leaf.Certificate = append(leaf.Certificate, ca.Raw)
	err := dialFail(t, leaf, x509.NewCertPool(), "rancher.example")
	ce := Classify(err, "rancher.example", time.Now(), Remedy{})
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.False(t, details.IsLeafCertificate())
	require.NotNil(t, details.Certificate())
	assert.Equal(t, ca.Raw, details.Certificate().Raw)
	assert.Contains(t, details.TextDetails(), "certificate authority: "+details.Subject)
	assert.Contains(t, ce.Message, "obtain its CA")
	assert.Contains(t, ce.Message, "intercepting TLS",
		"an unrecognized certificate on a previously trusted server can mean a middlebox; the message says so")
}

// TestDetailsCertificateNilOnNullableDetails asserts that a
// nullable-details Details (no candidate extracted) exposes no
// certificate either.
func TestDetailsCertificateNilOnNullableDetails(t *testing.T) {
	assert.Nil(t, Details{Kind: KindInvalidChain}.Certificate())
}

// TestClassifyNotYetValidCarriesNotBefore: the structured payload must let
// the user confirm the "check the local clock" hint - NotBefore is the one
// field that says when the certificate becomes valid.
func TestClassifyNotYetValidCarriesNotBefore(t *testing.T) {
	ca, caKey := genCA(t, "test-ca")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	notBefore := time.Now().Add(time.Hour)
	leaf := genLeaf(t, ca, caKey, "rancher.example", notBefore, time.Now().Add(2*time.Hour))
	err := dialFail(t, leaf, roots, "rancher.example")
	ce := Classify(err, "rancher.example", time.Now(), Remedy{})
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindNotYetValid, details.Kind)
	assert.Equal(t, notBefore.UTC().Format(time.RFC3339), details.NotBefore)
}

// TestClassifyForOriginMismatchMessageNamesPort asserts that a hostname
// mismatch attributed by origin names host and non-default port in its
// message, so two same-named servers on different ports stay
// distinguishable.
func TestClassifyForOriginMismatchMessageNamesPort(t *testing.T) {
	ca, caKey := genCA(t, "fixture-ca")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	leaf := genLeaf(t, ca, caKey, "other.example", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	err := dialFail(t, leaf, roots, "idp.example")
	urlErr, ok := errors.AsType[*url.Error](err)
	require.True(t, ok)
	terminal, perr := url.Parse(urlErr.URL)
	require.NoError(t, perr)
	want := originOf(t, urlErr.URL)
	ce := ClassifyForOrigin(err, want, time.Now(), Remedy{})
	require.NotNil(t, ce)
	assert.Equal(t, KindHostnameMismatch, ce.Details.(Details).Kind)
	require.ErrorContains(t, ce, "is not valid for "+terminal.Host+";", "host and port must both appear")
}
