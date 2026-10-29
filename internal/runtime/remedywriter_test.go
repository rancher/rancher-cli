package runtime

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
)

// writeRecorder records each Write call it receives, in the place of the
// writer a render goes through just before the stream.
type writeRecorder struct{ writes [][]byte }

func (w *writeRecorder) Write(b []byte) (int, error) {
	w.writes = append(w.writes, bytes.Clone(b))
	return len(b), nil
}

// markerSplitAcrossWrites reports whether an occurrence of needle in all the
// writes joined does not lie inside one write: the case a writer that takes
// each Write whole cannot handle.
func markerSplitAcrossWrites(writes [][]byte, needle string) bool {
	whole := 0
	for _, w := range writes {
		whole += bytes.Count(w, []byte(needle))
	}
	return whole != bytes.Count(bytes.Join(writes, nil), []byte(needle))
}

// TestPlainRemedyWriterRemovesOnlyThisProcessMarker asserts a marked
// command in one Write is written as the plain command, that U+2063 followed
// by the word rancher, and the marker of another process, are written as they
// are, and that the write reports its whole length.
func TestPlainRemedyWriterRemovesOnlyThisProcessMarker(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ in, want string }{
		"this process's marker": {"run " + clierr.Rancher + " get pods", "run rancher get pods"},
		"U+2063 and the word":   {"run \u2063rancher get pods", "run \u2063rancher get pods"},
		"another marker":        {"run \u2063\u2060\u2061\u2062\u2064rancher get pods", "run \u2063\u2060\u2061\u2062\u2064rancher get pods"},
		"no marker":             {"run rancher get pods", "run rancher get pods"},
	} {
		var out bytes.Buffer
		n, err := plainRemedyWriter{&out}.Write([]byte(tc.in))
		require.NoError(t, err, name)
		assert.Equal(t, len(tc.in), n, name)
		assert.Equal(t, tc.want, out.String(), name)
	}
}

// TestStructuredRenderersWriteAMarkedCommandInOneWrite asserts that json,
// yaml and a template each write a marked command into the writer the
// pipeline puts in front of the stream in one Write, so that a writer that
// takes each Write whole always sees all of it: a spy in that place records
// each call. The records hold the marked command, and no occurrence of it is
// split across two.
func TestStructuredRenderersWriteAMarkedCommandInOneWrite(t *testing.T) {
	t.Parallel()
	data := map[string]string{"next": clierr.Rancher + " config status", "also": clierr.Rancher + " get pods"}
	for name, format := range map[string]string{"json": "json", "yaml": "yaml", "template": "template={{.next}} {{.also}}"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var spy writeRecorder
			require.NoError(t, output.Render(&spy, format, output.NewEnvelope("Report", data)))
			joined := string(bytes.Join(spy.writes, nil))
			require.Contains(t, joined, clierr.Rancher, "the render writes a marked command, or the check below checks nothing")
			assert.False(t, markerSplitAcrossWrites(spy.writes, clierr.Rancher), "a marked command is split across %d writes", len(spy.writes))
		})
	}
}

// TestTheSplitCheckFailsForACommandWrittenInTwoParts asserts the check the
// test above makes: a fixture writer that splits one marked command across
// two Write calls is reported, and writes that each hold whole commands are
// not.
func TestTheSplitCheckFailsForACommandWrittenInTwoParts(t *testing.T) {
	t.Parallel()
	marked := clierr.Rancher + " get pods"
	half := len(clierr.Rancher) / 2
	var split writeRecorder
	_, _ = split.Write([]byte("x " + marked[:half]))
	_, _ = split.Write([]byte(marked[half:] + " y"))
	assert.True(t, markerSplitAcrossWrites(split.writes, clierr.Rancher))

	var whole writeRecorder
	_, _ = whole.Write([]byte("x " + marked))
	_, _ = whole.Write([]byte(strings.Repeat(" ", 3) + marked))
	assert.False(t, markerSplitAcrossWrites(whole.writes, clierr.Rancher))
}
