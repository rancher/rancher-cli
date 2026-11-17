package conformance

import (
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	currentPasswordFlag     = "--current-password-stdin"
	currentPasswordVariable = "$CURRENTPASSWORD"
	newPasswordVariable     = "$NEWPASSWORD"
)

// markdownListItem matches the first line of a list item.
var markdownListItem = regexp.MustCompile(`^\s*([-*+]|[0-9]+[.)])\s`)

// markdownUnits splits Markdown text into the units the order rule reads: a
// paragraph, a list item or a table row, with the lines of a wrapped unit
// joined by a space.
func markdownUnits(text string) []string {
	var units, current []string
	flush := func() {
		if len(current) > 0 {
			units = append(units, strings.Join(current, " "))
			current = nil
		}
	}
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			flush()
		case strings.HasPrefix(trimmed, "|"):
			flush()
			units = append(units, trimmed)
		case markdownListItem.MatchString(line):
			flush()
			current = append(current, trimmed)
		default:
			current = append(current, trimmed)
		}
	}
	flush()
	return units
}

// currentPasswordUnitProblems returns what is wrong with the order of the two
// passwords in one Markdown unit: a unit that holds both variables gives the
// current password's first, and a unit that holds both phrases "current
// password" and "new password" gives the first "current password" before the
// first "new password".
func currentPasswordUnitProblems(unit string) []string {
	var problems []string
	current, newPassword := strings.Index(unit, currentPasswordVariable), strings.Index(unit, newPasswordVariable)
	if current >= 0 && newPassword >= 0 && newPassword < current {
		problems = append(problems, "the new password variable comes before the current password variable")
	}
	lower := strings.ToLower(unit)
	current, newPassword = strings.Index(lower, "current password"), strings.Index(lower, "new password")
	if current >= 0 && newPassword >= 0 && newPassword < current {
		problems = append(problems, `the words "new password" come before the words "current password"`)
	}
	return problems
}

// currentPasswordRequiredFiles are the documents that must each hold a unit
// with --current-password-stdin.
var currentPasswordRequiredFiles = []string{"COMMANDS.md", "skills/rancher/SKILL.md", "MIGRATING.md"}

// currentPasswordDocProblems checks the units that hold --current-password-stdin
// of every Markdown file in texts (path to content): each unit's order rule,
// and one problem for each required file that yields no such unit. A rename of
// the flag or a reflow of the documents cannot make the check pass with
// nothing to read.
func currentPasswordDocProblems(texts map[string]string) []string {
	var problems []string
	for _, path := range slices.Sorted(maps.Keys(texts)) {
		for _, unit := range markdownUnits(texts[path]) {
			if !strings.Contains(unit, currentPasswordFlag) {
				continue
			}
			for _, p := range currentPasswordUnitProblems(unit) {
				problems = append(problems, path+": "+p+": "+unit)
			}
		}
	}
	for _, path := range currentPasswordRequiredFiles {
		if !slices.ContainsFunc(markdownUnits(texts[path]), func(unit string) bool { return strings.Contains(unit, currentPasswordFlag) }) {
			problems = append(problems, path+" has no unit with "+currentPasswordFlag)
		}
	}
	return problems
}

// TestCurrentPasswordComesFirstInEveryDoc asserts that every paragraph, list
// item and table row that holds --current-password-stdin, in every Markdown
// file git sees, gives the current password before the new password, and that
// COMMANDS.md, skills/rancher/SKILL.md and MIGRATING.md each hold one.
func TestCurrentPasswordComesFirstInEveryDoc(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	texts := map[string]string{}
	for _, path := range files {
		if !strings.HasSuffix(path, ".md") {
			continue
		}
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		texts[path] = string(raw)
	}
	assert.Empty(t, currentPasswordDocProblems(texts))
}

// TestCurrentPasswordDocCheckFailsOnAWrongOrder asserts the Markdown half of
// the order check on fixed input: a unit with the variables swapped fails, a
// unit with the phrases swapped fails (also across a wrapped line), a set of
// documents with no unit that holds the flag fails, and a unit in the right
// order passes.
func TestCurrentPasswordDocCheckFailsOnAWrongOrder(t *testing.T) {
	t.Parallel()
	good := "Pipe two lines:\n\n`printf '%s\\n%s\\n' \"$CURRENTPASSWORD\" \"$NEWPASSWORD\" | rancher create password-change-request <user id> --current-password-stdin --password-stdin`\n"
	right := map[string]string{"COMMANDS.md": good, "skills/rancher/SKILL.md": good, "MIGRATING.md": good}
	assert.Empty(t, currentPasswordDocProblems(right))

	swappedVariables := "`printf '%s\\n%s\\n' \"$NEWPASSWORD\" \"$CURRENTPASSWORD\" | rancher create password-change-request <user id> --current-password-stdin --password-stdin`\n"
	assert.NotEmpty(t, currentPasswordDocProblems(map[string]string{"COMMANDS.md": good, "skills/rancher/SKILL.md": swappedVariables, "MIGRATING.md": good}))

	swappedPhrases := "With --current-password-stdin, line 1 is the new password and line 2 is the current password.\n"
	assert.NotEmpty(t, currentPasswordDocProblems(map[string]string{"COMMANDS.md": good, "skills/rancher/SKILL.md": good, "MIGRATING.md": swappedPhrases}))

	wrappedSwapped := "With --current-password-stdin, the new\npassword comes first and the current password second.\n"
	assert.NotEmpty(t, currentPasswordDocProblems(map[string]string{"COMMANDS.md": good, "skills/rancher/SKILL.md": wrappedSwapped, "MIGRATING.md": good}),
		"the phrase is found across a wrapped line")

	assert.NotEmpty(t, currentPasswordDocProblems(map[string]string{}), "no document yields a unit that holds the flag")
	assert.Len(t, currentPasswordDocProblems(map[string]string{"COMMANDS.md": good, "skills/rancher/SKILL.md": good, "MIGRATING.md": "no flag here\n"}), 1,
		"one required document with no unit that holds the flag")
}

// TestMarkdownUnitsJoinWrappedLines asserts the units: a wrapped paragraph and
// a wrapped list item are one unit each, and a table row is its own unit.
func TestMarkdownUnitsJoinWrappedLines(t *testing.T) {
	t.Parallel()
	text := "first line\nsecond line\n\n- item one\n  continues\n- item two\n\n| a | b |\n| c | d |\nend\n"
	assert.Equal(t, []string{"first line second line", "- item one continues", "- item two", "| a | b |", "| c | d |", "end"}, markdownUnits(text))
}
