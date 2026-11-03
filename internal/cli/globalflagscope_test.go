package cli

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// globalFlagScopeRow is one place a global flag may be offered or listed: a
// command, a command group or the root. descriptors are the registered
// commands at or under it.
type globalFlagScopeRow struct {
	label       string
	words       []string
	root        bool
	group       bool
	descriptors []runtime.Descriptor
}

// globalFlagLongName is the long name of a manifest global flag without its
// dashes: "-o/--output" is "output".
func globalFlagLongName(gf runtime.GlobalFlag) string {
	if _, after, found := strings.CutLast(gf.Name, "--"); found {
		return after
	}
	return strings.TrimLeft(gf.Name, "-")
}

// globalFlagIsOfferedBy is what the manifest says about one row: whether a
// command, or any command under a group, accepts the flag by its appliesTo
// value and command type. The root takes every flag. It reads the manifest,
// the command types and the project flag of each descriptor itself and
// never calls the production rule.
func globalFlagIsOfferedBy(t *testing.T, row globalFlagScopeRow, gf runtime.GlobalFlag) bool {
	t.Helper()
	if row.root {
		return true
	}
	require.NotEmpty(t, row.descriptors, "%s: no registered command at or under it", row.label)
	return slices.ContainsFunc(row.descriptors, func(d runtime.Descriptor) bool {
		switch gf.AppliesTo {
		case "all":
			return true
		case "mutating":
			return d.CommandType == runtime.Mutating
		case "server":
			return d.CommandType == runtime.ReadOnly || d.CommandType == runtime.Mutating
		case "declared":
			return runtime.TakesProject(d)
		}
		require.Failf(t, "unknown appliesTo", "%s: %q", gf.Name, gf.AppliesTo)
		return false
	})
}

// offeredFlagTokens is the flag tokens a completion request for the current
// word "--" offers after words.
func offeredFlagTokens(t *testing.T, words []string) []string {
	t.Helper()
	return offeredTokensFor(t, words, "--")
}

// offeredTokensFor is the flag tokens a completion request for the current
// word cur offers after words.
func offeredTokensFor(t *testing.T, words []string, cur string) []string {
	t.Helper()
	out := surfaceRun(t, slices.Concat([]string{"--rancher-completion-proto=2"}, words,
		[]string{"--rancher-completion-cur=" + cur, "--generate-shell-completion"}))
	var tokens []string
	for line := range strings.SplitSeq(out, "\n") {
		if line != "" {
			tokens = append(tokens, candidateToken(line))
		}
	}
	return tokens
}

// listsVersionFlag reports whether a help section has a row for --version.
// The row of a flag with an alias starts with "--version," and the row of a
// flag without one with "--version " (listsFlag reads only the second form).
func listsVersionFlag(section string) bool {
	for line := range strings.SplitSeq(section, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "--version,") || strings.HasPrefix(line, "--version ") {
			return true
		}
	}
	return false
}

