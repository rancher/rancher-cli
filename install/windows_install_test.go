package install_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rawInstallPsURL = "https://raw.githubusercontent.com/rancher/rancher-cli/main/install.ps1"

// windowsPlat is the asset suffix for the host the tests emulate; every
// Windows test runs as windows/amd64 by setting PROCESSOR_ARCHITECTURE.
const windowsPlat = "windows-amd64"

func TestWindowsCleanInstallProducesPairAndReceipt(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			home := newWinHome(t)
			out, errOut, code := runInstallPs(t, s, sh, home, nil)
			require.Equalf(t, 0, code, "stderr: %s", errOut)

			assertJunctionTo(t, home.binDir(), filepath.Join(home.versionsDir(), "v1.2.3"))
			for _, name := range []string{"rancher", "rancher-credential-vault"} {
				assert.Contains(t, runExe(t, filepath.Join(home.binDir(), name+".exe"), "--version"), name+" version v1.2.3")
			}
			assert.Contains(t, out, "installed: rancher version v1.2.3")
			assert.Contains(t, out, "add to PATH")
			assert.Contains(t, out, home.binDir(), "the PATH hint must name the bin dir")
			assert.NotContains(t, errOut, "warning:", "a clean install must not warn")

			receipt := readReceipt(t, home)
			assert.Equal(t, "v1.2.3", receipt["tag"])
			assert.Equal(t, "1.2.3", receipt["version"])
			assert.Equal(t, "script", receipt["channel"])
			assert.Equal(t, "versioned-dir", receipt["layout"])
			assert.Equal(t, home.binDir(), receipt["binDir"])
			assert.Equal(t, home.dataDir(), receipt["dataDir"])

			_, err := os.Stat(home.lockDir())
			assert.True(t, os.IsNotExist(err), "the lock must be released")
			entries, err := os.ReadDir(home.temp)
			require.NoError(t, err)
			for _, e := range entries {
				assert.NotContains(t, e.Name(), "rancher-cli-install.", "the work dir must be cleaned up")
			}
		})
	}
}

func TestWindowsDryRunResolvesStableLatestAndWritesNothing(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			home := newWinHome(t)
			before := digestInstallTree(t, home)
			out, errOut, code := runInstallPs(t, s, sh, home, nil, "-DryRun")
			require.Equalf(t, 0, code, "stderr: %s", errOut)
			assert.Contains(t, out, "tag: v1.2.3")
			assert.Contains(t, out, "platform: windows/amd64")
			assert.Contains(t, out, "asset: "+s.srv.URL+"/rancher/rancher-cli/releases/download/v1.2.3/SHA256SUMS")
			assert.Contains(t, out, "bin-dir: "+home.binDir())
			assert.Contains(t, out, "data-dir: "+home.dataDir())
			assert.Equal(t, before, digestInstallTree(t, home), "dry-run must not touch the profile or TEMP")
		})
	}
}

func TestWindowsDryRunNoStableRelease(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	_, errOut, code := runInstallPs(t, s, sh, newWinHome(t), nil, "-DryRun")
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "no stable release found")
	assert.NotContains(t, errOut, "GitHub token")
	assert.Contains(t, errOut, "use -Pre or -Version")
}

func TestWindowsLatestHTTPErrorIsNotNoStableRelease(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	for _, status := range []int{401, 500} {
		s := newStub(t)
		s.setLatestStatus(status)
		_, errOut, code := runInstallPs(t, s, sh, newWinHome(t), nil, "-DryRun")
		require.NotEqualf(t, 0, code, "HTTP %d must fail", status)
		assert.Contains(t, errOut, "could not look up the latest stable release (HTTP")
		assert.NotContains(t, errOut, "no stable release", "only GitHub's 404 means release absence")
	}
}

func TestWindowsTagGrammar(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	accept := []string{"v0.1.0", "v10.2.30", "v1.0.0-alpha.1", "v1.0.0-rc.2"}
	reject := []string{"v01.0.0", "v1.0.0-alpha-1", "v1.0.0+meta", "v1/evil", "1.0.0", "v1.0.0-alpha"}
	for _, tag := range accept {
		t.Run(tag, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			out, errOut, code := runInstallPs(t, s, sh, newWinHome(t), nil, "-DryRun", "-Version", tag)
			require.Equalf(t, 0, code, "tag %s must be accepted: %s", tag, errOut)
			assert.Contains(t, out, "tag: "+tag)
		})
	}
	for _, tag := range reject {
		t.Run(tag, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			_, errOut, code := runInstallPs(t, s, sh, newWinHome(t), nil, "-DryRun", "-Version", tag)
			require.NotEqualf(t, 0, code, "tag %s must be rejected", tag)
			assert.Contains(t, errOut, "is not a valid release tag")
		})
	}
}

func TestWindowsVersionAndPreAreExclusive(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	_, errOut, code := runInstallPs(t, s, sh, newWinHome(t), nil, "-DryRun", "-Version", "v1.0.0", "-Pre")
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "mutually exclusive")
}

// TestWindowsEnvTwins asserts the environment twins of the parameters: the
// same names install.sh reads, with the parameter winning over the twin.
func TestWindowsEnvTwins(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	t.Run("version from env", func(t *testing.T) {
		t.Parallel()
		s := newStub(t)
		out, errOut, code := runInstallPs(t, s, sh, newWinHome(t), []string{"RANCHER_CLI_VERSION=v1.2.3"}, "-DryRun")
		require.Equalf(t, 0, code, "stderr: %s", errOut)
		assert.Contains(t, out, "tag: v1.2.3")
	})
	t.Run("parameter beats env", func(t *testing.T) {
		t.Parallel()
		s := newStub(t)
		out, errOut, code := runInstallPs(t, s, sh, newWinHome(t), []string{"RANCHER_CLI_VERSION=v1.2.3"}, "-DryRun", "-Version", "v1.3.0")
		require.Equalf(t, 0, code, "stderr: %s", errOut)
		assert.Contains(t, out, "tag: v1.3.0")
		assert.NotContains(t, out, "v1.2.3")
	})
	t.Run("bin dir from env", func(t *testing.T) {
		t.Parallel()
		s := newStub(t)
		home := newWinHome(t)
		binDir := filepath.Join(home.localAppData, "custom-bin")
		out, errOut, code := runInstallPs(t, s, sh, home, []string{"RANCHER_CLI_INSTALL_BIN_DIR=" + binDir}, "-DryRun", "-Version", "v1.2.3")
		require.Equalf(t, 0, code, "stderr: %s", errOut)
		assert.Contains(t, out, "bin-dir: "+binDir)
	})
}

