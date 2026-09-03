package clierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExitCodeMapping(t *testing.T) {
	cases := map[string]int{
		CodeValidation:           2,
		CodeConfirmationRequired: 2,
		CodeDryRunNotApplicable:  2,
		CodeAmbiguousSelection:   2,
		CodeAuth:                 3,
		CodeNotFound:             4,
		CodeConflict:             5,
		CodeRBAC:                 6,
		CodeGeneric:              1,
		CodeNotLoggedIn:          2,
		CodePrimeRequired:        9,
		CodeTLSTrust:             10,
	}
	for code, want := range cases {
		t.Run(code, func(t *testing.T) {
			assert.Equal(t, want, New(code, "x").ExitCode())
		})
	}
}

func TestBuilderAndError(t *testing.T) {
	e := New(CodeConfirmationRequired, "needs confirm").
		WithHint("re-run with: --apply --yes").
		WithRetryable(true)
	require.Equal(t, "needs confirm", e.Error())
	assert.Equal(t, "re-run with: --apply --yes", e.Hint)
	assert.True(t, e.Retryable)
}

func TestIsNotFound(t *testing.T) {
	assert.True(t, IsNotFound(New(CodeNotFound, "x")))
	assert.False(t, IsNotFound(New(CodeGeneric, "x")))
	assert.False(t, IsNotFound(nil))
}

func TestAllCodesNonEmpty(t *testing.T) {
	assert.Contains(t, AllCodes(), CodeNotLoggedIn)
	assert.Contains(t, AllCodes(), CodeDryRunNotApplicable)
}

// TestAllCodesDropsNotImplemented asserts DR-013: NOT_IMPLEMENTED is a
// permanently-dead branch (no construction site ever advertises it) and has
// been removed. The count asserts the full enum: 10 originals plus
// LOCK_TIMEOUT, PRIME_REQUIRED, TLS_TRUST, and CAPABILITY_UNSUPPORTED.
func TestAllCodesDropsNotImplemented(t *testing.T) {
	codes := AllCodes()
	assert.Len(t, codes, 14)
	assert.NotContains(t, codes, "NOT_IMPLEMENTED")
	assert.Contains(t, codes, CodeLockTimeout)
	assert.Contains(t, codes, CodePrimeRequired)
	assert.Contains(t, codes, CodeTLSTrust)
	assert.Contains(t, codes, CodeCapabilityUnsupported)
}

// TestPrimeRequiredDisjointFromSuccessExits asserts exit 9's explicit
// assignment: 7 is a successful `skill status` result and 8 is
// LOCK_TIMEOUT, so PRIME_REQUIRED ("next free slot") must be 9, and must
// never collide with a successful status (0 or 7).
func TestPrimeRequiredDisjointFromSuccessExits(t *testing.T) {
	exit := ExitCode(CodePrimeRequired)
	assert.Equal(t, 9, exit)
	assert.NotContains(t, []int{0, 7}, exit)
}

// TestExitCodeTableUnaffectedByTextRendering asserts the full code→exit
// mapping: how an error's text is written on stderr never changes its
// process exit status.
func TestExitCodeTableUnaffectedByTextRendering(t *testing.T) {
	want := map[string]int{
		CodeValidation:            2,
		CodeConfirmationRequired:  2,
		CodeDryRunNotApplicable:   2,
		CodeAmbiguousSelection:    2,
		CodeNotLoggedIn:           2,
		CodeAuth:                  3,
		CodeNotFound:              4,
		CodeConflict:              5,
		CodeRBAC:                  6,
		CodeLockTimeout:           8,
		CodePrimeRequired:         9,
		CodeTLSTrust:              10,
		CodeCapabilityUnsupported: 11,
		CodeGeneric:               1,
	}
	assert.Equal(t, want, CodeExits())
}

// stubDetails is a minimal TextDetails implementation for exercising
// WithDetails' envelope serialization independent of any real producer's
// shape.
type stubDetails struct {
	Deleted []string `json:"deleted"`
}

func (stubDetails) TextDetails() []string { return nil }

func TestErrorDetailsSerialization(t *testing.T) {
	e := New(CodeConflict, "partial failure").WithDetails(stubDetails{
		Deleted: []string{"a"},
	})
	b, err := json.Marshal(e)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	details, ok := out["details"].(map[string]any)
	require.True(t, ok, "details must serialize as an object")
	assert.Equal(t, []any{"a"}, details["deleted"])

	// omitempty: absent when unset
	b2, err := json.Marshal(New(CodeNotFound, "x"))
	require.NoError(t, err)
	assert.NotContains(t, string(b2), "details")
}

