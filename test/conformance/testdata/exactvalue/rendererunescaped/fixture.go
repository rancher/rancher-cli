// Package rendererunescaped is a fixture of the exact-value scans.
package rendererunescaped

import (
	"fmt"
	"io"
)

// writes writes each cell as it is.
func writes(w io.Writer, cells []string) error { // want 3c
	for _, cell := range cells {
		if _, err := fmt.Fprintln(w, cell); err != nil {
			return err
		}
	}
	return nil
}
