package cli

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// sweepInput is a runtime.Input built from values, for driving the
// pipeline without a command tree.
type sweepInput struct {
	s    map[string]string
	sl   map[string][]string
	set  map[string]bool
	args []string
}

func (i sweepInput) String(k string) string    { return i.s[k] }
func (i sweepInput) Bool(string) bool          { return false }
func (i sweepInput) Strings(k string) []string { return i.sl[k] }
func (i sweepInput) IsSet(k string) bool       { return i.set[k] }
func (i sweepInput) Args() []string            { return i.args }

// emptyValueMessage is the refusal of a typed blank value of f.
func emptyValueMessage(f runtime.ArgSpec) string {
	switch {
	case f.Name == runtime.FlagProject:
		return "--project is empty; pass a project id or display name, or omit --project"
	case f.Required:
		return fmt.Sprintf("--%s is empty; pass a value", f.Name)
	default:
		return fmt.Sprintf("--%s is empty; pass a value or omit --%s", f.Name, f.Name)
	}
}

// TestEmptyValueIsRefusedForEveryFlag asserts, for every registered
// command and every value-taking flag it lists, that a typed empty or
// blank value is VALIDATION naming the flag, before the command runs. The
// registry's commands carry no dependencies, so a command that ran would
// fail the row rather than send a request. The command tree is not built per
// row: how the tree turns each typed form into a set, empty value is
// asserted per flag type by TestEmptyValueFormsThroughTheCommandTree.
func TestEmptyValueIsRefusedForEveryFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	pairs := 0
	for _, cmd := range wr.reg.Commands() {
		d := cmd.Describe()
		base := sweepInput{s: map[string]string{}, sl: map[string][]string{}, set: map[string]bool{}}
		for _, a := range d.Args {
			if a.Required {
				v := "x"
				if len(a.Enum) > 0 {
					v = a.Enum[0]
				}
				base.args = append(base.args, v)
			}
		}
		for _, f := range d.Flags {
			if !f.Required || f.Type == "bool" {
				continue
			}
			v := "x"
			if len(f.Enum) > 0 {
				v = f.Enum[0]
			}
			base.s[f.Name], base.sl[f.Name], base.set[f.Name] = v, []string{v}, true
		}
		for _, f := range d.Flags {
			if f.Type == "bool" {
				continue
			}
			values := [][]string{{""}, {"  "}}
			if f.Type == "stringSlice" {
				values = [][]string{{}, {""}, {"a", " "}}
			}
			for _, v := range values {
				pairs++
				in := sweepInput{s: map[string]string{}, sl: map[string][]string{}, set: map[string]bool{}, args: base.args}
				for k, x := range base.s {
					in.s[k], in.sl[k], in.set[k] = x, base.sl[k], true
				}
				in.s[f.Name], in.sl[f.Name], in.set[f.Name] = strings.Join(v, ","), v, true
				var out, errOut bytes.Buffer
				var ce error
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("%s --%s %q: the command ran: %v", d.Name, f.Name, v, r)
						}
					}()
					_, cerr := runtime.Run(t.Context(), cmd, in, runtime.IO{Out: &out, Err: &errOut}, safety.Binding{})
					if cerr != nil {
						ce = cerr
						assert.Equal(t, "VALIDATION", cerr.Code, "%s --%s %q", d.Name, f.Name, v)
						assert.Equal(t, emptyValueMessage(f), cerr.Message, "%s --%s %q", d.Name, f.Name, v)
					}
				}()
				assert.NotNil(t, ce, "%s --%s %q: refused", d.Name, f.Name, v)
			}
		}
	}
	t.Logf("%d (command, flag, value) rows", pairs)
	assert.Greater(t, pairs, 1000, "the sweep covers the registry")
}

// TestEmptyValueFormsThroughTheCommandTree asserts each way to type an
// empty value - --flag=, --flag "" and --flag ' ' - reaches the pipeline
// as a typed, empty value for every flag type the descriptors use (a
// string, an enum, a list) and for the global -o and --color, and is
// refused naming the flag with no request sent.
func TestEmptyValueFormsThroughTheCommandTree(t *testing.T) {
	h := newContractHarness(t)
	for _, tc := range []struct {
		cmd  []string
		flag string
		want string
	}{
		{[]string{"revoke", "u-1", "--cluster", "c-abc", "--dry-run"}, "role", "--role is empty; pass a value or omit --role"},
		{[]string{"usage", "--cluster", "c-abc"}, "by", "--by is empty; pass a value or omit --by"},
		{[]string{"images", "--cluster", "c-abc"}, "match", "--match is empty; pass a value or omit --match"},
		{[]string{"get", "pod", "--cluster", "c-abc"}, "namespace", "--namespace is empty; pass a value or omit --namespace"},
		{[]string{"get", "pod"}, "cluster", "--cluster is empty; pass a value"},
		{[]string{"get", "pod", "--cluster", "c-abc"}, "project", "--project is empty; pass a project id or display name, or omit --project"},
		{[]string{"get", "pod", "--cluster", "c-abc"}, "output", "--output is empty; pass a value or omit --output"},
		{[]string{"get", "pod", "--cluster", "c-abc"}, "color", "--color is empty; pass a value or omit --color"},
	} {
		for _, form := range [][]string{{"--" + tc.flag + "="}, {"--" + tc.flag, ""}, {"--" + tc.flag, " "}} {
			args := append(append(slices.Clone(tc.cmd), form...), "-o", "json")
			if tc.flag == "output" {
				args = append(slices.Clone(tc.cmd), form...)
			}
			r := h.run(t, args...)
			assert.Empty(t, r.requests, "%v", args)
			if tc.flag == "output" {
				assert.Contains(t, r.stderr, tc.want, "%v", args)
				continue
			}
			require.NotNil(t, r.err, "%v: %s %s", args, r.stdout, r.stderr)
			assert.Equal(t, "VALIDATION", r.err.Code, "%v", args)
			assert.Equal(t, tc.want, r.err.Message, "%v", args)
		}
	}
}
