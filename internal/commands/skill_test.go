package commands

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
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

	res, err := NewSkillInstall("v1.0.0").(skillInstallCmd).Execute(context.Background(), in, nil)
	require.NoError(t, err)
	outs := res.Data.([]skillinstall.Outcome)
	require.Len(t, outs, 1)
	assert.Equal(t, "changed", outs[0].Action)

	res, err = NewSkillStatus("v1.0.0").(skillStatusCmd).Execute(context.Background(), in, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitStatus)
	rows := res.Data.([]skillinstall.Outcome)
	require.Len(t, rows, 1)
	assert.Equal(t, skillinstall.VerdictInSync, rows[0].Verdict)

	// exit 7 pins the skew case — the e2e harness can only assert nonzero,
	// so THIS test owns the exact value.
	res, err = NewSkillStatus("v9.9.9").(skillStatusCmd).Execute(context.Background(), in, nil)
	require.NoError(t, err)
	assert.Equal(t, 7, res.ExitStatus)

	res, err = NewSkillUninstall("v1.0.0").(skillUninstallCmd).Execute(context.Background(), in, nil)
	require.NoError(t, err)
	outs = res.Data.([]skillinstall.Outcome)
	assert.Equal(t, "changed", outs[0].Action)

	res, err = NewSkillStatus("v1.0.0").(skillStatusCmd).Execute(context.Background(), in, nil)
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
	res, err := NewSkillStatus("v1.0.0").(skillStatusCmd).Execute(context.Background(), in, nil)
	require.NoError(t, err)
	assert.Equal(t, 7, res.ExitStatus)
}

func TestSkillStatusFreshMachineEmptyReport(t *testing.T) {
	skillHome(t) // no harness dirs at all
	skillinstall.SetSource(skillTestSource(t))
	in := skillInput{strings: map[string]string{}}
	res, err := NewSkillStatus("v1.0.0").(skillStatusCmd).Execute(context.Background(), in, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitStatus)
	assert.Empty(t, res.Data.([]skillinstall.Outcome))
	assert.NotEmpty(t, res.Warnings)
}

func TestSkillInstallNothingDetectedIsError(t *testing.T) {
	skillHome(t)
	skillinstall.SetSource(skillTestSource(t))
	in := skillInput{strings: map[string]string{}}
	_, err := NewSkillInstall("v1.0.0").(skillInstallCmd).Execute(context.Background(), in, nil)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Hint, "--scope project")
}

// Preflight failure on ANY target changes nothing (the lease API refuses).
func TestSkillInstallBatchPreflightAllOrNothing(t *testing.T) {
	home := skillHome(t, ".claude", ".agents")
	skillinstall.SetSource(skillTestSource(t))

	foreignDir := filepath.Join(home, ".agents", "skills", "rancher")
	require.NoError(t, os.MkdirAll(foreignDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(foreignDir, "SKILL.md"), []byte("---\nname: x\n---\n"), 0o644))

	in := skillInput{strings: map[string]string{}} // agent=all scope=user
	_, err := NewSkillInstall("v1.0.0").(skillInstallCmd).Execute(context.Background(), in, nil)
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(home, ".claude", "skills", "rancher"))
	assert.True(t, os.IsNotExist(statErr), "claude target must be untouched")
}

// Execution-time failure mid-batch: remaining targets attempted; the
// structured error carries per-target outcomes AND cleanup warnings —
// a sweep warning must survive the error path, not just Result.Warnings.
func TestSkillInstallPartialFailureCarriesOutcomesAndWarnings(t *testing.T) {
	home := skillHome(t, ".agents", ".claude")
	skillinstall.SetSource(skillTestSource(t))

	// foreign-looking (non-empty, unmarked) staging residue next to the
	// claude target: sweep must report it, and the report must survive
	// the partial-failure error
	residue := filepath.Join(home, ".claude", "skills", ".rancher-skill-staging-cafe")
	require.NoError(t, os.MkdirAll(residue, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(residue, "junk.md"), []byte("junk\n"), 0o644))

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
	_, err := NewSkillInstall("v1.0.0").(skillInstallCmd).Execute(context.Background(), in, nil)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	report, ok := ce.Details.(skillinstall.Report)
	require.True(t, ok)
	require.Len(t, report.Outcomes, 2)
	actions := []string{report.Outcomes[0].Action, report.Outcomes[1].Action}
	assert.ElementsMatch(t, []string{"failed", "changed"}, actions)
	require.Len(t, report.Warnings, 1)
	assert.Contains(t, report.Warnings[0], ".rancher-skill-staging-cafe")
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
}
