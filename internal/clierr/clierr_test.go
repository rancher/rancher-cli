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

// TestCodesDescribeEveryCode asserts that Codes lists exactly the codes
// AllCodes lists, in order, that every one has a meaning, that no meaning
// exists for a code AllCodes does not list, and that each meaning is one
// plain line that reads as a table cell. The exit status of each entry is
// asserted against the manifest's exit map in the runtime package.
func TestCodesDescribeEveryCode(t *testing.T) {
	t.Parallel()
	all := AllCodes()
	infos := Codes()
	codes := make([]string, 0, len(infos))
	for _, info := range infos {
		codes = append(codes, info.Code)
	}
	assert.Equal(t, all, codes, "Codes lists exactly AllCodes, in order")
	for _, info := range infos {
		assert.NotEmpty(t, info.Desc, "%s has no description", info.Code)
		assert.NotContains(t, info.Desc, "\n", "%s: one line", info.Code)
		assert.NotContains(t, info.Desc, "|", "%s: no table separator", info.Code)
		assert.False(t, strings.HasSuffix(info.Desc, "."), "%s: a fragment, no trailing period", info.Code)
	}
	for code := range codeDescs {
		assert.Contains(t, all, code, "description for a code AllCodes does not list")
	}
}

// TestAllCodesDropsNotImplemented asserts that NOT_IMPLEMENTED, a
// permanently-dead branch (no construction site ever advertises it), is absent
// from the enum. The count asserts the full enum: 10 originals plus
// LOCK_TIMEOUT, PRIME_REQUIRED, TLS_TRUST, CAPABILITY_UNSUPPORTED,
// INTERRUPTED, CREDENTIAL_REPLACED, LOGIN_INCOMPLETE,
// CREDENTIAL_TRANSITION_INCOMPLETE and CREDENTIAL_STORE_UNAVAILABLE.
func TestAllCodesDropsNotImplemented(t *testing.T) {
	codes := AllCodes()
	assert.Len(t, codes, 19)
	assert.NotContains(t, codes, "NOT_IMPLEMENTED")
	assert.Contains(t, codes, CodeLockTimeout)
	assert.Contains(t, codes, CodePrimeRequired)
	assert.Contains(t, codes, CodeTLSTrust)
	assert.Contains(t, codes, CodeCapabilityUnsupported)
	assert.Contains(t, codes, CodeCredentialReplaced)
	assert.Contains(t, codes, CodeLoginIncomplete)
	assert.Contains(t, codes, CodeCredentialTransitionIncomplete)
	assert.Contains(t, codes, CodeCredentialStoreUnavailable)
}

// TestCredentialCodesRetryability asserts the pair a caller branches on: a
// login that did not finish is worth retrying, a credential another
// directory replaced is not, and both end the process the way a failed
// authentication does.
func TestCredentialCodesRetryability(t *testing.T) {
	replaced := New(CodeCredentialReplaced, "replaced")
	assert.Equal(t, 3, replaced.ExitCode())
	assert.False(t, replaced.Retryable)

	incomplete := Retryable(CodeLoginIncomplete, "in progress")
	assert.Equal(t, 3, incomplete.ExitCode())
	assert.True(t, incomplete.Retryable)
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
		CodeValidation:                     2,
		CodeConfirmationRequired:           2,
		CodeDryRunNotApplicable:            2,
		CodeAmbiguousSelection:             2,
		CodeNotLoggedIn:                    2,
		CodeAuth:                           3,
		CodeCredentialReplaced:             3,
		CodeLoginIncomplete:                3,
		CodeCredentialTransitionIncomplete: 3,
		CodeNotFound:                       4,
		CodeConflict:                       5,
		CodeRBAC:                           6,
		CodeLockTimeout:                    8,
		CodePrimeRequired:                  9,
		CodeTLSTrust:                       10,
		CodeCapabilityUnsupported:          11,
		CodeCredentialStoreUnavailable:     14,
		CodeInterrupted:                    130,
		CodeGeneric:                        1,
	}
	assert.Equal(t, want, CodeExits())
}

// stubDetails is a minimal TextDetails implementation for exercising
// WithDetails' envelope serialization independent of any real producer's
// shape.
type stubDetails struct {
	Deleted []string `json:"deleted"`
}

func (stubDetails) TextDetails() []DetailLine { return nil }

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
// appears through a direct json.Marshal of *Error - only a renderer reading
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

// TestWrapPreservesClassification asserts that
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
// argv a machine hint would invoke - proving it is valid, single-command
// POSIX sh - without running the real binary. Fails the test if hint is not
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

var shellQuoteArgsHostile = []string{
	"x", "-h", "--", "--x", "-1", "-", " -h", "\u2063-h", "a b", "a'b", "multi\nline", "",
}

