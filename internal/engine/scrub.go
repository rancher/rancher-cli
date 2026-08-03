package engine

// ScrubPaths returns a shallow copy of obj with every listed path deleted.
// A path is a []string key sequence addressing object fields; segments are
// fixed keys and never traverse arrays. The copy is shallow at the top
// level: nested maps along a path are deep-copied only as far as needed —
// and at most once per parent, even when several paths share one — so the
// original objects are not mutated. Shared by the resources table's
// -o json/yaml redaction (kind RedactPaths) and DiffObjects's skipped-path
// scrub, so path-removal semantics cannot drift between the two. The two
// callers have different stakes — RedactPaths hides credentials, the diff
// scrub hides noise — so a semantics change for one (e.g. fail-closed on an
// unexpected shape, or descending into arrays) must deliberately consider
// the other.
func ScrubPaths(obj map[string]any, paths [][]string) map[string]any {
	if len(paths) == 0 {
		return obj
	}
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		out[k] = v
	}
	deletePaths(out, paths)
	return out
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
		for k, v := range childMap {
			copied[k] = v
		}
		deletePaths(copied, rest)
		m[head] = copied
	}
}
