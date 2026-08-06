package cli

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompletionCommandRegisteredWithShellCompletionField pins that
// `completion` is registry-wired (reg.Register before reg.Validate, not
// Adapt alone) and that the manifest's shellCompletion capability field is
// always present.
func TestCompletionCommandRegisteredWithShellCompletionField(t *testing.T) {
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	assert.Equal(t, []string{"bash", "zsh", "fish", "pwsh"}, m.ShellCompletion)

	var names []string
	for _, c := range m.Commands {
		names = append(names, c.Name)
	}
	assert.Contains(t, names, "completion")
}

// TestCompletionEmissionHasNoSideEffects is the external-review no-
// side-effects regression: emission must never touch config/store/pin. It
// deliberately does NOT force RANCHER_CLI_CREDENTIAL_STORE=file like every
// other test in this package — leaving the store policy at its "auto"
// default is what would expose a rewire onto adaptEntry (which calls
// resolveDeps -> OpenStore, and OpenStore's auto path writes credstore-pin
// on first use). A fresh, empty config dir must remain byte-for-byte empty
// after `completion bash` runs.
func TestCompletionEmissionHasNoSideEffects(t *testing.T) {
	t.Setenv(config.EnvCredentialStore, "")
	dir := t.TempDir()

	before, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, before, "the fresh config dir must start empty")

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(context.Background(), []string{"rancher", "--config", dir, "completion", "bash"}))
	require.NotEmpty(t, out.String(), "the script must still have been emitted")

	after, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, after, "completion emission must create nothing in the config dir: no config.yaml, no credstore-pin")

	_, statErr := os.Stat(config.FilePath(dir))
	assert.True(t, os.IsNotExist(statErr), "config.yaml must not be created by completion emission")
	_, pinErr := os.Stat(config.PinPath(dir))
	assert.True(t, os.IsNotExist(pinErr), "credstore-pin must not be created by completion emission (would indicate a resolveDeps/OpenStore path)")
}

// TestCompletionArgsUsageRendersEnumValues pins that the completion command's
// positional arg, which carries an Enum, renders as "<bash|zsh|fish|pwsh>"
// rather than the bare "<shell>" name in the urfave ArgsUsage field.
func TestCompletionArgsUsageRendersEnumValues(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	cmd := root.Command("completion")
	require.NotNil(t, cmd, "completion command must be registered")
	assert.Equal(t, "<bash|zsh|fish|pwsh>", cmd.ArgsUsage)
}

// TestCompletionSelfCompletesShellArgument pins spec SC-2's self-completion
// requirement: `completion`'s own --generate-shell-completion lists the same
// shells the command accepts, via the same Adapt-level ShellCompleter wiring
// Task 3 mirrors for adaptEntry.
func TestCompletionSelfCompletesShellArgument(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(context.Background(), []string{"rancher", "--config", dir, "completion", "--generate-shell-completion"}))
	assert.Equal(t, "bash\nfish\npwsh\nzsh\n", out.String())
}
