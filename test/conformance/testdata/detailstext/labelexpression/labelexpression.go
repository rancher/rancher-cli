package labelexpression

import (
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

type payload struct{ Name string }

func (p payload) TextDetails() []clierr.DetailLine {
	return []clierr.DetailLine{clierr.Line(clierr.LabelPart(strings.ToUpper("name: ")))}
}
