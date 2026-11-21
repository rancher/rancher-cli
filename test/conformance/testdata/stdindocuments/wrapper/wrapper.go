package wrapper

import "github.com/rancher/rancher-cli/internal/engine"

// wrapper embeds the source of documents.
type wrapper struct {
	engine.FileSource
}

// Text reads through the embedded source.
func (w wrapper) Text(path string) string {
	b, _ := w.Read(path)
	return string(b)
}
