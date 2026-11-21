package extension

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// community015 is a community Rancher v2.15.3 whose UI has Extensions API
// version 3.0.12-rc.7, on a cluster running Kubernetes v1.36.5.
var community015 = Inputs{
	RancherVersion:       "v2.15.3",
	KubernetesVersion:    "v1.36.5",
	ExtensionsAPIVersion: "3.0.12-rc.7",
	Edition:              EditionCommunity,
}

func with(in Inputs, f func(*Inputs)) Inputs {
	f(&in)
	return in
}

func results(o Outcome) []string {
	var out []string
	for _, c := range o.Checks {
		out = append(out, c.Check+"="+c.Result)
	}
	return out
}

// TestCompatibility asserts the one outcome each check gives, in the order the
// Rancher UI runs them: every row has one failure, the reason, and the checks
// that ran up to it.
func TestCompatibility(t *testing.T) {
	t.Parallel()
	base := func(extra ...string) map[string]string {
		a := ann(AnnotationExtensionsVersion, ">= 3.0.0")
		for i := 0; i < len(extra); i += 2 {
			a[extra[i]] = extra[i+1]
		}
		return a
	}
	prime := with(community015, func(in *Inputs) { in.Edition = EditionPrime })
	unknown := with(community015, func(in *Inputs) { in.Edition = EditionUnknown })
	tests := []struct {
		name    string
		annots  map[string]string
		in      Inputs
		want    bool
		reason  string
		results []string
	}{
		{"no annotation except the extensions version", base(), community015, true, "", []string{"ui-extensions-version=ok"}},
		{"kube version satisfied", base(AnnotationKubeVersion, ">= 1.30.0-0"), community015, true, "", []string{"kube-version=ok", "ui-extensions-version=ok"}},
		{"kube version not satisfied", base(AnnotationKubeVersion, ">= 1.40.0-0"), community015, false,
			"requires Kubernetes >= 1.40.0-0; the local cluster runs v1.36.5", []string{"kube-version=failed"}},
		{"kube version unread with the annotation", base(AnnotationKubeVersion, ">= 1.30.0-0"), with(community015, func(in *Inputs) { in.KubernetesVersion = "" }), false,
			"the Kubernetes version of the local cluster could not be read", []string{"kube-version=failed"}},
		{"kube version unread without the annotation", base(), with(community015, func(in *Inputs) { in.KubernetesVersion = "" }), true, "", []string{"ui-extensions-version=ok"}},
		{"rancher version satisfied", base(AnnotationRancherVersion, ">= 2.15.0-0"), community015, true, "", []string{"rancher-version=ok", "ui-extensions-version=ok"}},
		{"rancher version not satisfied", base(AnnotationRancherVersion, ">= 2.16.0-0"), community015, false,
			"requires Rancher >= 2.16.0-0; this server is v2.15.3", []string{"rancher-version=failed"}},
		{"rancher version empty", base(AnnotationRancherVersion, ">= 2.15.0-0"), with(community015, func(in *Inputs) { in.RancherVersion = "" }), false,
			"the Rancher version could not be read", []string{"rancher-version=failed"}},
		{"rancher version without a number", base(AnnotationRancherVersion, ">= 2.15.0-0"), with(community015, func(in *Inputs) { in.RancherVersion = "unknown" }), false,
			"the Rancher version could not be read", []string{"rancher-version=failed"}},
		{"head version passes the range of its minor", base(AnnotationRancherVersion, ">= 2.16.0-0 < 2.17.0-0"), with(community015, func(in *Inputs) { in.RancherVersion = "v2.16-head" }), true, "",
			[]string{"rancher-version=ok", "ui-extensions-version=ok"}},
		{"prerelease version is a head build", base(AnnotationRancherVersion, ">= 2.16.999"), with(community015, func(in *Inputs) { in.RancherVersion = "v2.16.0-alpha1" }), true, "",
			[]string{"rancher-version=ok", "ui-extensions-version=ok"}},
		{"ui version satisfied", base(AnnotationUIVersion, ">= 2.7.2"), community015, true, "", []string{"ui-version=ok", "ui-extensions-version=ok"}},
		{"ui version not satisfied", base(AnnotationUIVersion, ">= 2.20.0"), community015, false,
			"requires Rancher >= 2.20.0; this server is v2.15.3", []string{"ui-version=failed"}},
		{"extensions version missing", ann(), community015, false,
			"has no annotation catalog.cattle.io/ui-extensions-version, which the Rancher UI requires", []string{"ui-extensions-version=failed"}},
		{"extensions version not satisfied", base(AnnotationExtensionsVersion, ">= 4.0.0"), community015, false,
			"requires Extensions API version >= 4.0.0; the Rancher UI of this server has 3.0.12", []string{"ui-extensions-version=failed"}},
		{"extensions version unsatisfied by a prerelease range edge", ann(AnnotationExtensionsVersion, ">= 3.0.5-rc.6"), community015, true, "", []string{"ui-extensions-version=ok"}},
		{"Extensions API version not in the table", base(), with(community015, func(in *Inputs) { in.ExtensionsAPIVersion = "" }), true, "", []string{"ui-extensions-version=notChecked"}},
		{"extensions host set", base(AnnotationExtensionsHost, "rancher-manager"), community015, false,
			"has ui-extensions-host rancher-manager, which is not compatible with the Rancher UI", []string{"ui-extensions-version=ok", "ui-extensions-host=failed"}},
		{"extensions host set to another name", base(AnnotationExtensionsHost, ">=1.0.0"), community015, false,
			"has ui-extensions-host >=1.0.0, which is not compatible with the Rancher UI", []string{"ui-extensions-version=ok", "ui-extensions-host=failed"}},
		{"prime only on prime", base(AnnotationPrimeOnly, "true"), prime, true, "", []string{"ui-extensions-version=ok", "prime-only=ok"}},
		{"prime only on community", base(AnnotationPrimeOnly, "true"), community015, false,
			"requires Rancher Prime", []string{"ui-extensions-version=ok", "prime-only=failed"}},
		{"prime only on an unknown edition", base(AnnotationPrimeOnly, "true"), unknown, false,
			"requires Rancher Prime; the edition of this server could not be read", []string{"ui-extensions-version=ok", "prime-only=failed"}},
		{"prime only false on community", base(AnnotationPrimeOnly, "false"), community015, true, "", []string{"ui-extensions-version=ok"}},
		{"two failures, the first failure is reported", base(AnnotationKubeVersion, ">= 1.40.0-0", AnnotationRancherVersion, ">= 2.20.0"), community015, false,
			"requires Kubernetes >= 1.40.0-0; the local cluster runs v1.36.5", []string{"kube-version=failed"}},
		{"rancher before ui version", base(AnnotationRancherVersion, ">= 2.20.0", AnnotationUIVersion, ">= 2.30.0"), community015, false,
			"requires Rancher >= 2.20.0; this server is v2.15.3", []string{"rancher-version=failed"}},
		{"version before host before prime", base(AnnotationExtensionsVersion, ">= 5.0.0", AnnotationExtensionsHost, "rancher-manager", AnnotationPrimeOnly, "true"), community015, false,
			"requires Extensions API version >= 5.0.0; the Rancher UI of this server has 3.0.12", []string{"ui-extensions-version=failed"}},
		{"host before prime", base(AnnotationExtensionsHost, "rancher-manager", AnnotationPrimeOnly, "true"), community015, false,
			"has ui-extensions-host rancher-manager, which is not compatible with the Rancher UI", []string{"ui-extensions-version=ok", "ui-extensions-host=failed"}},
	}
	for _, tt := range tests {
		got := Compatibility(tt.annots, tt.in)
		assert.Equal(t, tt.want, got.Installable, tt.name)
		assert.Equal(t, tt.reason, got.Reason, tt.name)
		assert.Equal(t, tt.results, results(got), tt.name)
	}
}

