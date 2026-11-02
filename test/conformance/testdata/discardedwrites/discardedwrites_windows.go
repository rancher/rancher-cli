package discardedwrites

import (
	"fmt"
	"io"
)

func windowsDrop(w io.Writer) { _, _ = fmt.Fprintln(w, "x") }
