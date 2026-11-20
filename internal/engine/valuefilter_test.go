package engine

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const filterSecret = "s3cr3t-value-0001"

func standardBase64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// TestTextFilterHidesEveryFormOfAValue asserts the filter replaces a value,
// each line of a multi-line value, and the JSON-escaped forms of both, and
// leaves a value shorter than 4 characters and unrelated text.
func TestTextFilterHidesEveryFormOfAValue(t *testing.T) {
	t.Parallel()
	filter := NewTextFilter([]string{filterSecret, "abc", "line-one-x\nline-two-y", `say "hi" now`, "<tag>&more"}, false)
	text := strings.Join([]string{
		filterSecret, "abc", "line-one-x", "line-two-y",
		`say \"hi\" now`, `<tag>&more`, "<tag>&more", "plain",
	}, " | ")
	assert.Equal(t, strings.Join([]string{
		HiddenText, "abc", HiddenText, HiddenText, HiddenText, HiddenText, HiddenText, "plain",
	}, " | "), filter(text))
	assert.Equal(t, "nothing", NewTextFilter(nil, true)("nothing"))
}

// TestTextFilterBase64Option asserts the standard base64 form of a value and of
// each line of a multi-line value is hidden only when the option is on.
func TestTextFilterBase64Option(t *testing.T) {
	t.Parallel()
	multi := "first-line-x\r\nsecond-line-y"
	values := []string{filterSecret, multi}
	forms := []string{
		standardBase64(filterSecret), standardBase64(multi),
		standardBase64("first-line-x"), standardBase64("second-line-y"),
	}
	on := NewTextFilter(values, true)
	off := NewTextFilter(values, false)
	for _, form := range forms {
		text := "data " + form + " end"
		assert.Equal(t, "data "+HiddenText+" end", on(text), form)
		assert.Equal(t, text, off(text), form)
	}
	assert.Equal(t, "short abc", NewTextFilter([]string{"abc"}, true)("short abc"))
	assert.Equal(t, "short "+standardBase64("abc"), NewTextFilter([]string{"abc"}, true)("short "+standardBase64("abc")))
}

// TestTextFilterBase64RunsJoin asserts touching and overlapping occurrences of
// a value, its base64 form and a line of it become one <hidden>.
func TestTextFilterBase64RunsJoin(t *testing.T) {
	t.Parallel()
	value := "alpha-beta-gamma"
	encoded := standardBase64(value)
	filter := NewTextFilter([]string{value}, true)
	assert.Equal(t, "x "+HiddenText+" y", filter("x "+value+encoded+" y"))
	assert.Equal(t, "x "+HiddenText+" y", filter("x "+encoded+value+" y"))
	assert.Equal(t, "x "+HiddenText+" "+HiddenText+" y", filter("x "+value+" "+encoded+" y"))
	overlap := NewTextFilter([]string{value, "gamma-delta-eps"}, true)
	assert.Equal(t, "x "+HiddenText+" y", overlap("x alpha-beta-gamma-delta-eps y"))
}

// TestTextFilterOverlappingValues asserts overlapping values are replaced with
// one <hidden> for the text both cover, found in the text as given.
func TestTextFilterOverlappingValues(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		values []string
		text   string
		want   string
	}{
		"shorter value starts first": {[]string{"bcdefgh", "abcd"}, "xx abcdefgh yy", "xx <hidden> yy"},
		"value equal to <hidden>":    {[]string{"<hidden>", "den>xyz"}, "a <hidden>xyz b", "a <hidden> b"},
	} {
		assert.Equal(t, tc.want, NewTextFilter(tc.values, false)(tc.text), name)
		assert.Equal(t, tc.want, NewTextFilter(tc.values, true)(tc.text), name)
	}
}

