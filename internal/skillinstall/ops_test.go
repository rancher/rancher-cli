package skillinstall

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testfs"
)

func targetIn(t *testing.T) Target {
	t.Helper()
	return Target{Harness: "claude", Scope: ScopeUser,
		Dir: filepath.Join(t.TempDir(), ".claude", "skills", "rancher")}
}

// testLockDir derives a deterministic lock dir from the target's own temp
// base, so single-target helpers and direct Acquire calls in the same test
// share one lock directory without threading a variable through every call.
func testLockDir(tg Target) string {
	return filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(tg.Dir))), ".test-locks")
}

func install(t *testing.T, tg Target, version string) {
	t.Helper()
	leases, err := Acquire(testLockDir(tg), []Target{tg}, "install", false)
	require.NoError(t, err)
	_, err = leases[0].Install(version, false)
	ReleaseAll(leases)
	require.NoError(t, err)
}

// installIfChanged runs InstallIfChanged under a lease, as the command does.
func installIfChanged(t *testing.T, tg Target, version string, force bool) (bool, []string, error) {
	t.Helper()
	leases, err := Acquire(testLockDir(tg), []Target{tg}, "install", force)
	require.NoError(t, err)
	defer ReleaseAll(leases)
	return leases[0].InstallIfChanged(version, force)
}

// countRenames counts the final renames of an install until the test ends.
func countRenames(t *testing.T) *int {
	t.Helper()
	n := 0
	renameHook = func() error { n++; return nil }
	t.Cleanup(func() { renameHook = nil })
	return &n
}

func TestInstallThenStatusInSync(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	install(t, tg, "v1.0.0")
	v, installed, err := StatusTarget(tg, "v1.0.0")
	require.NoError(t, err)
	assert.Equal(t, VerdictInSync, v)
	assert.Equal(t, "v1.0.0", installed)
}

func TestInstallIdempotentAndUpgradeClearsSkew(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	install(t, tg, "v1.0.0")
	install(t, tg, "v1.0.0")
	v, _, _ := StatusTarget(tg, "v2.0.0")
	assert.Equal(t, VerdictSkew, v)
	install(t, tg, "v2.0.0")
	v, _, _ = StatusTarget(tg, "v2.0.0")
	assert.Equal(t, VerdictInSync, v)
}

func TestStatusModifiedOnBodyEditMissingExtraAndSymlink(t *testing.T) {
	skipIfNoSymlinks(t)
	SetSource(testSource(t))
	tg := targetIn(t)

	install(t, tg, "v1.0.0")
	require.NoError(t, os.WriteFile(filepath.Join(tg.Dir, "ref.md"), []byte("edited\n"), 0o644))
	v, _, _ := StatusTarget(tg, "v1.0.0")
	assert.Equal(t, VerdictModified, v)

	install(t, tg, "v1.0.0")
	require.NoError(t, os.Remove(filepath.Join(tg.Dir, "ref.md")))
	v, _, _ = StatusTarget(tg, "v1.0.0")
	assert.Equal(t, VerdictModified, v)

	install(t, tg, "v1.0.0")
	require.NoError(t, os.WriteFile(filepath.Join(tg.Dir, "extra.md"), []byte("x\n"), 0o644))
	v, _, _ = StatusTarget(tg, "v1.0.0")
	assert.Equal(t, VerdictModified, v)

	// internal symlink: ReadTree rejects it; status classifies modified
	// without ever reading through the link.
	install(t, tg, "v1.0.0")
	outside := filepath.Join(t.TempDir(), "outside.md")
	require.NoError(t, os.WriteFile(outside, []byte("outside\n"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(tg.Dir, "sneaky.md")))
	v, _, _ = StatusTarget(tg, "v1.0.0")
	assert.Equal(t, VerdictModified, v)
}

func TestForeignRefusedWithoutForce(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	require.NoError(t, os.MkdirAll(tg.Dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tg.Dir, "SKILL.md"), []byte("---\nname: other\n---\nnot ours"), 0o644))

	_, err := Acquire(testLockDir(tg), []Target{tg}, "install", false)
	require.Error(t, err)
	require.ErrorContains(t, err, "did not install")

	_, err = Acquire(testLockDir(tg), []Target{tg}, "uninstall", false)
	require.Error(t, err) // force never authorizes uninstall of foreign

	v, _, _ := StatusTarget(tg, "v1.0.0")
	assert.Equal(t, VerdictForeign, v)

	leases, err := Acquire(testLockDir(tg), []Target{tg}, "install", true)
	require.NoError(t, err)
	_, err = leases[0].Install("v1.0.0", true)
	ReleaseAll(leases)
	require.NoError(t, err)
	v, _, _ = StatusTarget(tg, "v1.0.0")
	assert.Equal(t, VerdictInSync, v)

	// A SKILL.md with CRLF line endings in its frontmatter does not parse as
	// an installation of ours.
	crlf := targetIn(t)
	install(t, crlf, "v1.0.0")
	p := filepath.Join(crlf.Dir, "SKILL.md")
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, []byte(strings.ReplaceAll(string(b), "\n", "\r\n")), 0o644))
	_, err = Acquire(testLockDir(crlf), []Target{crlf}, "install", false)
	require.ErrorContains(t, err, "did not install")
}

