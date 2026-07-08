package runtime

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/ui"
)

func TestUIPrompterImplementsPrompter(t *testing.T) {
	var _ Prompter = (*ui.Prompter)(nil)
}
