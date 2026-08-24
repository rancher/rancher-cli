package tlstrust

import (
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
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

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
// dynamiclistener-style shape.
func selfSignedLeaf(t *testing.T, dns string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: dns},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{dns},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, cert
}

// dialFail starts an httptest TLS server presenting leafCert, issues a GET
// against it with an empty trust store (or explicit roots) and an explicit
// SNI/hostname-check name, and returns the resulting client.Do error — the
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
	_, err := http.Get("http://127.0.0.1:0")
	require.Error(t, err)
	assert.Nil(t, Classify(err, "example.com", time.Now()))
}

func TestClassifyUnknownAuthority(t *testing.T) {
	leaf, cert := selfSignedLeaf(t, "rancher.example")
	err := dialFail(t, leaf, x509.NewCertPool(), "rancher.example")
	ce := Classify(err, "rancher.example", time.Now())
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Contains(t, ce.Hint, "--cacert")
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
	ce := Classify(err, "rancher.example", time.Now())
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindHostnameMismatch, details.Kind)
	assert.Contains(t, ce.Hint, "cannot fix a hostname mismatch")
}

func TestClassifyExpired(t *testing.T) {
	ca, caKey := genCA(t, "test-ca")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	leaf := genLeaf(t, ca, caKey, "rancher.example", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	err := dialFail(t, leaf, roots, "rancher.example")
	ce := Classify(err, "rancher.example", time.Now())
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindExpired, details.Kind)
	assert.Contains(t, ce.Hint, "cannot fix an expired certificate")
}

func TestClassifyNotYetValid(t *testing.T) {
	ca, caKey := genCA(t, "test-ca")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	leaf := genLeaf(t, ca, caKey, "rancher.example", time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))
	err := dialFail(t, leaf, roots, "rancher.example")
	ce := Classify(err, "rancher.example", time.Now())
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindNotYetValid, details.Kind)
	assert.Contains(t, ce.Hint, "check the local clock")
}

// TestClassifyNullableDetailsOnEmptyChain pins the nullable-details
// contract directly: when a certificate-verification failure carries no
// presented certificates at all (the handshake failed before any could be
// captured), the classified error still carries its Kind but no fingerprint
// fields.
func TestClassifyNullableDetailsOnEmptyChain(t *testing.T) {
	err := &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
	ce := Classify(err, "example.com", time.Now())
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindUnknownAuthority, details.Kind)
	assert.Empty(t, details.Fingerprint)
	assert.Empty(t, details.Subject)
	assert.Empty(t, details.NotAfter)
}

func TestClassifyInvalidChainDefaultsUnrecognizedReason(t *testing.T) {
	err := &tls.CertificateVerificationError{
		Err: x509.CertificateInvalidError{Reason: x509.TooManyIntermediates},
	}
	ce := Classify(err, "example.com", time.Now())
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
// reflects whatever chain the real client actually verified against — here
// standing in for an intercepting proxy by simply using a distinct CA/leaf
// pair from the "direct" test above, since Classify never re-dials: it can
// only ever report the chain its caller's own failed request observed.
func TestClassifyProxyObservedChainDiffersFromDirect(t *testing.T) {
	proxyLeaf, proxyCert := selfSignedLeaf(t, "proxy-observed")
	err := dialFail(t, proxyLeaf, x509.NewCertPool(), "proxy-observed")
	ce := Classify(err, "proxy-observed", time.Now())
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, Fingerprint(proxyCert), details.Fingerprint)
}

// TestClassifyExpiredDetailsDescribeTheFailingCertificate: for every kind
// except unknown authority, the details must describe the certificate that
// failed verification — never the candidate anchor, which for an expired
// leaf under a valid CA would be the (not expired, differently named) CA.
func TestClassifyExpiredDetailsDescribeTheFailingCertificate(t *testing.T) {
	ca, caKey := genCA(t, "test-ca")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	leaf := genLeaf(t, ca, caKey, "rancher.example", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	err := dialFail(t, leaf, roots, "rancher.example")
	ce := Classify(err, "rancher.example", time.Now())
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

// TestClassifyUnknownAuthorityDetailsDescribeTheCandidateAnchor pins the
// one deliberate exception: unknown authority shows the certificate a user
// could pin, which for a chain [leaf, CA] is the CA.
func TestClassifyUnknownAuthorityDetailsDescribeTheCandidateAnchor(t *testing.T) {
	ca, caKey := genCA(t, "pinnable-ca")
	leaf := genLeaf(t, ca, caKey, "rancher.example", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	// Present the CA alongside the leaf so the candidate walk has an
	// anchor above the leaf to find.
	leaf.Certificate = append(leaf.Certificate, ca.Raw)
	err := dialFail(t, leaf, x509.NewCertPool(), "rancher.example")
	ce := Classify(err, "rancher.example", time.Now())
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindUnknownAuthority, details.Kind)
	assert.Contains(t, details.Subject, "pinnable-ca", "unknown authority shows the pinnable anchor")
}

// TestClassifyNotYetValidIntermediate: the verifier flags the certificate
// whose validity actually failed — a future-dated INTERMEDIATE under a
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
	ce := Classify(err, "rancher.example", now)
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindNotYetValid, details.Kind,
		"the flagged certificate's dates decide, not the leaf's")
	assert.Contains(t, details.Subject, "future-intermediate")
}

// TestClassifyForHostIgnoresOtherHosts pins the origin guard: a TLS failure
// from a request aimed at a DIFFERENT host (an external identity provider
// dialed with the Rancher-scoped CA pool) must not be classified — a
// confident TLS_TRUST diagnosis naming the wrong host is worse than the
// raw error.
func TestClassifyForHostIgnoresOtherHosts(t *testing.T) {
	fixLeaf, _ := selfSignedLeaf(t, "idp.example")
	err := dialFail(t, fixLeaf, x509.NewCertPool(), "idp.example")
	assert.Nil(t, ClassifyForHost(err, "rancher.example", time.Now()),
		"a failure on idp.example must not classify as rancher.example's")
	require.NotNil(t, ClassifyForHost(err, hostFromURLError(t, err), time.Now()),
		"the same failure classifies when the host matches")
}

// hostFromURLError extracts the actual host the failed request targeted.
func hostFromURLError(t *testing.T, err error) string {
	t.Helper()
	urlErr, ok := errors.AsType[*url.Error](err)
	require.True(t, ok)
	parsed, perr := url.Parse(urlErr.URL)
	require.NoError(t, perr)
	return parsed.Host
}

// TestClassifyNotYetValidCarriesNotBefore: the structured payload must let
// the user confirm the "check the local clock" hint — NotBefore is the one
// field that says when the certificate becomes valid.
func TestClassifyNotYetValidCarriesNotBefore(t *testing.T) {
	ca, caKey := genCA(t, "test-ca")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	notBefore := time.Now().Add(time.Hour)
	leaf := genLeaf(t, ca, caKey, "rancher.example", notBefore, time.Now().Add(2*time.Hour))
	err := dialFail(t, leaf, roots, "rancher.example")
	ce := Classify(err, "rancher.example", time.Now())
	require.NotNil(t, ce)
	details := ce.Details.(Details)
	assert.Equal(t, KindNotYetValid, details.Kind)
	assert.Equal(t, notBefore.UTC().Format(time.RFC3339), details.NotBefore)
}
