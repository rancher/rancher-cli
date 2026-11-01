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
	"github.com/rancher/rancher-cli/internal/testfs"
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
	assert.Equal(t, skillinstall.VerdictInSync, outs[0].Verdict)

	res, err = NewInstall("v1.0.0").(skillInstallCmd).Execute(t.Context(), in, nil)
	require.NoError(t, err)
	outs = res.Data.(skillOutcomes)
	require.Len(t, outs, 1)
	assert.Equal(t, "unchanged", outs[0].Action)
	assert.Equal(t, skillinstall.VerdictInSync, outs[0].Verdict)
	assert.Equal(t, "v1.0.0", outs[0].CLIVersion)
	assert.Empty(t, outs[0].Reason)

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

// installRows runs skill install and returns its rows keyed by harness.
func installRows(t *testing.T, version string, in skillInput) map[string]skillinstall.Outcome {
	t.Helper()
	res, err := NewInstall(version).(skillInstallCmd).Execute(t.Context(), in, nil)
	require.NoError(t, err)
	rows := map[string]skillinstall.Outcome{}
	for _, o := range res.Data.(skillOutcomes) {
		rows[o.Target.Harness] = o
	}
	return rows
}

func TestSkillInstallUpgradeIsChangedAndAgreesWithStatus(t *testing.T) {
	skillHome(t, ".claude")
	skillinstall.SetSource(skillTestSource(t))
	in := skillInput{strings: map[string]string{"agent": "claude", "scope": "user"}}

	installRows(t, "v1.0.0", in)
	rows := installRows(t, "v2.0.0", in)
	assert.Equal(t, "changed", rows["claude"].Action)
	assert.Equal(t, "v2.0.0", rows["claude"].CLIVersion)
	assert.Equal(t, skillinstall.VerdictInSync, rows["claude"].Verdict)

	res, err := NewStatus("v2.0.0").(skillStatusCmd).Execute(t.Context(), in, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitStatus)
	status := res.Data.(skillOutcomes)
	require.Len(t, status, 1)
	assert.Equal(t, rows["claude"].Verdict, status[0].Verdict)
	assert.Equal(t, rows["claude"].CLIVersion, status[0].CLIVersion)
}

func TestSkillInstallMixedBatchAndForce(t *testing.T) {
	home := skillHome(t, ".claude", ".codex")
	skillinstall.SetSource(skillTestSource(t))
	in := skillInput{strings: map[string]string{}} // agent=all scope=user
	installRows(t, "v1.0.0", in)

	require.NoError(t, os.WriteFile(filepath.Join(home, ".codex", "skills", "rancher", "ref.md"), []byte("edited\n"), 0o644))
	rows := installRows(t, "v1.0.0", in)
	require.Len(t, rows, 2)
	assert.Equal(t, "unchanged", rows["claude"].Action)
	assert.Equal(t, "changed", rows["codex"].Action)
	for _, o := range rows {
		assert.Equal(t, skillinstall.VerdictInSync, o.Verdict)
		assert.Equal(t, "v1.0.0", o.CLIVersion)
	}

	forced := skillInput{strings: map[string]string{}, bools: map[string]bool{"force": true}}
	rows = installRows(t, "v1.0.0", forced)
	require.Len(t, rows, 2)
	for _, o := range rows {
		assert.Equal(t, "changed", o.Action)
		assert.Equal(t, skillinstall.VerdictInSync, o.Verdict)
	}
}

// TestSkillInstallTwiceLeavesEveryTargetAlone asserts, for every harness at
// user and project scope, that a second install of the same content reports
// unchanged and leaves the target directory as it was: the same files, modes,
// sizes, times and contents, and no remove or rename.
func TestSkillInstallTwiceLeavesEveryTargetAlone(t *testing.T) {
	for _, h := range skillinstall.Harnesses() {
		for _, scope := range []skillinstall.Scope{skillinstall.ScopeUser, skillinstall.ScopeProject} {
			t.Run(h.Name+"/"+string(scope), func(t *testing.T) {
				skillHome(t)
				skillinstall.SetSource(skillTestSource(t))
				if scope == skillinstall.ScopeProject {
					t.Chdir(t.TempDir())
				}
				in := skillInput{strings: map[string]string{"agent": h.Name, "scope": string(scope)}}

				first := installRows(t, "v1.0.0", in)
				require.Len(t, first, 1)
				var dir string
				for _, o := range first {
					assert.Equal(t, "changed", o.Action)
					dir = o.Target.Dir
				}
				before := testfs.Snapshot(t, dir)
				renames := 0
				skillinstall.SetRenameHookForTest(func() error { renames++; return nil })
				defer skillinstall.SetRenameHookForTest(nil)

				second := installRows(t, "v1.0.0", in)
				require.Len(t, second, 1)
				for _, o := range second {
					assert.Equal(t, "unchanged", o.Action)
					assert.Equal(t, skillinstall.VerdictInSync, o.Verdict)
					assert.Equal(t, dir, o.Target.Dir)
				}
				assert.Equal(t, before, testfs.Snapshot(t, dir))
				assert.Zero(t, renames)
			})
		}
	}
}

