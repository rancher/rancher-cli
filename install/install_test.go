package install_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanHomeInstallProducesPairAndReceipt(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	out, errOut, code := runInstall(t, s, envHome(home))
	require.Equalf(t, 0, code, "stderr: %s", errOut)

	bin := filepath.Join(home, ".local", "bin")
	for _, name := range []string{"rancher", "rancher-credential-vault"} {
		fi, err := os.Lstat(filepath.Join(bin, name))
		require.NoError(t, err)
		assert.NotZero(t, fi.Mode()&os.ModeSymlink, "%s must be a symlink", name)
	}
	assert.Contains(t, out, "rancher version v1.2.3")

	receiptPath := filepath.Join(home, ".config", "rancher-cli", "install-receipt.json")
	raw, err := os.ReadFile(receiptPath)
	require.NoError(t, err, "clean home must still produce a receipt")
	var receipt map[string]string
	require.NoError(t, json.Unmarshal(raw, &receipt), "receipt must be valid JSON")
	assert.Equal(t, "v1.2.3", receipt["tag"])
	assert.Equal(t, "script", receipt["channel"])
	assert.Equal(t, "versioned-dir", receipt["layout"])
}

func TestChecksumMismatchIsFatal(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	// Tamper the CURRENT platform's CLI asset - derived, not hardcoded, so
	// the test exercises a mismatch on every supported platform (a fixed
	// list omitted linux-s390x and would pass untested there).
	s.setAsset("v1.2.3/rancher-"+runtime.GOOS+"-"+runtime.GOARCH, []byte("tampered"))
	_, errOut, code := runInstall(t, s, nil)
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "checksum verification failed")
}

// TestChecksumEntryPerAssetCompleteness asserts that each asset needs exactly
// one SHA256SUMS entry: a missing helper entry, and a duplicated CLI entry
// standing in for it, must both be fatal - a combined-count check would
// accept the duplicate and leave the helper entirely unchecked.
func TestChecksumEntryPerAssetCompleteness(t *testing.T) {
	t.Parallel()
	plat := runtime.GOOS + "-" + runtime.GOARCH
	cli, helper := "rancher-"+plat, "rancher-credential-vault-"+plat
	mutations := map[string]struct {
		mutate func(sums string) string
		want   string
	}{
		"missing helper entry": {want: "SHA256SUMS must contain exactly one entry for " + helper + " (found 0)", mutate: func(sums string) string {
			var keep []string
			for l := range strings.SplitSeq(sums, "\n") {
				if !strings.HasSuffix(l, "  "+helper) {
					keep = append(keep, l)
				}
			}
			return strings.Join(keep, "\n")
		}},
		"duplicate cli entry masking the helper": {want: "SHA256SUMS must contain exactly one entry for " + cli + " (found 2)", mutate: func(sums string) string {
			var out []string
			for l := range strings.SplitSeq(sums, "\n") {
				if strings.HasSuffix(l, "  "+helper) {
					continue
				}
				out = append(out, l)
				if strings.HasSuffix(l, "  "+cli) {
					out = append(out, l)
				}
			}
			return strings.Join(out, "\n")
		}},
		"missing cli entry": {want: "SHA256SUMS must contain exactly one entry for " + cli + " (found 0)", mutate: func(sums string) string {
			var keep []string
			for l := range strings.SplitSeq(sums, "\n") {
				if !strings.HasSuffix(l, "  "+cli) {
					keep = append(keep, l)
				}
			}
			return strings.Join(keep, "\n")
		}},
		"duplicate helper entry masking the cli": {want: "SHA256SUMS must contain exactly one entry for " + cli + " (found 0)", mutate: func(sums string) string {
			var out []string
			for l := range strings.SplitSeq(sums, "\n") {
				if strings.HasSuffix(l, "  "+cli) {
					continue
				}
				out = append(out, l)
				if strings.HasSuffix(l, "  "+helper) {
					out = append(out, l)
				}
			}
			return strings.Join(out, "\n")
		}},
	}
	// The wanted message names the asset the check fails on first: the CLI
	// is checked before the helper, so a dropped CLI entry reports the CLI
	// even when the helper entry is also wrong.
	for name, tc := range mutations {
		t.Run(name, func(t *testing.T) {
			s := newStub(t)
			writeRelease(s, "v1.2.3", false)
			s.setLatest("v1.2.3")
			s.setAsset("v1.2.3/SHA256SUMS", []byte(tc.mutate(string(s.asset("v1.2.3/SHA256SUMS")))))
			_, errOut, code := runInstall(t, s, nil)
			require.NotEqual(t, 0, code)
			assert.Contains(t, errOut, tc.want)
		})
	}
}

