package runtime

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingRead is a read command that counts its runs.
type countingRead struct {
	d    Descriptor
	runs *int
}

func (c countingRead) Describe() Descriptor { return c.d }
func (c countingRead) Execute(context.Context, Input, Prompter) (Result, error) {
	*c.runs++
	return Result{Kind: "OK", Text: "ok\n"}, nil
}

// TestValidateInputRefusesExtraArguments asserts that the pipeline refuses,
// before the command runs, more positional arguments than the command
// declares: a message stating what the command takes and quoting the first
// five extra words with the count of the rest, and the command's help as
// the hint. As many arguments as declared still run the command.
func TestValidateInputRefusesExtraArguments(t *testing.T) {
	t.Parallel()
	none := Descriptor{Name: "config path", CommandType: Local}
	one := Descriptor{Name: "create user", CommandType: Local, Args: []ArgSpec{{Name: "username", Type: "string", Required: true}}}
	secretOne := one
	secretOne.SecretInput = true
	optional := Descriptor{Name: "access can", CommandType: Local, Args: []ArgSpec{
		{Name: "verb", Type: "string", Required: true},
		{Name: "resource", Type: "string"},
	}}
	two := Descriptor{Name: "oidc-client remove-secret", CommandType: Local, Args: []ArgSpec{
		{Name: "client", Type: "string", Required: true},
		{Name: "secret", Type: "string", Required: true},
	}}
	many := make([]string, 3000)
	for i := range many {
		many[i] = fmt.Sprint(i + 1)
	}

	for _, tc := range []struct {
		name    string
		d       Descriptor
		in      Input
		message string
		hint    string
	}{
		{
			name:    "no arguments declared",
			d:       none,
			in:      mapInput{a: []string{"extra"}},
			message: "config path takes no arguments; unexpected argument 'extra'",
			hint:    "rancher config path --help",
		},
		{
			name:    "one declared, two extra, quoted",
			d:       one,
			in:      mapInput{a: []string{"u1", "b", "it's"}},
			message: `create user takes 1 argument (<username>); unexpected arguments 'b' 'it'\''s'`,
			hint:    "rancher create user --help",
		},
		{
			name:    "a secret input: one extra word named by its position, not its value",
			d:       secretOne,
			in:      mapInput{a: []string{"u1", "s3cretcanary9"}},
			message: "create user takes 1 argument (<username>); unexpected argument at position 2",
			hint:    "rancher create user --help",
		},
		{
			name:    "a secret input: several extra words named by position",
			d:       secretOne,
			in:      mapInput{a: []string{"u1", "s3cretcanary9", "it's"}},
			message: "create user takes 1 argument (<username>); unexpected arguments at positions 2 3",
			hint:    "rancher create user --help",
		},
		{
			name:    "an optional argument declared",
			d:       optional,
			in:      mapInput{a: []string{"get", "pods", "x"}},
			message: "access can takes at most 2 arguments (<verb> [resource]); unexpected argument 'x'",
			hint:    "rancher access can --help",
		},
		{
			name:    "two required declared",
			d:       two,
			in:      mapInput{a: []string{"c", "s", "x"}},
			message: "oidc-client remove-secret takes 2 arguments (<client> <secret>); unexpected argument 'x'",
			hint:    "rancher oidc-client remove-secret --help",
		},
		{
			name:    "a secret input: many extra words, the first five positions and the count of the rest",
			d:       Descriptor{Name: "config path", CommandType: Local, SecretInput: true},
			in:      mapInput{a: many},
			message: "config path takes no arguments; unexpected arguments at positions 1 2 3 4 5 and 2995 more",
			hint:    "rancher config path --help",
		},
		{
			name:    "many extra words: the first five and the count of the rest",
			d:       none,
			in:      mapInput{a: many},
			message: "config path takes no arguments; unexpected arguments '1' '2' '3' '4' '5' and 2995 more",
			hint:    "rancher config path --help",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runs := 0
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), countingRead{d: tc.d, runs: &runs}, tc.in, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.Equal(t, 2, ce.ExitCode())
			assert.Zero(t, runs, "the command must not run")
		})
	}

	for _, tc := range []struct {
		name string
		d    Descriptor
		in   Input
	}{
		{"no arguments", none, mapInput{}},
		{"one declared", one, mapInput{a: []string{"u1"}}},
		{"an optional argument omitted", optional, mapInput{a: []string{"get"}}},
		{"an optional argument given", optional, mapInput{a: []string{"get", "pods"}}},
		{"two declared", two, mapInput{a: []string{"c", "s"}}},
	} {
		t.Run(tc.name+" runs", func(t *testing.T) {
			t.Parallel()
			runs := 0
			var out, errBuf bytes.Buffer
			_, ce := Run(t.Context(), countingRead{d: tc.d, runs: &runs}, tc.in, IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
			require.Nil(t, ce)
			assert.Equal(t, 1, runs)
		})
	}
}

