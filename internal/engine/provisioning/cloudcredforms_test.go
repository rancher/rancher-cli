package provisioning

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/cloudcredfields"
	"github.com/rancher/rancher-cli/internal/secret"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedactionFilterHidesEveryQuotedForm asserts that the filter the command
// builds from the secret values it sent replaces every form of a value in a
// response of the server with <hidden>: the value, each of its lines, the text
// between the quotes of strconv.Quote, QuoteToASCII and QuoteToGraphic of the
// value and of each line, and the JSON-escaped and base64 forms of each of
// those. Each form, written inside a sentence, comes out as the sentence with
// the form replaced and nothing else changed. The values have a no-break space,
// a BEL, a tab, a double quote, a backslash, a non-ASCII letter and, in the
// last, several lines.
func TestRedactionFilterHidesEveryQuotedForm(t *testing.T) {
	values := map[string]string{
		"a no-break space":           "fake-nbsp\u00a0value-0001",
		"a no-break space and a BEL": "fake-both\u00a0and\abel-0011",
		"a BEL":                      "fake-bel\avalue-0002",
		"a tab":                      "fake-tab\tvalue-0003",
		"a double quote":             `fake-"quoted"-value-0004`,
		"a backslash":                `fake-back\slash-value-0005`,
		"a non-ASCII letter":         "fake-caf\u00e9-value-0006",
		"several lines":              "fake-line\u00a0one-0007\nfake-line\atwo-0008\r\nfake-line\tthree-0009\n",
	}
	for valueName, value := range values {
		t.Run(valueName, func(t *testing.T) {
			filter := responseFilter([]credentialValue{{field: cloudcredfields.Field{Secret: true}, value: secret.New(value)}})
			texts := []string{value}
			for line := range strings.FieldsFuncSeq(value, func(r rune) bool { return r == '\n' || r == '\r' }) {
				texts = append(texts, line)
			}
			forms := map[string]string{}
			for _, text := range texts {
				forms["the text "+strconv.Quote(text)] = text
				for quoteName, quoted := range map[string]string{"Quote": strconv.Quote(text), "QuoteToASCII": strconv.QuoteToASCII(text), "QuoteToGraphic": strconv.QuoteToGraphic(text)} {
					forms[quoteName+" of "+strconv.Quote(text)] = strings.Trim(quoted, `"`)
				}
			}
			for name, form := range maps.Clone(forms) {
				escaped, err := json.Marshal(form)
				require.NoError(t, err)
				forms["JSON of "+name] = strings.Trim(string(escaped), `"`)
				forms["base64 of "+name] = base64.StdEncoding.EncodeToString([]byte(form))
			}
			for name, form := range forms {
				assert.Equal(t, "the value "+engine.HiddenText+" was refused", filter("the value "+form+" was refused"), name)
			}
		})
	}
}
