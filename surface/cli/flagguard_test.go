package cli

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckFlagValuesIncident(t *testing.T) {
	t.Parallel()
	// Exact incident: DEMO_CC unset → --cloud-credential --k8s-version v1.35.7+rke2r1
	flags := []runtime.ArgSpec{
		{Name: "cloud-credential", Type: "string"},
		{Name: "k8s-version", Type: "string"},
	}
	argv := []string{"create", "cluster", "--cloud-credential", "--k8s-version", "v1.35.7+rke2r1"}
	ce := checkFlagValues(flags, argv, false)
	require.NotNil(t, ce)
	assert.Equal(t, "VALIDATION", ce.Code)
	assert.Contains(t, ce.Message, "--cloud-credential")
	assert.Contains(t, ce.Message, "--k8s-version")
	assert.Contains(t, ce.Hint, "value for --cloud-credential")
	assert.Contains(t, ce.Hint, "--cloud-credential=<value>")
}

func TestCheckFlagValuesEqualsFormPasses(t *testing.T) {
	t.Parallel()
	// --flag=--anything is a single token; explicit intent, must pass.
	flags := []runtime.ArgSpec{{Name: "cloud-credential", Type: "string"}}
	argv := []string{"create", "cluster", "--cloud-credential=--k8s-version"}
	ce := checkFlagValues(flags, argv, false)
	assert.Nil(t, ce)
}

func TestCheckFlagValuesShortFlagNormalValue(t *testing.T) {
	t.Parallel()
	// -o json — short alias, normal non-flag value; must pass.
	ce := checkFlagValues(nil, []string{"get", "cluster", "-o", "json"}, false)
	assert.Nil(t, ce)
}

func TestCheckFlagValuesNegativeNumberPasses(t *testing.T) {
	t.Parallel()
	// -1 is not a registered flag; must pass as a string value.
	flags := []runtime.ArgSpec{{Name: "count", Type: "string"}}
	ce := checkFlagValues(flags, []string{"--count", "-1"}, false)
	assert.Nil(t, ce)
}

func TestCheckFlagValuesCompletionSkips(t *testing.T) {
	t.Parallel()
	// inCompletion=true → shell completion path; guard must never fire.
	flags := []runtime.ArgSpec{
		{Name: "cloud-credential", Type: "string"},
		{Name: "k8s-version", Type: "string"},
	}
	argv := []string{"create", "cluster", "--cloud-credential", "--k8s-version"}
	ce := checkFlagValues(flags, argv, true)
	assert.Nil(t, ce, "guard must not fire during completion")
}

func TestCheckFlagValuesEndOfFlagsMarker(t *testing.T) {
	t.Parallel()
	// Tokens after "--" are positional; scan stops, no false positive.
	flags := []runtime.ArgSpec{{Name: "cloud-credential", Type: "string"}}
	argv := []string{"create", "cluster", "--", "--cloud-credential", "--dry-run"}
	ce := checkFlagValues(flags, argv, false)
	assert.Nil(t, ce)
}

func TestCheckFlagValuesBoolFlagAdjacency(t *testing.T) {
	t.Parallel()
	// --dry-run (bool) followed immediately by --cloud-credential (value-taking
	// with a real value): no fire — the bool flag has no next-value slot to eat.
	flags := []runtime.ArgSpec{
		{Name: "cloud-credential", Type: "string"},
	}
	argv := []string{"create", "cluster", "--dry-run", "--cloud-credential", "prod-cc"}
	ce := checkFlagValues(flags, argv, false)
	assert.Nil(t, ce, "bool flag adjacent to value-taking flag must not fire")
}

func TestCheckFlagValuesV1DanglingFlagNoFire(t *testing.T) {
	t.Parallel()
	// v1-shaped rawArgv ending in a dangling value-taking flag: the i+1 <
	// len(argv) guard means there is no next token to examine — no fire.
	flags := []runtime.ArgSpec{{Name: "cloud-credential", Type: "string"}}
	argv := []string{"create", "cluster", "--cloud-credential"}
	ce := checkFlagValues(flags, argv, false)
	assert.Nil(t, ce, "dangling last flag with no next token must not fire")
}

func TestCheckFlagValuesV1CompletionSuppressed(t *testing.T) {
	// NOT parallel: writes rawV1Completion (process-global).
	// v1 completion path: rawV1Completion set, inCompletion=completionActive()=true.
	// Even a genuine --flag --flag pattern must not fire during completion.
	rawV1Completion = true
	t.Cleanup(func() { rawV1Completion = false })
	flags := []runtime.ArgSpec{
		{Name: "cloud-credential", Type: "string"},
		{Name: "k8s-version", Type: "string"},
	}
	argv := []string{"create", "cluster", "--cloud-credential", "--k8s-version"}
	ce := checkFlagValues(flags, argv, completionActive())
	assert.Nil(t, ce, "guard must not fire during v1 completion")
}
