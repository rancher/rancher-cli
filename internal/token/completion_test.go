package token

import (
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	tokencomplete "github.com/rancher/rancher-cli/internal/token/complete"
)

// TestTheCompletionTypesEqualTheSupportedOnes asserts the provider types the
// completion source offers auth configs of are exactly the ones the sign-in
// can use, so a suggestion never names an auth config the command drops.
func TestTheCompletionTypesEqualTheSupportedOnes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, slices.Sorted(maps.Keys(supportedAuthProviders)), slices.Sorted(slices.Values(tokencomplete.SupportedTypes())))
}

// TestCompletionTargetsFollowEachCommandsOwnChoice asserts the target of
// each command's completion over a config: get-token's server is --server,
// else the current entry, and an entry --name identifies at another URL
// gives none; the legacy command's is --server alone and none with
// --skip-verify.
func TestCompletionTargetsFollowEachCommandsOwnChoice(t *testing.T) {
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCACert, "")
	cfg := &config.Config{CurrentServer: "cur", Servers: map[string]config.Server{
		"cur":   {URL: "https://cur.example.com"},
		"other": {URL: "https://other.example.com"},
	}}
	req := func(tail ...string) completion.Request { return completion.Request{Flag: "auth-provider", Tail: tail} }

	server, _, ok := tokenCmd{}.CompletionTarget(cfg, req("--auth-provider"))
	require.True(t, ok)
	assert.Equal(t, "https://cur.example.com", server)

	server, _, ok = tokenCmd{}.CompletionTarget(cfg, req("--server", "named.example.com", "--auth-provider"))
	require.True(t, ok)
	assert.Equal(t, "https://named.example.com", server, "a schemeless --server is https, as the command takes it")

	_, _, ok = tokenCmd{}.CompletionTarget(cfg, req("--server", "https://cur.example.com", "--name", "other", "--auth-provider"))
	assert.False(t, ok, "--name identifies an entry at another URL")

	_, _, ok = legacyTokenCmd{}.CompletionTarget(cfg, req("--auth-provider"))
	assert.False(t, ok, "the legacy command never uses the current entry")
	server, _, ok = legacyTokenCmd{}.CompletionTarget(cfg, req("--server", "https://named.example.com", "--auth-provider"))
	require.True(t, ok)
	assert.Equal(t, "https://named.example.com", server)
	_, _, ok = legacyTokenCmd{}.CompletionTarget(cfg, req("--skip-verify", "--server", "https://named.example.com", "--auth-provider"))
	assert.False(t, ok)
}

// TestTailBoolReadsABoolFlagAnywhereOnTheLine asserts a bool flag is seen
// where TailFlag, which takes the next word as a value, cannot: before
// another flag and as the last word, in its --flag=value form too, and a
// word that is another flag's value is not it.
func TestTailBoolReadsABoolFlagAnywhereOnTheLine(t *testing.T) {
	t.Parallel()
	specs := legacyTokenCmd{}.Describe().Flags
	for name, tc := range map[string]struct {
		tail []string
		want bool
	}{
		"absent":              {[]string{"--server", "x"}, false},
		"before a flag":       {[]string{"--skip-verify", "--server", "x"}, true},
		"last":                {[]string{"--server", "x", "--skip-verify"}, true},
		"equals true":         {[]string{"--skip-verify=true"}, true},
		"equals false":        {[]string{"--skip-verify=false"}, false},
		"as another's value":  {[]string{"--user", "--skip-verify"}, false},
		"after a valued flag": {[]string{"--user", "alice", "--skip-verify"}, true},
		"one dash":            {[]string{"--server", "x", "-skip-verify"}, true},
		"after --":            {[]string{"--", "--skip-verify"}, false},
	} {
		assert.Equal(t, tc.want, tailBool(tc.tail, specs, "skip-verify"), name)
	}
}

// TestTailBoolReadsAliasesAndSkipsAStdinDash asserts a bool flag is read by
// its alias, by its own name only, and that a lone "-" (stdin) is a word,
// not a flag. Mutations: drop the alias match; compare the typed word with
// the asked name; read "-" as a flag.
func TestTailBoolReadsAliasesAndSkipsAStdinDash(t *testing.T) {
	t.Parallel()
	specs := []runtime.ArgSpec{
		{Name: "insecure", Type: "bool", Aliases: []string{"k"}},
		// The empty alias makes a lone "-" read as a flag match this valued
		// flag and swallow the next word, so reading "-" as a flag fails the
		// last row.
		{Name: "file", Type: "string", Aliases: []string{""}},
	}
	assert.True(t, tailBool([]string{"-k"}, specs, "insecure"), "the alias")
	assert.True(t, tailBool([]string{"--k"}, specs, "insecure"), "the alias with two dashes")
	assert.False(t, tailBool([]string{"-k"}, specs, "k"), "an alias is not the flag's name")
	assert.True(t, tailBool([]string{"--file", "-", "--insecure"}, specs, "insecure"), "a stdin dash is the value")
	assert.True(t, tailBool([]string{"-", "--insecure"}, specs, "insecure"), "a lone dash is a word")
}