// TestShellQuoteArgs asserts the exact text of the helper for each hostile
// value, with and without flags, and for two values with one starting with a
// dash.
func TestQuoteArgsOfTheBuilderHelper(t *testing.T) {
	for _, v := range shellQuoteArgsHostile {
		dashed := strings.HasPrefix(strings.TrimSpace(strings.ReplaceAll(v, "\u2063", "")), "-")
		t.Run(fmt.Sprintf("%q", v), func(t *testing.T) {
			if !dashed {
				assert.Equal(t, " "+ShellQuote(v), shellQuoteArgs("", v))
				assert.Equal(t, " "+ShellQuote(v)+" --apply --yes", shellQuoteArgs(" --apply --yes", v))
				return
			}
			assert.Equal(t, " -- "+ShellQuote(v), shellQuoteArgs("", v))
			assert.Equal(t, " --apply --yes -- "+ShellQuote(v), shellQuoteArgs(" --apply --yes", v))
		})
	}
	assert.Equal(t, " -- '-h'", shellQuoteArgs("", "-h"))
	assert.Equal(t, " -- '-h' 'x'", shellQuoteArgs("", "-h", "x"))
	assert.Equal(t, " -- 'x' '-h'", shellQuoteArgs("", "x", "-h"))
	assert.Equal(t, " --dry-run -- 'x' '-h'", shellQuoteArgs(" --dry-run", "x", "-h"))
	assert.Equal(t, " 'x' 'y' --dry-run", shellQuoteArgs(" --dry-run", "x", "y"))
	assert.Equal(t, " ''", shellQuoteArgs("", ""))
}

// TestQuoteArgsTestsTheValueAsShellQuoteWritesIt asserts that a value
// that is U+2063 followed by "-h" prints as '-h' and gets the "--" form, and
// that a value with the separator in the middle of an ordinary word does not.
func TestQuoteArgsTestsTheValueAsShellQuoteWritesIt(t *testing.T) {
	assert.Equal(t, " -- '-h'", shellQuoteArgs("", "\u2063-h"))
	assert.Equal(t, " 'ab' --x", shellQuoteArgs(" --x", "a\u2063b"))
}

// TestInterruptedShape asserts the interrupt error's contract: its own
// code, exit 130, not retryable, no hint.
func TestInterruptedShape(t *testing.T) {
	t.Parallel()
	ce := Interrupted()
	assert.Equal(t, CodeInterrupted, ce.Code)
	assert.Equal(t, 130, ce.ExitCode())
	assert.False(t, ce.Retryable)
	assert.Empty(t, ce.Hint)
	assert.Contains(t, AllCodes(), CodeInterrupted)
}

func candidateRange(n int) []Candidate {
	out := make([]Candidate, n)
	for i := range out {
		out[i] = Candidate{ID: fmt.Sprintf("id-%d", i), Name: fmt.Sprintf("name-%d", i)}
	}
	return out
}

// TestWithCandidatesCutSetsTheTotalOnlyWhenEntriesWereDropped asserts the
// helper shows the first limit entries and sets the total to the size of
// the full list only when it is longer than the limit.
func TestWithCandidatesCutSetsTheTotalOnlyWhenEntriesWereDropped(t *testing.T) {
	cases := []struct {
		name      string
		full      []Candidate
		limit     int
		wantShown int
		wantTotal int
	}{
		{name: "nil list", full: nil, limit: 10, wantShown: 0, wantTotal: 0},
		{name: "empty list", full: []Candidate{}, limit: 10, wantShown: 0, wantTotal: 0},
		{name: "below the limit", full: candidateRange(3), limit: 10, wantShown: 3, wantTotal: 0},
		{name: "at the limit", full: candidateRange(10), limit: 10, wantShown: 10, wantTotal: 0},
		{name: "one over the limit", full: candidateRange(11), limit: 10, wantShown: 10, wantTotal: 11},
		{name: "far over the limit", full: candidateRange(37), limit: 10, wantShown: 10, wantTotal: 37},
		{name: "zero limit", full: candidateRange(4), limit: 0, wantShown: 0, wantTotal: 4},
		{name: "negative limit counts as zero", full: candidateRange(4), limit: -3, wantShown: 0, wantTotal: 4},
		{name: "negative limit with an empty list", full: nil, limit: -1, wantShown: 0, wantTotal: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := New(CodeNotFound, "x").WithCandidatesCut(tc.full, tc.limit)
			assert.Len(t, e.Candidates, tc.wantShown)
			assert.Equal(t, tc.wantTotal, e.CandidatesTotal)
			if tc.wantShown > 0 {
				assert.Equal(t, tc.full[:tc.wantShown], e.Candidates, "the first entries, in order")
			}
		})
	}
}

// TestWithCandidatesCutLeavesTheFullListAlone asserts appending to the cut
// candidates does not write into the caller's full list.
func TestWithCandidatesCutLeavesTheFullListAlone(t *testing.T) {
	full := candidateRange(12)
	want := full[10]
	e := New(CodeNotFound, "x").WithCandidatesCut(full, 10)
	e.Candidates = append(e.Candidates, Candidate{ID: "extra", Name: "extra"})
	assert.Equal(t, want, full[10])
}