// TestEveryCommandAndGroupOffersOnlyTheGlobalFlagsItAccepts asserts, for
// every registered command, every command group of the built tree and the
// root, that flag-name completion offers and the help card lists each
// global flag of the schema manifest exactly when the manifest's appliesTo
// says the command accepts it: --dry-run, --apply, --yes and
// --allow-deprecated on mutating commands, --read-only on commands that
// read or change server resources, --project on commands that take it and
// the rest on every command. A group offers a flag when a command under it
// accepts it; the root offers every flag. A command lists --project under
// its own OPTIONS, never under GLOBAL OPTIONS. Completion offers --version
// and its alias -v, and the help card lists --version, on the root only.
// The offer is checked in both directions: a flag a command accepts must
// still be offered. It also asserts that the built tree and the registry
// hold the same commands and that every command of the tree carries its
// descriptor.
//
// Limits:
//   - It compares the offer with the manifest's appliesTo and the command
//     type. It does not show that an accepted flag is useful to the command:
//     --yes is offered on a mutating command that has no destructive step,
//     and --allow-deprecated likewise.
//   - A flag typed before the command name (rancher --apply get pod) is
//     accepted by the root and the line is judged at the command; the root's
//     own offer is every flag.
//   - --version and its alias -v are asserted on the root only; --help is
//     accepted by every command and is not asserted.
//   - One candidate list serves bash, zsh, fish and pwsh; the test reads it
//     through the binary's protocol, not through each shell script.
//   - No command group in the built tree holds commands of mixed types; the
//     rule for such a group is asserted on a hand-built tree.
//   - Only the current word "--" is exercised; a longer prefix and the "="
//     form take the same code path.
//   - The expected table reads appliesTo a second time; a wrong reading
//     shared with the rule passes, and the typed-flag results are the only
//     check of behavior.
//   - At run time the root also holds a hidden command with no descriptor;
//     the root accepts every flag because the rule does, not because of a
//     classified command under it.
//
// Not parallel: it sets the process environment and the captured command
// line.
func TestEveryCommandAndGroupOffersOnlyTheGlobalFlagsItAccepts(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	t.Setenv(config.EnvCredentialStore, "file")
	t.Cleanup(func() { CaptureArgv(nil) })
	restoreRawArgv(t)

	flags := runtime.GlobalFlags()
	for _, gf := range flags {
		require.Contains(t, []string{"all", "mutating", "server", "declared"}, gf.AppliesTo, "%s: appliesTo", gf.Name)
	}

	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	var descriptors []runtime.Descriptor
	var registered []string
	for _, c := range wr.reg.Commands() {
		descriptors = append(descriptors, c.Describe())
		registered = append(registered, c.Describe().Name)
	}

	tree, err := BuildRoot("v-test")
	require.NoError(t, err)
	var treeCommands []string
	var groups [][]string
	var walk func(cmd *ucli.Command, path []string)
	walk = func(cmd *ucli.Command, path []string) {
		for _, sub := range cmd.Commands {
			p := append(slices.Clone(path), sub.Name)
			if len(sub.Commands) > 0 {
				groups = append(groups, p)
				walk(sub, p)
				continue
			}
			_, ok := commandDescriptor(sub)
			require.True(t, ok, "%s: the command of the tree has no descriptor", strings.Join(p, " "))
			treeCommands = append(treeCommands, strings.Join(p, " "))
		}
	}
	walk(tree, nil)
	require.Len(t, treeCommands, len(registered), "the tree and the registry hold the same number of commands")
	require.ElementsMatch(t, registered, treeCommands)

	var rows []globalFlagScopeRow
	for _, d := range descriptors {
		rows = append(rows, globalFlagScopeRow{label: d.Name, words: strings.Fields(d.Name), descriptors: []runtime.Descriptor{d}})
	}
	groupTypes := map[string]map[runtime.CommandType]bool{}
	for _, path := range groups {
		row := globalFlagScopeRow{label: strings.Join(path, " "), words: path, group: true}
		for _, d := range descriptors {
			if slices.Equal(strings.Fields(d.Name)[:len(path)], path) {
				row.descriptors = append(row.descriptors, d)
			}
		}
		rows = append(rows, row)
		groupTypes[row.label] = map[runtime.CommandType]bool{}
		for _, d := range row.descriptors {
			groupTypes[row.label][d.CommandType] = true
		}
	}
	rows = append(rows, globalFlagScopeRow{label: "(root)", root: true})

	checked := map[string]bool{}
	commandTypes := map[runtime.CommandType]int{}
	for _, row := range rows {
		isCommand := !row.root && !row.group
		if isCommand {
			commandTypes[row.descriptors[0].CommandType]++
		}
		offered := offeredFlagTokens(t, row.words)
		help := surfaceRun(t, slices.Concat(row.words, []string{"--help"}))
		options, globals := helpSections(help)
		for _, gf := range flags {
			name := globalFlagLongName(gf)
			want := globalFlagIsOfferedBy(t, row, gf)
			assert.Equal(t, want, slices.Contains(offered, "--"+name), "%s: completion offers --%s", row.label, name)
			if isCommand && name == runtime.FlagProject {
				assert.Equal(t, want, listsFlag(options, name), "%s: OPTIONS lists --%s", row.label, name)
				assert.False(t, listsFlag(globals, name), "%s: GLOBAL OPTIONS lists --%s", row.label, name)
				continue
			}
			assert.Equal(t, want, listsFlag(globals, name), "%s: GLOBAL OPTIONS lists --%s", row.label, name)
		}
		assert.Equal(t, row.root, slices.Contains(offered, "--version"), "%s: completion offers --version", row.label)
		assert.Equal(t, row.root, slices.Contains(offeredTokensFor(t, row.words, "-"), "-v"), "%s: completion offers -v", row.label)
		assert.Equal(t, row.root, listsVersionFlag(globals), "%s: GLOBAL OPTIONS lists --version", row.label)
		checked[row.label] = true
	}

	assert.Positive(t, commandTypes[runtime.ReadOnly])
	assert.Positive(t, commandTypes[runtime.Mutating])
	assert.Positive(t, commandTypes[runtime.Local])
	assert.Equal(t, map[runtime.CommandType]bool{runtime.Local: true}, groupTypes["config"])
	assert.Equal(t, map[runtime.CommandType]bool{runtime.ReadOnly: true}, groupTypes["get"])
	assert.Equal(t, map[runtime.CommandType]bool{runtime.Mutating: true}, groupTypes["delete"])
	for _, label := range []string{"config", "get", "delete", "(root)"} {
		assert.True(t, checked[label], "%s was checked", label)
	}
	assert.Len(t, checked, len(registered)+len(groups)+1)
}