// TestWindowsPreSkipsDraftsAndExhaustion runs on every host: the two hosts
// hand a JSON array to the script in different shapes, and a multi-entry
// listing must page correctly on both.
func TestWindowsPreSkipsDraftsAndExhaustion(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			t.Run("drafts across pages", func(t *testing.T) {
				t.Parallel()
				s := newStub(t)
				for range 100 {
					writeRelease(s, "v9.9.9-alpha.1", true)
				}
				writeWindowsRelease(t, s, "v0.1.0-alpha.1")
				out, errOut, code := runInstallPs(t, s, sh, newWinHome(t), nil, "-DryRun", "-Pre")
				require.Equalf(t, 0, code, "stderr: %s", errOut)
				assert.Contains(t, out, "tag: v0.1.0-alpha.1")
			})
			t.Run("draft then published on one page", func(t *testing.T) {
				t.Parallel()
				s := newStub(t)
				writeRelease(s, "v9.9.9-alpha.1", true)
				writeWindowsRelease(t, s, "v0.1.0-alpha.1")
				out, errOut, code := runInstallPs(t, s, sh, newWinHome(t), nil, "-DryRun", "-Pre")
				require.Equalf(t, 0, code, "stderr: %s", errOut)
				assert.Contains(t, out, "tag: v0.1.0-alpha.1")
			})
			t.Run("exhausted listing", func(t *testing.T) {
				t.Parallel()
				s := newStub(t)
				writeRelease(s, "v9.9.9-alpha.1", true)
				_, errOut, code := runInstallPs(t, s, sh, newWinHome(t), nil, "-DryRun", "-Pre")
				require.NotEqual(t, 0, code)
				assert.Contains(t, errOut, "no published release found")
			})
		})
	}
}

// TestWindowsPreRejectsNonListingBodies asserts that a 200 whose body is not a
// release listing (a captive portal answering null or prose, a body cut
// mid-stream) fails as a parse failure, not as a tag-grammar error on a
// missing tag_name or an unhandled parser exception.
func TestWindowsPreRejectsNonListingBodies(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			for name, body := range map[string]string{"null": "null", "prose": "<html>sign in</html>", "truncated open": "[", "truncated entry": `[{"tag_name":`} {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					s := newStub(t)
					s.setRawListing([][]byte{[]byte(body)})
					_, errOut, code := runInstallPs(t, s, sh, newWinHome(t), nil, "-DryRun", "-Pre")
					require.NotEqual(t, 0, code)
					assert.Contains(t, errOut, "release listing parse failed")
					assert.Contains(t, errOut, "unexpected response")
					assert.NotContains(t, errOut, "is not a valid release tag")
				})
			}
		})
	}
}

func TestWindowsChecksumMismatchIsFatal(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			s.setAsset("v1.2.3/rancher-"+windowsPlat+".exe", []byte("tampered"))
			home := newWinHome(t)
			_, errOut, code := runInstallPs(t, s, sh, home, nil)
			require.NotEqual(t, 0, code)
			assert.Contains(t, errOut, "checksum verification failed")
			_, err := os.Lstat(home.binDir())
			assert.True(t, os.IsNotExist(err), "nothing may be installed after a checksum failure")
		})
	}
}

// TestWindowsChecksumEntryPerAssetCompleteness asserts that each binary needs
// exactly one SHA256SUMS entry: a missing helper entry and a duplicated CLI
// entry standing in for it are both fatal.
func TestWindowsChecksumEntryPerAssetCompleteness(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	cli, helper := "rancher-"+windowsPlat+".exe", "rancher-credential-vault-"+windowsPlat+".exe"
	drop := func(name string) func(string) string {
		return func(sums string) string {
			var keep []string
			for l := range strings.SplitSeq(sums, "\n") {
				if !strings.HasSuffix(l, "  "+name) {
					keep = append(keep, l)
				}
			}
			return strings.Join(keep, "\n")
		}
	}
	duplicateInsteadOf := func(dup, missing string) func(string) string {
		return func(sums string) string {
			var out []string
			for l := range strings.SplitSeq(sums, "\n") {
				if strings.HasSuffix(l, "  "+missing) {
					continue
				}
				out = append(out, l)
				if strings.HasSuffix(l, "  "+dup) {
					out = append(out, l)
				}
			}
			return strings.Join(out, "\n")
		}
	}
	// The wanted message names the asset the check fails on first: the CLI
	// is checked before the helper, so a dropped CLI entry reports the CLI
	// even when the helper entry is also wrong.
	for name, tc := range map[string]struct {
		mutate func(string) string
		want   string
	}{
		"missing helper entry":                   {drop(helper), "SHA256SUMS must contain exactly one entry for " + helper + " (found 0)"},
		"missing cli entry":                      {drop(cli), "SHA256SUMS must contain exactly one entry for " + cli + " (found 0)"},
		"duplicate cli entry masking the helper": {duplicateInsteadOf(cli, helper), "SHA256SUMS must contain exactly one entry for " + cli + " (found 2)"},
		"duplicate helper entry masking the cli": {duplicateInsteadOf(helper, cli), "SHA256SUMS must contain exactly one entry for " + cli + " (found 0)"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			s.setAsset("v1.2.3/SHA256SUMS", []byte(tc.mutate(string(s.asset("v1.2.3/SHA256SUMS")))))
			_, errOut, code := runInstallPs(t, s, sh, newWinHome(t), nil)
			require.NotEqual(t, 0, code)
			assert.Contains(t, errOut, tc.want)
		})
	}
}

func TestWindowsUpgradeSwitchesJunctionAndCollectsOldVersion(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			home := newWinHome(t)
			_, errOut, code := runInstallPs(t, s, sh, home, nil)
			require.Equalf(t, 0, code, "stderr: %s", errOut)

			writeWindowsRelease(t, s, "v1.3.0")
			s.setLatest("v1.3.0")
			out, errOut, code := runInstallPs(t, s, sh, home, nil)
			require.Equalf(t, 0, code, "stderr: %s", errOut)
			assert.Contains(t, out, "upgrading from v1.2.3 to v1.3.0")
			assert.Contains(t, out, "installed: rancher version v1.3.0")
			assertJunctionTo(t, home.binDir(), filepath.Join(home.versionsDir(), "v1.3.0"))
			for _, name := range []string{"rancher", "rancher-credential-vault"} {
				assert.Contains(t, runExe(t, filepath.Join(home.binDir(), name+".exe"), "--version"), name+" version v1.3.0")
			}
			_, err := os.Stat(filepath.Join(home.versionsDir(), "v1.2.3"))
			assert.True(t, os.IsNotExist(err), "the superseded version dir must be collected")
			assert.Equal(t, "v1.3.0", readReceipt(t, home)["tag"])
		})
	}
}

// TestWindowsDowngradeIsNamedAsWarning asserts that rolling back with -Version
// works and is announced on stderr, while a same-version rerun says neither.
func TestWindowsDowngradeIsNamedAsWarning(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	writeWindowsRelease(t, s, "v1.2.3")
	writeWindowsRelease(t, s, "v1.3.0")
	home := newWinHome(t)
	_, errOut, code := runInstallPs(t, s, sh, home, nil, "-Version", "v1.3.0")
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	_, errOut, code = runInstallPs(t, s, sh, home, nil, "-Version", "v1.2.3")
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, errOut, "warning: downgrading from v1.3.0 to v1.2.3")
	assertJunctionTo(t, home.binDir(), filepath.Join(home.versionsDir(), "v1.2.3"))

	out, errOut, code := runInstallPs(t, s, sh, home, nil, "-Version", "v1.2.3")
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "already present and intact")
	assert.NotContains(t, out+errOut, "upgrading")
	assert.NotContains(t, out+errOut, "downgrading")
	entries, err := os.ReadDir(home.versionsDir())
	require.NoError(t, err)
	require.Len(t, entries, 1, "a same-tag rerun must not accumulate version dirs")
	assert.Equal(t, "v1.2.3", entries[0].Name())
}

