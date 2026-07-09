package config

import (
	"os"
	"testing"
)

// TestMain scrubs the ambient RANCHER_* env vars this package's tests
// exercise as overrides (RANCHER_CONTEXT/RANCHER_CACERT) or policy inputs
// (RANCHER_CREDENTIAL_STORE), so a normal interactive shell with these set
// cannot silently change what a test observes — DR-008.
func TestMain(m *testing.M) {
	for _, v := range []string{
		"RANCHER_URL", "RANCHER_TOKEN", "RANCHER_CONTEXT", "RANCHER_CACERT", "RANCHER_CREDENTIAL_STORE",
	} {
		_ = os.Unsetenv(v)
	}
	os.Exit(m.Run())
}