func TestVerifyAttestationWithoutGhIsFatal(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	// PATH without gh (built by toolPath): the capability
	// probe fails deterministically regardless of the host.
	_, errOut, code := runInstall(t, s, []string{"PATH=" + toolPath(t, baseTools...)}, "--verify-attestation")
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "--verify-attestation needs a gh CLI whose 'gh attestation verify' supports --signer-workflow")
}

func TestReceiptSymlinkToDirectoryIsReplacedNotEntered(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	cfg := filepath.Join(home, ".config", "rancher-cli")
	require.NoError(t, os.MkdirAll(filepath.Join(cfg, "trap-dir"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(cfg, "trap-dir"), filepath.Join(cfg, "install-receipt.json")))
	_, errOut, code := runInstall(t, s, envHome(home))
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	fi, err := os.Lstat(filepath.Join(cfg, "install-receipt.json"))
	require.NoError(t, err)
	assert.True(t, fi.Mode().IsRegular(),
		"the receipt path must be replaced without following the symlink, not written inside the symlinked directory")
	entries, err := os.ReadDir(filepath.Join(cfg, "trap-dir"))
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing may be dropped inside the trap directory")
}

func TestReceiptFailurePreservesPriorReceipt(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	_, _, code := runInstall(t, s, envHome(home))
	require.Equal(t, 0, code)
	receiptPath := filepath.Join(home, ".config", "rancher-cli", "install-receipt.json")
	prior, err := os.ReadFile(receiptPath)
	require.NoError(t, err)
	// Unwritable receipt dir: the atomic temp-file create fails BEFORE any
	// rename, so the prior receipt must survive byte-for-byte and the
	// install must still succeed with a warning.
	require.NoError(t, os.Chmod(filepath.Dir(receiptPath), 0o500))
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(receiptPath), 0o755) })
	_, errOut, code := runInstall(t, s, envHome(home))
	require.Equal(t, 0, code, "receipt failure must not fail the install")
	assert.Contains(t, errOut, "install receipt not written")
	after, err := os.ReadFile(receiptPath)
	require.NoError(t, err)
	assert.Equal(t, prior, after, "prior receipt must be unchanged")
}

func TestReceiptSurvivesMetacharBinDir(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	binDir := filepath.Join(home, `we"ird\dir`)
	_, errOut, code := runInstall(t, s, envHome(home), "--bin-dir", binDir)
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	raw, err := os.ReadFile(filepath.Join(home, ".config", "rancher-cli", "install-receipt.json"))
	require.NoError(t, err)
	var receipt map[string]string
	require.NoError(t, json.Unmarshal(raw, &receipt), "metachar bin-dir must still yield valid JSON")
	assert.Equal(t, binDir, receipt["binDir"])
}

// TestInternalRepoTokenModeInstalls proves the end-to-end API-asset path: when
// a token is in play and the browser download URL returns 404 (private/internal
// repo), the installer falls back to the GitHub API asset endpoint and succeeds.
// Assertions: install completes, receipt lands, Authorization header reached the
// stub on asset requests, token never appeared in any curl argv.
func TestInternalRepoTokenModeInstalls(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	s.setPrivate(true)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")

	wrapDir, argvLog, wrapEnv := recordingCurlWrapper(t)
	home := t.TempDir()

	env := append(envHome(home),
		"GH_TOKEN=test-internal-token",
		"PATH="+wrapDir+":"+toolPath(t, baseTools...),
	)
	env = append(env, wrapEnv...)

	out, errOut, code := runInstall(t, s, env)
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "rancher version v1.2.3")

	// Token must arrive as an Authorization header on API asset requests.
	assert.Equal(t, "Bearer test-internal-token", s.lastAuthHeader())

	// Token must never appear in any curl argv - stdin config only.
	argv, err := os.ReadFile(argvLog)
	require.NoError(t, err, "curl wrapper must have been invoked")
	assert.NotContains(t, string(argv), "test-internal-token",
		"token must never appear in any curl argv")

	// Direct-200 path: each of the three assets (CLI, helper, SHA256SUMS)
	// must be fetched in exactly one API request - no re-download after probe.
	assert.Equal(t, 3, s.apiAssetRequestCount(),
		"direct-200 mode: exactly one API request per asset")

	// Receipt and layout must land exactly as in the public flow.
	receiptPath := filepath.Join(home, ".config", "rancher-cli", "install-receipt.json")
	raw, err := os.ReadFile(receiptPath)
	require.NoError(t, err, "private-mode install must still write a receipt")
	var receipt map[string]string
	require.NoError(t, json.Unmarshal(raw, &receipt))
	assert.Equal(t, "v1.2.3", receipt["tag"])
	assert.Equal(t, "script", receipt["channel"])
	assert.Equal(t, "versioned-dir", receipt["layout"])
}