func TestWindowsCorruptVersionDirIsReplaced(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	writeWindowsRelease(t, s, "v1.2.3")
	s.setLatest("v1.2.3")
	home := newWinHome(t)
	_, errOut, code := runInstallPs(t, s, sh, home, nil)
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	require.NoError(t, os.WriteFile(filepath.Join(home.versionsDir(), "v1.2.3", "rancher.exe"), []byte("corrupted"), 0o755))
	out, errOut, code := runInstallPs(t, s, sh, home, nil)
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "installed: rancher version v1.2.3")
	assertJunctionTo(t, home.binDir(), filepath.Join(home.versionsDir(), "v1.2.3.2"))
}

// TestWindowsTokenModeInstallsThroughTheAssetEndpoint asserts that with a
// token set, against a server that serves release assets only through the API
// asset endpoint, install.ps1 downloads through the endpoint and sends the
// token to the API host only.
func TestWindowsTokenModeInstallsThroughTheAssetEndpoint(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			s.setPrivate(true)
			s.setAssetRedirect(true)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			home := newWinHome(t)
			out, errOut, code := runInstallPs(t, s, sh, home, []string{"GH_TOKEN=test-internal-token"})
			require.Equalf(t, 0, code, "stderr: %s", errOut)
			assert.Contains(t, out, "installed: rancher version v1.2.3")
			assert.Equal(t, "Bearer test-internal-token", s.lastAuthHeader(), "the API asset endpoint must receive the token")
			assert.Empty(t, s.lastCDNAuthHeader(), "the token must not be forwarded to the redirect URL")
			assert.Equal(t, 3, s.apiAssetRequestCount(), "one API request per asset")
			assert.Equal(t, 3, s.cdnAssetRequestCount(), "one redirect URL request per asset")
			assert.NotContains(t, out+errOut, "test-internal-token", "the token must never appear in installer output")
			assert.Equal(t, "v1.2.3", readReceipt(t, home)["tag"])
		})
	}
}

// TestWindowsAnonymousDownloadFailsClearlyWhenAssetsAre404 asserts that with
// no token, against a server that answers 404 on the release download URLs,
// install.ps1 fails with a message that names the asset.
func TestWindowsAnonymousDownloadFailsClearlyWhenAssetsAre404(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	s.setPrivate(true)
	writeWindowsRelease(t, s, "v1.2.3")
	s.setLatest("v1.2.3")
	_, errOut, code := runInstallPs(t, s, sh, newWinHome(t), nil)
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "download failed")
	assert.Contains(t, errOut, "rancher-"+windowsPlat+".exe")
	assert.Contains(t, errOut, "HTTP 404 after one retry; tag 'v1.2.3' has no asset with this name",
		"two 404 answers from the release download URL name the tag")
	assert.NotContains(t, errOut, "no such asset")
}

func TestWindowsDryRunWithTokenShowsAPIAssetURLs(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	writeWindowsRelease(t, s, "v1.2.3")
	s.setLatest("v1.2.3")
	out, errOut, code := runInstallPs(t, s, sh, newWinHome(t), []string{"GITHUB_TOKEN=test-token"}, "-DryRun")
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "asset: "+s.srv.URL+"/repos/rancher/rancher-cli/releases/assets/")
	assert.NotContains(t, out, "/releases/download/")
}

func TestWindowsScopedTokenFallsBackToDirectDownload(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	writeWindowsRelease(t, s, "v1.2.3")
	s.setLatest("v1.2.3")
	s.setReleaseTagStatus(403)
	out, errOut, code := runInstallPs(t, s, sh, newWinHome(t), []string{"GH_TOKEN=scoped-token"})
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, errOut, "falling back to the release download URL")
	assert.Contains(t, out, "installed: rancher version v1.2.3")
}

func TestWindowsMissingAssetInReleaseFails(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	s.setPrivate(true)
	writeWindowsRelease(t, s, "v1.2.3")
	s.setLatest("v1.2.3")
	var doc struct {
		TagName string           `json:"tag_name"`
		Draft   bool             `json:"draft"`
		Assets  []map[string]any `json:"assets"`
	}
	require.NoError(t, json.Unmarshal(s.releaseJSONFor("v1.2.3"), &doc))
	var kept []map[string]any
	for _, a := range doc.Assets {
		if a["name"] != "SHA256SUMS" {
			kept = append(kept, a)
		}
	}
	doc.Assets = kept
	modified, err := json.Marshal(doc)
	require.NoError(t, err)
	s.setReleaseJSON("v1.2.3", modified)
	_, errOut, code := runInstallPs(t, s, sh, newWinHome(t), []string{"GH_TOKEN=test-token"})
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "asset not found in release v1.2.3")
	assert.Contains(t, errOut, "SHA256SUMS")
}

// TestWindowsGhTokenFallback is table-driven over the token sources: no gh,
// gh supplying a token, an env token beating gh, and gh failing for the host.
func TestWindowsGhTokenFallback(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	cases := []struct {
		name           string
		ghMode         string // "" = no gh on PATH
		extraEnv       []string
		wantHeader     string
		wantOut        string
		wantNoOut      string
		wantGhContains string // %s = stub hostname
		wantGhAbsent   string
	}{
		{name: "NoGhStaysAnonymous"},
		{
			name:           "GhTokenFallback",
			ghMode:         "token",
			extraEnv:       []string{"RANCHER_TEST_GH_TOKEN=stub-gh-token-123"},
			wantHeader:     "Bearer stub-gh-token-123",
			wantOut:        "using gh auth token",
			wantNoOut:      "stub-gh-token-123",
			wantGhContains: "auth token --hostname %s",
		},
		{
			name:         "EnvTokenBeatsGhFallback",
			ghMode:       "token",
			extraEnv:     []string{"GH_TOKEN=env-token-456", "RANCHER_TEST_GH_TOKEN=stub-gh-token-123"},
			wantHeader:   "Bearer env-token-456",
			wantNoOut:    "using gh auth token",
			wantGhAbsent: "auth token",
		},
		{
			name:           "WrongHostDegrades",
			ghMode:         "fail",
			wantNoOut:      "using gh auth token",
			wantGhContains: "auth token --hostname %s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			u, err := url.Parse(s.srv.URL)
			require.NoError(t, err)

			env := append([]string{}, tc.extraEnv...)
			var ghLog string
			if tc.ghMode != "" {
				dir, log, ghEnv := ghStubDir(t, tc.ghMode)
				ghLog = log
				env = append(env, ghEnv...)
				env = append(env, "PATH="+hermeticPath(dir))
			}
			out, errOut, code := runInstallPs(t, s, sh, newWinHome(t), env)
			require.Equalf(t, 0, code, "stderr: %s", errOut)
			combined := out + errOut
			assert.Equal(t, tc.wantHeader, s.lastAuthHeader())
			if tc.wantOut != "" {
				assert.Contains(t, combined, tc.wantOut)
			}
			if tc.wantNoOut != "" {
				assert.NotContains(t, combined, tc.wantNoOut)
			}
			if tc.ghMode != "" {
				argv, err := os.ReadFile(ghLog)
				require.NoError(t, err, "gh stub must have been invoked")
				if tc.wantGhContains != "" {
					assert.Contains(t, string(argv), strings.Replace(tc.wantGhContains, "%s", u.Hostname(), 1))
				}
				if tc.wantGhAbsent != "" {
					assert.NotContains(t, string(argv), tc.wantGhAbsent)
				}
			}
		})
	}
}

