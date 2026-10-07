package cli

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
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
// args from one empty arg - strings.Split("", "\x00") is []string{""} -
// and the two classify differently (the positional ordinal shifts). Never
// call t.Parallel() here - it panics inside f.Fuzz.
func FuzzParseArgTail(f *testing.F) {
	type seed struct {
		argsBlob    string
		hasArgs     bool
		currentWord string
	}
	for _, s := range []seed{
		{"", false, ""},
		{"", true, ""},
		{"prod", true, ""},
		{"--cluster\x00prod\x00foo", true, ""},
		{"--verbose", true, ""},
		{"", false, "--by=n"},
		{"", false, "--cl"},
		{"--cluster", true, "c-abc"},
		{"--cluster\x00c-abc", true, ""},
		{"-l\x00a\x00-l\x00b", true, ""},
		{"--cluster\x00prod", true, ""},
		{"--cluster=pr", true, ""},
		{"--cluster", true, ""},
	} {
		f.Add(s.argsBlob, s.hasArgs, s.currentWord)
	}

	f.Fuzz(func(t *testing.T, argsBlob string, hasArgs bool, currentWord string) {
		var args []string
		if hasArgs {
			args = strings.Split(argsBlob, "\x00")
		}

		site := ParseArgTail(args, currentWord, fuzzLeaf, fuzzGlobals) // (a): no panic

		// (b) Kind is one of the three declared constants.
		assert.Contains(t, []CompletionSiteKind{SiteNameLike, SiteFlagValue, SitePositional}, site.Kind)

		// (d) Index is a non-negative positional ordinal (not an args index).
		assert.GreaterOrEqual(t, site.Index, 0)

		// (c) Partial provenance is currentWord-derived.
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
	})
}
