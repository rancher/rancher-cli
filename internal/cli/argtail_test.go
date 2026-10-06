package cli

import (
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestCompletionRequestIsDecidedByTheMarkersAlone asserts the one decision
// that a line is a completion request: it opens with the v2 marker, or ends
// with the flag every script puts last, wherever a "--" sits on the line. A line
// that only holds the flag mid-line is an ordinary command line.
func TestCompletionRequestIsDecidedByTheMarkersAlone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		proto2  bool
		request bool
	}{
		{"v2", []string{"--rancher-completion-proto=2", "get", "--rancher-completion-cur=", "--generate-shell-completion"}, true, true},
		{"v2 after a double dash", []string{"--rancher-completion-proto=2", "get", "--", "x", "--rancher-completion-cur=", "--generate-shell-completion"}, true, true},
		{"v2 without the flag", []string{"--rancher-completion-proto=2", "get", "--rancher-completion-cur="}, true, true},
		{"v1", []string{"get", "--generate-shell-completion"}, false, true},
		{"v1 after a double dash", []string{"get", "--", "x", "--generate-shell-completion"}, false, true},
		{"the flag mid-line", []string{"get", "--generate-shell-completion", "x"}, false, false},
		{"ordinary line", []string{"config", "add-server", "--", "x"}, false, false},
		{"no words", nil, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			proto2, request := completionRequest(tt.args)
			assert.Equal(t, tt.proto2, proto2)
			assert.Equal(t, tt.request, request)
		})
	}
}

// TestSanitizedArgsCutsACompletionRequestAtTheDoubleDash asserts that urfave
// receives a completion request as one it completes: the words from the first
// "--" on are dropped (it reads the flag after one as an argument and runs the
// command) and the flag is last. Under v1 a "--" that is the last word is the
// word being completed and stays. A line that is not a request is passed on
// unchanged, "--" included.
func TestSanitizedArgsCutsACompletionRequestAtTheDoubleDash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			"v2",
			[]string{"rancher", "--rancher-completion-proto=2", "config", "add-server", "--apply", "--", "myserver", "--rancher-completion-cur=", "--generate-shell-completion"},
			[]string{"rancher", "config", "add-server", "--apply", "--generate-shell-completion"},
		},
		{
			"v1",
			[]string{"rancher", "config", "add-server", "--apply", "--", "myserver", "--generate-shell-completion"},
			[]string{"rancher", "config", "add-server", "--apply", "--generate-shell-completion"},
		},
		{
			"the double dash is the word being completed under v2",
			[]string{"rancher", "--rancher-completion-proto=2", "config", "--rancher-completion-cur=--", "--generate-shell-completion"},
			[]string{"rancher", "config", "--generate-shell-completion"},
		},
		{
			"the double dash is the last word under v2",
			[]string{"rancher", "--rancher-completion-proto=2", "config", "add-server", "--", "--rancher-completion-cur=", "--generate-shell-completion"},
			[]string{"rancher", "config", "add-server", "--generate-shell-completion"},
		},
		{
			"the double dash is the word being completed under v1",
			[]string{"rancher", "usage", "--", "--generate-shell-completion"},
			[]string{"rancher", "usage", "--", "--generate-shell-completion"},
		},
		{
			"a second double dash under v1",
			[]string{"rancher", "usage", "--", "--", "--generate-shell-completion"},
			[]string{"rancher", "usage", "--generate-shell-completion"},
		},
		{
			"v2 without the flag",
			[]string{"rancher", "--rancher-completion-proto=2", "config", "add-server", "--", "myserver", "--rancher-completion-cur="},
			[]string{"rancher", "config", "add-server", "--generate-shell-completion"},
		},
		{
			"v2 with the flag mid-line",
			[]string{"rancher", "--rancher-completion-proto=2", "config", "--generate-shell-completion", "--", "x", "--rancher-completion-cur="},
			[]string{"rancher", "config", "--generate-shell-completion"},
		},
		{
			"a double dash first",
			[]string{"rancher", "--", "config", "--generate-shell-completion"},
			[]string{"rancher", "--generate-shell-completion"},
		},
		{
			"an ordinary line",
			[]string{"rancher", "config", "add-server", "--apply", "--", "myserver"},
			[]string{"rancher", "config", "add-server", "--apply", "--", "myserver"},
		},
		{
			"an ordinary line holding the flag mid-line",
			[]string{"rancher", "get", "--generate-shell-completion", "--", "x"},
			[]string{"rancher", "get", "--generate-shell-completion", "--", "x"},
		},
		{"no program name", nil, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, SanitizedArgs(tt.in))
		})
	}
}

