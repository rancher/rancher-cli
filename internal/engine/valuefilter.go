package engine

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/secret"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HiddenText is what every printed form of a hidden value shows, and what
// the server-text filter puts in place of a value.
const HiddenText = "<hidden>"

// MinFilteredLength is the shortest value, in characters, the server-text
// filter replaces: a shorter one would also replace ordinary words.
const MinFilteredLength = 4

// NewTextFilter returns a function that replaces with <hidden> every value
// of values that could be a secret: each value of 4 characters or more, each
// line of 4 characters or more of a multi-line value, and the JSON-escaped
// form of each; with base64 also the standard base64 form of each (the form a
// Kubernetes Secret's data has). Every occurrence of each of them in the
// text as given is found, occurrences that overlap included, and each run of
// text they cover (occurrences that overlap or touch form one run) becomes
// one <hidden>, so the result does not depend on the order of the values. It
// does not cover numbers, booleans, shorter values or a value the server
// transforms. A command that prints server text after it sent secret values
// runs it on every path.
//
// A strings.Replacer that deletes every value can only detect whether one
// occurs: it replaces the match that starts first and skips a value that
// overlaps it, and cannot hide both. When one occurs, each value marks the
// bytes its occurrences cover in one []bool, each byte at most once (only the bytes past
// the end of its previous occurrence): marking every byte of every occurrence
// is too slow on repetitive text, where occurrences overlap many times.
func NewTextFilter(values []string, base64Forms bool) func(string) string {
	needles := filterNeedles(values, base64Forms)
	if len(needles) == 0 {
		return func(text string) string { return text }
	}
	pairs := make([]string, 0, 2*len(needles))
	for _, n := range needles {
		pairs = append(pairs, n, "")
	}
	detect := strings.NewReplacer(pairs...)
	return func(text string) string {
		if len(detect.Replace(text)) == len(text) {
			return text
		}
		covered := make([]bool, len(text))
		for _, n := range needles {
			end := 0
			for from := 0; ; {
				i := strings.Index(text[from:], n)
				if i < 0 {
					break
				}
				start := from + i
				for j := max(start, end); j < start+len(n); j++ {
					covered[j] = true
				}
				end = start + len(n)
				from = start + 1
			}
		}
		var b strings.Builder
		for i := 0; i < len(text); {
			j := i
			for j < len(text) && covered[j] == covered[i] {
				j++
			}
			if covered[i] {
				b.WriteString(HiddenText)
			} else {
				b.WriteString(text[i:j])
			}
			i = j
		}
		return b.String()
	}
}

// filterNeedles lists the texts the filter replaces, longest first.
func filterNeedles(values []string, base64Forms bool) []string {
	var needles []string
	add := func(s string) {
		if utf8.RuneCountInString(s) < MinFilteredLength {
			return
		}
		needles = append(needles, s)
		for _, escaped := range JSONForms(s) {
			if escaped != s {
				needles = append(needles, escaped)
			}
		}
		if base64Forms {
			needles = append(needles, base64.StdEncoding.EncodeToString([]byte(s)))
		}
	}
	for _, s := range values {
		add(s)
		if strings.ContainsAny(s, "\r\n") {
			for line := range strings.FieldsFuncSeq(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
				add(line)
			}
		}
	}
	slices.SortFunc(needles, func(a, b string) int { return len(b) - len(a) })
	return slices.Compact(needles)
}

// NewSecretFilter is NewTextFilter, with base64 forms, over every form of the
// text of each secret value and of each extra text: the text, each of its
// lines (cut at a line feed or a carriage return, empty lines dropped) and, for
// the text and for each line, the text between the quotes of strconv.Quote,
// QuoteToASCII and QuoteToGraphic. A command that sends secret values filters
// every text of the server's response with it. extra lists the plain texts the
// request sent along that the response may echo.
func NewSecretFilter(values []secret.Value, extra ...string) func(string) string {
	texts := slices.Clone(extra)
	for _, v := range values {
		texts = append(texts, secret.Reveal(v))
	}
	var forms []string
	for _, text := range texts {
		forms = append(forms, redactionForms(text)...)
	}
	return NewTextFilter(forms, true)
}

// redactionForms lists the texts a filter hides for one value: the value, each
// of its lines and, for the value and for each line, the text between the
// quotes of strconv.Quote, QuoteToASCII and QuoteToGraphic.
func redactionForms(value string) []string {
	forms := []string{value}
	texts := []string{value}
	for line := range strings.FieldsFuncSeq(value, func(r rune) bool { return r == '\n' || r == '\r' }) {
		forms = append(forms, line)
		texts = append(texts, line)
	}
	for _, text := range texts {
		for _, quoted := range []string{strconv.Quote(text), strconv.QuoteToASCII(text), strconv.QuoteToGraphic(text)} {
			inner := strings.TrimSuffix(strings.TrimPrefix(quoted, `"`), `"`)
			if !slices.Contains(forms, inner) {
				forms = append(forms, inner)
			}
		}
	}
	return forms
}

