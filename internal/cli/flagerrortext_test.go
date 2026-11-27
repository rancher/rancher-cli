package cli

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	flagErrorPointer = "; pass --help for usage"
	flagErrorHint    = "rancher schema"
)

// flagErrorOf parses the command line argv (the arguments after rancher), which
// it also sets as the captured command line, and returns the message and the
// hint of the flag error the line ends in.
func flagErrorOf(t *testing.T, root *Command, argv []string) (msg, hint string) {
	t.Helper()
	rawArgv = argv
	p := parseLine(root, argv)
	require.Equal(t, lineError, p.end.kind, "the line %q ends in a flag error", argv)
	lv := p.levels[p.end.level]
	return usageErrorMessage(lv.cmd, lv.usageErr)
}

// flagTree is a tree of its own: a leaf with a descriptor that takes no secret
// input, a bool flag yes and a flag quota-limit that may be given once.
func flagTree() *Command {
	leaf := &Command{
		Name:       "x",
		descriptor: &runtime.Descriptor{Name: "x"},
		Flags: []*Flag{
			{Name: "yes", Kind: boolFlag},
			{Name: "quota-limit", Kind: stringFlag, Once: true},
		},
	}
	return link(&Command{Name: "rancher", StopAtFirstWord: true, Commands: []*Command{leaf}})
}

// TestUsageErrorTextNamesTheFlagAsTyped asserts the whole message of the flag
// errors of a leaf that takes no secret input: an undefined flag, a bool flag
// with a bad value and a flag that may be given once, given twice, name the
// flag as typed (the whitespace around the argument and everything from the
// first "=" cut off, one or two dashes as typed) and end with "pass --help for
// usage"; the refusal of an empty bool value, which the command-line checks
// read and the message function never sees, keeps its text. Not parallel: the
// functions under test read the captured command line, which is process-wide.
func TestUsageErrorTextNamesTheFlagAsTyped(t *testing.T) {
	restoreRawArgv(t)
	root := flagTree()

	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"an undefined flag", []string{"x", "--x"}, "flag provided but not defined: --x"},
		{"an undefined flag typed with one dash", []string{"x", "-x"}, "flag provided but not defined: -x"},
		{"an undefined flag typed with a value", []string{"x", "--x=v"}, "flag provided but not defined: --x"},
		{"an undefined flag typed with three dashes", []string{"x", "---x"}, "flag provided but not defined: ---x"},
		{"an undefined flag with an empty name and a value", []string{"x", "--=x"}, "flag provided but not defined: --"},
		{"an undefined flag with whitespace around it", []string{"x", " --x "}, "flag provided but not defined: --x"},
		{"a bool flag with a bad value", []string{"x", "--yes=maybe"}, "invalid value for flag --yes: the value must be true or false"},
		{"a bool flag with a bad value, typed with one dash", []string{"x", "-yes=maybe"}, "invalid value for flag -yes: the value must be true or false"},
		{"a flag given twice", []string{"x", "--quota-limit", "a", "--quota-limit", "b"}, "--quota-limit may be given once"},
		{"a flag given twice, the second typed with one dash", []string{"x", "--quota-limit", "a", "-quota-limit", "b"}, "-quota-limit may be given once"},
		{"a flag given twice, each typed with a value after =", []string{"x", "--quota-limit=a", "--quota-limit=b"}, "--quota-limit may be given once"},
		{"a flag typed without its value", []string{"x", "--quota-limit"}, "flag needs an argument: --quota-limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, hint := flagErrorOf(t, root, tc.argv)
			assert.Equal(t, tc.want+flagErrorPointer, msg)
			assert.Equal(t, flagErrorHint, hint)
		})
	}

	t.Run("a bool flag typed with = and a blank value", func(t *testing.T) {
		argv := []string{"x", "--yes= "}
		rawArgv = argv
		ce, _ := lineRefusal(parseLine(root, argv))
		require.NotNil(t, ce)
		assert.Equal(t, "--yes= has an empty value; a bool flag typed with = takes true or false", ce.Message)
	})
}

