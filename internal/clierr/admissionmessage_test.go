package clierr

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	vapText        = `ValidatingAdmissionPolicy 'p' with binding 'b' denied request: no helm pods`
	vapTextPrefix  = `ValidatingAdmissionPolicy 'p' with binding 'b' denied request: `
	notReadyText   = `not yet ready to handle request`
	podResource    = "pods"
	podNamePrefix  = "helm-operation-"
	podRefusalHead = `pods "helm-operation-x" is forbidden: `
)

// plantedToken is a secret-looking value of 32 letters and digits from
// crypto/rand, new on every run.
func plantedToken(t *testing.T) string {
	t.Helper()
	return randomToken(t, 32)
}

// randomToken is n letters and digits from crypto/rand.
func randomToken(t *testing.T, n int) string {
	t.Helper()
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// minFilteredLength is the number of characters of a part of a planted token
// that the checks look for; it equals the minimum length the filter hides.
const minFilteredLength = 4

// otherToken is a random token of the length of token that has no part of
// minFilteredLength characters in common with token. The case planted with it
// has the same fixed text as the case planted with token, and none of the parts
// of token.
func otherToken(t *testing.T, token string) string {
	t.Helper()
	for range 10 {
		other := randomToken(t, len(token))
		if _, shared := tokenRunLeft(token, other, ""); !shared {
			return other
		}
	}
	require.FailNow(t, "every token drawn shares a part with the planted token")
	return ""
}

// hideToken is the filter that replaces token with <hidden>.
func hideToken(token string) func(string) string {
	return func(s string) string { return strings.ReplaceAll(s, token, "<hidden>") }
}

// tokenRunLeft returns a run of minFilteredLength characters of token that text
// holds and second does not. second is the output of the same case planted with
// otherToken(token). A run both hold is fixed text of the case, not a part of
// the token.
func tokenRunLeft(token, text, second string) (string, bool) {
	for i := 0; i+minFilteredLength <= len(token); i++ {
		if run := token[i : i+minFilteredLength]; strings.Contains(text, run) && !strings.Contains(second, run) {
			return run, true
		}
	}
	return "", false
}

// assertNoTokenPart asserts text has no run of 4 characters of token that
// second does not hold.
func assertNoTokenPart(t *testing.T, token, text, second, row string) {
	t.Helper()
	run, found := tokenRunLeft(token, text, second)
	assert.False(t, found, "%s: %q, a part of the planted token, is left in %q", row, run, text)
}

// straddling is head, padding and token in that order. The padding puts the
// start of token 16 bytes before bound, which places the cut inside token.
// 3*bound more characters follow, which keeps the text longer than bound once
// token is hidden.
func straddling(head, token string, bound int) string {
	return head + strings.Repeat(".", bound-16-len(head)) + token + strings.Repeat("-", 3*bound)
}

// bodyOf is the JSON error object with message.
func bodyOf(t *testing.T, message string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"message": message})
	require.NoError(t, err)
	return body
}

func keepAsIs(s string) string { return s }

// refusalOf calls AdmissionRefusalOfBody for the pod, with a show that returns
// its argument.
func refusalOf(body []byte, filter func(string) string) (*Error, bool) {
	return AdmissionRefusalOfBody(body, filter, podResource, podNamePrefix, keepAsIs)
}

// TestAdmissionRefusalOfBodyReadsTheMessage asserts a Rancher error object, a
// Kubernetes Status and the first line of a plain-text body each give their
// message, and that a body with no message gives nothing.
func TestAdmissionRefusalOfBodyReadsTheMessage(t *testing.T) {
	t.Parallel()
	message := podRefusalHead + vapText
	status, err := json.Marshal(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "Forbidden", "code": 403, "message": message})
	require.NoError(t, err)
	for name, body := range map[string][]byte{
		"error object":    rancherErrorBody(t, message),
		"Status":          status,
		"plain text":      []byte(message + "\nsecond line"),
		"padded":          bodyOf(t, "  "+message+" \n"),
		"object no extra": bodyOf(t, message),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ce, ok := refusalOf(body, keepAsIs)
			require.True(t, ok)
			assert.Equal(t, CodeValidation, ce.Code)
			assert.Equal(t, message, ce.Message)
		})
	}
	for name, body := range map[string][]byte{
		"no message":           []byte(`{"type":"error","code":"ServerError","status":500}`),
		"empty message":        bodyOf(t, ""),
		"blank body":           []byte("  \n"),
		"nil body":             nil,
		"JSON with no message": []byte(`["x"]`),
		"JSON string":          []byte(`"x"`),
		"a number":             []byte(`7`),
		"object, bad syntax":   []byte(`{"message":`),
	} {
		t.Run("none "+name, func(t *testing.T) {
			t.Parallel()
			ce, ok := refusalOf(body, keepAsIs)
			assert.False(t, ok)
			assert.Nil(t, ce)
		})
	}
}