// A symlinked SKILL.md is the privileged install record path - it must never
// be followed. The linked-to file contains VALID owner-marked content: if
// the link were followed the verdict would be in-sync; foreign proves the
// outside bytes were never read.
func TestSymlinkedSkillMDIsForeignAndNeverFollowed(t *testing.T) {
	skipIfNoSymlinks(t)
	SetSource(testSource(t))
	real := targetIn(t)
	install(t, real, "v1.0.0")

	tg := targetIn(t)
	require.NoError(t, os.MkdirAll(tg.Dir, 0o755))
	require.NoError(t, os.Symlink(filepath.Join(real.Dir, "SKILL.md"), filepath.Join(tg.Dir, "SKILL.md")))

	v, _, err := StatusTarget(tg, "v1.0.0")
	require.NoError(t, err)
	assert.Equal(t, VerdictForeign, v)

	_, err = Acquire(testLockDir(tg), []Target{tg}, "install", false)
	require.Error(t, err) // foreign: refuse without force
	_, err = Acquire(testLockDir(tg), []Target{tg}, "uninstall", false)
	require.Error(t, err) // never remove through an unproven marker
}

func TestOwnershipNearMissIsForeign(t *testing.T) {
	SetSource(testSource(t))
	for _, fm := range []string{
		"---\nname: rancher\nmetadata:\n  rancherCliInstaller: skill-install/v2\n---\nbody",
		"---\nname: rancher\nmetadata:\n  rancherCliInstalled: skill-install/v1\n---\nbody",
	} {
		tg := targetIn(t)
		require.NoError(t, os.MkdirAll(tg.Dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(tg.Dir, "SKILL.md"), []byte(fm), 0o644))
		v, _, _ := StatusTarget(tg, "v1.0.0")
		assert.Equal(t, VerdictForeign, v)
		_, err := Acquire(testLockDir(tg), []Target{tg}, "install", false)
		require.Error(t, err)
	}
}

func TestSymlinkTargetRefused(t *testing.T) {
	skipIfNoSymlinks(t)
	SetSource(testSource(t))
	real := targetIn(t)
	install(t, real, "v1.0.0")
	parent := filepath.Join(t.TempDir(), ".agents", "skills")
	require.NoError(t, os.MkdirAll(parent, 0o755))
	link := Target{Harness: "agents", Scope: ScopeProject, Dir: filepath.Join(parent, "rancher")}
	require.NoError(t, os.Symlink(real.Dir, link.Dir))

	for _, force := range []bool{false, true} {
		_, err := Acquire(testLockDir(link), []Target{link}, "install", force)
		require.Error(t, err) // force does not bypass symlink refusal
	}
	_, err := Acquire(testLockDir(link), []Target{link}, "uninstall", false)
	require.Error(t, err)
	_, _, err = StatusTarget(link, "v1.0.0")
	require.Error(t, err)
	v, _, _ := StatusTarget(real, "v1.0.0")
	assert.Equal(t, VerdictInSync, v) // linked-to installation untouched
}

