package unlisted

import "github.com/rancher/rancher-cli/internal/engine"

// Load reads a document and decodes it with no drop and no allowlist entry.
func Load(src engine.FileSource, path string) (string, error) {
	b, err := src.Read(path)
	return string(b), err
}
