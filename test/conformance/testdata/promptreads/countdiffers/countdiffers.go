package countdiffers

import (
	"context"

	"github.com/rancher/rancher-cli/internal/runtime"
)

// Read asks twice.
func Read(p runtime.Prompter) {
	_, _ = p.PromptSecret(context.Background(), "a")
	_, _ = p.PromptSecret(context.Background(), "b")
}
