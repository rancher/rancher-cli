package config

import (
	"os"
	"testing"
)

// TestMain scrubs the ambient RANCHER_* env vars this package's tests
// exercise as overrides (RANCHER_CLI_CONTEXT/RANCHER_CLI_CACERT) or policy inputs
// (RANCHER_CLI_CREDENTIAL_STORE), so a normal interactive shell with these set
// cannot silently change what a test observes — DR-008.
func TestMain(m *testing.M) {
	for _, v := range []string{
		EnvURL, EnvToken, EnvContext, EnvCACert, EnvCredentialStore,
	} {
		_ = os.Unsetenv(v)
	}
	os.Exit(m.Run())
}
