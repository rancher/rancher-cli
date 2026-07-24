package fidelity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validProbe() Probe {
	return Probe{
		Name: "auth/example",
		Observations: []Observation{
			{Name: "get", Request: RequestSpec{Method: "GET", Path: "/v1/things", Identity: IdentityAdmin}},
		},
	}
}

func TestValidateAcceptsMinimalProbe(t *testing.T) {
	t.Parallel()
	require.NoError(t, validProbe().Validate())
}

func TestValidateRejectsInvalidProbes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Probe)
		wantErr string
	}{
		{"empty name", func(p *Probe) { p.Name = "" }, "no name"},
		{"no observations", func(p *Probe) { p.Observations = nil }, "no observations"},
		{"unnamed observation", func(p *Probe) { p.Observations[0].Name = "" }, "no name"},
		{"duplicate observation", func(p *Probe) {
			p.Observations = append(p.Observations, p.Observations[0])
		}, "duplicate observation"},
		{"missing method", func(p *Probe) { p.Observations[0].Request.Method = "" }, "method and path"},
		{"missing path", func(p *Probe) { p.Observations[0].Request.Path = "" }, "method and path"},
		{"oracle-only with divergence pin", func(p *Probe) {
			p.OracleOnly = true
			p.FacadeDivergence = "some reason"
		}, "mutually exclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := validProbe()
			tt.mutate(&p)
			err := p.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
