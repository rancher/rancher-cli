package kubeconfig_test

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kubeconfig"
	rt "github.com/rancher/rancher-cli/internal/runtime"
)

// noOwnerCommandRE picks one command out of the notice: rancher, the
// directory, and the words up to where the sentence goes on.
var noOwnerCommandRE = regexp.MustCompile(`rancher --config '[^']*'(?: (?:[a-z0-9][a-z0-9.-]*|--[a-z-]+|'[^']*'))+`)

// TestNoOwnerNoticeRemediesRunInTheCommandTree asserts every command the
// notice for a login that records no owner names - for a user entry, for
// the exec plugin's own login entry, and for a user entry --name
// identifies whose login the plugin login entry stood in for - reaches a
// real command of the tree with --config naming the directory, and that
// they are the commands the notice means: a login of the user entry, or
// the plugin login entry's logout and removal, then the kubeconfig again.
func TestNoOwnerNoticeRemediesRunInTheCommandTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the words are split by a POSIX shell, the one the remedies are quoted for")
	}
	t.Setenv(config.EnvCredentialStore, "file")
	plugin := config.ExecLoginPrefix + "r.example.com"
	for _, tc := range []struct {
		name, identified, entry string
		plugin                  bool
		want                    []string
	}{
		{"a user entry", "", "prod", false, []string{"auth login", "kubeconfig"}},
		{"the exec plugin's own login entry", "", plugin, true, []string{"auth logout", "config delete-server", "kubeconfig"}},
		{"an identified user entry the plugin login entry stood in for", "prod", plugin, true, []string{"auth login", "kubeconfig"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.Config{Servers: map[string]config.Server{tc.entry: {URL: "https://r.example.com"}}}
			notice := rt.IO{RemedyDir: dir}.ExpandText(kubeconfig.NoOwnerNoticeForTest(cfg, tc.identified, tc.entry, "c-1", tc.plugin))
			var ran []string
			for _, line := range noOwnerCommandRE.FindAllString(notice, -1) {
				words := noOwnerShellWords(t, strings.TrimSuffix(line, " again"))
				name, configDir := runInTree(t, words)
				assert.Equal(t, dir, configDir, line)
				ran = append(ran, name)
			}
			assert.Equal(t, tc.want, ran, notice)
		})
	}
}

// noOwnerShellWords splits line the way a POSIX shell does.
func noOwnerShellWords(t *testing.T, line string) []string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", `printf '%s\n' `+line).Output()
	require.NoError(t, err, line)
	return slices.DeleteFunc(strings.Split(string(out), "\n"), func(s string) bool { return s == "" })
}

// runInTree parses words with the real command tree, every action
// replaced by one that records which command ran and its directory.
func runInTree(t *testing.T, words []string) (ran, configDir string) {
	t.Helper()
	root, err := cli.BuildRoot("v-test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	var stderr bytes.Buffer
	root.ErrWriter = &stderr
	t.Cleanup(func() { cli.CaptureArgv(nil) })
	var record func(cmds []*ucli.Command, prefix string)
	record = func(cmds []*ucli.Command, prefix string) {
		for _, c := range cmds {
			name := strings.TrimSpace(prefix + " " + c.Name)
			if len(c.Commands) > 0 {
				record(c.Commands, name)
				continue
			}
			c.Action = func(_ context.Context, c *ucli.Command) error {
				ran, configDir = name, c.String("config")
				return nil
			}
		}
	}
	record(root.Commands, "")
	// words start with the program name. An unknown command is reported
	// through the exit code, with no error.
	code := cli.Run(t.Context(), root, words)
	require.Zero(t, code, "%v exited %d: %s", words, code, stderr.String())
	require.Empty(t, stderr.String())
	return ran, configDir
}
