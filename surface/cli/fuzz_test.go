package cli

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fuzzLeaf and fuzzGlobals are the fixed, trusted descriptor set ParseArgTail
// is fuzzed against. Only wire-derived values (token bytes, currentWord,
// marker mode) are fuzzed; descriptors are static configuration and stay
// fixed here, covering value-taking, boolean, aliased, and repeatable
// (stringSlice) flags.
var (
	fuzzLeaf = runtime.Descriptor{
		Name: "get",
		Flags: []runtime.ArgSpec{
			{Name: "cluster", Type: "string", Aliases: []string{"c"}},
			{Name: "verbose", Type: "bool"},
			{Name: "label", Type: "stringSlice", Aliases: []string{"l"}},
			{Name: "output", Type: "enum", Enum: []string{"json", "yaml", "table"}, Aliases: []string{"o"}},
		},
	}
	fuzzGlobals = []runtime.ArgSpec{
		{Name: "dry-run", Type: "bool"},
		{Name: "project", Type: "string"},
	}
)

// FuzzParseArgTail fuzzes TAB-time raw shell input. Args are encoded as one
// NUL-joined blob plus an explicit presence bool (native fuzzing has no
// []string primitive; a real argv token never contains a NUL byte). The
// presence bool exists because NUL-splitting alone cannot distinguish zero
// args from one empty arg — strings.Split("", "\x00") is []string{""} —
// and the two classify differently (the positional ordinal shifts). Never
// call t.Parallel() here — it panics inside f.Fuzz.
func FuzzParseArgTail(f *testing.F) {
	type seed struct {
		argsBlob    string
		hasArgs     bool
		currentWord string
		hasMarker   bool
	}
	for _, s := range []seed{
		{"", false, "", true},
		{"", true, "", true},
		{"prod", true, "", true},
		{"--cluster\x00prod\x00foo", true, "", true},
		{"--verbose", true, "", true},
		{"", false, "--by=n", true},
		{"", false, "--cl", true},
		{"--cluster", true, "c-abc", true},
		{"--cluster\x00c-abc", true, "", true},
		{"-l\x00a\x00-l\x00b", true, "", true},
		{"", false, "", false},
		{"--cluster\x00prod", true, "", false},
		{"--cluster=pr", true, "", false},
		{"--cluster", true, "", false},
	} {
		f.Add(s.argsBlob, s.hasArgs, s.currentWord, s.hasMarker)
	}

	f.Fuzz(func(t *testing.T, argsBlob string, hasArgs bool, currentWord string, hasMarker bool) {
		var args []string
		if hasArgs {
			args = strings.Split(argsBlob, "\x00")
		}

		site := ParseArgTail(args, currentWord, hasMarker, fuzzLeaf, fuzzGlobals) // (a): no panic

		// (b) Kind is one of the three declared constants.
		assert.Contains(t, []CompletionSiteKind{SiteNameLike, SiteFlagValue, SitePositional}, site.Kind)

		// (d) Index is a non-negative positional ordinal (not an args index).
		assert.GreaterOrEqual(t, site.Index, 0)

		if hasMarker {
			// (c) v2: Partial provenance is currentWord-derived.
			switch site.Kind {
			case SiteFlagValue:
				if site.EqualsForm {
					_, want, _ := strings.Cut(currentWord, "=")
					assert.Equal(t, want, site.Partial)
				} else {
					assert.Equal(t, currentWord, site.Partial)
				}
			case SitePositional:
				assert.Equal(t, currentWord, site.Partial)
			case SiteNameLike:
				assert.Empty(t, site.Partial)
			}
			return
		}

		// (c) v1: Partial is empty or derived from the FINAL argument — the
		// last-token-as-partial heuristic's only two non-empty sources are
		// the final arg's after-= suffix (equals form) and the final arg
		// verbatim (the value slot of a preceding value-taking flag).
		if site.Partial != "" {
			require.NotEmpty(t, args)
			last := args[len(args)-1]
			_, afterEq, _ := strings.Cut(last, "=")
			assert.True(t, site.Partial == last || site.Partial == afterEq,
				"v1 Partial %q not derived from final arg %q", site.Partial, last)
		}

		// And the whole v1 classification must be currentWord-invariant —
		// the fallback branch never reads currentWord.
		other := ParseArgTail(args, currentWord+"\x01differs", false, fuzzLeaf, fuzzGlobals)
		assert.Equal(t, site, other)
	})
}
