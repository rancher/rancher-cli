package cloudcredfields

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCloudCredentialValueWhiteSpace asserts the white space rules: a single
// line field loses one final line ending of a file value and is refused with
// any other line break or with white space left at an end (ASCII, U+00A0 and
// U+0085 included), a multi-line field keeps its interior CRLF and is trimmed
// at both ends, and a value from an argument has no line ending to drop.
func TestCloudCredentialValueWhiteSpace(t *testing.T) {
	t.Parallel()
	single := Field{Name: "token"}
	multi := Field{Name: "kubeconfigContent", Multiline: true}
	for name, tc := range map[string]struct {
		field    Field
		value    string
		fromFile bool
		want     string
		problem  Problem
		trimmed  bool
	}{
		"plain":                      {single, "abc", true, "abc", ProblemNone, false},
		"final newline of a file":    {single, "abc\n", true, "abc", ProblemNone, false},
		"final CRLF of a file":       {single, "abc\r\n", true, "abc", ProblemNone, false},
		"newline in an argument":     {single, "abc\n", false, "", ProblemLineBreak, false},
		"second line":                {single, "abc\ndef\n", true, "", ProblemLineBreak, false},
		"carriage return inside":     {single, "ab\rc", true, "", ProblemLineBreak, false},
		"two final newlines":         {single, "abc\n\n", true, "", ProblemLineBreak, false},
		"leading space":              {single, " abc", true, "", ProblemEdgeSpace, false},
		"trailing space":             {single, "abc ", true, "", ProblemEdgeSpace, false},
		"trailing space and newline": {single, "abc \n", true, "", ProblemEdgeSpace, false},
		"trailing tab":               {single, "abc\t", false, "", ProblemEdgeSpace, false},
		"leading no-break space":     {single, "\u00a0abc", false, "", ProblemEdgeSpace, false},
		"trailing no-break space":    {single, "abc\u00a0", true, "", ProblemEdgeSpace, false},
		"trailing next line":         {single, "abc\u0085", false, "", ProblemEdgeSpace, false},
		"multi-line keeps CRLF":      {multi, "a: 1\r\nb: 2\r\n", true, "a: 1\r\nb: 2", ProblemNone, true},
		"multi-line trimmed":         {multi, "\n  a: 1\n\n", true, "a: 1", ProblemNone, true},
		"multi-line untouched":       {multi, "a: 1\nb: 2", false, "a: 1\nb: 2", ProblemNone, false},
	} {
		got, problem, trimmed := Clean(tc.field, tc.value, tc.fromFile)
		assert.Equal(t, tc.problem, problem, name)
		assert.Equal(t, tc.want, got, name)
		assert.Equal(t, tc.trimmed, trimmed, name)
	}
}

// TestRawEncodingProblem asserts a UTF-16 mark and a NUL byte are refused on
// the raw bytes and a UTF-8 mark is not: the UTF-16 check runs before the
// UTF-8 marks are dropped.
func TestRawEncodingProblem(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		raw  []byte
		want Problem
	}{
		"utf-16 little endian":        {[]byte{0xFF, 0xFE, 'a', 0}, ProblemEncoding},
		"utf-16 big endian":           {[]byte{0xFE, 0xFF, 0, 'a'}, ProblemEncoding},
		"nul byte":                    {[]byte("ab\x00c"), ProblemEncoding},
		"utf-16 mark then utf-8 mark": {[]byte{0xFF, 0xFE, 0xEF, 0xBB, 0xBF, 'a'}, ProblemEncoding},
		"utf-8 mark":                  {[]byte{0xEF, 0xBB, 0xBF, 'a'}, ProblemNone},
		"plain":                       {[]byte("abc\n"), ProblemNone},
		"empty":                       {nil, ProblemNone},
	} {
		assert.Equal(t, tc.want, RawEncodingProblem(tc.raw), name)
	}
}

// TestMultilineFields asserts the multi-line list.
func TestMultilineFields(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"authEncodedJson", "kubeconfigContent", "privateKeyContents", "sshKeyContents", "defaultEndpointCA"} {
		assert.True(t, IsMultiline(name), name)
	}
	for _, name := range []string{"accessKey", "token", "password"} {
		assert.False(t, IsMultiline(name), name)
	}
}
