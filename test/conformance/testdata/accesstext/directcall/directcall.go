package directcall

import "github.com/rancher/rancher-cli/internal/output"

type chainNode struct{}

func renderTree(n chainNode, mark string) []string { return nil }

func text(n chainNode) string {
	return output.Printable(renderTree(n, "x")[0])
}
