package cli

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
)

// testLeaf is a minimal descriptor used across ParseArgTail tests.
var testLeaf = runtime.Descriptor{
	Name: "get",
	Flags: []runtime.ArgSpec{
		{Name: "cluster", Type: "string"},
		{Name: "by", Type: "string"},
		{Name: "verbose", Type: "bool"},
	},
}

func TestParseArgTail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		args        []string
		currentWord string
		hasMarker   bool
		want        CompletionSite
	}{
		// v2 protocol cases (hasMarker=true) - currentWord classified first.
		{
			name:        "v2: empty currentWord, no args → positional 0",
			args:        []string{},
			currentWord: "",
			hasMarker:   true,
			want:        CompletionSite{Kind: SitePositional, Index: 0},
		},
		{
			name:        "v2: empty currentWord, one positional in args → positional 1",
			args:        []string{"prod"},
			currentWord: "",
			hasMarker:   true,
			want:        CompletionSite{Kind: SitePositional, Index: 1, Args: []string{"prod"}},
		},
		{
			name:        "v2: string flag value consumed (non-final) → positional 1",
			args:        []string{"--cluster", "prod", "foo"},
			currentWord: "",
			hasMarker:   true,
			want:        CompletionSite{Kind: SitePositional, Index: 1, Args: []string{"foo"}},
		},
		{
			name:        "v2: bool flag → positional 0",
			args:        []string{"--verbose"},
			currentWord: "",
			hasMarker:   true,
			want:        CompletionSite{Kind: SitePositional, Index: 0},
		},
		// Equals-form currentWord classifies as an equals-form SiteFlagValue.
		{
			name:        "v2: equals-form currentWord → flagValue equals (currentWord classified first)",
			args:        []string{},
			currentWord: "--by=n",
			hasMarker:   true,
			want:        CompletionSite{Kind: SiteFlagValue, FlagName: "by", FlagSpelling: "--by", EqualsForm: true, Partial: "n"},
		},
		{
			// A partial flag in currentWord classifies as SiteNameLike.
			name:        "v2: partial flag currentWord → SiteNameLike (urfave default completer)",
			args:        []string{},
			currentWord: "--cl",
			hasMarker:   true,
			want:        CompletionSite{Kind: SiteNameLike},
		},
		{
			// A non-flag currentWord after a value-taking flag classifies as
			// → space-form SiteFlagValue with Partial=currentWord.
			name:        "v2: non-flag currentWord, flag-last in args → SiteFlagValue space-form",
			args:        []string{"--cluster"},
			currentWord: "c-abc",
			hasMarker:   true,
			want:        CompletionSite{Kind: SiteFlagValue, FlagName: "cluster", FlagSpelling: "--cluster", EqualsForm: false, Partial: "c-abc"},
		},
		{
			// A non-flag currentWord with no trailing flag classifies as
			// SitePositional.
			name:        "v2: non-flag currentWord, no trailing flag → SitePositional",
			args:        []string{"--cluster", "c-abc"},
			currentWord: "mypod",
			hasMarker:   true,
			want:        CompletionSite{Kind: SitePositional, Index: 0, Partial: "mypod"},
		},
		{
			// v2 both-sides test A: flag + completed-value word + currentWord="" → positional.
			// "--cluster c-test " TAB: rawArgv=["--cluster","c-test"], currentWord=""
			name:        "v2: flag+value in args, currentWord empty → next positional (disambiguates space-after-value)",
			args:        []string{"--cluster", "c-test"},
			currentWord: "",
			hasMarker:   true,
			want:        CompletionSite{Kind: SitePositional, Index: 0},
		},
		{
			// v2 both-sides test B: flag only in args + non-empty currentWord → flagValue partial.
			// "--cluster c-" TAB: rawArgv=["--cluster"], currentWord="c-"
			name:        "v2: flag-only in args, currentWord non-empty → flagValue with Partial (disambiguates partial-value)",
			args:        []string{"--cluster"},
			currentWord: "c-",
			hasMarker:   true,
			want:        CompletionSite{Kind: SiteFlagValue, FlagName: "cluster", FlagSpelling: "--cluster", EqualsForm: false, Partial: "c-"},
		},
		// v1 protocol cases (hasMarker=false) - last-word-as-partial heuristic.
		{
			name:      "v1: empty args → positional 0",
			args:      []string{},
			hasMarker: false,
			want:      CompletionSite{Kind: SitePositional, Index: 0},
		},
		{
			name:      "v1: flag last, takes value → SiteFlagValue",
			args:      []string{"--cluster"},
			hasMarker: false,
			want:      CompletionSite{Kind: SiteFlagValue, FlagName: "cluster", FlagSpelling: "--cluster"},
		},
		{
			name:      "v1: flag + partial value → SiteFlagValue space-form with Partial",
			args:      []string{"--cluster", "c-"},
			hasMarker: false,
			want:      CompletionSite{Kind: SiteFlagValue, FlagName: "cluster", FlagSpelling: "--cluster", EqualsForm: false, Partial: "c-"},
		},
		{
			name:      "v1: equals-form flag last → SiteFlagValue equals-form",
			args:      []string{"--cluster=prod"},
			hasMarker: false,
			want:      CompletionSite{Kind: SiteFlagValue, FlagName: "cluster", FlagSpelling: "--cluster", EqualsForm: true, Partial: "prod"},
		},
		{
			name:      "v1: unknown flag last → SiteNameLike",
			args:      []string{"--unknown-flag"},
			hasMarker: false,
			want:      CompletionSite{Kind: SiteNameLike},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ParseArgTail(tc.args, tc.currentWord, tc.hasMarker, testLeaf, globalArgSpecs)
			assert.Equal(t, tc.want, got)
		})
	}
}