// TestInternalRepoAnonymousFailsClearly asserts the failure mode when no token is
// available for a private/internal repo: the browser download URL returns 404
// and the installer exits with an error message that names the asset. This
// ensures the failure is clear rather than silent.
func TestInternalRepoAnonymousFailsClearly(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	s.setPrivate(true)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")

	// No token env var, no gh binary.
	_, errOut, code := runInstall(t, s, nil)
	require.NotEqual(t, 0, code, "anonymous access to a private repo must fail")
	// Must name the failing asset so the user knows what went wrong.
	assert.Contains(t, errOut, "download failed")
	assert.Contains(t, errOut, "rancher-",
		"error must name the asset to help the user understand what failed")
}

// TestMissingAssetInPrivateModeFails covers the "asset not found in release"
// error path: the release metadata is readable but a named asset is genuinely
// absent (broken/partial release). The error must name the missing asset.
// Uses the awk fallback (no jq/python3 on PATH - baseTools only).
func TestMissingAssetInPrivateModeFails(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	s.setPrivate(true)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")

	// Override release JSON: remove SHA256SUMS from the assets array.
	type relAsset struct {
		URL  string `json:"url"`
		Name string `json:"name"`
		ID   int64  `json:"id"`
	}
	type relDoc struct {
		TagName string     `json:"tag_name"`
		Draft   bool       `json:"draft"`
		Assets  []relAsset `json:"assets"`
	}
	var doc relDoc
	require.NoError(t, json.Unmarshal(s.releaseJSONFor("v1.2.3"), &doc))
	var kept []relAsset
	for _, a := range doc.Assets {
		if a.Name != "SHA256SUMS" {
			kept = append(kept, a)
		}
	}
	doc.Assets = kept
	modified, err := json.MarshalIndent(doc, "", "  ")
	require.NoError(t, err)
	s.setReleaseJSON("v1.2.3", modified)

	_, errOut, code := runInstall(t, s, []string{
		"GH_TOKEN=test-token",
		"PATH=" + toolPath(t, baseTools...),
	})
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "asset not found in release v1.2.3")
	assert.Contains(t, errOut, "SHA256SUMS")
}