func TestUninstall(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)

	leases, err := Acquire(testLockDir(tg), []Target{tg}, "uninstall", false)
	require.NoError(t, err)
	removed, warnings, err := leases[0].Uninstall()
	ReleaseAll(leases)
	require.NoError(t, err) // absent = no-op success
	assert.False(t, removed)
	assert.Empty(t, warnings)

	install(t, tg, "v1.0.0")

	leases, err = Acquire(testLockDir(tg), []Target{tg}, "uninstall", false)
	require.NoError(t, err)
	removed, warnings, err = leases[0].Uninstall()
	ReleaseAll(leases)
	require.NoError(t, err)
	assert.True(t, removed)
	assert.Empty(t, warnings)

	v, _, _ := StatusTarget(tg, "v1.0.0")
	assert.Equal(t, VerdictAbsent, v)
}

// uninstallTarget runs Uninstall under a lease, as the command does.
func uninstallTarget(t *testing.T, tg Target) (bool, []string) {
	t.Helper()
	leases, err := Acquire(testLockDir(tg), []Target{tg}, "uninstall", false)
	require.NoError(t, err)
	defer ReleaseAll(leases)
	removed, warnings, err := leases[0].Uninstall()
	require.NoError(t, err)
	return removed, warnings
}

// An uninstall that leaves its skills directory empty removes it, and goes
// no further up: the harness folder stays.
func TestUninstallRemovesEmptyParentOnly(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	install(t, tg, "v1.0.0")
	parent := filepath.Dir(tg.Dir)
	harnessDir := filepath.Dir(parent)

	removed, warnings := uninstallTarget(t, tg)
	assert.True(t, removed)
	assert.Empty(t, warnings)
	assert.NoDirExists(t, parent)
	assert.DirExists(t, harnessDir)
}

// A skills directory that holds anything else stays, without a message:
// another skill, a leftover temporary directory or any other entry.
func TestUninstallKeepsParentWithOtherEntries(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, parent string){
		"another skill": func(t *testing.T, parent string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(filepath.Join(parent, "other"), 0o755))
		},
		"leftover temporary directory": func(t *testing.T, parent string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(filepath.Join(parent, stagingPrefix+"cafe"), 0o755))
		},
		"plain file": func(t *testing.T, parent string) {
			t.Helper()
			require.NoError(t, os.WriteFile(filepath.Join(parent, "notes.txt"), []byte("x\n"), 0o644))
		},
	} {
		t.Run(name, func(t *testing.T) {
			SetSource(testSource(t))
			tg := targetIn(t)
			install(t, tg, "v1.0.0")
			parent := filepath.Dir(tg.Dir)
			setup(t, parent)
			before, err := os.ReadDir(parent)
			require.NoError(t, err)

			removed, warnings := uninstallTarget(t, tg)
			assert.True(t, removed)
			assert.Empty(t, warnings)
			assert.NoDirExists(t, tg.Dir)
			after, err := os.ReadDir(parent)
			require.NoError(t, err)
			assert.Len(t, after, len(before)-1, "only the target directory is gone")
		})
	}
}

// A target that was not installed removes nothing, an empty skills directory
// included.
func TestUninstallAbsentTargetKeepsEmptyParent(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	parent := filepath.Dir(tg.Dir)
	require.NoError(t, os.MkdirAll(parent, 0o755))

	removed, warnings := uninstallTarget(t, tg)
	assert.False(t, removed)
	assert.Empty(t, warnings)
	assert.DirExists(t, parent)
}

