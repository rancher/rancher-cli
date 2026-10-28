package engine

import (
	"maps"
	"slices"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

// ScrubPaths returns a shallow copy of obj with every listed path deleted.
// A path is a []string key sequence addressing object fields; segments are
// fixed keys and never traverse arrays. The copy is shallow at the top
// level: nested maps along a path are deep-copied only as far as needed -
// and at most once per parent, even when several paths share one - so the
// original objects are not mutated. DiffObjects uses it to drop skipped
// paths. It applies no redaction; ScrubObject does.
func ScrubPaths(obj map[string]any, paths [][]string) map[string]any {
	if len(paths) == 0 {
		return obj
	}
	out := make(map[string]any, len(obj))
	maps.Copy(out, obj)
	deletePaths(out, paths)
	return out
}

// scrubResult is ScrubObject for the result of a create or an update: the
// values the preview of the same command hides.
func scrubResult(obj map[string]any, r kinds.Redaction) map[string]any {
	return ScrubObject(obj, r)
}

// ScrubObject returns obj without every map entry whose path the redaction
// covers, so an object a command shows never holds a value the redaction
// hides. The maps along a removed entry are copied before they change, so
// obj, which the client may share, is never modified; an object with
// nothing to remove is returned as it is. A nil value stays and arrays are
// not entered: a hidden path is a sequence of object keys and never
// addresses an array item.
func ScrubObject(obj map[string]any, r kinds.Redaction) map[string]any {
	if !r.Hides() {
		return obj
	}
	out, _ := removeCovered(nil, obj, r.Covers)
	return out
}

// removeCovered returns m with every entry whose path (keys, extended by the
// entry's key) covered reports true removed, and whether anything changed.
func removeCovered(keys []string, m map[string]any, covered func([]string) bool) (map[string]any, bool) {
	var out map[string]any
	for k, child := range m {
		if child == nil {
			continue
		}
		childKeys := append(slices.Clone(keys), k)
		if covered(childKeys) {
			if out == nil {
				out = maps.Clone(m)
			}
			delete(out, k)
			continue
		}
		childMap, ok := child.(map[string]any)
		if !ok {
			continue
		}
		next, changed := removeCovered(childKeys, childMap, covered)
		if !changed {
			continue
		}
		if out == nil {
			out = maps.Clone(m)
		}
		out[k] = next
	}
	if out == nil {
		return m, false
	}
	return out, true
}

// deletePaths removes every leaf addressed by paths from m, copying each
// intermediate map at most once even when several paths share a parent.
func deletePaths(m map[string]any, paths [][]string) {
	var byHead map[string][][]string
	for _, path := range paths {
		switch len(path) {
		case 0:
		case 1:
			delete(m, path[0])
		default:
			if byHead == nil {
				byHead = map[string][][]string{}
			}
			byHead[path[0]] = append(byHead[path[0]], path[1:])
		}
	}
	for head, rest := range byHead {
		child, ok := m[head]
		if !ok {
			continue
		}
		childMap, ok := child.(map[string]any)
		if !ok {
			continue
		}
		// Copy the child map before mutating so the original is not touched.
		copied := make(map[string]any, len(childMap))
		maps.Copy(copied, childMap)
		deletePaths(copied, rest)
		m[head] = copied
	}
}

// replaceCovered returns v with every map value whose path (keys, extended
// by the map keys below v) covered reports true replaced by with, and
// whether anything was replaced. The maps along a replaced path are copied
// before they change, so the original value, which a caller may share with
// the objects a patch fetched, is never modified; an unchanged v is
// returned as it is. A nil value stays nil. Arrays are not entered: a
// covered path is a sequence of object keys and never addresses an array
// item.
func replaceCovered(keys []string, v any, covered func([]string) bool, with any) (any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return v, false
	}
	var out map[string]any
	for k, child := range m {
		if child == nil {
			continue
		}
		childKeys := append(slices.Clone(keys), k)
		next := with
		if !covered(childKeys) {
			var changed bool
			if next, changed = replaceCovered(childKeys, child, covered, with); !changed {
				continue
			}
		}
		if out == nil {
			out = maps.Clone(m)
		}
		out[k] = next
	}
	if out == nil {
		return v, false
	}
	return out, true
}
