package cmdtest

import (
	"os"

	"github.com/rancher/rancher-cli/internal/config"
)

// IsolateConfigDir points the process at a fresh temporary config
// directory, so a test that builds a command and forgets to set its own
// directory reads and writes there and never the operator's real one; the
// returned func removes the directory. The credential store policy is left
// alone: tests exercise it through the config file, which the environment
// variable would outrank. A test that sets the directory itself still takes
// precedence through t.Setenv.
func IsolateConfigDir() (cleanup func()) {
	dir, err := os.MkdirTemp("", "rancher-cli-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv(config.EnvConfigDir, dir); err != nil {
		panic(err)
	}
	return func() { _ = os.RemoveAll(dir) }
}
