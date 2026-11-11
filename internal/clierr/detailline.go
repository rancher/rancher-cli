package clierr

import "strings"

// DetailLine is one line of an error's default-format details: Indent, then
// each part's Label as written and its Value as the renderer shows a value.
// A payload hands over the raw value; the renderer shows a value that holds a
// control character, an invisible format character or a line or paragraph
// separator as a double-quoted string with escapes, the way IDText does.
type DetailLine struct {
	Indent string // spaces only
	Parts  []DetailPart
}

// DetailPart is the CLI's own text and the value that follows it. CLI text
// after a value is the next part's Label, with an empty Value.
type DetailPart struct {
	Label string // literals, constants and numbers only
	Value string // a string read from the payload, never escaped by the payload
}

// String is the line with every label and value written as given and nothing
// escaped. It serves a caller that filters the text itself and a test that
// compares lines.
func (l DetailLine) String() string {
	var b strings.Builder
	b.WriteString(l.Indent)
	for _, p := range l.Parts {
		b.WriteString(p.Label)
		b.WriteString(p.Value)
	}
	return b.String()
}

// Line is a line with no indent.
func Line(parts ...DetailPart) DetailLine { return DetailLine{Parts: parts} }

// LabelPart is the CLI's own text, with no value after it.
func LabelPart(label string) DetailPart { return DetailPart{Label: label} }

// ValuePart is the CLI's own text and the value that follows it.
func ValuePart(label, value string) DetailPart { return DetailPart{Label: label, Value: value} }

// ListParts is label and then each item as its own value, with ", " between
// them. An empty list gives the label alone.
func ListParts(label string, items []string) []DetailPart {
	if len(items) == 0 {
		return []DetailPart{LabelPart(label)}
	}
	parts := make([]DetailPart, len(items))
	for i, item := range items {
		sep := ", "
		if i == 0 {
			sep = label
		}
		parts[i] = ValuePart(sep, item)
	}
	return parts
}

// TokenRefParts is label, the kind of ref, a slash and the name of ref, the
// kind and the name each its own value.
func TokenRefParts(label string, ref TokenRef) []DetailPart {
	return []DetailPart{ValuePart(label, string(ref.Kind)), ValuePart("/", ref.Name)}
}
