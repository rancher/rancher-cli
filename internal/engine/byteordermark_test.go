package engine

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDropLeadingByteOrderMarks asserts every byte order mark at the start is
// dropped, a mark anywhere else stays, and the input is not changed.
func TestDropLeadingByteOrderMarks(t *testing.T) {
	t.Parallel()
	const mark = "\ufeff"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no mark", "a: 1", "a: 1"},
		{"one mark", mark + "a: 1", "a: 1"},
		{"two marks", mark + mark + "a: 1", "a: 1"},
		{"three marks", mark + mark + mark + "a: 1", "a: 1"},
		{"a mark after the first character stays", "a" + mark + "b", "a" + mark + "b"},
		{"a mark after the dropped ones stays", mark + mark + "a" + mark + "b", "a" + mark + "b"},
		{"a mark inside a value after two stays", mark + mark + `{"k":"` + mark + `v"}`, `{"k":"` + mark + `v"}`},
		{"empty", "", ""},
		{"every byte a mark", mark + mark + mark, ""},
		{"an invalid first byte stops the trim", "\xff" + mark + "a", "\xff" + mark + "a"},
		{"a partial mark stops the trim", "\xef\xbb" + mark + "a", "\xef\xbb" + mark + "a"},
		{"a UTF-16 byte order mark is not dropped", "\xff\xfea", "\xff\xfea"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := []byte(tt.in)
			got := DropLeadingByteOrderMarks(in)
			assert.Equal(t, tt.want, string(got))
			assert.Equal(t, tt.in, string(in), "the input is unchanged")
		})
	}
}

// TestDropLeadingByteOrderMarksNilAndEmpty asserts a nil and an empty input
// give an empty result, and an input of only marks gives a result that reads
// as empty.
func TestDropLeadingByteOrderMarksNilAndEmpty(t *testing.T) {
	t.Parallel()
	assert.Empty(t, DropLeadingByteOrderMarks(nil))
	assert.Empty(t, DropLeadingByteOrderMarks([]byte{}))
	assert.Empty(t, DropLeadingByteOrderMarks([]byte(strings.Repeat("\ufeff", 5))))
}
