package cli

import (
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
)

// wantGroupTitles is the title of every command group, word for word. A
// change to a title is a change to this table.
var wantGroupTitles = map[string]string{
	"auth":        "Auth commands",
	"config":      "Config commands",
	"create":      "Create resources",
	"get":         "Get resources",
	"patch":       "Patch resources",
	"delete":      "Delete resources",
	"inspect":     "Inspect a cluster, workload or project with its related objects",
	"project":     "Project namespace commands (project membership is managed with 'rancher grant' and 'rancher revoke')",
	"skill":       "Install, remove and check the agent skills that come with rancher-cli",
	"telemetry":   "Local usage recording: status, turn on, turn off, delete and list events",
	"oidc-client": "Manage OIDC client secrets (create oidc-client creates the client itself)",
	"cluster":     "Scale a machine pool or manage cluster-autoscaler settings on a provisioning cluster (create one with 'create cluster' and delete it with 'delete provisioning-cluster')",
	"access":      "Which clusters a subject can reach, which projects it can see into, and what it can do, as the server answers (never inferred from bindings)",
}

// titleHasOperationWord reports whether title holds the whole word
// "operation" or "operations", in any case.
func titleHasOperationWord(title string) bool {
	words := strings.FieldsFunc(title, func(r rune) bool { return !unicode.IsLetter(r) })
	for _, w := range words {
		if lw := strings.ToLower(w); lw == "operation" || lw == "operations" {
			return true
		}
	}
	return false
}

// TestGroupTitlesNameCommands asserts that no title in groupUsages holds the
// whole word "operation" or "operations" (a group holds commands, and the
// help says so), and that the titles equal wantGroupTitles. It does not
// assert that a title agrees with the description of its group.
func TestGroupTitlesNameCommands(t *testing.T) {
	for group, title := range groupUsages {
		assert.False(t, titleHasOperationWord(title), "the title of group %q says operations; say commands: %q", group, title)
	}
	assert.Equal(t, wantGroupTitles, groupUsages)
}
