package funcvalue

import (
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
)

type payload struct{ Name string }

func apply(f func(string) string, s string) string { return f(s) }

func (p payload) TextDetails() []clierr.DetailLine {
	return []clierr.DetailLine{clierr.Line(clierr.ValuePart("name: ", apply(output.Printable, p.Name)))}
}
