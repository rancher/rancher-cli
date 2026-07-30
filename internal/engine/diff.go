package engine

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// DiffObjects walks two decoded-JSON trees and emits one DiffEntry per
// changed leaf, keyed by JSON pointer.
func DiffObjects(a, b any) []safety.DiffEntry {
	return diffObjects("", a, b)
}

func diffObjects(prefix string, a, b any) []safety.DiffEntry {
	switch prefix {
	case "/metadata/resourceVersion", "/metadata/managedFields":
		return nil
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return []safety.DiffEntry{{Path: prefix, From: a, To: b}}
		}
		var out []safety.DiffEntry
		for _, k := range unionKeys(av, bv) {
			out = append(out, diffObjects(prefix+"/"+k, av[k], bv[k])...)
		}
		return out
	case []any:
		bv, ok := b.([]any)
		if !ok {
			return []safety.DiffEntry{{Path: prefix, From: a, To: b}}
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
			return []safety.DiffEntry{{Path: prefix, From: a, To: b}}
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
