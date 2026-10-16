package cli

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/runtime"
)

// kubectlAliases is the one short form each of these flags has on every
// command that declares it.
var kubectlAliases = map[string]string{
	"namespace":      "n",
	"all-namespaces": "A",
	"selector":       "l",
	"filename":       "f",
	"patch":          "p",
}

// aliasProblems lists how the flags of d break the alias table:
//   - a flag named in kubectlAliases has exactly the table's alias;
//   - every alias of a flag is a value of the table, on the flag the table
//     names for it.
//
// It returns the two lists apart so each rule reports on its own.
func aliasProblems(d runtime.Descriptor) (missing, stray []string) {
	for _, f := range d.Flags {
		if want, ok := kubectlAliases[f.Name]; ok && !slices.Equal(f.Aliases, []string{want}) {
			missing = append(missing, fmt.Sprintf("--%s has aliases %q, want [%q]", f.Name, f.Aliases, want))
		}
		for _, a := range f.Aliases {
			if long, ok := aliasOwner(a); !ok || long != f.Name {
				stray = append(stray, "--"+f.Name+" has alias \""+a+"\" that the table does not give it")
			}
		}
	}
	return missing, stray
}

// aliasOwner returns the flag the table gives alias to.
func aliasOwner(alias string) (string, bool) {
	for long, a := range kubectlAliases {
		if a == alias {
			return long, true
		}
	}
	return "", false
}

// TestFlagAliasesMatchTheTable asserts both alias rules for every
// registered command, one subtest per command and rule, so one run lists
// every failure. There is no allowlist: an exception is a decision for the
// maintainers and adds an entry then.
func TestFlagAliasesMatchTheTable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)

	for _, cmd := range wr.reg.Commands() {
		d := cmd.Describe()
		missing, stray := aliasProblems(d)

		t.Run(d.Name+"/tableFlagsHaveTheirAlias", func(t *testing.T) {
			for _, p := range missing {
				assert.Fail(t, d.Name+": "+p)
			}
		})
		t.Run(d.Name+"/aliasesBelongToTheTable", func(t *testing.T) {
			for _, p := range stray {
				assert.Fail(t, d.Name+": "+p)
			}
		})
	}
}

// TestFlagAliasRulesCatchADeviation asserts that the alias rules accept
// flags that follow the table and report a missing alias and an alias the
// table does not give.
func TestFlagAliasRulesCatchADeviation(t *testing.T) {
	ok := runtime.Descriptor{Flags: []runtime.ArgSpec{
		{Name: "namespace", Aliases: []string{"n"}},
		{Name: "selector", Aliases: []string{"l"}},
		{Name: "other"},
	}}
	missing, stray := aliasProblems(ok)
	assert.Empty(t, missing)
	assert.Empty(t, stray)

	bad := runtime.Descriptor{Flags: []runtime.ArgSpec{
		{Name: "namespace"},
		{Name: "selector", Aliases: []string{"l", "s"}},
		{Name: "filename", Aliases: []string{"n"}},
		{Name: "other", Aliases: []string{"x"}},
	}}
	missing, stray = aliasProblems(bad)
	assert.Len(t, missing, 3)
	assert.Len(t, stray, 3)
}
