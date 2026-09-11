package skill

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/skillinstall"
)

type skillInput struct {
	strings map[string]string
	bools   map[string]bool
}

func (f skillInput) String(k string) string  { return f.strings[k] }
func (f skillInput) Bool(k string) bool      { return f.bools[k] }
func (f skillInput) Strings(string) []string { return nil }
func (f skillInput) IsSet(k string) bool     { _, ok := f.strings[k]; return ok }
func (f skillInput) Args() []string          { return nil }

func skillTestSource(t *testing.T) fs.FS {
	t.Helper()
	return fstest.MapFS{
		"SKILL.md": &fstest.MapFile{Data: []byte("---\nname: rancher\ndescription: Operate Rancher.\n---\n\n# Body\n")},
		"ref.md":   &fstest.MapFile{Data: []byte("reference\n")},
	}
}

func skillHome(t *testing.T, harnessDirs ...string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache")) // linux: os.UserCacheDir precedence over HOME
	t.Setenv("LocalAppData", home)                            // windows: sandbox os.UserCacheDir too
	t.Setenv("CODEX_HOME", "")
	t.Setenv("COPILOT_HOME", "")
	for _, d := range harnessDirs {
		require.NoError(t, os.MkdirAll(filepath.Join(home, d), 0o755))
	}
	return home
}

func TestSkillInstallStatusUninstallRoundTrip(t *testing.T) {
	skillHome(t, ".claude")
	skillinstall.SetSource(skillTestSource(t))
	in := skillInput{strings: map[string]string{"agent": "claude", "scope": "user"}}

	res, err := NewInstall("v1.0.0").(skillInstallCmd).Execute(t.Context(), in, nil)
	require.NoError(t, err)
	outs := res.Data.(skillOutcomes)
	require.Len(t, outs, 1)
	assert.Equal(t, "changed", outs[0].Action)
	assert.Equal(t, "v1.0.0", outs[0].CLIVersion,
		"a changed target reports the version that was just installed")

	res, err = NewStatus("v1.0.0").(skillStatusCmd).Execute(t.Context(), in, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitStatus)
	rows := res.Data.(skillOutcomes)
	require.Len(t, rows, 1)
	assert.Equal(t, skillinstall.VerdictInSync, rows[0].Verdict)

	// exit 7 asserts the skew case - the e2e harness can only assert nonzero,
	// so THIS test owns the exact value.
	res, err = NewStatus("v9.9.9").(skillStatusCmd).Execute(t.Context(), in, nil)
	require.NoError(t, err)
	assert.Equal(t, 7, res.ExitStatus)

	res, err = NewUninstall("v1.0.0").(skillUninstallCmd).Execute(t.Context(), in, nil)
	require.NoError(t, err)
	outs = res.Data.(skillOutcomes)
	assert.Equal(t, "changed", outs[0].Action)

	res, err = NewStatus("v1.0.0").(skillStatusCmd).Execute(t.Context(), in, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitStatus) // absent is neutral
}

func TestSkillStatusRequireInstalled(t *testing.T) {
	skillHome(t, ".claude")
	skillinstall.SetSource(skillTestSource(t))
	in := skillInput{
		strings: map[string]string{"agent": "claude", "scope": "user"},
		bools:   map[string]bool{"require-installed": true},
	}
	res, err := NewStatus("v1.0.0").(skillStatusCmd).Execute(t.Context(), in, nil)
	require.NoError(t, err)
	assert.Equal(t, 7, res.ExitStatus)
}

func TestSkillStatusFreshMachineEmptyReport(t *testing.T) {
	skillHome(t) // no harness dirs at all
	skillinstall.SetSource(skillTestSource(t))
	in := skillInput{strings: map[string]string{}}
	res, err := NewStatus("v1.0.0").(skillStatusCmd).Execute(t.Context(), in, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitStatus)
	assert.Empty(t, res.Data.(skillOutcomes))
	assert.NotEmpty(t, res.Warnings)
}

func TestSkillInstallNothingDetectedIsError(t *testing.T) {
	skillHome(t)
	skillinstall.SetSource(skillTestSource(t))
	in := skillInput{strings: map[string]string{}}
	_, err := NewInstall("v1.0.0").(skillInstallCmd).Execute(t.Context(), in, nil)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "--scope project")
}

// Preflight failure on ANY target changes nothing (the lease API refuses).
func TestSkillInstallBatchPreflightAllOrNothing(t *testing.T) {
	home := skillHome(t, ".claude", ".agents")
	skillinstall.SetSource(skillTestSource(t))

	foreignDir := filepath.Join(home, ".agents", "skills", "rancher")
	require.NoError(t, os.MkdirAll(foreignDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(foreignDir, "SKILL.md"), []byte("---\nname: x\n---\n"), 0o644))

	in := skillInput{strings: map[string]string{}} // agent=all scope=user
	_, err := NewInstall("v1.0.0").(skillInstallCmd).Execute(t.Context(), in, nil)
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(home, ".claude", "skills", "rancher"))
	assert.True(t, os.IsNotExist(statErr), "claude target must be untouched")
}

