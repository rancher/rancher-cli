package clierr

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExpandRemediesWritesThePrefixAfterRancher asserts every marked
// command gains the prefix, an unmarked rancher and a marker elsewhere are
// left alone, and the empty prefix gives the plain command.
func TestExpandRemediesWritesThePrefixAfterRancher(t *testing.T) {
	t.Parallel()
	s := "run " + Rancher + " config status, then " + Rancher + " auth login 'prod'; the legacy rancher token command" + RemedyMarker
	assert.Equal(t,
		"run rancher --config '/d' config status, then rancher --config '/d' auth login 'prod'; the legacy rancher token command"+RemedyMarker,
		ExpandRemedies(s, " --config '/d'"))
	assert.Equal(t,
		"run rancher config status, then rancher auth login 'prod'; the legacy rancher token command"+RemedyMarker,
		ExpandRemedies(s, ""))
	assert.Equal(t, "no command here", ExpandRemedies("no command here", " --config '/d'"))
}

// TestShellQuoteDropsTheMarker asserts a quoted value cannot carry a
// marker into a command, so no name can inject the invocation's prefix.
func TestShellQuoteDropsTheMarker(t *testing.T) {
	t.Parallel()
	q := ShellQuote("prod" + Rancher + "x")
	assert.NotContains(t, q, "\u2063")
	assert.NotContains(t, q, Rancher)
	assert.Equal(t, "rancher --config '/d' auth login "+q, ExpandRemedies(Rancher+" auth login "+q, " --config '/d'"))
}

// TestMarkRemedy asserts a plain rancher command becomes a remedy command
// and anything else is returned unchanged.
func TestMarkRemedy(t *testing.T) {
	t.Parallel()
	assert.Equal(t, Rancher+" config use-server 'x'", MarkRemedy("rancher config use-server 'x'"))
	assert.Equal(t, "kubectl get pods", MarkRemedy("kubectl get pods"))
	assert.Equal(t, Rancher+" auth login", MarkRemedy(Rancher+" auth login"))
}

// TestErrorMapTextKeepsTheCandidatesTotal asserts mapping the text of an
// error keeps its candidates and their total.
func TestErrorMapTextKeepsTheCandidatesTotal(t *testing.T) {
	t.Parallel()
	e := New(CodeNotFound, "try "+Rancher+" get users").
		WithCandidatesCut(candidateRange(12), 10)
	got := e.MapText(func(s string) string { return ExpandRemedies(s, " -c '/d'") })
	require.NotSame(t, e, got)
	assert.Equal(t, "try rancher -c '/d' get users", got.Message)
	assert.Equal(t, 12, got.CandidatesTotal)
	assert.Len(t, got.Candidates, 10)
}

// TestErrorMapTextCoversEveryRenderedString asserts the message, the
// hint, the warnings and a login failure's recovery commands are mapped,
// the cause and code survive, and an error with no marker is returned
// itself.
func TestErrorMapTextCoversEveryRenderedString(t *testing.T) {
	t.Parallel()
	cause := errors.New("root")
	d := LoginFailureDetails{
		Primary:            PrimaryFailure{Code: CodeAuth, Message: "see " + Rancher + " auth status", Hint: Rancher + " auth status"},
		CredentialRecovery: &CredentialRecovery{Command: Rancher + " auth logout 'p'"},
		ConfigRecovery:     &ConfigRecovery{Entry: "p", Command: Rancher + " config delete-server 'p'"},
		Recovery:           "log in again with: " + Rancher + " auth login 'p'",
	}
	e := New(CodeAuth, "try "+Rancher+" auth login 'p'").WithHint(Rancher + " auth login 'p'").
		WithCause(cause).WithWarnings("see " + Rancher + " config status").WithDetails(d)
	expand := func(s string) string { return ExpandRemedies(s, " -c '/d'") }
	got := e.MapText(expand)

	assert.Equal(t, "try rancher -c '/d' auth login 'p'", got.Message)
	assert.Equal(t, "rancher -c '/d' auth login 'p'", got.Hint)
	assert.Equal(t, []string{"see rancher -c '/d' config status"}, got.Warnings())
	gd, ok := got.Details.(LoginFailureDetails)
	require.True(t, ok)
	assert.Equal(t, "see rancher -c '/d' auth status", gd.Primary.Message)
	assert.Equal(t, "rancher -c '/d' auth status", gd.Primary.Hint)
	assert.Equal(t, "rancher -c '/d' auth logout 'p'", gd.CredentialRecovery.Command)
	assert.Equal(t, "rancher -c '/d' config delete-server 'p'", gd.ConfigRecovery.Command)
	assert.Equal(t, "log in again with: rancher -c '/d' auth login 'p'", gd.Recovery)
	require.ErrorIs(t, got, cause)
	assert.Equal(t, CodeAuth, got.Code)
	// The original is untouched.
	assert.Equal(t, Rancher+" auth logout 'p'", d.CredentialRecovery.Command)
	assert.Equal(t, "try "+Rancher+" auth login 'p'", e.Message)

	plain := New(CodeAuth, "no command")
	assert.Same(t, plain, plain.MapText(expand))

	// The command fields alone.
	emptied, ok := d.MapCommands(func(string) string { return "" }).(LoginFailureDetails)
	require.True(t, ok)
	assert.Empty(t, emptied.CredentialRecovery.Command)
	assert.Empty(t, emptied.ConfigRecovery.Command)
	assert.Equal(t, d.Recovery, emptied.Recovery)
	assert.Contains(t, emptied.TextDetails(), "config entry p could not be restored", "no dangling remove-it-with when there is no command")

	// A marker only in the details is still found.
	onlyDetails := New(CodeAuth, "failed").WithDetails(LoginFailureDetails{Recovery: Rancher + " auth login 'p'"})
	assert.NotSame(t, onlyDetails, onlyDetails.MapText(expand))
}