// A skills directory that is a symlink stays, and so does the directory it
// points to.
func TestUninstallKeepsSymlinkedParent(t *testing.T) {
	skipIfNoSymlinks(t)
	SetSource(testSource(t))
	base := t.TempDir()
	realDir := filepath.Join(base, "real-skills")
	require.NoError(t, os.MkdirAll(realDir, 0o755))
	harnessDir := filepath.Join(base, ".claude")
	require.NoError(t, os.MkdirAll(harnessDir, 0o755))
	parent := filepath.Join(harnessDir, "skills")
	testfs.Symlink(t, realDir, parent)
	tg := Target{Harness: "claude", Scope: ScopeUser, Dir: filepath.Join(parent, "rancher")}
	install(t, tg, "v1.0.0")

	removed, warnings := uninstallTarget(t, tg)
	assert.True(t, removed)
	assert.Empty(t, warnings)
	assert.NoDirExists(t, filepath.Join(realDir, "rancher"))
	info, err := os.Lstat(parent)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the link stays")
	assert.DirExists(t, realDir)
}

// An empty skills directory that cannot be removed adds one warning that
// names it and the error; the target is still removed and no error returns.
func TestUninstallEmptyParentRemovalFailureWarns(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	install(t, tg, "v1.0.0")
	parent := filepath.Dir(tg.Dir)
	removeParentHook = func(string) error { return fmt.Errorf("induced removal failure") }
	t.Cleanup(func() { removeParentHook = nil })

	removed, warnings := uninstallTarget(t, tg)
	assert.True(t, removed)
	assert.Equal(t, []string{
		"could not remove the empty directory " + parent + ": induced removal failure",
	}, warnings)
	assert.NoDirExists(t, tg.Dir)
	assert.DirExists(t, parent)
}

// A removal that fails because an entry appeared in the directory since it was
// read is not a failure to report: the directory is not empty any more.
func TestUninstallParentRemovalRaceWithNewEntryIsSilent(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	install(t, tg, "v1.0.0")
	parent := filepath.Dir(tg.Dir)
	removeParentHook = func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, "late.txt"), []byte("x\n"), 0o644); err != nil {
			return err
		}
		return fmt.Errorf("directory not empty")
	}
	t.Cleanup(func() { removeParentHook = nil })

	removed, warnings := uninstallTarget(t, tg)
	assert.True(t, removed)
	assert.Empty(t, warnings)
	assert.DirExists(t, parent)
}

// Crash between remove and rename leaves ABSENT (never mixed) - asserted via
// the deterministic renameHook seam, not by staging simulation.
func TestCrashBetweenRemoveAndRenameLeavesAbsent(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	install(t, tg, "v1.0.0")

	renameHook = func() error { return fmt.Errorf("induced crash") }
	defer func() { renameHook = nil }()
	leases, err := Acquire(testLockDir(tg), []Target{tg}, "install", false)
	require.NoError(t, err)
	_, err = leases[0].Install("v2.0.0", false)
	ReleaseAll(leases)
	require.Error(t, err)

	renameHook = nil
	v, _, _ := StatusTarget(tg, "v2.0.0")
	assert.Equal(t, VerdictAbsent, v) // absent, never mixed
	install(t, tg, "v2.0.0")          // re-run repairs
	v, _, _ = StatusTarget(tg, "v2.0.0")
	assert.Equal(t, VerdictInSync, v)
}

