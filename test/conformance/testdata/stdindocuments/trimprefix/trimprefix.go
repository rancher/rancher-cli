package trimprefix

import (
	"bytes"

	"github.com/rancher/rancher-cli/internal/engine"
)

// Load drops one mark with bytes.TrimPrefix instead of every mark.
func Load(src engine.FileSource, path string) (string, error) {
	b, err := src.Read(path)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))), nil
}
