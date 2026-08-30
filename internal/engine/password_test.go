package engine

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stdinSrc struct{ body []byte }

func (s stdinSrc) Read(string) ([]byte, error) { return s.body, nil }

type stdinInput struct{ set bool }

func (i stdinInput) String(string) string    { return "" }
func (i stdinInput) Bool(string) bool        { return i.set }
func (i stdinInput) IsSet(string) bool       { return i.set }
func (i stdinInput) Strings(string) []string { return nil }
func (i stdinInput) Args() []string          { return nil }

// TestReadPasswordStdinTrimsExactlyOneLineEnding asserts the documented
// trim contract: at most one trailing newline or CRLF pair is removed,
// and a bare trailing carriage return — or any other byte a password may
// legitimately end with — survives.
func TestReadPasswordStdinTrimsExactlyOneLineEnding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain newline", "secretValue1\n", "secretValue1"},
		{"crlf pair", "secretValue1\r\n", "secretValue1"},
		{"bare trailing carriage return survives", "secretValue1\r", "secretValue1\r"},
		{"only one newline trimmed", "secretValue1\n\n", "secretValue1\n"},
		{"interior whitespace survives", "secret Value1\n", "secret Value1"},
		{"no line ending", "secretValue1", "secretValue1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReadPasswordStdin(stdinSrc{body: []byte(tc.in)}, stdinInput{set: true})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestReadPasswordStdinEmptyIsValidation asserts an empty (or
// newline-only) stdin refuses with VALIDATION before any network call.
func TestReadPasswordStdinEmptyIsValidation(t *testing.T) {
	t.Parallel()
	_, err := ReadPasswordStdin(stdinSrc{body: []byte("\n")}, stdinInput{set: true})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}
