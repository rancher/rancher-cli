package cli

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/testfs"
)

// TestShellQuoteArgsParsesAsArguments asserts the text clierr.ShellQuoteArgs
// writes is read by the real command tree as the command's flags and
// arguments: for a command with flags after its value, one with the value
// last and one with two values, each hostile value reaches the command as
// that argument, byte for byte (the separator ShellQuote removes aside).
func TestShellQuoteArgsParsesAsArguments(t *testing.T) {
	testfs.SkipOnWindows(t, "the words are split by a POSIX shell, the one the remedies are quoted for")
	hostile := []string{"x", "-h", "--", "--x", "-1", "-", " -h", "\u2063-h", "a b", "a'b", "multi\nline", ""}
	for _, tc := range []struct {
		name   string
		words  string
		flags  string
		setOn  []string
		values func(v string) []string
	}{
		{"a leaf with trailing flags", "config update-server", " --url https://a.invalid", []string{"--url"}, func(v string) []string { return []string{v} }},
		{"a leaf with the value last", "config delete-server", "", nil, func(v string) []string { return []string{v} }},
		{"a leaf with two values, the first hostile", "oidc-client rotate-secret", " --timeout 1m", []string{"--timeout"}, func(v string) []string { return []string{v, "client-secret-1"} }},
		{"a leaf with two values, the second hostile", "oidc-client rotate-secret", " --timeout 1m", []string{"--timeout"}, func(v string) []string { return []string{"my-app", v} }},
	} {
		for _, v := range hostile {
			t.Run(tc.name+" "+strings.NewReplacer("\n", `\n`, "\u2063", "separator").Replace(v), func(t *testing.T) {
				values := tc.values(v)
				line := tc.words + clierr.ShellQuoteArgs(tc.flags, values...)
				words := splitWithSh(t, line)
				ran, args, set := parseWithRecorder(t, words)
				assert.Equal(t, tc.words, ran, line)
				for _, flag := range tc.setOn {
					assert.True(t, set[flag], "%s is set in %q", flag, line)
				}
				want := make([]string, len(values))
				for i, x := range values {
					want[i] = strings.ReplaceAll(x, "\u2063", "")
				}
				assert.Equal(t, want, args, line)
			})
		}
	}
}

// splitWithSh splits line the way a POSIX shell does.
func splitWithSh(t *testing.T, line string) []string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", `printf '%s\0' `+line).Output()
	require.NoError(t, err, line)
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

// parseWithRecorder parses words with the real command tree, every command's
// action replaced by one that records what ran; nothing is executed.
func parseWithRecorder(t *testing.T, words []string) (ran string, args []string, set map[string]bool) {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	var stderr bytes.Buffer
	root.ErrWriter = &stderr
	set = map[string]bool{}
	var record func(cmds []*ucli.Command, prefix string)
	record = func(cmds []*ucli.Command, prefix string) {
		for _, c := range cmds {
			name := strings.TrimSpace(prefix + " " + c.Name)
			if len(c.Commands) > 0 {
				record(c.Commands, name)
				continue
			}
			c.Action = func(_ context.Context, c *ucli.Command) error {
				ran, args = name, c.Args().Slice()
				for _, f := range c.Flags {
					if n := f.Names()[0]; c.IsSet(n) {
						set["--"+n] = true
					}
				}
				return nil
			}
		}
	}
	record(root.Commands, "")
	argv := append([]string{"rancher", "--config", t.TempDir()}, words...)
	require.NoError(t, root.Run(t.Context(), argv), "stderr: %s", stderr.String())
	return ran, args, set
}
