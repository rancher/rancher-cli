package install_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// windowsInstallerPlatforms is every release platform install.ps1 must
// serve, derived from scripts/release-build's loop like installerPlatforms so
// the stub's Windows assets cannot drift from what a release publishes.
var windowsInstallerPlatforms = deriveWindowsInstallerPlatforms()

func deriveWindowsInstallerPlatforms() []string {
	raw, err := os.ReadFile(filepath.Join("..", "scripts", "release-build"))
	if err != nil {
		panic(err)
	}
	m := platformLoopRe.FindStringSubmatch(string(raw))
	if m == nil {
		panic("scripts/release-build: no `for platform in ...; do` build loop found - it moved or was reworded")
	}
	var out []string
	for p := range strings.FieldsSeq(m[1]) {
		if strings.HasPrefix(p, "windows/") {
			out = append(out, strings.Replace(p, "/", "-", 1))
		}
	}
	return out
}

// buildTestProgram compiles one of the testdata helper programs for Windows
// and returns the executable's bytes; callers write them wherever a test
// needs the program.
func buildTestProgram(pkg string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "rancher-install-test-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	out := filepath.Join(dir, pkg+".exe")
	cmd := exec.Command("go", "build", "-o", out, "./testdata/"+pkg)
	cmd.Env = append(os.Environ(), "GOOS=windows", "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("go build ./testdata/%s: %w\n%s", pkg, err, b)
	}
	return os.ReadFile(out)
}

var (
	fakeBinExe = sync.OnceValues(func() ([]byte, error) { return buildTestProgram("fakebin") })
	ghStubExe  = sync.OnceValues(func() ([]byte, error) { return buildTestProgram("ghstub") })
)

// fakeTagMarker is the separator the fakebin program looks for after its own
// code; the tag follows it.
const fakeTagMarker = "\nRANCHER-FAKE-TAG:"

// writeWindowsRelease registers a release whose Windows binaries are real
// executables printing "<name> version <tag>", so the installer's epilogue
// runs them exactly as it would a released binary.
func writeWindowsRelease(t *testing.T, s *stub, tag string) {
	t.Helper()
	exe, err := fakeBinExe()
	require.NoError(t, err)
	var assets []stubAsset
	for _, name := range []string{"rancher", "rancher-credential-vault"} {
		for _, plat := range windowsInstallerPlatforms {
			body := make([]byte, 0, len(exe)+len(fakeTagMarker)+len(tag)+1)
			body = append(body, exe...)
			body = append(body, fakeTagMarker...)
			body = append(body, tag...)
			body = append(body, '\n')
			assets = append(assets, stubAsset{name: name + "-" + plat + ".exe", body: body})
		}
	}
	registerRelease(s, tag, false, assets)
}

// powerShell is one PowerShell host the tests can drive.
type powerShell struct {
	name string // "pwsh" or "powershell"
	path string
}

// availablePowerShells lists the hosts on this machine, pwsh first.
func availablePowerShells() []powerShell {
	var out []powerShell
	for _, name := range []string{"pwsh", "powershell"} {
		if p, err := exec.LookPath(name); err == nil {
			out = append(out, powerShell{name: name, path: p})
		}
	}
	return out
}

// requirePowerShell skips unless the host is Windows with at least one
// PowerShell: install.ps1 relies on NTFS junctions and the Windows profile
// directories, so it has nothing to prove elsewhere. Returns the preferred
// host (pwsh when present).
func requirePowerShell(t *testing.T) powerShell {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 targets Windows; skipping on " + runtime.GOOS)
	}
	shells := availablePowerShells()
	if len(shells) == 0 {
		t.Skip("neither pwsh nor powershell found on PATH")
	}
	return shells[0]
}

// powerShells is every available host, for tests that run once per host
// so behavior is proven on Windows PowerShell 5.1 and PowerShell 7 alike.
// Skips like requirePowerShell when there is none.
func powerShells(t *testing.T) []powerShell {
	t.Helper()
	requirePowerShell(t)
	return availablePowerShells()
}

// winHome is a hermetic Windows profile: the three directories install.ps1
// writes under, each a fresh temp dir.
type winHome struct {
	localAppData, appData, temp string
}

func newWinHome(t *testing.T) winHome {
	t.Helper()
	return winHome{localAppData: t.TempDir(), appData: t.TempDir(), temp: t.TempDir()}
}

