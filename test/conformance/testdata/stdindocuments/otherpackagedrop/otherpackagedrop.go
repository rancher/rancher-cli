package otherpackagedrop

import "github.com/rancher/rancher-cli/internal/engine"

// DropLeadingByteOrderMarks has the name of the engine function and is not it.
func DropLeadingByteOrderMarks(b []byte) []byte { return b }

// Load calls the function of this package, which drops nothing.
func Load(src engine.FileSource, path string) (string, error) {
	b, err := src.Read(path)
	if err != nil {
		return "", err
	}
	return string(DropLeadingByteOrderMarks(b)), nil
}
