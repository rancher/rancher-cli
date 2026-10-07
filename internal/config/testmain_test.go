package config

import (
	"os"
	"testing"
)

// TestMain clears the RANCHER_CLI_* variables this package's tests exercise
// as overrides (RANCHER_CLI_CACERT) or policy inputs
// (RANCHER_CLI_CREDENTIAL_STORE), so a normal interactive shell with them
// set cannot silently change what a test observes. It also removes the
// directory the fake credential helper was built in.
func TestMain(m *testing.M) {
	for _, v := range []string{
		EnvURL, EnvToken, EnvCACert, EnvCredentialStore,
	} {
		_ = os.Unsetenv(v)
	}
	code := m.Run()
	if fakeHelperRoot != "" {
		_ = os.RemoveAll(fakeHelperRoot)
	}
	os.Exit(code)
}
