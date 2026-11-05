package token

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// cutRule says what the cut of the texts of one builder leaves when the config
// directory has characters a terminal cannot show and the commands are left
// out. A text of the builder that holds match (any text when match is empty)
// keeps fact, a phrase of the text before the clause that holds its first
// command; a rule with no fact is the note alone.
type cutRule struct{ builder, match, fact string }

// cutFacts has one rule for each kind of text that says a fact before its first
// command. The first rule that fits a text decides; a rule that fits no text
// of the rows fails.
var cutFacts = []cutRule{
	{"Deletion.Warnings", "", "are revoked by the next"},
	{"Deletion.manualWarning", "", "was not revoked"},
	{"Deletion.nextLoginText", "", "the next login of"},
	{"Deletion.revokeTLSRemedy", "; a Rancher administrator who deletes the token needs", "a Rancher administrator who deletes the token needs"},
	{"Deletion.revokeTLSRemedy", "; to save a CA for entry", "the CA came from"},
	{"NoTerminalLogin", "this exec entry carries the CA inline", "carries the CA inline"},
	{"NoTerminalLogin", "stands for a name of your choice", "stands for a name of your choice"},
	{"ResidualRecovery", "", "no credential was stored"},
	{"noTokenPermissionText", "", "the user has no role that allows creating tokens"},
	{"deleteFailureText", "", "failed to delete"},
	{"entryNotNamedError", "", "more than one server entry"},
	{"entryOnOtherServerError", "", "the exec entry names server entry"},
	{"keptWarning", "", "was not retired"},
	{"namedEntryRoute", "; selecting an entry", "selecting an entry switches the directory's current server"},
	{"pluginEntriesError", "", "more than one login entry"},
	{"revokedClause", "", "could not be revoked"},
	{"signInLeftover", "may have been created through", "may have been created through"},
	{"signInLeftover", "from a directory signed in as", "from a directory signed in as"},
	{"signInLeftover", "expires at", "expires at"},
	{"tokenCmd.noTerminalError", "", "no terminal to"},
	{"tokenCmd.withStoreConflictSteps", "", "conflict"},
	{"useServerNote", "", "selecting an entry"},
	{"userCheck.checkCreatedOwner", "belongs to Rancher user", "belongs to Rancher user"},
	{"userCheck.entryNameTooLong", "", "would have a name of"},
	{"userCheck.entryOwnerRefused", "", "this exec entry names user"},
	{"userCheck.loginOfNewEntry", "", "stands for a name of your choice"},
	{"v3AdminDelete", "", "it expires"},
	{"v3AdminRemedy", "", "expires"},
	{"v3Remedy", "", "the CLI cannot delete this v3 token"},
}

// cutNoteOnly lists the texts whose first clause holds their command: the
// cut leaves the note alone. Each is a lead and its command, or a command
// with the words that name it in the same clause.
var cutNoteOnly = []cutRule{
	{"NoTerminalLogin", "to log in, pipe an API token to: ", ""},
	{"namedEntryRoute", "if it has no login, run: ", ""},
	{"signInLeftover", "an administrator can list tokens with: ", ""},
	{"userCheck.authConfigRemedy", "remove '", ""},
	{"userCheck.authConfigRemedy", "sign in through auth config", ""},
	{"userCheck.relogin", "remove '", ""},
	{"userCheck.relogin", "pipe an API token of", ""},
	{"Deletion.revokeTLSRemedy", "; save it for entry", ""},
}

// ruleFor is the first rule of rules for a text of builder.
func ruleFor(rules []cutRule, used map[int]bool, builder, text string) (cutRule, bool) {
	for i, r := range rules {
		if r.builder == builder && strings.Contains(text, r.match) {
			used[i] = true
			return r, true
		}
	}
	return cutRule{}, false
}

// TestTheFactBeforeACommandSurvivesTheCutInTheTokenRemedies asserts, for each
// text of every remedy builder of the package that holds a command, that the
// text left when the config directory has characters a terminal cannot show
// holds no command and is either the note alone (a text whose first clause
// holds its command) or keeps the phrase the text's rule names. A text no
// rule fits fails, and so does a rule no text fits.
func TestTheFactBeforeACommandSurvivesTheCutInTheTokenRemedies(t *testing.T) {
	rows := remedyBuilderRows(t)
	io := runtime.IO{RemedyDir: "/d\x01"}
	usedFacts, usedNoteOnly := map[int]bool{}, map[int]bool{}
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		for _, text := range rows[name]() {
			if !clierr.HasRemedy(text) {
				continue
			}
			cut := io.ExpandText(text)
			assert.NotContains(t, cut, "rancher ", "%s: no command is left: %q", name, cut)
			if strings.HasPrefix(text, clierr.Rancher) {
				assert.Equal(t, runtime.UnprintableConfigDirNote, cut, "%s: a command alone is the note: %q", name, text)
				continue
			}
			if rule, ok := ruleFor(cutFacts, usedFacts, name, text); ok {
				require.NotEmpty(t, rule.fact, "%s: a rule that keeps a fact names it", name)
				assert.NotEqual(t, runtime.UnprintableConfigDirNote, cut, "%s: the fact before the command is kept: %q", name, text)
				assert.Contains(t, cut, rule.fact, "%s keeps its fact: %q", name, text)
				continue
			}
			if _, ok := ruleFor(cutNoteOnly, usedNoteOnly, name, text); ok {
				assert.Equal(t, runtime.UnprintableConfigDirNote, cut, "%s: the text is the note alone: %q", name, text)
				continue
			}
			assert.Fail(t, "a text with no cut rule", "%s: %q", name, text)
		}
	}
	for i, r := range cutFacts {
		assert.True(t, usedFacts[i], "the cut rule for %s %q fits no text", r.builder, r.match)
	}
	for i, r := range cutNoteOnly {
		assert.True(t, usedNoteOnly[i], "the note-only rule for %s %q fits no text", r.builder, r.match)
	}
}
