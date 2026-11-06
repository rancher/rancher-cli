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
	fuzzItemSentinel    = "ITEM-SENTINEL-VALUE"
)

// fuzzScrubRows are the registered rows for which FuzzScrubObject plants a
// hidden value at a top-level name, each with the name its redaction hides:
// a row path, a machine config Secret field, the Secret payload (which a
// read keeps) and the payload of another sensitive row (which a read
// hides). The helmop row, whose hidden paths run through a list, is a case
// of its own.
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
// redaction, except that a secret keeps its data. For the helmop row the
// hidden sentinels are in helm.values and helm.templateValues of each item of
// a spec.targets list whose length is fuzzed (0 to 4). A second sentinel,
// planted under a fuzzed key at a fuzzed depth (the key hex-encoded so it is
// one valid key that no hidden name matches), must survive, as must a third
// sentinel at the same hex key in each helmop target (once per target), and
// the input map must stay as it was.
// Never call t.Parallel() here - it panics inside f.Fuzz.
func FuzzScrubObject(f *testing.F) {
	for row := range fuzzScrubRows {
		for _, pattern := range []string{"", "\x00", "\x01", "\x00\x01\x01"} {
			f.Add("outside", uint8(0), pattern, uint8(row), uint8(0))
		}
		f.Add("a\x00b", uint8(2), "\x01", uint8(row), uint8(0))
		f.Add("", uint8(3), "\x00\x01", uint8(row), uint8(0))
	}
	for _, targets := range []uint8{0, 1, 2, 4} {
		for _, pattern := range []string{"", "\x01", "\x00\x01\x01"} {
			f.Add("outside", uint8(0), pattern, uint8(len(fuzzScrubRows)), targets)
		}
	}
	f.Add("a\x00b", uint8(2), "\x01", uint8(len(fuzzScrubRows)), uint8(2))

	f.Fuzz(func(t *testing.T, outsideKey string, depth uint8, pattern string, rowIndex, targetCount uint8) {
		row := struct{ key, hidden string }{key: "helmop"}
		if i := int(rowIndex) % (len(fuzzScrubRows) + 1); i < len(fuzzScrubRows) {
			row = fuzzScrubRows[i]
		}
		k, ok := kinds.ByKey(row.key)
		require.True(t, ok, row.key)
		r := k.ReadRedaction()
		targets := int(targetCount) % 5

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
		switch row.key {
		case "helmop":
			items := make([]any, targets)
			for i := range items {
				items[i] = map[string]any{
					fuzzCase("helm", pattern): map[string]any{
						fuzzCase("values", pattern):         map[string]any{"k": fuzzHiddenSentinel},
						fuzzCase("templateValues", pattern): fuzzHiddenSentinel,
					},
					key: fuzzItemSentinel,
				}
			}
			obj[fuzzCase("spec", pattern)] = map[string]any{fuzzCase("targets", pattern): items}
		case "secret":
			obj[fuzzCase(row.hidden, pattern)] = map[string]any{"k": fuzzPayloadSentinel}
		default:
			obj[fuzzCase(row.hidden, pattern)] = map[string]any{"k": fuzzHiddenSentinel}
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
		if row.key == "helmop" {
			assert.Equal(t, targets, bytes.Count(scrubbed, []byte(fuzzItemSentinel)), "a target value outside the redaction was removed: %s", scrubbed)
		}
		if row.key == "secret" {
			assert.Contains(t, string(scrubbed), fuzzPayloadSentinel, "a secret lost its data")
		}
	})
}
