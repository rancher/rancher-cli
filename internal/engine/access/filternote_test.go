package access

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
)

// TestCanTextPadsAChangedNamespaceLikeAnUnchangedOne asserts the namespace
// line of can pads a name the filter changed to the same width as the same
// name with nothing to remove: the mark after it prints nothing and is not
// counted.
func TestCanTextPadsAChangedNamespaceLikeAnUnchangedOne(t *testing.T) {
	t.Parallel()
	line := func(ns string) string {
		out := canText(canResult{
			Subject: AccessSubject{Name: "u-1"}, Question: Question{Verb: "get", Resource: "pods"},
			Scope: Scope{Cluster: "local"}, Decision: DecisionAllowed,
			Namespaces: []canNamespace{{Namespace: ns, Decision: DecisionAllowed}},
		}, true).Marked()
		for l := range strings.Lines(out) {
			if strings.HasPrefix(l, "  ab") {
				return l
			}
		}
		t.Fatalf("no namespace line in %q", out)
		return ""
	}
	marked, unchanged := line("a\u200bb"), line("ab")
	assert.True(t, clierr.HasRemovalMark(marked), "the changed name is followed by the mark")
	assert.Equal(t, unchanged, clierr.WithoutRemovalMark(marked), "both lines are padded to 24 characters as a terminal shows them")
	assert.Equal(t, "  ab"+strings.Repeat(" ", 22)+" "+string(DecisionAllowed)+"\n", unchanged)
}

// TestMemberPhraseDecidesOnTheIdentity asserts the membership line says
// "you are" for the caller and "<name> is" for any other subject, a subject
// named "you" or one whose name a filter changed included: the identity
// decides, never the text of the name.
func TestMemberPhraseDecidesOnTheIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, subject string
		identity      Identity
		want          string
	}{
		{"the caller", "u-1", IdentityCaller, "you are a member of: none\n"},
		{"another subject", "u-2", IdentitySAR, "u-2 is a member of: none\n"},
		{"another subject named you", "you", IdentitySAR, "you is a member of: none\n"},
		{"another subject whose name a filter changed", "y\u200bou", IdentitySAR, "you is a member of: none\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := projectsText(projectsResult{Subject: AccessSubject{Name: tc.subject}, Identity: tc.identity, MemberOf: &memberOfSet{}}, false).Marked()
			assert.Contains(t, clierr.WithoutRemovalMark(out), "\n"+tc.want)
		})
	}
}

// TestAMembershipWhoseGrantNameIsOnlyHiddenCharactersPrintsEmptyParentheses
// asserts that a membership whose one grant name is only characters the
// filter removes prints empty parentheses after its project, and the text
// has the removal mark: the name is not left out as if there were none.
func TestAMembershipWhoseGrantNameIsOnlyHiddenCharactersPrintsEmptyParentheses(t *testing.T) {
	t.Parallel()
	m := membership{Grants: []grant{{Name: "\u200b"}}}
	m.Cluster, m.Project = "c", "p"
	out := projectsText(projectsResult{Subject: AccessSubject{Name: "u-1"}, Identity: IdentityCaller, MemberOf: &memberOfSet{Projects: []membership{m}}}, false).Marked()
	var line string
	for l := range strings.Lines(out) {
		if strings.Contains(l, "a member of") {
			line = l
		}
	}
	assert.True(t, clierr.HasRemovalMark(line), "the emptied grant name is followed by the mark")
	assert.Equal(t, "you are a member of: c/p ()\n", clierr.WithoutRemovalMark(line))
}
