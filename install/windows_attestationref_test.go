package install_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// winAttestRun describes one install.ps1 run against the gh stand-in.
type winAttestRun struct {
	tag      string // the release the stub serves
	attested string // the ref the asset's attestation was made for
	mode     string // the gh stand-in mode: capable or older
	latest   bool   // the stub's latest release is tag
	env      []string
	args     []string
}

// winAttestResult is what one run produced.
type winAttestResult struct {
	out, errOut string
	code        int
	calls       []string
	argLists    []string // one line per gh call, every argument in brackets
	home        winHome
}

func runWindowsAttesting(t *testing.T, sh powerShell, r winAttestRun) winAttestResult {
	t.Helper()
	s := newStub(t)
	writeWindowsRelease(t, s, r.tag)
	if r.latest {
		s.setLatest(r.tag)
	}
	dir, ghLog, ghEnv := ghStubDir(t, r.mode)
	res := winAttestResult{home: newWinHome(t)}
	env := slices.Concat(ghEnv, []string{
		"PATH=" + hermeticPath(dir),
		"RANCHER_TEST_GH_ATTESTED_REF=" + r.attested,
	}, r.env)
	argsLog := filepath.Join(t.TempDir(), "gh-args.log")
	env = append(env, "RANCHER_TEST_GH_ARGS_LOG="+argsLog)
	res.out, res.errOut, res.code = runInstallPs(t, s, sh, res.home, env, r.args...)
	if raw, err := os.ReadFile(argsLog); err == nil {
		for line := range strings.SplitSeq(string(raw), "\n") {
			if line = strings.TrimSuffix(line, "\r"); line != "" {
				res.argLists = append(res.argLists, line)
			}
		}
	}
	if raw, err := os.ReadFile(ghLog); err == nil {
		for line := range strings.SplitSeq(string(raw), "\n") {
			if line = strings.TrimSuffix(line, "\r"); line != "" {
				res.calls = append(res.calls, line)
			}
		}
	}
	return res
}

// windowsVerifyCalls returns the logged calls that verify a file; the
// capability probe (--help) is not one.
func windowsVerifyCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "attestation verify ") && c != "attestation verify --help" {
			out = append(out, c)
		}
	}
	return out
}

// windowsBracketedVerifyCalls returns the argument lists of the calls that
// verify a file, as "[file]" and the bracketed arguments after it.
func windowsBracketedVerifyCalls(lines []string) (files, rests []string) {
	const head = "[attestation][verify]["
	for _, l := range lines {
		after, ok := strings.CutPrefix(l, head)
		if !ok || strings.HasPrefix(after, "--help]") {
			continue
		}
		file, rest, _ := strings.Cut(after, "]")
		files = append(files, file)
		rests = append(rests, rest)
	}
	return files, rests
}

func assertWindowsNothingInstalled(t *testing.T, home winHome) {
	t.Helper()
	for _, p := range []string{home.binDir(), home.versionsDir(), home.receiptPath()} {
		_, err := os.Lstat(p)
		assert.Truef(t, os.IsNotExist(err), "%s must not exist: %v", p, err)
	}
}

