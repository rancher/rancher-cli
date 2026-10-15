package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// commandPaths returns the argv path (without the program name) of every
// visible command in the tree below cmd: leaves (no visible subcommands)
// and groups separately. The tree is walked as built, so a command added
// later is covered without a list to edit.
func commandPaths(cmd *ucli.Command, prefix []string) (leaves, groups [][]string) {
	for _, sub := range cmd.Commands {
		if sub.Hidden {
			continue
		}
		path := append(slices.Clone(prefix), sub.Name)
		if len(sub.VisibleCommands()) == 0 {
			leaves = append(leaves, path)
			continue
		}
		groups = append(groups, path)
		l, g := commandPaths(sub, path)
		leaves = append(leaves, l...)
		groups = append(groups, g...)
	}
	return leaves, groups
}

// restoreRawArgv restores the captured-argv globals (rawArgv, the current
// word and the completion protocol flag) when the test ends. Every test
// that calls CaptureArgv or sets them uses it, and none of them is
// parallel.
func restoreRawArgv(t *testing.T) {
	t.Helper()
	orig, origW, origP := rawArgv, rawCurrentWord, rawCompleting
	t.Cleanup(func() { rawArgv, rawCurrentWord, rawCompleting = orig, origW, origP })
}

// stubExit replaces urfave's exit function, which unknown-command errors
// call, with one that records the code, and restores it when the test
// ends. The code is 0 until something exits.
func stubExit(t *testing.T) *int {
	t.Helper()
	orig := ucli.OsExiter
	code := new(int)
	ucli.OsExiter = func(c int) { *code = c }
	t.Cleanup(func() { ucli.OsExiter = orig })
	return code
}

// helpRoot returns a freshly built root with its output captured in out and
// errOut and an exit handler that does nothing, so an error a real process
// would exit with comes back as the return value of Run.
func helpRoot(t *testing.T) (root *ucli.Command, out, errOut *bytes.Buffer) {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	root.Writer = out
	root.ErrWriter = errOut
	root.Reader = strings.NewReader("")
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	return root, out, errOut
}

// runHelpArgv runs argv through a freshly built root and returns stdout,
// stderr and the error Run returned. It leaves the captured command line
// alone, so a parallel test can call it; a line the process entry point
// refuses before urfave parses it runs through runHelpArgvAsMain.
func runHelpArgv(t *testing.T, argv ...string) (stdout, stderr string, err error) {
	t.Helper()
	root, out, errOut := helpRoot(t)
	err = root.Run(t.Context(), append([]string{"rancher"}, argv...))
	return out.String(), errOut.String(), err
}

// runHelpArgvAsMain is runHelpArgv through Run, as the process entry point
// runs a line, with the command line captured. Not for a parallel test.
func runHelpArgvAsMain(t *testing.T, argv ...string) (stdout, stderr string, err error) {
	t.Helper()
	root, out, errOut := helpRoot(t)
	err = Run(t.Context(), root, append([]string{"rancher"}, argv...))
	return out.String(), errOut.String(), err
}

// TestLeafHelpShowsOwnHelpWithPositionalArgument asserts that for every
// leaf command in the built tree, `<leaf> x --help`, `<leaf> --help`,
// `--help <leaf>` and `-h <leaf> x` all succeed, print nothing on stderr,
// and print that leaf's own USAGE line. urfave treats a positional before
// --help as a help topic and, left to itself, fails with exit 3 (the AUTH
// exit code) and no output.
func TestLeafHelpShowsOwnHelpWithPositionalArgument(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	leaves, _ := commandPaths(root, nil)
	require.NotEmpty(t, leaves)

	for _, leaf := range leaves {
		name := strings.Join(leaf, " ")
		usage := regexp.MustCompile(`(?m)^USAGE:\n\s+rancher ` + regexp.QuoteMeta(name) + `( |$)`)
		argvs := [][]string{
			append(slices.Clone(leaf), "x", "--help"),
			append(slices.Clone(leaf), "--help"),
			append([]string{"--help"}, leaf...),
			append(append([]string{"-h"}, leaf...), "x"),
		}
		for _, argv := range argvs {
			t.Run(strings.Join(argv, " "), func(t *testing.T) {
				t.Parallel()
				stdout, stderr, err := runHelpArgv(t, argv...)
				require.NoError(t, err, "rancher %s", strings.Join(argv, " "))
				assert.Empty(t, stderr)
				assert.Regexp(t, usage, stdout, "rancher %s must show the USAGE line of %q", strings.Join(argv, " "), name)
			})
		}
	}
}