// Staging sweep buckets: empty dir removed (pre-marker crash leftover),
// complete-marker dir removed (ours), non-empty unmarked dir - including
// the truncated-SKILL.md crash shape - kept AND warned.
func TestStagingSweepBuckets(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	install(t, tg, "v1.0.0")
	parent := filepath.Dir(tg.Dir)

	empty := filepath.Join(parent, ".rancher-skill-staging-00")
	require.NoError(t, os.MkdirAll(empty, 0o755))

	marked := filepath.Join(parent, ".rancher-skill-staging-deadbeef")
	require.NoError(t, os.MkdirAll(marked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(marked, OwnerMarkerFile), []byte(OwnerValue+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(marked, "SKILL.md"), []byte("partial"), 0o644))

	// truncated first-file crash shape: content but no (complete) marker
	unmarked := filepath.Join(parent, ".rancher-skill-staging-cafe")
	require.NoError(t, os.MkdirAll(unmarked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(unmarked, "SKILL.md"), []byte("---\nname: ranch"), 0o644))

	// crash shape: temporary file stranded in the parent (written
	// outside staging, crash before the rename into staging)
	strayTmp := filepath.Join(parent, ".rancher-skill-marker-beef.tmp")
	require.NoError(t, os.WriteFile(strayTmp, []byte("skill-inst"), 0o644))

	// crash shape: file from the check that the directory is writable stranded in the parent
	// when os.Remove failed after probeWritable's CreateTemp (e.g.
	// transient lock on Windows)
	strayProbe := filepath.Join(parent, ".rancher-skill-probe-deadcafe")
	require.NoError(t, os.WriteFile(strayProbe, []byte(""), 0o644))

	leases, err := Acquire(testLockDir(tg), []Target{tg}, "install", false)
	require.NoError(t, err)
	warnings, err := leases[0].Install("v1.0.0", false)
	ReleaseAll(leases)
	require.NoError(t, err)

	_, statErr := os.Stat(empty)
	assert.True(t, os.IsNotExist(statErr), "empty leftover temporary directory must be removed")
	_, statErr = os.Stat(marked)
	assert.True(t, os.IsNotExist(statErr), "marker-complete leftover temporary directory must be removed")
	_, statErr = os.Stat(unmarked)
	require.NoError(t, statErr, "unmarked non-empty leftover must remain")
	_, statErr = os.Stat(strayTmp)
	assert.True(t, os.IsNotExist(statErr), "stray temporary file must be removed")
	_, statErr = os.Stat(strayProbe)
	assert.True(t, os.IsNotExist(statErr), "stray file from the check that the directory is writable must be removed")
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], ".rancher-skill-staging-cafe")
}

// Staged-tree validation failure: the target is left exactly as it was.
func TestStagedValidationFailureLeavesTargetUntouched(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	install(t, tg, "v1.0.0")

	postStageHook = func(staging string) error {
		// corrupt the staged body AFTER population: digest check must catch it
		return os.WriteFile(filepath.Join(staging, "ref.md"), []byte("corrupted\n"), 0o644)
	}
	defer func() { postStageHook = nil }()

	leases, err := Acquire(testLockDir(tg), []Target{tg}, "install", false)
	require.NoError(t, err)
	_, err = leases[0].Install("v2.0.0", false)
	ReleaseAll(leases)
	require.Error(t, err)
	require.ErrorContains(t, err, "the files to install failed validation")

	postStageHook = nil
	v, installed, err := StatusTarget(tg, "v1.0.0")
	require.NoError(t, err)
	assert.Equal(t, VerdictInSync, v, "target must be untouched")
	assert.Equal(t, "v1.0.0", installed)
}

// Population failure mid-staging: primary error surfaces, staging is
// cleaned, and nothing foreign-looking is stranded in the parent.
func TestStagingPopulationFailureLeavesNoLeftovers(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)

	stageHook = func(relPath string) error {
		if relPath == "ref.md" {
			return fmt.Errorf("induced population failure")
		}
		return nil
	}
	defer func() { stageHook = nil }()

	leases, err := Acquire(testLockDir(tg), []Target{tg}, "install", false)
	require.NoError(t, err)
	_, err = leases[0].Install("v1.0.0", false)
	ReleaseAll(leases)
	require.Error(t, err)
	require.ErrorContains(t, err, "induced population failure")

	entries, err := os.ReadDir(filepath.Dir(tg.Dir))
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".rancher-skill-staging-"),
			"leftover temporary directory stranded: %s", e.Name())
	}
}

// Absent uninstall is a FILESYSTEM no-op: no parent dirs, no lock files.
func TestUninstallAbsentIsFilesystemNoOp(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t) // parent chain never created
	parent := filepath.Dir(tg.Dir)

	leases, err := Acquire(testLockDir(tg), []Target{tg}, "uninstall", false)
	require.NoError(t, err)
	removed, _, err := leases[0].Uninstall()
	ReleaseAll(leases)
	require.NoError(t, err)
	assert.False(t, removed)
	_, statErr := os.Stat(parent)
	assert.True(t, os.IsNotExist(statErr), "absent uninstall must not create harness dirs")
}

