package extres

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	headerStart       = "# Kubeconfig "
	headerRevokeText  = " - shown once; revoke with: rancher delete kubeconfig"
	headerNoRevokeEnd = " - shown once; no revoke command is shown because the name cannot be printed so that every supported shell reads it as written; find it with: rancher get kubeconfigs"
	tokensStart       = "# Tokens: "
	tokensNoExpiry    = " - expiry not returned by the server\n"
)

// readShellWord reads the one word a POSIX sh command line holds in s: an
// unquoted word of letters, digits and the characters . _ / -, or one or more
// single-quoted segments (a segment may be empty) joined by an escaped quote
// the way clierr.ShellQuote writes a name with a quote in it. Any other text
// is an error, so a name with a space, a dollar sign or a quote that is not
// quoted cannot read as one word.
func readShellWord(s string) (string, error) {
	if s == "" {
		return "", errors.New("no word")
	}
	if s[0] != '\'' {
		for _, r := range s {
			plain := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
				r == '.' || r == '_' || r == '/' || r == '-'
			if !plain {
				return "", errors.New("the unquoted word holds " + strconv.QuoteRune(r))
			}
		}
		return s, nil
	}
	var word strings.Builder
	rest := s
	for {
		quoted, ok := strings.CutPrefix(rest, "'")
		if !ok {
			return "", errors.New("expected an opening quote before " + strconv.Quote(rest))
		}
		segment, after, found := strings.Cut(quoted, "'")
		if !found {
			return "", errors.New("a quoted segment has no closing quote")
		}
		word.WriteString(segment)
		if after == "" {
			return word.String(), nil
		}
		rest, ok = strings.CutPrefix(after, `\'`)
		if !ok {
			return "", errors.New("text after the closing quote: " + strconv.Quote(after))
		}
		word.WriteByte('\'')
	}
}

func TestReadShellWord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text string
		want string
		fail bool
	}{
		{text: "kubeconfig-abc12", want: "kubeconfig-abc12"},
		{text: "'kubeconfig-abc12'", want: "kubeconfig-abc12"},
		{text: "'kc x'", want: "kc x"},
		{text: `'it'\''s'`, want: "it's"},
		{text: `'a'\'''`, want: "a'"},
		{text: `''\'''`, want: "'"},
		{text: "''", want: ""},
		{text: "", fail: true},
		{text: "kc x", fail: true},
		{text: "a$b", fail: true},
		{text: "$(id);rm", fail: true},
		{text: `it's`, fail: true},
		{text: `a"b`, fail: true},
		{text: `a\b`, fail: true},
		{text: `'a'b`, fail: true},
		{text: `'a'\'`, fail: true},
		{text: `'a`, fail: true},
		{text: `'a' 'b'`, fail: true},
	} {
		got, err := readShellWord(tc.text)
		if tc.fail {
			require.Error(t, err, "%q", tc.text)
			continue
		}
		require.NoError(t, err, "%q", tc.text)
		assert.Equal(t, tc.want, got, "%q", tc.text)
	}
}