// cbCases tests the SiteFlagValue path for various flag types and spellings.
func TestParseArgTailFlagValueCases(t *testing.T) {
	t.Parallel()

	type cbCase struct {
		name     string
		flagArg  string
		leaf     runtime.Descriptor
		wantKind CompletionSiteKind
		wantName string
	}

	cases := []cbCase{
		{
			name:     "string flag --cluster → SiteFlagValue",
			flagArg:  "--cluster",
			leaf:     testLeaf,
			wantKind: SiteFlagValue,
			wantName: "cluster",
		},
		{
			name:     "string flag --by → SiteFlagValue",
			flagArg:  "--by",
			leaf:     testLeaf,
			wantKind: SiteFlagValue,
			wantName: "by",
		},
		{
			name:     "bool flag --verbose → SitePositional (bool takes no value)",
			flagArg:  "--verbose",
			leaf:     testLeaf,
			wantKind: SitePositional,
			wantName: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ParseArgTail([]string{tc.flagArg}, "", true, tc.leaf, globalArgSpecs)
			assert.Equal(t, tc.wantKind, got.Kind)
			if tc.wantKind == SiteFlagValue {
				assert.Equal(t, tc.wantName, got.FlagName)
			}
		})
	}
}

func TestCaptureArgvV2ProtoMarkerExtractsCurField(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord/rawProtoV2 package globals.
	orig := rawArgv
	origW, origP, origV1 := rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = origP; rawV1Completion = origV1 })
	// --rancher-completion-cur=c- carries the partial; all other words go to rawArgv.
	CaptureArgv([]string{"--rancher-completion-proto=2", "get", "pod", "--cluster", "--rancher-completion-cur=c-", "--generate-shell-completion"})
	assert.Equal(t, []string{"get", "pod", "--cluster"}, rawArgv)
	assert.Equal(t, "c-", rawCurrentWord)
	assert.True(t, rawProtoV2)
}

func TestCaptureArgvV2EmptyCurrentWord(t *testing.T) {
	// NOT parallel: mutates package globals.
	orig := rawArgv
	origW, origP, origV1 := rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = origP; rawV1Completion = origV1 })
	// --rancher-completion-cur= (empty value) → cursor after space, no partial typed.
	CaptureArgv([]string{"--rancher-completion-proto=2", "get", "pod", "--cluster", "c-test", "--rancher-completion-cur=", "--generate-shell-completion"})
	assert.Equal(t, []string{"get", "pod", "--cluster", "c-test"}, rawArgv)
	assert.Equal(t, "", rawCurrentWord)
	assert.True(t, rawProtoV2)
}

func TestCaptureArgvV2EqualsFormCurrentWord(t *testing.T) {
	// NOT parallel: mutates package globals.
	// --rancher-completion-cur=--by=n carries an equals-form current word.
	orig := rawArgv
	origW, origP, origV1 := rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = origP; rawV1Completion = origV1 })
	CaptureArgv([]string{"--rancher-completion-proto=2", "usage", "--rancher-completion-cur=--by=n", "--generate-shell-completion"})
	assert.Equal(t, []string{"usage"}, rawArgv)
	assert.Equal(t, "--by=n", rawCurrentWord)
	assert.True(t, rawProtoV2)
}

