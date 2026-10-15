package runtime

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
)

// TestEmptyBoolErrorNamesEachFlagOnce asserts the refusal of bool flags
// typed with "=" and an empty value: VALIDATION at exit 2, a message naming
// each flag once, as typed, and a hint saying to write the flag alone or
// with true or false.
func TestEmptyBoolErrorNamesEachFlagOnce(t *testing.T) {
	t.Parallel()
	const rule = " an empty value; a bool flag typed with = takes true or false"
	for _, tc := range []struct {
		name    string
		flags   []string
		message string
		hint    string
	}{
		{"one flag", []string{"--wait"}, "--wait= has" + rule, "write --wait or --wait=true|false"},
		{"a short alias", []string{"-A"}, "-A= has" + rule, "write -A or -A=true|false"},
		{"two flags, one typed twice", []string{"--apply", "--yes", "--apply"}, "--apply=, --yes= have" + rule, "write --apply or --apply=true|false; --yes or --yes=true|false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := EmptyBoolError(tc.flags)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.Equal(t, 2, ce.ExitCode())
		})
	}
}
