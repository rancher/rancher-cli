package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/testfs"
)

// runShim runs the real command tree with args and returns what it wrote to
// stdout and stderr and its exit code.
func runShim(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	runErr := root.Run(t.Context(), append([]string{"rancher"}, args...))
	if runErr != nil {
		code = 1
		if coder, ok := runErr.(ucli.ExitCoder); ok {
			code = coder.ExitCode()
		}
	}
	return out.String(), errOut.String(), code
}

// TestTheLegacyTokenCommandWithAnUnresolvableDirectoryKeepsItsOutput asserts
// a config directory that cannot be resolved (no flag, no variable and no
// home directory) leaves the legacy token command's stdout empty and its exit
// code 1, with the error on stderr, in text and in json. The literals are what
// the command wrote before it named its directory in its remedies.
func TestTheLegacyTokenCommandWithAnUnresolvableDirectoryKeepsItsOutput(t *testing.T) {
	testfs.SkipOnWindows(t, "the home directory is read from USERPROFILE there")
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(config.EnvConfigDir, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	base := []string{"token", "--server", "https://r.example.com", "--user", "alice"}

	stdout, stderr, code := runShim(t, base...)
	assert.Empty(t, stdout)
	assert.Equal(t, "error (GENERIC): $HOME is not defined\n", stderr)
	assert.Equal(t, 1, code)

	stdout, stderr, code = runShim(t, append(base, "-o", "json")...)
	assert.Empty(t, stdout)
	assert.Equal(t, 1, code)
	var env struct {
		Data struct {
			Error struct{ Code, Message string } `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stderr), &env))
	assert.Equal(t, "GENERIC", env.Data.Error.Code)
	assert.Equal(t, "$HOME is not defined", env.Data.Error.Message)
}

// directoryDigest maps each file below dir to the hash of its contents.
func directoryDigest(t *testing.T, dir string) map[string][sha256.Size]byte {
	t.Helper()
	out := map[string][sha256.Size]byte{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = sha256.Sum256(b)
		return nil
	}))
	return out
}

// TestTheLegacyTokenCommandServesACachedCredentialOpeningNoStore asserts a
// cache hit of the legacy token command, with a credential-store setting
// that no store accepts, still serves the cached credential: nothing on the
// path opens or resolves a store; that the run reads and writes the files of
// the directory as before (config.yaml loaded once by the invocation and the
// binding stamp read twice, no keychain operation); and that it leaves every
// file of the directory as it was.
func TestTheLegacyTokenCommandServesACachedCredentialOpeningNoStore(t *testing.T) {
	dir := recordingEnv(t)
	srv := newFactsServer(t)
	seedCachedHit(t, dir, srv.URL)
	// Recording writes telemetry files into the directory, which the digest
	// check below would count as a change.
	t.Setenv(telemetry.EnvTelemetry, "false")
	t.Setenv(config.EnvCredentialStore, "not-a-store")
	before := directoryDigest(t, dir)
	var identityOps, keyringOps []string
	restore := config.SetIdentityObserverForTest(func(op string) { identityOps = append(identityOps, op) })
	t.Cleanup(restore)
	config.SetKeyringObserverForTest(func(op string) { keyringOps = append(keyringOps, op) })
	t.Cleanup(func() { config.SetKeyringObserverForTest(nil) })

	got := runRecorded(t, newRoot(t), "token", "--server", srv.URL, "--user", "kube", "--cluster", "c-1")

	require.Zero(t, got.res.code, got.stderr)
	var credential struct {
		Kind   string
		Status struct{ Token string }
	}
	require.NoError(t, json.Unmarshal([]byte(got.stdout), &credential))
	assert.Equal(t, "ExecCredential", credential.Kind)
	assert.Equal(t, "cached-tok", credential.Status.Token)
	assert.Equal(t, []string{"stampread", "configload", "stampread"}, identityOps)
	assert.Empty(t, keyringOps)
	assert.Equal(t, before, directoryDigest(t, dir))
}
