package engine

import (
	"context"
	"maps"

	"github.com/rancher/rancher-cli/internal/runtime"
)

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
	maps.Copy(out, obj)
	deletePaths(out, paths)
	return out
}

// RedactMutationResult scrubs sensitive fields from a mutation's rendered
// response before it reaches output: the server can echo a submitted
// credential back verbatim on the commit response even though it never
// returns one on a later read. It removes the kind's declared redactPaths
// (a named field like User's root-level password) and, for a sensitive
// kind, a Secret-shaped payload's data/stringData — the same two guards
// the patch preview-diff redaction applies, so the seams cannot silently
// diverge on what counts as a credential. Every executor that places a
// server-echoed object in Result.Data wraps itself here.
func RedactMutationResult(exec runtime.ExecuteFunc, redactPaths [][]string, sensitive bool) runtime.ExecuteFunc {
	return func(ctx context.Context) (runtime.Result, error) {
		res, err := exec(ctx)
		if err != nil {
			return res, err
		}
		obj, ok := res.Data.(map[string]any)
		if !ok {
			return res, nil
		}
		obj = ScrubPaths(obj, redactPaths)
		if sensitive {
			obj = ScrubPaths(obj, [][]string{{"data"}, {"stringData"}})
		}
		res.Data = obj
		return res, nil
	}
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
