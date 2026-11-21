package barestatement

import "github.com/rancher/rancher-cli/internal/engine"

// Load calls the drop as a statement and keeps the input.
func Load(src engine.FileSource, path string) (string, error) {
	b, err := src.Read(path)
	if err != nil {
		return "", err
	}
	engine.DropLeadingByteOrderMarks(b)
	return string(b), nil
}