// TestAppliesToAcceptsByCommandType asserts the answer for every appliesTo
// value of the manifest and every command type, and that a value the table
// does not know is accepted.
func TestAppliesToAcceptsByCommandType(t *testing.T) {
	for _, row := range []struct {
		appliesTo   string
		commandType runtime.CommandType
		takes       bool
		want        bool
	}{
		{"all", runtime.ReadOnly, false, true},
		{"all", runtime.Mutating, false, true},
		{"all", runtime.Local, false, true},
		{"mutating", runtime.ReadOnly, false, false},
		{"mutating", runtime.Mutating, false, true},
		{"mutating", runtime.Local, false, false},
		{"server", runtime.ReadOnly, false, true},
		{"server", runtime.Mutating, false, true},
		{"server", runtime.Local, false, false},
		{"declared", runtime.ReadOnly, false, false},
		{"declared", runtime.ReadOnly, true, true},
		{"declared", runtime.Mutating, true, true},
		{"declared", runtime.Local, true, true},
		{"declared", runtime.Mutating, false, false},
		{"sometimes", runtime.Local, false, true},
		{"sometimes", runtime.ReadOnly, true, true},
	} {
		assert.Equal(t, row.want, appliesToAccepts(row.appliesTo, row.commandType, row.takes),
			"%s on %s, takes project %v", row.appliesTo, row.commandType, row.takes)
	}
}

