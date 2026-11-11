package samepackage

import (
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
)

type payload struct{ Name string }

func shown(s string) string { return output.Printable(s) }

func (p payload) TextDetails() []clierr.DetailLine {
	return []clierr.DetailLine{clierr.Line(clierr.ValuePart("name: ", shown(p.Name)))}
}
