package methodvalue

import "github.com/rancher/rancher-cli/internal/runtime"

// Read holds the method in a variable.
func Read(p runtime.Prompter) {
	read := p.ReadLine
	_ = read
}