// hostileKubeconfigNames are names a server can send in its reply, as the
// name of the kubeconfig and as the name of a token. plain is true for a name
// clierr.NotQuotable reports false for, the only names the first header line
// gives a revoke command for.
var hostileKubeconfigNames = []struct {
	row   string
	name  string
	plain bool
}{
	{"rancherName", "kubeconfig-abc12", true},
	{"newline", "kc\nusers: []", false},
	{"carriageReturnNewline", "kc\r\nx", false},
	{"carriageReturn", "kc\rx", false},
	{"tab", "kc\tx", false},
	{"escape", "kc\x1b[2Jx", false},
	{"nul", "kc\x00x", false},
	{"delete", "kc\x7fx", false},
	{"nextLine", "kc\u0085x", false},
	{"controlSequenceIntroducer", "kc\u009bx", false},
	{"lineSeparator", "kc\u2028x", false},
	{"paragraphSeparator", "kc\u2029x", false},
	{"rightToLeftOverride", "kc\u202edcba", false},
	{"invisibleSeparator", "kc\u2063x", false},
	{"invalidByte", "kc\xffx", false},
	{"singleQuote", "it's", false},
	{"curlyQuote", "it\u2019s", false},
	{"zeroWidthSpace", "kc\u200bx", false},
	{"doubleQuote", `a"b`, false},
	{"space", "kc x", true},
	{"backslash", `a\b`, true},
	{"doubleBackslash", `a\\b`, false},
	{"trailingBackslash", `a\`, false},
	{"quoteAndComma", `a", "b`, false},
	{"shellMetacharacters", "$(id);rm", true},
	{"dollarSign", "a$b", true},
	{"leadingDash", "-h", true},
	{"leadingDoubleDash", "--apply", true},
	{"empty", "", false},
	{"long", strings.Repeat("a", 5000), true},
}

// TestCreateKubeconfigHeaderLinesStayComments asserts that whatever name the
// server sends for the kubeconfig and for a token, the output is two comment
// lines followed by the document byte for byte. The first line shows the name
// in Go's quoted form, holds no character a terminal acts on, and gives a
// revoke command that reads as the one name only for a name clierr.NotQuotable
// reports false for; any other name gets a line with the command that lists
// the kubeconfigs. The second line shows the
// token name in Go's quoted form and holds no character a terminal acts on;
// an empty token name names no token, and the line says none is listed.
func TestCreateKubeconfigHeaderLinesStayComments(t *testing.T) {
	t.Parallel()
	rawKC := "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\ncurrent-context: \"\"\n"
	for _, tc := range hostileKubeconfigNames {
		t.Run(tc.row, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.plain, !clierr.NotQuotable(tc.name), "the row's plain flag")

			cmd := NewCreateKubeconfig(stubFactory{c: &fakeClient{name: tc.name, status: map[string]any{"value": rawKC, "tokens": []any{tc.name}}}})
			_, exec, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
			require.NoError(t, err)
			require.NotNil(t, exec)
			res, err := exec(t.Context())
			require.NoError(t, err)
			require.NotNil(t, res.RawBytes)
			out := string(res.RawBytes)

			before, afterFirst, ok := strings.Cut(out, "\n")
			require.True(t, ok, "the output has no line end")
			tokensLine, after, ok := strings.Cut(afterFirst, "\n")
			require.True(t, ok, "the output has no second line end")
			assert.Equal(t, rawKC, after, "everything after the second line is the document, byte for byte")

			require.True(t, strings.HasPrefix(tokensLine, tokensStart), "line 2 is %q", tokensLine)
			assert.Equal(t, tokensLine, output.Printable(tokensLine), "line 2 holds a character a terminal acts on")
			if tc.name == "" {
				assert.Equal(t, "# Tokens: none listed by the server", tokensLine, "an empty token name names no token")
			} else {
				tokenQuoted, err := strconv.QuotedPrefix(strings.TrimPrefix(tokensLine, tokensStart))
				require.NoError(t, err, "line 2 does not start with the quoted token name")
				tokenName, err := strconv.Unquote(tokenQuoted)
				require.NoError(t, err)
				assert.Equal(t, tc.name, tokenName, "the quoted token name is the whole name")
				assert.Equal(t, tokensStart+tokenQuoted+tokensNoExpiry, tokensLine+"\n")
			}

			require.True(t, strings.HasPrefix(before, headerStart), "line 1 is %q", before)
			assert.Equal(t, before, output.Printable(before), "line 1 holds a character a terminal acts on")

			afterStart := strings.TrimPrefix(before, headerStart)
			quoted, err := strconv.QuotedPrefix(afterStart)
			require.NoError(t, err, "line 1 does not start with the quoted name")
			unquoted, err := strconv.Unquote(quoted)
			require.NoError(t, err)
			assert.Equal(t, tc.name, unquoted, "the quoted name is the whole name")

			rest := strings.TrimPrefix(afterStart, quoted)
			if tc.plain {
				command, found := strings.CutPrefix(rest, headerRevokeText)
				require.True(t, found, "the text after the name is %q", rest)
				if strings.HasPrefix(tc.name, "-") {
					command, found = strings.CutPrefix(command, " --apply --yes -- ")
					require.True(t, found, "a name that starts with a dash follows the flags and -- in the command, not %q", command)
				} else {
					command, found = strings.CutSuffix(strings.TrimPrefix(command, " "), " --apply --yes")
					require.True(t, found, "the flags follow the name in the command, not %q", command)
				}
				word, err := readShellWord(command)
				require.NoError(t, err, "the revoke command %q", command)
				assert.Equal(t, tc.name, word, "the revoke command names another object")
			} else {
				assert.Equal(t, headerNoRevokeEnd, rest, "a name that cannot be shown or is empty gets no command")
			}

			_, err = clientcmd.Load(res.RawBytes)
			require.NoError(t, err)
		})
	}
}

// TestCreateKubeconfigHeaderForARancherName records the header lines of the
// output for a name Rancher generates.
func TestCreateKubeconfigHeaderForARancherName(t *testing.T) {
	t.Parallel()
	rawKC := "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\ncurrent-context: \"\"\n"
	cmd := NewCreateKubeconfig(stubFactory{c: &fakeClient{name: "kubeconfig-abc12", status: map[string]any{"value": rawKC}}})
	_, exec, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	want := `# Kubeconfig "kubeconfig-abc12" - shown once; revoke with: rancher delete kubeconfig 'kubeconfig-abc12' --apply --yes` + "\n" +
		"# Tokens: none listed by the server\n" + rawKC
	assert.Equal(t, want, string(res.RawBytes))
}

// TestCreateKubeconfigHeaderForANameWithALeadingDash records the header lines
// of the output for a name that starts with a dash: the command puts the flags
// first and -- before the quoted name.
func TestCreateKubeconfigHeaderForANameWithALeadingDash(t *testing.T) {
	t.Parallel()
	rawKC := "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\ncurrent-context: \"\"\n"
	cmd := NewCreateKubeconfig(stubFactory{c: &fakeClient{name: "-h", status: map[string]any{"value": rawKC}}})
	_, exec, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	want := `# Kubeconfig "-h" - shown once; revoke with: rancher delete kubeconfig --apply --yes -- '-h'` + "\n" +
		"# Tokens: none listed by the server\n" + rawKC
	assert.Equal(t, want, string(res.RawBytes))
}

// kubeconfigReply builds the object a create returns, with the fields the
// tokens line reads.
func kubeconfigReply(tokens []any, spec map[string]any, creationTimestamp string) *unstructured.Unstructured {
	obj := map[string]any{"metadata": map[string]any{}, "spec": spec}
	if tokens != nil {
		obj["status"] = map[string]any{"tokens": tokens}
	}
	if creationTimestamp != "" {
		obj["metadata"] = map[string]any{"creationTimestamp": creationTimestamp}
	}
	return &unstructured.Unstructured{Object: obj}
}

// tokensLineOf returns the second line of the header for a reply.
func tokensLineOf(reply *unstructured.Unstructured) string {
	lines := strings.SplitN(kubeconfigHeader("kubeconfig-abc", reply), "\n", 3)
	return lines[1] + "\n"
}

// TestKubeconfigTokensLine asserts the exact second header line for each
// reply: the token names in Go's quoted form, and the expiry as the creation
// time plus the ttl of the reply, or a statement that the server returned no
// expiry.
func TestKubeconfigTokensLine(t *testing.T) {
	t.Parallel()
	const created = "2026-11-11T00:00:00Z"
	two := []any{"kubeconfig-u-abc", "kubeconfig-u-def"}
	for _, tc := range []struct {
		row   string
		reply *unstructured.Unstructured
		want  string
	}{
		{"twoTokensAndATtl", kubeconfigReply(two, map[string]any{"ttl": int64(2592000)}, created),
			`# Tokens: "kubeconfig-u-abc", "kubeconfig-u-def" - expiry 2026-12-11T00:00:00Z` + "\n"},
		{"ttlAsFloat", kubeconfigReply(two, map[string]any{"ttl": float64(3600)}, created),
			`# Tokens: "kubeconfig-u-abc", "kubeconfig-u-def" - expiry 2026-11-11T01:00:00Z` + "\n"},
		{"creationTimeWithAnOffset", kubeconfigReply(two, map[string]any{"ttl": int64(3600)}, "2026-11-11T02:00:00+02:00"),
			`# Tokens: "kubeconfig-u-abc", "kubeconfig-u-def" - expiry 2026-11-11T01:00:00Z` + "\n"},
		{"noTokens", kubeconfigReply([]any{}, map[string]any{"ttl": int64(2592000)}, created),
			"# Tokens: none listed by the server\n"},
		{"noTokensField", kubeconfigReply(nil, map[string]any{"ttl": int64(2592000)}, created),
			"# Tokens: none listed by the server\n"},
		{"onlyNamesThatAreNotStrings", kubeconfigReply([]any{int64(1), nil}, map[string]any{"ttl": int64(2592000)}, created),
			"# Tokens: none listed by the server\n"},
		{"onlyEmptyNames", kubeconfigReply([]any{"", ""}, map[string]any{"ttl": int64(2592000)}, created),
			"# Tokens: none listed by the server\n"},
		{"namesMixedWithANumberAndAnEmptyName", kubeconfigReply([]any{int64(1), "kubeconfig-u-abc", "", nil}, map[string]any{"ttl": int64(3600)}, created),
			`# Tokens: "kubeconfig-u-abc" - expiry 2026-11-11T01:00:00Z` + "\n"},
		{"ttlNotAWholeNumberOfSeconds", kubeconfigReply(two, map[string]any{"ttl": float64(0.5)}, created),
			`# Tokens: "kubeconfig-u-abc", "kubeconfig-u-def" - expiry not returned by the server` + "\n"},
		{"ttlWithAFraction", kubeconfigReply(two, map[string]any{"ttl": float64(3600.5)}, created),
			`# Tokens: "kubeconfig-u-abc", "kubeconfig-u-def" - expiry not returned by the server` + "\n"},
		{"noTtl", kubeconfigReply(two, map[string]any{}, created),
			`# Tokens: "kubeconfig-u-abc", "kubeconfig-u-def" - expiry not returned by the server` + "\n"},
		{"ttlZero", kubeconfigReply(two, map[string]any{"ttl": int64(0)}, created),
			`# Tokens: "kubeconfig-u-abc", "kubeconfig-u-def" - expiry not returned by the server` + "\n"},
		{"ttlNegative", kubeconfigReply(two, map[string]any{"ttl": int64(-1)}, created),
			`# Tokens: "kubeconfig-u-abc", "kubeconfig-u-def" - expiry not returned by the server` + "\n"},
		{"ttlNotANumber", kubeconfigReply(two, map[string]any{"ttl": "30d"}, created),
			`# Tokens: "kubeconfig-u-abc", "kubeconfig-u-def" - expiry not returned by the server` + "\n"},
		{"ttlTooLarge", kubeconfigReply(two, map[string]any{"ttl": int64(1) << 60}, created),
			`# Tokens: "kubeconfig-u-abc", "kubeconfig-u-def" - expiry not returned by the server` + "\n"},
		{"noCreationTimestamp", kubeconfigReply(two, map[string]any{"ttl": int64(2592000)}, ""),
			`# Tokens: "kubeconfig-u-abc", "kubeconfig-u-def" - expiry not returned by the server` + "\n"},
		{"creationTimestampNotATime", kubeconfigReply(two, map[string]any{"ttl": int64(2592000)}, "yesterday"),
			`# Tokens: "kubeconfig-u-abc", "kubeconfig-u-def" - expiry not returned by the server` + "\n"},
		{"tokenWithAControlCharacter", kubeconfigReply([]any{"kc\x1b[2Jx"}, map[string]any{"ttl": int64(3600)}, created),
			`# Tokens: "kc\x1b[2Jx" - expiry 2026-11-11T01:00:00Z` + "\n"},
		{"tokenWithALineBreak", kubeconfigReply([]any{"a\nb", "c\r\nd"}, map[string]any{"ttl": int64(3600)}, created),
			`# Tokens: "a\nb", "c\r\nd" - expiry 2026-11-11T01:00:00Z` + "\n"},
		{"tokenWithAQuoteAndAComma", kubeconfigReply([]any{`a", "b`}, map[string]any{"ttl": int64(3600)}, created),
			`# Tokens: "a\", \"b" - expiry 2026-11-11T01:00:00Z` + "\n"},
	} {
		t.Run(tc.row, func(t *testing.T) {
			t.Parallel()
			got := tokensLineOf(tc.reply)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, 1, strings.Count(got, "\n"), "the tokens line is one line")
			line := strings.TrimSuffix(got, "\n")
			assert.Equal(t, line, output.Printable(line), "the tokens line holds a character a terminal acts on")
		})
	}
}

