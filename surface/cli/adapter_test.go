package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

type pingCmd struct{}

func (pingCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "ping", CommandType: runtime.Local}
}
func (pingCmd) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return runtime.Result{Kind: "Pong", Data: map[string]string{"msg": "pong"}}, nil
}

func TestAdaptRunsAndRendersJSON(t *testing.T) {
	var out bytes.Buffer
	reg := runtime.NewRegistry(provider.NewResolver())
	root := &ucli.Command{
		Name:     "rancher",
		Writer:   &out,
		Flags:    GlobalFlags(),
		Commands: []*ucli.Command{Adapt(reg, pingCmd{})},
	}
	err := root.Run(context.Background(), []string{"rancher", "ping", "-o", "json"})
	require.NoError(t, err)

	var env map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.Equal(t, "Pong", env["kind"])
}

// runResolveDeps drives resolveDeps with a parsed --config flag, returning the
// resolved dir and error.
func runResolveDeps(t *testing.T, configFlag string) (string, error) {
	t.Helper()
	var (
		gotDir string
		gotErr error
	)
	cmd := &ucli.Command{
		Name:  "x",
		Flags: []ucli.Flag{&ucli.StringFlag{Name: "config"}},
		Action: func(_ context.Context, c *ucli.Command) error {
			gotDir, _, gotErr = resolveDeps(c)
			return nil
		},
	}
	argv := []string{"x"}
	if configFlag != "" {
		argv = append(argv, "--config", configFlag)
	}
	require.NoError(t, cmd.Run(context.Background(), argv))
	return gotDir, gotErr
}

func TestResolveDepsGuardsMigrationForOverriddenDir(t *testing.T) {
	t.Setenv("RANCHER_CREDENTIAL_STORE", "file")
	legacyDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(legacyDir, "cli2.json"), []byte("{}"), 0o600))
	t.Setenv("RANCHER_CONFIG_DIR", legacyDir) // makes FindLegacy discover the legacy file

	freshDir := t.TempDir()

	orig := logrus.StandardLogger().Out
	var buf bytes.Buffer
	logrus.SetOutput(&buf)
	defer logrus.SetOutput(orig)

	dir, err := runResolveDeps(t, freshDir)
	require.NoError(t, err)
	assert.Equal(t, freshDir, dir)
	_, statErr := os.Stat(filepath.Join(freshDir, "config.yaml"))
	assert.True(t, os.IsNotExist(statErr), "must not migrate into an explicitly overridden config dir")
	assert.Empty(t, buf.String(), "an explicitly overridden config dir must stay silent — no notice to corrupt a scripted session's stderr JSON")
}

func TestResolveDepsMigratesIntoDefaultDir(t *testing.T) {
	t.Setenv("RANCHER_CREDENTIAL_STORE", "file")
	t.Setenv("RANCHER_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".rancher"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".rancher", "cli2.json"), []byte("{}"), 0o600))

	dir, err := runResolveDeps(t, "")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".config", "rancher"), dir)
	_, statErr := os.Stat(filepath.Join(dir, "config.yaml"))
	require.NoError(t, statErr, "migration must create config.yaml in the default dir")
}
