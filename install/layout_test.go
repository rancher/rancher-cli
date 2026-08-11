package install_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpgradeSwitchesCurrentAtomically(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	_, _, code := runInstall(t, s, envHome(home))
	require.Equal(t, 0, code)

	writeRelease(s, "v1.3.0", false)
	s.setLatest("v1.3.0")
	out, errOut, code := runInstall(t, s, envHome(home))
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "rancher version v1.3.0")

	target, err := os.Readlink(filepath.Join(home, ".local", "share", "rancher-cli", "current"))
	require.NoError(t, err)
	assert.Equal(t, "versions/v1.3.0", target)
	_, err = os.Stat(filepath.Join(home, ".local", "share", "rancher-cli", "versions", "v1.2.3"))
	assert.True(t, os.IsNotExist(err), "superseded version dir must be GC'd by the next run")
}

func TestSameTagReinstallIsIdempotent(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	_, _, code := runInstall(t, s, envHome(home))
	require.Equal(t, 0, code)
	out, errOut, code := runInstall(t, s, envHome(home))
	require.Equalf(t, 0, code, "same-tag reinstall must succeed, stderr: %s", errOut)
	assert.Contains(t, out, "rancher version v1.2.3")
	// Idempotent means REUSED, not re-staged: a version_dir_intact that always
	// answered false would still serve v1.2.3 (from a fresh versions/v1.2.3.2)
	// and satisfy every assertion above.
	assert.Contains(t, out, "already present and intact",
		"the intact same-tag dir must be reused, not re-staged")
	entries, err := os.ReadDir(filepath.Join(home, ".local", "share", "rancher-cli", "versions"))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"v1.2.3"}, names, "reinstall must not accumulate a suffixed dir")
}

func TestMigrationTakesOverRegularFilesWithNotice(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "rancher"), []byte("old"), 0o755))
	// partial pair: only one prior file
	out, errOut, code := runInstall(t, s, envHome(home))
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "notice: replacing existing file")
	fi, err := os.Lstat(filepath.Join(bin, "rancher"))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink)
}

func TestMigrationRefusesDirectory(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	require.NoError(t, os.MkdirAll(filepath.Join(bin, "rancher"), 0o755))
	_, errOut, code := runInstall(t, s, envHome(home))
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "refusing to replace")
}

func TestForeignSymlinkReplacedWithNotice(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.Symlink("/usr/bin/true", filepath.Join(bin, "rancher")))
	out, _, code := runInstall(t, s, envHome(home))
	require.Equal(t, 0, code)
	assert.Contains(t, out, "notice: replacing existing symlink")
}

// startHeld launches an install blocked at the named seam and returns the
// running cmd once the seam is reached (detected via the probe func). The env
// mirrors runInstall's: hermetic PATH, controlled TMPDIR, stub CA bundle.
//
// The child's output goes to a FILE, deliberately not an os/exec pipe: SIGKILL
// leaves hold_at's `sleep` child orphaned holding the write end, so a pipe would
// make the caller's cmd.Wait block until that sleep expires. On a probe timeout
// the captured output is logged — otherwise a seam that errored out early is
// indistinguishable from one that is merely slow.
func startHeld(t *testing.T, s *stub, home, seamEnv, holdFile string, reached func() bool) *exec.Cmd {
	t.Helper()
	require.NoError(t, os.WriteFile(holdFile, nil, 0o600))
	script, err := filepath.Abs("../install.sh")
	require.NoError(t, err)
	logPath := filepath.Join(t.TempDir(), "held-install.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = logFile.Close() })
	cmd := exec.Command("sh", script)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = []string{"HOME=" + home, "PATH=" + toolPath(t, baseTools...),
		"TMPDIR=" + t.TempDir(),
		"RANCHER_CLI_INSTALL_BASE_URL=" + s.srv.URL,
		"CURL_CA_BUNDLE=" + s.caPath,
		seamEnv + "=" + holdFile}
	require.NoError(t, cmd.Start())
	// A probe that never comes true must not leak a live installer into a
	// HOME the test framework is about to delete under it.
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	// assert, not require: require's FailNow would skip the log below.
	if !assert.Eventually(t, reached, 30*time.Second, 20*time.Millisecond) {
		raw, _ := os.ReadFile(logPath)
		t.Logf("held installer never reached %s; its output was:\n%s", seamEnv, raw)
		t.FailNow()
	}
	return cmd
}

// stagingReached reports whether an installer has created its staging dir.
func stagingReached(versions string) func() bool {
	return func() bool {
		entries, _ := os.ReadDir(versions)
		for _, e := range entries {
			if strings.Contains(e.Name(), ".staging.") {
				return true
			}
		}
		return false
	}
}

func TestSigkillDuringStagingLeavesOldPairAndRerunCompletes(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	_, _, code := runInstall(t, s, envHome(home))
	require.Equal(t, 0, code)

	writeRelease(s, "v1.3.0", false)
	s.setLatest("v1.3.0")
	versions := filepath.Join(home, ".local", "share", "rancher-cli", "versions")
	hold := filepath.Join(t.TempDir(), "hold")
	// Wait for staging to appear, then SIGKILL: untrappable termination.
	cmd := startHeld(t, s, home, "RANCHER_CLI_INSTALL_HOLD_AFTER_STAGING", hold,
		stagingReached(versions))
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()

	// Old pair still serves.
	current, err := os.Readlink(filepath.Join(home, ".local", "share", "rancher-cli", "current"))
	require.NoError(t, err)
	assert.Equal(t, "versions/v1.2.3", current)

	// Rerun collects the garbage (incl. the stale lock is NOT held — the
	// kill left it; the error message names it, and removing it is safe by
	// construction) and completes.
	require.NoError(t, os.RemoveAll(filepath.Join(home, ".local", "share", "rancher-cli", ".install.lock")))
	out, errOut, code := runInstall(t, s, envHome(home))
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "rancher version v1.3.0")
	// Paired with the loop below, which alone would pass on an empty dir.
	require.DirExists(t, filepath.Join(versions, "v1.3.0"))
	entries, err := os.ReadDir(versions)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".staging.", "rerun must GC staging litter")
	}
}

