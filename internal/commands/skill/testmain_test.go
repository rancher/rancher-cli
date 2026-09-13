package skill

import (
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
)

func TestMain(m *testing.M) {
	cmdtest.UnsetResolutionEnv()
	cleanup := cmdtest.IsolateConfigDir()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