// TestAdversarialBodyDoesNotConfuseAwkFallback is the discrimination test for
// the /"body":/ exit bound. The hand-built release JSON has no SHA256SUMS in
// the assets array, but places literal url/name lines - matching exactly what
// awk's patterns look for - as top-level JSON fields AFTER the "body": line.
// An unbounded awk would scan past "body": and extract fake id 999999; the
// bounded awk must exit at "body": and report "asset not found" instead.
//
// This test uses hand-built raw bytes (not json.MarshalIndent) because
// json.MarshalIndent encodes the body string value with escaped quotes
// (\"url\": ...) which does not faithfully replicate the literal "url":
// pattern awk matches. The adversarial content must appear as real JSON field
// lines to exercise the actual attack surface.
func TestAdversarialBodyDoesNotConfuseAwkFallback(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	s.setPrivate(true)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")

	// Parse the stub's release JSON to extract real asset entries without
	// SHA256SUMS, preserving their numeric IDs so assetByID can serve them.
	type relAsset struct {
		URL  string `json:"url"`
		Name string `json:"name"`
		ID   int64  `json:"id"`
	}
	var origDoc struct {
		TagName string     `json:"tag_name"`
		Draft   bool       `json:"draft"`
		Assets  []relAsset `json:"assets"`
	}
	require.NoError(t, json.Unmarshal(s.releaseJSONFor("v1.2.3"), &origDoc))
	var realAssets []relAsset
	for _, a := range origDoc.Assets {
		if a.Name != "SHA256SUMS" {
			realAssets = append(realAssets, a)
		}
	}
	assetsJSON, err := json.MarshalIndent(realAssets, "", "  ")
	require.NoError(t, err)

	// Hand-build raw JSON: real assets (no SHA256SUMS) + "body": line + fake
	// url/name as literal top-level fields after it.  Bounded awk exits at
	// "body": before reaching the adversarial lines and reports "asset not
	// found".  Without the bound, awk matches the fake url (extracting 999999)
	// and the fake "name": "SHA256SUMS", misidentifying SHA256SUMS_ASSET_ID.
	fakeURL := s.srv.URL + "/repos/rancher/rancher-cli/releases/assets/999999"
	raw := fmt.Sprintf(`{
  "tag_name": "v1.2.3",
  "draft": false,
  "assets": %s,
  "body": "release notes",
  "url": %q,
  "name": "SHA256SUMS"
}`, string(assetsJSON), fakeURL)
	s.setReleaseJSON("v1.2.3", []byte(raw))

	// Force the awk fallback: no jq or python3 on PATH.
	_, errOut, code := runInstall(t, s, []string{
		"GH_TOKEN=test-token",
		"PATH=" + toolPath(t, baseTools...),
	})
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "asset not found in release v1.2.3",
		"bounded awk must stop at body: and not extract the fake id")
	assert.Contains(t, errOut, "SHA256SUMS")
	assert.NotContains(t, errOut, "999999",
		"fake id 999999 must not appear - awk must have exited at the body: line")
}

// TestScopedTokenPublicRepoFallsBackToBrowserPath covers a present token whose
// releases/tags metadata endpoint returns 403 (a scoped PAT that cannot read
// this repo). fetch_asset_ids must print a fallback note and leave
// CLI_ASSET_ID empty so download() uses the browser URL, which succeeds on a
// public repo.
func TestScopedTokenPublicRepoFallsBackToBrowserPath(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	// NOT private: browser download URLs work fine.
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	// Metadata endpoint returns 403 - simulates a narrowly-scoped PAT.
	s.setReleaseTagStatus(403)

	home := t.TempDir()
	out, errOut, code := runInstall(t, s, append(envHome(home),
		"GH_TOKEN=scoped-token",
		"PATH="+toolPath(t, baseTools...),
	))
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, errOut, "falling back to the release download URL",
		"scoped-token metadata failure must produce the fallback warning on stderr")
	assert.Contains(t, errOut, "release metadata not readable with this GitHub token (HTTP 403)",
		"the warning must name the HTTP status of the metadata request")
	assert.Contains(t, out, "rancher version v1.2.3",
		"install must succeed via the browser download path")
}

// TestMetadataRequestWithNoStatusFallsBackToBrowserPath covers a present token
// whose releases/tags request ends with no HTTP status at all (curl prints
// nothing and fails): the warning says "no HTTP status" and the install
// succeeds through the browser URL on a public repository.
func TestMetadataRequestWithNoStatusFallsBackToBrowserPath(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	wrapDir, wrapEnv := curlFailingReleaseTag(t)

	home := t.TempDir()
	env := append(envHome(home),
		"GH_TOKEN=scoped-token",
		"PATH="+wrapDir+":"+toolPath(t, baseTools...),
	)
	out, errOut, code := runInstall(t, s, append(env, wrapEnv...))
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, errOut,
		"release metadata not readable with this GitHub token (no HTTP status) - falling back to the release download URL")
	assert.NotContains(t, errOut, "(HTTP error)",
		"a request with no status must not print a made-up status")
	assert.Contains(t, out, "rancher version v1.2.3",
		"install must succeed via the browser download path")
}