// When the check that gives the verdict fails, the row keeps its action, has
// no verdict, and a warning names the directory and the error.
func TestSkillInstallVerdictCheckFailureKeepsActionAndWarns(t *testing.T) {
	skillHome(t, ".claude")
	skillinstall.SetSource(skillTestSource(t))
	in := skillInput{strings: map[string]string{"agent": "claude", "scope": "user"}}
	statusTarget = func(skillinstall.Target, string) (skillinstall.Verdict, string, error) {
		return "", "", fmt.Errorf("induced check failure")
	}
	t.Cleanup(func() { statusTarget = skillinstall.StatusTarget })

	for _, want := range []string{"changed", "unchanged"} {
		res, err := NewInstall("v1.0.0").(skillInstallCmd).Execute(t.Context(), in, nil)
		require.NoError(t, err)
		outs := res.Data.(skillOutcomes)
		require.Len(t, outs, 1)
		assert.Equal(t, want, outs[0].Action)
		assert.Empty(t, outs[0].Verdict)
		assert.Equal(t, "v1.0.0", outs[0].CLIVersion)
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], outs[0].Target.Dir)
		assert.Contains(t, res.Warnings[0], "induced check failure")
	}
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
	home := skillHome(t, ".agents", ".claude", ".codex")
	skillinstall.SetSource(skillTestSource(t))
	installRows(t, "v1.0.0", skillInput{strings: map[string]string{"agent": "codex"}})

	// foreign-looking (non-empty, unmarked) leftover temporary directory next to the
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

	in := skillInput{strings: map[string]string{}} // agent=all → agents, claude and codex (sorted by dir at execution)
	_, err := NewInstall("v1.0.0").(skillInstallCmd).Execute(t.Context(), in, nil)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	report, ok := ce.Details.(skillinstall.Report)
	require.True(t, ok)
	require.Len(t, report.Outcomes, 3)
	var actions []string
	for _, o := range report.Outcomes {
		actions = append(actions, o.Action)
	}
	assert.ElementsMatch(t, []string{"failed", "changed", "unchanged"}, actions)
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
	assert.Equal(t, 2, changed)
	assert.Equal(t, 1, failed)

	// Default-format text: the cleanup warning (WithWarnings) prints as a
	// "# warning:" line ahead of the error, then the outcomes projection
	// (Details.TextDetails) - one channel each, never both under data.error.
	var errBuf bytes.Buffer
	_ = runtime.RenderError(&errBuf, "", ce)
	text := errBuf.String()
	assert.Contains(t, text, "# warning: ")
	assert.Contains(t, text, ".rancher-skill-staging-cafe")
	assert.Contains(t, text, "error (GENERIC): skill install failed for part of the selected targets")
	assert.Contains(t, text, filepath.Join(home, ".codex", "skills", "rancher")+": unchanged")
	assert.Contains(t, text, "to see each target's state, run: rancher skill status --scope user; to see every reason, run the command again with -o json")
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
			Verdict:    skillinstall.VerdictInSync,
			CLIVersion: "v1.2.3",
		},
		{
			Target: skillinstall.Target{Harness: "agents", Scope: skillinstall.ScopeUser, Dir: "/home/bob/.agents/skills/rancher"},
			Action: "skipped",
			Reason: "not installed",
		},
		{
			Target:     skillinstall.Target{Harness: "codex", Scope: skillinstall.ScopeUser, Dir: "/home/bob/.codex/skills/rancher"},
			Action:     "unchanged",
			Verdict:    skillinstall.VerdictInSync,
			CLIVersion: "v1.2.3",
		},
	}
	assert.Equal(t, []string{"HARNESS", "SCOPE", "ACTION", "VERDICT", "VERSION", "DETAIL"}, s.Columns())
	rows := s.Rows()
	require.Len(t, rows, 3)
	assert.Equal(t, "claude", rows[0][0])
	assert.Equal(t, "user", rows[0][1])
	assert.Equal(t, "changed", rows[0][2])
	assert.Equal(t, "in-sync", rows[0][3])
	assert.Equal(t, "v1.2.3", rows[0][4])
	assert.Equal(t, "/home/bob/.claude/skills/rancher", rows[0][5]) // dir when no reason
	assert.Equal(t, "agents", rows[1][0])
	assert.Equal(t, "skipped", rows[1][2])
	assert.Equal(t, "not installed", rows[1][5]) // reason when set
	assert.Equal(t, "unchanged", rows[2][2])
	assert.Equal(t, "in-sync", rows[2][3])
	assert.Equal(t, "v1.2.3", rows[2][4])
}
