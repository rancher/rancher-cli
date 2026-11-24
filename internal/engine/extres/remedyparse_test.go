package extres_test

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/extres"
	"github.com/rancher/rancher-cli/internal/testfs"
)

// TestTheLoginCommandOfTheKubeconfigRefusalParses asserts the command the
// refusal of create kubeconfig on an old server ends with, written as the
// invocation that prints it would, reaches auth login with the directory,
// the entry as its one argument and --token-stdin, also for an entry whose
// name starts with a dash.
func TestTheLoginCommandOfTheKubeconfigRefusalParses(t *testing.T) {
	testfs.SkipOnWindows(t, "the words are split by a POSIX shell, the one the remedies are quoted for")
	dir := t.TempDir()
	for _, entry := range []string{"prod", "-x", "my login"} {
		t.Run(entry, func(t *testing.T) {
			message := extres.KubeconfigRefusalMessage(t, entry)
			_, command, found := strings.Cut(message, "pipe id:secret to ")
			require.True(t, found, message)
			require.True(t, strings.HasPrefix(command, clierr.Rancher+" auth login "), command)

			text := clierr.ExpandRemedies(command, " --config "+clierr.ShellQuote(dir))
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", `printf '%s\n' `+text)
			cmd.Env = testfs.EnvWithoutGit()
			out, err := cmd.Output()
			require.NoError(t, err, text)
			words := strings.Split(strings.TrimRight(string(out), "\n"), "\n")

			root, err := cli.BuildRoot("v-test")
			require.NoError(t, err)
			var stderr bytes.Buffer
			root.ErrWriter = &stderr
			var ran, configDir string
			var args []string
			var tokenStdin bool
			var record func(cmds []*cli.Command, prefix string)
			record = func(cmds []*cli.Command, prefix string) {
				for _, c := range cmds {
					name := strings.TrimSpace(prefix + " " + c.Name)
					if len(c.Commands) > 0 {
						record(c.Commands, name)
						continue
					}
					c.Action = func(_ context.Context, c *cli.Command, in *cli.Input) error {
						ran, configDir, args, tokenStdin = name, in.String("config"), in.Args(), in.Bool("token-stdin")
						return nil
					}
				}
			}
			record(root.Commands, "")
			require.NoError(t, root.Run(t.Context(), words), "stderr: %s", stderr.String())
			require.Empty(t, stderr.String())
			assert.Equal(t, "auth login", ran)
			assert.Equal(t, dir, configDir)
			assert.Equal(t, []string{entry}, args)
			assert.True(t, tokenStdin)
		})
	}
}
