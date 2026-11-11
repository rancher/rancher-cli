package labelfield

import "github.com/rancher/rancher-cli/internal/clierr"

type payload struct{ Issuer string }

func (p payload) TextDetails() []clierr.DetailLine {
	return []clierr.DetailLine{clierr.Line(clierr.ValuePart("issuer: "+p.Issuer, ""))}
}