// TestPython3BranchExtractsAssetIds covers the python3 extraction branch:
// private repo, token present, python3 on PATH but no jq. Skipped when
// python3 is not functional on the host (e.g. macOS CLT stub exits non-zero).
func TestPython3BranchExtractsAssetIds(t *testing.T) {
	t.Parallel()
	if err := exec.Command("python3", "-c", "pass").Run(); err != nil {
		t.Skip("python3 not functional on this host:", err)
	}
	s := newStub(t)
	s.setPrivate(true)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")

	home := t.TempDir()
	// python3 on PATH, jq absent: exercises the python3 branch of fetch_asset_ids.
	out, errOut, code := runInstall(t, s, append(envHome(home),
		"GH_TOKEN=test-python3-token",
		"PATH="+toolPath(t, append(append([]string{}, baseTools...), "python3")...),
	))
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "rancher version v1.2.3")
	assert.Equal(t, "Bearer test-python3-token", s.lastAuthHeader(),
		"python3 extraction must route downloads through the API asset endpoint")
}

// TestPrivateRepoTokenModeWith302Redirect proves the two-step fetch_asset path:
// the API asset endpoint returns 302 to a signed CDN URL; fetch_asset must
// follow it WITHOUT the token (the CDN endpoint must receive no Authorization
// header) while the API endpoint itself still gets the token.
func TestPrivateRepoTokenModeWith302Redirect(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	s.setPrivate(true)
	s.setAssetRedirect(true) // API asset endpoint 302s to /cdn/assets/<id>
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")

	wrapDir, argvLog, wrapEnv := recordingCurlWrapper(t)
	home := t.TempDir()
	env := append(envHome(home),
		"GH_TOKEN=test-302-token",
		"PATH="+wrapDir+":"+toolPath(t, baseTools...),
	)
	env = append(env, wrapEnv...)

	out, errOut, code := runInstall(t, s, env)
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "rancher version v1.2.3")

	// API endpoint received the token.
	assert.Equal(t, "Bearer test-302-token", s.lastAuthHeader())
	// CDN endpoint must NOT have received the token.
	assert.Empty(t, s.lastCDNAuthHeader(),
		"token must not be forwarded to the CDN redirect target")

	// Token must never appear in any curl argv.
	argv, err := os.ReadFile(argvLog)
	require.NoError(t, err)
	assert.NotContains(t, string(argv), "test-302-token",
		"token must never appear in any curl argv")

	// 302 path: one API probe and one redirect URL request per asset -
	// three of each.
	assert.Equal(t, 3, s.apiAssetRequestCount(),
		"302 mode: exactly one API request per asset")
	assert.Equal(t, 3, s.cdnAssetRequestCount(),
		"302 mode: exactly one redirect URL request per asset")
}

// TestPrivateRepoTokenWithMetadata403FailsWithEnrichedMessage covers the case
// where TOKEN is present but the metadata endpoint returns 403 (missing scope
// or SSO authorization) and the repo is private: download falls back to
// browser URLs which 404, and the error must name BOTH the asset failure AND
// the metadata failure code - so the user knows the real cause is their
// token, not a missing asset.
func TestPrivateRepoTokenWithMetadata403FailsWithEnrichedMessage(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	s.setPrivate(true)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	// Metadata endpoint returns 403 (token lacks repo access or SSO not authorized).
	s.setReleaseTagStatus(403)

	_, errOut, code := runInstall(t, s, []string{
		"GH_TOKEN=test-token",
		"PATH=" + toolPath(t, baseTools...),
	})
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "download failed",
		"download must fail because browser URLs 404 on a private repo")
	assert.Contains(t, errOut, "release metadata was also unreadable (HTTP 403)",
		"enriched message must name the metadata HTTP code")
	assert.NotContains(t, errOut, "asset not found",
		"error must not mis-diagnose the cause as a missing release asset")
}

// TestPrivateRepoTokenWithNoMetadataStatusFailsWithEnrichedMessage covers the
// private repository case where the releases/tags request ends with no HTTP
// status at all: download falls back to browser URLs which 404, and the error
// must say the metadata request had "no HTTP status".
func TestPrivateRepoTokenWithNoMetadataStatusFailsWithEnrichedMessage(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	s.setPrivate(true)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	wrapDir, wrapEnv := curlFailingReleaseTag(t)

	env := append([]string{
		"GH_TOKEN=test-token",
		"PATH=" + wrapDir + ":" + toolPath(t, baseTools...),
	}, wrapEnv...)
	_, errOut, code := runInstall(t, s, env)
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "download failed",
		"download must fail because browser URLs 404 on a private repo")
	assert.Contains(t, errOut, "release metadata was also unreadable (no HTTP status)",
		"enriched message must say the metadata request had no HTTP status")
	assert.NotContains(t, errOut, "(HTTP error)",
		"a request with no status must not print a made-up status")
}

