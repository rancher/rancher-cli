package token

import (
	"fmt"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// TestMain clears the RANCHER_CLI_* variables this package's tests exercise
// as overrides, so a normal interactive shell with them set cannot silently
// change what a test observes. A RANCHER_CLI_URL inherited from the
// environment once made a unit test dial out to whatever it pointed at
// instead of the test's local httptest server. A test that sends a request
// through http.DefaultTransport fails the package: closing any httptest
// server closes that transport's idle connections, so a test uses its
// server's own client.
func TestMain(m *testing.M) {
	for _, v := range []string{
		config.EnvURL, config.EnvToken, config.EnvCACert, config.EnvCredentialStore,
	} {
		_ = os.Unsetenv(v)
	}
	restore := testtransport.GuardDefaultTransport()
	code := m.Run()
	code = testrancher.FailOnWritesWithoutFieldManager(code, os.Stderr)
	if err := restore(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if fakeHelperDir != "" {
		_ = os.RemoveAll(fakeHelperDir)
	}
	os.Exit(code)
}
