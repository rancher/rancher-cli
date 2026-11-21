package conformance

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPreCommitHookClearsTheVariablesGitSets asserts that .githooks/pre-commit
// runs the gate with no GIT_* variable. git sets GIT_DIR, GIT_INDEX_FILE and
// others for a hook; a test that runs git in a temporary directory with them
// set would act on this repository. The test runs the hook as written, with a
// stand-in make first on PATH that records the names of the variables it
// receives, and with the GIT_* variables git sets for a hook present in the
// hook's environment. Mutation: remove the loop that clears the variables, or
// narrow it to GIT_DIR.
func TestPreCommitHookClearsTheVariablesGitSets(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, "the hook is a bash script that runs on the developer's machine")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	raw, err := os.ReadFile(filepath.Join(".githooks", "pre-commit"))
	require.NoError(t, err)
	hook := bytes.ReplaceAll(raw, []byte("\r"), nil)

	dir := t.TempDir()
	hookPath := filepath.Join(dir, "pre-commit")
	testfs.WriteExecutable(t, hookPath, hook)
	stubDir := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(stubDir, 0o755))
	record := filepath.Join(dir, "record")
	testfs.WriteExecutable(t, filepath.Join(stubDir, "make"), []byte(`#!`+bash+`
{
  echo "args=$*"
  compgen -e
} > "$HOOK_TEST_RECORD"
`))
	cmd := exec.Command(hookPath)
	cmd.Dir = dir
	cmd.Env = append(testfs.EnvWithoutGit(),
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOOK_TEST_RECORD="+record,
		"HOOK_TEST_KEPT=1",
		"GIT_DIR=/nowhere/.git",
		"GIT_INDEX_FILE=/nowhere/.git/index.lock",
		"GIT_WORK_TREE=/nowhere",
		"GIT_PREFIX=sub/",
		"GIT_EDITOR=:",
		"GIT_AUTHOR_NAME=someone",
		"GIT_EXEC_PATH=/nowhere/libexec",
		"GIT_CONFIG_COUNT=1",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "the hook must run: %s", out)

	data, err := os.ReadFile(record)
	require.NoError(t, err, "the stand-in make must run")
	lines := strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n")
	assert.Equal(t, "args=validate", lines[0])
	names := lines[1:]
	assert.Contains(t, names, "HOOK_TEST_KEPT", "the hook keeps the variables that are not git's")
	for _, name := range names {
		assert.False(t, strings.HasPrefix(name, "GIT_"), "the gate receives %s", name)
	}
}
