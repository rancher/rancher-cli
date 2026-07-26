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
)

func targetIn(t *testing.T) Target {
	return Target{Harness: "claude", Scope: ScopeUser,
		Dir: filepath.Join(t.TempDir(), ".claude", "skills", "rancher")}
}

// testLockDir derives a deterministic lock dir from the target's own temp
// base, so single-target helpers and direct Acquire calls in the same test
// share one lock domain without threading a variable through every call.
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
	assert.Contains(t, err.Error(), "foreign")

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
}

// A symlinked SKILL.md is the privileged provenance path — it must never
// be followed. The linked-to file contains VALID owner-marked content: if
// the link were followed the verdict would be in-sync; foreign proves the
// outside bytes were never read.
func TestSymlinkedSkillMDIsForeignAndNeverFollowed(t *testing.T) {
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
	SetSource(testSource(t))
	real := targetIn(t)
	install(t, real, "v1.0.0")
	parent := filepath.Join(t.TempDir(), ".agents", "skills")
	require.NoError(t, os.MkdirAll(parent, 0o755))
	link := Target{Harness: "agents", Scope: ScopeProject, Dir: filepath.Join(parent, "rancher")}
	require.NoError(t, os.Symlink(real.Dir, link.Dir))

	for _, force := range []bool{false, true} {
		_, err := Acquire(testLockDir(link), []Target{link}, "install", force)
		assert.Error(t, err) // force does not bypass symlink refusal
	}
	_, err := Acquire(testLockDir(link), []Target{link}, "uninstall", false)
	assert.Error(t, err)
	_, _, err = StatusTarget(link, "v1.0.0")
	assert.Error(t, err)
	v, _, _ := StatusTarget(real, "v1.0.0")
	assert.Equal(t, VerdictInSync, v) // linked-to installation untouched
}

func TestUninstall(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)

	leases, err := Acquire(testLockDir(tg), []Target{tg}, "uninstall", false)
	require.NoError(t, err)
	removed, err := leases[0].Uninstall()
	ReleaseAll(leases)
	require.NoError(t, err) // absent = no-op success
	assert.False(t, removed)

	install(t, tg, "v1.0.0")

	leases, err = Acquire(testLockDir(tg), []Target{tg}, "uninstall", false)
	require.NoError(t, err)
	removed, err = leases[0].Uninstall()
	ReleaseAll(leases)
	require.NoError(t, err)
	assert.True(t, removed)

	v, _, _ := StatusTarget(tg, "v1.0.0")
	assert.Equal(t, VerdictAbsent, v)
}

// Crash between remove and rename leaves ABSENT (never mixed) — pinned via
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

// Staging sweep buckets: empty dir removed (pre-marker crash residue),
// complete-marker dir removed (ours), non-empty unmarked dir — including
// the truncated-SKILL.md crash shape — kept AND warned.
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

	// crash shape: marker temp file stranded in the parent (written
	// outside staging, crash before the rename into staging)
	strayTmp := filepath.Join(parent, ".rancher-skill-marker-beef.tmp")
	require.NoError(t, os.WriteFile(strayTmp, []byte("skill-inst"), 0o644))

	// crash shape: probe file stranded in the parent when os.Remove failed
	// after probeWritable's CreateTemp (e.g. transient lock on Windows)
	strayProbe := filepath.Join(parent, ".rancher-skill-probe-deadcafe")
	require.NoError(t, os.WriteFile(strayProbe, []byte(""), 0o644))

	leases, err := Acquire(testLockDir(tg), []Target{tg}, "install", false)
	require.NoError(t, err)
	warnings, err := leases[0].Install("v1.0.0", false)
	ReleaseAll(leases)
	require.NoError(t, err)

	_, statErr := os.Stat(empty)
	assert.True(t, os.IsNotExist(statErr), "empty staging residue must be removed")
	_, statErr = os.Stat(marked)
	assert.True(t, os.IsNotExist(statErr), "marker-complete staging residue must be removed")
	_, statErr = os.Stat(unmarked)
	assert.NoError(t, statErr, "unmarked non-empty residue must remain")
	_, statErr = os.Stat(strayTmp)
	assert.True(t, os.IsNotExist(statErr), "stray marker temp file must be removed")
	_, statErr = os.Stat(strayProbe)
	assert.True(t, os.IsNotExist(statErr), "stray probe file must be removed")
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
	assert.Contains(t, err.Error(), "staged tree failed validation")

	postStageHook = nil
	v, installed, err := StatusTarget(tg, "v1.0.0")
	require.NoError(t, err)
	assert.Equal(t, VerdictInSync, v, "target must be untouched")
	assert.Equal(t, "v1.0.0", installed)
}

// Population failure mid-staging: primary error surfaces, staging is
// cleaned, and nothing foreign-looking is stranded in the parent.
func TestStagingPopulationFailureLeavesNoResidue(t *testing.T) {
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
	assert.Contains(t, err.Error(), "induced population failure")

	entries, err := os.ReadDir(filepath.Dir(tg.Dir))
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".rancher-skill-staging-"),
			"staging residue stranded: %s", e.Name())
	}
}

// Absent uninstall is a FILESYSTEM no-op: no parent dirs, no lock files.
func TestUninstallAbsentIsFilesystemNoOp(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t) // parent chain never created
	parent := filepath.Dir(tg.Dir)

	leases, err := Acquire(testLockDir(tg), []Target{tg}, "uninstall", false)
	require.NoError(t, err)
	removed, err := leases[0].Uninstall()
	ReleaseAll(leases)
	require.NoError(t, err)
	assert.False(t, removed)
	_, statErr := os.Stat(parent)
	assert.True(t, os.IsNotExist(statErr), "absent uninstall must not create harness dirs")
}

// The absence race is closed by the shared lock domain, not rechecking: a
// held absent-target uninstall lease blocks a concurrent install of the
// same target outright.
func TestUninstallAbsentLeaseBlocksConcurrentInstall(t *testing.T) {
	SetSource(testSource(t))
	tg := targetIn(t)

	leases, err := Acquire(testLockDir(tg), []Target{tg}, "uninstall", false)
	require.NoError(t, err)

	// a concurrent install in the same lock domain must fail busy
	_, err = Acquire(testLockDir(tg), []Target{tg}, "install", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "another skill operation")

	removed, err := leases[0].Uninstall()
	ReleaseAll(leases)
	require.NoError(t, err)
	assert.False(t, removed)
}

// The lock key is canonical: a path reached through a symlinked parent
// and its physical path key the SAME lock file, and a lease held on one
// blocks acquisition through the other. (Config-dir independence is
// structural: the production domain is DefaultLockDir — os.UserCacheDir —
// which RANCHER_CLI_CONFIG_DIR never influences.)
func TestLockKeyCanonicalAcrossSymlinkedParents(t *testing.T) {
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
	assert.Contains(t, err.Error(), "another skill operation")
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
	assert.Contains(t, err.Error(), "another skill operation")
	_, statErr := os.Stat(a.Dir)
	assert.True(t, os.IsNotExist(statErr), "first target must be untouched")
}

// TestLockFilePathCaseFold pins the case-folding contract: on case-insensitive
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
