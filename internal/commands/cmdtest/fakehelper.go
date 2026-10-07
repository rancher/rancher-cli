package cmdtest

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/config"
)

var (
	cmdFakeHelperOnce sync.Once
	cmdFakeHelperDir  string
	cmdFakeHelperErr  error
	// cmdFakeHelperRoot is the directory BuildFakeHelper created, kept even
	// when the build fails so IsolateConfigDir's cleanup removes it.
	cmdFakeHelperRoot string
)

// firstRunLimit bounds the fake helper's first run, so a hung helper fails
// the package with a message.
const firstRunLimit = 5 * time.Minute

// BuildFakeHelper compiles the config package's fake credential helper
// once per test binary and returns the directory holding
// rancher-credential-fake.
func BuildFakeHelper(t *testing.T) string {
	t.Helper()
	cmdFakeHelperOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakehelper-commands") //nolint:usetesting // shared across the whole test binary via sync.Once; t.TempDir() would bind its lifetime to the first caller's test
		if err != nil {
			cmdFakeHelperErr = err
			return
		}
		cmdFakeHelperRoot = dir
		bin := filepath.Join(dir, "rancher-credential-fake")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		out, err := exec.Command("go", "build", "-o", bin,
			"github.com/rancher/rancher-cli/internal/config/testdata/fakehelper").CombinedOutput()
		if err != nil {
			cmdFakeHelperErr = err
			_ = out // surfaced via require below with the message
			cmdFakeHelperDir = string(out)
			return
		}
		// A new binary's first run can take many seconds under load; run it
		// here, outside any helper time limit.
		ctx, cancel := context.WithTimeout(t.Context(), firstRunLimit)
		defer cancel()
		if out, err := exec.CommandContext(ctx, bin, "noop").CombinedOutput(); err != nil {
			if ctx.Err() != nil {
				err = errors.New("did not finish within " + firstRunLimit.String())
			}
			cmdFakeHelperErr = err
			cmdFakeHelperDir = "first run of " + bin + ": " + string(out)
			return
		}
		cmdFakeHelperDir = dir
	})
	require.NoError(t, cmdFakeHelperErr, "building fake helper: %s", cmdFakeHelperDir)
	return cmdFakeHelperDir
}

// SeedExecLogout writes a config declaring the exec:fake credential store
// with one token stored through the fake helper per server, and puts the
// helper on PATH. Returns the config dir and the helper's state-file path.
func SeedExecLogout(t *testing.T, current string, servers ...string) (string, string) {
	t.Helper()
	t.Setenv("PATH", BuildFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEHELPER_MODE", "ok")
	statePath := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("FAKEHELPER_STATE", statePath)
	keyring.MockInit()

	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer:   current,
		CredentialStore: "exec:fake",
		Servers:         map[string]config.Server{},
	}
	st, err := config.OpenStore("exec:fake", dir)
	require.NoError(t, err)
	for _, s := range servers {
		url := "https://" + s + ".ex"
		cfg.Servers[s] = config.Server{URL: url}
		require.NoError(t, st.SetToken(config.ServerRef{Name: s, URL: url}, "id:sec"))
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir, statePath
}

// HelperCreds decodes the fake helper's stored credential map.
func HelperCreds(t *testing.T, statePath string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(statePath)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}
	}
	require.NoError(t, err)
	var s struct {
		Creds map[string]string `json:"creds"`
	}
	require.NoError(t, json.Unmarshal(b, &s))
	return s.Creds
}
