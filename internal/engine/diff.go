package engine

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// DiffObjects walks two decoded-JSON trees and emits one DiffEntry per
// changed leaf, keyed by JSON pointer.
func DiffObjects(a, b any) []safety.DiffEntry {
	return diffObjects("", a, b)
}

// ServerOwnedPaths are the metadata fields the server writes and a client
// never meaningfully changes: the resource version (the CLI stamps the
// fetched one as an update precondition) and the managed fields (the
// server's record of field ownership). The diff suppresses them and patch
// refuses ops that target them. []string key sequences, the same shape
// kinds.Kind.RedactPaths uses and ScrubPaths consumes directly.
var ServerOwnedPaths = [][]string{
	{"metadata", "resourceVersion"},
	{"metadata", "managedFields"},
}

// skippedPointers is ServerOwnedPaths rendered as JSON pointers, for comparison
// against the pointer-shaped prefixes diffObjects builds while walking.
var skippedPointers = func() []string {
	out := make([]string, len(ServerOwnedPaths))
	for i, p := range ServerOwnedPaths {
		out[i] = PointerFor(p)
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
// inside a larger emitted subtree. Skipped paths address object fields only -
// every segment is a fixed key, never an array index - so a non-map value
// cannot contain one and is returned unchanged, as is any map when no
// skipped path can lie under prefix (no allocation on subtrees like /spec).
// The removal itself is ScrubPaths.
func scrubValue(prefix string, v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	// prefix has one "/" per segment, so its segment count relativizes a
	// matching skipped path by slicing - no re-parsing.
	depth := strings.Count(prefix, "/")
	var rel [][]string
	for i, p := range skippedPointers {
		if strings.HasPrefix(p, prefix+"/") {
			rel = append(rel, ServerOwnedPaths[i][depth:])
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
			out = append(out, diffObjects(prefix+"/"+escapePointerToken(k), av[k], bv[k])...)
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

// identityPointers are the paths of the identity fields: the object's
// apiVersion, kind, name and generateName, which the preview's headline
// (create <kind>/<name>) already shows. Only these exact paths are skipped:
// a nested "kind" or "name" key, such as a machine pool's machineConfigRef,
// is a field the command sets.
var identityPointers = []string{"/apiVersion", "/kind", "/metadata/name", "/metadata/generateName"}

// CreateDiff lists what a create sends: one entry per leaf of the submission
// obj, with the RFC 6901 pointer as the path, a nil From and the submitted
// value as To. The entries follow the order DiffObjects uses: keys sorted at
// each level, list items by index. A leaf is a scalar, a null, a list that
// is empty (at any depth) or a map that is empty below the top level. The
// identity fields and ServerOwnedPaths are left out, and a map the skip
// empties is not a leaf. A create that sets nothing past its identity has no
// entries. The values the redaction covers are hidden (see RedactDiff).
//
// A redaction that is the zero value, or was built for another resource than
// gvr, is an internal error: no preview of a create may be built without the
// redaction of what it creates.
func CreateDiff(gvr schema.GroupVersionResource, obj map[string]any, r kinds.Redaction) ([]safety.DiffEntry, error) {
	if err := checkRedaction(gvr, r, "create"); err != nil {
		return nil, err
	}
	return createDiff(obj, r), nil
}

// createDiff is CreateDiff for a caller that has checked the redaction.
func createDiff(obj map[string]any, r kinds.Redaction) []safety.DiffEntry {
	var out []safety.DiffEntry
	walkSubmission(&out, "", 0, obj)
	return RedactDiff(out, r)
}

// walkSubmission appends the entries for the value v at path, which sits
// depth keys below the object root.
func walkSubmission(out *[]safety.DiffEntry, path string, depth int, v any) {
	if isSkippedPath(path) || slices.Contains(identityPointers, path) {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			// An empty top-level map (metadata: {}, spec: {}) sets nothing.
			if depth >= 2 {
				*out = append(*out, safety.DiffEntry{Path: path, To: map[string]any{}})
			}
			return
		}
		for _, k := range slices.Sorted(maps.Keys(t)) {
			walkSubmission(out, path+"/"+escapePointerToken(k), depth+1, t[k])
		}
	case []any:
		if len(t) == 0 {
			*out = append(*out, safety.DiffEntry{Path: path, To: []any{}})
			return
		}
		for i, item := range t {
			walkSubmission(out, fmt.Sprintf("%s/%d", path, i), depth+1, item)
		}
	default:
		*out = append(*out, safety.DiffEntry{Path: path, To: v})
	}
}
