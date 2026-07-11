package commands

import (
	"os"
	"testing"
)

// TestMain scrubs the ambient RANCHER_* env vars this package's tests
// exercise as overrides, so a normal interactive shell with these set cannot
// silently change what a test observes — DR-008.
func TestMain(m *testing.M) {
	for _, v := range []string{
		"RANCHER_CLI_URL", "RANCHER_CLI_TOKEN", "RANCHER_CLI_CONTEXT", "RANCHER_CLI_CACERT", "RANCHER_CLI_CREDENTIAL_STORE",
	} {
		_ = os.Unsetenv(v)
	}
	os.Exit(m.Run())
}
