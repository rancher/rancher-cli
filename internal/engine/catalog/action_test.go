package catalog

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const refusedPath = "/v1/catalog.cattle.io.clusterrepos/rancher-charts?action=install"

// refusedPrefix is the text the action client puts in front of a refusal.
const refusedPrefix = "POST " + refusedPath + " on the Rancher server"

// refusal is the answer the action client hands over for a refused POST.
func refusal(status int, body string) *engine.ActionRefused {
	request := &clierr.Request{Server: "https://rancher.test"}
	return &engine.ActionRefused{
		Err:     clierr.FromResponse(refusedPrefix, http.MethodPost, status, []byte(body), request),
		Body:    []byte(body),
		Request: request,
	}
}

// expectedRefusal is the status table's error for body with the value
// replaced by <hidden>, and the server's text in it, without the mark of a
// cut.
func expectedRefusal(status int, hiddenBody string) (*clierr.Error, string) {
	ce := clierr.FromResponse(refusedPrefix, http.MethodPost, status, []byte(hiddenBody), &clierr.Request{Server: "https://rancher.test"})
	said := strings.TrimSuffix(clierr.FilteredServerMessage([]byte(hiddenBody), func(s string) string { return s }), "…")
	return ce, said
}

// chainText is the text of err and of every error it wraps.
func chainText(err error) string {
	var b strings.Builder
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		b.WriteString(e.Error())
		b.WriteString("\n")
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		case interface{ Unwrap() []error }:
			for _, w := range u.Unwrap() {
				walk(w)
			}
		}
	}
	walk(err)
	return b.String()
}

// valuePart returns a run of minFilteredLength characters of value that
// text holds, if any.
func valuePart(value, text string) (string, bool) {
	for i := 0; i+minFilteredLength <= len(value); i++ {
		if part := value[i : i+minFilteredLength]; strings.Contains(text, part) {
			return part, true
		}
	}
	return "", false
}

// assertNoValuePart asserts the message, the hint and the whole chain text
// of err hold no run of minFilteredLength characters of value.
func assertNoValuePart(t *testing.T, value string, err error, row string) {
	t.Helper()
	ce := clierr.From(err)
	for what, text := range map[string]string{"message": ce.Message, "hint": ce.Hint, "chain": chainText(err)} {
		part, found := valuePart(value, text)
		assert.False(t, found, "%s: the %s holds %q of the value: %s", row, what, part, text)
	}
}

// randomToken is 32 letters and digits from crypto/rand.
func randomToken() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// errorObjectBody is a Rancher error object whose message holds value from
// byte 180, so the 200-byte cut of server text splits it.
func errorObjectBody(value string) string {
	message := "the chart cannot be installed: " + strings.Repeat("x", 149) + value + " and the rest of the reason"
	b, _ := json.Marshal(map[string]string{"type": "error", "code": "X", "message": message})
	return string(b)
}

// TestRefusedActionKeepsTheTableAndAddsTheServerText asserts, for every
// status from 300 to 599 the action client hands over as a refusal (all but
// 500, 502 and 504), that a refused action keeps the status table's code,
// retry advice, hint and message for the answer with the value hidden,
// holds Rancher's text with the value hidden exactly once (where it is not
// the status text), and shows no run of 4 characters of the value in its
// message, hint or whole error chain text, though the value crosses the
// 200-byte cut of server text.
func TestRefusedActionKeepsTheTableAndAddsTheServerText(t *testing.T) {
	t.Parallel()
	for status := 300; status <= 599; status++ {
		if status == http.StatusInternalServerError || status == http.StatusBadGateway || status == http.StatusGatewayTimeout {
			continue
		}
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			var token, body string
			var want *clierr.Error
			var said string
			for draw := 0; ; draw++ {
				require.Less(t, draw, 10, "every token drawn shares 4 characters with the expected text")
				token = randomToken()
				body = errorObjectBody(token)
				want, said = expectedRefusal(status, strings.ReplaceAll(body, token, hiddenText))
				if _, found := valuePart(token, want.Message+"\n"+want.Hint+"\n"+chainText(want)+"\n"+said); !found {
					break
				}
			}
			err := refused(refusedPath, refusal(status, body), Values{tree: map[string]any{"password": token}})
			ce := clierr.From(err)
			assert.Equal(t, want.Code, ce.Code)
			assert.Equal(t, want.Retryable, ce.Retryable)
			assert.Equal(t, want.Hint, ce.Hint)
			assert.True(t, strings.HasPrefix(ce.Message, want.Message), "the table's message %q starts %q", want.Message, ce.Message)
			if said != "" && !strings.EqualFold(said, http.StatusText(status)) {
				assert.Equal(t, 1, strings.Count(ce.Message, said), "the server text %q once in %q", said, ce.Message)
			}
			assertNoValuePart(t, token, err, strconv.Itoa(status))
		})
	}
}

