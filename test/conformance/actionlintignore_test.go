package conformance

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	actionlintIgnoreArgRE = regexp.MustCompile(`-ignore '([^']*)'`)
	actionlintIgnoreAnyRE = regexp.MustCompile(`-ignore\b`)
)

// actionlintIgnores returns the patterns of the -ignore arguments in
// scripts/validate, and the number of places outside comment lines that
// spell -ignore at all. An ignore written another way is counted too.
func actionlintIgnores(t *testing.T) (patterns []string, spelled int) {
	t.Helper()
	data, err := os.ReadFile(validateScript)
	require.NoError(t, err)
	var code []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			code = append(code, line)
		}
	}
	text := strings.Join(code, "\n")
	for _, m := range actionlintIgnoreArgRE.FindAllStringSubmatch(text, -1) {
		patterns = append(patterns, m[1])
	}
	return patterns, len(actionlintIgnoreAnyRE.FindAllStringIndex(text, -1))
}

// ignoredBy reports whether actionlint drops a message: -ignore matches the
// message text alone, unanchored, without the file position and the kind.
func ignoredBy(patterns []string, message string) bool {
	for _, p := range patterns {
		if regexp.MustCompile(p).MatchString(message) {
			return true
		}
	}
	return false
}

// syncedWorkflow is the workflow scripts/validate lints on its own: it is
// synced from rancher/renovate-config, whose refreshGroup choice input lists
// an empty string as its first option.
const syncedWorkflow = ".github/workflows/renovate-vault.yml"

// emptyOptionMessage is the actionlint message for an empty choice option.
const emptyOptionMessage = `string should not be empty`

