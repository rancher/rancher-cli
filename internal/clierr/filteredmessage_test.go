package clierr

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// hideSecret replaces the value the FilteredServerMessage tests hide.
func hideSecret(s string) string { return strings.ReplaceAll(s, "secretvalue", "<hidden>") }

// TestFilteredServerMessage asserts the filtered message is decoded as every
// server message is (a JSON error object's message, the first line of a
// body that is not JSON, nothing for other JSON or a blank body), with the
// filter run before the cut: a value the 200-byte cut would split is
// hidden whole.
func TestFilteredServerMessage(t *testing.T) {
	t.Parallel()
	straddle := strings.Repeat("a", maxServerMessage-5) + "secretvalue tail"
	cases := map[string]struct {
		body, want string
	}{
		"object":                 {`{"type":"error","message":" refused secretvalue "}`, "refused <hidden>"},
		"value across the cut":   {`{"message":"` + straddle + `"}`, strings.Repeat("a", maxServerMessage-5) + "<hidd…"},
		"plain text first line":  {"refused secretvalue \nsecond line secretvalue", "refused <hidden>"},
		"blank":                  {"   \n ", ""},
		"blank message":          {`{"message":"   "}`, ""},
		"array":                  {`["secretvalue"]`, ""},
		"object without message": {`{"code":"ServerError"}`, ""},
		"other JSON":             {`"secretvalue"`, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := FilteredServerMessage([]byte(tc.body), hideSecret)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "secret")
		})
	}
}

// TestFilteredServerMessageIsBounded asserts the message is never longer
// than the bound and its ellipsis, whatever the filter returns.
func TestFilteredServerMessageIsBounded(t *testing.T) {
	t.Parallel()
	grow := func(s string) string { return s + strings.Repeat("x", 10*maxServerMessage) }
	for _, body := range []string{`{"message":"m"}`, "plain", strings.Repeat("y", 5*maxServerMessage)} {
		got := FilteredServerMessage([]byte(body), grow)
		assert.LessOrEqual(t, len(got), maxServerMessage+len("…"), body)
	}
}

// TestServerMessageUnchangedByTheFilteredDecode asserts the callers that
// show the message as sent get what they got before: the object's message
// trimmed and cut, the plain line cut, nothing for other JSON.
func TestServerMessageUnchangedByTheFilteredDecode(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("z", 3*maxServerMessage)
	assert.Equal(t, "refused", serverMessage([]byte(`{"message":"  refused  "}`)))
	assert.Equal(t, "plain line ", serverMessage([]byte("plain line \nnext")))
	assert.Equal(t, "", serverMessage([]byte(`[1,2]`)))
	assert.Equal(t, strings.Repeat("z", maxServerMessage)+"…", serverMessage([]byte(long)))
}