func TestSigkillAfterSwitchServesNewPair(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	_, _, code := runInstall(t, s, envHome(home))
	require.Equal(t, 0, code)

	writeRelease(s, "v1.3.0", false)
	s.setLatest("v1.3.0")
	data := filepath.Join(home, ".local", "share", "rancher-cli")
	hold := filepath.Join(t.TempDir(), "hold")
	cmd := startHeld(t, s, home, "RANCHER_CLI_INSTALL_HOLD_AFTER_SWITCH", hold, func() bool {
		target, err := os.Readlink(filepath.Join(data, "current"))
		return err == nil && target == "versions/v1.3.0"
	})
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()

	// The switch happened: the new pair is fully served through the links
	// even though the epilogue, GC, and receipt never ran. BOTH links, not
	// just the CLI — "pair" is the unit the switch is atomic over.
	for _, name := range []string{"rancher", "rancher-credential-vault"} {
		out, err := exec.Command(filepath.Join(home, ".local", "bin", name), "--version").Output()
		require.NoErrorf(t, err, "%s must execute through its link", name)
		assert.Containsf(t, string(out), name+" version v1.3.0",
			"%s must resolve to the new version", name)
	}
}

func TestSigkillMidFreshInstallRerunRepairs(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	hold := filepath.Join(t.TempDir(), "hold")
	cmd := startHeld(t, s, home, "RANCHER_CLI_INSTALL_HOLD_AFTER_FIRST_LINK", hold, func() bool {
		_, err := os.Lstat(filepath.Join(bin, "rancher"))
		return err == nil
	})
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()

	// Half a fresh install: first link exists, second does not.
	_, err := os.Lstat(filepath.Join(bin, "rancher-credential-vault"))
	require.True(t, os.IsNotExist(err))

	require.NoError(t, os.RemoveAll(filepath.Join(home, ".local", "share", "rancher-cli", ".install.lock")))
	_, errOut, code := runInstall(t, s, envHome(home))
	require.Equalf(t, 0, code, "rerun must repair, stderr: %s", errOut)
	for _, name := range []string{"rancher", "rancher-credential-vault"} {
		_, err := os.Stat(filepath.Join(bin, name))
		assert.NoErrorf(t, err, "%s must resolve after the repair rerun", name)
	}
}

func TestCorruptVersionDirIsReplaced(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	_, _, code := runInstall(t, s, envHome(home))
	require.Equal(t, 0, code)

	vdir := filepath.Join(home, ".local", "share", "rancher-cli", "versions", "v1.2.3")
	require.NoError(t, os.WriteFile(filepath.Join(vdir, "rancher"), []byte("#!/bin/sh\necho corrupted\n"), 0o755))

	out, errOut, code := runInstall(t, s, envHome(home))
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "rancher version v1.2.3", "reinstall must serve the pristine binary")
	target, err := os.Readlink(filepath.Join(home, ".local", "share", "rancher-cli", "current"))
	require.NoError(t, err)
	assert.Equal(t, "versions/v1.2.3.2", target, "corrupt dir must be replaced via the suffixed name")
}

func TestMigrationRefusalOnSecondPathMutatesNothing(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	require.NoError(t, os.MkdirAll(filepath.Join(bin, "rancher-credential-vault"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "rancher"), []byte("old"), 0o755))
	_, errOut, code := runInstall(t, s, envHome(home))
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "refusing to replace")
	raw, err := os.ReadFile(filepath.Join(bin, "rancher"))
	require.NoError(t, err)
	assert.Equal(t, "old", string(raw), "preflight must refuse BEFORE replacing the first path")
}

