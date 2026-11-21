package runtime_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// renderForInvocation writes ce and notices as an invocation that names dir
// writes them to standard error.
func renderForInvocation(t *testing.T, format string, ce *clierr.Error, dir string, notices ...string) string {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, runtime.RenderErrorStyled(&b, format, ce, style.Style{}, dir, notices...))
	return b.String()
}

// envelopeStrings is every string of the json or yaml document b, keys
// included.
func envelopeStrings(t *testing.T, format string, b []byte) []string {
	t.Helper()
	var doc any
	if format == "yaml" {
		require.NoError(t, yaml.Unmarshal(b, &doc))
	} else {
		require.NoError(t, json.Unmarshal(b, &doc))
	}
	return jsonStrings(doc)
}

// certificateError is a TLS_TRUST error as the trust check builds it, wrapped
// by engine.CommittedPartialSuccess, which adds the certificate facts to the
// message. The subject has a hidden character.
func certificateError(hint string) *clierr.Error {
	cause := clierr.New(clierr.CodeTLSTrust, "the server's certificate is not trusted").WithHint(hint).
		WithDetails(tlstrust.Details{Subject: "CN=a\u200bb", Issuer: "CN=issuer", Fingerprint: "ab:cd"})
	return engine.CommittedPartialSuccess(cause, clierr.CommittedContextDetails{Committed: true, ResourceKind: "entry", ResourceName: "p", Recovery: "log in again"})
}

// lineWithPrefix is the first line of text that starts with prefix.
func lineWithPrefix(t *testing.T, text, prefix string) string {
	t.Helper()
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("no line with prefix %q in %q", prefix, text)
	return ""
}

// TestTLSTrustMessageAndHintKeepTheirHiddenCharactersRemoved asserts the
// message and the hint of a TLS_TRUST error, which can have the subject, the
// issuer and the names of a certificate, print with every hidden character
// removed, and that json and yaml carry them as the error has them.
func TestTLSTrustMessageAndHintKeepTheirHiddenCharactersRemoved(t *testing.T) {
	t.Parallel()
	const hint = "trust it with a\u200bb"
	var b bytes.Buffer
	require.NoError(t, runtime.RenderError(&b, "", certificateError(hint)))
	assert.Equal(t, "error (TLS_TRUST): the server's certificate is not trusted; certificate: CN=ab; issuer: CN=issuer; fingerprint: ab:cd",
		lineWithPrefix(t, b.String(), "error (TLS_TRUST):"))
	assert.Equal(t, "hint: trust it with ab", lineWithPrefix(t, b.String(), "hint:"))
	assert.NotContains(t, b.String(), "\u200b")

	for _, format := range []string{"json", "yaml"} {
		var env bytes.Buffer
		require.NoError(t, runtime.RenderError(&env, format, certificateError(hint)))
		strs := envelopeStrings(t, format, env.Bytes())
		assert.Contains(t, strs, "the server's certificate is not trusted; certificate: CN=a\u200bb; issuer: CN=issuer; fingerprint: ab:cd", format)
		assert.Contains(t, strs, hint, format)
	}
}

// TestTLSTrustMessageAndHintForAnInvocationNameTheDirectoryAndHaveHiddenCharactersRemoved
// asserts a remedy command in the message and hint of a TLS_TRUST error names
// the invocation's directory, and that a hidden character before it is removed.
func TestTLSTrustMessageAndHintForAnInvocationNameTheDirectoryAndHaveHiddenCharactersRemoved(t *testing.T) {
	t.Parallel()
	const dir = "/h/o brien"
	ce := clierr.New(clierr.CodeTLSTrust, "x\u200b run "+clierr.Rancher+" auth status").WithHint("y\u202e " + clierr.Rancher + " auth login 'p'")
	got := renderForInvocation(t, "", ce, dir)
	assert.Equal(t, "error (TLS_TRUST): x run rancher --config '/h/o brien' auth status\n  hint: y rancher --config '/h/o brien' auth login 'p'\n", got)

	for _, format := range []string{"json", "yaml"} {
		env := renderForInvocation(t, format, ce, dir)
		strs := envelopeStrings(t, format, []byte(env))
		assert.Contains(t, strs, "x\u200b run rancher --config '/h/o brien' auth status", format)
		assert.Contains(t, strs, "y\u202e rancher --config '/h/o brien' auth login 'p'", format)
	}
}