// TestRefusedActionMessages asserts the whole message of a refusal: the
// status table's wording, then Rancher's text with the value hidden, cut
// at 200 bytes; and for a 401 whose text is the status text, the table's
// message alone.
func TestRefusedActionMessages(t *testing.T) {
	t.Parallel()
	body := errorObjectBody(canary)
	said := "the chart cannot be installed: " + strings.Repeat("x", 149) + "<hidden> and the res…"
	for _, tc := range []struct {
		status int
		code   string
		body   string
		want   string
	}{
		{http.StatusBadRequest, clierr.CodeValidation, body, refusedPrefix + ": the server rejected our request for an unknown reason: " + said},
		{http.StatusNotFound, clierr.CodeNotFound, body, refusedPrefix + ": the server could not find the requested resource: " + said},
		{http.StatusConflict, clierr.CodeConflict, body, refusedPrefix + ": the server reported a conflict: " + said},
		{http.StatusUnprocessableEntity, clierr.CodeValidation, body, refusedPrefix + ": the server rejected our request due to an error in our request: " + said},
		{http.StatusUnauthorized, clierr.CodeAuth, `{"type":"error","code":"Unauthorized","message":"Unauthorized"}`, refusedPrefix + ": the server rejected the credential (401)"},
	} {
		ce := clierr.From(refused(refusedPath, refusal(tc.status, tc.body), canaryValues(t)))
		assert.Equal(t, tc.code, ce.Code, tc.status)
		assert.Equal(t, tc.want, ce.Message, tc.status)
	}
}

// TestRefusedActionHidesValuesInBodiesThatAreNotAnObject asserts a refusal
// whose body is plain text (its first line holding the value across the
// 200-byte cut), invalid JSON or a JSON array keeps the table's code and
// shows no part of the value.
func TestRefusedActionHidesValuesInBodiesThatAreNotAnObject(t *testing.T) {
	t.Parallel()
	line := "request denied by the gateway: " + strings.Repeat("x", 149) + canary + " and the rest of the line"
	for name, tc := range map[string]struct {
		status int
		code   string
		body   string
	}{
		"plain text 403": {http.StatusForbidden, clierr.CodeRBAC, line + "\n<html>" + canary + "</html>\n"},
		"plain text 503": {http.StatusServiceUnavailable, clierr.CodeGeneric, line + "\nsecond line " + canary + "\n"},
		"invalid JSON":   {http.StatusBadRequest, clierr.CodeValidation, `{"type":"error","message":"` + line + `"`},
		"JSON array":     {http.StatusBadRequest, clierr.CodeValidation, `[{"message":"` + line + `"},"` + canary + `"]`},
	} {
		err := refused(refusedPath, refusal(tc.status, tc.body), canaryValues(t))
		assert.Equal(t, tc.code, clierr.From(err).Code, name)
		assertNoValuePart(t, canary, err, name)
	}
}

// statusBody is a Kubernetes Status body with keys as named.
func statusBody(kindKey, messageKey, message string, details map[string]any) string {
	st := map[string]any{
		kindKey: "Status", "apiVersion": "v1", "status": "Failure",
		"reason": "Invalid", "code": http.StatusUnprocessableEntity, messageKey: message,
	}
	if details != nil {
		st["details"] = details
	}
	b, _ := json.Marshal(st)
	return string(b)
}

// TestRefusedActionHidesValuesInStatusDetails asserts the Kubernetes Status
// in a refusal's error chain holds no part of a value in its details.name
// or in its causes' message and field.
func TestRefusedActionHidesValuesInStatusDetails(t *testing.T) {
	t.Parallel()
	body := statusBody("kind", "message", "admission webhook denied the request", map[string]any{
		"name": "release-" + canary,
		"causes": []any{map[string]any{
			"reason": "FieldValueInvalid", "message": "invalid " + canary, "field": "spec.chart." + canary,
		}},
	})
	err := refused(refusedPath, refusal(http.StatusUnprocessableEntity, body), canaryValues(t))
	se, ok := errors.AsType[*apierrors.StatusError](err)
	require.True(t, ok, "a StatusError in the chain of %v", err)
	require.NotNil(t, se.ErrStatus.Details)
	texts := []string{se.ErrStatus.Details.Name}
	require.Len(t, se.ErrStatus.Details.Causes, 1)
	for _, c := range se.ErrStatus.Details.Causes {
		texts = append(texts, c.Message, c.Field)
	}
	for _, text := range texts {
		part, found := valuePart(canary, text)
		assert.False(t, found, "%q holds %q of the value", text, part)
	}
	assert.Equal(t, "release-<hidden>", se.ErrStatus.Details.Name)
}

