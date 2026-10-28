package engine

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"testing"
	"unicode"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fuzzHiddenSentinel  = "HIDDEN-SENTINEL-VALUE"
	fuzzPayloadSentinel = "PAYLOAD-SENTINEL-VALUE"
	fuzzOutsideSentinel = "OUTSIDE-SENTINEL-VALUE"
)

// fuzzScrubRows are the registered rows FuzzScrubObject plants a hidden
// value in, each with the top-level name its redaction hides: a row path, a
// machine config Secret field, the Secret payload (which a read keeps) and
// the payload of another sensitive row (which a read hides).
var fuzzScrubRows = []struct{ key, hidden string }{
	{"user", "password"},
	{"amazonec2config", "sshKeyContents"},
	{"secret", "data"},
	{"setting", "data"},
}

// fuzzCase writes name with each letter in the case the pattern picks: an
// odd byte makes the letter upper case and an even one lower case, the
// pattern repeating. An empty pattern leaves the name as it is.
func fuzzCase(name, pattern string) string {
	if pattern == "" {
		return name
	}
	out := []rune(name)
	for i, r := range out {
		if pattern[i%len(pattern)]&1 == 1 {
			out[i] = unicode.ToUpper(r)
		} else {
			out[i] = unicode.ToLower(r)
		}
	}
	return string(out)
}

// FuzzScrubObject fuzzes what a read hides: a sentinel planted at a
// registered row's hidden name, written in a fuzzed letter case, and in the
// last-applied annotation must not survive ScrubObject with the row's read
// redaction, except that a secret keeps its data. A second sentinel, planted
// under a fuzzed key at a fuzzed depth (the key hex-encoded so it is one
// valid key that no hidden name matches), must survive, and the input map
// must stay as it was. Never call t.Parallel() here - it panics inside
// f.Fuzz.
func FuzzScrubObject(f *testing.F) {
	for row := range fuzzScrubRows {
		for _, pattern := range []string{"", "\x00", "\x01", "\x00\x01\x01"} {
			f.Add("outside", uint8(0), pattern, uint8(row))
		}
		f.Add("a\x00b", uint8(2), "\x01", uint8(row))
		f.Add("", uint8(3), "\x00\x01", uint8(row))
	}

	f.Fuzz(func(t *testing.T, outsideKey string, depth uint8, pattern string, rowIndex uint8) {
		row := fuzzScrubRows[int(rowIndex)%len(fuzzScrubRows)]
		k, ok := kinds.ByKey(row.key)
		require.True(t, ok, row.key)
		r := k.ReadRedaction()

		key := hex.EncodeToString([]byte(outsideKey))
		outsidePath := make([]string, int(depth)%4+1)
		for i := range outsidePath {
			outsidePath[i] = key
		}
		require.False(t, r.Covers(outsidePath), "the hex-encoded outside key is hidden: %v", outsidePath)

		var outside any = fuzzOutsideSentinel
		for range outsidePath[1:] {
			outside = map[string]any{key: outside}
		}
		obj := map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{fuzzCase(kinds.LastAppliedAnnotation, pattern): fuzzHiddenSentinel}},
			key:        outside,
		}
		hiddenName := fuzzCase(row.hidden, pattern)
		if row.key == "secret" {
			obj[hiddenName] = map[string]any{"k": fuzzPayloadSentinel}
		} else {
			obj[hiddenName] = map[string]any{"k": fuzzHiddenSentinel}
		}
		before, err := json.Marshal(obj)
		require.NoError(t, err)

		out := ScrubObject(obj, r) // no panic

		after, err := json.Marshal(obj)
		require.NoError(t, err)
		assert.JSONEq(t, string(before), string(after), "the input map changed")
		scrubbed, err := json.Marshal(out)
		require.NoError(t, err)
		assert.False(t, bytes.Contains(scrubbed, []byte(fuzzHiddenSentinel)), "a hidden value survived: %s", scrubbed)
		assert.Contains(t, string(scrubbed), fuzzOutsideSentinel, "a value outside the redaction was removed")
		if row.key == "secret" {
			assert.Contains(t, string(scrubbed), fuzzPayloadSentinel, "a secret lost its data")
		}
	})
}
