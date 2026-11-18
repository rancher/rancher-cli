// Package acceptedrenderers is a fixture of the exact-value scans.
package acceptedrenderers

import (
	"fmt"
	"io"

	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// writes writes each cell through clierr.WholeValueText.
func writes(w io.Writer, cells []string) error {
	for _, cell := range cells {
		if _, err := fmt.Fprintln(w, clierr.WholeValueText(cell)); err != nil {
			return err
		}
	}
	return nil
}

// writesByValue holds clierr.WholeValueText in a variable and calls it.
func writesByValue(w io.Writer, cells []string) error {
	escape := clierr.WholeValueText
	for _, cell := range cells {
		if _, err := fmt.Fprintln(w, escape(cell)); err != nil {
			return err
		}
	}
	return nil
}