// TestRefusedActionKeepsTheStatusFields asserts a Kubernetes Status is
// still read as that Status when values equal its kind and reason, with
// its keys in any case: the code is VALIDATION, the reason stays Invalid,
// and its whole 300-byte message is shown once, with the value hidden.
func TestRefusedActionKeepsTheStatusFields(t *testing.T) {
	t.Parallel()
	message := "admission webhook denied the request: " + strings.Repeat("b", 180) + " field " + canary + " " + strings.Repeat("c", 70)
	hidden := strings.ReplaceAll(message, canary, hiddenText)
	require.Greater(t, len(hidden), 300)
	values := Values{tree: map[string]any{"kind": "Status", "reason": "Invalid", "password": canary}}
	for name, body := range map[string]string{
		"lower-case keys": statusBody("kind", "message", message, nil),
		"other case keys": statusBody("KIND", "Message", message, nil),
	} {
		err := refused(refusedPath, refusal(http.StatusUnprocessableEntity, body), values)
		ce := clierr.From(err)
		assert.Equal(t, clierr.CodeValidation, ce.Code, name)
		se, ok := errors.AsType[*apierrors.StatusError](err)
		require.True(t, ok, name)
		assert.Equal(t, "Invalid", string(se.ErrStatus.Reason), name)
		assert.Equal(t, 1, strings.Count(ce.Message, hidden), "%s: %s", name, ce.Message)
		assert.Equal(t, 1, strings.Count(ce.Message, hidden[:150]), "%s: %s", name, ce.Message)
		assertNoValuePart(t, canary, err, name)
	}
}

// TestRefusedActionWithNoStatus asserts a refusal whose error carries no
// server status keeps that error's code and retry advice, has no cause and
// no hint, and words its message from the body with the values hidden,
// never from the error's text.
func TestRefusedActionWithNoStatus(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"body with a value": {`{"type":"error","message":"denied ` + canary + `"}`, refusedPrefix + " was refused: denied <hidden>"},
		"empty body":        {"", refusedPrefix + " was refused"},
	} {
		r := &engine.ActionRefused{
			Err:  clierr.Retryable(clierr.CodeConflict, "server text "+canary).WithHint("try " + canary),
			Body: []byte(tc.body),
		}
		err := refused(refusedPath, r, canaryValues(t))
		ce := clierr.From(err)
		assert.Equal(t, clierr.CodeConflict, ce.Code, name)
		assert.True(t, ce.Retryable, name)
		assert.Empty(t, ce.Hint, name)
		assert.Nil(t, errors.Unwrap(ce), name)
		assert.Equal(t, tc.want, ce.Message, name)
	}
}

// largeRefusal returns a refusal for n values, the values, and the size of
// its body in bytes. The body is a JSON object with an array of 75*n strings,
// n of which each hold one of the values.
func largeRefusal(t *testing.T, n int) (*engine.ActionRefused, Values, int) {
	t.Helper()
	tree := map[string]any{}
	var values []string
	for i := range n {
		v := fmt.Sprintf("value-%04d <&> secret", i)
		if i%4 == 0 {
			v = fmt.Sprintf("first-%04d <a>\nsecond-%04d &b", i, i)
		}
		tree["k"+strconv.Itoa(i)] = v
		values = append(values, v)
	}
	items := make([]string, 75*n)
	for i := range items {
		items[i] = strconv.Itoa(i % 100)
		if i < len(values) {
			items[i] = "has " + values[i] + " in it"
		}
	}
	b, err := json.Marshal(map[string]any{"type": "error", "message": "denied", "items": items})
	require.NoError(t, err)
	return refusal(http.StatusBadRequest, string(b)), Values{tree: tree}, len(b)
}

