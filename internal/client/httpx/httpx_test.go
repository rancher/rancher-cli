package httpx

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func generateSelfSignedCert(t *testing.T) (certPEM string, cert *x509.Certificate) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "test-cert",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	require.NoError(t, err)

	certPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	require.NotNil(t, certPEMBytes)

	cert, err = x509.ParseCertificate(certDER)
	require.NoError(t, err)

	return string(certPEMBytes), cert
}

func TestNew(t *testing.T) {
	t.Run("default secure client", func(t *testing.T) {
		client, err := New(Options{})
		require.NoError(t, err)
		require.NotNil(t, client)

		tr, ok := client.Transport.(*http.Transport)
		require.True(t, ok)
		require.NotNil(t, tr.TLSClientConfig)
		assert.Equal(t, uint16(tls.VersionTLS12), tr.TLSClientConfig.MinVersion)
		assert.False(t, tr.TLSClientConfig.InsecureSkipVerify)
		assert.Nil(t, tr.TLSClientConfig.RootCAs)
	})

	t.Run("insecure mode", func(t *testing.T) {
		client, err := New(Options{Insecure: true})
		require.NoError(t, err)

		tr, ok := client.Transport.(*http.Transport)
		require.True(t, ok)
		assert.True(t, tr.TLSClientConfig.InsecureSkipVerify)
	})

	t.Run("custom CA cert", func(t *testing.T) {
		certPEM, _ := generateSelfSignedCert(t)

		client, err := New(Options{CACert: certPEM})
		require.NoError(t, err)

		tr, ok := client.Transport.(*http.Transport)
		require.True(t, ok)
		require.NotNil(t, tr.TLSClientConfig.RootCAs)
		assert.False(t, tr.TLSClientConfig.InsecureSkipVerify)
	})

	t.Run("invalid CA cert PEM", func(t *testing.T) {
		_, err := New(Options{CACert: "not-valid-pem"})
		require.Error(t, err)
		assert.ErrorContains(t, err, "failed to append CA certs from PEM")
	})

	t.Run("proxy from environment", func(t *testing.T) {
		client, err := New(Options{})
		require.NoError(t, err)

		tr, ok := client.Transport.(*http.Transport)
		require.True(t, ok)
		require.NotNil(t, tr.Proxy)
	})

	t.Run("timeout passed through", func(t *testing.T) {
		client, err := New(Options{Timeout: 5 * time.Second})
		require.NoError(t, err)
		assert.Equal(t, 5*time.Second, client.Timeout)
	})

	t.Run("zero timeout", func(t *testing.T) {
		client, err := New(Options{Timeout: 0})
		require.NoError(t, err)
		assert.Equal(t, time.Duration(0), client.Timeout)
	})

	t.Run("transport configuration", func(t *testing.T) {
		client, err := New(Options{})
		require.NoError(t, err)

		tr, ok := client.Transport.(*http.Transport)
		require.True(t, ok)
		assert.True(t, tr.ForceAttemptHTTP2)
		assert.Equal(t, 100, tr.MaxIdleConns)
		assert.Equal(t, 10, tr.MaxIdleConnsPerHost)
		assert.Equal(t, 90*time.Second, tr.IdleConnTimeout)
		assert.Equal(t, 10*time.Second, tr.TLSHandshakeTimeout)
		assert.Equal(t, 1*time.Second, tr.ExpectContinueTimeout)
	})
}

func TestFunctionalRoundTrip(t *testing.T) {
	t.Run("successful HTTPS request with custom CA", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}))
		defer server.Close()

		certPEM := pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: server.Certificate().Raw,
		})

		client, err := New(Options{CACert: string(certPEM)})
		require.NoError(t, err)

		serverURL, err := url.Parse(server.URL)
		require.NoError(t, err)

		req, err := http.NewRequest(http.MethodGet, server.URL, nil)
		require.NoError(t, err)

		req.Host = serverURL.Host

		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("HTTPS request fails with wrong CA", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		bogusCertPEM, _ := generateSelfSignedCert(t)

		client, err := New(Options{CACert: bogusCertPEM})
		require.NoError(t, err)

		req, err := http.NewRequest(http.MethodGet, server.URL, nil)
		require.NoError(t, err)

		_, err = client.Do(req)
		require.Error(t, err)
		assert.ErrorContains(t, err, "certificate")
	})
}

func TestDo(t *testing.T) {
	t.Run("reads and closes the body, returns it alongside the response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer server.Close()

		req, err := http.NewRequest(http.MethodGet, server.URL, nil)
		require.NoError(t, err)

		resp, body, err := Do(http.DefaultClient, req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusCreated, resp.StatusCode)
		assert.Equal(t, `{"ok":true}`, string(body))

		// The body reader is already closed by Do; a second read must not hang
		// or panic — it's just empty.
		n, _ := resp.Body.Read(make([]byte, 1))
		assert.Equal(t, 0, n)
	})

	t.Run("propagates a client.Do transport error without a response", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:0", nil)
		require.NoError(t, err)

		_, body, err := Do(http.DefaultClient, req)
		require.Error(t, err)
		assert.Nil(t, body)
	})
}