// TestAnEntryNameWithQuotesPrintsAsItIs asserts a message that has a name
// written as ShellQuote writes it prints unchanged: a name with double quotes,
// and a name already written as an escaped string, which is not escaped
// again.
func TestAnEntryNameWithQuotesPrintsAsItIs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, message, want string
	}{
		{"a name with double quotes", "no entry " + clierr.ShellQuote(`"ops"`), "error (GENERIC): no entry '\"ops\"'\n"},
		{"a name written as an escaped string", "no entry " + clierr.ShellQuote(clierr.IDText("o\u200bwner")), "error (GENERIC): no entry '\"o\\u200bwner\"'\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var b bytes.Buffer
			require.NoError(t, runtime.RenderError(&b, "", clierr.New(clierr.CodeGeneric, tc.message)))
			assert.Equal(t, tc.want, b.String())
		})
	}
}

// TestOrdinaryErrorTextPrintsTheSameForAnInvocation asserts a message, hint,
// warning and details value with no hidden character prints as written, with
// its commands naming the invocation's directory.
func TestOrdinaryErrorTextPrintsTheSameForAnInvocation(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeNotLoggedIn, "not logged in; run "+clierr.Rancher+" auth login 'prod'").
		WithHint(clierr.Rancher + " auth login 'prod'").
		WithWarnings("see " + clierr.Rancher + " config status").
		WithDetails(clierr.CommittedContextDetails{Committed: true, ResourceKind: "token", ResourceName: "t", Recovery: "log in again with: " + clierr.Rancher + " auth login 'prod'"})
	got := renderForInvocation(t, "", ce, "/h/o brien", "note: "+clierr.Rancher+" auth status")
	assert.Equal(t, `# warning: note: rancher --config '/h/o brien' auth status
# warning: see rancher --config '/h/o brien' config status
error (NOT_LOGGED_IN): not logged in; run rancher --config '/h/o brien' auth login 'prod'
  hint: rancher --config '/h/o brien' auth login 'prod'
  committed: token t - the change may already be on the server
  recover: log in again with: rancher --config '/h/o brien' auth login 'prod'
`, got)
}

// valueDetails is a details payload with one line: a label and one value.
type valueDetails struct{ value string }

func (d valueDetails) TextDetails() []clierr.DetailLine {
	return []clierr.DetailLine{clierr.Line(clierr.ValuePart("value: ", d.value))}
}

// TestADetailsValueThatIsAnEscapedStringPrintsDifferentlyFromTheValueItEscapes
// asserts a details value that is exactly the quoted form of a text with a
// hidden character is quoted again. It prints differently from the value
// that has the character.
func TestADetailsValueThatIsAnEscapedStringPrintsDifferentlyFromTheValueItEscapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, value, want string
	}{
		{"the quoted form", "\"a\\u200bb\"", "error (GENERIC): failed\n  value: \"\\\"a\\\\u200bb\\\"\"\n"},
		{"the value with the character", "a\u200bb", "error (GENERIC): failed\n  value: \"a\\u200bb\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var b bytes.Buffer
			require.NoError(t, runtime.RenderError(&b, "", clierr.New(clierr.CodeGeneric, "failed").WithDetails(valueDetails{value: tc.value})))
			assert.Equal(t, tc.want, b.String())
		})
	}
}

// failingWriter fails every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("scripted write failure") }

// TestANoticeWithAHiddenCharacterBeforeACommandKeepsTheCommandWhole asserts a
// notice recorded before a result whose output could not be written is
// written with the error as a warning, the words before the command escaped
// and the command whole.
func TestANoticeWithAHiddenCharacterBeforeACommandKeepsTheCommandWhole(t *testing.T) {
	t.Parallel()
	var errBuf bytes.Buffer
	streams := runtime.IO{Out: failingWriter{}, Err: &errBuf, RemedyDir: "/h/o brien",
		Notices: &siteNotices{[]string{"x\u200b run " + clierr.Rancher + " auth status"}}}
	_, ce := runtime.Run(t.Context(), siteRead{res: runtime.Result{Text: "ok\n"}}, siteInput{}, streams, safety.Binding{})
	require.NotNil(t, ce)
	assert.True(t, strings.HasPrefix(errBuf.String(), "# warning: \"x\\u200b run \"rancher --config '/h/o brien' auth status\n"), "%q", errBuf.String())
}
