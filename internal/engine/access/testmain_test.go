package access

import (
	"fmt"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// TestMain fails any request this package's tests send through
// http.DefaultTransport, and fails the package when one did: a test sends
// its requests through its httptest server's own client, because closing
// any httptest server closes the idle connections of the default transport.
func TestMain(m *testing.M) {
	restore := testtransport.GuardDefaultTransport()
	code := m.Run()
	code = testrancher.FailOnWritesWithoutFieldManager(code, os.Stderr)
	if err := restore(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
