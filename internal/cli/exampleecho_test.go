package cli

import (
	"regexp"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// echoOptionPattern matches echo at a command position of POSIX shell text,
// followed by an option word: a dash and then only the letters n, e and E,
// with or without one quote around it. The command positions it knows are the
// start of a line, and the place right after (white space allowed) one of ; &
// | ( ) { a backtick, !, a single or double quote (the quote that opens
// sh -c), or one of the words then, do, else, elif, if, while, until, time,
// command, env, sudo and xargs. Such a word counts only at the start of a line
// or after white space, ; & | ( ) { or a backtick. A flag name such as
// --command, or a name such as a-time that ends in one of these words, does
// not count. Another prefix of a command (exec, builtin, nohup) is not in the
// set; add it, with a row of TestEchoOptionCheckRows, when an example needs
// it. White space alone is not a command position: echo after any other word
// is read as an argument (a Pod named echo). The option word ends at white
// space, a separator, a closing parenthesis, brace or backtick, or the end of
// the line.
var echoOptionPattern = regexp.MustCompile("(?m)(?:^|[;&|(){`!'\"]|(?:^|[\\s;&|(){`])(?:then|do|else|elif|if|while|until|time|command|env|sudo|xargs)\\b)\\s*echo\\s+[\"']?-[neE]+[\"']?(?:[\\s;&|)}`]|$)")

// commandExample is one example string and the command that has it.
type commandExample struct{ command, text string }

// echoOptionProblems returns the problems of one example: a use of echo with
// -n, -e or -E. Under a /bin/sh whose echo takes no options (the macOS one)
// such an echo prints the option as text. A password piped through it gains
// a -n and a newline. The check reads POSIX shell text only; it does not run
// a shell and does not handle PowerShell, whose echo is an alias of
// Write-Output. A plain echo with no option is allowed here.
func echoOptionProblems(example string) []string {
	if echoOptionPattern.MatchString(example) {
		return []string{"runs echo with an option; use printf '%s' to print a value"}
	}
	return nil
}

// echoExampleGaps returns one problem per example that echoOptionProblems
// reports and allowed has no entry for; one per entry of allowed that is not
// the text of any example; and one per entry that is the text of an example
// and has no reason. To fix a real use of echo with an option, replace it with
// printf '%s'. Add an entry only for a text the pattern matches by mistake.
func echoExampleGaps(examples []commandExample, allowed map[string]string) []string {
	seen := map[string]bool{}
	var problems []string
	for _, e := range examples {
		seen[e.text] = true
		if _, ok := allowed[e.text]; ok {
			continue
		}
		for _, p := range echoOptionProblems(e.text) {
			problems = append(problems, "example of "+e.command+" "+p+": "+e.text)
		}
	}
	for text, reason := range allowed {
		switch {
		case !seen[text]:
			problems = append(problems, "the allowed list has an entry that no command's examples contain: "+text)
		case reason == "":
			problems = append(problems, "the allowed entry for the example has no reason: "+text)
		}
	}
	slices.Sort(problems)
	return problems
}

// echoExampleAllowed names the examples that echoOptionPattern matches by
// mistake, with the reason. It is empty by design: a real use of echo with an
// option is replaced by printf '%s', and the pattern is never loosened to
// make an example pass.
var echoExampleAllowed = map[string]string{}

// TestNoExampleRunsEchoWithAnOption asserts that no example of any command
// runs echo with -n, -e or -E, and that the scan read examples. A plain echo
// with no option is allowed.
func TestNoExampleRunsEchoWithAnOption(t *testing.T) {
	t.Parallel()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	var examples []commandExample
	for _, c := range m.Commands {
		for _, e := range c.Examples {
			examples = append(examples, commandExample{c.Name, e})
		}
	}
	require.Positive(t, len(examples), "the scan read no example")
	assert.Empty(t, echoExampleGaps(examples, echoExampleAllowed))
}

// TestEchoOptionCheckRows asserts that the check reports echo with an option
// at each command position that echoOptionPattern lists. It reports nothing
// for echo as an argument or value, a plain echo, printf or an empty text. It
// also asserts that the allowed list allows an example only when its entry
// has a reason, and that an entry that is not the text of any example is
// reported.
func TestEchoOptionCheckRows(t *testing.T) {
	t.Parallel()
	fail := []struct{ name, text string }{
		{"echo -n piped", `echo -n "$P" | rancher x`},
		{"echo -e", `echo -e x`},
		{"echo -ne", `echo -ne x`},
		{"echo -En", `echo -En x`},
		{"echo -n in double quotes", `echo "-n" x`},
		{"echo -n in single quotes", `echo '-n' x`},
		{"echo -n then a semicolon", `echo -n;x`},
		{"echo -n then a pipe", `echo -n|x`},
		{"after &&", `true && echo -n y`},
		{"after a semicolon", `true;echo -n y`},
		{"after a pipe", `x|echo -n y`},
		{"in parentheses", `(echo -n y)`},
		{"in a command substitution", `echo $(echo -n y)`},
		{"in sh -c", `sh -c 'echo -n y'`},
		{"on a later line", "set -e\necho -n y"},
		{"after else", `if a; then b; else echo -n y; fi`},
		{"after elif", `if a; then b; elif echo -n y; then c; fi`},
		{"after time", `time echo -n y`},
		{"after command", `command echo -n y`},
		{"after env", `env echo -n y`},
		{"after sudo", `sudo echo -n y`},
		{"as the command of xargs", `printf '%s' x | xargs echo -n`},
		{"after then, with no space before it", `if a;then echo -n y; fi`},
		{"after if", `if echo -n y; then z; fi`},
		{"after while", `while echo -n y; do z; done`},
		{"after until", `until echo -n y; do z; done`},
		{"after do", `for i in a; do echo -n y; done`},
		{"after !", `! echo -n y`},
		{"after &", `true & echo -n y`},
		{"after {", `{ echo -n y; }`},
		{"after a backtick", "x=`echo -n y`"},
		{"after a closing parenthesis", `case x in a) echo -n y;; esac`},
		{"after a double quote", `sh -c "echo -n y"`},
	}
	for _, tt := range fail {
		t.Run("fails "+tt.name, func(t *testing.T) {
			t.Parallel()
			assert.NotEmpty(t, echoOptionProblems(tt.text))
		})
	}
	pass := []struct{ name, text string }{
		{"printf", `printf '%s' "$P" | rancher x`},
		{"plain echo", `echo "$P" | rancher x`},
		{"echo as a Secret name", `rancher create secret echo --from-literal a=b`},
		{"echo as a Pod name", `rancher logs echo -n kube-system`},
		{"echo after the value of --command", `rancher exec p --command echo -n`},
		{"echo after the value of --env", `kubectl run x --env echo -n`},
		{"echo after a name that ends in time", `a-time echo -n y`},
		{"empty", ``},
	}
	for _, tt := range pass {
		t.Run("passes "+tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Empty(t, echoOptionProblems(tt.text))
		})
	}

	const bad = `echo -n "$P" | rancher x`
	examples := []commandExample{{"x", bad}}
	t.Run("an entry with a reason allows its example", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, echoExampleGaps(examples, map[string]string{bad: "a match by mistake"}))
	})
	t.Run("an example with no entry fails", func(t *testing.T) {
		t.Parallel()
		require.Len(t, echoExampleGaps(examples, nil), 1)
	})
	t.Run("an entry that is not the text of any example fails", func(t *testing.T) {
		t.Parallel()
		got := echoExampleGaps(nil, map[string]string{bad: "a match by mistake"})
		require.Len(t, got, 1)
		assert.Contains(t, got[0], "no command's examples contain")
	})
	t.Run("an entry with no reason fails", func(t *testing.T) {
		t.Parallel()
		got := echoExampleGaps(examples, map[string]string{bad: ""})
		require.Len(t, got, 1)
		assert.Contains(t, got[0], "has no reason")
	})
}