// TestAGroupAcceptsAGlobalFlagWhenAnyCommandUnderItDoes asserts the rule on
// a hand-built tree: a command accepts a flag by its type, a group when any
// command under it does, whatever the order of the commands, and a command
// with no descriptor accepts every flag. The help flag, which the manifest
// does not list, is accepted everywhere.
func TestAGroupAcceptsAGlobalFlagWhenAnyCommandUnderItDoes(t *testing.T) {
	leaf := func(name string, ct runtime.CommandType, flags ...runtime.ArgSpec) *ucli.Command {
		return &ucli.Command{Name: name, Metadata: map[string]any{
			descriptorMetadataKey: runtime.Descriptor{Name: "g " + name, CommandType: ct, Flags: flags},
		}}
	}
	group := func(subs ...*ucli.Command) *ucli.Command { return &ucli.Command{Name: "g", Commands: subs} }
	project := runtime.ProjectFlag(false, "the project")
	mutating := []string{"apply", "dry-run", "yes", "allow-deprecated"}
	always := []string{"output", "help"}

	for _, row := range []struct {
		name string
		cmd  *ucli.Command
		// accepted lists the flags the command or group accepts; every
		// other flag of the row's check list is refused.
		accepted []string
	}{
		{"a group of a local then a mutating command", group(leaf("a", runtime.Local), leaf("b", runtime.Mutating)),
			slices.Concat(mutating, []string{"read-only"}, always)},
		{"a group of a mutating then a local command", group(leaf("a", runtime.Mutating), leaf("b", runtime.Local)),
			slices.Concat(mutating, []string{"read-only"}, always)},
		{"a group of a local then a read-only command", group(leaf("a", runtime.Local), leaf("b", runtime.ReadOnly)),
			slices.Concat([]string{"read-only"}, always)},
		{"a group of a read-only then a local command", group(leaf("a", runtime.ReadOnly), leaf("b", runtime.Local)),
			slices.Concat([]string{"read-only"}, always)},
		{"a group of local commands", group(leaf("a", runtime.Local), leaf("b", runtime.Local)),
			always},
		{"a group of read-only commands", group(leaf("a", runtime.ReadOnly), leaf("b", runtime.ReadOnly)),
			slices.Concat([]string{"read-only"}, always)},
		{"a group whose last command takes project", group(leaf("a", runtime.ReadOnly), leaf("b", runtime.ReadOnly, project)),
			slices.Concat([]string{"read-only", "project"}, always)},
		{"a group whose first command takes project", group(leaf("a", runtime.ReadOnly, project), leaf("b", runtime.ReadOnly)),
			slices.Concat([]string{"read-only", "project"}, always)},
		{"a group in a group", group(leaf("a", runtime.Local), group(leaf("b", runtime.Mutating))),
			slices.Concat(mutating, []string{"read-only"}, always)},
		{"a local command", leaf("a", runtime.Local), always},
		{"a read-only command", leaf("a", runtime.ReadOnly), slices.Concat([]string{"read-only"}, always)},
		{"a mutating command", leaf("a", runtime.Mutating), slices.Concat(mutating, []string{"read-only"}, always)},
		{"a read-only command that takes project", leaf("a", runtime.ReadOnly, project),
			slices.Concat([]string{"read-only", "project"}, always)},
		{"a command with no descriptor", &ucli.Command{Name: "a"},
			slices.Concat(mutating, []string{"read-only", "project"}, always)},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, name := range []string{"apply", "dry-run", "yes", "allow-deprecated", "read-only", "project", "output", "help"} {
				assert.Equal(t, slices.Contains(row.accepted, name), commandAcceptsGlobalFlag(row.cmd, name), "--%s", name)
			}
		})
	}
}

// TestOnlyTheRootAcceptsTheVersionFlag asserts that the rule accepts the
// version flag on the root and refuses it on a command group and on a
// command, and that the help flag is accepted by all three. The tree is run
// once: a node that has not run has no parent and reads as its own root.
func TestOnlyTheRootAcceptsTheVersionFlag(t *testing.T) {
	cmd := &ucli.Command{Name: "c", Metadata: map[string]any{
		descriptorMetadataKey: runtime.Descriptor{Name: "g c", CommandType: runtime.Local},
	}}
	group := &ucli.Command{Name: "g", Commands: []*ucli.Command{cmd}}
	root := &ucli.Command{
		Name:      "r",
		Version:   "v-test",
		Commands:  []*ucli.Command{group},
		Action:    func(context.Context, *ucli.Command) error { return nil },
		Writer:    io.Discard,
		ErrWriter: io.Discard,
	}
	require.NoError(t, root.Run(t.Context(), []string{"r"}))

	assert.True(t, commandAcceptsGlobalFlag(root, "version"), "the root accepts --version")
	assert.False(t, commandAcceptsGlobalFlag(group, "version"), "a command group refuses --version")
	assert.False(t, commandAcceptsGlobalFlag(cmd, "version"), "a command refuses --version")
	for _, c := range []*ucli.Command{root, group, cmd} {
		assert.True(t, commandAcceptsGlobalFlag(c, "help"), "%s accepts --help", c.Name)
	}
}

