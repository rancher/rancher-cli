package methodexpr

import "github.com/rancher/rancher-cli/internal/runtime"

// Read names the method through its type.
func Read() {
	_ = runtime.Prompter.PromptSecret
}
