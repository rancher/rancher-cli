package runtime

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// warningExpand writes the remedy command of a warning the way an invocation
// with a config directory does.
func warningExpand(s string) string {
	return strings.ReplaceAll(s, clierr.Rancher, "rancher --config 'd'")
}

// mirrored returns what mirrorWarnings writes for warnings.
func mirrored(warnings []string, silent map[string]int) string {
	var buf bytes.Buffer
	mirrorWarnings(&buf, warnings, silent, style.Style{}, warningExpand)
	return buf.String()
}

// TestMirrorWarningsWritesHiddenCharactersAsEscapes asserts a warning clause
// that has a hidden character or an invalid byte is written whole in double
// quotes with escapes; it cannot drive the terminal or forge a second line, and
// every other clause and every remedy command stays as written.
func TestMirrorWarningsWritesHiddenCharactersAsEscapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		warn string
		want string
	}{
		{
			name: "a warning with nothing to quote",
			warn: "cert expires soon",
			want: "# warning: cert expires soon\n",
		},
		{
			name: "an escape sequence and a forged warning line",
			warn: "condition: a\x1b[2Jb\n# warning: forged",
			want: `# warning: "condition: a\x1b[2Jb\n# warning: forged"` + "\n",
		},
		{
			name: "only the clause with the hidden character is quoted",
			warn: "value a\u200bb; run later",
			want: `# warning: "value a\u200bb"; run later` + "\n",
		},
		{
			name: "a command after a quoted clause expands",
			warn: "x\u200b; " + clierr.Rancher + " get pods",
			want: `# warning: "x\u200b"; rancher --config 'd' get pods` + "\n",
		},
		{
			name: "an invalid byte",
			warn: "a\xffb",
			want: `# warning: "a\xffb"` + "\n",
		},
		{
			name: "a single quote and a hidden character quote only the piece",
			warn: "o'b\u200b",
			want: `# warning: o'"b\u200b"` + "\n",
		},
		{
			name: "a hidden character before the command in the same clause",
			warn: "x\u200b run " + clierr.Rancher + " auth status",
			want: `# warning: "x\u200b run "rancher --config 'd' auth status` + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, mirrored([]string{tc.warn}, nil))
		})
	}

	t.Run("a remedy marker with no command after it is written as escapes", func(t *testing.T) {
		t.Parallel()
		got := mirrored([]string{"a" + clierr.RemedyMarker + "x"}, nil)
		assert.Equal(t, "# warning: "+strconv.Quote("a"+clierr.RemedyMarker+"x")+"\n", got)
		assert.NotRegexp(t, "[\u2060-\u2064]", got)
	})

	t.Run("another process's remedy marker before rancher is written as escapes", func(t *testing.T) {
		t.Parallel()
		got := mirrored([]string{"a" + "\u2063" + strings.Repeat("\u2060\u2061\u2062\u2064", 8) + "rancher x"}, nil)
		foreign := `\u2063` + strings.Repeat(`\u2060\u2061\u2062\u2064`, 8)
		assert.Equal(t, `# warning: "a`+foreign+`rancher x"`+"\n", got)
		assert.NotRegexp(t, "[\u2060-\u2064]", got)
	})
}

// TestMirrorWarningsSilentSkipsTheEntryAsTheCommandWroteIt asserts silent is
// read against the warnings as the command wrote them: one entry that has a
// remedy command is skipped and its second copy is written, expanded.
func TestMirrorWarningsSilentSkipsTheEntryAsTheCommandWroteIt(t *testing.T) {
	t.Parallel()
	raw := "run " + clierr.Rancher + " auth status"
	got := mirrored([]string{raw, raw}, map[string]int{raw: 1})
	assert.Equal(t, "# warning: run rancher --config 'd' auth status\n", got)
}

// TestMirrorWarningsKeepsDifferentValuesApart asserts values that differ only
// by a hidden character or an invalid byte print as four different lines.
func TestMirrorWarningsKeepsDifferentValuesApart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ warn, want string }{
		{"a\u200bb", `# warning: "a\u200bb"` + "\n"},
		{"a\u200cb", `# warning: "a\u200cb"` + "\n"},
		{"ab", "# warning: ab\n"},
		{"a\xffb", `# warning: "a\xffb"` + "\n"},
	} {
		assert.Equal(t, tc.want, mirrored([]string{tc.warn}, nil))
	}
	lines := map[string]bool{}
	for _, w := range []string{"a\u200bb", "a\u200cb", "ab", "a\xffb"} {
		lines[mirrored([]string{w}, nil)] = true
	}
	assert.Len(t, lines, 4)
}