// TestACommandsOwnFlagNamedLikeAGlobalFlagIsOffered asserts that flag-name
// completion never drops a flag a command defines itself, even when its name
// is a global flag the command does not accept, and still drops the global
// flag of that name from another command.
func TestACommandsOwnFlagNamedLikeAGlobalFlagIsOffered(t *testing.T) {
	var own, other []string
	action := func(into *[]string) func(context.Context, *ucli.Command) error {
		return func(_ context.Context, c *ucli.Command) error {
			*into = flagNameCandidates(c, "--")
			return nil
		}
	}
	readOnly := func(name string) map[string]any {
		return map[string]any{descriptorMetadataKey: runtime.Descriptor{Name: name, CommandType: runtime.ReadOnly}}
	}
	build := func() *ucli.Command {
		return &ucli.Command{
			Name:  "r",
			Flags: []ucli.Flag{&ucli.BoolFlag{Name: "apply", Usage: "global"}, &ucli.BoolFlag{Name: "yes", Usage: "global yes"}},
			Commands: []*ucli.Command{
				{Name: "own", HideHelpCommand: true, Metadata: readOnly("own"), Flags: []ucli.Flag{&ucli.BoolFlag{Name: "apply", Usage: "own"}}, Action: action(&own)},
				{Name: "plain", HideHelpCommand: true, Metadata: readOnly("plain"), Action: action(&other)},
			},
		}
	}
	require.NoError(t, build().Run(t.Context(), []string{"r", "own"}))
	require.NoError(t, build().Run(t.Context(), []string{"r", "plain"}))
	assert.Contains(t, own, "--apply:own")
	assert.NotContains(t, own, "--yes:global yes")
	assert.NotContains(t, other, "--apply:global")
	assert.NotContains(t, other, "--yes:global yes")
}

// TestAGlobalFlagTypedWhereItIsNotOfferedKeepsItsResult asserts that a flag
// a command does not offer or list still gives its result when typed:
// --dry-run on a read-only or local command is refused with
// DRY_RUN_NOT_APPLICABLE, also when typed before the command's name,
// --read-only on a local command is refused with VALIDATION, and --apply,
// --yes and --allow-deprecated are ignored there. Not parallel: it sets the
// process environment and the captured command line.
func TestAGlobalFlagTypedWhereItIsNotOfferedKeepsItsResult(t *testing.T) {
	restoreRawArgv(t)
	extraArgsEnv(t)
	for _, row := range []struct {
		argv    []string
		code    string
		message string
	}{
		{[]string{"get", "pod", "--dry-run", "-o", "json"}, clierr.CodeDryRunNotApplicable, "get pod is a read-only command"},
		{[]string{"get", "--dry-run", "pod", "-o", "json"}, clierr.CodeDryRunNotApplicable, "get pod is a read-only command"},
		{[]string{"config", "--dry-run", "path", "-o", "json"}, clierr.CodeDryRunNotApplicable, "config path is a local command"},
		{[]string{"config", "--read-only", "path", "-o", "json"}, clierr.CodeValidation, "config path is a local command"},
	} {
		t.Run(strings.Join(row.argv, " "), func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			_, stderr, code := runBounded(t, root, row.argv)
			assert.Equal(t, 2, code)
			ce := renderedError(t, "json", stderr)
			assert.Equal(t, row.code, ce.Code)
			assert.Contains(t, ce.Message, row.message)
		})
	}

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	stdout, stderr, code := runBounded(t, root, []string{"config", "path", "--apply", "--yes", "--allow-deprecated", "-o", "json"})
	require.Equal(t, 0, code, stderr)
	var env struct {
		Data struct {
			Path string `json:"path"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &env), "stdout: %s", stdout)
	assert.NotEmpty(t, env.Data.Path)
}

// TestTheVersionFlagTypedOnACommandIsRefused asserts that --version and -v
// typed after a command's name are refused with VALIDATION and exit code 2,
// as they are for any flag the command does not define. Not parallel: it
// sets the process environment and the captured command line.
func TestTheVersionFlagTypedOnACommandIsRefused(t *testing.T) {
	restoreRawArgv(t)
	extraArgsEnv(t)
	for _, argv := range [][]string{
		{"config", "path", "--version", "-o", "json"},
		{"get", "pod", "-v", "-o", "json"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			_, stderr, code := runBounded(t, root, argv)
			assert.Equal(t, 2, code)
			ce := renderedError(t, "json", stderr)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Contains(t, ce.Message, "flag provided but not defined")
		})
	}
}
