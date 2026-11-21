package conformance

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testfs"
)

// TestAnErrorTextWithAHiddenCharacterRunsWhenPastedIntoAShell asserts, for a
// message, a hint, a warning and a details value, that the command a failed
// run prints after a piece that has a hidden character runs when pasted into
// a POSIX shell, with the directory as one argument, and that an argument the
// CLI quoted never runs as a command. Two texts are planted: one with the
// hidden character before the command, one with it in the same piece as the
// argument.
func TestAnErrorTextWithAHiddenCharacterRunsWhenPastedIntoAShell(t *testing.T) {
	t.Parallel()
	if testfs.OnWindows() {
		t.Skip("the row needs a POSIX shell and a copy of the test binary run as rancher")
	}
	program, lookErr := exec.LookPath("sh")
	sh := posixShell(pasteShellOrSkip(t, "sh", program, lookErr))
	pasteDir := newPasteDir(t)
	sentinel := filepath.Join(pasteDir, "sentinel")
	injection := posixInjection(sentinel)
	configDir := filepath.Join(t.TempDir(), "o brien")

	plants := []struct {
		name string
		text string
		want []string
	}{
		{"hidden character before the command", "x\u200b run " + clierr.Rancher + " get " + clierr.ShellQuote(injection),
			[]string{"--config", configDir, "get", injection}},
		{"hidden character in the clause of the argument", clierr.Rancher + " get " + clierr.ShellQuote(injection) + " x\u200b",
			[]string{"--config", configDir, "get", injection + ` x\u200b`}},
	}
	channels := []struct {
		name  string
		build func(text string) *clierr.Error
	}{
		{"message", func(s string) *clierr.Error { return clierr.New(clierr.CodeGeneric, s) }},
		{"hint", func(s string) *clierr.Error { return clierr.New(clierr.CodeGeneric, "failed").WithHint(s) }},
		{"warning", func(s string) *clierr.Error { return clierr.New(clierr.CodeGeneric, "failed").WithWarnings(s) }},
		{"details value", func(s string) *clierr.Error {
			return clierr.New(clierr.CodeGeneric, "failed").WithDetails(clierr.CommittedContextDetails{Recovery: s})
		}},
	}
	for _, channel := range channels {
		for _, plant := range plants {
			t.Run(channel.name+" "+plant.name, func(t *testing.T) {
				e := newTerminalEnv(t)
				_, errOut, ce := e.runFailing(terminalRead{d: terminalDescriptor(runtime.ShapeText), err: channel.build(plant.text)},
					func(io *runtime.IO) { io.RemedyDir = configDir })
				require.NotNil(t, ce)
				var line string
				for l := range strings.SplitSeq(errOut, "\n") {
					if _, after, found := strings.Cut(l, "rancher --config"); found {
						line = "rancher --config" + after
						break
					}
				}
				require.NotEmpty(t, line, "no printed command in %q", errOut)
				run := runPaste(t, sh, pasteDir, line)
				require.NoError(t, run.err, line)
				assert.Equal(t, plant.want, run.args, line)
				assert.NoFileExists(t, sentinel, line)
			})
		}
	}
}