// TestLeafReceivesPositionalNamedHelp asserts that for every leaf command
// in the built tree, `<leaf> help` behaves exactly like `<leaf> zzz`: the
// same stdout, the same stderr and the same exit, with the argument text
// masked. A leaf that receives the argument reports whatever it reports for
// any other argument; if a help command intercepts the word instead, the
// leaf help card reaches stdout and the two runs differ. The runs use an
// empty config directory, the file credential store and no server, so no
// leaf has anything to fetch. Not parallel: it sets the process
// environment.
func TestLeafReceivesPositionalNamedHelp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("RANCHER_CLI_CONFIG_DIR", t.TempDir())
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	for _, name := range []string{"RANCHER_CLI_TOKEN", "RANCHER_CLI_URL", "RANCHER_CLI_SERVER"} {
		t.Setenv(name, "")
	}

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	leaves, _ := commandPaths(root, nil)
	require.NotEmpty(t, leaves)

	mask := func(s string) string { return strings.ReplaceAll(s, "help", "zzz") }
	for _, leaf := range leaves {
		t.Run(strings.Join(leaf, " "), func(t *testing.T) {
			helpOut, helpErr, helpRunErr := runHelpArgv(t, append(slices.Clone(leaf), "help")...)
			zzzOut, zzzErr, zzzRunErr := runHelpArgv(t, append(slices.Clone(leaf), "zzz")...)
			assert.Equal(t, mask(zzzOut), mask(helpOut), "stdout")
			assert.Equal(t, mask(zzzErr), mask(helpErr), "stderr")
			assert.Equal(t, exitCodeOf(zzzRunErr), exitCodeOf(helpRunErr), "exit")
		})
	}
}

// exitCodeOf returns the process exit code an error returned from Run
// stands for: 0 for nil, the ExitCoder's code, else 1.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	if ec, ok := errors.AsType[ucli.ExitCoder](err); ok {
		return ec.ExitCode()
	}
	return 1
}

// TestGroupHelpWithUnknownNameKeepsValidationError asserts that the leaf
// help override leaves groups alone: for every group in the built tree,
// `<group> x --help` still reports the unknown command as a VALIDATION
// error at exit 2, not a help page and not urfave's exit 3.
func TestGroupHelpWithUnknownNameKeepsValidationError(t *testing.T) {
	restoreRawArgv(t)
	gotCode := stubExit(t)

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	_, groups := commandPaths(root, nil)
	require.NotEmpty(t, groups)

	for _, group := range groups {
		argv := append(slices.Clone(group), "x", "--help")
		*gotCode = 0
		CaptureArgv(argv)
		stdout, stderr, err := runHelpArgv(t, argv...)
		require.NoError(t, err, "rancher %s", strings.Join(argv, " "))
		assert.Equal(t, 2, *gotCode, "rancher %s", strings.Join(argv, " "))
		assert.Empty(t, stdout, "rancher %s", strings.Join(argv, " "))
		assert.Contains(t, stderr, `error (VALIDATION): unknown command "x"`, "rancher %s", strings.Join(argv, " "))
	}
}

// TestHelpForHiddenChildOfGroupWithoutNotFoundSeam asserts that a group
// with no CommandNotFound seam and only hidden subcommands still shows the
// hidden subcommand's card for `<group> <hidden> --help`, not the group's:
// the name is a subcommand, so it is a topic.
func TestHelpForHiddenChildOfGroupWithoutNotFoundSeam(t *testing.T) {
	t.Parallel()
	hidden := &ucli.Command{Name: "hid", Hidden: true, Usage: "hidden child", Action: func(context.Context, *ucli.Command) error { return nil }}
	group := &ucli.Command{Name: "grp", Usage: "group", Commands: []*ucli.Command{hidden}}
	root := &ucli.Command{Name: "rancher", HideHelpCommand: true, Commands: []*ucli.Command{group}}
	var out bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &out
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "grp", "hid", "--help"}))
	assert.Regexp(t, `(?m)^USAGE:\n\s+rancher grp hid( |$)`, out.String())
}

// TestHelpWalkShowsHiddenChildCard asserts that `--help <group> <hidden>`
// shows the hidden subcommand's card, as `<group> <hidden> --help` does,
// not an unknown command error: below the first word of the walk a hidden
// subcommand is a topic too. Not parallel: an unknown command error would
// reach the exit function.
func TestHelpWalkShowsHiddenChildCard(t *testing.T) {
	restoreRawArgv(t)
	gotCode := stubExit(t)
	noop := func(context.Context, *ucli.Command) error { return nil }
	hidden := &ucli.Command{Name: "hid", Hidden: true, Usage: "hidden child", Action: noop}
	visible := &ucli.Command{Name: "vis", Usage: "visible child", Action: noop}
	group := &ucli.Command{Name: "grp", Usage: "group", Commands: []*ucli.Command{visible, hidden}}
	root := &ucli.Command{Name: "rancher", HideHelpCommand: true, Commands: []*ucli.Command{group}}
	var out bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &out
	CaptureArgv([]string{"--help", "grp", "hid"})
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--help", "grp", "hid"}))
	assert.Equal(t, 0, *gotCode)
	assert.Regexp(t, `(?m)^USAGE:\n\s+rancher grp hid( |$)`, out.String())
}