// A refusal during an UPGRADE must leave the whole managed installation
// untouched — the preflight runs before staging, switch, and GC.
func TestRefusalOnManagedInstallLeavesLayoutUntouched(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	_, _, code := runInstall(t, s, envHome(home))
	require.Equal(t, 0, code)
	// Sabotage one public path into a directory, then attempt an upgrade.
	helper := filepath.Join(home, ".local", "bin", "rancher-credential-vault")
	require.NoError(t, os.Remove(helper))
	require.NoError(t, os.MkdirAll(helper, 0o755))
	writeRelease(s, "v1.3.0", false)
	s.setLatest("v1.3.0")
	before := digestTree(t, filepath.Join(home, ".local", "share", "rancher-cli"))
	_, errOut, code := runInstall(t, s, envHome(home))
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "refusing to replace")
	assert.Equal(t, before, digestTree(t, filepath.Join(home, ".local", "share", "rancher-cli")),
		"current, version dirs, and staging must be untouched by a refused upgrade")
}

func TestMigrationTakesOverFullLegacyPair(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "rancher"), []byte("old-cli"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "rancher-credential-vault"), []byte("old-helper"), 0o755))
	out, errOut, code := runInstall(t, s, envHome(home))
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Equal(t, 2, strings.Count(out, "notice: replacing existing file"),
		"both legacy files must be announced")
	for _, name := range []string{"rancher", "rancher-credential-vault"} {
		fi, err := os.Lstat(filepath.Join(bin, name))
		require.NoError(t, err)
		assert.NotZerof(t, fi.Mode()&os.ModeSymlink, "%s must be a managed symlink after takeover", name)
	}
	assert.Contains(t, out, "rancher version v1.2.3")
}

// Real concurrency, not a hand-placed lock dir: a live installer parked mid-
// staging holds the lock, and the second one must lose and mutate nothing.
func TestConcurrentInstallErrorsOnHeldLock(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	versions := filepath.Join(home, ".local", "share", "rancher-cli", "versions")
	hold := filepath.Join(t.TempDir(), "hold")
	// The winner acquires the lock before it stages, so a reached staging dir
	// proves the lock is held. It stays held until the deferred kill.
	_ = startHeld(t, s, home, "RANCHER_CLI_INSTALL_HOLD_AFTER_STAGING", hold,
		stagingReached(versions))
	lock := filepath.Join(home, ".local", "share", "rancher-cli", ".install.lock")
	require.DirExists(t, lock, "the parked installer must be holding the lock")

	// Digest BIN_DIR, not the data dir: the winner's staging dir is live and
	// its name carries the winner's pid, so the data dir is not stable to
	// compare. BIN_DIR is what the loser must not touch.
	before := digestTree(t, bin)
	_, errOut, code := runInstall(t, s, envHome(home))
	require.NotEqual(t, 0, code, "the second installer must lose")
	assert.Contains(t, errOut, "another install is in progress")
	assert.Contains(t, errOut, lock, "the error must name the lock to remove")
	assert.Equal(t, before, digestTree(t, bin),
		"the loser must publish no links — it failed before any layout mutation")
	// The loser's cleanup trap must not remove a lock it never acquired.
	assert.DirExists(t, lock, "the loser must not release the winner's lock")
}

func TestStaleLockDirectoryErrors(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	lock := filepath.Join(home, ".local", "share", "rancher-cli", ".install.lock")
	require.NoError(t, os.MkdirAll(lock, 0o755))
	_, errOut, code := runInstall(t, s, envHome(home))
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "another install is in progress")
}

func TestCapableGhAttestationRuns(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	// Fake gh: capable probe answers + verification succeeds, recording calls.
	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls")
	fake := "#!/bin/sh\n" +
		"echo \"$@\" >> " + callLog + "\n" +
		"case \"$1 $2\" in\n" +
		"  'auth status') exit 0 ;;\n" +
		"  'attestation verify') if [ \"$3\" = --help ] || [ \"$3\" = '' ]; then echo '--signer-workflow'; fi; exit 0 ;;\n" +
		"esac\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gh"), []byte(fake), 0o755))
	home := t.TempDir()
	out, errOut, code := runInstall(t, s, append(envHome(home),
		"PATH="+dir+":"+toolPath(t, baseTools...)), "--verify-attestation")
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "build provenance verified")
	logBytes, err := os.ReadFile(callLog)
	require.NoError(t, err)
	// Twice, once per binary: a single count would pass if the helper's
	// verification were dropped and only the CLI checked.
	assert.Equal(t, 2, strings.Count(string(logBytes),
		"--signer-workflow rancher/rancher-cli/.github/workflows/release.yaml"),
		"both binaries must be verified against the pinned signer workflow")
	// And they are the two DIFFERENT binaries, not the CLI twice. The logged
	// paths are absolute (the download work dir), so match on the asset stem
	// rather than the whole argument.
	var verified []string
	for _, line := range strings.Split(string(logBytes), "\n") {
		// The capability probe is also an "attestation verify" line
		// ("--help"); only the real calls carry the pinned workflow.
		if strings.HasPrefix(line, "attestation verify ") && strings.Contains(line, "--signer-workflow ") {
			verified = append(verified, line)
		}
	}
	require.Len(t, verified, 2, "verify calls: %q", string(logBytes))
	helperFirst := strings.Contains(verified[0], "/rancher-credential-vault-")
	assert.NotEqual(t, helperFirst, strings.Contains(verified[1], "/rancher-credential-vault-"),
		"exactly one verify call must name the helper: %q", verified)
}
