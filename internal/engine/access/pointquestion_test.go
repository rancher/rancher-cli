package access

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestPointQuestionTextsAreCompleteCommands asserts the point question a
// refusal suggests: it names a verb, a resource and a cluster, and a group is
// named with --type group, because a bare name is read as a user.
func TestPointQuestionTextsAreCompleteCommands(t *testing.T) {
	t.Parallel()
	assert.Equal(t, clierr.Rancher+" access can <verb> <resource> --cluster <cluster> --as 'eng' --type group", groupAccessCan("eng").String())
	assert.Equal(t, clierr.Rancher+" access can <verb> <resource> --cluster <cluster> --as '-eng' --type group", groupAccessCan("-eng").String())
	assert.Equal(t, clierr.Rancher+" access can <verb> <resource> --cluster <cluster> --as system:anonymous", anonymousAccessCan().String())
	assert.Equal(t, "the management cluster cannot impersonate; <verb> and <resource> are what to ask; ask with: "+
		clierr.Rancher+" access can <verb> <resource> --cluster local", localCannotImpersonateNote)
}

// TestPointQuestionTextsLeaveOutTheCommandForAValueAShellMisreads asserts that
// a group or service account name holding a quote, a curly quote, a zero-width
// space or two backslashes in a row leaves the command out of the clause and,
// where the name is shown unchanged, puts the note in once.
func TestPointQuestionTextsLeaveOutTheCommandForAValueAShellMisreads(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"o'brien", "o\u2019brien", "o\u200bbrien", `o\\brien`, "eng\\"} {
		for name, clause := range map[string]string{
			"group":           groupPointQuestion(raw),
			"service account": serviceAccountList(raw),
			"group message":   groupListMessage(raw),
		} {
			assert.NotContains(t, clause, "access can <verb> <resource> --cluster <cluster> --as", "%s %q", name, raw)
			assert.NotContains(t, clause, "access list", "%s %q", name, raw)
			assert.LessOrEqual(t, strings.Count(clause, clierr.LeftOutNote), 1, "%s %q", name, raw)
			if clierr.IDText(raw) == raw {
				assert.Contains(t, clause, clierr.LeftOutNote, "%s %q", name, raw)
			}
		}
	}
}
