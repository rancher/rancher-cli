package otherpackage

import (
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/test/conformance/testdata/detailstext/otherpackage/helper"
)

type payload struct{ Name string }

func (p payload) TextDetails() []clierr.DetailLine {
	return []clierr.DetailLine{clierr.Line(clierr.ValuePart("name: ", helper.Quoted(p.Name)))}
}
