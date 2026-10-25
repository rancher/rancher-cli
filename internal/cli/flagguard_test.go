package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

func TestCheckFlagValuesUnsetVariableBeforeFlag(t *testing.T) {
	t.Parallel()
	// An unset shell variable after --cloud-credential leaves the flag
	// followed by the next one:
	// --cloud-credential --k8s-version v1.35.7+rke2r1
	flags := []runtime.ArgSpec{
		{Name: "cloud-credential", Type: "string"},
		{Name: "k8s-version", Type: "string"},
	}
	argv := []string{"create", "cluster", "--cloud-credential", "--k8s-version", "v1.35.7+rke2r1"}
	ce := checkFlagValues(flags, argv, false, false)
	require.NotNil(t, ce)
	assert.Equal(t, "VALIDATION", ce.Code)
	assert.Contains(t, ce.Message, "--cloud-credential")
	assert.Contains(t, ce.Message, "--k8s-version")
	assert.Contains(t, ce.Message, "value for --cloud-credential")
	assert.Contains(t, ce.Message, "--cloud-credential=<value>")
	assert.Empty(t, ce.Hint, "the empty-shell-variable guidance lives in the message, not a hint - <value> is a genuine placeholder, never a known literal")
}

func TestCheckFlagValuesEqualsFormPasses(t *testing.T) {
	t.Parallel()
	// --flag=--anything is a single token; explicit intent, must pass.
	flags := []runtime.ArgSpec{{Name: "cloud-credential", Type: "string"}}
	argv := []string{"create", "cluster", "--cloud-credential=--k8s-version"}
	ce := checkFlagValues(flags, argv, false, false)
	assert.Nil(t, ce)
}

func TestCheckFlagValuesShortFlagNormalValue(t *testing.T) {
	t.Parallel()
	// -o json - short alias, normal non-flag value; must pass.
	ce := checkFlagValues(nil, []string{"get", "cluster", "-o", "json"}, false, false)
	assert.Nil(t, ce)
}

func TestCheckFlagValuesNegativeNumberPasses(t *testing.T) {
	t.Parallel()
	// -1 is not a registered flag; must pass as a string value.
	flags := []runtime.ArgSpec{{Name: "count", Type: "string"}}
	ce := checkFlagValues(flags, []string{"--count", "-1"}, false, false)
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
	ce := checkFlagValues(flags, argv, true, false)
	assert.Nil(t, ce, "guard must not fire during completion")
}

func TestCheckFlagValuesEndOfFlagsMarker(t *testing.T) {
	t.Parallel()
	// Tokens after "--" are positional; scan stops, no false positive.
	flags := []runtime.ArgSpec{{Name: "cloud-credential", Type: "string"}}
	argv := []string{"create", "cluster", "--", "--cloud-credential", "--dry-run"}
	ce := checkFlagValues(flags, argv, false, false)
	assert.Nil(t, ce)
}

func TestCheckFlagValuesBoolFlagAdjacency(t *testing.T) {
	t.Parallel()
	// --dry-run (bool) followed immediately by --cloud-credential (value-taking
	// with a real value): no fire - the bool flag has no next-value slot to eat.
	flags := []runtime.ArgSpec{
		{Name: "cloud-credential", Type: "string"},
	}
	argv := []string{"create", "cluster", "--dry-run", "--cloud-credential", "prod-cc"}
	ce := checkFlagValues(flags, argv, false, false)
	assert.Nil(t, ce, "bool flag adjacent to value-taking flag must not fire")
}

func TestCheckFlagValuesDanglingFlagNoFire(t *testing.T) {
	t.Parallel()
	// rawArgv ending in a dangling value-taking flag: the i+1 <
	// len(argv) guard means there is no next token to examine - no fire.
	flags := []runtime.ArgSpec{{Name: "cloud-credential", Type: "string"}}
	argv := []string{"create", "cluster", "--cloud-credential"}
	ce := checkFlagValues(flags, argv, false, false)
	assert.Nil(t, ce, "dangling last flag with no next token must not fire")
}

func TestCheckFlagValuesCompletionRequestSuppressed(t *testing.T) {
	// NOT parallel: writes the argv globals (process-global).
	// A completion request: CaptureArgv sets rawCompleting, so
	// inCompletion=completionActive()=true. Even a genuine --flag --flag
	// pattern must not fire during completion.
	t.Cleanup(func() { CaptureArgv(nil) })
	CaptureArgv([]string{protoSentinel, "create", "cluster", "--cloud-credential", "--k8s-version", curPrefix, completionFlag})
	flags := []runtime.ArgSpec{
		{Name: "cloud-credential", Type: "string"},
		{Name: "k8s-version", Type: "string"},
	}
	argv := []string{"create", "cluster", "--cloud-credential", "--k8s-version"}
	ce := checkFlagValues(flags, argv, completionActive(), false)
	assert.Nil(t, ce, "guard must not fire during completion")
}

