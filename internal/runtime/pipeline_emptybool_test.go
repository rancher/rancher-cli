package runtime

import (
	"bytes"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// emptyBoolInput is a mapInput that also answers EmptyBoolFlags.
type emptyBoolInput struct {
	mapInput
	flags []string
	ok    bool
}

func (e emptyBoolInput) EmptyBoolFlags() ([]string, bool) { return e.flags, e.ok }

// TestRunRefusesAnEmptyBoolValue asserts that the pipeline refuses, before
// the command runs and ahead of the read-only and dry-run gates, every bool
// flag the Input reports as typed with "=" and an empty value: VALIDATION
// at exit 2, a message naming each flag once, as typed, and a hint saying
// to write the flag alone or with true or false. An Input that reports no
// such flag, or does not answer EmptyBoolFlags, runs the command.
func TestRunRefusesAnEmptyBoolValue(t *testing.T) {
	t.Parallel()
	read := Descriptor{Name: "get pod", CommandType: ReadOnly}
	mutating := Descriptor{Name: "create user", CommandType: Mutating}
	const rule = " an empty value; a bool flag typed with = takes true or false"

	for _, tc := range []struct {
		name    string
		d       Descriptor
		streams IO
		flags   []string
		message string
		hint    string
	}{
		{
			name:    "one flag",
			d:       read,
			flags:   []string{"--wait"},
			message: "--wait= has" + rule,
			hint:    "write --wait or --wait=true|false",
		},
		{
			name:    "a short alias",
			d:       read,
			flags:   []string{"-A"},
			message: "-A= has" + rule,
			hint:    "write -A or -A=true|false",
		},
		{
			name:    "ahead of the dry-run gate of a command that is not mutating",
			d:       read,
			streams: IO{DryRun: true},
			flags:   []string{"--dry-run"},
			message: "--dry-run= has" + rule,
			hint:    "write --dry-run or --dry-run=true|false",
		},
		{
			name:    "ahead of the read-only gate of a mutating command",
			d:       mutating,
			streams: IO{ReadOnly: true, ReadOnlySources: []string{"--read-only"}},
			flags:   []string{"--read-only"},
			message: "--read-only= has" + rule,
			hint:    "write --read-only or --read-only=true|false",
		},
		{
			name:    "two flags, one typed twice",
			d:       mutating,
			flags:   []string{"--apply", "--yes", "--apply"},
			message: "--apply=, --yes= have" + rule,
			hint:    "write --apply or --apply=true|false; --yes or --yes=true|false",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runs := 0
			var out, errBuf bytes.Buffer
			streams := tc.streams
			streams.Out, streams.Err, streams.Format = &out, &errBuf, "json"
			_, ce := Run(t.Context(), countingRead{d: tc.d, runs: &runs}, emptyBoolInput{flags: tc.flags, ok: true}, streams, safety.Binding{})
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.Equal(t, 2, ce.ExitCode())
			assert.Zero(t, runs, "the command must not run")
		})
	}

	for name, in := range map[string]Input{
		"the Input finds no empty bool value": emptyBoolInput{flags: []string{"--wait"}},
		"the Input cannot tell":               mapInput{},
	} {
		t.Run(name+" runs", func(t *testing.T) {
			t.Parallel()
			runs := 0
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), countingRead{d: read, runs: &runs}, in, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
			require.Nil(t, ce)
			assert.Equal(t, 1, runs)
		})
	}
}
