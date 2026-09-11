package token

import (
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
)

// TestMain clears the RANCHER_CLI_* variables this package's tests exercise
// as overrides, so a normal interactive shell with them set cannot silently
// change what a test observes. A RANCHER_CLI_URL inherited from the
// environment once made a unit test dial out to whatever it pointed at
// instead of the test's local httptest server.
func TestMain(m *testing.M) {
	for _, v := range []string{
		config.EnvURL, config.EnvToken, config.EnvCACert, config.EnvCredentialStore,
	} {
		_ = os.Unsetenv(v)
	}
	code := m.Run()
	if fakeHelperDir != "" {
		_ = os.RemoveAll(fakeHelperDir)
	}
	os.Exit(code)
}
