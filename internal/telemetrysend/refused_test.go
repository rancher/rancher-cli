package telemetrysend

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRefusedCount asserts both results for each form of answer: a count
// given as a decimal string or as an integer number, an object that gives no
// count (a full success), and a body that gives no usable number.
func TestRefusedCount(t *testing.T) {
	digits30 := strings.Repeat("9", 30)
	rows := []struct {
		name   string
		answer string
		count  int
		known  bool
	}{
		{"string count", `{"partialSuccess":{"rejectedLogRecords":"3","errorMessage":"x"}}`, 3, true},
		{"number count", `{"partialSuccess":{"rejectedLogRecords":3,"errorMessage":"x"}}`, 3, true},
		{"number with a zero fraction", `{"partialSuccess":{"rejectedLogRecords":3.0,"errorMessage":"x"}}`, 3, true},
		{"number with an exponent", `{"partialSuccess":{"rejectedLogRecords":1e2,"errorMessage":"x"}}`, 100, true},
		{"zero count with a warning", `{"partialSuccess":{"rejectedLogRecords":"0","errorMessage":"x"}}`, 0, true},
		{"zero number", `{"partialSuccess":{"rejectedLogRecords":0}}`, 0, true},
		{"empty object", `{}`, 0, true},
		{"object with surrounding space", " \n{} \n", 0, true},
		{"null partial success", `{"partialSuccess":null}`, 0, true},
		{"empty partial success", `{"partialSuccess":{}}`, 0, true},
		{"null count", `{"partialSuccess":{"rejectedLogRecords":null}}`, 0, true},
		{"warning only", `{"partialSuccess":{"errorMessage":"x"}}`, 0, true},
		{"error message is not a count", `{"partialSuccess":{"errorMessage":"7"}}`, 0, true},
		{"empty body", ``, 0, false},
		{"spaces only", "  \n ", 0, false},
		{"negative string", `{"partialSuccess":{"rejectedLogRecords":"-1"}}`, 0, false},
		{"negative number", `{"partialSuccess":{"rejectedLogRecords":-1}}`, 0, false},
		{"fractional string", `{"partialSuccess":{"rejectedLogRecords":"1.5"}}`, 0, false},
		{"fractional number", `{"partialSuccess":{"rejectedLogRecords":1.5}}`, 0, false},
		{"text count", `{"partialSuccess":{"rejectedLogRecords":"abc"}}`, 0, false},
		{"empty string count", `{"partialSuccess":{"rejectedLogRecords":""}}`, 0, false},
		{"padded string count", `{"partialSuccess":{"rejectedLogRecords":" 3 "}}`, 0, false},
		{"signed string count", `{"partialSuccess":{"rejectedLogRecords":"+3"}}`, 0, false},
		{"string count too large", `{"partialSuccess":{"rejectedLogRecords":"` + digits30 + `"}}`, 0, false},
		{"number count too large", `{"partialSuccess":{"rejectedLogRecords":` + digits30 + `}}`, 0, false},
		{"largest int as a number", `{"partialSuccess":{"rejectedLogRecords":` + strconv.Itoa(math.MaxInt) + `}}`, math.MaxInt, true},
		{"largest int as a string", `{"partialSuccess":{"rejectedLogRecords":"` + strconv.Itoa(math.MaxInt) + `"}}`, math.MaxInt, true},
		{"one above the largest int as a number", `{"partialSuccess":{"rejectedLogRecords":9223372036854775808}}`, 0, false},
		{"one above the largest int as a string", `{"partialSuccess":{"rejectedLogRecords":"9223372036854775808"}}`, 0, false},
		{"exponent too large", `{"partialSuccess":{"rejectedLogRecords":1e400}}`, 0, false},
		{"boolean count", `{"partialSuccess":{"rejectedLogRecords":true}}`, 0, false},
		{"object count", `{"partialSuccess":{"rejectedLogRecords":{}}}`, 0, false},
		{"partial success is text", `{"partialSuccess":"x"}`, 0, false},
		{"partial success is a list", `{"partialSuccess":[]}`, 0, false},
		{"array", `[]`, 0, false},
		{"text", `"text"`, 0, false},
		{"null", `null`, 0, false},
		{"not JSON", `not json`, 0, false},
		{"object followed by text", `{} x`, 0, false},
		{"unfinished object", `{"partialSuccess":`, 0, false},
	}
	enc := NewOTLPEncoder()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			count, known := enc.RefusedCount([]byte(row.answer))
			assert.Equal(t, row.count, count)
			assert.Equal(t, row.known, known)
		})
	}
}