// TestEveryCommandHandlesUsageErrors asserts that, once urfave has set up
// the command tree, every command node - hidden ones and any subcommand
// urfave adds on its own - has an OnUsageError handler, and that no command
// is named help: the CLI has none, and urfave's own would take a positional
// argument named help away from its command. A node without the handler
// makes urfave print "Incorrect Usage: ..." itself and also return the
// error, so the same message would be written twice.
func TestEveryCommandHandlesUsageErrors(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &out
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--help"}))

	var nodes int
	var walk func(c *ucli.Command, path string)
	walk = func(c *ucli.Command, path string) {
		nodes++
		assert.NotNil(t, c.OnUsageError, "%q has no OnUsageError", path)
		for _, sub := range c.Commands {
			assert.False(t, sub.HasName("help"), "%q has a help command", path)
			walk(sub, path+" "+sub.Name)
		}
	}
	walk(root, "rancher")
	wr, err := buildWiredRegistry("golden", config.ReadOnlyOverride)
	require.NoError(t, err)
	commandCount := len(wr.reg.Schema("golden", config.MinRancherVersion).Commands)
	assert.Greater(t, nodes, commandCount, "the walk must cover every command the schema lists")
}

// TestHelpIsUnknownCommandWithHelpFlagHint asserts that `help` is an
// ordinary unknown command: VALIDATION, exit 2, message `unknown command
// "help"`, and a hint that is the runnable --help command for the words
// typed around it, cut back to the deepest command that exists. Not
// parallel: commandNotFound exits through urfave's exit function and reads
// the captured argv.
func TestHelpIsUnknownCommandWithHelpFlagHint(t *testing.T) {
	restoreRawArgv(t)
	gotCode := stubExit(t)

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	leaves, _ := commandPaths(root, nil)
	var leaf []string
	for _, l := range leaves {
		if len(l) >= 2 {
			leaf = l
			break
		}
	}
	require.NotEmpty(t, leaf, "the tree has a leaf below a group")

	tests := []struct {
		name string
		argv []string
		word string
		hint string
	}{
		{"bare help", []string{"help"}, "help", "rancher --help"},
		{"help then a leaf path", []string{"help", "get", "pod"}, "help", "rancher get pod --help"},
		{"help then a group", []string{"help", "get"}, "help", "rancher get --help"},
		{"help inside a group", []string{"get", "help"}, "help", "rancher get --help"},
		{"help then a group and an unknown name", []string{"help", "get", "nosuch"}, "help", "rancher get --help"},
		{"help inside a group then a leaf", []string{"get", "help", "pod"}, "help", "rancher get pod --help"},
		{"global flag between the words", []string{"help", "-o", "json", "get", "pod"}, "help", "rancher get pod --help"},
		{"--help, help, then a leaf", []string{"--help", "get", "help", "pod"}, "help", "rancher get pod --help"},
		{"--help, help, a global flag, then a leaf", []string{"--help", "get", "help", "-o", "json", "pod"}, "help", "rancher get pod --help"},
		{"-h, help, then a nested leaf", []string{"-h", "auth", "help", "get-token"}, "help", "rancher auth get-token --help"},
		{"--help, short spelling, then an alias", []string{"--help", "get", "h", "po"}, "h", "rancher get po --help"},
		{"--help, help, then a leaf path from the built tree", append([]string{"--help", leaf[0], "help"}, leaf[1:]...), "help", "rancher " + strings.Join(leaf, " ") + " --help"},
		{"another flag ends the words", []string{"--help", "get", "help", "--bogus", "pod"}, "help", "rancher get --help"},
		{"the first other flag ends the words", []string{"--help", "get", "help", "--bogus", "pod", "--x"}, "help", "rancher get --help"},
		{"global flag with an equals-form value", []string{"--help", "get", "help", "-o=json", "pod"}, "help", "rancher get pod --help"},
		{"global flag with a long equals-form value", []string{"--help", "get", "help", "--output=json", "pod"}, "help", "rancher get pod --help"},
		{"words after -- are words", []string{"--help", "get", "help", "--", "pod"}, "help", "rancher get pod --help"},
		{"words after -- are words in a parsed line", []string{"get", "help", "--", "pod"}, "help", "rancher get pod --help"},
		{"a number ends the flags", []string{"--help", "get", "help", "-1", "pod"}, "help", "rancher get --help"},
		{"short spelling", []string{"h"}, "h", "rancher --help"},
		{"short spelling then a leaf path", []string{"h", "get", "pod"}, "h", "rancher get pod --help"},
		{"short spelling inside a group", []string{"get", "h"}, "h", "rancher get --help"},
		{"hidden command is not named in the hint", []string{"__completion", "help"}, "help", "rancher --help"},
		{"leaf path from the built tree", append([]string{"help"}, leaf...), "help", "rancher " + strings.Join(leaf, " ") + " --help"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argv := tt.argv
			if !slices.Contains(argv, "json") {
				argv = append([]string{"-o", "json"}, argv...)
			}
			*gotCode = 0
			CaptureArgv(argv)
			_, stderr, err := runHelpArgv(t, argv...)
			require.NoError(t, err)
			assert.Equal(t, 2, *gotCode)
			var env errEnvelope
			require.NoError(t, json.Unmarshal([]byte(stderr), &env))
			assert.Equal(t, "VALIDATION", env.Data.Error.Code)
			assert.Equal(t, fmt.Sprintf("unknown command %q", tt.word), env.Data.Error.Message)
			assert.Equal(t, tt.hint, env.Data.Error.Hint)
		})
	}

	t.Run("text output", func(t *testing.T) {
		*gotCode = 0
		CaptureArgv([]string{"help", "get"})
		_, stderr, err := runHelpArgv(t, "help", "get")
		require.NoError(t, err)
		assert.Equal(t, 2, *gotCode)
		assert.Equal(t, "error (VALIDATION): unknown command \"help\"\n  hint: rancher get --help\n", stderr)
	})
}

