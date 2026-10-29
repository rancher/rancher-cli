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

// TestActionlintIgnoresStayNarrow asserts that the actionlint command of
// scripts/validate carries two ignores and no more: the one for the
// self-repository form of a local action, and the one for the
// self-repository form of the zizmor workflow called from a job. Each drops
// its own message; a message for another called workflow, for the relative
// form, and any other message still fail the gate. When actionlint is
// installed the check also runs it over the workflows with the ignores
// taken from the script, and over a copy whose job calls another workflow.
func TestActionlintIgnoresStayNarrow(t *testing.T) {
	t.Parallel()
	patterns, spelled := actionlintIgnores(t)
	require.Len(t, patterns, 2, "the actionlint command carries exactly two -ignore arguments")
	assert.Equal(t, 2, spelled, "no other ignore is spelled outside comments in %s", validateScript)
	assert.Equal(t, "invalid format because ref is missing", patterns[0])

	const format = ` is not following the format "owner/repo/path/to/workflow.yml@ref" nor "./path/to/workflow.yml". see https://docs.github.com/en/actions/learn-github-actions/reusing-workflows for more details`
	for _, c := range []struct {
		name, message string
		dropped       bool
	}{
		{"local action", `specifying action "$/.github/actions/setup-tools" in invalid format because ref is missing. available formats are "{owner}/{repo}@{ref}" or "{owner}/{repo}/{path}@{ref}"`, true},
		{"zizmor workflow", `reusable workflow call "$/.github/workflows/zizmor.yaml" at "uses"` + format, true},
		{"another called workflow", `reusable workflow call "$/.github/workflows/other.yaml" at "uses"` + format, false},
		{"a path that ends like the zizmor workflow", `reusable workflow call "$/.github/workflows/not-zizmor.yaml" at "uses"` + format, false},
		{"the relative form, file missing", `could not read reusable workflow file for "./.github/workflows/nothere.yaml": open /tmp/x/.github/workflows/nothere.yaml: no such file or directory`, false},
		{"the relative form, unknown input", `input "foo" is not defined in "./.github/workflows/zizmor.yaml" reusable workflow. no input is defined`, false},
		{"an unrelated message", `property "foo" is not defined in object type {}`, false},
	} {
		assert.Equal(t, c.dropped, ignoredBy(patterns, c.message), c.name)
	}

	t.Run("actionlint", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip(posixOnlyReason)
		}
		tool, err := exec.LookPath("actionlint")
		if err != nil {
			t.Skip("actionlint is not installed; scripts/validate treats it as best effort")
		}
		var args []string
		for _, p := range patterns {
			args = append(args, "-ignore", p)
		}
		run := func(dir string) (string, int) {
			cmd := exec.CommandContext(t.Context(), tool, args...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err == nil {
				return string(out), 0
			}
			exitErr, ok := errors.AsType[*exec.ExitError](err)
			require.True(t, ok, "%v", err)
			return string(out), exitErr.ExitCode()
		}

		root, err := os.Getwd()
		require.NoError(t, err)
		out, code := run(root)
		require.Zero(t, code, "the repository workflows must pass with the ignores:\n%s", out)

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
		out, code = run(copyDir)
		require.Zero(t, code, "the copy must be a valid project:\n%s", out)

		release := filepath.Join(workflows, "release.yaml")
		data, err := os.ReadFile(release)
		require.NoError(t, err)
		changed := strings.Replace(string(data), "uses: "+zizmorCallRef, "uses: $/.github/workflows/other.yaml", 1)
		require.NotEqual(t, string(data), changed, "release.yaml must call %s", zizmorCallRef)
		require.NoError(t, os.WriteFile(release, []byte(changed), 0o644))
		out, code = run(copyDir)
		assert.NotZero(t, code, "a call to another workflow must still fail")
		assert.Contains(t, out, "other.yaml")
	})
}
