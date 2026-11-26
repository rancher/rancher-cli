package readline

import (
	"context"

	"github.com/rancher/rancher-cli/internal/runtime"
)

// Read reads a line.
func Read(p runtime.Prompter) (string, error) { return p.ReadLine(context.Background()) }