// TestRefusedActionFilterTimeScalesWithTheBodySize asserts the time to classify
// a refusal does not grow as fast as the square of the size of its body. The
// body is a JSON object of about 750 KB, an array of 150,000 strings of which
// 2,000 each hold one of 2,000 values. The check compares the shortest of three
// runs on that body with the shortest of seven runs on a body one eighth its
// size, with items and values both scaled; the three full-size runs alternate
// with the first three small runs. The work is mainly the total length of the
// strings plus, for every string that holds a value, a search of that string
// for each value, so it grows between 8 and 64 times for an eight times larger
// body; the limit is 32 times. CI runs the test under the race detector, where
// the ratio of correct code is furthest from the limit. A time in seconds
// depends on the machine; the ratio of two runs on the same machine changes
// little with its speed. Each full-size run must also take under 60 seconds;
// this only reports a slow run. A hang ends with the test timeout of go test.
// The test does not run in parallel, so other tests do not share its time.
func TestRefusedActionFilterTimeScalesWithTheBodySize(t *testing.T) {
	const (
		values    = 2000
		scale     = 8
		maxRatio  = 32
		timeLimit = 60 * time.Second
	)
	smallRefusal, smallValues, _ := largeRefusal(t, values/scale)
	fullRefusal, fullValues, size := largeRefusal(t, values)
	assert.InDelta(t, 750_000, size, 150_000)
	run := func(r *engine.ActionRefused, v Values, n int) time.Duration {
		start := time.Now()
		got := refused(refusedPath, r, v)
		elapsed := time.Since(start)
		require.Less(t, elapsed, timeLimit, "a run on a body of %d values took too long", n)
		assert.Equal(t, clierr.CodeValidation, clierr.From(got).Code)
		return elapsed
	}
	small, full := time.Duration(math.MaxInt64), time.Duration(math.MaxInt64)
	for i := range 7 {
		small = min(small, run(smallRefusal, smallValues, values/scale))
		if i < 3 {
			full = min(full, run(fullRefusal, fullValues, values))
		}
	}
	t.Logf("shortest run: %v on the full body, %v on one eighth of it; ratio %.1f", full, small, float64(full)/float64(small))
	assert.Less(t, full, maxRatio*small)
}

// TestRefusedActionAddsServerTextThePathHolds asserts Rancher's text is
// added to a refusal when the path in front of the status table's text
// holds it, and that a text whose words the table's text already holds adds
// nothing.
func TestRefusedActionAddsServerTextThePathHolds(t *testing.T) {
	t.Parallel()
	const table = refusedPrefix + ": the server rejected our request for an unknown reason"
	for name, tc := range map[string]struct {
		said string
		want string
	}{
		"the action name":     {"install", table + ": install"},
		"a word of the table": {"server", table},
		"another table word":  {"request", table},
	} {
		body := `{"type":"error","code":"X","message":"` + tc.said + `"}`
		ce := clierr.From(refused(refusedPath, refusal(http.StatusBadRequest, body), Values{}))
		assert.Equal(t, tc.want, ce.Message, name)
		assert.Equal(t, 1, strings.Count(strings.TrimPrefix(ce.Message, refusedPrefix), tc.said), name)
	}
}

// overlapRow is a set of values built from one random token, a text that
// holds them and the text the filter must give.
type overlapRow struct {
	values []string
	text   string
	want   string
	// plain is true for a row whose text holds the JSON-escaped form of the
	// values: it is sent as a plain-text body, since a JSON object body is
	// decoded before the filter runs and never holds that form.
	plain bool
}

