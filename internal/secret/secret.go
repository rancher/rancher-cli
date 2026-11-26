package secret

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// hidden is what every printing and encoding method of a Value writes.
const hidden = "<hidden>"

// Value is a secret value the CLI read from a flag, a file or stdin. Every
// printing and encoding method writes <hidden>. The text is stored behind a
// pointer to a string: a printer that reads the struct by reflection (fmt on
// an unexported field, fmt's %p and %w) prints an address, never the text. The
// pointee is a string and never an array, slice, struct or map, because fmt
// dereferences a pointer to one of those once on its bad-verb path and prints
// the content. Only Reveal returns the text.
//
// The zero value is the empty secret.
type Value struct {
	// The array of functions makes Value not comparable: == and use as a map
	// key do not compile. It comes first: Go pads a zero-size last field, which
	// would double the size of the struct.
	_    [0]func()
	held *string
}

// New returns a Value with the text s.
func New(s string) Value {
	return Value{held: &s}
}

// FromBytes returns a Value with a copy of b.
func FromBytes(b []byte) Value {
	return New(string(b))
}

// Read calls read with path and returns the bytes it returned as a Value. It
// clears the buffer read returned and returns read's error unchanged; the
// bytes never sit in a variable of the caller.
func Read(read func(path string) ([]byte, error), path string) (Value, error) {
	b, err := read(path)
	if err != nil {
		clear(b)
		return Value{}, err
	}
	v := FromBytes(b)
	clear(b)
	return v, nil
}

// Reveal returns the text of v. Every call is listed, with its reason, in the
// reveal call site table of test/conformance.
func Reveal(v Value) string {
	if v.held == nil {
		return ""
	}
	return *v.held
}

// Len returns the length of the text in bytes.
func (v Value) Len() int {
	if v.held == nil {
		return 0
	}
	return len(*v.held)
}

// IsZero reports whether the text is empty.
func (v Value) IsZero() bool {
	return v.held == nil || *v.held == ""
}

// IsBlank reports whether the text is empty or white space only.
func (v Value) IsBlank() bool {
	return v.held == nil || strings.TrimSpace(*v.held) == ""
}

// Equal reports whether v and o have the same text.
func (v Value) Equal(o Value) bool {
	return Reveal(v) == Reveal(o)
}

// EqualString reports whether the text of v is s, a known public text.
func (v Value) EqualString(s string) bool {
	return Reveal(v) == s
}

// EqualFoldString reports whether the text of v is s, a known public text,
// without regard to case.
func (v Value) EqualFoldString(s string) bool {
	return strings.EqualFold(Reveal(v), s)
}

// Cut slices v around the first instance of sep, as strings.Cut does, and
// keeps both parts as Values. When sep is absent, before is v and after is the
// zero Value.
func (v Value) Cut(sep string) (before, after Value, found bool) {
	b, a, found := strings.Cut(Reveal(v), sep)
	if !found {
		return v, Value{}, false
	}
	return New(b), New(a), true
}

// String returns <hidden>.
func (v Value) String() string {
	return hidden
}

// GoString returns <hidden>.
func (v Value) GoString() string {
	return hidden
}

// Format writes <hidden> for every verb that reaches it, padded to the width
// with the - flag honored and the precision ignored. fmt handles %T, %p and %w
// before it calls Format.
func (v Value) Format(f fmt.State, verb rune) {
	pad := ""
	if w, ok := f.Width(); ok && w > len(hidden) {
		pad = strings.Repeat(" ", w-len(hidden))
	}
	text := pad + hidden
	if f.Flag('-') {
		text = hidden + pad
	}
	if _, err := io.WriteString(f, text); err != nil {
		return
	}
}

// MarshalJSON returns the JSON string "<hidden>".
func (v Value) MarshalJSON() ([]byte, error) {
	return []byte(`"` + hidden + `"`), nil
}

// MarshalText returns <hidden>.
func (v Value) MarshalText() ([]byte, error) {
	return []byte(hidden), nil
}

// MarshalYAML returns <hidden> for gopkg.in/yaml.v3.
func (v Value) MarshalYAML() (any, error) {
	return hidden, nil
}

// MarshalLog returns <hidden> for logr and klog.
func (v Value) MarshalLog() any {
	return hidden
}

// LogValue returns <hidden> for log/slog.
func (v Value) LogValue() slog.Value {
	return slog.StringValue(hidden)
}
