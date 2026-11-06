package engine

import (
	"maps"
	"slices"
	"strconv"

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

// ScrubObject returns obj without every map entry or list item whose path
// the redaction covers, so an object a command shows never holds a value the
// redaction hides. A list item's path segment is its index. The maps and
// lists on the path to a removed value are copied before they change, so
// obj, which the client may share, is never modified; an object with nothing
// to remove is returned as it is. A nil value stays. obj must be JSON-shaped:
// `map[string]any`, `[]any` and scalars, as JSON decoding produces. A typed
// Go slice or map, such as `[]map[string]any`, is not searched; a value below
// one is not removed.
func ScrubObject(obj map[string]any, r kinds.Redaction) map[string]any {
	if !r.Hides() {
		return obj
	}
	out, _ := removeCovered(nil, obj, r.Covers)
	return out
}

// removeCovered returns m with every entry whose path (keys, extended by the
// entry's key) covered reports true removed, and whether anything changed.
// It searches the maps and lists below m; m is never modified.
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
		next, changed := removeCoveredIn(childKeys, child, covered)
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

// removeCoveredIn calls removeCovered for a map and removeCoveredItems for a
// list, and returns any other value unchanged.
func removeCoveredIn(keys []string, v any, covered func([]string) bool) (any, bool) {
	switch v := v.(type) {
	case map[string]any:
		return removeCovered(keys, v, covered)
	case []any:
		return removeCoveredItems(keys, v, covered)
	}
	return v, false
}

// removeCoveredItems returns items after two removals: an item is removed
// when covered reports true for its path (keys, extended by the item's
// index), and covered values below the other items are removed. It also
// returns whether anything changed. The items after a removed item get lower
// indexes. A nil item stays. items is never modified; a list with nothing to
// remove is returned as it is.
func removeCoveredItems(keys []string, items []any, covered func([]string) bool) ([]any, bool) {
	var out []any
	for i, item := range items {
		itemKeys := append(slices.Clone(keys), strconv.Itoa(i))
		removed := item != nil && covered(itemKeys)
		next, changed := item, false
		if !removed {
			next, changed = removeCoveredIn(itemKeys, item, covered)
		}
		if out == nil {
			if !removed && !changed {
				continue
			}
			out = make([]any, i, len(items))
			copy(out, items[:i])
		}
		if !removed {
			out = append(out, next)
		}
	}
	if out == nil {
		return items, false
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

// replaceCovered returns v with every map value or list item whose path
// (keys, extended by the map keys and list indexes below v) covered reports
// true replaced by with, and whether anything was replaced. The maps and
// lists on the path to a replaced value are copied before they change, so the
// original value, which a caller may share with the objects a patch
// fetched, is never modified; an unchanged v is returned as it is. A nil
// value stays nil.
func replaceCovered(keys []string, v any, covered func([]string) bool, with any) (any, bool) {
	switch v := v.(type) {
	case map[string]any:
		var out map[string]any
		for k, child := range v {
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
				out = maps.Clone(v)
			}
			out[k] = next
		}
		if out == nil {
			return v, false
		}
		return out, true
	case []any:
		var out []any
		for i, item := range v {
			if item == nil {
				continue
			}
			itemKeys := append(slices.Clone(keys), strconv.Itoa(i))
			next := with
			if !covered(itemKeys) {
				var changed bool
				if next, changed = replaceCovered(itemKeys, item, covered, with); !changed {
					continue
				}
			}
			if out == nil {
				out = slices.Clone(v)
			}
			out[i] = next
		}
		if out == nil {
			return v, false
		}
		return out, true
	}
	return v, false
}
