package cli

import (
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// runCompletionRequest runs on root the completion request the shipped
// scripts send for typed, the words before the cursor, and current, the word
// being completed: CaptureArgv, then Run on the argv SanitizedArgs hands
// urfave, as main does. The argv globals are restored when the test ends.
func runCompletionRequest(t *testing.T, root *ucli.Command, typed []string, current string) error {
	t.Helper()
	restoreRawArgv(t)
	argv := append(append([]string{protoSentinel}, typed...), curPrefix+current, completionFlag)
	CaptureArgv(argv)
	return root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, argv...)))
}

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
		want        CompletionSite
	}{
		// currentWord is classified first.
		{
			name:        "empty currentWord, no args → positional 0",
			args:        []string{},
			currentWord: "",
			want:        CompletionSite{Kind: SitePositional, Index: 0},
		},
		{
			name:        "empty currentWord, one positional in args → positional 1",
			args:        []string{"prod"},
			currentWord: "",
			want:        CompletionSite{Kind: SitePositional, Index: 1, Args: []string{"prod"}},
		},
		{
			name:        "string flag value consumed (non-final) → positional 1",
			args:        []string{"--cluster", "prod", "foo"},
			currentWord: "",
			want:        CompletionSite{Kind: SitePositional, Index: 1, Args: []string{"foo"}},
		},
		{
			name:        "bool flag → positional 0",
			args:        []string{"--verbose"},
			currentWord: "",
			want:        CompletionSite{Kind: SitePositional, Index: 0},
		},
		// Equals-form currentWord classifies as an equals-form SiteFlagValue.
		{
			name:        "equals-form currentWord → flagValue equals (currentWord classified first)",
			args:        []string{},
			currentWord: "--by=n",
			want:        CompletionSite{Kind: SiteFlagValue, FlagName: "by", FlagSpelling: "--by", EqualsForm: true, Partial: "n"},
		},
		{
			// A partial flag in currentWord classifies as SiteNameLike.
			name:        "partial flag currentWord → SiteNameLike (urfave default completer)",
			args:        []string{},
			currentWord: "--cl",
			want:        CompletionSite{Kind: SiteNameLike},
		},
		{
			// A non-flag currentWord after a value-taking flag classifies as
			// → space-form SiteFlagValue with Partial=currentWord.
			name:        "non-flag currentWord, flag-last in args → SiteFlagValue space-form",
			args:        []string{"--cluster"},
			currentWord: "c-abc",
			want:        CompletionSite{Kind: SiteFlagValue, FlagName: "cluster", FlagSpelling: "--cluster", EqualsForm: false, Partial: "c-abc"},
		},
		{
			// A non-flag currentWord with no trailing flag classifies as
			// SitePositional.
			name:        "non-flag currentWord, no trailing flag → SitePositional",
			args:        []string{"--cluster", "c-abc"},
			currentWord: "mypod",
			want:        CompletionSite{Kind: SitePositional, Index: 0, Partial: "mypod"},
		},
		{
			// Both-sides test A: flag + completed-value word + currentWord="" → positional.
			// "--cluster c-test " TAB: rawArgv=["--cluster","c-test"], currentWord=""
			name:        "flag+value in args, currentWord empty → next positional (disambiguates space-after-value)",
			args:        []string{"--cluster", "c-test"},
			currentWord: "",
			want:        CompletionSite{Kind: SitePositional, Index: 0},
		},
		{
			// Both-sides test B: flag only in args + non-empty currentWord → flagValue partial.
			// "--cluster c-" TAB: rawArgv=["--cluster"], currentWord="c-"
			name:        "flag-only in args, currentWord non-empty → flagValue with Partial (disambiguates partial-value)",
			args:        []string{"--cluster"},
			currentWord: "c-",
			want:        CompletionSite{Kind: SiteFlagValue, FlagName: "cluster", FlagSpelling: "--cluster", EqualsForm: false, Partial: "c-"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ParseArgTail(tc.args, tc.currentWord, testLeaf, globalArgSpecs)
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
			got := ParseArgTail([]string{tc.flagArg}, "", tc.leaf, globalArgSpecs)
			assert.Equal(t, tc.wantKind, got.Kind)
			if tc.wantKind == SiteFlagValue {
				assert.Equal(t, tc.wantName, got.FlagName)
			}
		})
	}
}