// summaryLine returns the first line a table result with the given summary
// writes to stdout, with the remedy directory set to dir.
func summaryLine(t *testing.T, summary, dir string) string {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := fakeRead{
		d:   Descriptor{Name: "get cluster", CommandType: ReadOnly},
		res: Result{Kind: "Cluster", Data: widgetTable{Name: "n"}, Summary: summary},
	}
	_, ce := Run(t.Context(), cmd, mapInput{}, IO{Out: &out, Err: &errBuf, Format: "table", RemedyDir: dir}, safety.Binding{})
	require.Nil(t, ce)
	line, _, found := strings.Cut(out.String(), "\n")
	require.True(t, found)
	return line
}

// TestSummaryLineWritesHiddenCharactersAsEscapes asserts the summary line over
// a table writes a clause that has a hidden character in double quotes with
// escapes, expands a remedy command that follows it, writes an ordinary
// summary as it is, and prints values that differ only by a hidden character
// differently.
func TestSummaryLineWritesHiddenCharactersAsEscapes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	prefix, ok := RemedyPrefix(dir)
	require.True(t, ok)

	assert.Equal(t, `"wait: a\u200bb satisfied after 3s"`, summaryLine(t, "wait: a\u200bb satisfied after 3s", dir))
	assert.Equal(t, `"x\u200b run "rancher`+prefix+` auth status`,
		summaryLine(t, "x\u200b run "+clierr.Rancher+" auth status", dir))
	assert.Equal(t, "wait: ab satisfied after 3s", summaryLine(t, "wait: ab satisfied after 3s", dir))

	lines := map[string]bool{}
	for _, v := range []string{"a\u200bb", "a\u200cb", "ab"} {
		lines[summaryLine(t, v, dir)] = true
	}
	assert.Len(t, lines, 3)
}

// TestErrorTextQuotesControlCharacters asserts the default-format error
// block writes the message and the hint, both of which may carry server text,
// as double-quoted strings with escapes where they have a control character.
func TestErrorTextQuotesControlCharacters(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeValidation, "denied: a\x1b[2Jb\nforged line").
		WithHint("rancher get x\r")
	out := errorText(ce, style.Style{}, plainRemedies)
	assert.NotContains(t, out, "\x1b")
	assert.NotContains(t, out, "\r")
	assert.Contains(t, out, `"denied: a\x1b[2Jb\nforged line"`)
	assert.Contains(t, out, `"rancher get x\r"`)
}

// TestDryRunTextStripsControlCharacters asserts a diff value from the
// server's dry-run answer cannot forge a preview line.
func TestDryRunTextStripsControlCharacters(t *testing.T) {
	t.Parallel()
	eff := safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionCreate, DisplayKind: "project", DisplayName: "web",
		Diff: []safety.DiffEntry{{Path: "/spec/description", From: "", To: "a\nb\x1b[2J"}}}
	out := dryRunText(eff)
	assert.Contains(t, out, "/spec/description:  -> ab[2J")
	assert.NotContains(t, out, "\x1b")
}

// candidatesLine returns the candidates line of the default-format error text
// for the candidates, cut to limit when limit is positive, with its
// indentation removed.
func candidatesLine(t *testing.T, candidates []clierr.Candidate, limit int) string {
	t.Helper()
	ce := clierr.New(clierr.CodeNotFound, "not found")
	if limit > 0 {
		ce = ce.WithCandidatesCut(candidates, limit)
	} else {
		ce = ce.WithCandidates(candidates)
	}
	for line := range strings.SplitSeq(errorText(ce, style.Style{}, plainRemedies), "\n") {
		if strings.HasPrefix(line, "  candidates") {
			return strings.TrimPrefix(line, "  ")
		}
	}
	t.Fatalf("no candidates line in %q", errorText(ce, style.Style{}, plainRemedies))
	return ""
}

