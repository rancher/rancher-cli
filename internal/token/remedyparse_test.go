package token_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	rt "github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
)

// remedyCommandRE picks one command out of a remedy text: rancher, the
// directory, and the words that follow up to the end of the command.
var remedyCommandRE = regexp.MustCompile(`rancher --config '[^']*'(?: (?:[a-z0-9][a-z0-9.-]*|--[a-z-]+|'(?:[^']|'\\'')*'|<[a-z-]+>|https?://[^\s,;)']+))+`)

// proseWords are the words where a command ends and the sentence around it
// goes on.
var proseWords = map[string]bool{"for": true, "and": true, "then": true, "with": true, "to": true, "if": true, "which": true, "or": true, "first": true}

// remedyCommands is every command in text, cut where the sentence goes on.
func remedyCommands(text string) []string {
	var out []string
	for _, m := range remedyCommandRE.FindAllString(text, -1) {
		words := strings.Split(m, " ")
		for i, w := range words {
			if proseWords[w] {
				words = words[:i]
				break
			}
		}
		out = append(out, strings.Join(words, " "))
	}
	return out
}

// placeholderValues fill the placeholders a remedy names for a value the
// operator chooses.
var placeholderValues = strings.NewReplacer("<entry>", "x", "<name>", "x", "<id>", "c-1", "<cluster>", "c-1",
	"<cluster-id>", "c-1", "<url>", "https://r.example.com")

// manifestCommand is what the schema manifest says about one command.
type manifestCommand struct {
	Name        string `json:"name"`
	CommandType string `json:"commandType"`
	Destructive string `json:"destructive"`
	Flags       []struct {
		Name     string `json:"name"`
		Required bool   `json:"required"`
	} `json:"flags"`
}

// TestExecPluginRemediesParseAndPassTheGates asserts every command in the
// exec plugin's remedies, as the operator reads it, is one the command
// tree accepts: it reaches a real command, --config names the directory,
// every required flag is there, and a mutation carries the gates it needs
// to run rather than preview (--apply, and --yes when destructive).
func TestExecPluginRemediesParseAndPassTheGates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the words are split by a POSIX shell, the one the remedies are quoted for")
	}
	t.Setenv(config.EnvCredentialStore, "file")
	dir := t.TempDir()
	manifest := schemaCommands(t, dir)

	var commands []string
	for _, text := range token.ExecPluginRemedyTextsForTest(t) {
		expanded := rt.IO{RemedyDir: dir}.ExpandText(text)
		commands = append(commands, remedyCommands(expanded)...)
	}
	slices.Sort(commands)
	commands = slices.Compact(commands)
	require.NotEmpty(t, commands)

	seen := map[string]bool{}
	for _, line := range commands {
		t.Run(line, func(t *testing.T) {
			words := shellWords(t, placeholderValues.Replace(line))
			ran, configDir, setFlags := parseWithTree(t, words)
			require.NotEmpty(t, ran, "no command ran for %q", line)
			assert.Equal(t, dir, configDir)
			mc, ok := manifest[ran]
			require.True(t, ok, "%s is not in the manifest", ran)
			seen[ran] = true
			for _, f := range mc.Flags {
				if f.Required {
					assert.True(t, setFlags["--"+f.Name], "%s needs --%s", ran, f.Name)
				}
			}
			if mc.CommandType == "mutating" {
				assert.Contains(t, words, "--apply", "a remedy runs the change, it does not preview it")
				if mc.Destructive == "true" {
					assert.Contains(t, words, "--yes", "%s is destructive", ran)
				}
			}
		})
	}
	for _, want := range []string{"auth login", "auth logout", "auth status", "config use-server", "config delete-server", "config status",
		"config set-token-reuse", "kubeconfig", "create kubeconfig", "inspect cluster", "get cluster", "get pod"} {
		assert.True(t, seen[want], "no remedy runs %s", want)
	}
}

// schemaCommands reads the manifest from the real command tree.
func schemaCommands(t *testing.T, dir string) map[string]manifestCommand {
	t.Helper()
	root, err := cli.BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--config", dir, "schema", "--all", "-o", "json"}))
	var env struct {
		Data struct {
			Commands []manifestCommand `json:"commands"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	m := map[string]manifestCommand{}
	for _, c := range env.Data.Commands {
		m[c.Name] = c
	}
	return m
}

// shellWords splits line the way a POSIX shell does.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", `printf '%s\n' `+line).Output()
	require.NoError(t, err, line)
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

// parseWithTree parses words with the real command tree, every command's
// action replaced by one that records what ran, so nothing is executed.
func parseWithTree(t *testing.T, words []string) (ran, configDir string, setFlags map[string]bool) {
	t.Helper()
	root, err := cli.BuildRoot("v-test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	var stderr bytes.Buffer
	root.ErrWriter = &stderr
	setFlags = map[string]bool{}
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
				for _, f := range c.Flags {
					if n := f.Names()[0]; c.IsSet(n) {
						setFlags["--"+n] = true
					}
				}
				return nil
			}
		}
	}
	record(root.Commands, "")
	require.NoError(t, root.Run(t.Context(), words), "stderr: %s", stderr.String())
	require.Empty(t, stderr.String())
	return ran, configDir, setFlags
}