// TestHelpWalkStopsAtFlag asserts where the `--help <words>` walk ends: at
// a flag token, with the card of the command reached so far and exit 0.
// The root leaves a flag that comes after the first command word in the
// arguments, so the walk sees it. Not parallel: a walk that failed to stop
// would reach the exit function, which is replaced here so the failure is
// an assertion, not a process exit.
func TestHelpWalkStopsAtFlag(t *testing.T) {
	restoreRawArgv(t)
	gotCode := stubExit(t)
	usage := regexp.MustCompile(`(?m)^USAGE:\n\s+rancher get( |$)`)
	for _, argv := range [][]string{
		{"--help", "get", "--help"},
		{"--help", "get", "-o", "json", "pod"},
		{"--help", "get", "--bogus", "pod"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			*gotCode = 0
			CaptureArgv(argv)
			stdout, stderr, err := runHelpArgv(t, argv...)
			require.NoError(t, err)
			assert.Equal(t, 0, *gotCode)
			assert.Empty(t, stderr)
			assert.Regexp(t, usage, stdout)
		})
	}
}

// TestHelpWalkUnknownWordUnderEveryGroup asserts, for every group in the
// built tree, that `--help <group> <word>` and `-h <group> <word>`
// neither panic nor leave the error contract. The words are help, h and an
// unknown name (VALIDATION, exit 2, and under -o json a decodable
// envelope), and a flag (the walk ends and the group card is shown, exit
// 0). The group was never run by urfave when the walk reaches the word, so
// the error must be built from the words the walk holds, not from the
// group's parsed arguments. Not parallel: the exit function and the
// captured argv are process-wide.
func TestHelpWalkUnknownWordUnderEveryGroup(t *testing.T) {
	restoreRawArgv(t)
	gotCode := stubExit(t)

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	_, groups := commandPaths(root, nil)
	require.NotEmpty(t, groups)

	for _, group := range groups {
		path := strings.Join(group, " ")
		for _, flag := range []string{"--help", "-h"} {
			for _, word := range []string{"help", "h", "x", "--bogus"} {
				for _, mode := range []string{"table", "json"} {
					argv := append([]string{"-o", mode, flag}, group...)
					argv = append(argv, word)
					name := strings.Join(argv, " ")
					*gotCode = 0
					CaptureArgv(argv)
					var stdout, stderr string
					var runErr error
					func() {
						defer func() {
							if r := recover(); r != nil {
								t.Errorf("rancher %s panicked: %v", name, r)
							}
						}()
						stdout, stderr, runErr = runHelpArgv(t, argv...)
					}()
					require.NoError(t, runErr, "rancher %s", name)
					if word == "--bogus" {
						assert.Equal(t, 0, *gotCode, "rancher %s", name)
						assert.Empty(t, stderr, "rancher %s", name)
						assert.Regexp(t, `(?m)^USAGE:\n\s+rancher `+regexp.QuoteMeta(path)+`( |$)`, stdout, "rancher %s", name)
						continue
					}
					assert.Equal(t, 2, *gotCode, "rancher %s", name)
					assert.Empty(t, stdout, "rancher %s", name)
					if mode != "json" {
						assert.Contains(t, stderr, "error (VALIDATION): unknown command", "rancher %s", name)
						continue
					}
					var env errEnvelope
					if assert.NoError(t, json.Unmarshal([]byte(stderr), &env), "rancher %s", name) {
						assert.Equal(t, "VALIDATION", env.Data.Error.Code, "rancher %s", name)
						assert.Equal(t, fmt.Sprintf("unknown command %q", word), env.Data.Error.Message, "rancher %s", name)
						if word == "help" || word == "h" {
							assert.Equal(t, "rancher "+path+" --help", env.Data.Error.Hint, "rancher %s", name)
						}
					}
				}
			}
		}
	}
}

