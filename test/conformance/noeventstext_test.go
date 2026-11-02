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

// phraseExempt are the files that name the phrases on purpose: this test, and
// the scan of file headers, whose allowlist holds the heading a generated
// file once had.
var phraseExempt = []string{
	"test/conformance/noeventstext_test.go",
	"test/conformance/fileheaders_test.go",
}

// phraseFindings returns one line per phrase found in a text file of files
// (path to content), other than the exempt files.
func phraseFindings(files map[string]string) []string {
	var out []string
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if slices.Contains(phraseExempt, path) {
			continue
		}
		text := strings.ToLower(strings.Join(strings.Fields(files[path]), " "))
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
// wordings listed, wrapped over lines or not. The fixtures assert each phrase
// is found when a line break splits it, and that the exempt files are skipped.
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
}