// TestCompatibilityCheckFields asserts the fields of a check: the annotation,
// its range and what it was checked against.
func TestCompatibilityCheckFields(t *testing.T) {
	t.Parallel()
	got := Compatibility(ann(
		AnnotationKubeVersion, ">= 1.30.0-0",
		AnnotationRancherVersion, ">= 2.15.0-0",
		AnnotationExtensionsVersion, ">= 3.0.0",
	), community015)
	require.True(t, got.Installable)
	require.Len(t, got.Checks, 3)
	assert.Equal(t, Check{Check: CheckKubeVersion, Annotation: AnnotationKubeVersion, Range: ">= 1.30.0-0", Value: "v1.36.5", Result: ResultOK}, got.Checks[0])
	assert.Equal(t, Check{Check: CheckRancherVersion, Annotation: AnnotationRancherVersion, Range: ">= 2.15.0-0", Value: "2.15.3", Result: ResultOK}, got.Checks[1])
	assert.Equal(t, Check{Check: CheckExtensionsVersion, Annotation: AnnotationExtensionsVersion, Range: ">= 3.0.0", Value: "3.0.12", Result: ResultOK}, got.Checks[2])
}

// TestParseRancherVersion asserts the Rancher UI's reading of the server
// version: the numbers, and a patch of 999 for any version with a "-".
func TestParseRancherVersion(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"v2.15.3":          "2.15.3",
		"2.15.3":           "2.15.3",
		"v2.16-head":       "2.16.999",
		"v2.16.0-alpha1":   "2.16.999",
		"v2.15.4-rc.1":     "2.15.999",
		"v2.16":            "2.16.0",
		"":                 "",
		"head":             "",
		"v2.14.0-ent+meta": "2.14.999",
	} {
		assert.Equal(t, want, ParseRancherVersion(in), in)
	}
}

// TestParseEdition asserts the decoding of the RancherPrime field: only the
// text "true" and "false", ignoring case and space, and Unknown for anything
// else.
func TestParseEdition(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]Edition{
		"true": EditionPrime, "TRUE": EditionPrime, " true ": EditionPrime,
		"false": EditionCommunity, "False": EditionCommunity,
		"": EditionUnknown, "yes": EditionUnknown, "1": EditionUnknown,
	} {
		assert.Equal(t, want, ParseEdition(in), in)
	}
	assert.Equal(t, "prime", EditionPrime.String())
	assert.Equal(t, "community", EditionCommunity.String())
	assert.Equal(t, "unknown", EditionUnknown.String())
}
