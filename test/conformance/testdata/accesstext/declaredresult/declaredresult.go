package declaredresult

import "github.com/rancher/rancher-cli/internal/output"

type chainNode struct{}

func renderTree(n chainNode, mark string) []string { return nil }

func text(n chainNode) string {
	var lines = renderTree(n, "x")
	var s string
	for _, l := range lines {
		s += output.Printable(l)
	}
	return s
}