func (h winHome) dataDir() string     { return filepath.Join(h.localAppData, "rancher-cli") }
func (h winHome) binDir() string      { return filepath.Join(h.dataDir(), "bin") }
func (h winHome) versionsDir() string { return filepath.Join(h.dataDir(), "versions") }
func (h winHome) lockDir() string     { return filepath.Join(h.dataDir(), ".install.lock") }
func (h winHome) receiptPath() string {
	return filepath.Join(h.appData, "rancher-cli", "install-receipt.json")
}

// scrubbedEnviron is the host environment minus every variable install.ps1
// reads, so a developer's token or overrides can never leak into a test,
// and minus PSModulePath: a value inherited from one PowerShell host breaks
// cmdlet autoloading in the other, and each host computes its own default.
func scrubbedEnviron() []string {
	var out []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(key)
		if upper == "GH_TOKEN" || upper == "GITHUB_TOKEN" || upper == "PSMODULEPATH" ||
			strings.HasPrefix(upper, "RANCHER_CLI_") || strings.HasPrefix(upper, "RANCHER_TEST_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// hermeticPath is a PATH holding only the Windows system directories plus
// the given tool dirs: no real gh, no stray rancher.
func hermeticPath(toolDirs ...string) string {
	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	parts := append([]string{}, toolDirs...)
	parts = append(parts, filepath.Join(sysRoot, "System32"), sysRoot)
	return strings.Join(parts, ";")
}

// psEnv assembles the child environment for one install.ps1 run.
func psEnv(s *stub, home winHome, extra ...string) []string {
	env := scrubbedEnviron()
	env = append(env,
		"LOCALAPPDATA="+home.localAppData,
		"APPDATA="+home.appData,
		"TEMP="+home.temp,
		"TMP="+home.temp,
		"PATH="+hermeticPath(),
		"RANCHER_CLI_INSTALL_BASE_URL="+s.srv.URL,
		"RANCHER_CLI_INSTALL_TEST_PIN_CERT="+s.caPath,
		// No pause before the one retry of a failed download.
		"RANCHER_CLI_INSTALL_TEST_RETRY_DELAY=0",
	)
	return append(env, extra...)
}

// psArgs is the host invocation for running install.ps1 from a file.
func psArgs(sh powerShell, script string, args ...string) []string {
	out := []string{"-NoProfile", "-NonInteractive"}
	if sh.name == "powershell" {
		out = append(out, "-ExecutionPolicy", "Bypass")
	}
	out = append(out, "-File", script)
	return append(out, args...)
}

// runInstallPs executes the real install.ps1 under the given host with a
// hermetic profile and PATH, pointed at the stub.
func runInstallPs(t *testing.T, s *stub, sh powerShell, home winHome, extraEnv []string, args ...string) (string, string, int) {
	t.Helper()
	script, err := filepath.Abs("../install.ps1")
	require.NoError(t, err)
	cmd := exec.Command(sh.path, psArgs(sh, script, args...)...)
	cmd.Env = psEnv(s, home, extraEnv...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	code := 0
	if exit, ok := runErr.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if runErr != nil {
		t.Fatalf("running install.ps1: %v", runErr)
	}
	return out.String(), errb.String(), code
}

// ghStubDir writes the gh stand-in as gh.exe into a fresh dir and returns
// the dir and the env that selects its behavior and argv log.
func ghStubDir(t *testing.T, mode string) (dir, argvLog string, env []string) {
	t.Helper()
	exe, err := ghStubExe()
	require.NoError(t, err)
	dir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gh.exe"), exe, 0o755))
	argvLog = filepath.Join(dir, "gh.log")
	env = []string{"RANCHER_TEST_GH_MODE=" + mode, "RANCHER_TEST_GH_ARGV_LOG=" + argvLog}
	return
}

// canonicalPath returns the filesystem's own final name for an existing
// path (resolving reparse points and 8.3 short forms - runner TEMP dirs
// arrive short); a path that cannot be resolved is cleaned as given.
func canonicalPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// junctionTarget reports where the bin junction points, normalized for a
// case-insensitive comparison against a versions dir path.
func junctionTarget(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	require.NoError(t, err, "%s must be a junction", path)
	target = strings.TrimPrefix(target, `\\?\`)
	return canonicalPath(target)
}

func assertJunctionTo(t *testing.T, link, want string) {
	t.Helper()
	got := junctionTarget(t, link)
	assert.Truef(t, strings.EqualFold(got, canonicalPath(want)), "junction %s points at %s, want %s", link, got, want)
}

// readReceipt decodes the install receipt.
func readReceipt(t *testing.T, home winHome) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(home.receiptPath())
	require.NoError(t, err, "install must write a receipt")
	var receipt map[string]string
	require.NoError(t, json.Unmarshal(raw, &receipt), "receipt must be valid JSON")
	return receipt
}

// digestInstallTree captures everything install.ps1 could have written in a
// profile: the two rancher-cli directories and any work dir left in TEMP.
// A missing directory digests as empty.
func digestInstallTree(t *testing.T, home winHome) string {
	t.Helper()
	var b strings.Builder
	for _, root := range []string{home.dataDir(), filepath.Join(home.appData, "rancher-cli")} {
		if _, err := os.Lstat(root); err == nil {
			b.WriteString(digestTree(t, root))
		}
	}
	entries, err := os.ReadDir(home.temp)
	require.NoError(t, err)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "rancher-cli-install.") {
			b.WriteString("temp:" + e.Name() + "\n")
		}
	}
	return b.String()
}

// startHeldPs launches an install blocked at the named seam and returns the
// running cmd once the seam is reached. Output goes to a file so a killed
// host cannot leave a pipe open.
func startHeldPs(t *testing.T, s *stub, sh powerShell, home winHome, seamEnv, holdFile string, reached func() bool) *exec.Cmd {
	t.Helper()
	cmd, _ := startHeldPsLogged(t, s, sh, home, seamEnv, holdFile, reached)
	return cmd
}

// startHeldPsLogged is startHeldPs for tests that release the hold and let
// the installer finish: it also returns the path of the installer's
// combined output.
func startHeldPsLogged(t *testing.T, s *stub, sh powerShell, home winHome, seamEnv, holdFile string, reached func() bool) (*exec.Cmd, string) {
	t.Helper()
	require.NoError(t, os.WriteFile(holdFile, nil, 0o600))
	script, err := filepath.Abs("../install.ps1")
	require.NoError(t, err)
	logPath := filepath.Join(t.TempDir(), "held-install.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = logFile.Close() })
	cmd := exec.Command(sh.path, psArgs(sh, script)...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = psEnv(s, home, seamEnv+"="+holdFile)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if !assert.Eventually(t, reached, 60*time.Second, 50*time.Millisecond) {
		raw, _ := os.ReadFile(logPath)
		t.Logf("held installer never reached %s; its output was:\n%s", seamEnv, raw)
		t.FailNow()
	}
	return cmd, logPath
}

// stagingReachedIn reports whether an installer has created its staging dir
// under the versions dir.
func stagingReachedIn(versions string) func() bool {
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

// makeJunction creates an NTFS junction, which needs no privilege or
// developer mode, unlike the directory symbolic link os.Symlink would make.
func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	require.NoErrorf(t, err, "mklink /J %s %s: %s", link, target, out)
}

// seedByHandPair writes a prior by-hand install into a plain bin dir: the
// two expected names with recognizable stand-in contents.
func seedByHandPair(t *testing.T, binDir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "rancher.exe"), []byte("old-cli"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "rancher-credential-vault.exe"), []byte("old-helper"), 0o755))
}

// asidesBeside lists the <bin>.old.* directories an interrupted by-hand
// takeover leaves beside the bin dir.
func asidesBeside(t *testing.T, binDir string) []string {
	t.Helper()
	return siblingsMatching(t, binDir, filepath.Base(binDir)+".old.")
}

// stagingLinksBeside lists the .<bin>.<pid> staging junctions an interrupted
// switch leaves beside the bin dir.
func stagingLinksBeside(t *testing.T, binDir string) []string {
	t.Helper()
	return siblingsMatching(t, binDir, "."+filepath.Base(binDir)+".")
}

func siblingsMatching(t *testing.T, binDir, prefix string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(binDir))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, filepath.Join(filepath.Dir(binDir), e.Name()))
		}
	}
	return out
}

// runExe runs an installed binary and returns its combined output.
func runExe(t *testing.T, path string, args ...string) string {
	t.Helper()
	out, err := exec.Command(path, args...).CombinedOutput()
	require.NoErrorf(t, err, "%s must execute: %s", path, out)
	return string(out)
}