// TestDryRunWithTokenShowsAPIAssetURLs covers --dry-run with a token runs
// fetch_asset_ids so the asset lines in the preview reflect the true download
// source (API endpoint with numeric ids) rather than the browser URL.
func TestDryRunWithTokenShowsAPIAssetURLs(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")

	home := t.TempDir()
	out, errOut, code := runInstall(t, s, append(envHome(home),
		"GH_TOKEN=test-token",
		"PATH="+toolPath(t, baseTools...),
	), "--dry-run")
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "asset: "+s.srv.URL+"/repos/rancher/rancher-cli/releases/assets/",
		"dry-run with token must show API asset URLs, not browser download URLs")
	assert.NotContains(t, out, "/releases/download/",
		"browser download URL form must not appear in a tokened dry-run preview")
}

// TestPreInstallWithTokenUsesAPIPath covers the --pre + token path end to end:
// resolve_pre_tag caches the release JSON (REL_JSON), fetch_asset_ids uses it
// without a second tags-endpoint fetch, and the install completes via the API
// asset endpoint.
func TestPreInstallWithTokenUsesAPIPath(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	s.setPrivate(true)
	writeRelease(s, "v0.1.0-alpha.1", false)

	home := t.TempDir()
	out, errOut, code := runInstall(t, s, append(append(envHome(home), withParser(t)...),
		"GH_TOKEN=test-pre-token",
	), "--pre")
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "rancher version v0.1.0-alpha.1")
	assert.Equal(t, "Bearer test-pre-token", s.lastAuthHeader(),
		"--pre install with token must route downloads through the API asset path")
}

// TestParserFirstWithAdversarialBody proves the parser path (jq) is immune to
// adversarial release-body content: the same adversarial body that would fool
// the unbounded awk does NOT affect jq's structured traversal of .assets[].
func TestParserFirstWithAdversarialBody(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	s.setPrivate(true)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")

	// Inject adversarial body into the release JSON while keeping all real assets.
	type relAsset struct {
		URL  string `json:"url"`
		Name string `json:"name"`
		ID   int64  `json:"id"`
	}
	type relDoc struct {
		TagName string     `json:"tag_name"`
		Draft   bool       `json:"draft"`
		Assets  []relAsset `json:"assets"`
		Body    string     `json:"body,omitempty"`
	}
	var doc relDoc
	require.NoError(t, json.Unmarshal(s.releaseJSONFor("v1.2.3"), &doc))
	doc.Body = `"url": "https://api.github.com/repos/rancher/rancher-cli/releases/assets/999999"` +
		"\n" + `"name": "SHA256SUMS"`
	modified, err := json.MarshalIndent(doc, "", "  ")
	require.NoError(t, err)
	s.setReleaseJSON("v1.2.3", modified)

	home := t.TempDir()
	// jq on PATH: parser-first path must extract correct ids, ignoring body.
	out, errOut, code := runInstall(t, s, append(envHome(home),
		"GH_TOKEN=test-token",
		"PATH="+toolPath(t, append(append([]string{}, baseTools...), "jq")...),
	))
	require.Equalf(t, 0, code, "jq parser must extract correct ids despite adversarial body; stderr: %s", errOut)
	assert.Contains(t, out, "rancher version v1.2.3")
}

// TestEpilogueWarnsWhenAnotherBinaryShadowsInstall covers the PATH shadow
// scan: a rancher binary elsewhere on PATH (dev installs, package managers)
// can shadow the fresh links by PATH order or a stale shell command hash -
// the installer must name the hazard and the remedy.
func TestEpilogueWarnsWhenAnotherBinaryShadowsInstall(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()

	shadowDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(shadowDir, "rancher"),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755))

	env := append(envHome(home),
		"PATH="+shadowDir+":"+toolPath(t, baseTools...))
	_, errOut, code := runInstall(t, s, env)
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, errOut, "another rancher exists at "+filepath.Join(shadowDir, "rancher"),
		"a shadowing binary elsewhere on PATH must be named")
	assert.Contains(t, errOut, "hash -r",
		"the stale-command-hash remedy must be named")
}

