package configcmd

import (
	"fmt"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// TestMain fails the package when a test leaves a new file in the
// package directory, or sends a request through http.DefaultTransport:
// closing any httptest server closes that transport's idle connections, so
// a test uses its server's own client.
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