// TestEveryFlagErrorNamesTheFlagAsTyped asserts, for every flag error a command
// line of the tree can end in, that the message names the flag as typed and
// ends with "pass --help for usage", that the hint is "rancher schema", and that
// no message holds a typed value. Every expected message is a literal built
// here from the line the test types, never a call of the code under test. It
// reads parseLine and the message function, not a run of the line. The
// built-in --help, -h, --version and -v flags are out of scope: they are added
// by link, not by a descriptor or the global flags, and their value forms show
// the help page or the version. Not parallel: the functions under test read the
// captured command line, which is process-wide.
func TestEveryFlagErrorNamesTheFlagAsTyped(t *testing.T) {
	restoreRawArgv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	const canary = "zz-flag-error-canary-q9"
	started := time.Now()
	cases, canaryCases, refusedCases, onceCases := 0, 0, 0, 0
	check := func(argv []string, want string) {
		t.Helper()
		cases++
		msg, hint := flagErrorOf(t, root, argv)
		assert.Equal(t, want+flagErrorPointer, msg, "line %q", argv)
		assert.Equal(t, flagErrorHint, hint, "line %q", argv)
		assert.NotContains(t, msg, canary, "line %q", argv)
		if slices.ContainsFunc(argv, func(w string) bool { return strings.Contains(w, canary) }) {
			canaryCases++
		}
	}

	const noAllNamespaces = "there is no all-namespaces flag"
	const getDetail = noAllNamespaces + " - a get command lists every namespace when you omit --namespace, except for a kind with a default namespace (fleet-default); use --namespace for a kind that takes one"
	line := func(words ...string) []string { return words }
	fixed := []struct {
		argv []string
		want string
	}{
		{line("--zz"), "unknown flag at argument 1 after rancher"},
		{line("get", "--zz"), "unknown flag at argument 2 after rancher"},
		{line("get", "--zz", "pod"), "flag provided but not defined: --zz"},
		{line("get", "--zz", "v", "pod"), "unknown flag at argument 2 after rancher"},
		{line("get", "---zz"), "unknown flag"},
		{line("get", "pod", "--zz"), "flag provided but not defined: --zz"},
		{line("get", "pod", "-zz"), "flag provided but not defined: -zz"},
		{line("get", "pod", " --zz "), "flag provided but not defined: --zz"},
		{line("get", "pod", "---zz"), "flag provided but not defined: ---zz"},
		{line("get", "pod", "--zz="+canary), "flag provided but not defined: --zz"},
		{line("get", "pod", "-zz="+canary), "flag provided but not defined: -zz"},
		{line("create", "user", "bob", "--zz"), "unknown flag at argument 4 after rancher"},
		{line("create", "user", "bob", "-zz"), "unknown flag at argument 4 after rancher"},
		{line("create", "user", "bob", " --zz"), "unknown flag"},
		{line("create", "user", "bob", "---zz"), "unknown flag"},
		{line("get", "pod", "-A"), "flag provided but not defined: -A; " + getDetail},
		{line("get", "pod", "--all-namespaces"), "flag provided but not defined: --all-namespaces; " + getDetail},
		{line("access", "-A", "list"), "flag provided but not defined: -A; " + noAllNamespaces},
		{line("get", "-A"), "unknown flag at argument 2 after rancher; " + getDetail},
	}
	for _, tc := range fixed {
		check(tc.argv, tc.want)
	}

	var walk func(c *Command, path []string)
	walk = func(c *Command, path []string) {
		d, described := commandDescriptor(c)
		leaf := len(c.Commands) == 0
		secret := described && d.SecretInput
		at := func(words ...string) []string { return append(slices.Clone(path), words...) }

		for _, typed := range []string{"--zz", "-zz"} {
			if leaf && described && !secret {
				check(at(typed), "flag provided but not defined: "+typed)
			} else {
				check(at(typed), "unknown flag at argument "+strconv.Itoa(len(path)+1)+" after rancher")
			}
		}

		for _, f := range typeableFlags(c) {
			for _, name := range f.Names() {
				if name == "" {
					continue
				}
				spellings := []string{"--" + name, "-" + name}
				if utf8.RuneCountInString(name) == 1 {
					spellings = []string{"-" + name, "--" + name}
				}
				for _, typed := range spellings {
					switch f.Kind {
					case boolFlag:
						bad := "invalid value for flag " + typed + ": the value must be true or false"
						check(at(typed+"=maybe"), bad)
						if secret {
							check(at(typed+"="+canary), bad)
						}
					default:
						check(at(typed), "flag needs an argument: "+typed)
						if f.Once {
							once := typed + " may be given once"
							check(at(typed, "a", typed, "b"), once)
							onceCases++
							if secret {
								check(at(typed+"="+canary, typed+"="+canary), once)
							}
						}
					}
				}
			}
		}

		if leaf && described {
			for _, r := range d.RefusedFlags {
				spellings := []string{"--" + r.Name, "-" + r.Name}
				if utf8.RuneCountInString(r.Name) == 1 {
					spellings = []string{"-" + r.Name, "--" + r.Name}
				}
				for _, typed := range spellings {
					check(at(typed), typed+": "+r.Text)
					refusedCases++
				}
			}
		}

		for _, sub := range c.Commands {
			walk(sub, append(slices.Clone(path), sub.Name))
		}
	}
	walk(root, nil)
	assert.NotZero(t, canaryCases, "no line typed the canary as a value")
	assert.NotZero(t, onceCases, "no flag that may be given once was typed twice")
	assert.NotZero(t, refusedCases, "no refused flag was typed")
	t.Logf("%d flag error lines checked in %v", cases, time.Since(started))
}

// typeableFlags returns the flags a command line typed at c can set: the flags c
// has, then the global flags of the root that c does not shadow. The built-in
// help and version flags are left out.
func typeableFlags(c *Command) []*Flag {
	var out []*Flag
	taken := map[string]bool{}
	add := func(flags []*Flag) {
		var batch []*Flag
		for _, f := range flags {
			if f.Local || slices.ContainsFunc(f.Names(), func(name string) bool { return taken[name] }) {
				continue
			}
			batch = append(batch, f)
		}
		for _, f := range batch {
			for _, name := range f.Names() {
				taken[name] = true
			}
		}
		out = append(out, batch...)
	}
	add(c.Flags)
	if root := c.Root(); root != c {
		add(root.Flags)
	}
	return out
}