// TestFilterErrorKeepsCodeAndAdviceAndHidesValues asserts the filtered error
// keeps its code and retry advice and hides values in its message and hint.
func TestFilterErrorKeepsCodeAndAdviceAndHidesValues(t *testing.T) {
	t.Parallel()
	filter := NewTextFilter([]string{filterSecret}, false)
	err := FilterError(clierr.New(clierr.CodeConflict, "server said "+filterSecret).WithHint("try "+filterSecret).WithRetryable(true), filter)
	ce := clierr.From(err)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Equal(t, "server said <hidden>", ce.Message)
	assert.Equal(t, "try <hidden>", ce.Hint)
}

// TestFilterBodyShapes asserts a Kubernetes Status hides its message,
// details.name and each cause's message and field and keeps the rest, any other
// JSON object hides every string, and any other body is filtered whole.
func TestFilterBodyShapes(t *testing.T) {
	t.Parallel()
	filter := NewTextFilter([]string{filterSecret}, false)
	status := `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Invalid","code":422,` +
		`"message":"bad ` + filterSecret + `","details":{"name":"` + filterSecret + `","causes":[{"message":"m ` + filterSecret + `","field":"f.` + filterSecret + `"}]}}`
	var got map[string]any
	require.NoError(t, json.Unmarshal(FilterBody([]byte(status), filter), &got))
	assert.Equal(t, "Status", got["kind"])
	assert.Equal(t, "Invalid", got["reason"])
	assert.Equal(t, "bad "+HiddenText, got["message"])
	details := got["details"].(map[string]any)
	assert.Equal(t, HiddenText, details["name"])
	cause := details["causes"].([]any)[0].(map[string]any)
	assert.Equal(t, "m "+HiddenText, cause["message"])
	assert.Equal(t, "f."+HiddenText, cause["field"])

	var other map[string]any
	require.NoError(t, json.Unmarshal(FilterBody([]byte(`{"type":"error","message":"`+filterSecret+`","list":["`+filterSecret+`"]}`), filter), &other))
	assert.Equal(t, HiddenText, other["message"])
	assert.Equal(t, []any{HiddenText}, other["list"])

	assert.Equal(t, "plain "+HiddenText, string(FilterBody([]byte("plain "+filterSecret), filter)))
}

// TestRefusedErrorHidesValuesAndKeepsTheStatusClass asserts a refused answer is
// classified again from the filtered body: the status table's code is kept and
// the value is absent from the message.
func TestRefusedErrorHidesValuesAndKeepsTheStatusClass(t *testing.T) {
	t.Parallel()
	filter := NewTextFilter([]string{filterSecret}, false)
	body := []byte(`{"type":"error","code":"MissingRequired","message":"field ` + filterSecret + ` is missing"}`)
	refused := &ActionRefused{
		Err:  clierr.FromResponse("POST /v3/x on the Rancher server", http.MethodPost, http.StatusUnprocessableEntity, body, nil),
		Body: body,
	}
	ce := clierr.From(RefusedError(http.MethodPost, "/v3/x", refused, filter))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.NotContains(t, ce.Message, filterSecret)
	assert.Contains(t, ce.Message, HiddenText)
	assert.Contains(t, ce.Message, "POST /v3/x on the Rancher server")

	noStatus := clierr.From(RefusedError(http.MethodPut, "/v3/x", &ActionRefused{Err: clierr.New(clierr.CodeValidation, "text "+filterSecret), Body: body}, filter))
	assert.Equal(t, "PUT /v3/x on the Rancher server was refused: field "+HiddenText+" is missing", noStatus.Message)
}

// TestDropLeadingByteOrderMarks asserts every mark at the start is dropped and
// a mark anywhere else stays.
func TestDropLeadingByteOrderMarks(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ in, want string }{
		"none":         {"value", "value"},
		"one":          {"\ufeffvalue", "value"},
		"two":          {"\ufeff\ufeffvalue", "value"},
		"after a byte": {"v\ufeffalue", "v\ufeffalue"},
		"only marks":   {"\ufeff\ufeff", ""},
		"empty":        {"", ""},
	} {
		assert.Equal(t, tc.want, DropLeadingByteOrderMarks(tc.in), name)
	}
}
