package cloudcredfields

import (
	"bytes"
	"strings"
)

// Problem names what is wrong with a value Rancher would store changed or
// refuse, so the caller can word the refusal with the field and the flag. The
// problems never carry a byte of the value.
type Problem int

const (
	// ProblemNone means the value is fine as given.
	ProblemNone Problem = iota
	// ProblemEncoding means the raw bytes start with a UTF-16 byte order mark or
	// hold a NUL byte: the file is UTF-16, which Rancher stores as garbage.
	ProblemEncoding
	// ProblemLineBreak means a single-line value holds a line break other than
	// one final one.
	ProblemLineBreak
	// ProblemEdgeSpace means the value starts or ends with white space that
	// Rancher removes: strings.TrimSpace would change it.
	ProblemEdgeSpace
)

// RawEncodingProblem reports ProblemEncoding when raw, the bytes read from a
// file or stdin, starts with the UTF-16 marks FF FE or FE FF or holds a NUL
// byte. It runs on the raw bytes, before the UTF-8 byte order marks are
// dropped: afterwards a UTF-16 file no longer looks like one.
func RawEncodingProblem(raw []byte) Problem {
	if bytes.HasPrefix(raw, []byte{0xFF, 0xFE}) || bytes.HasPrefix(raw, []byte{0xFE, 0xFF}) || bytes.IndexByte(raw, 0) >= 0 {
		return ProblemEncoding
	}
	return ProblemNone
}

// Clean applies the white space rules to value, as Rancher stores it, and
// returns what the request sends, the problem when there is one, and whether a
// multi-line value was trimmed. fromFile says the value was read from a file or
// stdin, where one final line ending is not part of the value; a value from an
// argument holds none.
//
// A single-line field loses exactly one final \r\n or \n (a file value only),
// then refuses any other \r or \n and any white space strings.TrimSpace would
// still remove at either end, because Rancher stores the trimmed value. A
// multi-line field keeps its interior line endings and is trimmed at both ends.
func Clean(f Field, value string, fromFile bool) (cleaned string, problem Problem, trimmed bool) {
	if f.Multiline {
		cleaned = strings.TrimSpace(value)
		return cleaned, ProblemNone, cleaned != value
	}
	if fromFile {
		if v, ok := strings.CutSuffix(value, "\r\n"); ok {
			value = v
		} else {
			value = strings.TrimSuffix(value, "\n")
		}
	}
	switch {
	case strings.ContainsAny(value, "\r\n"):
		return "", ProblemLineBreak, false
	case strings.TrimSpace(value) != value:
		return "", ProblemEdgeSpace, false
	}
	return value, ProblemNone, false
}
