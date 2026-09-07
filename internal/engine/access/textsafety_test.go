package access

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestTableLinesStripControlCharacters asserts the fitted text table
// strips control characters from headers and cells and measures widths on
// the stripped text, so a server-sourced name cannot forge a row or skew
// the columns.
func TestTableLinesStripControlCharacters(t *testing.T) {
	t.Parallel()
	lines := tableLines("  ", []string{"NAME", "VIA"}, [][]string{{"u-1\nFAKE", "ClusterRoleBinding \x1b[2Jx"}})
	assert.Equal(t, []string{"  NAME     VIA", "  u-1FAKE  ClusterRoleBinding [2Jx"}, lines)
}

// TestCanTextStripsControlCharacters asserts the authorizer's reason, the
// note, and the evaluation error render without control characters.
func TestCanTextStripsControlCharacters(t *testing.T) {
	t.Parallel()
	out := canText(canResult{
		Subject: AccessSubject{Name: "u-1"}, Question: Question{Verb: "get", Resource: "pods"},
		Scope: Scope{Cluster: "local"}, Decision: DecisionDenied,
		Reason: "RBAC: a\x1b[2Jb", Note: "n\nforged", EvaluationError: "e\rx",
	}, false)
	assert.NotContains(t, out, "\x1b")
	assert.Contains(t, out, "  reason: RBAC: a[2Jb\n")
	assert.Contains(t, out, "  note: nforged\n")
	assert.Contains(t, out, "  evaluation error: ex\n")
}

// TestWhoCanTextStripsControlCharacters asserts the note and the known
// members line render without control characters; the table cells go
// through tableLines.
func TestWhoCanTextStripsControlCharacters(t *testing.T) {
	t.Parallel()
	out := whoCanText(subjectsResult{
		Question: Question{Verb: "get", Resource: "pods"}, Scope: Scope{Cluster: "local"},
		Note:     "n\x1b[2J",
		Subjects: []subjectRow{{Kind: KindGroup, Name: "g", Decision: DecisionAllowed, KnownMembers: []string{"u-1\nFAKE", "u-2"}}},
	}, false, false)
	assert.NotContains(t, out, "\x1b")
	assert.Contains(t, out, "  note: n[2J\n")
	assert.Contains(t, out, "  known members: u-1FAKE, u-2\n")
	assert.NotContains(t, out, "\nFAKE")
}
