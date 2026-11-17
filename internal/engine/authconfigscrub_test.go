package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const authConfigFixturesPath = "kinds/testdata/authconfig-fixtures.json"

// authConfigFixture is one object of the shared auth config fixtures: a value
// that holds "SECRET-" or is a reference must be hidden; every other leaf must
// be shown.
type authConfigFixture struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Variant string         `json:"variant"`
	Object  map[string]any `json:"object"`
}

func loadAuthConfigFixtures(t *testing.T) []authConfigFixture {
	t.Helper()
	b, err := os.ReadFile(authConfigFixturesPath)
	require.NoError(t, err)
	var out []authConfigFixture
	require.NoError(t, json.Unmarshal(b, &out))
	require.NotEmpty(t, out)
	return out
}

// authConfigLeaves adds one string per leaf of v: the map keys and list markers
// on the way, then "=" and the leaf value. A list item is marked "[]" with no
// index, which keeps a string the same when a removal moves the items up.
func authConfigLeaves(v any, path string, out map[string]bool) {
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			authConfigLeaves(child, path+"."+k, out)
		}
	case []any:
		for _, child := range v {
			authConfigLeaves(child, path+"[]", out)
		}
	default:
		out[path+"="+fmt.Sprint(v)] = true
	}
}

// expectedShownLeaves returns the leaves of obj that must survive the scrub:
// those that carry no "SECRET-" marker in the path or the value and whose
// value is not a reference.
func expectedShownLeaves(obj map[string]any) map[string]bool {
	all := map[string]bool{}
	authConfigLeaves(obj, "", all)
	out := map[string]bool{}
	for leaf := range all {
		_, value, _ := strings.Cut(leaf, "=")
		if strings.Contains(leaf, "SECRET-") || strings.HasPrefix(value, "cattle-global-data:") {
			continue
		}
		out[leaf] = true
	}
	return out
}

// authConfigFixtureObject reads the fixtures file and returns the object of
// the named fixture as a new object that no test has modified.
func authConfigFixtureObject(t *testing.T, name string) map[string]any {
	t.Helper()
	for _, f := range loadAuthConfigFixtures(t) {
		if f.Name == name {
			return f.Object
		}
	}
	require.Failf(t, "no such fixture", name)
	return nil
}

// TestAuthConfigScrubHidesEverySecretAndKeepsEveryShownValue asserts, over
// every auth provider type and both variants (references and plaintext), that
// ScrubObject with the row's preview and read redactions removes every
// credential field, every reference at any depth and as a list item, keeps
// every other value (a map key that starts with the prefix, a number, a
// boolean, the prefix in another letter case, the public certificates), and
// never modifies its input.
func TestAuthConfigScrubHidesEverySecretAndKeepsEveryShownValue(t *testing.T) {
	t.Parallel()
	row, ok := kinds.ByKey("authconfig")
	require.True(t, ok)
	for _, f := range loadAuthConfigFixtures(t) {
		for name, redaction := range map[string]kinds.Redaction{"preview": row.Redaction(), "read": row.ReadRedaction()} {
			t.Run(f.Name+" "+name, func(t *testing.T) {
				t.Parallel()
				before := authConfigFixtureObject(t, f.Name)
				beforeLeaves := map[string]bool{}
				authConfigLeaves(before, "", beforeLeaves)
				hasSecret, hasShown := false, false
				for leaf := range beforeLeaves {
					hasSecret = hasSecret || strings.Contains(leaf, "SECRET-")
					hasShown = hasShown || strings.Contains(leaf, "SHOWN-")
				}
				require.True(t, f.Type == "localConfig" || hasSecret, "the fixture has a value marked SECRET-")
				require.True(t, hasShown, "the fixture has a value marked SHOWN-")

				got := ScrubObject(f.Object, redaction)

				leaves := map[string]bool{}
				authConfigLeaves(got, "", leaves)
				assert.Equal(t, expectedShownLeaves(f.Object), leaves)
				assert.Equal(t, before, f.Object, "the input is not modified")
			})
		}
	}
}

// TestRemoveReferencesWalksListsAndMaps asserts removeReferences removes a
// reference item of a list and moves the items after it up, removes a map
// entry whose value is a reference at any depth, never tests map keys, leaves
// an object with nothing to remove as it is, and never modifies the input.
func TestRemoveReferencesWalksListsAndMaps(t *testing.T) {
	t.Parallel()
	ref := "cattle-global-data:x"
	in := map[string]any{
		"list":   []any{"a", ref, map[string]any{"k": ref, "keep": "b"}, []any{ref, "c"}},
		"nested": map[string]any{"deep": map[string]any{"r": ref}},
		ref:      "key is never tested",
		"number": float64(1),
	}
	got, changed := removeReferences(in)
	require.True(t, changed)
	assert.Equal(t, map[string]any{
		"list":   []any{"a", map[string]any{"keep": "b"}, []any{"c"}},
		"nested": map[string]any{"deep": map[string]any{}},
		ref:      "key is never tested",
		"number": float64(1),
	}, got)
	assert.Equal(t, ref, in["list"].([]any)[1], "the input list is not modified")
	assert.Contains(t, in["nested"].(map[string]any)["deep"], "r", "the input map is not modified")

	clean := map[string]any{"a": []any{"x", map[string]any{"b": "y"}}}
	same, changed := removeReferences(clean)
	assert.False(t, changed)
	assert.Equal(t, clean, same)
}
