package heldvalue

import (
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/secret"
)

// Load reads the document into a secret.Value.
func Load(src engine.FileSource, path string) (secret.Value, error) {
	return secret.Read(src.Read, path)
}

// decode drops the byte order marks and uses the result.
func decode(raw []byte) []byte {
	return engine.DropLeadingByteOrderMarks(raw)
}