func TestCaptureArgvProtoMarkerExtractsCurField(t *testing.T) {
	// NOT parallel: mutates rawArgv/rawCurrentWord/rawCompleting package globals.
	orig := rawArgv
	origW, origP := rawCurrentWord, rawCompleting
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawCompleting = origP })
	// --rancher-completion-cur=c- carries the partial; all other words go to rawArgv.
	CaptureArgv([]string{"--rancher-completion-proto=2", "get", "pod", "--cluster", "--rancher-completion-cur=c-", "--generate-shell-completion"})
	assert.Equal(t, []string{"get", "pod", "--cluster"}, rawArgv)
	assert.Equal(t, "c-", rawCurrentWord)
	assert.True(t, rawCompleting)
}

func TestCaptureArgvEmptyCurrentWord(t *testing.T) {
	// NOT parallel: mutates package globals.
	orig := rawArgv
	origW, origP := rawCurrentWord, rawCompleting
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawCompleting = origP })
	// --rancher-completion-cur= (empty value) → cursor after space, no partial typed.
	CaptureArgv([]string{"--rancher-completion-proto=2", "get", "pod", "--cluster", "c-test", "--rancher-completion-cur=", "--generate-shell-completion"})
	assert.Equal(t, []string{"get", "pod", "--cluster", "c-test"}, rawArgv)
	assert.Equal(t, "", rawCurrentWord)
	assert.True(t, rawCompleting)
}

func TestCaptureArgvEqualsFormCurrentWord(t *testing.T) {
	// NOT parallel: mutates package globals.
	// --rancher-completion-cur=--by=n carries an equals-form current word.
	orig := rawArgv
	origW, origP := rawCurrentWord, rawCompleting
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawCompleting = origP })
	CaptureArgv([]string{"--rancher-completion-proto=2", "usage", "--rancher-completion-cur=--by=n", "--generate-shell-completion"})
	assert.Equal(t, []string{"usage"}, rawArgv)
	assert.Equal(t, "--by=n", rawCurrentWord)
	assert.True(t, rawCompleting)
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
			"", leaf, globalArgSpecs)
		assert.Equal(t, SiteFlagValue, site.Kind)
		assert.Equal(t, "set", site.FlagName)
	})

	t.Run("completed occurrences do not shift positional index", func(t *testing.T) {
		t.Parallel()
		site := ParseArgTail([]string{"digitaloceanconfig", "--set", "region=nyc3"},
			"", leaf, globalArgSpecs)
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

// TestCompletionRequestIsDecidedByTheMarkerAlone asserts the one decision
// that a line is a completion request: it opens with the protocol marker,
// wherever a "--" sits on the line. A line without the marker is an ordinary
// command line, wherever it holds the flag.
func TestCompletionRequestIsDecidedByTheMarkerAlone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		request bool
	}{
		{"a request", []string{"--rancher-completion-proto=2", "get", "--rancher-completion-cur=", "--generate-shell-completion"}, true},
		{"after a double dash", []string{"--rancher-completion-proto=2", "get", "--", "x", "--rancher-completion-cur=", "--generate-shell-completion"}, true},
		{"without the flag", []string{"--rancher-completion-proto=2", "get", "--rancher-completion-cur="}, true},
		{"the marker after the first word", []string{"get", "--rancher-completion-proto=2", "--rancher-completion-cur=", "--generate-shell-completion"}, false},
		{"the flag last without the marker", []string{"get", "--generate-shell-completion"}, false},
		{"the flag mid-line", []string{"get", "--generate-shell-completion", "x"}, false},
		{"ordinary line", []string{"config", "add-server", "--", "x"}, false},
		{"no words", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.request, completionRequest(tt.args))
		})
	}
}

