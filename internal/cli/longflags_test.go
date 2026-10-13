package cli

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/runtime"
)

// TestLongFlagSpellingsEveryAliasOfEveryCommand asserts, for every flag of
// every registered command and every global flag that has an alias, that
// the alias with a separate value, joined by "=", or alone (a bool flag)
// becomes the flag's long spelling with the same value.
func TestLongFlagSpellingsEveryAliasOfEveryCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)

	for _, cmd := range wr.reg.Commands() {
		d := cmd.Describe()
		flags := slices.Concat(d.Flags, globalArgSpecs)
		t.Run(d.Name, func(t *testing.T) {
			for _, f := range flags {
				for _, a := range f.Aliases {
					if f.Type == "bool" {
						assert.Equal(t, []string{"--" + f.Name}, longFlagSpellings([]string{"-" + a}, flags), "-%s", a)
						continue
					}
					assert.Equal(t, []string{"--" + f.Name, "v"}, longFlagSpellings([]string{"-" + a, "v"}, flags), "-%s v", a)
					assert.Equal(t, []string{"--" + f.Name + "=v"}, longFlagSpellings([]string{"-" + a + "=v"}, flags), "-%s=v", a)
				}
			}
		})
	}
}

func TestLongFlagSpellingsFixedCases(t *testing.T) {
	flags := []runtime.ArgSpec{
		{Name: "namespace", Type: "string", Aliases: []string{"n"}},
		{Name: "selector", Type: "string", Aliases: []string{"l"}},
		{Name: "all-namespaces", Type: "bool", Aliases: []string{"A"}},
		{Name: "output", Type: "enum", Aliases: []string{"o"}},
		{Name: "pool", Type: "string"},
	}
	tests := []struct {
		name string
		tail []string
		want []string
	}{
		{"two dashes and a name that is an alias", []string{"--n", "x"}, []string{"--namespace", "x"}},
		{"one dash and the long name", []string{"-namespace", "x"}, []string{"--namespace", "x"}},
		{"a long form stays", []string{"--namespace", "x"}, []string{"--namespace", "x"}},
		{"a long form joined stays", []string{"--namespace=x"}, []string{"--namespace=x"}},
		{"a joined value that looks like a flag", []string{"-l=-n"}, []string{"--selector=-n"}},
		{"a joined value keeps its equals signs", []string{"-l=a=b"}, []string{"--selector=a=b"}},
		{"an alias typed as the value of another flag", []string{"--selector", "-n"}, []string{"--selector", "-n"}},
		{"an alias typed as the value of an aliased flag", []string{"-l", "-n"}, []string{"--selector", "-n"}},
		{"words after the end of the flags", []string{"pod", "--", "-n", "x"}, []string{"pod", "--", "-n", "x"}},
		{"words after a bare dash", []string{"-", "-n", "x"}, []string{"-", "-n", "x"}},
		{"a flag no spec names", []string{"-z", "x"}, []string{"-z", "x"}},
		{"a bool flag then a value flag", []string{"-A", "-n", "x"}, []string{"--all-namespaces", "--namespace", "x"}},
		{"a flag with its value still to type", []string{"get", "pod", "-n"}, []string{"get", "pod", "--namespace"}},
		{"padding on a positional and a value stays", []string{" x ", "pod ", "-n", " v "}, []string{" x ", "pod ", "--namespace", " v "}},
		{"a padded flag word is trimmed", []string{" -n=x "}, []string{"--namespace=x"}},
		{"a padded long form stays as typed", []string{" --namespace ", "x"}, []string{" --namespace ", "x"}},
		{"only the flag word changes", []string{"pod", "-n", "a", "web", "-o", "json"}, []string{"pod", "--namespace", "a", "web", "--output", "json"}},
		{"a flag typed twice", []string{"-n", "a", "-n", "b"}, []string{"--namespace", "a", "--namespace", "b"}},
		{"a bool alias with a joined value", []string{"-A=true"}, []string{"--all-namespaces=true"}},
		{"an empty tail", nil, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := slices.Clone(tt.tail)
			assert.Equal(t, tt.want, longFlagSpellings(tt.tail, flags))
			assert.Equal(t, in, tt.tail, "the tail passed in is not modified")
		})
	}
}
