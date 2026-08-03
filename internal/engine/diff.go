package engine

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// DiffObjects walks two decoded-JSON trees and emits one DiffEntry per
// changed leaf, keyed by JSON pointer.
func DiffObjects(a, b any) []safety.DiffEntry {
	return diffObjects("", a, b)
}

// skippedPaths are the field paths diffObjects suppresses, as []string key
// sequences — the same shape kinds.Kind.RedactPaths uses and ScrubPaths
// consumes directly.
var skippedPaths = [][]string{
	{"metadata", "resourceVersion"},
	{"metadata", "managedFields"},
}

// skippedPointers is skippedPaths rendered as JSON pointers, for comparison
// against the pointer-shaped prefixes diffObjects builds while walking.
var skippedPointers = func() []string {
	out := make([]string, len(skippedPaths))
	for i, p := range skippedPaths {
		out[i] = "/" + strings.Join(p, "/")
	}
	return out
}()

// isSkippedPath reports whether diffObjects should suppress the given path.
func isSkippedPath(path string) bool {
	return slices.Contains(skippedPointers, path)
}

// scrubValue returns v with every skipped path under prefix removed. Used
// whenever a value that can be a map is emitted wholesale into a DiffEntry
// (type mismatches and leaf changes), so no branch can leak a skipped field
// inside a larger emitted subtree. Skipped paths address object fields only —
// every segment is a fixed key, never an array index — so a non-map value
// cannot contain one and is returned unchanged, as is any map when no
// skipped path can lie under prefix (no allocation on subtrees like /spec).
// The removal itself is ScrubPaths, the same copy-on-write scrub the
// resources table uses for RedactPaths.
func scrubValue(prefix string, v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	// prefix has one "/" per segment, so its segment count relativizes a
	// matching skipped path by slicing — no re-parsing.
	depth := strings.Count(prefix, "/")
	var rel [][]string
	for i, p := range skippedPointers {
		if strings.HasPrefix(p, prefix+"/") {
			rel = append(rel, skippedPaths[i][depth:])
		}
	}
	if len(rel) == 0 {
		return v
	}
	return ScrubPaths(m, rel)
}

func diffObjects(prefix string, a, b any) []safety.DiffEntry {
	if isSkippedPath(prefix) {
		return nil
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			// b is not a map here, and a skipped path can only sit inside a
			// map, so only the map side needs scrubbing.
			return []safety.DiffEntry{{Path: prefix, From: scrubValue(prefix, av), To: b}}
		}
		var out []safety.DiffEntry
		for _, k := range unionKeys(av, bv) {
			out = append(out, diffObjects(prefix+"/"+k, av[k], bv[k])...)
		}
		return out
	case []any:
		bv, ok := b.([]any)
		if !ok {
			// a is a slice, which cannot contain a skipped path; b may be a
			// map and is scrubbed.
			return []safety.DiffEntry{{Path: prefix, From: a, To: scrubValue(prefix, b)}}
		}
		var out []safety.DiffEntry
		for i := 0; i < max(len(av), len(bv)); i++ {
			var ai, bi any
			if i < len(av) {
				ai = av[i]
			}
			if i < len(bv) {
				bi = bv[i]
			}
			out = append(out, diffObjects(fmt.Sprintf("%s/%d", prefix, i), ai, bi)...)
		}
		return out
	default:
		if !reflect.DeepEqual(a, b) {
			// a is neither map nor slice in this branch, so only b can be a
			// map needing a scrub (e.g. DiffObjects("not-a-map", fullObject)).
			return []safety.DiffEntry{{Path: prefix, From: a, To: scrubValue(prefix, b)}}
		}
		return nil
	}
}

// unionKeys returns the sorted union of two maps' keys, for deterministic diffs.
func unionKeys(a, b map[string]any) []string {
	set := map[string]struct{}{}
	for k := range a {
		set[k] = struct{}{}
	}
	for k := range b {
		set[k] = struct{}{}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