// TestAdmissionRefusalOfBodyNearMisses asserts a message that is not the
// refusal of a pod whose name begins with the prefix, or whose text is not an
// admission text, gives nothing and never runs show.
func TestAdmissionRefusalOfBodyNearMisses(t *testing.T) {
	t.Parallel()
	for name, message := range map[string]string{
		"another resource, a name with the prefix": `secrets "helm-operation-x" is forbidden: ` + vapText,
		"another resource with a group":            `operations.catalog.cattle.io "helm-operation-x" is forbidden: ` + notReadyText,
		"a name without the prefix":                `pods "other-x" is forbidden: ` + vapText,
		"a name that is only another prefix":       `pods "other-" is forbidden: ` + notReadyText,
		"a name that has the prefix inside":        `pods "x-helm-operation-x" is forbidden: ` + vapText,
		"no name":                                  `pods is forbidden: ` + vapText,
		"a bare beginning":                         `forbidden: ` + vapText,
		"an empty name":                            `pods "" is forbidden: ` + vapText,
		"an unquoted name":                         `pods helm-operation-x is forbidden: ` + vapText,
		"an unterminated quote":                    `pods "helm-operation-x is forbidden: ` + vapText,
		"a wrapped message":                        `failed to create pod: ` + podRefusalHead + vapText,
		"a text that is not an admission text":     podRefusalHead + `User "u" cannot create resource "pods" in API group "" in the namespace "n"`,
		"a webhook denial":                         podRefusalHead + `admission webhook "w" denied the request: no`,
		"no text":                                  `pods "helm-operation-x" is forbidden: `,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, body := range [][]byte{rancherErrorBody(t, message), []byte(message)} {
				ce, ok := AdmissionRefusalOfBody(body, keepAsIs, podResource, podNamePrefix, func(string) string {
					t.Error("show ran for a message that is not the refusal of the pod")
					return ""
				})
				assert.False(t, ok)
				assert.Nil(t, ce)
			}
		})
	}
}

// TestAdmissionRefusalOfBodyShowsTheCutMessage asserts show gets the message
// after the filter, trimmed and cut at maxAdmissionMessage bytes on a
// character boundary with the ellipsis, that the match and the object check
// read the whole message, and that the error has show's result, followed by
// " - retry later" for a temporary refusal.
func TestAdmissionRefusalOfBodyShowsTheCutMessage(t *testing.T) {
	t.Parallel()
	pad := func(head string, n int) string { return head + strings.Repeat("x", n-len(head)) }

	t.Run("the bound is 2048", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 2048, maxAdmissionMessage)
	})
	t.Run("exactly the bound is not cut", func(t *testing.T) {
		t.Parallel()
		message := pad(podRefusalHead+vapTextPrefix, maxAdmissionMessage)
		require.Len(t, message, maxAdmissionMessage)
		ce, ok := refusalOf(bodyOf(t, message), keepAsIs)
		require.True(t, ok)
		assert.Equal(t, message, ce.Message)
	})
	t.Run("one byte past the bound is cut at the bound", func(t *testing.T) {
		t.Parallel()
		message := pad(podRefusalHead+vapTextPrefix, maxAdmissionMessage+1)
		ce, ok := refusalOf(bodyOf(t, message), keepAsIs)
		require.True(t, ok)
		assert.Equal(t, message[:maxAdmissionMessage]+"…", ce.Message)
	})
	t.Run("a cut inside a character backs up to its start", func(t *testing.T) {
		t.Parallel()
		head := pad(podRefusalHead+vapTextPrefix, maxAdmissionMessage-1)
		message := head + "\u00e9" + "tail"
		require.Equal(t, byte(0xc3), message[maxAdmissionMessage-1])
		ce, ok := refusalOf(bodyOf(t, message), keepAsIs)
		require.True(t, ok)
		assert.Equal(t, head+"…", ce.Message)
	})
	t.Run("a marker past the bound still matches", func(t *testing.T) {
		t.Parallel()
		requested := strings.Repeat("a=1,", 1250)
		message := podRefusalHead + "exceeded quota: q1, requested: " + requested + "pods=1, used: pods=10, limited: pods=10"
		require.Greater(t, strings.Index(message, ", limited: "), maxAdmissionMessage)
		ce, ok := refusalOf(bodyOf(t, message), keepAsIs)
		require.True(t, ok)
		assert.Equal(t, CodeValidation, ce.Code)
		assert.Equal(t, message[:maxAdmissionMessage]+"…", ce.Message)
	})
	t.Run("filter sees the whole decoded message", func(t *testing.T) {
		t.Parallel()
		message := podRefusalHead + vapTextPrefix + strings.Repeat("y", 5000)
		var seen []string
		ce, ok := AdmissionRefusalOfBody(bodyOf(t, message), func(s string) string {
			seen = append(seen, s)
			return s
		}, podResource, podNamePrefix, keepAsIs)
		require.True(t, ok)
		assert.Equal(t, message[:maxAdmissionMessage]+"…", ce.Message)
		assert.NotEmpty(t, seen)
		for _, s := range seen {
			assert.Equal(t, message, s)
		}
	})
	t.Run("the error has what show returns", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			text      string
			retryable bool
		}{{vapText, false}, {notReadyText, true}} {
			message := podRefusalHead + tc.text
			ce, ok := AdmissionRefusalOfBody(bodyOf(t, message), keepAsIs, podResource, podNamePrefix, func(filtered string) string {
				assert.Equal(t, message, filtered)
				return "shown: " + filtered
			})
			require.True(t, ok)
			want := "shown: " + message
			if tc.retryable {
				want += " - retry later"
				assert.Equal(t, CodeGeneric, ce.Code)
			} else {
				assert.Equal(t, CodeValidation, ce.Code)
			}
			assert.Equal(t, tc.retryable, ce.Retryable)
			assert.Equal(t, want, ce.Message)
		}
	})
}

