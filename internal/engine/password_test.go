package engine

import (
	"errors"
	"fmt"
	"strings"
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

// passwordFlagInput returns Bool per flag name: a flag is set when its name is in the
// map with true.
type passwordFlagInput map[string]bool

func (i passwordFlagInput) String(string) string    { return "" }
func (i passwordFlagInput) Bool(name string) bool   { return i[name] }
func (i passwordFlagInput) IsSet(name string) bool  { return i[name] }
func (i passwordFlagInput) Strings(string) []string { return nil }
func (i passwordFlagInput) Args() []string          { return nil }

// bothFlags sets --current-password-stdin and --password-stdin.
var bothFlags = passwordFlagInput{"current-password-stdin": true, "password-stdin": true}

// failingSrc fails the test when stdin is read.
type failingSrc struct{ t *testing.T }

func (s failingSrc) Read(string) ([]byte, error) {
	s.t.Helper()
	s.t.Error("stdin was read")
	return nil, errors.New("must not be read")
}

// errorSrc fails every read.
type errorSrc struct{ err error }

func (s errorSrc) Read(string) ([]byte, error) { return nil, s.err }

// requireRefusal asserts err is a VALIDATION error with no hint whose message
// holds want and none of the secret texts.
func requireRefusal(t *testing.T, err error, want string, secrets ...string) {
	t.Helper()
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Empty(t, ce.Hint)
	assert.Contains(t, ce.Message, want)
	for _, secret := range secrets {
		assert.NotContains(t, err.Error(), secret)
		assert.NotContains(t, ce.Message, secret)
	}
}

// TestReadPasswordStdinTrimsExactlyOneNewline asserts the documented trim
// contract: at most one final newline (`\n` or a CRLF pair) is trimmed, and
// a bare final carriage return - or any other byte a password may
// legitimately end with - survives.
func TestReadPasswordStdinTrimsExactlyOneNewline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain newline", "secretValue1\n", "secretValue1"},
		{"crlf pair", "secretValue1\r\n", "secretValue1"},
		{"bare final carriage return survives", "secretValue1\r", "secretValue1\r"},
		{"a carriage return inside survives", "secret\rValue1\n", "secret\rValue1"},
		{"interior whitespace survives", "secret Value1\n", "secret Value1"},
		{"no newline", "secretValue1", "secretValue1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReadPasswordStdin(stdinSrc{body: []byte(tc.in)}, stdinInput{set: true}, "")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestReadPasswordStdinRefusesANewlineInsideTheValue asserts that a value
// that still contains a newline after the one final newline is trimmed is
// refused with VALIDATION and no hint, without the typed text, and that the
// remedy sentence is appended only when it is given.
func TestReadPasswordStdinRefusesANewlineInsideTheValue(t *testing.T) {
	t.Parallel()
	const base = "the password read from stdin has more than one line; --password-stdin takes the password on one line"
	const remedy = "To send the current password too, pass --current-password-stdin and pipe two lines"
	for _, tc := range []struct{ name, in string }{
		{"two newlines at the end", "secretValue1\n\n"},
		{"two lines", "firstLine1\nsecondLine2\n"},
		{"a crlf pair and a newline", "secretValue1\r\n\n"},
		{"two crlf pairs", "secretValue1\r\n\r\n"},
		{"a newline first", "\nsecretValue1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ReadPasswordStdin(stdinSrc{body: []byte(tc.in)}, stdinInput{set: true}, "")
			requireRefusal(t, err, base, "secretValue1", "firstLine1", "secondLine2")
			assert.Equal(t, base, err.(*clierr.Error).Message, "no remedy sentence when none is given")

			_, err = ReadPasswordStdin(stdinSrc{body: []byte(tc.in)}, stdinInput{set: true}, remedy)
			requireRefusal(t, err, base+". "+remedy, "secretValue1", "firstLine1", "secondLine2")
			assert.Equal(t, base+". "+remedy, err.(*clierr.Error).Message)
		})
	}
}

// TestReadPasswordStdinMissingFlagHasNoHint asserts that omitting
// --password-stdin is a VALIDATION error whose message names the
// requirement and that carries no hint: the password is a value the CLI
// does not have, so no paste-valid command exists.
func TestReadPasswordStdinMissingFlagHasNoHint(t *testing.T) {
	t.Parallel()
	_, err := ReadPasswordStdin(stdinSrc{body: []byte("secretValue1\n")}, stdinInput{set: false}, "")
	requireRefusal(t, err, "--password-stdin is required")
}

// TestReadPasswordStdinEmptyIsValidation asserts an empty (or
// newline-only) stdin refuses with VALIDATION before any network call, and
// that the empty refusal comes before the newline refusal.
func TestReadPasswordStdinEmptyIsValidation(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "\n", "\r\n"} {
		_, err := ReadPasswordStdin(stdinSrc{body: []byte(in)}, stdinInput{set: true}, "")
		requireRefusal(t, err, "the password read from stdin is empty")
	}
}

// TestReadPasswordStdinReadErrorIsGeneric asserts a failed read is GENERIC.
func TestReadPasswordStdinReadErrorIsGeneric(t *testing.T) {
	t.Parallel()
	_, err := ReadPasswordStdin(errorSrc{err: errors.New("boom")}, stdinInput{set: true}, "")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Equal(t, "reading --password-stdin: boom", ce.Message)
}

