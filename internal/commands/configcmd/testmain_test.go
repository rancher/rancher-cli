package configcmd

import (
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
)

func TestMain(m *testing.M) {
	cmdtest.UnsetResolutionEnv()
	os.Exit(m.Run())
}