func TestWindowsVerifyAttestationWithoutGhIsFatal(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	writeWindowsRelease(t, s, "v1.2.3")
	s.setLatest("v1.2.3")
	home := newWinHome(t)
	_, errOut, code := runInstallPs(t, s, sh, home, nil, "-VerifyAttestation")
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "-VerifyAttestation needs gh 2.68.0 or newer (its 'gh attestation verify' supports --cert-identity and --source-ref)")
	_, err := os.Lstat(home.binDir())
	assert.True(t, os.IsNotExist(err), "a refused attestation must install nothing")
}

func TestWindowsCapableGhAttestationRuns(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	writeWindowsRelease(t, s, "v1.2.3")
	s.setLatest("v1.2.3")
	dir, ghLog, ghEnv := ghStubDir(t, "capable")
	out, errOut, code := runInstallPs(t, s, sh, newWinHome(t), append(ghEnv, "PATH="+hermeticPath(dir)), "-VerifyAttestation")
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "build provenance attestation verified")
	raw, err := os.ReadFile(ghLog)
	require.NoError(t, err)
	var verified []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.HasPrefix(line, "attestation verify ") && strings.Contains(line, "--cert-identity https://github.com/rancher/rancher-cli/.github/workflows/release.yaml@refs/tags/v1.2.3 --source-ref refs/tags/v1.2.3") {
			verified = append(verified, line)
		}
	}
	require.Len(t, verified, 2, "both binaries must be verified: %q", raw)
	helperFirst := strings.Contains(verified[0], "rancher-credential-vault-")
	assert.NotEqual(t, helperFirst, strings.Contains(verified[1], "rancher-credential-vault-"),
		"exactly one verify call must name the helper: %q", verified)
	for _, line := range verified {
		assert.Contains(t, line, "--repo rancher/rancher-cli")
	}
}

func TestWindowsTakesOverByHandPairWithNotice(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			home := newWinHome(t)
			seedByHandPair(t, home.binDir())
			out, errOut, code := runInstallPs(t, s, sh, home, nil)
			require.Equalf(t, 0, code, "stderr: %s", errOut)
			assert.Equal(t, 2, strings.Count(out, "notice: replacing existing file"), "both by-hand files must be announced")
			assertJunctionTo(t, home.binDir(), filepath.Join(home.versionsDir(), "v1.2.3"))
			assert.Contains(t, runExe(t, filepath.Join(home.binDir(), "rancher.exe"), "--version"), "rancher version v1.2.3")
			assert.Empty(t, asidesBeside(t, home.binDir()), "the moved-aside pair must be removed once the junction is in place")
		})
	}
}

// TestWindowsShadowScanFollowsReparseChains asserts the epilogue's PATH shadow
// warning: a junction elsewhere on PATH that leads back to the install runs
// the identical binaries and must stay silent, while a plain copy elsewhere
// on PATH is a shadow and must warn.
func TestWindowsShadowScanFollowsReparseChains(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			home := newWinHome(t)
			_, errOut, code := runInstallPs(t, s, sh, home, nil)
			require.Equalf(t, 0, code, "stderr: %s", errOut)

			cases := []struct {
				name     string
				pathDir  func(t *testing.T) string
				wantWarn bool
			}{
				{
					name: "junction alias to the bin junction",
					pathDir: func(t *testing.T) string {
						t.Helper()
						alias := filepath.Join(t.TempDir(), "alias")
						makeJunction(t, alias, home.binDir())
						return alias
					},
				},
				{
					name: "junction alias to the version dir",
					pathDir: func(t *testing.T) string {
						t.Helper()
						alias := filepath.Join(t.TempDir(), "alias")
						makeJunction(t, alias, filepath.Join(home.versionsDir(), "v1.2.3"))
						return alias
					},
				},
				{
					name: "plain copy elsewhere on PATH",
					pathDir: func(t *testing.T) string {
						t.Helper()
						dir := t.TempDir()
						raw, err := os.ReadFile(filepath.Join(home.binDir(), "rancher.exe"))
						require.NoError(t, err)
						require.NoError(t, os.WriteFile(filepath.Join(dir, "rancher.exe"), raw, 0o755))
						return dir
					},
					wantWarn: true,
				},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					dir := tc.pathDir(t)
					_, errOut, code := runInstallPs(t, s, sh, home, []string{"PATH=" + hermeticPath(dir)})
					require.Equalf(t, 0, code, "stderr: %s", errOut)
					if tc.wantWarn {
						assert.Contains(t, errOut, "another rancher exists at "+filepath.Join(dir, "rancher.exe"))
						assert.Contains(t, errOut, "may shadow this install")
						return
					}
					assert.NotContains(t, errOut, "may shadow this install", "a link back at the install is not a shadow")
				})
			}
		})
	}
}

func TestWindowsRefusesBinDirHoldingOtherFiles(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	writeWindowsRelease(t, s, "v1.2.3")
	s.setLatest("v1.2.3")
	home := newWinHome(t)
	binDir := filepath.Join(home.localAppData, "tools")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "other.exe"), []byte("keep"), 0o755))
	before := digestTree(t, binDir)
	_, errOut, code := runInstallPs(t, s, sh, home, nil, "-BinDir", binDir)
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "holds other files")
	assert.Equal(t, before, digestTree(t, binDir), "a refused bin dir must be untouched")
	_, err := os.Lstat(filepath.Join(home.versionsDir(), "v1.2.3"))
	assert.True(t, os.IsNotExist(err), "the refusal must precede staging")
}

func TestWindowsCustomBinDirIsJunction(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	writeWindowsRelease(t, s, "v1.2.3")
	s.setLatest("v1.2.3")
	home := newWinHome(t)
	binDir := filepath.Join(home.localAppData, "my tools", "rancher")
	out, errOut, code := runInstallPs(t, s, sh, home, nil, "-BinDir", binDir)
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assertJunctionTo(t, binDir, filepath.Join(home.versionsDir(), "v1.2.3"))
	assert.Contains(t, out, "installed: rancher version v1.2.3")
	assert.Equal(t, binDir, readReceipt(t, home)["binDir"])
}

