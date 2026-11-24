package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigratingNamesTheCreateClusterCredentialSelection asserts the "Changed in
// this CLI" table of MIGRATING.md has a row for a `--cloud-credential` value of
// `create cluster` that is a display name: the Before cell says it selected a
// credential, and the Now cell says the command takes the credential id and a
// display name is refused as not found.
func TestMigratingNamesTheCreateClusterCredentialSelection(t *testing.T) {
	t.Parallel()
	migrating, err := os.ReadFile(filepath.Join(migratingRepoRoot(t), "MIGRATING.md"))
	require.NoError(t, err)
	var found [][]string
	for line := range strings.SplitSeq(migratingSection(t, string(migrating), "## Changed in this CLI"), "\n") {
		cells := strings.Split(line, "|")
		if len(cells) != 4 {
			continue
		}
		before, now := cells[1], cells[2]
		if strings.Contains(before, "`--cloud-credential`") && strings.Contains(before, "`create cluster`") && strings.Contains(before, "display name selected") {
			found = append(found, []string{before, now})
		}
	}
	require.Len(t, found, 1, "one row covers a display name given to create cluster --cloud-credential")
	now := found[0][1]
	assert.Contains(t, now, "takes the credential id")
	assert.Contains(t, now, "a display name is refused (NOT_FOUND)")
	assert.Contains(t, now, "`rancher get cloud-credential`")
}
