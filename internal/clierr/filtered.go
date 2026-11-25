package clierr

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Filtered is a text built from the results of the builder filters
// (output.FilterValue, runtime.FilterRemedies) and the CLI's own words.
// It cannot be sliced, indexed, measured with len, ranged over, compared
// with <, joined with + or converted to a string or a []rune: a cut of a
// filtered value is a compile error, and the only ways to change a
// Filtered are its methods, none of which cuts at a length. The text comes
// back as a string through Marked, for the Text or WideText of a
// runtime.Result or an argument of runtime.Prompter.Notice, and through
// WithoutMark, for an output that is not the printed text (a json
// projection, the input of a StyledText builder). A Filtered handed to fmt,
// encoding/json or an encoder that uses encoding.TextMarshaler prints or
// returns a refusal, never the text: write it with Sprintf or
// Builder.Printf.
//
// A value a filter changed carries RemovalMark after it, and every method
// that returns a Filtered keeps it: the renderer of a result text or a notice
// finds the mark and says the output had characters a terminal cannot show.
// Marked gives the text with the mark, WithoutMark without it. A Filtered
// cannot be compared with == or used as a map key: the mark would make two
// values that print the same compare as different.
type Filtered struct {
	// The blank field makes Filtered not comparable and takes no space.
	_ [0]func()
	s string
}

// NewFiltered returns the result of a filter: filtered is the text the
// filter wrote, and changed reports whether the filter removed or replaced
// anything; a changed text is followed by RemovalMark. Only
// output.FilterValue and runtime.FilterRemedies call it.
func NewFiltered(filtered string, changed bool) Filtered {
	if changed {
		filtered += RemovalMark
	}
	return Filtered{s: filtered}
}

// Plain is CLI text, or a text read back from a runtime.Result's Text, as a
// Filtered. It filters nothing and keeps any RemovalMark in s.
func Plain(s string) Filtered { return Filtered{s: s} }

// Append is f followed by each part, every mark kept.
func (f Filtered) Append(parts ...Filtered) Filtered {
	var b strings.Builder
	b.WriteString(f.s)
	for _, p := range parts {
		b.WriteString(p.s)
	}
	return Filtered{s: b.String()}
}

// AppendString is f followed by the CLI text s, the mark of f kept.
func (f Filtered) AppendString(s string) Filtered { return Filtered{s: f.s + s} }

// JoinFiltered is strings.Join over Filtered elements, every mark kept.
func JoinFiltered(elems []Filtered, sep string) Filtered {
	parts := make([]string, len(elems))
	for i, e := range elems {
		parts[i] = e.s
	}
	return Filtered{s: strings.Join(parts, sep)}
}

// IsEmpty reports whether f has no text. A text that is only RemovalMark is
// not empty: a filter removed every character of the value.
func (f Filtered) IsEmpty() bool { return f.s == "" }

// Width is the number of runes in f without RemovalMark: the runes a
// terminal prints.
func (f Filtered) Width() int { return utf8.RuneCountInString(WithoutRemovalMark(f.s)) }

// TrimSuffix is f without one trailing suffix. It trims the text without its
// marks and writes one RemovalMark at the end when f had one: a mark is
// never cut, and never hides the suffix.
func (f Filtered) TrimSuffix(suffix string) Filtered {
	t := strings.TrimSuffix(WithoutRemovalMark(f.s), suffix)
	if HasRemovalMark(f.s) {
		t += RemovalMark
	}
	return Filtered{s: t}
}

// TrimRight is f without the trailing runes that are in cutset. It trims the
// text without its marks and writes one RemovalMark at the end when f had
// one: a mark is never cut, and never hides the runes before it.
func (f Filtered) TrimRight(cutset string) Filtered {
	t := strings.TrimRight(WithoutRemovalMark(f.s), cutset)
	if HasRemovalMark(f.s) {
		t += RemovalMark
	}
	return Filtered{s: t}
}

// Marked is the text for a sink, RemovalMark included: the Text or WideText
// of a runtime.Result, or an argument of runtime.Prompter.Notice. A test in
// test/conformance allows a call outside internal/clierr and
// internal/runtime only in those places.
func (f Filtered) Marked() string { return f.s }