// overlapRows are the rows of TestFilterHidesOverlappingValuesWhole for the
// token tok.
func overlapRows(tok string) map[string]overlapRow {
	p := tok[0:4]
	u := tok[0:14] + `"` + tok[14:17] + `\` + tok[17:32]
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(u)
	return map[string]overlapRow{
		"overlap by 10 bytes":            {values: []string{tok[0:20], tok[10:32]}, text: "x " + tok + " y", want: "x <hidden> y"},
		"equal length, overlap by 11":    {values: []string{tok[0:16], tok[5:21]}, text: "x " + tok[0:21] + " y", want: "x <hidden> y"},
		"values that touch":              {values: []string{tok[0:16], tok[16:32]}, text: "x " + tok + " y", want: "x <hidden> y"},
		"one value inside another":       {values: []string{tok[0:32], tok[10:20]}, text: "x " + tok + " y", want: "x <hidden> y"},
		"a value that overlaps itself":   {values: []string{p + p}, text: "x " + p + p + p + " y", want: "x <hidden> y"},
		"a value that is part of marker": {values: []string{tok[0:12], "idde"}, text: "a " + tok[0:12] + " b", want: "a <hidden> b"},
		"escaped quote and backslash":    {values: []string{u[0:24], u[10:34]}, text: "x " + escaped + " y", want: "x <hidden> y", plain: true},
	}
}

// valueForms are the texts of values the filter hides: each value as it
// is and its JSON-escaped forms.
func valueForms(values []string) []string {
	var forms []string
	for _, v := range values {
		forms = append(forms, v)
		forms = append(forms, jsonForms(v)...)
	}
	return forms
}

// formPart returns a run of minFilteredLength bytes of a form that text
// holds and that is not part of <hidden>, if any.
func formPart(forms []string, text string) (string, bool) {
	for _, f := range forms {
		for i := 0; i+minFilteredLength <= len(f); i++ {
			part := f[i : i+minFilteredLength]
			if !strings.Contains(hiddenText, part) && strings.Contains(text, part) {
				return part, true
			}
		}
	}
	return "", false
}

// overlapBody is the answer body of row r for a refusal: a Rancher error
// object whose message is text, or text itself for a plain row.
func overlapBody(r overlapRow, text string) string {
	if r.plain {
		return text
	}
	b, _ := json.Marshal(map[string]string{"type": "error", "code": "X", "message": text})
	return string(b)
}

// TestFilterHidesOverlappingValuesWhole asserts, for values that overlap,
// touch, hold one another, overlap themselves, are part of <hidden>, or
// overlap in their JSON-escaped form, that Values.Filter gives the row's
// exact text with one <hidden> for each run the values cover, and that a
// refusal on a 400 and a 403 holds that text once in its message and no run
// of 4 characters of any form of any value in its message, hint or whole
// error chain text.
func TestFilterHidesOverlappingValuesWhole(t *testing.T) {
	t.Parallel()
	statuses := []int{http.StatusBadRequest, http.StatusForbidden}
	for name := range overlapRows(randomToken()) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var row overlapRow
			for draw := 0; ; draw++ {
				require.Less(t, draw, 10, "every token drawn shares 4 characters with the expected text")
				tok := randomToken()
				row = overlapRows(tok)[name]
				constant := []string{row.want}
				for _, status := range statuses {
					want, said := expectedRefusal(status, overlapBody(row, row.want))
					constant = append(constant, want.Message, want.Hint, chainText(want), said)
				}
				text := strings.Join(constant, "\n")
				_, tokHit := valuePart(tok, text)
				_, formHit := formPart(valueForms(row.values), text)
				if !tokHit && !formHit {
					break
				}
			}
			tree := map[string]any{}
			for i, v := range row.values {
				tree[strconv.Itoa(i)] = v
			}
			values := Values{tree: tree}
			assert.Equal(t, row.want, values.Filter(row.text))
			forms := valueForms(row.values)
			for _, status := range statuses {
				err := refused(refusedPath, refusal(status, overlapBody(row, row.text)), values)
				ce := clierr.From(err)
				assert.Equal(t, 1, strings.Count(ce.Message, row.want), "%d: %s", status, ce.Message)
				for what, text := range map[string]string{"message": ce.Message, "hint": ce.Hint, "chain": chainText(err)} {
					part, found := formPart(forms, text)
					assert.False(t, found, "%d: the %s holds %q of a value: %s", status, what, part, text)
				}
			}
		})
	}
}

// timedRefusal is a refusal, the values to hide in its body, and the size of
// its body in bytes.
type timedRefusal struct {
	refusal *engine.ActionRefused
	values  Values
	size    int
}

const (
	// sizeScale is how many times smaller the small body of a scaling check
	// is than the full body.
	sizeScale = 16
	// maxTimeRatio is the largest ratio of the time on the full body to the
	// time on the small body that a scaling check accepts. Work that grows
	// with the size of the body takes sizeScale times as long on the full
	// body, and work that grows as fast as the square of the size takes
	// sizeScale squared times as long.
	maxTimeRatio = 48
	// refusalTimeLimit is the longest one run of a scaling check may take.
	refusalTimeLimit = 60 * time.Second
	// shortestRunLimit is the longest the shortest run on the full body may
	// take. The ratio does not detect work that is many times slower at every
	// size, such as marking every byte of every occurrence of a value in a
	// body of one repeated letter. The test fails on such work only once it
	// makes the shortest run on the full body longer than this limit.
	shortestRunLimit = 10 * time.Second
)

// assertRefusalTimeScales asserts the time to classify full is less than
// maxTimeRatio times the time to classify small, and returns the error of the
// last run on full. It compares the shortest of five runs on full with the
// shortest of nine runs on small; the five runs on full alternate with the
// first five runs on small. A time in seconds depends on the machine; the
// ratio of the times of two runs on the same machine changes little with its
// speed. Each run must also take less than refusalTimeLimit, and the shortest
// run on full less than shortestRunLimit; these two limits fail a slow run
// whatever the ratio. A hang ends with the test timeout of go test.
func assertRefusalTimeScales(t *testing.T, small, full timedRefusal) error {
	t.Helper()
	var last error
	run := func(c timedRefusal) (time.Duration, error) {
		start := time.Now()
		got := refused(refusedPath, c.refusal, c.values)
		elapsed := time.Since(start)
		require.Less(t, elapsed, refusalTimeLimit, "a run on a body of %d bytes took too long", c.size)
		assert.Equal(t, clierr.CodeValidation, clierr.From(got).Code)
		return elapsed, got
	}
	shortSmall, shortFull := time.Duration(math.MaxInt64), time.Duration(math.MaxInt64)
	for i := range 9 {
		elapsed, _ := run(small)
		shortSmall = min(shortSmall, elapsed)
		if i < 5 {
			elapsed, last = run(full)
			shortFull = min(shortFull, elapsed)
		}
	}
	t.Logf("shortest run: %v on a body of %d bytes, %v on one of %d bytes; ratio %.1f",
		shortFull, full.size, shortSmall, small.size, float64(shortFull)/float64(shortSmall))
	assert.Less(t, shortFull, maxTimeRatio*shortSmall)
	assert.Less(t, shortFull, shortestRunLimit, "the shortest run on a body of %d bytes took too long", full.size)
	return last
}

// TestRefusedActionPlainBodyFilterTimeScalesWithTheBodySize asserts the time
// to classify a refusal does not grow as fast as the square of the size of its
// body. The body is 1 MiB of random letters and digits, with 2,000 random
// values of 32 characters of which one occurs once in the middle; the check
// compares it with a body one sixteenth its size and the same values (see
// assertRefusalTimeScales). The work mostly grows with the length of the
// body, plus a search of the whole body for each value once one occurs. The
// time grows about 12 to 19 times for a body 16 times larger; the limit is 48
// times. It does not run in parallel; other tests do not share its time.
func TestRefusedActionPlainBodyFilterTimeScalesWithTheBodySize(t *testing.T) {
	const fullSize = 1 << 20
	tree := map[string]any{}
	for i := range 2000 {
		tree["k"+strconv.Itoa(i)] = randomToken()
	}
	body := func(size int) timedRefusal {
		var b strings.Builder
		for b.Len() < size {
			b.WriteString(randomToken())
		}
		text := b.String()[:size]
		middle := size / 2
		text = text[:middle] + tree["k0"].(string) + text[middle+32:]
		return timedRefusal{refusal(http.StatusBadRequest, text), Values{tree: tree}, size}
	}
	_ = assertRefusalTimeScales(t, body(fullSize/sizeScale), body(fullSize))
}

// TestRefusedActionRepeatedOverlappingValuesFilterTimeScalesWithTheBodySize
// asserts the time to classify a refusal does not grow as fast as the square
// of the size of its body, and that the result shows one <hidden> and no run
// of the letter. The body is 256 KiB of one letter, with 200 values of that
// letter from 4 to 203 characters long, each occurring at every offset; the
// check compares it with a body one sixteenth its size and the same values
// (see assertRefusalTimeScales). The work mostly is, for each value, a search
// of the body that finds the value at every offset. The time grows about 15 to
// 18 times for a body 16 times larger; the limit is 48 times. It does not run
// in parallel; other tests do not share its time.
func TestRefusedActionRepeatedOverlappingValuesFilterTimeScalesWithTheBodySize(t *testing.T) {
	const fullSize = 256 << 10
	tree := map[string]any{}
	for n := 4; n <= 203; n++ {
		tree["k"+strconv.Itoa(n)] = strings.Repeat("a", n)
	}
	body := func(size int) timedRefusal {
		return timedRefusal{refusal(http.StatusBadRequest, strings.Repeat("a", size)), Values{tree: tree}, size}
	}
	got := assertRefusalTimeScales(t, body(fullSize/sizeScale), body(fullSize))
	ce := clierr.From(got)
	assert.Equal(t, 1, strings.Count(ce.Message, hiddenText), ce.Message)
	assertNoValuePart(t, "aaaa", got, "repeated letter")
}
