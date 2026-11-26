package cli

import (
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type liveEnumClaim struct {
	command string
	flag    string
	reason  string
}

// liveEnumClaims is the one list of enum flags whose static enum a command's
// live completer replaces: the command, the flag and the reason. Every other
// enum flag, of every command and of the global flags, answers its static
// enum.
var liveEnumClaims = []liveEnumClaim{
	{"cluster restore-etcd-snapshot", "restore-mode", "the modes offered depend on the snapshot: Rancher's restore-mode annotation and the snapshot's metadata"},
}

func isLiveEnumClaim(command, flag string) bool {
	return slices.ContainsFunc(liveEnumClaims, func(c liveEnumClaim) bool {
		return c.command == command && c.flag == flag
	})
}

// TestEveryEnumFlagAnswersItsStaticEnumUnlessClaimed asserts the completion of
// the value of every enum flag of every command, and of every global flag on
// every command, is its static enum (for --output the formats the command's
// output policy accepts), except for the flags in liveEnumClaims, which offer
// nothing while the network is off, never the static list.
func TestEveryEnumFlagAnswersItsStaticEnumUnlessClaimed(t *testing.T) {
	setStoredConfigForTest(t, &config.Config{}, t.TempDir())
	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)
	claimed := 0
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		var lc completion.Completer
		if l, ok := c.(completion.Completer); ok {
			lc = l
		}
		seen := map[string]bool{}
		for _, f := range slices.Concat(d.Flags, globalArgSpecs) {
			if len(f.Enum) == 0 || seen[f.Name] {
				continue
			}
			seen[f.Name] = true
			site := CompletionSite{Kind: SiteFlagValue, FlagName: f.Name, FlagSpelling: "--" + f.Name}
			enum := f.Enum
			if f.Name == "output" {
				enum = allowedOutputEnum(enum, d.OutputPolicy)
			}
			static := resolveStaticEnum(enum, site)
			if len(static) == 0 {
				continue
			}
			got := dispatchSite(t.Context(), site, d, nil, lc, nil)
			if isLiveEnumClaim(d.Name, f.Name) {
				claimed++
				assert.Empty(t, got, "%s --%s is claimed: with the network off it offers nothing", d.Name, f.Name)
				continue
			}
			assert.Equal(t, static, got, "%s --%s answers its static enum", d.Name, f.Name)
		}
	}
	assert.Equal(t, len(liveEnumClaims), claimed, "every claimed flag was found as an enum flag of its command")
}

// TestTheCommandsThatClaimAnEnumFlagAreTheClaimedList asserts the commands
// that implement liveEnumFlags and the flags they list are exactly
// liveEnumClaims, and every listed flag is an enum flag of its command.
func TestTheCommandsThatClaimAnEnumFlagAreTheClaimedList(t *testing.T) {
	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)
	var got []string
	for _, c := range wr.reg.Commands() {
		l, ok := c.(liveEnumFlags)
		if !ok {
			continue
		}
		d := c.Describe()
		for _, flag := range l.LiveEnumFlags() {
			got = append(got, d.Name+" --"+flag)
			assert.True(t, slices.ContainsFunc(d.Flags, func(f runtime.ArgSpec) bool { return f.Name == flag && len(f.Enum) > 0 }),
				"%s lists --%s, which is not one of its enum flags", d.Name, flag)
		}
	}
	var want []string
	for _, c := range liveEnumClaims {
		assert.NotEmpty(t, c.reason, "%s --%s has a reason", c.command, c.flag)
		want = append(want, c.command+" --"+c.flag)
	}
	slices.Sort(got)
	slices.Sort(want)
	assert.Equal(t, want, got)
}
