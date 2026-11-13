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

// TestErrorTextStripsControlCharacters asserts the default-format error
// block strips control characters from the message and the hint, both of
// which may carry server text.
func TestErrorTextStripsControlCharacters(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeValidation, "denied: a\x1b[2Jb\nforged line").
		WithHint("rancher get x\r")
	out := errorText(ce, style.Style{})
	assert.NotContains(t, out, "\x1b")
	assert.NotContains(t, out, "\r")
	assert.Contains(t, out, "denied: a[2Jbforged line")
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
	for line := range strings.SplitSeq(errorText(ce, style.Style{}), "\n") {
		if strings.HasPrefix(line, "  candidates") {
			return strings.TrimPrefix(line, "  ")
		}
	}
	t.Fatalf("no candidates line in %q", errorText(ce, style.Style{}))
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
