package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/rancher/rancher-cli/internal/engine"
)

type fileSource struct{ stdin io.Reader }

func newFileSource(stdin io.Reader) engine.FileSource { return fileSource{stdin: stdin} }

func (f fileSource) Read(path string) ([]byte, error) {
	if path == "-" {
		if f.stdin == nil {
			return nil, fmt.Errorf("no stdin available")
		}
		return io.ReadAll(f.stdin)
	}
	return os.ReadFile(path)
}
