package cli

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	currentPasswordFlag     = "--current-password-stdin"
	currentPasswordVariable = "$CURRENTPASSWORD"
	newPasswordVariable     = "$NEWPASSWORD"
)

// currentPasswordOrderProblems returns what is wrong with the order of the two
// passwords in one text: a text with both variables gives the current
// password's first, and a text with both phrases "current password" and "new
// password" gives the first "current password" before the first "new password".
func currentPasswordOrderProblems(text string) []string {
	var problems []string
	current, newPassword := strings.Index(text, currentPasswordVariable), strings.Index(text, newPasswordVariable)
	if current >= 0 && newPassword >= 0 && newPassword < current {
		problems = append(problems, "the new password variable comes before the current password variable")
	}
	lower := strings.ToLower(text)
	current, newPassword = strings.Index(lower, "current password"), strings.Index(lower, "new password")
	if current >= 0 && newPassword >= 0 && newPassword < current {
		problems = append(problems, `the words "new password" come before the words "current password"`)
	}
	return problems
}

// currentPasswordExampleProblems adds a second rule to the order rule: an
// example with --current-password-stdin has both $CURRENTPASSWORD and
// $NEWPASSWORD.
func currentPasswordExampleProblems(example string) []string {
	problems := currentPasswordOrderProblems(example)
	if strings.Contains(example, currentPasswordFlag) &&
		(!strings.Contains(example, currentPasswordVariable) || !strings.Contains(example, newPasswordVariable)) {
		problems = append(problems, "an example with "+currentPasswordFlag+" has both "+currentPasswordVariable+" and "+newPasswordVariable)
	}
	return problems
}

// currentPasswordManifestProblems checks every text of the manifest entry of
// the command: every example, every flag description and the command
// description. The check fails when no example has --current-password-stdin:
// a rename of the flag cannot make it pass with nothing to read.
func currentPasswordManifestProblems(c runtime.SchemaCommand) []string {
	var problems []string
	found := false
	for _, example := range c.Examples {
		found = found || strings.Contains(example, currentPasswordFlag)
		for _, p := range currentPasswordExampleProblems(example) {
			problems = append(problems, "example "+example+": "+p)
		}
	}
	if !found {
		problems = append(problems, "no example has "+currentPasswordFlag)
	}
	for _, f := range c.Flags {
		for _, p := range currentPasswordOrderProblems(f.Desc) {
			problems = append(problems, "flag --"+f.Name+": "+p)
		}
	}
	for _, p := range currentPasswordOrderProblems(c.Desc) {
		problems = append(problems, "description: "+p)
	}
	return problems
}

// TestCurrentPasswordComesFirstInEveryExample asserts that every example, flag
// description and the description of create password-change-request in the
// manifest gives the current password before the new password, and that an
// example with --current-password-stdin has both variables.
func TestCurrentPasswordComesFirstInEveryExample(t *testing.T) {
	t.Parallel()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	for _, c := range m.Commands {
		if c.Name == "create password-change-request" {
			assert.Empty(t, currentPasswordManifestProblems(c))
			return
		}
	}
	t.Fatal("the manifest has no create password-change-request")
}

// TestCurrentPasswordOrderCheckFailsOnAWrongOrder asserts the manifest half of
// the order check on fixed input: a text with the variables swapped fails, a
// text with the phrases swapped fails, an example with the flag and one
// variable fails, a manifest entry with no example that has the flag fails,
// and a text in the right order passes.
func TestCurrentPasswordOrderCheckFailsOnAWrongOrder(t *testing.T) {
	t.Parallel()
	const good = `printf '%s\n%s\n' "$CURRENTPASSWORD" "$NEWPASSWORD" | rancher create password-change-request u-abc --current-password-stdin --password-stdin`
	assert.Empty(t, currentPasswordOrderProblems(good))
	assert.Empty(t, currentPasswordManifestProblems(runtime.SchemaCommand{Examples: []string{good}}))

	assert.NotEmpty(t, currentPasswordOrderProblems(`printf '%s\n%s\n' "$NEWPASSWORD" "$CURRENTPASSWORD" | rancher create password-change-request u-abc`))
	assert.NotEmpty(t, currentPasswordOrderProblems("pipe the new password, then the current password"))
	assert.NotEmpty(t, currentPasswordExampleProblems(`printf '%s\n' "$NEWPASSWORD" | rancher create password-change-request u-abc --current-password-stdin --password-stdin`))
	assert.NotEmpty(t, currentPasswordExampleProblems(`printf '%s\n' "$CURRENTPASSWORD" | rancher create password-change-request u-abc --current-password-stdin --password-stdin`))
	assert.NotEmpty(t, currentPasswordManifestProblems(runtime.SchemaCommand{Examples: []string{good[:strings.Index(good, currentPasswordFlag)]}}))
	assert.NotEmpty(t, currentPasswordManifestProblems(runtime.SchemaCommand{}), "a manifest entry with no example has nothing to check")
}
