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
		// v2 protocol cases (hasMarker=true) — currentWord classified first.
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
			want:        CompletionSite{Kind: SitePositional, Index: 1},
		},
		{
			name:        "v2: string flag value consumed (non-final) → positional 0",
			args:        []string{"--cluster", "prod", "foo"},
			currentWord: "",
			hasMarker:   true,
			want:        CompletionSite{Kind: SitePositional, Index: 1},
		},
		{
			name:        "v2: bool flag → positional 0",
			args:        []string{"--verbose"},
			currentWord: "",
			hasMarker:   true,
			want:        CompletionSite{Kind: SitePositional, Index: 0},
		},
		// Round-5 finding 1: equals-form currentWord → SiteFlagValue equals-form.
		{
			name:        "v2: equals-form currentWord → flagValue equals (currentWord classified first)",
			args:        []string{},
			currentWord: "--by=n",
			hasMarker:   true,
			want:        CompletionSite{Kind: SiteFlagValue, FlagName: "by", FlagSpelling: "--by", EqualsForm: true, Partial: "n"},
		},
		{
			// Round-5 finding 1: partial flag in currentWord → SiteNameLike.
			name:        "v2: partial flag currentWord → SiteNameLike (urfave default completer)",
			args:        []string{},
			currentWord: "--cl",
			hasMarker:   true,
			want:        CompletionSite{Kind: SiteNameLike},
		},
		{
			// Round-5 finding 1: non-flag currentWord, last args token is value-taking flag
			// → space-form SiteFlagValue with Partial=currentWord.
			name:        "v2: non-flag currentWord, flag-last in args → SiteFlagValue space-form",
			args:        []string{"--cluster"},
			currentWord: "c-abc",
			hasMarker:   true,
			want:        CompletionSite{Kind: SiteFlagValue, FlagName: "cluster", FlagSpelling: "--cluster", EqualsForm: false, Partial: "c-abc"},
		},
		{
			// Round-5 finding 1: non-flag currentWord, no flag at end → SitePositional.
			name:        "v2: non-flag currentWord, no trailing flag → SitePositional",
			args:        []string{"--cluster", "c-abc"},
			currentWord: "mypod",
			hasMarker:   true,
			want:        CompletionSite{Kind: SitePositional, Index: 0, Partial: "mypod"},
		},
		{
			// v2 both-sides test A: flag + completed-value token + currentWord="" → positional.
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
		// v1 protocol cases (hasMarker=false) — last-token-as-partial heuristic.
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
	origW := rawCurrentWord
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = false })
	// --rancher-completion-cur=c- carries the partial; all other tokens go to rawArgv.
	CaptureArgv([]string{"--rancher-completion-proto=2", "get", "pod", "--cluster", "--rancher-completion-cur=c-", "--generate-shell-completion"})
	assert.Equal(t, []string{"get", "pod", "--cluster"}, rawArgv)
	assert.Equal(t, "c-", rawCurrentWord)
	assert.True(t, rawProtoV2)
}

func TestCaptureArgvV2EmptyCurrentWord(t *testing.T) {
	// NOT parallel: mutates package globals.
	orig := rawArgv
	origW := rawCurrentWord
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = false })
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
	origW := rawCurrentWord
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = false })
	CaptureArgv([]string{"--rancher-completion-proto=2", "usage", "--rancher-completion-cur=--by=n", "--generate-shell-completion"})
	assert.Equal(t, []string{"usage"}, rawArgv)
	assert.Equal(t, "--by=n", rawCurrentWord)
	assert.True(t, rawProtoV2)
}

func TestCaptureArgvV1NoSentinelNoCurrentWord(t *testing.T) {
	// NOT parallel: mutates package globals.
	orig := rawArgv
	origW := rawCurrentWord
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = false })
	CaptureArgv([]string{"get", "pod", "--cluster", "prod", "--generate-shell-completion"})
	assert.Equal(t, []string{"get", "pod", "--cluster", "prod"}, rawArgv)
	assert.Equal(t, "", rawCurrentWord)
	assert.False(t, rawProtoV2)
}

func TestSanitizedArgsStripsBothProtocolFields(t *testing.T) {
	t.Parallel()
	// SanitizedArgs must strip both the sentinel and the cur field (round-5: never forward
	// currentWord to urfave, so unknown-command dispatch cannot fire on typed text).
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
