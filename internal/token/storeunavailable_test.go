package token

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	goruntime "runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// lockedKeychain is a keyring seam whose writes fail the way go-keyring
// reports a locked macOS keychain whose prompt was cancelled; reads go to
// go-keyring's in-memory mock.
type lockedKeychain struct{ err error }

func (k lockedKeychain) Set(string, string, string) error { return k.err }
func (k lockedKeychain) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}
func (k lockedKeychain) Delete(service, user string) error { return keyring.Delete(service, user) }

// TestPluginFirstLoginOnALockedKeychainRefuses asserts the exec plugin's
// own first login on an `auto` directory with nothing stored, when the
// keychain probe gets security's status 128: on macOS the envelope is
// CREDENTIAL_STORE_UNAVAILABLE, exit 14, retryable, with no hint, its
// remedy is unlocking the keychain or approving the prompt and running
// again, and it names no `--credential-store file` command, which neither
// this command nor `auth login` on the plugin's entry name accepts. No
// one is asked to sign in and nothing is pinned. Elsewhere the status
// means nothing and the login is not refused for it.
// Not parallel: it swaps the package-global keyring seam.
func TestPluginFirstLoginOnALockedKeychainRefuses(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("needs a POSIX shell to produce an exit status")
	}
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	config.ResetKeyringUsableCacheForTest(t)
	x := newPluginFixture(t, true, testrancher.Options{})
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(t, err)
	cfg.CredentialStore = ""
	require.NoError(t, cfg.Write())
	locked := exec.Command("/bin/sh", "-c", "exit 128").Run()
	t.Cleanup(config.SetKeyringBackendForTest(lockedKeychain{err: locked}))

	_, err = x.getToken(t, "c-test", "n", localPrompter())
	ce, ok := errors.AsType[*clierr.Error](err)
	if goruntime.GOOS != "darwin" {
		if ok {
			assert.NotEqual(t, clierr.CodeCredentialStoreUnavailable, ce.Code)
		}
		return
	}
	require.True(t, ok, "%v", err)
	var buf bytes.Buffer
	require.NoError(t, runtime.RenderError(&buf, "json", ce))
	var env struct {
		Data struct {
			Error struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				Hint      string `json:"hint"`
				Retryable bool   `json:"retryable"`
			} `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	got := env.Data.Error
	assert.Equal(t, clierr.CodeCredentialStoreUnavailable, got.Code)
	assert.Equal(t, 14, ce.ExitCode())
	assert.True(t, got.Retryable)
	assert.Empty(t, got.Hint)
	assert.Contains(t, got.Message, "the keychain is locked or its prompt was cancelled (security exited with status 128); unlock the keychain with security unlock-keychain, or approve the macOS prompt, then run this command again")
	assert.NotContains(t, got.Message, "--credential-store")
	assert.Zero(t, x.f.LoginCount(), "nobody was asked to sign in")
	assert.Empty(t, config.ReadPin(x.dir), "nothing was pinned")
}
