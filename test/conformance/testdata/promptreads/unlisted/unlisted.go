package unlisted

import (
	"context"

	"github.com/rancher/rancher-cli/internal/runtime"
)

// Read asks for a secret.
func Read(p runtime.Prompter) (string, error) {
	return p.PromptSecret(context.Background(), "Password: ")
}
