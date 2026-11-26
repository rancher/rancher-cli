package uiprompter

import (
	"context"

	"github.com/rancher/rancher-cli/internal/ui"
)

// Read asks for a secret through the terminal prompter.
func Read(p *ui.Prompter) (string, error) { return p.PromptSecret(context.Background(), "Token: ") }