// TestCheckFlagValuesBeforeHookRendersPerParsedFormat asserts the Before hook's
// row of the pre-pipeline format table: flags have already parsed
// successfully by the time checkFlagValues fires, so its rendering follows
// the invocation's own parsed --output value directly (no raw-argv scan).
func TestCheckFlagValuesBeforeHookRendersPerParsedFormat(t *testing.T) {
	incidentArgv := []string{
		"rancher", "create", "cluster", "--cloud-credential", "--k8s-version", "v1.35.7+rke2r1",
	}

	t.Run("no -o: text", func(t *testing.T) {
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		root.Writer = &out
		root.ErrWriter = &errOut
		root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

		CaptureArgv(incidentArgv[1:])
		runErr := root.Run(t.Context(), incidentArgv)
		require.Error(t, runErr)
		assert.Contains(t, errOut.String(), "error (VALIDATION):")
		assert.Contains(t, errOut.String(), "--cloud-credential")
		assert.NotContains(t, errOut.String(), "apiVersion")
	})

	t.Run("-o json: envelope", func(t *testing.T) {
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		root.Writer = &out
		root.ErrWriter = &errOut
		root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

		argv := append(append([]string{}, incidentArgv...), "-o", "json")
		CaptureArgv(argv[1:])
		runErr := root.Run(t.Context(), argv)
		require.Error(t, runErr)
		var env errEnvelope
		require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
		assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	})
}

// TestCheckFlagValuesStopsAtABareDash asserts that the scan reads no word
// after a bare "-", padded or not, since a line with one is refused before
// the command runs, and that a "-"
// typed as a flag's value is a value, after which the scan goes on.
func TestCheckFlagValuesStopsAtABareDash(t *testing.T) {
	t.Parallel()
	flags := []runtime.ArgSpec{
		{Name: "cloud-credential", Type: "string"},
		{Name: "k8s-version", Type: "string"},
	}
	for _, dash := range []string{"-", " - ", "\t-"} {
		argv := []string{"create", "cluster", dash, "--cloud-credential", "--k8s-version", "v1"}
		assert.Nil(t, checkFlagValues(flags, argv, false, false), "%q", argv)
	}
	argv := []string{"create", "cluster", "--cloud-credential", "-", "--k8s-version", "--dry-run"}
	ce := checkFlagValues(flags, argv, false, false)
	require.NotNil(t, ce, "%q", argv)
	assert.Contains(t, ce.Message, "flag --k8s-version got flag --dry-run as its value")
}

// TestCheckFlagValuesRefusesAFlagWithEqualsTakenAsAValue asserts that, on a
// command that takes a secret input, a value-taking flag left without its
// value whose next word is a registered flag spelling written with "="
// (long, alias and single-dash forms) is refused with the two flag
// spellings and nothing that follows the "="; that the same words pass on
// an ordinary command, that a next word with "=" whose part before it is
// not a registered flag passes, that the equals form of the first flag
// passes on both, and that a literal value that starts with a registered
// flag spelling and "=" is refused and the equals form of it passes.
func TestCheckFlagValuesRefusesAFlagWithEqualsTakenAsAValue(t *testing.T) {
	t.Parallel()
	const secret = "s3cretcanary9"
	flags := []runtime.ArgSpec{
		{Name: "cluster", Type: "string"},
		{Name: "from-literal", Type: "string", Aliases: []string{"l"}},
	}
	for _, tc := range []struct {
		name         string
		argv         []string
		first, other string
	}{
		{"a long flag", []string{"create", "secret", "s1", "--cluster", "--from-literal=k=" + secret}, "--cluster", "--from-literal"},
		{"an alias", []string{"create", "secret", "s1", "--cluster", "-l=k=" + secret}, "--cluster", "-l"},
		{"a long flag with one dash", []string{"create", "secret", "s1", "-cluster", "-from-literal=k=" + secret}, "-cluster", "-from-literal"},
		{"a global flag given a flag", []string{"create", "secret", "s1", "--project", "--cluster=" + secret}, "--project", "--cluster"},
		{"a literal value that starts with a flag spelling", []string{"create", "secret", "s1", "--from-literal", "-l=" + secret}, "--from-literal", "-l"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := checkFlagValues(flags, tc.argv, false, true)
			require.NotNil(t, ce)
			assert.Equal(t, "VALIDATION", ce.Code)
			assert.Equal(t, "flag "+tc.first+" got flag "+tc.other+" as its value - empty shell variable? check that the value for "+tc.first+
				" is set, or use "+tc.first+"=<value> to pass a literal flag-like string", ce.Message)
			assert.NotContains(t, ce.Message, secret)
			assert.NotContains(t, ce.Message, "k=")
			assert.Empty(t, ce.Hint)

			assert.Nil(t, checkFlagValues(flags, tc.argv, false, false), "an ordinary command takes the word as the value")
		})
	}

	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"a word that is not a registered flag", []string{"create", "secret", "s1", "--cluster", "-x=1"}},
		{"a flag spelling joined to its flag with =", []string{"create", "secret", "s1", "--cluster=--from-literal=k=v"}},
		{"the equals form of a literal that starts with a flag spelling", []string{"create", "secret", "s1", "--from-literal=-l=v"}},
		{"a value with no dash", []string{"create", "secret", "s1", "--cluster", "c-x", "--from-literal", "k=v"}},
	} {
		t.Run(tc.name+" passes on every command", func(t *testing.T) {
			t.Parallel()
			assert.Nil(t, checkFlagValues(flags, tc.argv, false, true))
			assert.Nil(t, checkFlagValues(flags, tc.argv, false, false))
		})
	}
}