// TestWindowsRefusesDriveRootBinDir asserts that a drive root cannot serve as
// the bin directory: the run fails during argument validation, before any
// request leaves the process and before anything is written.
func TestWindowsRefusesDriveRootBinDir(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			home := newWinHome(t)
			before := digestInstallTree(t, home)
			_, errOut, code := runInstallPs(t, s, sh, home, nil, "-BinDir", `C:\`)
			require.NotEqual(t, 0, code)
			assert.Contains(t, errOut, "the bin directory cannot be a drive root")
			assert.NotContains(t, errOut, "no stable release", "the refusal must precede the release lookup")
			assert.Zero(t, s.apiAssetRequestCount(), "no asset may be requested")
			assert.Zero(t, s.cdnAssetRequestCount(), "no asset may be requested")
			assert.Equal(t, before, digestInstallTree(t, home), "the refusal must write nothing")
			for _, dir := range []string{home.dataDir(), home.lockDir()} {
				_, err := os.Lstat(dir)
				assert.Truef(t, os.IsNotExist(err), "%s must not be created", dir)
			}
		})
	}
}

func TestWindowsRefusesBinDirThatIsAFile(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			home := newWinHome(t)
			binDir := filepath.Join(home.localAppData, "toolfile")
			require.NoError(t, os.WriteFile(binDir, []byte("keep"), 0o644))
			_, errOut, code := runInstallPs(t, s, sh, home, nil, "-BinDir", binDir)
			require.NotEqual(t, 0, code)
			assert.Contains(t, errOut, binDir+" is a file - refusing to replace it")
			raw, err := os.ReadFile(binDir)
			require.NoError(t, err)
			assert.Equal(t, "keep", string(raw), "the refused file must be untouched")
			_, err = os.Lstat(filepath.Join(home.versionsDir(), "v1.2.3"))
			assert.True(t, os.IsNotExist(err), "the refusal must precede staging")
		})
	}
}

// TestWindowsRefusesBinDirThatIsADirectorySymlink asserts the refusal of a
// directory symbolic link as the bin dir; only a junction may be replaced.
// Creating one needs SeCreateSymbolicLinkPrivilege or developer mode, so
// the test skips where the link cannot be made.
func TestWindowsRefusesBinDirThatIsADirectorySymlink(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			home := newWinHome(t)
			target := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(target, "keep.txt"), []byte("keep"), 0o644))
			link := filepath.Join(home.localAppData, "linkbin")
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("cannot create a directory symbolic link (needs SeCreateSymbolicLinkPrivilege or developer mode): %v", err)
			}
			before := digestTree(t, target)
			_, errOut, code := runInstallPs(t, s, sh, home, nil, "-BinDir", link)
			require.NotEqual(t, 0, code)
			assert.Contains(t, errOut, link+" is a symbolic link - refusing to replace it")
			fi, err := os.Lstat(link)
			require.NoError(t, err)
			assert.NotZero(t, fi.Mode()&os.ModeSymlink, "the refused symlink must survive")
			assert.Equal(t, before, digestTree(t, target), "the symlink target must be untouched")
		})
	}
}

// TestWindowsForeignJunctionBinDirIsReplacedWithNotice asserts the takeover of
// a junction pointing outside the versions dir: the reparse point itself is
// replaced under a notice while its target's contents stay untouched.
func TestWindowsForeignJunctionBinDirIsReplacedWithNotice(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			home := newWinHome(t)
			foreign := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(foreign, "keep.txt"), []byte("keep"), 0o644))
			require.NoError(t, os.MkdirAll(home.dataDir(), 0o755))
			makeJunction(t, home.binDir(), foreign)
			before := digestTree(t, foreign)

			out, errOut, code := runInstallPs(t, s, sh, home, nil)
			require.Equalf(t, 0, code, "stderr: %s", errOut)
			assert.Contains(t, out, "notice: replacing existing Windows directory link (NTFS junction) "+home.binDir())
			assertJunctionTo(t, home.binDir(), filepath.Join(home.versionsDir(), "v1.2.3"))
			assert.Equal(t, before, digestTree(t, foreign), "the foreign target's contents must be untouched")
			for _, name := range []string{"rancher", "rancher-credential-vault"} {
				assert.Contains(t, runExe(t, filepath.Join(home.binDir(), name+".exe"), "--version"), name+" version v1.2.3")
			}
		})
	}
}

func TestWindowsKillDuringStagingLeavesOldPairAndRerunCompletes(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	writeWindowsRelease(t, s, "v1.2.3")
	s.setLatest("v1.2.3")
	home := newWinHome(t)
	_, errOut, code := runInstallPs(t, s, sh, home, nil)
	require.Equalf(t, 0, code, "stderr: %s", errOut)

	writeWindowsRelease(t, s, "v1.3.0")
	s.setLatest("v1.3.0")
	hold := filepath.Join(t.TempDir(), "hold")
	cmd := startHeldPs(t, s, sh, home, "RANCHER_CLI_INSTALL_HOLD_AFTER_STAGING", hold, stagingReachedIn(home.versionsDir()))
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()

	assertJunctionTo(t, home.binDir(), filepath.Join(home.versionsDir(), "v1.2.3"))
	for _, name := range []string{"rancher", "rancher-credential-vault"} {
		assert.Contains(t, runExe(t, filepath.Join(home.binDir(), name+".exe"), "--version"), name+" version v1.2.3")
	}

	require.NoError(t, os.RemoveAll(home.lockDir()))
	out, errOut, code := runInstallPs(t, s, sh, home, nil)
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "installed: rancher version v1.3.0")
	entries, err := os.ReadDir(home.versionsDir())
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"v1.3.0"}, names, "rerun must collect staging litter and the old version")
}

// TestWindowsKillBetweenAsideAndSwitchKeepsOldPairAndRerunRecovers asserts
// the by-hand takeover's kill window: a kill after the plain bin dir moved
// aside but before the junction was renamed in leaves the old pair intact
// in the aside, and the next run restores it, switches, and collects it.
func TestWindowsKillBetweenAsideAndSwitchKeepsOldPairAndRerunRecovers(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			home := newWinHome(t)
			seedByHandPair(t, home.binDir())

			hold := filepath.Join(t.TempDir(), "hold")
			cmd := startHeldPs(t, s, sh, home, "RANCHER_CLI_INSTALL_HOLD_AFTER_ASIDE", hold, func() bool {
				return len(asidesBeside(t, home.binDir())) == 1
			})
			require.NoError(t, cmd.Process.Kill())
			_ = cmd.Wait()

			_, err := os.Lstat(home.binDir())
			assert.True(t, os.IsNotExist(err), "the bin dir is absent between the two renames")
			asides := asidesBeside(t, home.binDir())
			require.Len(t, asides, 1)
			for name, want := range map[string]string{"rancher.exe": "old-cli", "rancher-credential-vault.exe": "old-helper"} {
				raw, err := os.ReadFile(filepath.Join(asides[0], name))
				require.NoError(t, err, "the aside must hold the old pair")
				assert.Equal(t, want, string(raw))
			}

			require.NoError(t, os.RemoveAll(home.lockDir()))
			out, errOut, code := runInstallPs(t, s, sh, home, nil)
			require.Equalf(t, 0, code, "stderr: %s", errOut)
			assert.Contains(t, out, "notice: restoring "+asides[0]+" to "+home.binDir())
			assert.Equal(t, 2, strings.Count(out, "notice: replacing existing file"), "the restored pair is taken over again")
			assert.Contains(t, out, "installed: rancher version v1.2.3")
			assertJunctionTo(t, home.binDir(), filepath.Join(home.versionsDir(), "v1.2.3"))
			assert.Empty(t, asidesBeside(t, home.binDir()), "the rerun must collect the aside")
			assert.Empty(t, stagingLinksBeside(t, home.binDir()), "the rerun must collect staging junction litter")
			assert.NotContains(t, errOut, "warning:")
		})
	}
}

// TestWindowsMigrationSurvivesLockedStaleAside asserts the by-hand takeover
// against a stale <bin>.old.<pid> from an earlier run whose pid the
// installer now reuses, held by a file open with no delete share: the
// takeover moves the pair to a uniquely named aside and finishes, the
// locked aside is warned about and left, and a run after the lock is gone
// collects it.
func TestWindowsMigrationSurvivesLockedStaleAside(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			home := newWinHome(t)
			seedByHandPair(t, home.binDir())

			hold := filepath.Join(t.TempDir(), "hold")
			cmd, logPath := startHeldPsLogged(t, s, sh, home, "RANCHER_CLI_INSTALL_HOLD_AFTER_STAGING", hold, stagingReachedIn(home.versionsDir()))
			stale := fmt.Sprintf("%s.old.%d", home.binDir(), cmd.Process.Pid)
			require.NoError(t, os.Mkdir(stale, 0o755))
			locked, err := os.OpenFile(filepath.Join(stale, "rancher.exe"), os.O_CREATE|os.O_RDWR, 0o755)
			require.NoError(t, err)
			t.Cleanup(func() { _ = locked.Close() })
			require.NoError(t, os.Remove(hold))
			_ = cmd.Wait()
			raw, err := os.ReadFile(logPath)
			require.NoError(t, err)
			out := string(raw)
			require.Equalf(t, 0, cmd.ProcessState.ExitCode(), "output: %s", out)

			assertJunctionTo(t, home.binDir(), filepath.Join(home.versionsDir(), "v1.2.3"))
			assert.Contains(t, runExe(t, filepath.Join(home.binDir(), "rancher.exe"), "--version"), "rancher version v1.2.3")
			assert.Equal(t, 2, strings.Count(out, "notice: replacing existing file"))
			assert.Contains(t, out, "warning: cannot remove the moved-aside bin directory "+stale)
			assert.Equal(t, []string{stale}, asidesBeside(t, home.binDir()), "only the locked stale aside may remain")

			require.NoError(t, locked.Close())
			out2, errOut, code := runInstallPs(t, s, sh, home, nil)
			require.Equalf(t, 0, code, "stderr: %s", errOut)
			assert.Contains(t, out2, "already present and intact")
			assert.NotContains(t, errOut, "warning:")
			assert.Empty(t, asidesBeside(t, home.binDir()), "the unlocked stale aside must be collected")
		})
	}
}

// TestWindowsKillAfterMigrationSwitchLeavesAsideForNextRun asserts that a
// by-hand takeover killed after the junction rename already serves the new
// pair, and that the aside it never removed is collected by the next run.
func TestWindowsKillAfterMigrationSwitchLeavesAsideForNextRun(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	writeWindowsRelease(t, s, "v1.2.3")
	s.setLatest("v1.2.3")
	home := newWinHome(t)
	seedByHandPair(t, home.binDir())

	hold := filepath.Join(t.TempDir(), "hold")
	want := filepath.Join(home.versionsDir(), "v1.2.3")
	cmd := startHeldPs(t, s, sh, home, "RANCHER_CLI_INSTALL_HOLD_AFTER_SWITCH", hold, func() bool {
		target, err := os.Readlink(home.binDir())
		return err == nil && strings.EqualFold(canonicalPath(strings.TrimPrefix(target, `\\?\`)), canonicalPath(want))
	})
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()

	for _, name := range []string{"rancher", "rancher-credential-vault"} {
		assert.Contains(t, runExe(t, filepath.Join(home.binDir(), name+".exe"), "--version"), name+" version v1.2.3")
	}
	require.Len(t, asidesBeside(t, home.binDir()), 1, "the aside outlives a kill before its removal")

	require.NoError(t, os.RemoveAll(home.lockDir()))
	out, errOut, code := runInstallPs(t, s, sh, home, nil)
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "already present and intact")
	assert.NotContains(t, out, "notice: restoring", "a bin junction in place needs no restore")
	assertJunctionTo(t, home.binDir(), want)
	assert.Empty(t, asidesBeside(t, home.binDir()), "the rerun must collect the aside")
}

// TestWindowsIntactVersionDirIsReusedAfterInterruptedSwitch asserts that a
// version dir fully staged by a run killed before its switch is reused by
// the next run rather than collected and copied again: a marker dropped
// into it survives, and the rerun reports the dir as already present.
func TestWindowsIntactVersionDirIsReusedAfterInterruptedSwitch(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			home := newWinHome(t)
			seedByHandPair(t, home.binDir())

			hold := filepath.Join(t.TempDir(), "hold")
			cmd := startHeldPs(t, s, sh, home, "RANCHER_CLI_INSTALL_HOLD_AFTER_ASIDE", hold, func() bool {
				return len(asidesBeside(t, home.binDir())) == 1
			})
			require.NoError(t, cmd.Process.Kill())
			_ = cmd.Wait()

			vdir := filepath.Join(home.versionsDir(), "v1.2.3")
			marker := filepath.Join(vdir, "staged-by-first-run")
			require.NoError(t, os.WriteFile(marker, nil, 0o644), "the first run must have staged the version dir")

			require.NoError(t, os.RemoveAll(home.lockDir()))
			out, errOut, code := runInstallPs(t, s, sh, home, nil)
			require.Equalf(t, 0, code, "stderr: %s", errOut)
			assert.Contains(t, out, "version v1.2.3 already present and intact")
			assert.FileExists(t, marker, "the intact dir must be reused, not collected and staged again")
			assertJunctionTo(t, home.binDir(), vdir)
			entries, err := os.ReadDir(home.versionsDir())
			require.NoError(t, err)
			require.Len(t, entries, 1)
			assert.Equal(t, "v1.2.3", entries[0].Name())
		})
	}
}

func TestWindowsKillAfterSwitchServesNewPair(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	writeWindowsRelease(t, s, "v1.2.3")
	s.setLatest("v1.2.3")
	home := newWinHome(t)
	_, errOut, code := runInstallPs(t, s, sh, home, nil)
	require.Equalf(t, 0, code, "stderr: %s", errOut)

	writeWindowsRelease(t, s, "v1.3.0")
	s.setLatest("v1.3.0")
	hold := filepath.Join(t.TempDir(), "hold")
	want := filepath.Join(home.versionsDir(), "v1.3.0")
	cmd := startHeldPs(t, s, sh, home, "RANCHER_CLI_INSTALL_HOLD_AFTER_SWITCH", hold, func() bool {
		target, err := os.Readlink(home.binDir())
		return err == nil && strings.EqualFold(canonicalPath(strings.TrimPrefix(target, `\\?\`)), canonicalPath(want))
	})
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()

	// The switch happened: both binaries serve the new version through the
	// junction even though GC, the receipt, and the epilogue never ran.
	for _, name := range []string{"rancher", "rancher-credential-vault"} {
		assert.Contains(t, runExe(t, filepath.Join(home.binDir(), name+".exe"), "--version"), name+" version v1.3.0")
	}
}

func TestWindowsStaleLockErrors(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	writeWindowsRelease(t, s, "v1.2.3")
	s.setLatest("v1.2.3")
	home := newWinHome(t)
	require.NoError(t, os.MkdirAll(home.lockDir(), 0o755))
	_, errOut, code := runInstallPs(t, s, sh, home, nil)
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "another install is in progress")
	assert.Contains(t, errOut, home.lockDir())
	assert.DirExists(t, home.lockDir(), "the loser must not release a lock it never acquired")
}

// TestWindowsConcurrentInstallErrorsOnHeldLock runs two installers for
// real: the winner is parked at the staging seam, which sits inside the
// lock, so the loser contends with a genuinely held lock rather than a
// pre-created directory. The loser must fail naming the lock and leave
// both the lock and the layout alone; the winner must then complete.
func TestWindowsConcurrentInstallErrorsOnHeldLock(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			home := newWinHome(t)
			hold := filepath.Join(t.TempDir(), "hold")
			// The winner acquires the lock before it stages, so a reached
			// staging dir proves the lock is held. It stays held until the
			// hold file is removed.
			cmd, logPath := startHeldPsLogged(t, s, sh, home, "RANCHER_CLI_INSTALL_HOLD_AFTER_STAGING", hold,
				stagingReachedIn(home.versionsDir()))
			require.DirExists(t, home.lockDir(), "the parked installer must be holding the lock")

			_, errOut, code := runInstallPs(t, s, sh, home, nil)
			require.NotEqual(t, 0, code, "the second installer must lose")
			assert.Contains(t, errOut, "another install is in progress")
			assert.Contains(t, errOut, home.lockDir(), "the error must name the lock to remove")
			assert.DirExists(t, home.lockDir(), "the loser must not release the winner's lock")
			_, err := os.Lstat(home.binDir())
			assert.True(t, os.IsNotExist(err), "the loser must publish no links")

			require.NoError(t, os.Remove(hold))
			_ = cmd.Wait()
			raw, err := os.ReadFile(logPath)
			require.NoError(t, err)
			require.Equalf(t, 0, cmd.ProcessState.ExitCode(), "winner output: %s", raw)
			assertJunctionTo(t, home.binDir(), filepath.Join(home.versionsDir(), "v1.2.3"))
			for _, name := range []string{"rancher", "rancher-credential-vault"} {
				assert.Contains(t, runExe(t, filepath.Join(home.binDir(), name+".exe"), "--version"), name+" version v1.2.3")
			}
		})
	}
}

// TestWindowsQuietSuppressesNotesNotWarnings asserts that -Quiet hides progress
// notes while warnings still reach stderr.
func TestWindowsQuietSuppressesNotesNotWarnings(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	writeWindowsRelease(t, s, "v1.2.3")
	writeWindowsRelease(t, s, "v1.3.0")
	home := newWinHome(t)
	_, errOut, code := runInstallPs(t, s, sh, home, nil, "-Version", "v1.3.0", "-Quiet")
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	out, errOut, code := runInstallPs(t, s, sh, home, nil, "-Version", "v1.2.3", "-Quiet")
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Empty(t, out, "-Quiet must silence stdout")
	assert.Contains(t, errOut, "warning: downgrading")
}

// windowsPlatformMappingCases is install.ps1's PROCESSOR_ARCHITECTURE map.
// TestWindowsPlatformMatrixConformance cross-checks its accepted set against
// scripts/release-build's windows/* targets.
var windowsPlatformMappingCases = []struct {
	arch string
	want string // "" = must be rejected
}{
	{"AMD64", "windows/amd64"},
	{"ARM64", ""},
	{"x86", ""},
}

func TestWindowsPlatformMapping(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	for _, tc := range windowsPlatformMappingCases {
		t.Run(tc.arch, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			env := []string{"PROCESSOR_ARCHITECTURE=" + tc.arch}
			out, errOut, code := runInstallPs(t, s, sh, newWinHome(t), env, "-DryRun", "-Version", "v1.2.3")
			if tc.want == "" {
				require.NotEqualf(t, 0, code, "%s must be rejected", tc.arch)
				assert.Contains(t, errOut, "unsupported platform")
				return
			}
			require.Equalf(t, 0, code, "%s stderr: %s", tc.arch, errOut)
			assert.Contains(t, out, "platform: "+tc.want)
		})
	}
}

// TestWindowsPlatformMatrixConformance keeps install.ps1's accepted
// architectures in lockstep with scripts/release-build's windows/* build targets.
func TestWindowsPlatformMatrixConformance(t *testing.T) {
	t.Parallel()
	var accepts []string
	for _, tc := range windowsPlatformMappingCases {
		if tc.want != "" {
			accepts = append(accepts, tc.want)
		}
	}
	var want []string
	for _, p := range windowsInstallerPlatforms {
		want = append(want, strings.Replace(p, "-", "/", 1))
	}
	assert.ElementsMatch(t, want, accepts,
		"install.ps1's accepted platforms (windowsPlatformMappingCases) must match scripts/release-build's windows targets")
}

// TestWindowsTruncatedScriptMidFunctionExecutesNothing asserts that a download
// cut inside Install-RancherCli cannot run a partial install: the host
// rejects the unbalanced script and the profile is unchanged.
func TestWindowsTruncatedScriptMidFunctionExecutesNothing(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			content, err := os.ReadFile("../install.ps1")
			require.NoError(t, err)
			marker := []byte("            Add-Stage\r\n            Switch-Current\r\n")
			if idx := strings.Index(string(content), string(marker)); idx < 0 {
				marker = []byte("            Add-Stage\n            Switch-Current\n")
			}
			idx := strings.Index(string(content), string(marker))
			require.Greater(t, idx, -1, "cut marker must be present in install.ps1")
			cutAt := idx + 4
			truncPath := filepath.Join(t.TempDir(), "install-truncated.ps1")
			require.NoError(t, os.WriteFile(truncPath, content[:cutAt], 0o644))

			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			home := newWinHome(t)
			before := digestInstallTree(t, home)
			cmd := exec.Command(sh.path, psArgs(sh, truncPath)...)
			cmd.Env = psEnv(s, home, "RANCHER_CLI_VERSION=v1.2.3")
			var out, errb strings.Builder
			cmd.Stdout, cmd.Stderr = &out, &errb
			require.Error(t, cmd.Run(), "a script cut mid-function must fail to parse")
			assert.Empty(t, out.String(), "a parse failure must produce no stdout")
			assert.Equal(t, before, digestInstallTree(t, home), "a truncated script must write nothing")
		})
	}
}

// TestWindowsTruncatedScriptAtFunctionBoundaryIsSilentNoOp documents the
// accepted trade-off: a cut before the final invocation line leaves only a
// definition, so nothing runs and nothing is written.
func TestWindowsTruncatedScriptAtFunctionBoundaryIsSilentNoOp(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	content, err := os.ReadFile("../install.ps1")
	require.NoError(t, err)
	lines := strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")
	require.Empty(t, lines[len(lines)-1], "install.ps1 must end with a newline")
	require.True(t, strings.HasPrefix(lines[len(lines)-2], "Install-RancherCli "),
		"the last line of install.ps1 must be the Install-RancherCli invocation")
	truncPath := filepath.Join(t.TempDir(), "install-truncated.ps1")
	require.NoError(t, os.WriteFile(truncPath, []byte(strings.Join(lines[:len(lines)-2], "\n")+"\n"), 0o644))

	s := newStub(t)
	home := newWinHome(t)
	before := digestInstallTree(t, home)
	cmd := exec.Command(sh.path, psArgs(sh, truncPath)...)
	cmd.Env = psEnv(s, home, "RANCHER_CLI_VERSION=v1.2.3")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "a definition-only script must exit 0: %s", out)
	assert.Empty(t, string(out), "must produce no output")
	assert.Equal(t, before, digestInstallTree(t, home), "must write nothing")
}

// TestWindowsBootstrapOneLiner runs the README one-liner byte-for-byte (URL
// swapped for a local server) against the stub: the pipe form must define
// the parameter block, run the install, and leave the pair in place.
func TestWindowsBootstrapOneLiner(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			script, err := os.ReadFile("../install.ps1")
			require.NoError(t, err)
			raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = w.Write(script)
			}))
			defer raw.Close()
			s := newStub(t)
			writeWindowsRelease(t, s, "v1.2.3")
			s.setLatest("v1.2.3")
			home := newWinHome(t)

			snippet := "irm " + raw.URL + "/install.ps1 | iex"
			assertSnippetMatchesDoc(t, "README.md", "irm https://raw.githubusercontent.com/", snippet,
				func(s string) string { return strings.Replace(s, raw.URL+"/install.ps1", rawInstallPsURL, 1) })

			cmd := exec.Command(sh.path, "-NoProfile", "-NonInteractive", "-Command", snippet)
			cmd.Env = psEnv(s, home)
			out, runErr := cmd.CombinedOutput()
			require.NoErrorf(t, runErr, "README one-liner failed: %s", out)
			assert.Contains(t, string(out), "installed: rancher version v1.2.3")
			assertJunctionTo(t, home.binDir(), filepath.Join(home.versionsDir(), "v1.2.3"))
		})
	}
}

// TestWindowsBootstrapParameterForm runs the README parameter form (the
// scriptblock wrapper that passes switches through Invoke-Expression) with
// -Pre and -DryRun, proving parameters reach the script's param block.
func TestWindowsBootstrapParameterForm(t *testing.T) {
	t.Parallel()
	for _, sh := range powerShells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			script, err := os.ReadFile("../install.ps1")
			require.NoError(t, err)
			raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = w.Write(script)
			}))
			defer raw.Close()
			s := newStub(t)
			writeWindowsRelease(t, s, "v0.1.0-alpha.1")
			home := newWinHome(t)

			snippet := `iex "& { $(irm ` + raw.URL + `/install.ps1) } -Pre -DryRun"`
			assertSnippetMatchesDoc(t, "README.md", `iex "& { $(irm https://raw.githubusercontent.com/`, snippet,
				func(s string) string {
					return strings.NewReplacer(raw.URL+"/install.ps1", rawInstallPsURL, " -Pre -DryRun", " -Pre").Replace(s)
				})

			cmd := exec.Command(sh.path, "-NoProfile", "-NonInteractive", "-Command", snippet)
			cmd.Env = psEnv(s, home)
			out, runErr := cmd.CombinedOutput()
			require.NoErrorf(t, runErr, "README parameter form failed: %s", out)
			assert.Contains(t, string(out), "tag: v0.1.0-alpha.1")
			_, err = os.Lstat(home.binDir())
			assert.True(t, os.IsNotExist(err), "a dry run must install nothing")
		})
	}
}

