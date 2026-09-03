package runtime

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTextLinesStyledTextBoldsFirstLineOnly asserts TextLines' StyledText
// builder: only the headline is bold, the strip invariant holds, and a
// disabled Style returns Text unchanged.
func TestTextLinesStyledTextBoldsFirstLineOnly(t *testing.T) {
	t.Parallel()
	res := TextLines("server x added", "url: https://x.example.com", "ca: pinned")
	enabled := style.Style{Enabled: true}

	styled := res.StyledText(enabled)
	assert.Equal(t, res.Text, style.Strip(styled))
	assert.Equal(t, res.Text, res.StyledText(style.Style{}), "disabled style returns Text unchanged")

	lines := splitKeepNewline(styled)
	require.Len(t, lines, 3)
	assert.Contains(t, lines[0], "\x1b[1m", "the headline must be bold")
	assert.NotContains(t, lines[1], "\x1b[", "a detail line must not be styled")
	assert.NotContains(t, lines[2], "\x1b[", "a detail line must not be styled")
}

// TestMutationTextStyledTextBoldsFirstLineOnly asserts the same rule through
// MutationText, the "<kind>/<name> <verb>" headline shape every generic
// create/patch/delete command shares.
func TestMutationTextStyledTextBoldsFirstLineOnly(t *testing.T) {
	t.Parallel()
	res := MutationText("thing", "x", "created", "detail: y")
	styled := res.StyledText(style.Style{Enabled: true})
	assert.Equal(t, res.Text, style.Strip(styled))
	lines := splitKeepNewline(styled)
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], "\x1b[1m")
	assert.NotContains(t, lines[1], "\x1b[")
}

// TestAccentLineColorsDetailLineYellowKeepingHeadlineBold asserts the common
// accent shape (config delete-server's "was current — pointer cleared"): the
// accented detail line turns yellow, the headline keeps its own bold, and
// every other line stays plain.
func TestAccentLineColorsDetailLineYellowKeepingHeadlineBold(t *testing.T) {
	t.Parallel()
	res := TextLines("server x deleted", "was current — pointer cleared", "credentials removed (keychain)")
	res = AccentLine(res, "was current — pointer cleared")
	enabled := style.Style{Enabled: true}

	styled := res.StyledText(enabled)
	assert.Equal(t, res.Text, style.Strip(styled))
	assert.Equal(t, res.Text, res.StyledText(style.Style{}))

	lines := splitKeepNewline(styled)
	require.Len(t, lines, 3)
	assert.Contains(t, lines[0], "\x1b[1m", "the headline stays bold")
	assert.Contains(t, lines[1], "\x1b[33m", "the accented line turns yellow")
	assert.NotContains(t, lines[1], "\x1b[1m", "the accented line is not also bold")
	assert.NotContains(t, lines[2], "\x1b[", "an unrelated detail line stays plain")
}

// TestAccentLineOnHeadlineReplacesBoldWithYellow asserts login --no-verify's
// shape: when the accented line IS the headline, yellow replaces bold rather
// than compounding with it — one emphasis, the alarming one.
func TestAccentLineOnHeadlineReplacesBoldWithYellow(t *testing.T) {
	t.Parallel()
	headline := "token stored for prod (https://prod.example.com) — not verified"
	res := TextLines(headline)
	res = AccentLine(res, headline)
	styled := res.StyledText(style.Style{Enabled: true})
	assert.Equal(t, res.Text, style.Strip(styled))
	assert.Contains(t, styled, "\x1b[33m", "the headline turns yellow")
	assert.NotContains(t, styled, "\x1b[1m", "yellow replaces bold, it does not compound with it")
}

// TestRenderResultMutationTextKeepsShownOnceBold extends
// TestRenderResultStyledTextKeepsShownOnceBold to the helper-built case: a
// DeliveryOnceOnly command that builds its Result from MutationText (the
// real shape every once-only create/secret-op command uses) keeps BOTH the
// helper's own headline bold and the shown-once line's bold — the two
// treatments compose, neither one silences the other.
func TestRenderResultMutationTextKeepsShownOnceBold(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "make credential", Shape: ShapeText, Delivery: DeliveryOnceOnly}
	mt := MutationText("credential", "x", "created", "value: v"+ShownOnceSuffix)
	res := Result{
		Kind:           "CredentialCreated",
		Data:           map[string]string{"name": "x"},
		Text:           mt.Text,
		StyledText:     mt.StyledText,
		OnceOnlyNotice: "this value is shown exactly once — store it now",
	}

	var plainOut, styledOut bytes.Buffer
	_, ce := renderResult(IO{Out: &plainOut, Err: new(bytes.Buffer), Format: "table"}, d, res)
	require.Nil(t, ce)
	_, ce = renderResult(IO{Out: &styledOut, Err: new(bytes.Buffer), Format: "table", Color: "always", OutTTY: true}, d, res)
	require.Nil(t, ce)

	assert.Equal(t, plainOut.String(), mt.Text)
	assert.Equal(t, plainOut.String(), style.Strip(styledOut.String()))

	lines := strings.SplitAfter(styledOut.String(), "\n")
	require.Len(t, lines, 3)
	assert.Contains(t, lines[0], "\x1b[1m", "the MutationText headline is bold")
	assert.Contains(t, lines[1], "\x1b[1m", "the shown-once line gains bold through composition")
}