// TestWindowsAttestationVerifyNamesTheInstalledTag asserts, on each PowerShell
// host, that whichever way the tag is chosen (the latest release, -Version,
// RANCHER_CLI_VERSION, -Pre), install.ps1 verifies each of the two binaries
// with exactly the repository, the release.yaml identity for refs/tags/<tag>
// and that ref as the source ref, as the gh stand-in reads them from the
// command line, and that an attestation made for that ref passes.
func TestWindowsAttestationVerifyNamesTheInstalledTag(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		run  winAttestRun
	}{
		{"latest release", winAttestRun{tag: "v1.2.3", latest: true}},
		{"version flag", winAttestRun{tag: "v1.2.3", args: []string{"-Version", "v1.2.3"}}},
		{"version variable", winAttestRun{tag: "v1.2.3", env: []string{"RANCHER_CLI_VERSION=v1.2.3"}}},
		{"pre-release", winAttestRun{tag: "v1.3.0-rc.1", args: []string{"-Pre"}}},
	}
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			for _, tc := range rows {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					r := tc.run
					r.mode = "capable"
					r.attested = "refs/tags/" + r.tag
					r.args = append(slices.Clone(r.args), "-VerifyAttestation")
					res := runWindowsAttesting(t, sh, r)
					require.Equalf(t, 0, res.code, "stderr: %s", res.errOut)
					assert.Contains(t, res.out+res.errOut, "build provenance attestation verified")
					wantRest := "[--repo][rancher/rancher-cli][--cert-identity]" +
						"[https://github.com/rancher/rancher-cli/.github/workflows/release.yaml@refs/tags/" + r.tag + "]" +
						"[--source-ref][refs/tags/" + r.tag + "]"
					files, rests := windowsBracketedVerifyCalls(res.argLists)
					require.Lenf(t, files, 2, "one verify call per binary: %q", res.argLists)
					var helpers int
					for i, f := range files {
						assert.Equal(t, wantRest, rests[i], "the argument list after the file")
						if strings.Contains(f, "rancher-credential-vault-") {
							helpers++
						}
					}
					assert.Equal(t, 1, helpers, "exactly one call names the helper: %q", files)
				})
			}
		})
	}
}

// TestWindowsAttestationOfAnotherRefFailsTheInstall asserts, on each PowerShell
// host, that an attestation made for another ref (a rehearsal on the main
// branch, another tag, a longer tag, the tag in another case) fails the
// install, with and without -VerifyAttestation, and leaves nothing installed.
func TestWindowsAttestationOfAnotherRefFailsTheInstall(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			for _, attested := range []string{"refs/heads/main", "refs/tags/v1.2.2", "refs/tags/v1.2.3-rc.1", "refs/tags/V1.2.3"} {
				for _, flag := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s with flag %t", attested, flag), func(t *testing.T) {
						t.Parallel()
						r := winAttestRun{tag: "v1.2.3", attested: attested, mode: "capable", args: []string{"-Version", "v1.2.3"}}
						if flag {
							r.args = append(r.args, "-VerifyAttestation")
						}
						res := runWindowsAttesting(t, sh, r)
						require.NotEqual(t, 0, res.code, "stdout: %s", res.out)
						assert.Contains(t, res.errOut, "build provenance attestation failed")
						assert.Contains(t, res.errOut, "refs/tags/v1.2.3")
						assertWindowsNothingInstalled(t, res.home)
					})
				}
			}
		})
	}
}

// TestWindowsGhWithoutSourceRefSkipsAttestation asserts, on each PowerShell
// host, that a gh whose help does not list --source-ref takes the skip path: a
// notice and a normal install, or a stop with the version message under
// -VerifyAttestation. No file is verified in either case.
func TestWindowsGhWithoutSourceRefSkipsAttestation(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			t.Run("without the flag", func(t *testing.T) {
				t.Parallel()
				res := runWindowsAttesting(t, sh, winAttestRun{tag: "v1.2.3", attested: "refs/tags/v1.2.3", mode: "older", latest: true})
				require.Equalf(t, 0, res.code, "stderr: %s", res.errOut)
				assert.Contains(t, res.out+res.errOut, "notice: attestation skipped (gh is not installed, is older than 2.68.0, or is not logged in)")
				assert.Empty(t, windowsVerifyCalls(res.calls), "no file may be verified: %q", res.calls)
			})
			t.Run("with the flag", func(t *testing.T) {
				t.Parallel()
				res := runWindowsAttesting(t, sh, winAttestRun{tag: "v1.2.3", attested: "refs/tags/v1.2.3", mode: "older", latest: true,
					args: []string{"-VerifyAttestation"}})
				require.NotEqual(t, 0, res.code)
				assert.Contains(t, res.errOut, "-VerifyAttestation needs gh 2.68.0 or newer")
				assert.Empty(t, windowsVerifyCalls(res.calls), "no file may be verified: %q", res.calls)
				assertWindowsNothingInstalled(t, res.home)
			})
		})
	}
}
