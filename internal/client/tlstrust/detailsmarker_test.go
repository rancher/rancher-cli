package tlstrust_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// isMarkerRune reports whether r is one of the five characters a remedy marker
// is made of, U+2060 to U+2064.
func isMarkerRune(r rune) bool { return r >= '\u2060' && r <= '\u2064' }

// TestACertificateFailureLeavesNoMarkerInStructuredOutput asserts a
// certificate failure worded with a remedy command, classified as a
// production caller does, expands for an invocation against a config
// directory and writes no character of the remedy marker (U+2060 to U+2064)
// to its json or yaml output, whose details payload has no mapping of its
// own, and that the command in the message names the directory.
func TestACertificateFailureLeavesNoMarkerInStructuredOutput(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "private-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	failure := &tls.CertificateVerificationError{
		UnverifiedCertificates: []*x509.Certificate{cert},
		Err:                    x509.UnknownAuthorityError{Cert: cert},
	}
	ce := tlstrust.Classify(failure, "r.example", time.Now(), tlstrust.Remedy{
		CA: "; save it for entry 'prod' with: " + clierr.Rancher + " config update-server 'prod' --cacert <file>",
	})
	require.NotNil(t, ce)
	require.IsType(t, tlstrust.Details{}, ce.Details)

	dir := t.TempDir()
	prefix, printable := runtime.RemedyPrefix(dir)
	require.True(t, printable)
	io := runtime.IO{RemedyDir: dir}
	for _, format := range []string{"json", "yaml"} {
		var out bytes.Buffer
		require.NoError(t, runtime.RenderErrorStyled(&out, format, io.ExpandError(ce), style.Style{}))
		var doc map[string]any
		require.NoError(t, yaml.Unmarshal(out.Bytes(), &doc))
		data, _ := doc["data"].(map[string]any)
		errDoc, _ := data["error"].(map[string]any)
		message, _ := errDoc["message"].(string)
		assert.Contains(t, message, "rancher"+prefix+" config update-server 'prod' --cacert <file>", format)
		assert.False(t, strings.ContainsFunc(out.String(), isMarkerRune), "%s output holds a character of the remedy marker: %q", format, out.String())
		assert.False(t, strings.ContainsFunc(message, isMarkerRune), "the decoded message holds a character of the remedy marker: %q", message)
	}
}
