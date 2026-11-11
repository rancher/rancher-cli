package direct

import "github.com/rancher/rancher-cli/internal/clierr"

type payload struct{ Name string }

func (p payload) TextDetails() []clierr.DetailLine {
	return []clierr.DetailLine{clierr.Line(clierr.ValuePart("name: ", clierr.IDText(p.Name)))}
}