// TestEpilogueQuietOnCleanPath asserts the inverse: no shadow, no warning.
func TestEpilogueQuietOnCleanPath(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()
	_, errOut, code := runInstall(t, s, envHome(home))
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.NotContains(t, errOut, "may shadow this install",
		"a clean PATH must not produce shadow warnings")
}

// TestVersionChangeIsNamedAtSwitch asserts the version-change messaging: an
// upgrade is a note, a downgrade a warning (rollback is documented but must
// never be silent), and a same-version rerun says neither.
func TestVersionChangeIsNamedAtSwitch(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.2", false)
	writeRelease(s, "v1.2.3", false)
	writeRelease(s, "v2.0.0-rc.1", false)
	s.setLatest("v1.2.3")
	home := t.TempDir()

	_, errOut, code := runInstall(t, s, envHome(home), "--version", "v1.2.3")
	require.Equalf(t, 0, code, "stderr: %s", errOut)

	// Downgrade: stable to older stable.
	_, errOut, code = runInstall(t, s, envHome(home), "--version", "v1.2.2")
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, errOut, "downgrading from v1.2.3 to v1.2.2")

	// Upgrade across a prerelease boundary: rc orders above older stable.
	out, errOut, code := runInstall(t, s, envHome(home), "--version", "v2.0.0-rc.1")
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, out, "upgrading from v1.2.2 to v2.0.0-rc.1")

	// Downgrade within one version: rc back to... rc is highest prerelease;
	// stable v2.0.0 does not exist in the fixture, so cross back to stable 1.2.3
	// which orders BELOW 2.0.0-rc.1 (major wins over prerelease class).
	_, errOut, code = runInstall(t, s, envHome(home), "--version", "v1.2.3")
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.Contains(t, errOut, "downgrading from v2.0.0-rc.1 to v1.2.3")

	// Same-version rerun: repair, no change messaging.
	out, errOut, code = runInstall(t, s, envHome(home), "--version", "v1.2.3")
	require.Equalf(t, 0, code, "stderr: %s", errOut)
	assert.NotContains(t, out+errOut, "upgrading")
	assert.NotContains(t, out+errOut, "downgrading")
}

// TestShadowScanIgnoresConvenienceSymlink asserts the false-positive fix: a
// symlink elsewhere on PATH that points back at the installed binary runs
// the identical code - the scan must stay silent for absolute, relative,
// and chained (update-alternatives-style) links alike.
func TestShadowScanIgnoresConvenienceSymlink(t *testing.T) {
	t.Parallel()
	for name, link := range map[string]func(t *testing.T, home, dir string){
		"absolute target": func(t *testing.T, home, dir string) {
			t.Helper()
			require.NoError(t, os.Symlink(
				filepath.Join(home, ".local", "bin", "rancher"),
				filepath.Join(dir, "rancher")))
		},
		// Relative from home/convenience into home/.local/bin - the
		// ln -s ../bin/rancher style the canonicalization must resolve.
		"relative target": func(t *testing.T, home, dir string) {
			t.Helper()
			require.NoError(t, os.Symlink(
				filepath.Join("..", ".local", "bin", "rancher"),
				filepath.Join(dir, "rancher")))
		},
		// Two hops, both relative - the update-alternatives layout the
		// chain resolution must follow to the end.
		"chained target": func(t *testing.T, home, dir string) {
			t.Helper()
			require.NoError(t, os.Symlink(
				filepath.Join("..", ".local", "bin", "rancher"),
				filepath.Join(dir, "rancher-link")))
			require.NoError(t, os.Symlink("rancher-link",
				filepath.Join(dir, "rancher")))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t)
			writeRelease(s, "v1.2.3", false)
			s.setLatest("v1.2.3")
			home := t.TempDir()

			convenienceDir := filepath.Join(home, "convenience")
			require.NoError(t, os.MkdirAll(convenienceDir, 0o755))
			// Dangling at creation; resolves once the install lands the link.
			link(t, home, convenienceDir)

			env := append(envHome(home),
				"PATH="+convenienceDir+":"+toolPath(t, baseTools...))
			_, errOut, code := runInstall(t, s, env)
			require.Equalf(t, 0, code, "stderr: %s", errOut)
			assert.NotContains(t, errOut, "may shadow this install",
				"a symlink back at the installed binary is not a shadow")
		})
	}
}
