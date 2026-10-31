package unrelated

import "github.com/rancher/rancher-cli/internal/output"

type chainNode struct{}

func renderTree(n chainNode, mark string) []string { return nil }

func text(n chainNode) string {
	var s string
	for i := range renderTree(n, "x") {
		_ = i
	}
	for i, _ := range renderTree(n, "x") {
		_ = i
	}
	return s + output.Printable("y")
}