// TestReadCurrentAndNewPasswordStdinAccepts asserts the two-line input forms
// the reader accepts and the values it returns: each newline is `\n` or
// a CRLF pair, one final newline is trimmed, a carriage return that is not
// part of a newline is kept, and the byte order mark and a long line 2 are
// kept.
func TestReadCurrentAndNewPasswordStdinAccepts(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 1<<20)
	for _, tc := range []struct {
		name, in, current, newPassword string
	}{
		{"no final newline", "a\nb", "a", "b"},
		{"a final newline", "a\nb\n", "a", "b"},
		{"crlf newlines", "a\r\nb\r\n", "a", "b"},
		{"crlf and a final newline", "a\nb\r\n", "a", "b"},
		{"a crlf and no final newline", "a\r\nb", "a", "b"},
		{"a bare final carriage return stays in line 2", "a\nb\r", "a", "b\r"},
		{"a carriage return inside a line stays", "a\rb\nc", "a\rb", "c"},
		{"a byte order mark stays in line 1", "\ufeffa\nb\n", "\ufeffa", "b"},
		{"the two passwords PowerShell 7 pipes on Windows", "cur rent$HOME\\x\r\nnew pa$$ss\\y\r\n", "cur rent$HOME\\x", "new pa$$ss\\y"},
		{"the byte order mark Windows PowerShell 5.1 sends is part of the first password", "\ufeffcur rent$HOME\\x\r\nnew pa$$ss\\y\r\n", "\ufeffcur rent$HOME\\x", "new pa$$ss\\y"},
		{"a line 2 of 1 MiB", "a\n" + long + "\n", "a", long},
		{"interior whitespace stays", "a b\n c \n", "a b", " c "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current, newPassword, err := ReadCurrentAndNewPasswordStdin(stdinSrc{body: []byte(tc.in)}, bothFlags)
			require.NoError(t, err)
			assert.Equal(t, tc.current, current)
			assert.Equal(t, tc.newPassword, newPassword)
		})
	}
}

// TestReadCurrentAndNewPasswordStdinRefuses asserts every refused input:
// VALIDATION, no hint, a message that names the count or the line and holds
// no typed text.
func TestReadCurrentAndNewPasswordStdinRefuses(t *testing.T) {
	t.Parallel()
	const stdinEmpty = "stdin is empty; with --current-password-stdin, pipe the current password and the new password, one per line"
	many := strings.Repeat("lineText\n", 999) + "lineText"
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", stdinEmpty},
		{"a newline only", "\n", "stdin must have two lines, the current password and then the new password; lines found: 1"},
		{"one line", "secretOne", "lines found: 1"},
		{"one line with a final newline", "secretOne\n", "lines found: 1"},
		{"a lone carriage return is not a newline", "secret\rOne", "lines found: 1"},
		{"an empty line after line 2", "secretOne\nsecretTwo\n\n", "lines found: 3"},
		{"three lines", "secretOne\nsecretTwo\nsecretThree", "lines found: 3"},
		{"many lines", many, "lines found: 1000"},
		{"line 1 empty", "\nsecretTwo", "line 1 (the current password) is empty"},
		{"line 1 empty with a crlf", "\r\nsecretTwo", "line 1 (the current password) is empty"},
		{"line 2 empty", "secretOne\n\n", "line 2 (the new password) is empty"},
		{"line 2 empty with a crlf", "secretOne\r\n\r\n", "line 2 (the new password) is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := ReadCurrentAndNewPasswordStdin(stdinSrc{body: []byte(tc.in)}, bothFlags)
			requireRefusal(t, err, tc.want, "secretOne", "secretTwo", "secretThree", "lineText")
		})
	}
}

// TestReadCurrentAndNewPasswordStdinCountsEveryLine asserts the message holds
// the count of lines and nothing else from the input.
func TestReadCurrentAndNewPasswordStdinCountsEveryLine(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 3, 4, 10} {
		in := strings.TrimSuffix(strings.Repeat("secretLine\n", n), "\n")
		_, _, err := ReadCurrentAndNewPasswordStdin(stdinSrc{body: []byte(in)}, bothFlags)
		requireRefusal(t, err, fmt.Sprintf("lines found: %d", n), "secretLine")
		assert.Equal(t, fmt.Sprintf("with --current-password-stdin, stdin must have two lines, the current password and then the new password; lines found: %d", n), err.(*clierr.Error).Message)
	}
}

// TestReadCurrentAndNewPasswordStdinNeedsPasswordStdin asserts that the flag
// check comes first and reads no stdin.
func TestReadCurrentAndNewPasswordStdinNeedsPasswordStdin(t *testing.T) {
	t.Parallel()
	for _, in := range []passwordFlagInput{{"current-password-stdin": true}, {}} {
		_, _, err := ReadCurrentAndNewPasswordStdin(failingSrc{t}, in)
		requireRefusal(t, err, "--current-password-stdin needs --password-stdin: pipe the current password and the new password, one per line")
	}
}

// TestReadCurrentAndNewPasswordStdinReadErrorIsGeneric asserts a failed read
// is GENERIC.
func TestReadCurrentAndNewPasswordStdinReadErrorIsGeneric(t *testing.T) {
	t.Parallel()
	_, _, err := ReadCurrentAndNewPasswordStdin(errorSrc{err: errors.New("boom")}, bothFlags)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Equal(t, "reading --password-stdin: boom", ce.Message)
}
