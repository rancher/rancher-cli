package countdiffers

import "github.com/rancher/rancher-cli/internal/engine"

// Load reads two documents and drops the marks of both.
func Load(src engine.FileSource, a, b string) (string, error) {
	first, err := src.Read(a)
	if err != nil {
		return "", err
	}
	second, err := src.Read(b)
	if err != nil {
		return "", err
	}
	return string(engine.DropLeadingByteOrderMarks(first)) + string(engine.DropLeadingByteOrderMarks(second)), nil
}