// TestErrorTextCandidatesSafeValues asserts a candidate name and id with no
// hidden character and no invalid byte print as they are, a single quote and
// double quotes included.
func TestErrorTextCandidatesSafeValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		candidates []clierr.Candidate
		want       string
	}{
		{
			name: "safe values are unchanged",
			candidates: []clierr.Candidate{
				{Name: "web-prod", ID: "c-1"},
				{Name: `'"ops"'`, ID: `"$PASSWORD"`},
			},
			want: `candidates: web-prod (c-1), '"ops"' ("$PASSWORD")`,
		},
		{
			name:       "a single quote without a hidden character",
			candidates: []clierr.Candidate{{Name: "o'b", ID: "x"}},
			want:       `candidates: o'b (x)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, candidatesLine(t, tc.candidates, 0))
		})
	}
}

// TestErrorTextEscapesCandidates asserts a candidate's name and id show each
// hidden character and each invalid UTF-8 byte as an escape, double-quoting
// only the parts of the value, split at each single quote, that hold either;
// two values that differ only by a hidden character or an invalid byte print
// differently.
func TestErrorTextEscapesCandidates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		candidates []clierr.Candidate
		limit      int
		want       string
	}{
		{
			name:       "hidden rune and newline",
			candidates: []clierr.Candidate{{Name: "n\x1b[0m", ID: "id\n"}},
			want:       `candidates: "n\x1b[0m" ("id\n")`,
		},
		{
			name: "a pair that differs only by an invalid byte",
			candidates: []clierr.Candidate{
				{Name: "a\xffb", ID: "c-1"},
				{Name: "a\x9bb", ID: "c-2"},
			},
			want: `candidates: "a\xffb" (c-1), "a\x9bb" (c-2)`,
		},
		{
			name:       "an id with an invalid byte",
			candidates: []clierr.Candidate{{Name: "x", ID: "u-a\x9bb"}},
			want:       `candidates: x ("u-a\x9bb")`,
		},
		{
			name:       "a single quote and a hidden character in the name",
			candidates: []clierr.Candidate{{Name: "o'b\u200b", ID: "x"}},
			want:       `candidates: o'"b\u200b" (x)`,
		},
		{
			name:       "a single quote and a hidden character in the id",
			candidates: []clierr.Candidate{{Name: "x", ID: "o'b\u200b"}},
			want:       `candidates: x (o'"b\u200b")`,
		},
		{
			name:       "CRLF in the name",
			candidates: []clierr.Candidate{{Name: "a\r\nb", ID: "x"}},
			want:       `candidates: "a\r\nb" (x)`,
		},
		{
			name:       "CRLF in the id",
			candidates: []clierr.Candidate{{Name: "x", ID: "a\r\nb"}},
			want:       `candidates: x ("a\r\nb")`,
		},
		{
			name: "a cut list with its count",
			candidates: []clierr.Candidate{
				{Name: "a", ID: "u-1\u200b"},
				{Name: "b", ID: "u-2"},
				{Name: "c", ID: "u-3"},
			},
			limit: 2,
			want:  `candidates (2 of 3): a ("u-1\u200b"), b (u-2)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := candidatesLine(t, tc.candidates, tc.limit)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "\x1b")
			assert.NotContains(t, got, "\r")
			assert.NotContains(t, got, "\ufffd")
			assert.NotContains(t, got, "\u200b")
		})
	}
}

// TestErrorTextCandidatesThatReadAsEscapedStrings asserts a candidate name or
// id that is itself the double-quoted, escaped form of a text with a hidden
// character prints differently from a candidate that holds the character, in
// the name and in the id. A value that holds a single quote, or that
// unquotes to a printable text, prints as it is.
func TestErrorTextCandidatesThatReadAsEscapedStrings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		candidates []clierr.Candidate
		want       string
	}{
		{
			name: "two names, one the escaped form of the other",
			candidates: []clierr.Candidate{
				{Name: "\"a\\u200bb\"", ID: "c-1"},
				{Name: "a\u200bb", ID: "c-2"},
			},
			want: `candidates: "\"a\\u200bb\"" (c-1), "a\u200bb" (c-2)`,
		},
		{
			name: "two ids, one the escaped form of the other",
			candidates: []clierr.Candidate{
				{Name: "x", ID: "\"a\\u200bb\""},
				{Name: "y", ID: "a\u200bb"},
			},
			want: `candidates: x ("\"a\\u200bb\""), y ("a\u200bb")`,
		},
		{
			name:       "double-quoted values that unquote to printable text",
			candidates: []clierr.Candidate{{Name: "\"ops\"", ID: "\"$PASSWORD\""}},
			want:       `candidates: "ops" ("$PASSWORD")`,
		},
		{
			name:       "an escaped form that holds a single quote",
			candidates: []clierr.Candidate{{Name: "\"o'b\\u200b\"", ID: "x"}},
			want:       `candidates: "o'b\u200b" (x)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, candidatesLine(t, tc.candidates, 0))
		})
	}
}
