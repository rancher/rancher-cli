package engine

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// sentinelMarker is planted at the fuzzed path's leaf. It uses only
// uppercase letters and a hyphen, so it can never appear as a substring of
// a lowercase-hex-encoded path segment (see below).
const sentinelMarker = "SENTINEL-MARKER-VALUE"

// FuzzScrubPaths fuzzes the marker-secret property: a sentinel planted at
// every retained fuzzed path must not survive scrubbing. TWO independently
// fuzzed paths per iteration exercise the multi-path grouping (paths
// sharing a head bucket, sibling leaves, shared parents) that a single
// path never reaches. The document is built from the fuzzed paths
// themselves — maps only — with segments HEX-ENCODED into the keys, so no
// key can contain the sentinel as a substring (otherwise a fuzzed segment
// holding the marker would false-fail the search after a correct
// deletion). Prefix-colliding paths are normalized at construction (a
// path that is a prefix of the other would overwrite the other's
// intermediate map with its sentinel leaf); duplicates collapse. Each blob
// is NUL-split into segments, the same encoding FuzzParseArgTail uses.
// Never call t.Parallel() here — it panics inside f.Fuzz.
func FuzzScrubPaths(f *testing.F) {
	f.Add("secret", "password")
	f.Add("secret\x00password", "secret\x00token")
	f.Add("", "")
	f.Add("a\x00b\x00c", "a\x00b")
	f.Add("a\x00b", "a\x00b")

	f.Fuzz(func(t *testing.T, blob1, blob2 string) {
		decode := func(blob string) []string {
			segments := strings.Split(blob, "\x00")
			path := make([]string, len(segments))
			for i, s := range segments {
				path[i] = hex.EncodeToString([]byte(s))
			}
			return path
		}
		paths := [][]string{decode(blob1)}
		p2 := decode(blob2)
		if !isPathPrefix(paths[0], p2) && !isPathPrefix(p2, paths[0]) {
			paths = append(paths, p2)
		}

		obj := map[string]any{}
		for _, path := range paths {
			cur := obj
			for i, key := range path {
				if i == len(path)-1 {
					cur[key] = sentinelMarker
					break
				}
				next, ok := cur[key].(map[string]any)
				if !ok {
					next = map[string]any{}
					cur[key] = next
				}
				cur = next
			}
		}

		out := ScrubPaths(obj, paths) // no panic

		assert.False(t, containsSentinel(out), "sentinel survived scrubbing: %#v", out)
	})
}

// isPathPrefix reports whether a is a (non-strict) prefix of b — such
// pairs cannot coexist in one document (a's sentinel leaf occupies the
// map b needs as an intermediate), so the harness retains only the first.
func isPathPrefix(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsSentinel(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, sentinelMarker)
	case map[string]any:
		for k, vv := range x {
			if strings.Contains(k, sentinelMarker) || containsSentinel(vv) {
				return true
			}
		}
	}
	return false
}
