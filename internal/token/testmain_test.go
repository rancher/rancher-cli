package token

import (
	"os"
	"testing"
)

// TestMain scrubs the ambient RANCHER_* env vars this package's tests
// exercise as overrides, so a normal interactive shell with these set cannot
// silently change what a test observes — DR-008. An ambient RANCHER_URL in
// particular made a unit test dial out to whatever it pointed at instead of
// the test's local httptest server.
func TestMain(m *testing.M) {
	for _, v := range []string{
		"RANCHER_URL", "RANCHER_TOKEN", "RANCHER_CONTEXT", "RANCHER_CACERT", "RANCHER_CREDENTIAL_STORE",
	} {
		_ = os.Unsetenv(v)
	}
	os.Exit(m.Run())
}
