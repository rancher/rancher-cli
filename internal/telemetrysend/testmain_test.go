package telemetrysend

import (
	"fmt"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/testtransport"
)

// TestMain fails the package when a test sends a request through
// http.DefaultTransport: every test client is the sender's own or a test
// server's.
func TestMain(m *testing.M) {
	restore := testtransport.GuardDefaultTransport()
	code := m.Run()
	if err := restore(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