// The absence race is closed by the shared lock directory, not rechecking: a
// held absent-target uninstall lease blocks a concurrent install of the
// same target outright.
func TestUninstallAbsentLeaseBlocksConcurrentInstall(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)

	leases, err := Acquire(testLockDir(tg), []Target{tg}, "uninstall", false)
	require.NoError(t, err)

	// a concurrent install in the same lock directory must fail busy
	_, err = Acquire(testLockDir(tg), []Target{tg}, "install", false)
	require.Error(t, err)
	require.ErrorContains(t, err, "another skill operation")

	removed, _, err := leases[0].Uninstall()
	ReleaseAll(leases)
	require.NoError(t, err)
	assert.False(t, removed)
}

// The lock key is canonical: a path reached through a symlinked parent
// and its physical path key the SAME lock file, and a lease held on one
// blocks acquisition through the other. (Config-dir independence is
// structural: the production domain is DefaultLockDir - os.UserCacheDir -
// which RANCHER_CLI_CONFIG_DIR never influences.)
func TestLockKeyCanonicalAcrossSymlinkedParents(t *testing.T) {
	skipIfNoSymlinks(t)
	SetSource(testSource(t))
	base := t.TempDir()
	physical := filepath.Join(base, "real", ".claude", "skills", "rancher")
	require.NoError(t, os.MkdirAll(filepath.Dir(physical), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")))
	aliased := filepath.Join(base, "alias", ".claude", "skills", "rancher")

	lockDir := t.TempDir()
	assert.Equal(t, lockFilePath(lockDir, physical), lockFilePath(lockDir, aliased))

	tgA := Target{Harness: "claude", Scope: ScopeUser, Dir: physical}
	tgB := Target{Harness: "claude", Scope: ScopeUser, Dir: aliased}
	leases, err := Acquire(lockDir, []Target{tgA}, "install", false)
	require.NoError(t, err)
	defer ReleaseAll(leases)
	_, err = Acquire(lockDir, []Target{tgB}, "install", false)
	require.Error(t, err)
	assert.ErrorContains(t, err, "another skill operation")
}

// Batch acquire: a busy second target means ZERO targets change.
func TestAcquireBusySecondTargetChangesNothing(t *testing.T) {
	SetSource(testSource(t))
	lockDir := t.TempDir()
	a, b := targetIn(t), targetIn(t)
	require.NoError(t, os.MkdirAll(filepath.Join(lockDir, "skill-locks"), 0o700))
	fl := flock.New(lockFilePath(lockDir, b.Dir))
	locked, err := fl.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	defer func() { _ = fl.Unlock() }()

	_, err = Acquire(lockDir, []Target{a, b}, "install", false)
	require.Error(t, err)
	require.ErrorContains(t, err, "another skill operation")
	_, statErr := os.Stat(a.Dir)
	assert.True(t, os.IsNotExist(statErr), "first target must be untouched")
}

// TestLockFilePathCaseFold asserts the case-folding contract: on case-insensitive
// platforms (windows, darwin) two paths that differ only in case must produce
// the same lock file path so they serialize on the same lock.
func TestLockFilePathCaseFold(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("case folding only on windows/darwin")
	}
	lockDir := t.TempDir()
	assert.Equal(t, lockFilePath(lockDir, "/A/B/C"), lockFilePath(lockDir, "/a/b/c"))
}

func TestAcquireUnwritableParentChangesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-bit semantics differ on windows")
	}
	SetSource(testSource(t))
	lockDir := t.TempDir()
	a := targetIn(t)
	roParent := filepath.Join(t.TempDir(), "ro")
	require.NoError(t, os.MkdirAll(roParent, 0o555))
	b := Target{Harness: "agents", Scope: ScopeUser, Dir: filepath.Join(roParent, "skills", "rancher")}

	_, err := Acquire(lockDir, []Target{a, b}, "install", false)
	require.Error(t, err)
	_, statErr := os.Stat(a.Dir)
	assert.True(t, os.IsNotExist(statErr))
}

