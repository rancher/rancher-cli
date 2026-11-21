package methodexpr

import "github.com/rancher/rancher-cli/internal/engine"

// Load calls the method through its type.
func Load(src engine.FileSource, path string) string {
	b, _ := engine.FileSource.Read(src, path)
	return string(b)
}