// JSONForms returns the forms s takes inside a JSON string: with and
// without the escapes of <, > and &.
func JSONForms(s string) []string {
	plain, _ := json.Marshal(s)
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return []string{trimQuotes(string(plain))}
	}
	unescapedHTML := strings.TrimSuffix(b.String(), "\n")
	return []string{trimQuotes(string(plain)), trimQuotes(unescapedHTML)}
}

func trimQuotes(s string) string {
	return strings.TrimSuffix(strings.TrimPrefix(s, `"`), `"`)
}

// FilterError returns err as a clierr.Error with the server text in its
// message and hint passed through filter. Its code and retry advice are kept;
// the cause is dropped, because its text is the server's response as it was
// sent.
func FilterError(err error, filter func(string) string) error {
	ce := clierr.From(err)
	out := clierr.New(ce.Code, filter(ce.Message)).WithRetryable(ce.Retryable)
	if ce.Hint != "" {
		out = out.WithHint(filter(ce.Hint))
	}
	return out
}

// RefusedError is the error for a write the server refused: the response
// classified again by clierr's status table, as the client did, from a body
// whose server text has the values hidden, with the server's text added where
// the table's message does not have it. The table's code, retry advice, hint
// and wording stay; the filter runs on the whole text before the table cuts
// it, so no part of a value is left, and a Kubernetes Status keeps its whole
// message. The hint comes from the request and is never filtered. method and
// path name the request in the message.
func RefusedError(method, path string, r *ActionRefused, filter func(string) string) error {
	body := FilterBody(r.Body, filter)
	said := ServerText(body)
	status, ok := clierr.HTTPStatus(r.Err)
	if !ok {
		// r.Err's message has the server's text as sent and is never read
		// here.
		src := clierr.From(r.Err)
		msg := method + " " + path + " on the Rancher server was refused"
		if said != "" {
			msg += ": " + said
		}
		return clierr.New(src.Code, msg).WithRetryable(src.Retryable)
	}
	prefix := method + " " + path + " on the Rancher server"
	ce := clierr.FromResponse(prefix, method, status, body, r.Request)
	// The server text is looked for after the prefix only, so a short text
	// that the path has, such as the action name, is still shown.
	if said != "" && !strings.EqualFold(said, http.StatusText(status)) && !strings.Contains(strings.TrimPrefix(ce.Message, prefix), strings.TrimSuffix(said, "…")) {
		ce = ce.WithMessage(ce.Message + ": " + said)
	}
	return ce
}

// ServerText is the server's message in a body whose values are already
// hidden. It runs no second filter, which would replace a value that is
// part of <hidden> inside the marker.
func ServerText(filtered []byte) string {
	return clierr.FilteredServerMessage(filtered, func(s string) string { return s })
}

// FilterBody is body with the values hidden in its server text: in a
// Kubernetes Status, its message, details.name and each cause's message and
// field; in any other JSON object, every string; or the whole of any other
// body. A Status's kind, apiVersion, status, reason and code stay as sent,
// so it is still read as that Status.
func FilterBody(body []byte, filter func(string) string) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		return []byte(filter(string(body)))
	}
	var st metav1.Status
	if json.Unmarshal(body, &st) == nil && st.Kind == "Status" {
		filterStatus(obj, filter)
	} else {
		filterStrings(obj, filter)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return []byte(filter(string(body)))
	}
	return out
}

// filterStatus runs filter on the text fields of a Status decoded into obj.
// Keys match without regard to case, as encoding/json reads them.
func filterStatus(obj map[string]any, filter func(string) string) {
	filterField(obj, "message", filter)
	for _, details := range fieldsNamed(obj, "details") {
		d, ok := details.(map[string]any)
		if !ok {
			continue
		}
		filterField(d, "name", filter)
		for _, causes := range fieldsNamed(d, "causes") {
			list, ok := causes.([]any)
			if !ok {
				continue
			}
			for _, cause := range list {
				if c, ok := cause.(map[string]any); ok {
					filterField(c, "message", filter)
					filterField(c, "field", filter)
				}
			}
		}
	}
}

// fieldsNamed returns the values of obj's keys that equal name without regard
// to case.
func fieldsNamed(obj map[string]any, name string) []any {
	var out []any
	for k, v := range obj {
		if strings.EqualFold(k, name) {
			out = append(out, v)
		}
	}
	return out
}

// filterField runs filter on each string under a key of obj that equals name
// without regard to case.
func filterField(obj map[string]any, name string, filter func(string) string) {
	for k, v := range obj {
		if s, ok := v.(string); ok && strings.EqualFold(k, name) {
			obj[k] = filter(s)
		}
	}
}

// filterStrings returns v with filter run on every string in it.
func filterStrings(v any, filter func(string) string) any {
	switch t := v.(type) {
	case string:
		return filter(t)
	case map[string]any:
		for k, e := range t {
			t[k] = filterStrings(e, filter)
		}
	case []any:
		for i, e := range t {
			t[i] = filterStrings(e, filter)
		}
	}
	return v
}
