package cli

import (
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
)

// TestMain scrubs the RANCHER_* env vars inherited from the environment this package's tests
// exercise as overrides, so a normal interactive shell with these set cannot
// silently change what a test observes - DR-008.
func TestMain(m *testing.M) {
	for _, v := range []string{
		config.EnvURL, config.EnvToken, config.EnvCACert, config.EnvCredentialStore,
	} {
		_ = os.Unsetenv(v)
	}
	os.Exit(m.Run())
}