// TestWithCandidatesClearsTheTotal asserts setting a full list, or a cut
// that dropped nothing, removes a total an earlier cut set.
func TestWithCandidatesClearsTheTotal(t *testing.T) {
	cut := New(CodeNotFound, "x").WithCandidatesCut(candidateRange(12), 10)
	require.Equal(t, 12, cut.CandidatesTotal)

	assert.Zero(t, cut.WithCandidates(candidateRange(2)).CandidatesTotal)

	cut = New(CodeNotFound, "x").WithCandidatesCut(candidateRange(12), 10)
	again := cut.WithCandidatesCut(candidateRange(4), 10)
	assert.Zero(t, again.CandidatesTotal)
	assert.Len(t, again.Candidates, 4)
}

// TestCandidatesTotalReachesTheEnvelopeOnlyWhenSet asserts the marshaled
// error carries candidatesTotal for a cut list and omits it otherwise.
func TestCandidatesTotalReachesTheEnvelopeOnlyWhenSet(t *testing.T) {
	decode := func(e *Error) map[string]any {
		raw, err := json.Marshal(e)
		require.NoError(t, err)
		var got map[string]any
		require.NoError(t, json.Unmarshal(raw, &got))
		return got
	}

	cut := decode(New(CodeNotFound, "x").WithCandidatesCut(candidateRange(37), 10))
	assert.EqualValues(t, 37, cut["candidatesTotal"])
	assert.Len(t, cut["candidates"], 10)

	full := decode(New(CodeNotFound, "x").WithCandidatesCut(candidateRange(10), 10))
	assert.NotContains(t, full, "candidatesTotal")
	assert.Len(t, full["candidates"], 10)
}

// TestWithMessageKeepsTheCandidatesTotal asserts re-messaging an error
// keeps the total of its cut list.
func TestWithMessageKeepsTheCandidatesTotal(t *testing.T) {
	e := New(CodeNotFound, "x").WithCandidatesCut(candidateRange(12), 10)
	got := e.WithMessage("y")
	assert.Equal(t, 12, got.CandidatesTotal)
	assert.Len(t, got.Candidates, 10)
}

// TestRedactURLUserinfoFailsClosed asserts that a text with an "@" is shown
// with everything up to the last "@" replaced: after the "://" when a URL
// scheme precedes it and it comes before that "@", from the start of the text
// otherwise (no scheme, a scheme-relative URL, one slash, a "://" after the
// "@" or after a prefix that is not a scheme, plain text). A text with no
// "@" is unchanged. It also asserts the cases the doc comment names: an "@"
// only in the path or query (the host is redacted too), several "@" in the
// password, "/", "?" and "#" in the password, "://" twice, an IPv6 host and
// an empty text.
func TestRedactURLUserinfoFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"a user and a password", "https://u:pw@host:abc", "https://<redacted>@host:abc"},
		{"no userinfo", "https://host:abc/path", "https://host:abc/path"},
		{"no scheme", "u:s@host", "<redacted>@host"},
		{"a scheme-relative URL", "//u:s@host", "<redacted>@host"},
		{"one slash", "https:/u:s@host", "<redacted>@host"},
		{"plain text with an at sign", "write to a@b about it", "<redacted>@b about it"},
		{"the scheme separator after the at sign", "u:pw@host/?r=https://z", "<redacted>@host/?r=https://z"},
		{"the scheme separator inside the userinfo", "u:pa://ss@host", "<redacted>@host"},
		{"a scheme with every character it may hold", "a1+b-c.d://u:s@host", "a1+b-c.d://<redacted>@host"},
		{"a scheme that starts with a digit", "1ab://u:s@host", "<redacted>@host"},
		{"an empty scheme", "://u:s@host", "<redacted>@host"},
		{"a slash in the password", "https://u:pa/ss@host:abc", "https://<redacted>@host:abc"},
		{"a question mark in the password", "https://u:pa?ss@host:abc", "https://<redacted>@host:abc"},
		{"a number sign in the password", "https://u:pa#ss@host:abc", "https://<redacted>@host:abc"},
		{"several at signs in the password", "https://u:p@s@s@host:abc", "https://<redacted>@host:abc"},
		{"an at sign only in the path", "https://host:abc/a@b", "https://<redacted>@b"},
		{"an at sign only in the query", "https://host/p?mail=a@b.example", "https://<redacted>@b.example"},
		{"the scheme separator twice, the at sign after the second", "https://host/next?u=http://x:y@z", "https://<redacted>@z"},
		{"the scheme separator twice, the at sign before the second", "https://u:pw@host/next?u=http://z", "https://<redacted>@host/next?u=http://z"},
		{"an IPv6 host", "https://u:pw@[::1]:abc/x", "https://<redacted>@[::1]:abc/x"},
		{"an IPv6 host and no userinfo", "https://[::1]:abc/x", "https://[::1]:abc/x"},
		{"a whole message, which the caller never passes", "invalid server URL: ftp://u:pw@host/x", "<redacted>@host/x"},
		{"an empty text", "", ""},
		{"only the scheme separator", "://", "://"},
		{"only an at sign", "@", "<redacted>@"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, RedactURLUserinfo(tc.in))
		})
	}
}