// TestAdmissionRefusalOfBodyHidesAPlantedValue asserts a secret-looking value
// that the filter hides leaves no run of 4 of its characters in the shown
// message, whether the message is shorter than the bound or the bound falls
// inside the value, in AdmissionRefusalOfBody and in FilteredServerMessage.
func TestAdmissionRefusalOfBodyHidesAPlantedValue(t *testing.T) {
	t.Parallel()
	t.Run("AdmissionRefusalOfBody, not cut", func(t *testing.T) {
		t.Parallel()
		token := plantedToken(t)
		other := otherToken(t, token)
		message := func(v string) string { return podRefusalHead + vapTextPrefix + "value " + v + " was refused" }
		ce, ok := refusalOf(bodyOf(t, message(token)), hideToken(token))
		require.True(t, ok)
		second, ok := refusalOf(bodyOf(t, message(other)), hideToken(other))
		require.True(t, ok)
		assertNoTokenPart(t, token, ce.Message, second.Message, "not cut")
		assert.Contains(t, ce.Message, "<hidden>")
	})
	t.Run("AdmissionRefusalOfBody, cut inside the value", func(t *testing.T) {
		t.Parallel()
		token := plantedToken(t)
		other := otherToken(t, token)
		ce, ok := refusalOf(bodyOf(t, straddling(podRefusalHead+vapTextPrefix, token, maxAdmissionMessage)), hideToken(token))
		require.True(t, ok)
		second, ok := refusalOf(bodyOf(t, straddling(podRefusalHead+vapTextPrefix, other, maxAdmissionMessage)), hideToken(other))
		require.True(t, ok)
		assertNoTokenPart(t, token, ce.Message, second.Message, "cut")
		assert.Contains(t, ce.Message, "<hidden>")
		assert.True(t, strings.HasSuffix(ce.Message, "…"))
	})
	t.Run("FilteredServerMessage, not cut", func(t *testing.T) {
		t.Parallel()
		token := plantedToken(t)
		other := otherToken(t, token)
		got := FilteredServerMessage(bodyOf(t, "refused "+token+" now"), hideToken(token))
		second := FilteredServerMessage(bodyOf(t, "refused "+other+" now"), hideToken(other))
		assertNoTokenPart(t, token, got, second, "not cut")
		assert.Equal(t, "refused <hidden> now", got)
	})
	t.Run("FilteredServerMessage, cut inside the value", func(t *testing.T) {
		t.Parallel()
		token := plantedToken(t)
		other := otherToken(t, token)
		got := FilteredServerMessage(bodyOf(t, straddling("refused ", token, maxServerMessage)), hideToken(token))
		second := FilteredServerMessage(bodyOf(t, straddling("refused ", other, maxServerMessage)), hideToken(other))
		assertNoTokenPart(t, token, got, second, "cut")
		assert.Contains(t, got, "<hidden>")
		assert.True(t, strings.HasSuffix(got, "…"))
	})
}