// TestActionlintIgnoresStayNarrow asserts that scripts/validate runs
// actionlint twice, with these ignores and no others. The first command lints
// every workflow but the synced one with two ignores: the self-repository
// form of a local action, and the self-repository form of the zizmor
// workflow called from a job. The second lints the synced workflow alone
// with the first of those and the empty choice option message. Each drops
// its own message; a message for another called workflow, for the relative
// form, an empty choice option in any other workflow, and any other message
// still fail the gate. When actionlint is installed the check also runs both
// commands over the workflows with the ignores taken from the script, and
// over a copy whose job calls another workflow.
func TestActionlintIgnoresStayNarrow(t *testing.T) {
	t.Parallel()
	patterns, spelled := actionlintIgnores(t)
	require.Len(t, patterns, 4, "the two actionlint commands carry two -ignore arguments each")
	assert.Equal(t, 4, spelled, "no other ignore is spelled outside comments in %s", validateScript)
	first, synced := patterns[:2], patterns[2:]
	assert.Equal(t, "invalid format because ref is missing", first[0])
	assert.Equal(t, []string{first[0], emptyOptionMessage}, synced)
	data, err := os.ReadFile(validateScript)
	require.NoError(t, err)
	assert.Contains(t, string(data), "synced="+syncedWorkflow+"\n", "the second command lints %s alone", syncedWorkflow)

	const format = ` is not following the format "owner/repo/path/to/workflow.yml@ref" nor "./path/to/workflow.yml". see https://docs.github.com/en/actions/learn-github-actions/reusing-workflows for more details`
	for _, c := range []struct {
		name, message           string
		droppedMain, droppedSyn bool
	}{
		{"local action", `specifying action "$/.github/actions/setup-tools" in invalid format because ref is missing. available formats are "{owner}/{repo}@{ref}" or "{owner}/{repo}/{path}@{ref}"`, true, true},
		{"zizmor workflow", `reusable workflow call "$/.github/workflows/zizmor.yaml" at "uses"` + format, true, false},
		{"another called workflow", `reusable workflow call "$/.github/workflows/other.yaml" at "uses"` + format, false, false},
		{"a path that ends like the zizmor workflow", `reusable workflow call "$/.github/workflows/not-zizmor.yaml" at "uses"` + format, false, false},
		{"the relative form, file missing", `could not read reusable workflow file for "./.github/workflows/nothere.yaml": open /tmp/x/.github/workflows/nothere.yaml: no such file or directory`, false, false},
		{"the relative form, unknown input", `input "foo" is not defined in "./.github/workflows/zizmor.yaml" reusable workflow. no input is defined`, false, false},
		{"an empty choice option", emptyOptionMessage, false, true},
		{"an unrelated message", `property "foo" is not defined in object type {}`, false, false},
	} {
		assert.Equal(t, c.droppedMain, ignoredBy(first, c.message), "first command: %s", c.name)
		assert.Equal(t, c.droppedSyn, ignoredBy(synced, c.message), "second command: %s", c.name)
	}

	t.Run("actionlint", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip(posixOnlyReason)
		}
		tool, err := exec.LookPath("actionlint")
		if err != nil {
			t.Skip("actionlint is not installed; scripts/validate treats it as best effort")
		}
		run := func(dir string, ignores []string, files ...string) (string, int) {
			var args []string
			for _, p := range ignores {
				args = append(args, "-ignore", p)
			}
			cmd := exec.CommandContext(t.Context(), tool, append(args, files...)...)
			cmd.Env = testfs.EnvWithoutGit()
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err == nil {
				return string(out), 0
			}
			exitErr, ok := errors.AsType[*exec.ExitError](err)
			require.True(t, ok, "%v", err)
			return string(out), exitErr.ExitCode()
		}
		// others lists the workflows of dir but the synced one, as the
		// first command of scripts/validate does.
		others := func(dir string) []string {
			entries, err := os.ReadDir(filepath.Join(dir, ".github", "workflows"))
			require.NoError(t, err)
			var files []string
			for _, e := range entries {
				f := ".github/workflows/" + e.Name()
				if (strings.HasSuffix(f, ".yml") || strings.HasSuffix(f, ".yaml")) && f != syncedWorkflow {
					files = append(files, f)
				}
			}
			require.NotEmpty(t, files)
			return files
		}

		root, err := os.Getwd()
		require.NoError(t, err)
		out, code := run(root, first, others(root)...)
		require.Zero(t, code, "the repository workflows must pass the first command:\n%s", out)
		out, code = run(root, synced, syncedWorkflow)
		require.Zero(t, code, "the synced workflow must pass the second command:\n%s", out)
		out, code = run(root, first, syncedWorkflow)
		assert.NotZero(t, code, "the synced workflow needs the empty option ignore; drop it from the second command once the template no longer needs it")
		assert.Contains(t, out, emptyOptionMessage)

		// A copy of the workflows is a project of its own: an empty .git
		// directory marks the root, without which actionlint stops with
		// "no project was found" for good and changed copies alike.
		copyDir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(copyDir, ".git"), 0o755))
		workflows := filepath.Join(copyDir, ".github", "workflows")
		require.NoError(t, os.MkdirAll(workflows, 0o755))
		entries, err := os.ReadDir(filepath.Join(root, ".github", "workflows"))
		require.NoError(t, err)
		for _, e := range entries {
			data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", e.Name()))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(workflows, e.Name()), data, 0o644))
		}
		out, code = run(copyDir, first, others(copyDir)...)
		require.Zero(t, code, "the copy must be a valid project:\n%s", out)

		release := filepath.Join(workflows, "release.yaml")
		data, err := os.ReadFile(release)
		require.NoError(t, err)
		changed := strings.Replace(string(data), "uses: "+zizmorCallRef, "uses: $/.github/workflows/other.yaml", 1)
		require.NotEqual(t, string(data), changed, "release.yaml must call %s", zizmorCallRef)
		require.NoError(t, os.WriteFile(release, []byte(changed), 0o644))
		out, code = run(copyDir, first, others(copyDir)...)
		assert.NotZero(t, code, "a call to another workflow must still fail")
		assert.Contains(t, out, "other.yaml")
	})
}