func TestCaptureArgvV1NoSentinelNoCurrentWord(t *testing.T) {
	// NOT parallel: mutates package globals.
	orig := rawArgv
	origW, origP, origV1 := rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = origP; rawV1Completion = origV1 })
	CaptureArgv([]string{"get", "pod", "--cluster", "prod", "--generate-shell-completion"})
	assert.Equal(t, []string{"get", "pod", "--cluster", "prod"}, rawArgv)
	assert.Equal(t, "", rawCurrentWord)
	assert.False(t, rawProtoV2)
}

func TestSanitizedArgsStripsBothProtocolFields(t *testing.T) {
	t.Parallel()
	// SanitizedArgs must strip both the sentinel and the cur field (never
	// forward currentWord to urfave, so unknown-command dispatch cannot fire on
	// typed text).
	in := []string{"rancher", "--rancher-completion-proto=2", "get", "pod", "--rancher-completion-cur=c-", "--generate-shell-completion"}
	got := SanitizedArgs(in)
	assert.Equal(t, []string{"rancher", "get", "pod", "--generate-shell-completion"}, got)
}

func TestSanitizedArgsStripsEmptyCurField(t *testing.T) {
	t.Parallel()
	in := []string{"rancher", "--rancher-completion-proto=2", "usage", "--rancher-completion-cur=", "--generate-shell-completion"}
	got := SanitizedArgs(in)
	assert.Equal(t, []string{"rancher", "usage", "--generate-shell-completion"}, got)
}

// TestParseArgTailRepeatableFlagArity asserts the completion arity of a
// repeatable flag: a stringSlice flag takes a value on EVERY occurrence - a
// second --set after a completed first one is a flag-value site, and a
// completed pair counts no positional.
func TestParseArgTailRepeatableFlagArity(t *testing.T) {
	t.Parallel()
	leaf := runtime.Descriptor{
		Name: "create machine-config",
		Flags: []runtime.ArgSpec{
			{Name: "set", Type: "stringSlice"},
		},
	}

	t.Run("second occurrence is a value site", func(t *testing.T) {
		t.Parallel()
		site := ParseArgTail([]string{"digitaloceanconfig", "cfg", "--set", "region=nyc3", "--set"},
			"", true, leaf, globalArgSpecs)
		assert.Equal(t, SiteFlagValue, site.Kind)
		assert.Equal(t, "set", site.FlagName)
	})

	t.Run("completed occurrences do not shift positional index", func(t *testing.T) {
		t.Parallel()
		site := ParseArgTail([]string{"digitaloceanconfig", "--set", "region=nyc3"},
			"", true, leaf, globalArgSpecs)
		assert.Equal(t, SitePositional, site.Kind)
		assert.Equal(t, 1, site.Index)
	})
}

// TestScanOutputFormat is table-driven over every accepted -o/--output
// spelling, the "--" cutoff, a trailing valueless flag, repeated
// occurrences (last wins), and root- vs. leaf-position placement.
func TestScanOutputFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"no flag at all", []string{"get", "pod"}, ""},
		{"space form, root position", []string{"-o", "json", "get", "pod"}, "json"},
		{"space form, leaf position", []string{"get", "pod", "-o", "json"}, "json"},
		{"equals form short", []string{"get", "pod", "-o=yaml"}, "yaml"},
		{"long space form", []string{"get", "pod", "--output", "yaml"}, "yaml"},
		{"long equals form", []string{"get", "pod", "--output=yaml"}, "yaml"},
		{"stops at the first --", []string{"get", "pod", "--", "-o", "json"}, ""},
		{"trailing valueless flag selects no format", []string{"get", "pod", "-o"}, ""},
		{"trailing valueless flag before --", []string{"get", "pod", "-o", "--", "pos"}, ""},
		{"repeated flag: last wins", []string{"-o", "json", "get", "pod", "-o", "yaml"}, "yaml"},
		{"repeated flag: trailing valueless wins", []string{"-o", "json", "get", "pod", "-o"}, ""},
		{"unrecognized value passes through untrusted", []string{"get", "pod", "-o", "csv"}, "csv"},
		{"non-flag words ignored", []string{"get", "pod", "positional-o"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, scanOutputFormat(tc.argv))
		})
	}
}
