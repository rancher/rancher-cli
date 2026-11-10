package cmdtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
)

// CAPem1 and CAPem2 are two distinct, syntactically shaped CA PEM values
// for tests that compare or swap a server entry's CA.
const (
	CAPem1 = "-----BEGIN CERTIFICATE-----\nMIIB...one\n-----END CERTIFICATE-----\n"
	CAPem2 = "-----BEGIN CERTIFICATE-----\nMIIB...two\n-----END CERTIFICATE-----\n"
)

// CertificatePEM generates a new self-signed CA certificate that parses and
// returns it as PEM text. Every call produces a different certificate.
func CertificatePEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// FilePolicyDir returns a config dir pinned to the file credential store,
// so a login's store re-pin is a no-op and never probes or writes the real
// OS keychain, together with the file store over that dir.
func FilePolicyDir(t *testing.T) (string, config.CredStore) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		CredentialStore: "file",
		Servers:         map[string]config.Server{},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir, config.NewFileStore(dir)
}

// WriteCAFile writes contents to ca.pem in a fresh temp dir and returns
// the path, for flags that take a CA file.
func WriteCAFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

// CacheAnchor is the anchor the root hook builds in production: the
// binding stamp read first, the config loaded after it. The order is the
// point - a stamp read after the load could not tell a reader that its
// snapshot was overtaken.
func CacheAnchor(t *testing.T, dir string) *config.CacheAnchor {
	t.Helper()
	anchor := config.NewCacheAnchor(dir)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	return anchor.WithConfig(cfg)
}