// TestLevelFlagsOwnFlagWinsOverAncestors asserts, on a scratch tree, that a
// command's own flag keeps its arity when an ancestor has a flag of the same
// name, as in urfave (an ancestor's flag is not applied when the command
// already has a flag with one of its names), and that an ancestor's other
// flags are still accepted below it.
func TestLevelFlagsOwnFlagWinsOverAncestors(t *testing.T) {
	t.Parallel()
	noop := func(context.Context, *ucli.Command) error { return nil }
	child := &ucli.Command{Name: "child", Action: noop, Flags: []ucli.Flag{&ucli.BoolFlag{Name: "mode"}}}
	root := &ucli.Command{
		Name:     "rancher",
		Commands: []*ucli.Command{child},
		Flags:    []ucli.Flag{&ucli.StringFlag{Name: "mode"}, &ucli.StringFlag{Name: "name", Aliases: []string{"n"}}},
	}
	var out bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &out
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "child", "--help"}))

	flags := levelFlags(child)
	takesValue, ok := flags["mode"]
	assert.True(t, ok)
	assert.False(t, takesValue, "the command's own bool flag wins over the ancestor's value flag")
	assert.True(t, flags["name"], "an ancestor's flag with another name is accepted and takes a value")
	assert.True(t, flags["n"], "and so is its alias")
	assert.Contains(t, flags, "help", "the help flag urfave adds is accepted")
	assert.True(t, levelFlags(root)["mode"], "at the root the flag keeps its own arity")
}

// TestTailWords asserts how a tail of words is read under a set of flags:
// words and flag values apart, the cut at the first undefined flag, a bare
// dash as the last word read, "--" and a word that starts with a dash and a
// character that is not a letter as the start of the words, and a dash and
// a letter of any script as a flag.
func TestTailWords(t *testing.T) {
	t.Parallel()
	flags := map[string]bool{"output": true, "o": true, "debug": false}
	tests := []struct {
		name      string
		tail      []string
		words     []string
		before    int
		undefined bool
	}{
		{"words and a flag value", []string{"a", "-o", "json", "b"}, []string{"a", "b"}, 2, false},
		{"equals value takes no next word", []string{"--output=json", "a"}, []string{"a"}, 1, false},
		{"bool flag", []string{"--debug", "a"}, []string{"a"}, 1, false},
		{"cut at the first undefined flag", []string{"a", "--x", "b", "--y", "c"}, []string{"a", "b", "c"}, 1, true},
		{"bare dash is the last word", []string{"a", "-", "b"}, []string{"a", "-"}, 2, false},
		{"no flag is read after a bare dash", []string{"-", "--x"}, []string{"-"}, 1, false},
		{"double dash starts the words", []string{"a", "--", "--x", "-o"}, []string{"a", "--x", "-o"}, 3, false},
		{"number starts the words", []string{"a", "-1", "--x", "-o"}, []string{"a", "-1", "--x", "-o"}, 4, false},
		{"multi-byte symbol after the dash starts the words", []string{"a", "-\u20ac", "--x"}, []string{"a", "-\u20ac", "--x"}, 3, false},
		{"multi-byte letter after the dash is a flag", []string{"a", "-\u05d0", "--x"}, []string{"a"}, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			words, before, undefined := tailWords(tt.tail, flags)
			assert.Equal(t, tt.words, words)
			assert.Equal(t, tt.before, before)
			assert.Equal(t, tt.undefined, undefined)
		})
	}
}
