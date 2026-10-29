package remedymarker

import (
	"fmt"
	"io"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func write(w io.Writer, entry string) {
	fmt.Fprintf(w, clierr.RemedyMarker+"rancher auth login %s", entry)
}
