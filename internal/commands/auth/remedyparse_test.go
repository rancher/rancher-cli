package auth_test

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/auth"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testfs"
)

// parsedCommand is what the command tree made of one remedy command.
type parsedCommand struct {
	ran       string
	configDir string
	args      []string
}

// parseRemedyCommand writes command as the invocation that prints it would,
// naming dir, splits it the way a POSIX shell does and parses it with the
// real command tree, every command's action replaced by one that records
// what ran, so nothing is executed.
func parseRemedyCommand(t *testing.T, dir, command string) parsedCommand {
	t.Helper()
	text := clierr.ExpandRemedies(command, " --config "+clierr.ShellQuote(dir))
	text = strings.NewReplacer("<file>", "/etc/ca.pem", "<name>", "x", "<url>", "https://r.example.com", "<cluster-id>", "c-1").Replace(text)
	out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", `printf '%s\n' `+text).Output()
	require.NoError(t, err, text)
	words := strings.Split(strings.TrimRight(string(out), "\n"), "\n")

	root, err := cli.BuildRoot("v-test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	var stderr bytes.Buffer
	root.ErrWriter = &stderr
	var got parsedCommand
	var record func(cmds []*ucli.Command, prefix string)
	record = func(cmds []*ucli.Command, prefix string) {
		for _, c := range cmds {
			name := strings.TrimSpace(prefix + " " + c.Name)
			if len(c.Commands) > 0 {
				record(c.Commands, name)
				continue
			}
			c.Action = func(_ context.Context, c *ucli.Command) error {
				got = parsedCommand{ran: name, configDir: c.String("config"), args: c.Args().Slice()}
				return nil
			}
		}
	}
	record(root.Commands, "")
	require.NoError(t, root.Run(t.Context(), words), "stderr: %s", stderr.String())
	require.Empty(t, stderr.String())
	return got
}

// TestRemedyCommandsOfADashedEntryNameParse asserts the command each login
// path writes for an entry whose name starts with a dash reaches the command
// it names, with the directory and with the name as its one argument.
func TestRemedyCommandsOfADashedEntryNameParse(t *testing.T) {
	testfs.SkipOnWindows(t, "the words are split by a POSIX shell, the one the remedies are quoted for")
	dir := t.TempDir()
	rows := auth.DashedNameRemedies(t)
	require.NotEmpty(t, rows)
	for _, row := range rows {
		t.Run(row.Site, func(t *testing.T) {
			got := parseRemedyCommand(t, dir, row.Command)
			words := strings.Fields(clierr.ExpandRemedies(row.Command, ""))
			assert.Equal(t, words[1]+" "+words[2], got.ran)
			assert.Equal(t, dir, got.configDir)
			assert.Equal(t, []string{"-x"}, got.args)
		})
	}
}

// TestRemoveCommandsOfAStoreChangeRefusalParse asserts the commands that
// remove the other entries, which a refused store change names one per entry
// and for a dashed name after "--", each reach config delete-server with the
// entry as its argument, whether the credential helper is the source or the
// destination of the change.
func TestRemoveCommandsOfAStoreChangeRefusalParse(t *testing.T) {
	testfs.SkipOnWindows(t, "the words are split by a POSIX shell, the one the remedies are quoted for")
	dir := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	cfg := &config.Config{Servers: map[string]config.Server{
		"prod": {URL: "https://r.ex"},
		"-x":   {URL: "https://x.ex"},
		"qa":   {URL: "https://q.ex"},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	for name, err := range map[string]error{
		"the helper is the source":      config.CheckTransitionSources(dir, cfg, "prod", "exec:fake", "keychain"),
		"the helper is the destination": config.CheckTransitionSources(dir, cfg, "prod", "file", "exec:fake"),
	} {
		t.Run(name, func(t *testing.T) {
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			_, list, found := strings.Cut(ce.Message, "or remove them with: ")
			require.True(t, found, ce.Message)
			list, _, found = strings.Cut(list, "; then run this command again")
			require.True(t, found, ce.Message)
			commands := strings.Split(list, "; ")
			require.Len(t, commands, 2)
			for i, want := range []string{"-x", "qa"} {
				got := parseRemedyCommand(t, dir, commands[i])
				assert.Equal(t, "config delete-server", got.ran)
				assert.Equal(t, dir, got.configDir)
				assert.Equal(t, []string{want}, got.args)
			}
		})
	}
}

// TestFixedRemedyCommandsParse asserts each command a login, logout or
// status path names with a placeholder or with no argument ends its clause
// and reaches the command it names with the directory and the arguments the
// placeholders stand for, and the commands that the credential store texts
// are built on parse the same way.
func TestFixedRemedyCommandsParse(t *testing.T) {
	testfs.SkipOnWindows(t, "the words are split by a POSIX shell, the one the remedies are quoted for")
	dir := t.TempDir()
	check := func(t *testing.T, command string, want []string) {
		t.Helper()
		got := parseRemedyCommand(t, dir, command)
		words := strings.Fields(clierr.ExpandRemedies(command, ""))
		assert.Equal(t, words[1]+" "+words[2], got.ran)
		assert.Equal(t, dir, got.configDir)
		assert.ElementsMatch(t, want, got.args)
	}
	for _, row := range auth.FixedCommandRemedies(t) {
		t.Run(row.Site, func(t *testing.T) {
			for _, c := range row.Commands {
				_, after, found := strings.Cut(row.Text, c.Command)
				require.True(t, found, "%q is not in %q", c.Command, row.Text)
				assert.True(t, after == "" || strings.HasPrefix(after, ";"), "the command does not end its clause: %q", after)
				check(t, c.Command, c.Args)
			}
		})
	}
	t.Run("commands the store texts are built on", func(t *testing.T) {
		for _, tc := range []struct {
			command string
			args    []string
		}{
			{config.StatusCommand, nil},
			{config.TokenReuseOffCommand, []string{"off"}},
			{config.LoginCommand, nil},
			{config.DeleteServerCommand + " 'x'", []string{"x"}},
			{provider.InspectClusterRemedyFor("c-1"), []string{"c-1"}},
			{clierr.Rancher + " auth list-tokens", nil},
		} {
			check(t, tc.command, tc.args)
		}
	})
}
