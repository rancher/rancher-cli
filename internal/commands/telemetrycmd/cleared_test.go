package telemetrycmd

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearedNames maps each top-level path of the telemetry directory that off
// and reset remove to the name they report for it.
var clearedNames = map[string]string{
	"events":              clearedEvents,
	"servers":             clearedFacts,
	"config-directory-id": clearedDirectory,
	"upload":              clearedUploadState,
}

// TestClearedListsEveryRemovedPath asserts, for off and reset over a store
// that holds every kind of data, that the cleared list names exactly the
// top-level paths the command removed, and that the prune marker stays: a
// removal added without its entry in the list fails here.
func TestClearedListsEveryRemovedPath(t *testing.T) {
	for _, name := range []string{"off", "reset"} {
		t.Run(name, func(t *testing.T) {
			controlsEnv(t)
			dir := newDir(t)
			seedStore(t, dir)
			putFile(t, dir, "telemetry/upload/state.json", `{"httpStatus":200}`)
			putFile(t, dir, "telemetry/upload/lock", "2026-11-01T10:00:00Z 0123456789abcdef")
			tel := filepath.Join(dir, "telemetry")
			before := topLevel(names(t, tel))
			require.Equal(t, []string{"config-directory-id", "events", "pruned", "servers", "upload"}, before)

			res, err := execute(t, commandWith(name, dir, &fakeChanges{}))
			require.NoError(t, err)

			after := topLevel(names(t, tel))
			var removed []string
			for _, p := range before {
				if slices.Contains(after, p) {
					continue
				}
				reported, ok := clearedNames[p]
				require.True(t, ok, "%s removed %s, which the list of reported names has no entry for", name, p)
				removed = append(removed, reported)
			}
			assert.Equal(t, []string{"pruned"}, after)
			assert.ElementsMatch(t, removed, dataMap(t, res)["cleared"])
		})
	}
}

// topLevel returns the first element of each path, once, sorted.
func topLevel(paths []string) []string {
	var out []string
	for _, p := range paths {
		first, _, _ := strings.Cut(p, "/")
		if !slices.Contains(out, first) {
			out = append(out, first)
		}
	}
	slices.Sort(out)
	return out
}