// TestSanitizedArgsCutsACompletionRequestAtTheDoubleDash asserts that urfave
// receives a completion request as one it completes: the words from the first
// "--" on are dropped (it reads the flag after one as an argument and runs the
// command) and the flag is last. A line that is not a request is passed on
// unchanged, "--" included.
func TestSanitizedArgsCutsACompletionRequestAtTheDoubleDash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			"a request",
			[]string{"rancher", "--rancher-completion-proto=2", "config", "add-server", "--apply", "--", "myserver", "--rancher-completion-cur=", "--generate-shell-completion"},
			[]string{"rancher", "config", "add-server", "--apply", "--generate-shell-completion"},
		},
		{
			"the double dash is the word being completed",
			[]string{"rancher", "--rancher-completion-proto=2", "config", "--rancher-completion-cur=--", "--generate-shell-completion"},
			[]string{"rancher", "config", "--generate-shell-completion"},
		},
		{
			"the double dash is the last word",
			[]string{"rancher", "--rancher-completion-proto=2", "config", "add-server", "--", "--rancher-completion-cur=", "--generate-shell-completion"},
			[]string{"rancher", "config", "add-server", "--generate-shell-completion"},
		},
		{
			"a second double dash",
			[]string{"rancher", "--rancher-completion-proto=2", "usage", "--", "--", "--rancher-completion-cur=", "--generate-shell-completion"},
			[]string{"rancher", "usage", "--generate-shell-completion"},
		},
		{
			"without the flag",
			[]string{"rancher", "--rancher-completion-proto=2", "config", "add-server", "--", "myserver", "--rancher-completion-cur="},
			[]string{"rancher", "config", "add-server", "--generate-shell-completion"},
		},
		{
			"the flag mid-line",
			[]string{"rancher", "--rancher-completion-proto=2", "config", "--generate-shell-completion", "--", "x", "--rancher-completion-cur="},
			[]string{"rancher", "config", "--generate-shell-completion"},
		},
		{
			"a double dash first",
			[]string{"rancher", "--rancher-completion-proto=2", "--", "config", "--rancher-completion-cur=", "--generate-shell-completion"},
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
	origW, origP := rawCurrentWord, rawCompleting
	t.Cleanup(func() { rawArgv = orig; rawCurrentWord = origW; rawCompleting = origP })

	CaptureArgv([]string{"--rancher-completion-proto=2", "get", "pod", "--", "x", "--rancher-completion-cur=y", "--generate-shell-completion"})
	assert.Equal(t, []string{"get", "pod", "--", "x"}, rawArgv)
	assert.Equal(t, "y", rawCurrentWord)
	assert.True(t, rawCompleting)

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
// completed (a separate field) left out. A "--" that is the last word ends the
// flags. NOT parallel: sets rawCurrentWord.
func TestDoubleDashSiteCountsTheWordsAfterIt(t *testing.T) {
	origW := rawCurrentWord
	t.Cleanup(func() { rawCurrentWord = origW })
	flags := map[string]bool{"cluster": true}
	leafFlags := []runtime.ArgSpec{{Name: "cluster", Type: "string"}}

	rawCurrentWord = "z"
	tail := []string{"--cluster", "c1", "first", "--", "second", "--cluster"}
	delimiter := doubleDashAt(tail, flags)
	require.Equal(t, 3, delimiter)
	site := doubleDashSite(tail, delimiter, flags)
	assert.Equal(t, CompletionSite{Kind: SitePositional, Index: 3, Args: []string{"first", "second", "--cluster"}, Partial: "z", DoubleDash: true, After: []string{"second", "--cluster"}}, site)
	assert.Equal(t, []string{"first", "second", "--cluster"}, typedPositionals(site, tail[:delimiter], leafFlags))

	assert.Equal(t, 1, doubleDashAt([]string{"first", "--"}, flags))
}
