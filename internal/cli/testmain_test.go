package cli

import (
	"fmt"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// TestMain clears the RANCHER_CLI_* variables this package's tests exercise
// as overrides, so a normal interactive shell with them set cannot silently
// change what a test observes, and points the config directory at a
// temporary one, so a test that forgets its own cannot reach the
// operator's real one. A test that leaves a new file in the package
// directory fails the package, and so does one that sends a request through
// http.DefaultTransport: closing any httptest server closes that transport's
// idle connections, so a test uses its server's own client.
func TestMain(m *testing.M) {
	cmdtest.UnsetResolutionEnv()
	cleanup := cmdtest.IsolateConfigDir()
	since := cmdtest.NewFiles()
	restore := testtransport.GuardDefaultTransport()
	code := m.Run()
	code = testrancher.FailOnWritesWithoutFieldManager(code, os.Stderr)
	if err := restore(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	code = cmdtest.FailOnNewFiles(code, since, os.Stderr)
	cleanup()
	os.Exit(code)
}