// TestCreateKubeconfigHeaderExpiryComesFromTheReply asserts that the expiry in
// the header is the creation time plus the ttl the server returned, never the
// ttl the caller asked for.
func TestCreateKubeconfigHeaderExpiryComesFromTheReply(t *testing.T) {
	t.Parallel()
	rawKC := "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\ncurrent-context: \"\"\n"
	for _, tc := range []struct {
		row   string
		input map[string]string
		spec  map[string]any
		want  string
	}{
		{"askedForAnHourReplyHasThirtyDays", map[string]string{"ttl": "1h"}, map[string]any{"ttl": int64(2592000)},
			`# Tokens: "kubeconfig-u-abc" - expiry 2026-12-11T00:00:00Z` + "\n"},
		{"askedForNothingReplyHasTheDefault", map[string]string{}, map[string]any{"ttl": int64(7776000)},
			`# Tokens: "kubeconfig-u-abc" - expiry 2027-02-09T00:00:00Z` + "\n"},
		{"askedForAnHourReplyHasNoTtl", map[string]string{"ttl": "1h"}, map[string]any{"ttl": nil},
			`# Tokens: "kubeconfig-u-abc" - expiry not returned by the server` + "\n"},
	} {
		t.Run(tc.row, func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{
				name:              "kubeconfig-abc",
				status:            map[string]any{"value": rawKC, "tokens": []any{"kubeconfig-u-abc"}},
				spec:              tc.spec,
				creationTimestamp: "2026-11-11T00:00:00Z",
			}
			cmd := NewCreateKubeconfig(stubFactory{c: client})
			_, exec, err := planKubeconfig(t, cmd, in{s: tc.input})
			require.NoError(t, err)
			res, err := exec(t.Context())
			require.NoError(t, err)
			lines := strings.SplitN(string(res.RawBytes), "\n", 3)
			require.Len(t, lines, 3)
			assert.Equal(t, tc.want, lines[1]+"\n")
		})
	}
}