// A target that holds exactly the files an install would write now is left
// alone: no staging, no remove, no rename. The rename counter catches a
// rewrite within one clock tick, which the snapshot's times miss.
func TestInstallIfChangedLeavesCurrentTargetAlone(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	install(t, tg, "v1.0.0")
	before := testfs.Snapshot(t, tg.Dir)
	renames := countRenames(t)

	changed, warnings, err := installIfChanged(t, tg, "v1.0.0", false)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Empty(t, warnings)
	assert.Equal(t, before, testfs.Snapshot(t, tg.Dir))
	assert.Zero(t, *renames)
}

func TestInstallIfChangedRewritesWhatDiffers(t *testing.T) {
	// crlfAfter rewrites the line endings of the file to CRLF after the
	// first occurrence of marker; an empty marker rewrites the whole file.
	crlfAfter := func(t *testing.T, path, marker string) {
		t.Helper()
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		head, body, found := strings.Cut(string(b), marker)
		require.True(t, found)
		body = strings.ReplaceAll(body, "\n", "\r\n")
		require.NoError(t, os.WriteFile(path, []byte(head+marker+body), 0o644))
	}
	tests := []struct {
		name    string
		version string
		mutate  func(t *testing.T, dir string)
	}{
		{name: "another version", version: "v2.0.0", mutate: func(*testing.T, string) {}},
		{name: "edited body", version: "v1.0.0", mutate: func(t *testing.T, dir string) {
			t.Helper()
			p := filepath.Join(dir, "SKILL.md")
			b, err := os.ReadFile(p)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(p, append(b, "edit\n"...), 0o644))
		}},
		{name: "missing file", version: "v1.0.0", mutate: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(dir, "ref.md")))
		}},
		{name: "extra file", version: "v1.0.0", mutate: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "extra.md"), []byte("x\n"), 0o644))
		}},
		{name: "changed owner marker", version: "v1.0.0", mutate: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.WriteFile(filepath.Join(dir, OwnerMarkerFile), []byte(OwnerValue+"X\n"), 0o644))
		}},
		{name: "deleted owner marker", version: "v1.0.0", mutate: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(dir, OwnerMarkerFile)))
		}},
		{name: "symlink entry", version: "v1.0.0", mutate: func(t *testing.T, dir string) {
			t.Helper()
			outside := filepath.Join(t.TempDir(), "outside.md")
			require.NoError(t, os.WriteFile(outside, []byte("outside\n"), 0o644))
			testfs.Symlink(t, outside, filepath.Join(dir, "sneaky.md"))
		}},
		{name: "absent target", version: "v1.0.0", mutate: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.RemoveAll(dir))
		}},
		{name: "CRLF in SKILL.md body", version: "v1.0.0", mutate: func(t *testing.T, dir string) {
			t.Helper()
			crlfAfter(t, filepath.Join(dir, "SKILL.md"), "\n---\n")
		}},
		{name: "CRLF in ref.md", version: "v1.0.0", mutate: func(t *testing.T, dir string) {
			t.Helper()
			crlfAfter(t, filepath.Join(dir, "ref.md"), "")
		}},
		{name: "unreadable file", version: "v1.0.0", mutate: func(t *testing.T, dir string) {
			t.Helper()
			testfs.RestrictAccess(t, filepath.Join(dir, "ref.md"), 0o000)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			SetSource(testSource(t))
			tg := targetIn(t)
			install(t, tg, "v1.0.0")
			tc.mutate(t, tg.Dir)
			renames := countRenames(t)

			changed, _, err := installIfChanged(t, tg, tc.version, false)
			require.NoError(t, err)
			assert.True(t, changed)
			assert.Equal(t, 1, *renames)
			v, installed, err := StatusTarget(tg, tc.version)
			require.NoError(t, err)
			assert.Equal(t, VerdictInSync, v)
			assert.Equal(t, tc.version, installed)
		})
	}
}

