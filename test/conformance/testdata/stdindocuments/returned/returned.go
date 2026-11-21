package returned

import "github.com/rancher/rancher-cli/internal/engine"

// Load returns the result of the drop.
func Load(src engine.FileSource, path string) ([]byte, error) {
	b, err := src.Read(path)
	if err != nil {
		return nil, err
	}
	return engine.DropLeadingByteOrderMarks(b), nil
}
