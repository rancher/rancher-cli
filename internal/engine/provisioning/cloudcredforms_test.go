package provisioning

import (
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/cloudcredfields"
	"github.com/stretchr/testify/assert"
)

// TestRedactionFiltersHideEveryQuotedForm asserts that each filter the command
// builds, the one that hides a read value in a refusal and the one that hides a
// secret in an answer of the server, replaces the text between the quotes of
// strconv.Quote, QuoteToASCII and QuoteToGraphic of a value, and of each of its
// lines, with <hidden>. The values hold a no-break space, a BEL, a tab, a
// double quote, a backslash, a non-ASCII letter and, in the last, several
// lines. Both filters take their forms from redactionForms, so a form missing
// from it fails both rows of this test.
func TestRedactionFiltersHideEveryQuotedForm(t *testing.T) {
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
	filters := map[string]func(value string) func(string) string{
		"the refusal filter": func(value string) func(string) string {
			var read readTexts
			read.record(value)
			return read.filter()
		},
		"the answer filter": func(value string) func(string) string {
			return answerFilter([]credentialValue{{field: cloudcredfields.Field{Secret: true}, value: value}})
		},
	}
	for filterName, build := range filters {
		for valueName, value := range values {
			t.Run(filterName+" "+valueName, func(t *testing.T) {
				filter := build(value)
				texts := []string{value}
				for line := range strings.FieldsFuncSeq(value, func(r rune) bool { return r == '\n' || r == '\r' }) {
					texts = append(texts, line)
				}
				for _, text := range texts {
					for quoteName, quoted := range map[string]string{"Quote": strconv.Quote(text), "QuoteToASCII": strconv.QuoteToASCII(text), "QuoteToGraphic": strconv.QuoteToGraphic(text)} {
						assert.Equal(t, `"`+engine.HiddenText+`"`, filter(quoted), quoteName+" of "+strconv.Quote(text))
					}
				}
			})
		}
	}
}
