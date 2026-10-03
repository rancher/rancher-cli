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

// boolValueInput is a mapInput that also answers BoolValueArgs.
type boolValueInput struct {
	mapInput
	vals []BoolValue
	line string
	ok   bool
}

func (b boolValueInput) BoolValueArgs() ([]BoolValue, string, bool) {
	return b.vals, b.line, b.ok
}

// TestValidateInputRefusesExtraArguments asserts that the pipeline refuses,
// before the command runs, a positional argument the Input reports as the
// separate value of a bool flag, in any position - the message names each
// such word and its flag, says a bool flag takes its value joined with =
// and where an argument that reads as a bool goes, and the hint is the
// Input's joined line when the arguments left fill every declared one,
// and the command's help otherwise - and more positional arguments than
// the command declares: a message stating what the command takes and
// quoting the first five extra words with the count of the rest, and the
// command's help as the hint. As many arguments as declared still
// run the command, also when the Input finds no bool value.
func TestValidateInputRefusesExtraArguments(t *testing.T) {
	t.Parallel()
	none := Descriptor{Name: "config path", CommandType: Local}
	one := Descriptor{Name: "create user", CommandType: Local, Args: []ArgSpec{{Name: "username", Type: "string", Required: true}}}
	optional := Descriptor{Name: "access can", CommandType: Local, Args: []ArgSpec{
		{Name: "verb", Type: "string", Required: true},
		{Name: "resource", Type: "string"},
	}}
	two := Descriptor{Name: "oidc-client remove-secret", CommandType: Local, Args: []ArgSpec{
		{Name: "client", Type: "string", Required: true},
		{Name: "secret", Type: "string", Required: true},
	}}
	const boolRule = "; a bool flag takes its value joined with =, and an argument that reads as a bool goes before the flags or after --"
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
			name:    "many extra words: the first five and the count of the rest",
			d:       none,
			in:      mapInput{a: many},
			message: "config path takes no arguments; unexpected arguments '1' '2' '3' '4' '5' and 2995 more",
			hint:    "rancher config path --help",
		},
		{
			name: "bool values beyond the declared argument",
			d:    one,
			in: boolValueInput{a: []string{"u1", "false", "1"},
				vals: []BoolValue{{Index: 1, Flag: "--a"}, {Index: 2, Flag: "--b"}},
				line: "rancher create user 'u1' --a='false' --b='1'", ok: true},
			message: "create user: 'false' after the bool flag --a, '1' after the bool flag --b" + boolRule,
			hint:    "rancher create user 'u1' --a='false' --b='1'",
		},
		{
			name: "a bool value in a free required slot: the joined line would lack it",
			d:    one,
			in: boolValueInput{a: []string{"false"},
				vals: []BoolValue{{Index: 0, Flag: "--dry-run"}},
				line: "rancher create user --dry-run='false'", ok: true},
			message: "create user: 'false' after the bool flag --dry-run" + boolRule,
			hint:    "rancher create user --help",
		},
		{
			name: "a bool value in a free optional slot: the joined line would leave it empty",
			d:    optional,
			in: boolValueInput{a: []string{"get", "true"},
				vals: []BoolValue{{Index: 1, Flag: "--wait"}},
				line: "rancher access can 'get' --wait='true'", ok: true},
			message: "access can: 'true' after the bool flag --wait" + boolRule,
			hint:    "rancher access can --help",
		},
		{
			name: "a bool value with every declared argument filled, the optional one too",
			d:    optional,
			in: boolValueInput{a: []string{"get", "pods", "true"},
				vals: []BoolValue{{Index: 2, Flag: "--wait"}},
				line: "rancher access can 'get' 'pods' --wait='true'", ok: true},
			message: "access can: 'true' after the bool flag --wait" + boolRule,
			hint:    "rancher access can 'get' 'pods' --wait='true'",
		},
		{
			name: "a bool value where the optional argument is empty",
			d:    optional,
			in: boolValueInput{a: []string{"get", "", "true"},
				vals: []BoolValue{{Index: 2, Flag: "--wait"}},
				line: "rancher access can 'get' '' --wait='true'", ok: true},
			message: "access can: 'true' after the bool flag --wait" + boolRule,
			hint:    "rancher access can --help",
		},
		{
			name: "a bool value where the required argument is empty",
			d:    one,
			in: boolValueInput{a: []string{"", "true"},
				vals: []BoolValue{{Index: 1, Flag: "--wait"}},
				line: "rancher create user '' --wait='true'", ok: true},
			message: "create user: 'true' after the bool flag --wait" + boolRule,
			hint:    "rancher create user --help",
		},
		{
			name: "a bool value before the declared argument",
			d:    one,
			in: boolValueInput{a: []string{"true", "mypod"},
				vals: []BoolValue{{Index: 0, Flag: "--wait"}},
				line: "rancher create user --wait='true' 'mypod'", ok: true},
			message: "create user: 'true' after the bool flag --wait" + boolRule,
			hint:    "rancher create user --wait='true' 'mypod'",
		},
		{
			name: "a bool value and an extra word that is not one",
			d:    one,
			in: boolValueInput{a: []string{"u1", "false", "extra"},
				vals: []BoolValue{{Index: 1, Flag: "--dry-run"}},
				line: "rancher create user 'u1' --dry-run='false' 'extra'", ok: true},
			message: "create user: 'false' after the bool flag --dry-run" + boolRule +
				"; create user takes 1 argument (<username>); unexpected argument 'extra'",
			hint: "rancher create user --help",
		},
		{
			name: "more than five bool values",
			d:    none,
			in: boolValueInput{a: []string{"1", "1", "1", "1", "1", "1", "1"},
				vals: []BoolValue{{Index: 0, Flag: "--a"}, {Index: 1, Flag: "--a"}, {Index: 2, Flag: "--a"}, {Index: 3, Flag: "--a"},
					{Index: 4, Flag: "--a"}, {Index: 5, Flag: "--a"}, {Index: 6, Flag: "--a"}},
				line: "rancher config path --a='1' --a='1' --a='1' --a='1' --a='1' --a='1' --a='1'", ok: true},
			message: "config path: '1' after the bool flag --a, '1' after the bool flag --a, '1' after the bool flag --a, " +
				"'1' after the bool flag --a, '1' after the bool flag --a and 2 more" + boolRule,
			hint: "rancher config path --a='1' --a='1' --a='1' --a='1' --a='1' --a='1' --a='1'",
		},
		{
			name:    "the Input finds no bool values",
			d:       one,
			in:      boolValueInput{a: []string{"u1", "false"}, vals: []BoolValue{{Index: 1, Flag: "--a"}}, line: "unused"},
			message: "create user takes 1 argument (<username>); unexpected argument 'false'",
			hint:    "rancher create user --help",
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
		{"the Input finds no bool values", one, boolValueInput{a: []string{"true"}, vals: []BoolValue{{Index: 0, Flag: "--a"}}, line: "unused"}},
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
