package definedrange

import "github.com/rancher/rancher-cli/internal/output"

type chainNode struct{}

func renderTree(n chainNode, mark string) []string { return nil }

func text(n chainNode) string {
	var s string
	for _, line := range renderTree(n, "x") {
		s += output.Printable(line)
	}
	return s
}
