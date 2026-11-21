package assigned

import "github.com/rancher/rancher-cli/internal/engine"

// Load assigns the result of the drop back to its input.
func Load(src engine.FileSource, path string) (string, error) {
	b, err := src.Read(path)
	if err != nil {
		return "", err
	}
	b = engine.DropLeadingByteOrderMarks(b)
	return string(b), nil
}