// TestCaptureArgvKeepsTheWordsAfterADoubleDash asserts that the completion
// callbacks read the whole typed line, the words after a "--" included, from
// rawArgv.
func TestCaptureArgvKeepsTheWordsAfterADoubleDash(t *testing.T) {
	// NOT parallel: mutates package globals.
	orig := rawArgv
	origW, origP, origV1 := rawCurrentWord, rawProtoV2, rawV1Completion
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawProtoV2 = origP; rawV1Completion = origV1 })

	CaptureArgv([]string{"--rancher-completion-proto=2", "get", "pod", "--", "x", "--rancher-completion-cur=y", "--generate-shell-completion"})
	assert.Equal(t, []string{"get", "pod", "--", "x"}, rawArgv)
	assert.Equal(t, "y", rawCurrentWord)
	assert.True(t, rawProtoV2)

	CaptureArgv([]string{"get", "pod", "--", "x", "--generate-shell-completion"})
	assert.Equal(t, []string{"get", "pod", "--", "x"}, rawArgv)
	assert.False(t, rawProtoV2)
	assert.True(t, rawV1Completion)

	CaptureArgv([]string{"get", "pod", "--", "x"})
	assert.False(t, completionActive())

	// Not a request: the word is an ordinary one, a flag's value, and stays.
	CaptureArgv([]string{"get", "pod", "--cluster", "--generate-shell-completion", "--namespace", "x"})
	assert.Equal(t, []string{"get", "pod", "--cluster", "--generate-shell-completion", "--namespace", "x"}, rawArgv)
	assert.False(t, completionActive())
}

// TestScanTailDelimiter asserts which "--" ends the flags of a tail: the first
// one that is not a flag's value and does not follow a word urfave reads as a
// value, with its index in the tail.
func TestScanTailDelimiter(t *testing.T) {
	t.Parallel()
	flags := map[string]bool{"output": true, "o": true, "debug": false}
	tests := []struct {
		name string
		tail []string
		want int
	}{
		{"none", []string{"a", "--debug"}, -1},
		{"first word", []string{"--", "a"}, 0},
		{"after a word and a flag", []string{"a", "--debug", "--", "b"}, 2},
		{"the value of a flag", []string{"-o", "--", "a"}, -1},
		{"after the value of a flag", []string{"-o", "json", "--", "a"}, 2},
		{"after an equals value", []string{"--output=json", "--"}, 1},
		{"a second double dash is a word", []string{"--", "--", "a"}, 0},
		{"after a word read as a value", []string{"-1", "--"}, -1},
		{"at the end", []string{"a", "--"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := slices.IndexFunc(scanTail(tt.tail, flags), func(w tailWord) bool { return w.kind == endWord })
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestDoubleDashSiteCountsTheWordsAfterIt asserts the argument slot after a
// "--" and the positionals a live source reads: the words before the "--" that
// are not flags or flag values, then the words after it, with the word being
// completed left out under v2 (a separate field) and under v1 (the last word).
// A v1 line that ends at the "--" ends no flags: the "--" is the word being
// completed. NOT parallel: sets rawProtoV2 and rawCurrentWord.
func TestDoubleDashSiteCountsTheWordsAfterIt(t *testing.T) {
	origW, origP := rawCurrentWord, rawProtoV2
	t.Cleanup(func() { rawCurrentWord = origW; rawProtoV2 = origP })
	flags := map[string]bool{"cluster": true}
	leafFlags := []runtime.ArgSpec{{Name: "cluster", Type: "string"}}

	rawProtoV2, rawCurrentWord = true, "z"
	tail := []string{"--cluster", "c1", "first", "--", "second", "--cluster"}
	delimiter := doubleDashAt(tail, flags)
	require.Equal(t, 3, delimiter)
	site := doubleDashSite(tail, delimiter, flags)
	assert.Equal(t, CompletionSite{Kind: SitePositional, Index: 3, Args: []string{"first", "second", "--cluster"}, Partial: "z", DoubleDash: true, After: []string{"second", "--cluster"}}, site)
	assert.Equal(t, []string{"first", "second", "--cluster"}, typedPositionals(site, tail[:delimiter], leafFlags))

	rawProtoV2, rawCurrentWord = false, ""
	tail = []string{"--cluster", "c1", "first", "--", "second", "z"}
	delimiter = doubleDashAt(tail, flags)
	require.Equal(t, 3, delimiter)
	site = doubleDashSite(tail, delimiter, flags)
	assert.Equal(t, CompletionSite{Kind: SitePositional, Index: 2, Args: []string{"first", "second"}, Partial: "z", DoubleDash: true, After: []string{"second"}}, site)
	assert.Equal(t, []string{"first", "second"}, typedPositionals(site, tail[:delimiter], leafFlags))

	assert.Equal(t, -1, doubleDashAt([]string{"first", "--"}, flags))
	rawProtoV2 = true
	assert.Equal(t, 1, doubleDashAt([]string{"first", "--"}, flags))
}
