package runtime

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
)

var errScriptedWrite = errors.New("scripted write failure")

// scriptedWriter keeps the bytes it accepted and counts its calls. failFrom
// fails every call from that number on, failOnly fails that one call, and
// limit accepts that many bytes in total and fails the one call that crosses
// it, after which it accepts every call; a zero value of each turns it off.
type scriptedWriter struct {
	buf      bytes.Buffer
	calls    int
	failFrom int
	failOnly int
	limit    int
	crossed  bool
}

func (w *scriptedWriter) Write(p []byte) (int, error) {
	w.calls++
	if (w.failFrom > 0 && w.calls >= w.failFrom) || w.calls == w.failOnly {
		return 0, errScriptedWrite
	}
	if w.limit > 0 && !w.crossed && w.buf.Len()+len(p) > w.limit {
		w.crossed = true
		n := w.limit - w.buf.Len()
		w.buf.Write(p[:n])
		return n, errScriptedWrite
	}
	return w.buf.Write(p)
}

var writeErrorFormats = []string{"", "table", "wide", "json", "yaml", "name", "template={{.}}"}

// unencodableDetails is a details payload with a text projection and a field
// that json cannot encode.
type unencodableDetails struct {
	Channel chan int `json:"channel"`
}

func (unencodableDetails) TextDetails() []clierr.DetailLine {
	return []clierr.DetailLine{clierr.Line(clierr.LabelPart("details line"))}
}

func validationError() *clierr.Error {
	return clierr.New(clierr.CodeValidation, "message")
}

// TestWriteErrorWritesTheErrorWhenTheWriterWorks asserts a writer that
// accepts everything gets what RenderErrorStyled writes, and nothing else.
func TestWriteErrorWritesTheErrorWhenTheWriterWorks(t *testing.T) {
	t.Parallel()
	for _, format := range writeErrorFormats {
		var want bytes.Buffer
		require.NoError(t, RenderErrorStyled(&want, format, validationError(), style.Style{}), format)
		var w scriptedWriter
		WriteError(&w, format, validationError(), style.Style{})
		assert.Equal(t, want.String(), w.buf.String(), format)
		assert.Equal(t, 1, w.calls, format)
	}
}

// TestWriteErrorWritesOneLineWhenTheEnvelopeCannotBeEncoded asserts an error
// whose details json cannot encode is written under json and yaml as the one
// line, and as its normal text block under every other format.
func TestWriteErrorWritesOneLineWhenTheEnvelopeCannotBeEncoded(t *testing.T) {
	t.Parallel()
	ce := func() *clierr.Error {
		return validationError().WithDetails(unencodableDetails{Channel: make(chan int)})
	}
	for _, format := range writeErrorFormats {
		var w scriptedWriter
		WriteError(&w, format, ce(), style.Style{})
		if format == "json" || format == "yaml" {
			assert.Equal(t, "error (VALIDATION): message\n", w.buf.String(), format)
			continue
		}
		var want bytes.Buffer
		require.NoError(t, RenderErrorStyled(&want, format, ce(), style.Style{}), format)
		assert.Equal(t, want.String(), w.buf.String(), format)
		assert.Contains(t, w.buf.String(), "details line", format)
	}
}

// TestWriteErrorReturnsWhenEveryWriteFails asserts a writer that fails every
// call gets no byte, and the function returns.
func TestWriteErrorReturnsWhenEveryWriteFails(t *testing.T) {
	t.Parallel()
	for _, format := range writeErrorFormats {
		w := scriptedWriter{failFrom: 1}
		WriteError(&w, format, validationError(), style.Style{})
		assert.Zero(t, w.buf.Len(), format)
		assert.LessOrEqual(t, w.calls, 2, format)
	}
}

// TestWriteErrorWritesNoLineAfterAPartialWrite asserts that when the writer
// accepted some bytes of the error before it failed, the line is not
// written: the bytes accepted are exactly the first of the error.
func TestWriteErrorWritesNoLineAfterAPartialWrite(t *testing.T) {
	t.Parallel()
	const accepted = 5
	for _, format := range writeErrorFormats {
		var want bytes.Buffer
		require.NoError(t, RenderErrorStyled(&want, format, validationError(), style.Style{}), format)
		require.Greater(t, want.Len(), accepted, format)
		w := scriptedWriter{limit: accepted}
		WriteError(&w, format, validationError(), style.Style{})
		assert.Equal(t, want.String()[:accepted], w.buf.String(), format)
	}
}

// TestWriteErrorWritesTheLineWhenTheFirstWriteFails asserts a writer that
// fails its first call only gets exactly the one line, in every format.
func TestWriteErrorWritesTheLineWhenTheFirstWriteFails(t *testing.T) {
	t.Parallel()
	for _, format := range writeErrorFormats {
		w := scriptedWriter{failOnly: 1}
		WriteError(&w, format, validationError(), style.Style{})
		assert.Equal(t, "error (VALIDATION): message\n", w.buf.String(), format)
	}
}

// TestWriteErrorLineHoldsNoControlCharacterNewlineOrMarker asserts the line
// is one line of plain text: a message with a terminal escape, a newline and
// a remedy marker is written with the escape and the newline removed and
// the command plain, and an empty message leaves the code and a space.
func TestWriteErrorLineHoldsNoControlCharacterNewlineOrMarker(t *testing.T) {
	t.Parallel()
	for _, format := range writeErrorFormats {
		ce := clierr.New(clierr.CodeGeneric, "bad \x1b[2J\nrun "+clierr.Rancher+" auth status")
		w := scriptedWriter{failOnly: 1}
		WriteError(&w, format, ce, style.Style{})
		got := w.buf.String()
		assert.Equal(t, "error (GENERIC): bad [2Jrun rancher auth status\n", got, format)
		assert.Equal(t, 1, strings.Count(got, "\n"), format)
		assert.NotContains(t, got, "\x1b", format)
		assert.NotContains(t, got, clierr.RemedyMarker, format)
		assert.False(t, strings.ContainsFunc(got, func(r rune) bool { return r >= '\u2060' && r <= '\u2064' }), "a character of the remedy marker in %q (%s)", got, format)

		empty := scriptedWriter{failOnly: 1}
		WriteError(&empty, format, clierr.New(clierr.CodeGeneric, ""), style.Style{})
		assert.Equal(t, "error (GENERIC): \n", empty.buf.String(), format)
	}
}
