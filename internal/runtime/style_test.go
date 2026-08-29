package runtime

import (
	"bytes"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStylesForEligibility pins the eligibility rule ahead of activation:
// only ""/"table"/"wide" ever produce an Enabled Style, regardless of
// --color, TTY-ness, or the environment — json, yaml, name, and template
// always get the disabled zero value.
func TestStylesForEligibility(t *testing.T) {
	for _, format := range []string{"", "table", "wide"} {
		t.Run("eligible format="+format, func(t *testing.T) {
			out, err := stylesFor(IO{Format: format, Color: "always", OutTTY: false, ErrTTY: false})
			assert.True(t, out.Enabled)
			assert.True(t, err.Enabled)
		})
	}
	for _, format := range []string{"json", "yaml", "name", "template={{.}}"} {
		t.Run("ineligible format="+format, func(t *testing.T) {
			out, err := stylesFor(IO{Format: format, Color: "always", OutTTY: true, ErrTTY: true})
			assert.False(t, out.Enabled)
			assert.False(t, err.Enabled)
		})
	}
}

// TestBoldShownOnceLine pins boldShownOnceLine's scope: only the line ending
// in ShownOnceSuffix is wrapped; every other line, and all whitespace, is
// untouched.
func TestBoldShownOnceLine(t *testing.T) {
	t.Parallel()
	enabled := style.Style{Enabled: true}

	text := "token/t-abc created\n  bearer token: tok:xxxx" + ShownOnceSuffix + "\n  expires: 2026-08-01T00:00:00Z\n"
	got := boldShownOnceLine(text, enabled)
	assert.Equal(t, text, style.Strip(got), "strip must restore the exact plain text")
	lines := splitKeepNewline(got)
	require.Len(t, lines, 3)
	assert.NotContains(t, lines[0], "\x1b[", "headline must not be styled")
	assert.Contains(t, lines[1], "\x1b[1m", "the shown-once line must be bold")
	assert.Contains(t, lines[1], "\x1b[0m")
	assert.NotContains(t, lines[2], "\x1b[", "a later unrelated line must not be styled")

	t.Run("no match returns text unchanged", func(t *testing.T) {
		t.Parallel()
		plain := "token/t-abc created\n  no shown-once line here\n"
		assert.Equal(t, plain, boldShownOnceLine(plain, enabled))
	})

	t.Run("disabled style returns text unchanged", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, text, boldShownOnceLine(text, style.Style{}))
	})
}

func splitKeepNewline(s string) []string {
	var lines []string
	start := 0
	for i := range s {
		if s[i] == '\n' {
			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// TestRenderErrStyledStripInvariant pins the content-invariant across the
// error render path: strip(styled) == plain byte-for-byte, and the styled
// form actually differs (proving the wrap ran, not that it was a no-op).
func TestRenderErrStyledStripInvariant(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeValidation, "bad value").WithHint("try again")

	var plainBuf, styledBuf bytes.Buffer
	_ = renderErr(IO{Err: &plainBuf, Format: "table"}, ce)
	_ = renderErr(IO{Err: &styledBuf, Format: "table", Color: "always", ErrTTY: true}, ce)

	assert.NotEqual(t, plainBuf.String(), styledBuf.String())
	assert.Equal(t, plainBuf.String(), style.Strip(styledBuf.String()))
	assert.Contains(t, styledBuf.String(), "\x1b[31m", "error prefix must be red")
	assert.Contains(t, styledBuf.String(), "\x1b[36m", "hint prefix must be cyan")
}

// TestRenderErrNeverStylesIneligibleFormats asserts the eligibility gate
// holds even when --color/TTY would otherwise turn styling on: json, yaml,
// name, and template render byte-identical with styling forced on.
func TestRenderErrNeverStylesIneligibleFormats(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeValidation, "bad value").WithHint("try again")

	for _, format := range []string{"json", "yaml", "name", "template={{.}}"} {
		t.Run("format="+format, func(t *testing.T) {
			t.Parallel()
			var plainBuf, forcedBuf bytes.Buffer
			_ = renderErr(IO{Err: &plainBuf, Format: format}, ce)
			_ = renderErr(IO{Err: &forcedBuf, Format: format, Color: "always", ErrTTY: true}, ce)
			assert.Equal(t, plainBuf.String(), forcedBuf.String())
			assert.NotContains(t, forcedBuf.String(), "\x1b[")
		})
	}
}

// TestRenderResultTableHeaderStyledStripInvariant pins the table-render
// content-invariant through the real renderResult path (not just
// output.RenderStyled directly): the header row is bold, every row is
// plain, and stripping restores the unstyled render exactly.
func TestRenderResultTableHeaderStyledStripInvariant(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "get thing", Shape: ShapeTabular}
	res := Result{Kind: "Thing", Data: fooTabularData{Foo: "bar"}}

	var plainBuf, styledBuf bytes.Buffer
	_, ce := renderResult(IO{Out: &plainBuf, Err: new(bytes.Buffer), Format: "table"}, d, res)
	require.Nil(t, ce)
	_, ce = renderResult(IO{Out: &styledBuf, Err: new(bytes.Buffer), Format: "table", Color: "always", OutTTY: true}, d, res)
	require.Nil(t, ce)

	assert.NotEqual(t, plainBuf.String(), styledBuf.String())
	assert.Equal(t, plainBuf.String(), style.Strip(styledBuf.String()))
	lines := splitKeepNewline(styledBuf.String())
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], "\x1b[1m", "header row must be bold")
	assert.NotContains(t, lines[1], "\x1b[", "data rows must never carry an escape code")
}

