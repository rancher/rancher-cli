package cli_test

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompletionAfterUnknownWordOffersNothing asserts that once a word that
// names no command precedes the word being completed, completion at the
// root or a group offers no candidate, whatever kind of word is completed
// (a command name, a flag name, a flag's value in either spelling): the
// command would reject the line. The unknown words are `help` (there is no
// help command) and `nosuch`, at the root and after every top-level group
// in the built tree. The word after `--help` or `-h` is a word too. Global
// flags and their values before the word do not count as a word, so `-o json
// <TAB>` and `<group> -o json <TAB>` still list commands. This covers the v2
// protocol the shipped shell scripts send. NOT parallel: mutates
// rawArgv/rawCurrentWord globals and env.
func TestCompletionAfterUnknownWordOffersNothing(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	t.Setenv(config.EnvConfigDir, t.TempDir())

	root, err := cli.BuildRoot("test")
	require.NoError(t, err)
	paths := [][]string{nil}
	for _, c := range root.Commands {
		if !c.Hidden && len(c.VisibleCommands()) > 0 {
			paths = append(paths, []string{c.Name})
		}
	}
	require.Greater(t, len(paths), 1)

	// Each case is the words typed after the unknown word, then the word
	// being completed.
	cases := []struct {
		name    string
		after   []string
		current string
	}{
		{"command name", nil, ""},
		{"flag name", nil, "--"},
		{"space-form flag value", []string{"-o"}, ""},
		{"space-form flag value of a flag with a long name", []string{"--color"}, ""},
		{"equals-form flag value", nil, "--output="},
	}
	for _, path := range paths {
		for _, word := range []string{"help", "nosuch"} {
			for _, tc := range cases {
				words := append(append(append([]string{}, path...), word), tc.after...)
				got := extractTokens(runNameLikeCompletion(t, words, tc.current))
				assert.Empty(t, got, "rancher %v %q <TAB> (%s)", words, tc.current, tc.name)
			}
		}
		listed := append(append([]string{}, path...), "-o", "json")
		assert.NotEmpty(t, extractTokens(runNameLikeCompletion(t, listed, "")), "rancher %v <TAB>", listed)
		for _, typed := range [][]string{
			{"--help", "nosuch"},
			{"-h", "nosuch"},
			{"--help", "help"},
			{"--output=json", "nosuch"},
		} {
			words := append(append([]string{}, path...), typed...)
			assert.Empty(t, extractTokens(runNameLikeCompletion(t, words, "")), "rancher %v <TAB>", words)
		}
	}
}