// Execution-time failure mid-batch: remaining targets attempted; the
// structured error carries per-target outcomes (Details) AND cleanup
// warnings (the error's own warning ride) - a sweep warning must survive
// the error path, not just Result.Warnings.
func TestSkillInstallPartialFailureCarriesOutcomesAndWarnings(t *testing.T) {
	home := skillHome(t, ".agents", ".claude")
	skillinstall.SetSource(skillTestSource(t))

	// foreign-looking (non-empty, unmarked) leftover staging entry next to the
	// claude target: sweep must report it, and the report must survive
	// the partial-failure error
	leftover := filepath.Join(home, ".claude", "skills", ".rancher-skill-staging-cafe")
	require.NoError(t, os.MkdirAll(leftover, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(leftover, "junk.md"), []byte("junk\n"), 0o644))

	calls := 0
	skillinstall.SetRenameHookForTest(func() error {
		calls++
		if calls == 1 {
			return fmt.Errorf("induced execution failure")
		}
		return nil
	})
	defer skillinstall.SetRenameHookForTest(nil)

	in := skillInput{strings: map[string]string{}} // agent=all → agents+claude (sorted by dir at execution)
	_, err := NewInstall("v1.0.0").(skillInstallCmd).Execute(t.Context(), in, nil)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	report, ok := ce.Details.(skillinstall.Report)
	require.True(t, ok)
	require.Len(t, report.Outcomes, 2)
	actions := []string{report.Outcomes[0].Action, report.Outcomes[1].Action}
	assert.ElementsMatch(t, []string{"failed", "changed"}, actions)
	warnings := ce.Warnings()
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], ".rancher-skill-staging-cafe")
	// filesystem state matches the outcomes
	var changed, failed int
	for _, o := range report.Outcomes {
		if _, err := os.Stat(o.Target.Dir); err == nil {
			changed++
		} else {
			failed++
		}
	}
	assert.Equal(t, 1, changed)
	assert.Equal(t, 1, failed)

	// Default-format text: the cleanup warning (WithWarnings) prints as a
	// "# warning:" line ahead of the error, then the outcomes projection
	// (Details.TextDetails) - one channel each, never both under data.error.
	var errBuf bytes.Buffer
	_ = runtime.RenderError(&errBuf, "", ce)
	text := errBuf.String()
	assert.Contains(t, text, "# warning: ")
	assert.Contains(t, text, ".rancher-skill-staging-cafe")
	assert.Contains(t, text, "error (GENERIC): skill install changed only part of the selected targets")
	assert.Contains(t, text, "full report: rancher skill status, or re-run with -o json")
}

// TestSkillOutcomesImplementsTabular asserts that skillOutcomes implements
// output.Tabular and that its column headers and row cells are correct.
// This asserts the default (-o table) rendering path, which the existing tests
// exercise only via -o json.
func TestSkillOutcomesImplementsTabular(t *testing.T) {
	t.Parallel()
	var _ output.Tabular = skillOutcomes(nil)

	s := skillOutcomes{
		{
			Target:     skillinstall.Target{Harness: "claude", Scope: skillinstall.ScopeUser, Dir: "/home/bob/.claude/skills/rancher"},
			Action:     "changed",
			CLIVersion: "v1.2.3",
		},
		{
			Target: skillinstall.Target{Harness: "agents", Scope: skillinstall.ScopeUser, Dir: "/home/bob/.agents/skills/rancher"},
			Action: "skipped",
			Reason: "not installed",
		},
	}
	assert.Equal(t, []string{"HARNESS", "SCOPE", "ACTION", "VERDICT", "VERSION", "DETAIL"}, s.Columns())
	rows := s.Rows()
	require.Len(t, rows, 2)
	assert.Equal(t, "claude", rows[0][0])
	assert.Equal(t, "user", rows[0][1])
	assert.Equal(t, "changed", rows[0][2])
	assert.Equal(t, "", rows[0][3]) // no verdict for install
	assert.Equal(t, "v1.2.3", rows[0][4])
	assert.Equal(t, "/home/bob/.claude/skills/rancher", rows[0][5]) // dir when no reason
	assert.Equal(t, "agents", rows[1][0])
	assert.Equal(t, "skipped", rows[1][2])
	assert.Equal(t, "not installed", rows[1][5]) // reason when set
}
