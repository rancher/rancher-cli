package discarded

import "github.com/rancher/rancher-cli/internal/engine"

// Load calls the drop and assigns its result to the blank identifier.
func Load(src engine.FileSource, path string) (string, error) {
	b, err := src.Read(path)
	if err != nil {
		return "", err
	}
	_ = engine.DropLeadingByteOrderMarks(b)
	return string(b), nil
}
