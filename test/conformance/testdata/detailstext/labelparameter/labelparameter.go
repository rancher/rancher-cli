package labelparameter

import "github.com/rancher/rancher-cli/internal/clierr"

type payload struct{ Name string }

func line(label, value string) clierr.DetailLine {
	return clierr.Line(clierr.DetailPart{Label: label, Value: value})
}

func (p payload) TextDetails() []clierr.DetailLine {
	return []clierr.DetailLine{line("name: ", p.Name)}
}