// TestRenderResultShownOnceLineBoldStripInvariant pins the once-only text
// scope: the shown-once line is bold, the headline and warnings mirror are
// untouched, and stripping restores the plain render exactly.
func TestRenderResultShownOnceLineBoldStripInvariant(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "create token", Delivery: DeliveryOnceOnly, Shape: ShapeText}
	shownOnce := "this token value is shown exactly once — store it now"
	res := Result{
		Kind:             "TokenCreated",
		Data:             map[string]string{"name": "t-abc"},
		Text:             MutationText("token", "t-abc", "created", "bearer token: tok:xxxx"+ShownOnceSuffix),
		Warnings:         []string{shownOnce},
		TextOnlyWarnings: []string{shownOnce},
	}

	var plainOut, plainErr, styledOut, styledErr bytes.Buffer
	_, ce := renderResult(IO{Out: &plainOut, Err: &plainErr, Format: "table"}, d, res)
	require.Nil(t, ce)
	_, ce = renderResult(IO{Out: &styledOut, Err: &styledErr, Format: "table", Color: "always", OutTTY: true, ErrTTY: true}, d, res)
	require.Nil(t, ce)

	assert.Equal(t, plainErr.String(), styledErr.String(), "the shown-once warning is silenced on both paths — nothing left to style")
	assert.NotEqual(t, plainOut.String(), styledOut.String())
	assert.Equal(t, plainOut.String(), style.Strip(styledOut.String()))

	lines := splitKeepNewline(styledOut.String())
	require.Len(t, lines, 2)
	assert.NotContains(t, lines[0], "\x1b[", "the headline must not be styled")
	assert.Contains(t, lines[1], "\x1b[1m", "the shown-once line must be bold")
}

// TestRenderResultOrdinaryTextNeverBoldedByShownOnceSuffixCoincidence pins
// the gate: only a DeliveryOnceOnly command's Text is scanned for
// ShownOnceSuffix, so an ordinary command's line ending in the identical
// bytes is never bolded.
func TestRenderResultOrdinaryTextNeverBoldedByShownOnceSuffixCoincidence(t *testing.T) {
	t.Parallel()
	d := Descriptor{Name: "do thing", Shape: ShapeText}
	res := Result{
		Kind: "ThingDone",
		Data: map[string]string{"name": "x"},
		Text: MutationText("thing", "x", "done", "value: y"+ShownOnceSuffix),
	}
	var out bytes.Buffer
	_, ce := renderResult(IO{Out: &out, Err: new(bytes.Buffer), Format: "table", Color: "always", OutTTY: true}, d, res)
	require.Nil(t, ce)
	assert.NotContains(t, out.String(), "\x1b[", "an ordinary command's Text is never scanned for the once-only marker")
}

// TestMirrorWarningsYellowPrefixStripInvariant pins the warning-mirror
// styling scope: only the "# warning:" prefix carries the yellow wrap, the
// message never does, and stripping restores the plain line exactly.
func TestMirrorWarningsYellowPrefixStripInvariant(t *testing.T) {
	t.Parallel()
	var plainBuf, styledBuf bytes.Buffer
	mirrorWarnings(&plainBuf, []string{"cert expires soon"}, nil, style.Style{})
	mirrorWarnings(&styledBuf, []string{"cert expires soon"}, nil, style.Style{Enabled: true})

	assert.Equal(t, "# warning: cert expires soon\n", plainBuf.String())
	assert.Equal(t, plainBuf.String(), style.Strip(styledBuf.String()))
	assert.Contains(t, styledBuf.String(), "\x1b[33m# warning:\x1b[0m cert expires soon\n")
}