// WithoutMark is the text without RemovalMark, for an output that is not the
// printed text: the json projection of config status, the input of a
// StyledText builder. A test in test/conformance allows a call only in the
// functions it lists.
func (f Filtered) WithoutMark() string { return WithoutRemovalMark(f.s) }

// Format refuses every fmt verb: the text fmt printed would be a string,
// which code can cut. Write a Filtered with Sprintf or Builder.Printf.
func (Filtered) Format(st fmt.State, verb rune) {
	_, _ = fmt.Fprintf(st, "%%!%c(clierr.Filtered)", verb)
}

// errFilteredEncoding is the error of an encoder handed a Filtered.
var errFilteredEncoding = errors.New("clierr.Filtered cannot be encoded: encode the string that WithoutMark returns")

// MarshalJSON refuses: without it a Filtered in json would be {} with no
// error.
func (Filtered) MarshalJSON() ([]byte, error) { return nil, errFilteredEncoding }

// MarshalText refuses, for the encoders that use encoding.TextMarshaler.
func (Filtered) MarshalText() ([]byte, error) { return nil, errFilteredEncoding }

// whole writes a Filtered argument of Sprintf as it is, its mark included,
// for %s and %v: a width pads it with spaces to Width runes (on the right
// with the - flag), a precision is ignored and the text is never cut. Any
// other verb writes the refusal.
type whole struct{ f Filtered }

func (w whole) Format(st fmt.State, verb rune) {
	if verb != 's' && verb != 'v' {
		_, _ = fmt.Fprintf(st, "%%!%c(clierr.Filtered)", verb)
		return
	}
	pad := ""
	if wd, ok := st.Width(); ok && wd > w.f.Width() {
		pad = strings.Repeat(" ", wd-w.f.Width())
	}
	if st.Flag('-') {
		_, _ = st.Write([]byte(w.f.s + pad))
		return
	}
	_, _ = st.Write([]byte(pad + w.f.s))
}

// Sprintf is fmt.Sprintf with the Filtered arguments written whole (see
// whole). It replaces each Filtered element of args with a whole, in place,
// and passes format and args to fmt.Sprintf; go vet's printf check then
// treats Sprintf, and Builder.Printf through it, as printf wrappers. A
// slice spread into args has no Filtered: a test in test/conformance
// refuses a Filtered in a []any literal.
func Sprintf(format string, args ...any) Filtered {
	for i, a := range args {
		if f, ok := a.(Filtered); ok {
			args[i] = whole{f}
		}
	}
	return Filtered{s: fmt.Sprintf(format, args...)}
}

// Builder is a strings.Builder for a Filtered text. It keeps every mark
// written into it. It is not an io.Writer, and it has no String or Len:
// fmt.Fprintf cannot write into it, and its text comes back only as a
// Filtered.
type Builder struct{ b strings.Builder }

// WriteString writes the CLI text s.
func (b *Builder) WriteString(s string) { b.b.WriteString(s) }

// Write writes f whole.
func (b *Builder) Write(f Filtered) { b.b.WriteString(f.s) }

// Printf writes Sprintf(format, args...).
func (b *Builder) Printf(format string, args ...any) {
	b.b.WriteString(Sprintf(format, args...).s)
}

// Filtered is the text written so far.
func (b *Builder) Filtered() Filtered { return Filtered{s: b.b.String()} }

// TextOf is Text for a text that starts with a Filtered: first is written
// whole as the first prose part, and parts are written as Text writes them.
// parts accept only what Text accepts (string, Lead, Command, Sequence). A
// Filtered among parts panics: no Filtered reaches the ...any of Text. The
// mark of first is kept: Text writes a prose part whole.
func TextOf(first Filtered, parts ...any) Filtered {
	for _, p := range parts {
		if _, ok := p.(Filtered); ok {
			panic("clierr.TextOf: pass a Filtered as first, not in parts")
		}
	}
	return Filtered{s: textAfter(first.s, parts)}
}
