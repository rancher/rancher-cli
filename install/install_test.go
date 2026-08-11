package install_test

import (
	"encoding/json"
	"os"
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
	// Tamper the CURRENT platform's CLI asset — derived, not hardcoded, so
	// the test exercises a mismatch on every supported platform (a fixed
	// list omitted linux-s390x and would pass untested there).
	s.setAsset("v1.2.3/rancher-"+runtime.GOOS+"-"+runtime.GOARCH, []byte("tampered"))
	_, errOut, code := runInstall(t, s, nil)
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "checksum verification failed")
}

// TestChecksumEntryPerAssetCompleteness pins that each asset needs exactly
// one SHA256SUMS entry: a missing helper entry, and a duplicated CLI entry
// standing in for it, must both be fatal — a combined-count check would
// accept the duplicate and leave the helper entirely unchecked.
func TestChecksumEntryPerAssetCompleteness(t *testing.T) {
	t.Parallel()
	plat := runtime.GOOS + "-" + runtime.GOARCH
	cli, helper := "rancher-"+plat, "rancher-credential-vault-"+plat
	mutations := map[string]func(sums string) string{
		"missing helper entry": func(sums string) string {
			var keep []string
			for _, l := range strings.Split(sums, "\n") {
				if !strings.HasSuffix(l, "  "+helper) {
					keep = append(keep, l)
				}
			}
			return strings.Join(keep, "\n")
		},
		"duplicate cli entry masking the helper": func(sums string) string {
			var out []string
			for _, l := range strings.Split(sums, "\n") {
				if strings.HasSuffix(l, "  "+helper) {
					continue
				}
				out = append(out, l)
				if strings.HasSuffix(l, "  "+cli) {
					out = append(out, l)
				}
			}
			return strings.Join(out, "\n")
		},
		"missing cli entry": func(sums string) string {
			var keep []string
			for _, l := range strings.Split(sums, "\n") {
				if !strings.HasSuffix(l, "  "+cli) {
					keep = append(keep, l)
				}
			}
			return strings.Join(keep, "\n")
		},
		"duplicate helper entry masking the cli": func(sums string) string {
			var out []string
			for _, l := range strings.Split(sums, "\n") {
				if strings.HasSuffix(l, "  "+cli) {
					continue
				}
				out = append(out, l)
				if strings.HasSuffix(l, "  "+helper) {
					out = append(out, l)
				}
			}
			return strings.Join(out, "\n")
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s := newStub(t)
			writeRelease(s, "v1.2.3", false)
			s.setLatest("v1.2.3")
			s.setAsset("v1.2.3/SHA256SUMS", []byte(mutate(string(s.asset("v1.2.3/SHA256SUMS")))))
			_, errOut, code := runInstall(t, s, nil)
			require.NotEqual(t, 0, code)
			assert.Contains(t, errOut, "exactly one entry")
		})
	}
}

func TestVerifyAttestationWithoutGhIsFatal(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	writeRelease(s, "v1.2.3", false)
	s.setLatest("v1.2.3")
	// PATH without gh (toolPath from the Task 3 harness): the capability
	// probe fails deterministically regardless of the host.
	_, errOut, code := runInstall(t, s, []string{"PATH=" + toolPath(t, baseTools...)}, "--verify-attestation")
	require.NotEqual(t, 0, code)
	assert.Contains(t, errOut, "no capable authenticated gh")
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
		"the receipt path must be REPLACED (no-follow), not written inside the symlinked directory")
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
	assert.Contains(t, errOut, "receipt skipped")
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
