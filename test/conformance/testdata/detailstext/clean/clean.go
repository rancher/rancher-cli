package clean

import (
	"fmt"
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
)

type payload struct {
	Name  string
	Items []string
	Ref   clierr.TokenRef
	Count int
}

func noun(scoped bool) string {
	if scoped {
		return "cluster token"
	}
	return "token"
}

func (p payload) TextDetails() []clierr.DetailLine {
	label := "name"
	if p.Count > 1 {
		label = "names"
	}
	parts := []clierr.DetailPart{clierr.ValuePart(label+": ", p.Name)}
	parts = append(parts, clierr.ListParts(noun(p.Count > 0)+" items: ", p.Items)...)
	parts = append(parts, clierr.TokenRefParts(" ref ", p.Ref)...)
	parts = append(parts, clierr.LabelPart(fmt.Sprintf(" and %d more (", p.Count)+strconv.Itoa(p.Count)+")"))
	return []clierr.DetailLine{clierr.Line(parts...), {Indent: "  ", Parts: []clierr.DetailPart{{Label: "tail"}}}}
}
