package heldraw

import "github.com/rancher/rancher-cli/internal/engine"

// Load reads the document as bytes, though its entry says it is held as a
// secret.Value.
func Load(src engine.FileSource, path string) (string, error) {
	raw, err := src.Read(path)
	if err != nil {
		return "", err
	}
	return string(engine.DropLeadingByteOrderMarks(raw)), nil
}
