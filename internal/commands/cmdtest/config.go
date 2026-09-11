package cmdtest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
)

// CAPem1 and CAPem2 are two distinct, syntactically shaped CA PEM values
// for tests that compare or swap a server entry's CA.
const (
	CAPem1 = "-----BEGIN CERTIFICATE-----\nMIIB...one\n-----END CERTIFICATE-----\n"
	CAPem2 = "-----BEGIN CERTIFICATE-----\nMIIB...two\n-----END CERTIFICATE-----\n"
)

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