// Two builds can carry one version string and different skill text; the
// second install must write the new text.
func TestInstallIfChangedRewritesWhenEmbeddedTextDiffers(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	install(t, tg, "v1.0.0")

	src := testSource(t)
	src["ref.md"].Data = []byte("reference, newer text\n")
	SetSource(src)
	changed, _, err := installIfChanged(t, tg, "v1.0.0", false)
	require.NoError(t, err)
	assert.True(t, changed)
	v, _, err := StatusTarget(tg, "v1.0.0")
	require.NoError(t, err)
	assert.Equal(t, VerdictInSync, v)
	got, err := os.ReadFile(filepath.Join(tg.Dir, "ref.md"))
	require.NoError(t, err)
	assert.Equal(t, "reference, newer text\n", string(got))
}

// ReadTree skips directories, so an empty directory in the tree does not make
// the target differ; skill status treats it the same way.
func TestInstallIfChangedIgnoresExtraEmptyDirectory(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	install(t, tg, "v1.0.0")
	require.NoError(t, os.Mkdir(filepath.Join(tg.Dir, "empty"), 0o755))
	v, _, err := StatusTarget(tg, "v1.0.0")
	require.NoError(t, err)
	require.Equal(t, VerdictInSync, v)
	renames := countRenames(t)

	changed, _, err := installIfChanged(t, tg, "v1.0.0", false)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Zero(t, *renames)
	assert.Contains(t, testfs.Tree(t, tg.Dir), "empty")
}

func TestInstallIfChangedForceRewritesCurrentTarget(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	install(t, tg, "v1.0.0")
	renames := countRenames(t)

	changed, _, err := installIfChanged(t, tg, "v1.0.0", true)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 1, *renames)
}

// Acquire refuses a directory that rancher-cli did not install unless force is
// set; with force, InstallIfChanged replaces it.
func TestInstallIfChangedReplacesForeignDirectoryWithForce(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	require.NoError(t, os.MkdirAll(tg.Dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tg.Dir, "SKILL.md"), []byte("---\nname: other\n---\nnot ours"), 0o644))

	_, err := Acquire(testLockDir(tg), []Target{tg}, "install", false)
	require.ErrorContains(t, err, "did not install")

	changed, _, err := installIfChanged(t, tg, "v1.0.0", true)
	require.NoError(t, err)
	assert.True(t, changed)
	v, _, err := StatusTarget(tg, "v1.0.0")
	require.NoError(t, err)
	assert.Equal(t, VerdictInSync, v)
}

// Leftovers are cleaned whether or not the target is current, and each
// warning is reported once.
func TestInstallIfChangedCleansLeftoversNextToCurrentTarget(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)
	install(t, tg, "v1.0.0")
	parent := filepath.Dir(tg.Dir)

	marked := filepath.Join(parent, stagingPrefix+"deadbeef")
	require.NoError(t, os.MkdirAll(marked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(marked, OwnerMarkerFile), []byte(OwnerValue+"\n"), 0o644))
	strayTmp := filepath.Join(parent, markerTmpPrefix+"beef.tmp")
	require.NoError(t, os.WriteFile(strayTmp, []byte("skill-inst"), 0o644))
	unknown := filepath.Join(parent, stagingPrefix+"cafe")
	require.NoError(t, os.MkdirAll(unknown, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(unknown, "junk.md"), []byte("junk\n"), 0o644))
	renames := countRenames(t)

	changed, warnings, err := installIfChanged(t, tg, "v1.0.0", false)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Zero(t, *renames)
	assert.NoDirExists(t, marked)
	assert.NoFileExists(t, strayTmp)
	assert.DirExists(t, unknown)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], stagingPrefix+"cafe")

	changed, warnings, err = installIfChanged(t, tg, "v2.0.0", false)
	require.NoError(t, err)
	assert.True(t, changed)
	require.Len(t, warnings, 1)
}
