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

// TestValidateAcceptsMaxMinor asserts that major.minor with no leading zero
// is accepted on an OracleOnly probe and on a Downstream probe, and that an
// empty MaxMinor is accepted on a facade probe.
func TestValidateAcceptsMaxMinor(t *testing.T) {
	t.Parallel()
	for _, maxMinor := range []string{"2.15", "2.0", "2.9", "10.100", "3.0"} {
		for name, mutate := range map[string]func(*Probe){
			"oracle only": func(p *Probe) { p.OracleOnly = true },
			"downstream":  func(p *Probe) { p.Downstream = true },
		} {
			p := validProbe()
			mutate(&p)
			p.MaxMinor = maxMinor
			require.NoError(t, p.Validate(), "%s with MaxMinor %s", name, maxMinor)
		}
	}
	assert.NoError(t, validProbe().Validate(), "no MaxMinor on a facade probe")
}

// TestValidateAcceptsWaitForStatus asserts that 0 (unset) and every HTTP
// status from 100 to 599 are accepted on an observation without captures.
func TestValidateAcceptsWaitForStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []int{0, 100, 401, 599} {
		p := validProbe()
		p.Observations[0].WaitForStatus = status
		assert.NoError(t, p.Validate(), "WaitForStatus %d", status)
	}
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
		{"wait status below 100", func(p *Probe) { p.Observations[0].WaitForStatus = 99 }, "is not an HTTP status"},
		{"wait status above 599", func(p *Probe) { p.Observations[0].WaitForStatus = 600 }, "is not an HTTP status"},
		{"negative wait status", func(p *Probe) { p.Observations[0].WaitForStatus = -1 }, "is not an HTTP status"},
		{"wait on an observation that captures", func(p *Probe) {
			p.Observations[0].WaitForStatus = 201
			p.Observations[0].Captures = map[string]Capture{"name": {Path: "metadata.name"}}
		}, "captures cannot set WaitForStatus"},
		{"oracle-only with divergence pin", func(p *Probe) {
			p.OracleOnly = true
			p.FacadeDivergence = "some reason"
		}, "mutually exclusive"},
		{"downstream and oracle-only", func(p *Probe) {
			p.Downstream = true
			p.OracleOnly = true
		}, "mutually exclusive"},
		{"downstream with divergence pin", func(p *Probe) {
			p.Downstream = true
			p.FacadeDivergence = "some reason"
		}, "mutually exclusive"},
		{"normalize rule with both replace and keep-prefix", func(p *Probe) {
			p.Normalize = []NormalizeRule{{Path: "status.userInfo.groups", Replace: "x", KeepPrefix: "system:"}}
		}, "mutually exclusive"},
		{"drop rule without drop-by", func(p *Probe) {
			p.Normalize = []NormalizeRule{{Path: "data", DropPrefix: "e2e-fid-"}}
		}, "DropPrefix needs DropBy"},
		{"drop rule with replace", func(p *Probe) {
			p.Normalize = []NormalizeRule{{Path: "data", DropBy: "id", DropPrefix: "e2e-fid-", Replace: "x"}}
		}, "DropPrefix is exclusive"},
		{"drop rule with keep-prefix", func(p *Probe) {
			p.Normalize = []NormalizeRule{{Path: "data", DropBy: "id", DropPrefix: "e2e-fid-", KeepPrefix: "system:"}}
		}, "DropPrefix is exclusive"},
		{"drop rule with sort-by", func(p *Probe) {
			p.Normalize = []NormalizeRule{{Path: "data", DropBy: "id", DropPrefix: "e2e-fid-", SortBy: "id"}}
		}, "DropPrefix is exclusive"},
		{"keep-keys rule with replace", func(p *Probe) {
			p.Normalize = []NormalizeRule{{Path: "metadata.annotations", KeepKeys: []string{"a"}, Replace: "x"}}
		}, "KeepKeys is exclusive"},
		{"keep-keys rule with keep-prefix", func(p *Probe) {
			p.Normalize = []NormalizeRule{{Path: "metadata.annotations", KeepKeys: []string{"a"}, KeepPrefix: "system:"}}
		}, "KeepKeys is exclusive"},
		{"keep-keys rule with sort-by", func(p *Probe) {
			p.Normalize = []NormalizeRule{{Path: "metadata.annotations", KeepKeys: []string{"a"}, SortBy: "id"}}
		}, "KeepKeys is exclusive"},
		{"keep-keys rule with drop-prefix", func(p *Probe) {
			p.Normalize = []NormalizeRule{{Path: "metadata.annotations", KeepKeys: []string{"a"}, DropBy: "id", DropPrefix: "e2e-fid-"}}
		}, "KeepKeys is exclusive"},
		{"keep-keys rule with an empty entry", func(p *Probe) {
			p.Normalize = []NormalizeRule{{Path: "metadata.annotations", KeepKeys: []string{"a", ""}}}
		}, "KeepKeys has an empty entry"},
		{"MaxMinor with only a major", func(p *Probe) { p.OracleOnly = true; p.MaxMinor = "2" }, "MaxMinor"},
		{"max minor with a letter", func(p *Probe) { p.OracleOnly = true; p.MaxMinor = "2.x" }, "MaxMinor"},
		{"max minor with a leading v", func(p *Probe) { p.OracleOnly = true; p.MaxMinor = "v2.15" }, "MaxMinor"},
		{"max minor with a leading zero", func(p *Probe) { p.OracleOnly = true; p.MaxMinor = "2.015" }, "MaxMinor"},
		{"max minor with a patch", func(p *Probe) { p.Downstream = true; p.MaxMinor = "2.15.1" }, "MaxMinor"},
		{"max minor with a blank", func(p *Probe) { p.Downstream = true; p.MaxMinor = " 2.15" }, "MaxMinor"},
		{"max minor on a probe that runs on the facade", func(p *Probe) { p.MaxMinor = "2.15" }, "only on an OracleOnly or a Downstream probe"},
		{"max minor on a facade divergence", func(p *Probe) { p.FacadeDivergence = "reason"; p.MaxMinor = "2.15" }, "only on an OracleOnly or a Downstream probe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := validProbe()
			tt.mutate(&p)
			err := p.Validate()
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}
