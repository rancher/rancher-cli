package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
)

// seedKeychainDir writes a directory that keeps its credentials in the
// keychain, with kc1 current and logged in, and points the CLI at it.
func seedKeychainDir(t *testing.T) string {
	t.Helper()
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	dir := t.TempDir()
	cfg := &config.Config{CredentialStore: "keychain", CurrentServer: "kc1",
		Servers: map[string]config.Server{"kc1": {URL: "https://kc1.ex"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	ks, _, err := config.OpenResolvedStore("keychain", dir)
	require.NoError(t, err)
	require.NoError(t, ks.SetToken(config.ServerRef{Name: "kc1", URL: "https://kc1.ex"}, "kc1-tok"))
	t.Setenv(config.EnvConfigDir, dir)
	return dir
}

// runRootJSON runs the real CLI with -o json and returns the exit status
// and the error envelope it printed, if any.
func runRootJSON(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	root.Reader = strings.NewReader("")
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	runErr := root.Run(ctx, append(append([]string{"rancher"}, args...), "-o", "json"))
	return exitCodeOf(runErr), out.String(), errOut.String()
}

// TestCommandsFailAsCredentialStoreUnavailable asserts, through the real
// CLI, that each command reaching the keychain or a credential helper
// fails as CREDENTIAL_STORE_UNAVAILABLE when the store cannot be used:
// the envelope an agent decodes - code, exit 14, retryable, no hint - and
// a message naming the store, the entry, the cause and the remedy. The
// keychain fails the way go-keyring reports a locked macOS keychain
// whose prompt was cancelled; the helper exits non-zero with its own
// message.
func TestCommandsFailAsCredentialStoreUnavailable(t *testing.T) {
	keychainCause := "exit status 128"
	keychainRemedy := "then run this command again"
	if runtime.GOOS == "darwin" {
		keychainCause = "the keychain is locked or its prompt was cancelled (security exited with status 128)"
		keychainRemedy = "unlock the keychain with security unlock-keychain, or approve the macOS prompt, then run this command again"
	}
	for _, tc := range []struct {
		name string
		// helper seeds an exec:fake directory instead of a keychain one.
		helper bool
		// read is a command that only reads, so config.yaml must be
		// byte-for-byte as it was; a delete advances the entry's cache
		// binding before it reaches the store, and says so.
		read bool
		fail cmdtest.FailingKeychain
		args []string
		want []string
	}{
		{name: "auth logout, the keychain read", fail: cmdtest.FailingKeychain{FailGet: true},
			args: []string{"auth", "logout", "kc1"},
			want: []string{`failed to log out "kc1"`, "the keychain could not read the credential for 'kc1'", keychainCause, keychainRemedy}},
		{name: "auth logout, the keychain delete", fail: cmdtest.FailingKeychain{FailDelete: true},
			args: []string{"auth", "logout", "kc1"},
			want: []string{"the keychain could not delete the credential for 'kc1'", keychainCause, keychainRemedy}},
		{name: "auth logout --all", fail: cmdtest.FailingKeychain{FailGet: true},
			args: []string{"auth", "logout", "--all"},
			want: []string{"the keychain could not read the credential for 'kc1'", keychainCause, keychainRemedy}},
		{name: "config delete-server", fail: cmdtest.FailingKeychain{FailGet: true},
			args: []string{"config", "delete-server", "kc1"},
			want: []string{"the keychain could not read the credential for 'kc1'", keychainCause, keychainRemedy}},
		{name: "auth status, the stored-login read", read: true, fail: cmdtest.FailingKeychain{FailGet: true},
			args: []string{"auth", "status"},
			want: []string{"the keychain could not read the credential for 'kc1'", keychainCause, keychainRemedy}},
		{name: "auth get-token, the exec plugin's stored-login read", read: true, fail: cmdtest.FailingKeychain{FailGet: true},
			args: []string{"auth", "get-token", "--server", "https://kc1.ex", "--cluster", "c-1", "--name", "kc1"},
			want: []string{"the keychain could not read the credential for 'kc1'", keychainCause, keychainRemedy}},
		{name: "auth status, a credential helper that fails", helper: true, read: true,
			args: []string{"auth", "status"},
			want: []string{"credential helper rancher-credential-fake could not read the credential for 'kc1': fakehelper: deliberate failure"}},
		{name: "auth logout, a credential helper that fails", helper: true,
			args: []string{"auth", "logout", "kc1"},
			want: []string{"credential helper rancher-credential-fake could not erase the credential for 'kc1': fakehelper: deliberate failure"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dir string
			if tc.helper {
				dir, _ = cmdtest.SeedExecLogout(t, "kc1", "kc1")
				t.Setenv(config.EnvConfigDir, dir)
				t.Setenv("FAKEHELPER_MODE", "fail")
			} else {
				dir = seedKeychainDir(t)
				fail := tc.fail
				fail.Err = cmdtest.SecurityExitError(t, 128)
				t.Cleanup(config.SetKeyringBackendForTest(fail))
			}
			before, err := os.ReadFile(config.FilePath(dir))
			require.NoError(t, err)

			exit, stdout, stderr := runRootJSON(t, tc.args...)
			assert.Equal(t, 14, exit, "stdout: %s\nstderr: %s", stdout, stderr)
			got := cmdtest.DecodeErrorEnvelope(t, []byte(strings.TrimSpace(stderr)))
			assert.Equal(t, clierr.CodeCredentialStoreUnavailable, got.Code)
			assert.True(t, got.Retryable)
			assert.Empty(t, got.Hint)
			for _, w := range tc.want {
				assert.Contains(t, got.Message, w)
			}
			if tc.read {
				after, rerr := os.ReadFile(config.FilePath(dir))
				require.NoError(t, rerr)
				assert.Equal(t, string(before), string(after), "the directory is left as it was")
			}
		})
	}
}

// TestLogoutOutsideTheKeychainStoreStaysResilient asserts the one delete
// site that keeps its resilient path: a directory whose store is the
// file, with a leftover keychain identity record, logs out of the file
// and only warns that the keychain could not be reached.
func TestLogoutOutsideTheKeychainStoreStaysResilient(t *testing.T) {
	dir := seedKeychainDir(t)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.CredentialStore = "file"
	require.NoError(t, cfg.Write())
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "kc1", URL: "https://kc1.ex"}, "file-tok"))
	t.Cleanup(config.SetKeyringBackendForTest(cmdtest.FailingKeychain{Err: errors.New("the keychain is not reachable"), FailGet: true}))

	exit, stdout, stderr := runRootJSON(t, "auth", "logout", "kc1")
	require.Equal(t, 0, exit, "stdout: %s\nstderr: %s", stdout, stderr)
	var env struct {
		Kind     string   `json:"kind"`
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &env), stdout)
	assert.NotEqual(t, "Error", env.Kind)
	require.Len(t, env.Warnings, 1, stdout)
	assert.Contains(t, env.Warnings[0], "keychain unreachable, the credential identity record was kept")
	_, terr := config.NewFileStore(dir).GetToken(config.ServerRef{Name: "kc1"})
	assert.ErrorIs(t, terr, config.ErrNoToken, "the file credential is gone")
}
