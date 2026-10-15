package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// lineResult is what a pass over a command line comes to, as urfave does it
// and as parseLine reads it.
type lineResult struct {
	// kind is runs, help, version or error.
	kind string
	// cmd is the command that runs, shows its help or reports the error.
	cmd string
	// args and set are, for a command that runs, its arguments and the
	// names of the flags in its flag set that are set.
	args []string
	set  []string
	// class is the kind of parse error.
	class string
}

// lineRecorder holds what urfave does with one line run on a tree whose
// actions are replaced.
type lineRecorder struct {
	ran         *ucli.Command
	args        []string
	set         []string
	usageCmd    string
	usageErr    string
	notFound    bool
	out, errOut bytes.Buffer
}

// flagNamesSet returns the names of the flags in c's flag set that are set.
func flagNamesSet(c *ucli.Command) []string {
	var out []string
	for _, l := range c.Lineage() {
		for _, f := range l.Flags {
			for _, name := range f.Names() {
				if c.IsSet(name) && !slices.Contains(out, name) {
					out = append(out, name)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// recordTree replaces what urfave runs on the commands of cmd and below
// with recorders: the action, the hooks before and after it, the handler of
// a parse error, and the handler of an unknown command where the command
// has one (the help of a command that has none reads that as a leaf).
func recordTree(cmd *ucli.Command, rec *lineRecorder) {
	cmd.Before, cmd.After = nil, nil
	cmd.Action = func(_ context.Context, c *ucli.Command) error {
		rec.ran = c
		rec.args = c.Args().Slice()
		rec.set = flagNamesSet(c)
		return nil
	}
	cmd.OnUsageError = func(_ context.Context, c *ucli.Command, err error, _ bool) error {
		rec.usageCmd, rec.usageErr = c.FullName(), err.Error()
		return nil
	}
	if cmd.CommandNotFound != nil {
		cmd.CommandNotFound = func(context.Context, *ucli.Command, string) { rec.notFound = true }
	}
	for _, sub := range cmd.Commands {
		recordTree(sub, rec)
	}
}

// urfaveClass reads the kind of a parse error from urfave's message.
func urfaveClass(msg string) string {
	switch {
	case strings.Contains(msg, "flag provided but not defined"):
		return "undefined flag"
	case strings.Contains(msg, "flag needs an argument"):
		return "missing value"
	case strings.Contains(msg, "can't duplicate this flag"):
		return "repeated flag"
	case strings.Contains(msg, "parse error"):
		return "bad bool value"
	}
	return msg
}

// runOnUrfave runs line on a new root, with the words checked before urfave
// parses left out of the way: the line goes to root.Run directly.
func runOnUrfave(t *testing.T, line []string) (lineResult, *lineRecorder) {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	rec := &lineRecorder{}
	recordTree(root, rec)
	root.Writer, root.ErrWriter = &rec.out, &rec.errOut
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	_ = root.Run(t.Context(), append([]string{"rancher"}, line...))
	var res lineResult
	switch {
	case rec.ran != nil:
		res = lineResult{kind: "runs", cmd: rec.ran.FullName(), args: rec.args, set: rec.set}
	case rec.usageErr != "":
		res = lineResult{kind: "error", cmd: rec.usageCmd, class: urfaveClass(rec.usageErr)}
	case strings.Contains(rec.out.String(), "version\n") && !strings.Contains(rec.out.String(), "help "):
		res = lineResult{kind: "version"}
	case rec.notFound || rec.errOut.Len() > 0:
		// An unknown command shown while showing help is reported through
		// the handler or as an envelope on the error writer.
		if rec.errOut.Len() > 0 {
			var env struct {
				Data struct {
					Error struct{ Message string }
				}
			}
			require.NoError(t, json.Unmarshal(rec.errOut.Bytes(), &env), "stderr: %s", rec.errOut.String())
			require.True(t, strings.HasPrefix(env.Data.Error.Message, "unknown command"), "stderr: %s", rec.errOut.String())
		}
		res = lineResult{kind: "error", class: "unknown command"}
	case strings.Contains(rec.out.String(), "help "):
		res = lineResult{kind: "help", cmd: strings.TrimSpace(strings.TrimPrefix(firstLine(rec.out.String()), "help "))}
	default:
		res = lineResult{kind: "nothing"}
	}
	return res, rec
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// modelHelp reads the help a level shows the way the command's help seam
// does: its own card when it is a leaf, has no argument or shows help after
// a parse error, and otherwise the card of the command its first argument
// names and the words after it name, or an unknown command.
func modelHelp(p parsedLine, level int, afterError bool) lineResult {
	lv := p.levels[level]
	if afterError || lv.leaf || len(lv.args) == 0 || lv.args[0] == "" {
		return lineResult{kind: "help", cmd: p.commandName(level)}
	}
	unknown := lineResult{kind: "error", class: "unknown command"}
	path := []string{p.commandName(level)}
	node := visibleSubcommand(lv.cmd, lv.args[0])
	if node == nil {
		hidden := lv.cmd.Command(lv.args[0])
		if hidden == nil {
			return unknown
		}
		return lineResult{kind: "help", cmd: p.commandName(level) + " " + hidden.Name}
	}
	path = append(path, node.Name)
	for _, word := range lv.args[1:] {
		if strings.HasPrefix(word, "-") || len(node.VisibleCommands()) == 0 {
			break
		}
		next := visibleSubcommand(node, word)
		if next == nil {
			if hidden := node.Command(word); hidden != nil {
				path = append(path, hidden.Name)
				return lineResult{kind: "help", cmd: strings.Join(path, " ")}
			}
			return unknown
		}
		node = next
		path = append(path, node.Name)
	}
	return lineResult{kind: "help", cmd: strings.Join(path, " ")}
}

// modelResult reads parsedLine's result in the shape urfave's is recorded
// in.
func modelResult(p parsedLine) lineResult {
	end := p.urfaveEnd
	lv := p.levels[end.level]
	res := lineResult{cmd: p.commandName(end.level)}
	switch end.kind {
	case lineRuns:
		res.kind, res.args = "runs", lv.args
		for _, l := range p.levels[:end.level+1] {
			for _, f := range l.flags[:l.own] {
				for _, name := range f.names {
					if p.isSet(end.level, name) && !slices.Contains(res.set, name) {
						res.set = append(res.set, name)
					}
				}
			}
		}
		slices.Sort(res.set)
	case lineHelp:
		return modelHelp(p, end.level, end.afterError)
	case lineVersion:
		res = lineResult{kind: "version"}
	case lineError:
		res.kind = "error"
		res.class = [...]string{"", "undefined flag", "missing value", "repeated flag", "bad bool value"}[end.class]
	}
	return res
}

// normalized returns res with no empty slices, so a result with no
// arguments equals one built another way.
func (res lineResult) normalized() lineResult {
	if len(res.args) == 0 {
		res.args = nil
	}
	if len(res.set) == 0 {
		res.set = nil
	}
	return res
}

// lineShape is a few words typed on a command line and the rule of urfave's
// that they exercise.
type lineShape struct {
	rule  string
	words []string
}

// shapes are the words every command is run with, after its name and, for
// the commands that name others, ahead of it and between its names.
var shapes = []lineShape{
	{"a blank word is an argument", []string{""}},
	{"a space is an argument", []string{" "}},
	{"a tab is an argument", []string{"\t"}},
	{"a word is an argument", []string{"x"}},
	{"a padded word is an argument as typed", []string{" x "}},
	{"two blank words then a lone -", []string{"", "", "-"}},
	{"two blank words, a lone - and a word", []string{"", "", "-", "x"}},
	{"a blank word then a lone -", []string{"", "-"}},
	{"a blank word then an undefined flag", []string{"", "--bogus"}},
	{"a blank word then a command name", []string{"", "get"}},
	{"a blank word then the help flag", []string{"", "--help"}},
	{"a space, a lone - and a word", []string{" ", "-", "x"}},
	{"a command name", []string{"get"}},
	{"a padded command name is an unknown command", []string{" get"}},
	{"an unknown command", []string{"nosuch"}},
	{"a lone - keeps parsing at a leaf", []string{"-"}},
	{"a padded lone - is read as one", []string{" - "}},
	{"a lone - with a tab", []string{"\t-"}},
	{"a lone - and a word", []string{"-", "x"}},
	{"a padded lone - and a word", []string{" - ", "x"}},
	{"a lone - and the help flag", []string{"-", "--help"}},
	{"a lone - and an undefined flag", []string{"-", "--bogus"}},
	{"a - and a space and a word", []string{"- x"}},
	{"-- ends the flags", []string{"--"}},
	{"a padded -- ends the flags", []string{" -- "}},
	{"words after -- are arguments", []string{"--", "--help"}},
	{"a lone - after --", []string{"--", "-", "x"}},
	{"a flag after --", []string{"--", "--yes="}},
	{"a number is not a flag", []string{"-1"}},
	{"words after a number are arguments", []string{"-1", "--help"}},
	{"a dash and a symbol is not a flag", []string{"-€"}},
	{"a dash and an accented letter is a flag", []string{"-\u00e9"}},
	{"three dashes cut two", []string{"---x"}},
	{"three dashes before a defined flag", []string{"---yes"}},
	{"three dashes before the help flag", []string{"---help"}},
	{"an empty flag name", []string{"--=x"}},
	{"a dash and = is not a flag", []string{"-="}},
	{"the help flag", []string{"--help"}},
	{"the short help flag", []string{"-h"}},
	{"the help flag with one dash", []string{"-help"}},
	{"the help flag off", []string{"--help=false"}},
	{"the help flag with an empty value", []string{"--help="}},
	{"the help flag with a blank value", []string{"--help= "}},
	{"the short help flag with an empty value", []string{"-h="}},
	{"the help flag on", []string{"--help=true"}},
	{"the version flag", []string{"--version"}},
	{"the short version flag", []string{"-v"}},
	{"the version flag off", []string{"--version=false"}},
	{"the version flag with a blank value", []string{"--version= "}},
	{"an undefined flag", []string{"--bogus"}},
	{"a bool flag", []string{"--yes"}},
	{"a bool flag with an empty value", []string{"--yes="}},
	{"a bool flag with a blank value", []string{"--yes= "}},
	{"a bool flag with a tab value", []string{"--yes=\t"}},
	{"a bool flag off", []string{"--yes=false"}},
	{"a bool flag with a value that is not a bool", []string{"--yes=maybe"}},
	{"a bool value as its own word is an argument", []string{"--yes", "false"}},
	{"a value flag with no value", []string{"--output"}},
	{"a value flag and its value", []string{"-o", "json"}},
	{"a value flag joined to its value", []string{"-o=json"}},
	{"a value flag with a lone - as its value", []string{"-o", "-"}},
	{"a value flag with the help flag as its value", []string{"-o", "--help"}},
	{"a padded flag", []string{" --yes "}},
}

// coreShapes are the words paired with each other on the commands that get
// pairs.
var coreShapes = []lineShape{
	{"a word", []string{"x"}},
	{"a lone -", []string{"-"}},
	{"a padded lone -", []string{" - "}},
	{"--", []string{"--"}},
	{"a number", []string{"-1"}},
	{"the help flag", []string{"--help"}},
	{"the help flag off", []string{"--help=false"}},
	{"the version flag", []string{"--version"}},
	{"an undefined flag", []string{"--bogus"}},
	{"a bool flag with a blank value", []string{"--yes= "}},
	{"a bool flag with an empty value", []string{"--yes="}},
	{"a bool flag", []string{"--yes"}},
	{"a value flag and its value", []string{"-o", "json"}},
	{"a command name", []string{"get"}},
	{"an unknown command", []string{"nosuch"}},
}

// flagKind returns the shape of f: whether it takes a value and whether it
// may be given once.
func flagKind(f ucli.Flag) (takesValue, once bool) {
	if df, ok := f.(ucli.DocGenerationFlag); ok {
		takesValue = df.TakesValue()
	}
	switch f := f.(type) {
	case *ucli.StringFlag:
		once = f.OnlyOnce
	case *ucli.BoolFlag:
		once = f.OnlyOnce
	case *ucli.StringSliceFlag:
		once = f.OnlyOnce
	}
	return takesValue, once
}

// flagShapes returns the lines that spell each of flags every way urfave
// reads: a value flag with its value after it, joined to it and by its
// other name, with no value at the end of the line; a bool flag bare, with
// an empty value, with false, with a blank value and by its other name; a
// flag that may be given once given twice.
func flagShapes(flags []ucli.Flag) []lineShape {
	var out []lineShape
	missing := false
	for _, f := range flags {
		names := f.Names()
		long := "--" + names[0]
		other := "-" + names[len(names)-1]
		takesValue, once := flagKind(f)
		if takesValue {
			out = append(out,
				lineShape{"a value flag and its value", []string{long, "v"}},
				lineShape{"a value flag joined to its value", []string{long + "=v"}},
				lineShape{"a value flag by its other name", []string{other, "v"}},
			)
			if !missing {
				missing = true
				out = append(out, lineShape{"a value flag with no value at the end", []string{long}})
			}
			if once {
				out = append(out, lineShape{"a flag given once, given twice", []string{long, "v", long, "w"}})
			}
			continue
		}
		out = append(out,
			lineShape{"a bool flag", []string{long}},
			lineShape{"a bool flag with an empty value", []string{long + "="}},
			lineShape{"a bool flag off", []string{long + "=false"}},
			lineShape{"a bool flag with a blank value", []string{long + "= "}},
			lineShape{"a bool flag by its other name", []string{other}},
		)
		if once {
			out = append(out, lineShape{"a flag given once, given twice", []string{long, long}})
		}
	}
	return out
}

// lineCase is one command line, with the rule it exercises.
type lineCase struct {
	rule string
	line []string
}

// blankShapes are a blank word and a padded blank word, which count toward
// the number of arguments a root or a group takes before it stops reading
// flags.
var blankShapes = []lineShape{
	{"a blank word", []string{""}},
	{"a space", []string{" "}},
}

// lineTier says which lines a command is run with, besides the spellings of
// its own flags: the shapes after its name and ahead of its names, the
// spellings of the global flags, pairs of shapes, and pairs of a blank word
// and another shape.
type lineTier struct {
	shapes, globals, pairs, blankPairs bool
}

// casesFor returns the lines the test runs on the command at path.
func casesFor(path []string, cmd *ucli.Command, rootFlags []ucli.Flag, tier lineTier) []lineCase {
	seen := map[string]bool{}
	var out []lineCase
	add := func(rule string, words ...[]string) {
		line := slices.Concat(words...)
		key := strings.Join(line, "\x00")
		if !seen[key] {
			seen[key] = true
			out = append(out, lineCase{rule, line})
		}
	}
	if tier.shapes {
		for _, s := range shapes {
			add(s.rule, path, s.words)
		}
		for _, s := range slices.Concat(coreShapes, blankShapes) {
			for k := range len(path) {
				add(s.rule+" ahead of a name", path[:k], s.words, path[k:])
			}
		}
	}
	pairShapes := append(slices.Clone(coreShapes[:9]), blankShapes...)
	if tier.pairs {
		for _, a := range pairShapes {
			for _, b := range pairShapes {
				add(a.rule+", then "+b.rule, path, a.words, b.words)
			}
		}
	}
	if tier.blankPairs {
		for _, a := range blankShapes {
			for _, b := range pairShapes {
				add(a.rule+", then "+b.rule, path, a.words, b.words)
				add(b.rule+", then "+a.rule, path, b.words, a.words)
			}
		}
	}
	if tier.globals {
		for _, s := range flagShapes(rootFlags) {
			add(s.rule, path, s.words)
			add(s.rule+" ahead of the names", s.words, path)
		}
	}
	for _, s := range flagShapes(cmd.Flags) {
		add(s.rule, path, s.words)
	}
	return out
}

// allCommands lists every command of the tree under root with the words that
// name it, hidden ones included.
func allCommands(root *ucli.Command, path []string) (out []struct {
	path []string
	cmd  *ucli.Command
}) {
	for _, sub := range root.Commands {
		p := append(slices.Clone(path), sub.Name)
		out = append(out, struct {
			path []string
			cmd  *ucli.Command
		}{p, sub})
		out = append(out, allCommands(sub, p)...)
	}
	return out
}

// TestParseLineAgreesWithUrfave asserts that parseLine reads a command line
// as urfave does: for a set of lines on the root, every group and every
// leaf, the command urfave runs or the level and kind of the error it
// reports, or whether it shows help or the version, and for a command that
// runs its arguments and its set flags, are the ones parseLine reads. The
// lines hold a blank word, padded words, a lone dash, two dashes, a number,
// a dash and a symbol, the help and version flags, undefined flags and
// every flag of every command spelled each way urfave reads it. The
// lines run on a new root through root.Run directly, with the actions and
// hooks of every command replaced by recorders, so what urfave does with
// the line is not changed by the checks of the entry point. Not parallel:
// it replaces urfave's exit function, help and version printers, and sets
// the process environment and the captured command line.
func TestParseLineAgreesWithUrfave(t *testing.T) {
	restoreRawArgv(t)
	t.Setenv(config.EnvConfigDir, "")
	require.NoError(t, os.Unsetenv(config.EnvConfigDir))
	origExiter, origHelp, origVersion := ucli.OsExiter, ucli.HelpPrinter, ucli.VersionPrinter
	t.Cleanup(func() { ucli.OsExiter, ucli.HelpPrinter, ucli.VersionPrinter = origExiter, origHelp, origVersion })
	ucli.OsExiter = func(int) {}
	ucli.HelpPrinter = func(w io.Writer, _ string, data any) {
		if c, ok := data.(*ucli.Command); ok {
			_, _ = fmt.Fprintf(w, "help %s\n", c.FullName())
		}
	}
	ucli.VersionPrinter = func(c *ucli.Command) { _, _ = fmt.Fprintln(c.Root().Writer, "version") }
	// urfave reports an unknown command it meets while showing help through
	// renderRawArgvError, which reads the captured line for the format.
	SetRawArgv([]string{"-o", "json"})

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	commands := allCommands(root, nil)
	keys := map[string]bool{}
	for _, name := range []string{"get pod", "create project"} {
		keys[name] = true
	}
	var withSlice, withOnce string
	for _, c := range commands {
		if len(c.cmd.Commands) > 0 {
			continue
		}
		for _, f := range c.cmd.Flags {
			_, once := flagKind(f)
			if _, isSlice := f.(*ucli.StringSliceFlag); isSlice && withSlice == "" {
				withSlice = strings.Join(c.path, " ")
			}
			if once && withOnce == "" {
				withOnce = strings.Join(c.path, " ")
			}
		}
	}
	keys[withSlice], keys[withOnce] = true, true
	require.NotEmpty(t, withSlice, "a leaf with a string slice flag")
	require.NotEmpty(t, withOnce, "a leaf with a flag that may be given once")

	type target struct {
		path []string
		tier lineTier
	}
	targets := []target{{nil, lineTier{shapes: true, globals: true, pairs: true}}}
	for _, c := range commands {
		name := strings.Join(c.path, " ")
		tier := lineTier{}
		if len(c.cmd.Commands) > 0 || keys[name] {
			tier.shapes = true
			tier.globals = name == "get" || keys[name]
			tier.pairs = tier.globals
			tier.blankPairs = !tier.pairs && len(c.cmd.Commands) > 0
		}
		targets = append(targets, target{c.path, tier})
	}

	total := 0
	for _, tg := range targets {
		cmd := root
		for _, name := range tg.path {
			cmd = cmd.Command(name)
		}
		cases := casesFor(tg.path, cmd, root.Flags, tg.tier)
		total += len(cases)
		t.Run(strings.Join(append([]string{"rancher"}, tg.path...), " "), func(t *testing.T) {
			t.Parallel()
			model, err := BuildRoot("v-test")
			require.NoError(t, err)
			for _, tc := range cases {
				label := fmt.Sprintf("rancher %q (%s)", tc.line, tc.rule)
				p := parseLine(model, tc.line)
				want := modelResult(p).normalized()
				again := modelResult(parseLine(model, tc.line)).normalized()
				assert.Equal(t, want, again, "%s: parsing a line twice on one tree", label)
				got, rec := runOnUrfave(t, tc.line)
				got = got.normalized()
				if !assert.Equal(t, want, got, label) {
					continue
				}
				if got.kind != "runs" || len(rec.ran.Commands) > 0 {
					continue
				}
				_, tail := scanLine(tc.line, rec.ran)
				if hasBareDash(tail) {
					continue
				}
				var plain []string
				for _, w := range tail {
					if w.kind == plainWord {
						plain = append(plain, w.text)
					}
				}
				assert.Equal(t, got.args, lineResult{args: plain}.normalized().args, "%s: the plain words of the line scan", label)
			}
		})
	}
	t.Logf("%d command lines on %d commands", total, len(targets))
}
