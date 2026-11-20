package provisioning

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/testtransport"
)

// TestMain fails any request this package's tests send through
// http.DefaultTransport, and fails the package when one did: a test sends
// its requests through its httptest server's own client, because closing
// any httptest server closes the idle connections of the default transport.
// It also shortens the interval of the cluster commands' waits once, before
// any test runs. The wait tests can then run in parallel.
func TestMain(m *testing.M) {
	clusterWaitInterval = time.Millisecond
	SetCloudCredentialWaitForTest(time.Millisecond, 100*time.Millisecond)
	restore := testtransport.GuardDefaultTransport()
	code := m.Run()
	if err := restore(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
