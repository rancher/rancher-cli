package conformance

import (
	"bytes"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stayOnTheMachinePhrases are texts that say recorded events never leave the
// machine, that nothing is sent, or that nothing encodes or uploads them. A
// development build with the telemetryupload experiment sends events, so a
// text that says otherwise is false. Each is matched without regard to case,
// after every run of spaces and line breaks reads as one space.
var stayOnTheMachinePhrases = []string{
	"sends no events anywhere",
	"events recorded while nothing is sent are never uploaded",
	"documentation only; nothing encodes otlp yet",
	"documentation only: nothing in the cli encodes otlp",
	"## never uploaded",
	"events recorded while nothing is uploaded stay on the machine",
	"events are recorded in a separate directory that starts empty",
	"recording is on by default and stays on the machine",
	"no recorded event leaves the machine",
	"the cli has no upload code",
	"nothing is sent in this version",
	"nothing sends it yet",
	"recorded events stay on this machine",
	"records events on the local machine only",
}

// statusSendingLines are the two lines `telemetry status` prints when this
// build sends nothing. They are true as written: one for a release build, one
// for a development build with no upload endpoint. The phrase is allowed only
// inside these exact lines; they are removed from a text before it is matched.
var statusSendingLines = []string{
	"sending: none - this build sends no event; recorded events stay on this machine",
	"sending: none - no upload endpoint is set; recorded events stay on this machine",
}

// phraseExempt are the files that name the phrases on purpose: this test, and
// the scan of file headers, whose allowlist holds the heading a generated
// file once had.
var phraseExempt = []string{
	"test/conformance/noeventstext_test.go",
	"test/conformance/fileheaders_test.go",
}

// phraseFindings returns one line per phrase found in a text file of files
// (path to content), other than the exempt files. The two status lines are
// removed first. The phrase counts only outside them.
func phraseFindings(files map[string]string) []string {
	var out []string
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if slices.Contains(phraseExempt, path) {
			continue
		}
		text := strings.ToLower(strings.Join(strings.Fields(files[path]), " "))
		for _, line := range statusSendingLines {
			text = strings.ReplaceAll(text, line, "")
		}
		for _, phrase := range stayOnTheMachinePhrases {
			if strings.Contains(text, phrase) {
				out = append(out, path+": "+phrase)
			}
		}
	}
	return out
}

// TestNoTextSaysEventsStayOnTheMachine asserts no tracked text file says that
// recorded events stay on the machine or that nothing is sent, in any of the
// wordings listed, wrapped over lines or not, except inside the two status
// lines. The fixtures assert each phrase is found when a line break splits it,
// that the exempt files are skipped, that each status line is allowed whole
// (also wrapped over lines) and that a status line with one word changed, or
// the bare phrase beside it, is found.
func TestNoTextSaysEventsStayOnTheMachine(t *testing.T) {
	paths, err := repositoryFiles()
	require.NoError(t, err)
	files := map[string]string{}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		require.NoError(t, err, path)
		if bytes.IndexByte(b, 0) >= 0 {
			continue
		}
		files[path] = string(b)
	}
	assert.Empty(t, phraseFindings(files))

	for _, phrase := range stayOnTheMachinePhrases {
		wrapped := strings.Replace(phrase, " ", "\n  ", 1)
		assert.Equal(t, []string{"a.md: " + phrase}, phraseFindings(map[string]string{"a.md": "x " + strings.ToUpper(wrapped) + " y"}), phrase)
		assert.Empty(t, phraseFindings(map[string]string{phraseExempt[0]: phrase, phraseExempt[1]: phrase}))
	}

	for _, line := range statusSendingLines {
		assert.Empty(t, phraseFindings(map[string]string{"a.md": "x `" + line + "` y"}), line)
		assert.Empty(t, phraseFindings(map[string]string{"a.md": strings.ReplaceAll(line, " ", "\n  ")}), line)
		assert.Equal(t, []string{"a.md: recorded events stay on this machine"},
			phraseFindings(map[string]string{"a.md": line + "\nrecorded events stay on this machine"}), line)
		changed := strings.Replace(line, "none", "nothing", 1)
		assert.Equal(t, []string{"a.md: recorded events stay on this machine"}, phraseFindings(map[string]string{"a.md": changed}), changed)
	}
}
