package methodvalue

import "github.com/rancher/rancher-cli/internal/engine"

// Load holds the method in a variable and calls it.
func Load(src engine.FileSource, path string) string {
	read := src.Read
	b, _ := read(path)
	return string(b)
}

func apply(f func(string) ([]byte, error)) {}

// Pass hands the method to another function.
func Pass(src engine.FileSource) {
	apply(src.Read)
}
