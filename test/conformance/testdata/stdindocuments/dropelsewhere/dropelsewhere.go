package dropelsewhere

import "github.com/rancher/rancher-cli/internal/engine"

// Load reads a document and hands it to decode, which drops.
func Load(src engine.FileSource, path string) (string, error) {
	b, err := src.Read(path)
	if err != nil {
		return "", err
	}
	return decode(b), nil
}

func decode(b []byte) string {
	return string(engine.DropLeadingByteOrderMarks(b))
}