// TestWindowsBootstrapPropagatesInstallerFailure asserts that a failing install
// under Invoke-Expression surfaces as a non-zero host exit code, so scripted
// callers of the one-liner see the failure.
func TestWindowsBootstrapPropagatesInstallerFailure(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	script, err := os.ReadFile("../install.ps1")
	require.NoError(t, err)
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(script)
	}))
	defer raw.Close()
	s := newStub(t)
	home := newWinHome(t)
	cmd := exec.Command(sh.path, "-NoProfile", "-NonInteractive", "-Command", "irm "+raw.URL+"/install.ps1 | iex")
	cmd.Env = psEnv(s, home, "RANCHER_CLI_VERSION=v1/evil")
	out, runErr := cmd.CombinedOutput()
	require.Errorf(t, runErr, "installer failure must propagate through the pipe, output: %s", out)
	assert.Contains(t, string(out), "is not a valid release tag")
}

// shortPathName returns the 8.3 short form of an existing path - the form
// some environments hand out in LOCALAPPDATA/APPDATA/TEMP (GitHub-hosted
// runners, notably), where a naive string compare against the long form of
// the same directory fails.
func shortPathName(t *testing.T, path string) string {
	t.Helper()
	// The path travels via the environment, never argv: PowerShell and
	// cmd quoting both mangle quoted paths handed through exec argv.
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		"(New-Object -ComObject Scripting.FileSystemObject).GetFolder($env:RANCHER_TEST_SHORTPATH_TARGET).ShortPath")
	cmd.Env = append(os.Environ(), "RANCHER_TEST_SHORTPATH_TARGET="+path)
	out, err := cmd.Output()
	require.NoError(t, err)
	s := strings.TrimSpace(string(out))
	require.NotEmpty(t, s)
	return s
}

