package extres

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/output"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	headerStart       = "# Kubeconfig "
	headerRevokeText  = " - shown once; revoke with: rancher delete kubeconfig "
	headerNoRevokeEnd = " - shown once; no revoke command is shown because the name is empty or holds characters a terminal cannot show"
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

// hostileKubeconfigNames are names a server can send in its reply. plain is
// true for a name that prints as it is and is not empty, the only names the
// header gives a revoke command for.
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
	{"singleQuote", "it's", true},
	{"doubleQuote", `a"b`, true},
	{"space", "kc x", true},
	{"backslash", `a\b`, true},
	{"shellMetacharacters", "$(id);rm", true},
	{"dollarSign", "a$b", true},
	{"leadingDash", "-h", true},
	{"leadingDoubleDash", "--apply", true},
	{"empty", "", false},
	{"long", strings.Repeat("a", 5000), true},
}

// TestCreateKubeconfigHeaderStaysOneCommentLine asserts that whatever name
// the server sends, the output is one comment line followed by the document
// byte for byte: the first line shows the name in Go's quoted form, holds no
// character a terminal acts on, and gives a revoke command that reads as the
// one name only for a name that prints as it is. Any other name gets a line
// with no command.
func TestCreateKubeconfigHeaderStaysOneCommentLine(t *testing.T) {
	t.Parallel()
	rawKC := "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\ncurrent-context: \"\"\n"
	for _, tc := range hostileKubeconfigNames {
		t.Run(tc.row, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.plain, tc.name != "" && output.Printable(tc.name) == tc.name, "the row's plain flag")

			cmd := NewCreateKubeconfig(stubFactory{c: &fakeClient{name: tc.name, status: map[string]any{"value": rawKC}}})
			_, exec, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
			require.NoError(t, err)
			require.NotNil(t, exec)
			res, err := exec(t.Context())
			require.NoError(t, err)
			require.NotNil(t, res.RawBytes)
			out := string(res.RawBytes)

			before, after, ok := strings.Cut(out, "\n")
			require.True(t, ok, "the output has no line end")
			assert.Equal(t, rawKC, after, "everything after the first line is the document, byte for byte")

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
					command, found = strings.CutPrefix(command, "-- ")
					require.True(t, found, "a name that starts with a dash follows -- in the command, not %q", command)
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

// TestCreateKubeconfigHeaderForARancherName records the first line of the
// output for a name Rancher generates.
func TestCreateKubeconfigHeaderForARancherName(t *testing.T) {
	t.Parallel()
	rawKC := "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\ncurrent-context: \"\"\n"
	cmd := NewCreateKubeconfig(stubFactory{c: &fakeClient{name: "kubeconfig-abc12", status: map[string]any{"value": rawKC}}})
	_, exec, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	want := `# Kubeconfig "kubeconfig-abc12" - shown once; revoke with: rancher delete kubeconfig 'kubeconfig-abc12'` + "\n" + rawKC
	assert.Equal(t, want, string(res.RawBytes))
}

// TestCreateKubeconfigHeaderForANameWithALeadingDash records the first line
// of the output for a name that starts with a dash: the command puts -- before
// the quoted name.
func TestCreateKubeconfigHeaderForANameWithALeadingDash(t *testing.T) {
	t.Parallel()
	rawKC := "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\ncurrent-context: \"\"\n"
	cmd := NewCreateKubeconfig(stubFactory{c: &fakeClient{name: "-h", status: map[string]any{"value": rawKC}}})
	_, exec, err := planKubeconfig(t, cmd, in{s: map[string]string{}})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	want := `# Kubeconfig "-h" - shown once; revoke with: rancher delete kubeconfig -- '-h'` + "\n" + rawKC
	assert.Equal(t, want, string(res.RawBytes))
}