// TestBoolValueErrorNamesEachValueAndItsFlag asserts the refusal of
// positional arguments typed as the separate value of a bool flag: the
// message names each such word, as typed, and its flag, and says a bool flag
// takes its value only as <flag>=<value> and where an argument such as true
// or false goes; the hint is the joined line when the arguments left fill
// every declared one, and the command's help otherwise.
func TestBoolValueErrorNamesEachValueAndItsFlag(t *testing.T) {
	t.Parallel()
	none := Descriptor{Name: "config path", CommandType: Local}
	one := Descriptor{Name: "create user", CommandType: Local, Args: []ArgSpec{{Name: "username", Type: "string", Required: true}}}
	secretOne := one
	secretOne.SecretInput = true
	optional := Descriptor{Name: "access can", CommandType: Local, Args: []ArgSpec{
		{Name: "verb", Type: "string", Required: true},
		{Name: "resource", Type: "string"},
	}}
	boolRule := func(flag string) string {
		return "; a bool flag takes its value only as " + flag + "=<value>. To pass an argument such as true or false, put it before the flags or after --"
	}

	for _, tc := range []struct {
		name    string
		d       Descriptor
		args    []string
		vals    []BoolValue
		line    string
		message string
		hint    string
	}{
		{
			name:    "bool values beyond the declared argument",
			d:       one,
			args:    []string{"u1", "false", "1"},
			vals:    []BoolValue{{Index: 1, Flag: "--a"}, {Index: 2, Flag: "--b"}},
			line:    "rancher create user 'u1' --a='false' --b='1'",
			message: "create user: 'false' was typed after the bool flag --a, '1' after the bool flag --b" + boolRule("--a"),
			hint:    "rancher create user 'u1' --a='false' --b='1'",
		},
		{
			name:    "a bool value in a free required slot: the joined line would lack it",
			d:       one,
			args:    []string{"false"},
			vals:    []BoolValue{{Index: 0, Flag: "--dry-run"}},
			line:    "rancher create user --dry-run='false'",
			message: "create user: 'false' was typed after the bool flag --dry-run" + boolRule("--dry-run"),
			hint:    "rancher create user --help",
		},
		{
			name:    "a bool value in a free optional slot: the joined line would leave it empty",
			d:       optional,
			args:    []string{"get", "true"},
			vals:    []BoolValue{{Index: 1, Flag: "--wait"}},
			line:    "rancher access can 'get' --wait='true'",
			message: "access can: 'true' was typed after the bool flag --wait" + boolRule("--wait"),
			hint:    "rancher access can --help",
		},
		{
			name:    "a bool value with every declared argument filled, the optional one too",
			d:       optional,
			args:    []string{"get", "pods", "true"},
			vals:    []BoolValue{{Index: 2, Flag: "--wait"}},
			line:    "rancher access can 'get' 'pods' --wait='true'",
			message: "access can: 'true' was typed after the bool flag --wait" + boolRule("--wait"),
			hint:    "rancher access can 'get' 'pods' --wait='true'",
		},
		{
			name:    "a bool value where the optional argument is empty",
			d:       optional,
			args:    []string{"get", "", "true"},
			vals:    []BoolValue{{Index: 2, Flag: "--wait"}},
			line:    "rancher access can 'get' '' --wait='true'",
			message: "access can: 'true' was typed after the bool flag --wait" + boolRule("--wait"),
			hint:    "rancher access can --help",
		},
		{
			name:    "a bool value where the required argument is empty",
			d:       one,
			args:    []string{"", "true"},
			vals:    []BoolValue{{Index: 1, Flag: "--wait"}},
			line:    "rancher create user '' --wait='true'",
			message: "create user: 'true' was typed after the bool flag --wait" + boolRule("--wait"),
			hint:    "rancher create user --help",
		},
		{
			name:    "a bool value before the declared argument",
			d:       one,
			args:    []string{"true", "mypod"},
			vals:    []BoolValue{{Index: 0, Flag: "--wait"}},
			line:    "rancher create user --wait='true' 'mypod'",
			message: "create user: 'true' was typed after the bool flag --wait" + boolRule("--wait"),
			hint:    "rancher create user --wait='true' 'mypod'",
		},
		{
			name:    "a secret input: the words are named by position and the hint is the help, not the typed line",
			d:       secretOne,
			args:    []string{"u1", "true"},
			vals:    []BoolValue{{Index: 1, Flag: "--password-stdin"}},
			line:    "rancher create user 'u1' --from-literal='pw=hunter2' --password-stdin='true'",
			message: "create user: argument at position 2 was typed after the bool flag --password-stdin" + boolRule("--password-stdin"),
			hint:    "rancher create user --help",
		},
		{
			name:    "a secret input: an extra word is numbered by its position before the bool values are removed",
			d:       secretOne,
			args:    []string{"u1", "false", "extra"},
			vals:    []BoolValue{{Index: 1, Flag: "--dry-run"}},
			line:    "rancher create user 'u1' --dry-run='false' 'extra'",
			message: "create user: argument at position 2 was typed after the bool flag --dry-run" + boolRule("--dry-run") + ". create user takes 1 argument (<username>); unexpected argument at position 3",
			hint:    "rancher create user --help",
		},
		{
			name:    "a bool value as typed, with whitespace around it",
			d:       one,
			args:    []string{" false", "u1"},
			vals:    []BoolValue{{Index: 0, Flag: "--dry-run"}},
			line:    "rancher create user --dry-run='false' 'u1'",
			message: "create user: ' false' was typed after the bool flag --dry-run" + boolRule("--dry-run"),
			hint:    "rancher create user --dry-run='false' 'u1'",
		},
		{
			name:    "a bool value and an extra word that is not one",
			d:       one,
			args:    []string{"u1", "false", "extra"},
			vals:    []BoolValue{{Index: 1, Flag: "--dry-run"}},
			line:    "rancher create user 'u1' --dry-run='false' 'extra'",
			message: "create user: 'false' was typed after the bool flag --dry-run" + boolRule("--dry-run") + ". create user takes 1 argument (<username>); unexpected argument 'extra'",
			hint:    "rancher create user --help",
		},
		{
			name: "more than five bool values",
			d:    none,
			args: []string{"1", "1", "1", "1", "1", "1", "1"},
			vals: []BoolValue{{Index: 0, Flag: "--a"}, {Index: 1, Flag: "--a"}, {Index: 2, Flag: "--a"}, {Index: 3, Flag: "--a"},
				{Index: 4, Flag: "--a"}, {Index: 5, Flag: "--a"}, {Index: 6, Flag: "--a"}},
			line: "rancher config path --a='1' --a='1' --a='1' --a='1' --a='1' --a='1' --a='1'",
			message: "config path: '1' was typed after the bool flag --a, '1' after the bool flag --a, '1' after the bool flag --a, " +
				"'1' after the bool flag --a, '1' after the bool flag --a and 2 more after bool flags" + boolRule("--a"),
			hint: "rancher config path --a='1' --a='1' --a='1' --a='1' --a='1' --a='1' --a='1'",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := BoolValueError(tc.d, tc.args, tc.vals, tc.line)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.Equal(t, 2, ce.ExitCode())
		})
	}
}

// TestDroppedWordsCountErrorNamesNoWord asserts the refusal of words after a
// "-" on its own, for a command that takes a secret input, counts them with
// the plain singular or plural and names none.
func TestDroppedWordsCountErrorNamesNoWord(t *testing.T) {
	t.Parallel()
	one := DroppedWordsCountError(1)
	assert.Equal(t, clierr.CodeValidation, one.Code)
	assert.Equal(t, "1 word after a - on its own is not used", one.Message)
	assert.Equal(t, "put - last, or after -- when it is meant as an argument", one.Hint)
	assert.Equal(t, 2, one.ExitCode())
	assert.Equal(t, "3 words after a - on its own are not used", DroppedWordsCountError(3).Message)
}