// TestWindowsShortPathEnvironmentInstallsAndReruns asserts that an environment
// handing out 8.3 short paths installs cleanly, passes its own switch
// verification, and on a rerun recognizes its own junction rather than
// treating it as foreign or garbage-collecting the version directory the
// live junction points at. Printed paths and the receipt echo the short
// form the environment handed in: canonical long-form paths exist only
// inside comparisons, never in what the operator sees.
func TestWindowsShortPathEnvironmentInstallsAndReruns(t *testing.T) {
	t.Parallel()
	sh := requirePowerShell(t)
	s := newStub(t)
	writeWindowsRelease(t, s, "v1.2.3")
	s.setLatest("v1.2.3")
	long := newWinHome(t)
	home := winHome{
		localAppData: shortPathName(t, long.localAppData),
		appData:      shortPathName(t, long.appData),
		temp:         shortPathName(t, long.temp),
	}
	if home == long {
		t.Skip("8.3 short names are disabled on this volume")
	}

	out, errOut, code := runInstallPs(t, s, sh, home, nil)
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "installed: rancher version v1.2.3")
	assert.Contains(t, out, home.binDir(),
		"the PATH hint must echo the bin dir as the environment spelled it")
	receipt := readReceipt(t, home)
	assert.Equal(t, home.binDir(), receipt["binDir"],
		"the receipt must echo the bin dir as the environment spelled it")
	assert.Equal(t, home.dataDir(), receipt["dataDir"],
		"the receipt must echo the data dir as the environment spelled it")

	out2, errOut2, code2 := runInstallPs(t, s, sh, home, nil)
	require.Equalf(t, 0, code2, "rerun stderr: %s", errOut2)
	assert.NotContains(t, out2, "replacing existing Windows directory link",
		"the rerun must recognize its own junction under a short-path environment")
	assert.DirExists(t, filepath.Join(long.versionsDir(), "v1.2.3"),
		"the version directory the live junction points at must survive the rerun's GC")
}
