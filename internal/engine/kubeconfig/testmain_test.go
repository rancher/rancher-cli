package kubeconfig

import (
	"fmt"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// TestMain clears the RANCHER_CLI_* variables the config resolution reads
// ahead of the stored file: the command tests set their own and must not
// inherit an operator's. It also points the config directory at a
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
	if err := restore(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	code = cmdtest.FailOnNewFiles(code, since, os.Stderr)
	cleanup()
	os.Exit(code)
}
