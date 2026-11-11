package clierr

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDetailLineStringJoinsTheIndentLabelsAndValuesAsGiven asserts the plain
// join writes the indent, then each label and value, and escapes nothing.
func TestDetailLineStringJoinsTheIndentLabelsAndValuesAsGiven(t *testing.T) {
	t.Parallel()
	line := DetailLine{Indent: "  ", Parts: []DetailPart{ValuePart("a: ", "x\u200by"), LabelPart(" (b)")}}
	assert.Equal(t, "  a: x\u200by (b)", line.String())
	assert.Equal(t, "", Line().String())
	assert.Equal(t, "label", Line(LabelPart("label")).String())
}

// TestListPartsPutEachItemInItsOwnValue asserts each item is a part of its own
// after the label, with a comma between them, and that an empty list still
// gives the label.
func TestListPartsPutEachItemInItsOwnValue(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []DetailPart{{Label: "x: ", Value: "a"}, {Label: ", ", Value: "b"}, {Label: ", ", Value: "c"}}, ListParts("x: ", []string{"a", "b", "c"}))
	assert.Equal(t, []DetailPart{{Label: "x: "}}, ListParts("x: ", nil))
	assert.Equal(t, "x: a, b", Line(ListParts("x: ", []string{"a", "b"})...).String())
}

// TestTokenRefPartsKeepTheKindAndTheNameApart asserts the kind and the name
// are two values with the slash between them as a label, and that the join is
// the text String gives.
func TestTokenRefPartsKeepTheKindAndTheNameApart(t *testing.T) {
	t.Parallel()
	ref := TokenRef{Kind: TokenKindExt, Name: "token-a"}
	assert.Equal(t, []DetailPart{{Label: "t ", Value: "ext"}, {Label: "/", Value: "token-a"}}, TokenRefParts("t ", ref))
	assert.Equal(t, "t "+ref.String(), Line(TokenRefParts("t ", ref)...).String())
}
