package runtime

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
)

// TestMirrorWarningsStripsControlCharacters asserts a warning carrying
// server text cannot drive the terminal or forge a second warning line.
func TestMirrorWarningsStripsControlCharacters(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	mirrorWarnings(&buf, []string{"condition: a\x1b[2Jb\n# warning: forged"}, nil, style.Style{})
	assert.Equal(t, "# warning: condition: a[2Jb# warning: forged\n", buf.String())
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