// TestWithWarningsNotSerializedDirectly asserts that the warning ride never
// appears through a direct json.Marshal of *Error — only a renderer reading
// Warnings() places it, at the envelope's top level, never under data.error.
func TestWithWarningsNotSerializedDirectly(t *testing.T) {
	e := New(CodeGeneric, "boom").WithWarnings("cert expires soon")
	assert.Equal(t, []string{"cert expires soon"}, e.Warnings())
	b, err := json.Marshal(e)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "cert expires soon")
	assert.NotContains(t, string(b), "warnings")
}

// TestWithWarningsAccumulates asserts that repeated calls append, in order.
func TestWithWarningsAccumulates(t *testing.T) {
	e := New(CodeGeneric, "boom").WithWarnings("a", "b").WithWarnings("c")
	assert.Equal(t, []string{"a", "b", "c"}, e.Warnings())
}

// TestWithWarningsPreservedByWithMessageAndWrap asserts that re-messaging an
// error (WithMessage, or Wrap's re-classification) never drops warnings
// already attached to it.
func TestWithWarningsPreservedByWithMessageAndWrap(t *testing.T) {
	t.Run("WithMessage", func(t *testing.T) {
		e := New(CodeGeneric, "boom").WithWarnings("a")
		got := e.WithMessage("new message")
		assert.Equal(t, []string{"a"}, got.Warnings())
	})

	t.Run("Wrap of a classified error", func(t *testing.T) {
		inner := New(CodeConflict, "boom").WithWarnings("a")
		got := Wrap(inner, "new message")
		assert.Equal(t, []string{"a"}, got.Warnings())
	})
}

// TestWrapPreservesClassification asserts the exercise-8/9-fixes review fix:
// command wrappers adding context must not flatten a classified error (a
// LOCK_TIMEOUT from a store lock) into the GENERIC catch-all.
func TestWrapPreservesClassification(t *testing.T) {
	t.Run("classified error keeps code and retryability under the new message", func(t *testing.T) {
		inner := Retryable(CodeLockTimeout, "locking token-cache.yaml: context deadline exceeded")
		got := Wrap(inner, "failed to delete: boom")
		assert.Equal(t, CodeLockTimeout, got.Code)
		assert.True(t, got.Retryable)
		assert.Equal(t, "failed to delete: boom", got.Message)
		assert.Equal(t, 8, got.ExitCode())
	})

	t.Run("wrapped classified error is found through the chain", func(t *testing.T) {
		inner := Retryable(CodeLockTimeout, "locked").WithCause(errors.New("flock"))
		chained := fmt.Errorf("outer: %w", inner)
		got := Wrap(chained, "ctx: outer: locked")
		assert.Equal(t, CodeLockTimeout, got.Code)
		assert.True(t, got.Retryable)
	})

	t.Run("plain error becomes GENERIC with the cause kept", func(t *testing.T) {
		plain := errors.New("disk failure")
		got := Wrap(plain, "ctx: disk failure")
		assert.Equal(t, CodeGeneric, got.Code)
		assert.False(t, got.Retryable)
		assert.ErrorIs(t, got, plain)
	})

	t.Run("does not mutate the wrapped error", func(t *testing.T) {
		inner := Retryable(CodeLockTimeout, "original")
		_ = Wrap(inner, "new message").WithHint("added hint")
		assert.Equal(t, "original", inner.Message)
		assert.Empty(t, inner.Hint)
	})
}

// execArgv runs a hint through /bin/sh with `rancher` shadowed by a shell
// function that prints each argument it receives, separated by a byte no
// shell-quoted value can produce unescaped (0x1e). This recovers the EXACT
// argv a machine hint would invoke — proving it is valid, single-command
// POSIX sh — without running the real binary. Fails the test if hint is not
// executable shell syntax at all (the failure mode a stray placeholder like
// "<url>" produces: sh reads it as redirection).
func execArgv(t *testing.T, hint string) []string {
	t.Helper()
	const sep = "\x1e"
	script := "rancher() { for a in \"$@\"; do printf '%s" + sep + "' \"$a\"; done; }\n" + hint
	out, err := exec.Command("sh", "-c", script).Output()
	require.NoError(t, err, "hint must be valid, executable POSIX sh: %q", hint)
	s := strings.TrimSuffix(string(out), sep)
	if s == "" {
		return nil
	}
	return strings.Split(s, sep)
}

func TestShellQuoteRoundTripsThroughSh(t *testing.T) {
	cases := []string{
		"prod",
		"has space",
		"has'quote",
		"'wrapped'",
		"multi\nline\nvalue",
		"",
		"$(rm -rf /)",
		"`echo pwned`",
		"; rm -rf /",
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			hint := "rancher x " + ShellQuote(c)
			got := execArgv(t, hint)
			require.Len(t, got, 2, "the quoted value must round-trip as exactly one argument")
			assert.Equal(t, "x", got[0])
			assert.Equal(t, c, got[1])
		})
	}
}
