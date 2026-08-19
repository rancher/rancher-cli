package commands

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/config"
)

var (
	cmdFakeHelperOnce sync.Once
	cmdFakeHelperDir  string
	cmdFakeHelperErr  error
)

// buildCommandsFakeHelper compiles the config package's fake helper once per
// test binary and returns the directory holding rancher-credential-fake.
func buildCommandsFakeHelper(t *testing.T) string {
	t.Helper()
	cmdFakeHelperOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakehelper-commands") //nolint:usetesting // shared across the whole test binary via sync.Once; t.TempDir() would bind its lifetime to the first caller's test
		if err != nil {
			cmdFakeHelperErr = err
			return
		}
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
		cmdFakeHelperDir = dir
	})
	require.NoError(t, cmdFakeHelperErr, "building fake helper: %s", cmdFakeHelperDir)
	return cmdFakeHelperDir
}

// seedExecLogout mirrors seedLogout for the exec policy: fake helper on
// PATH, config declaring exec:fake, one token stored through the helper per
// server. Returns dir + the helper state-file path.
func seedExecLogout(t *testing.T, current string, servers ...string) (string, string) {
	t.Helper()
	t.Setenv("PATH", buildCommandsFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
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

// helperCreds decodes the fake helper's stored credential map.
func helperCreds(t *testing.T, statePath string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		return map[string]string{}
	}
	require.NoError(t, err)
	var s struct {
		Creds map[string]string `json:"creds"`
	}
	require.NoError(t, json.Unmarshal(b, &s))
	return s.Creds
}
