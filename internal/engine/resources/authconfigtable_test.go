package resources

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const authConfigFixturesPath = "../kinds/testdata/authconfig-fixtures.json"

// authConfigBase reads the plaintext githubConfig fixture from the fixtures
// file and returns it as a new object. The fixture sets every column of the
// authconfig row to a plain value.
func authConfigBase(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(authConfigFixturesPath)
	require.NoError(t, err)
	var cases []struct {
		Name   string         `json:"name"`
		Object map[string]any `json:"object"`
	}
	require.NoError(t, json.Unmarshal(b, &cases))
	for _, c := range cases {
		if c.Name == "githubConfig/plaintext" {
			return c.Object
		}
	}
	require.Fail(t, "fixture githubConfig/plaintext is missing")
	return nil
}

// TestRowsThatHideReferencesUseOnlyPathAndCountColumns asserts every row that
// sets RedactReferences has only Path or AltPaths columns (with or without
// Bool) and Count columns. The RedactReferences check covers those; a Join or
// Condition column on such a row could show a reference, because the check does
// not cover it.
func TestRowsThatHideReferencesUseOnlyPathAndCountColumns(t *testing.T) {
	t.Parallel()
	found := 0
	for _, k := range kinds.All() {
		if !k.RedactReferences {
			continue
		}
		found++
		for _, c := range k.Columns {
			assert.False(t, c.Join || c.Condition != "", "a Join or Condition column on a row that hides references could show a reference: %s %s", k.Key, c.Header)
			assert.True(t, len(c.Path) > 0 || len(c.Count) > 0, "%s %s", k.Key, c.Header)
		}
	}
	assert.NotZero(t, found)
}

// TestAuthConfigCellsHideAReferenceAtEveryColumn asserts, with the table built
// from the registered row, that a reference at the path of a column gives an
// empty cell in -o table and -o wide (ENABLED too, not "false"), that
// ALLOWED-PRINCIPALS over a list that holds a reference shows the count, and
// that the same object with plain values shows them.
func TestAuthConfigCellsHideAReferenceAtEveryColumn(t *testing.T) {
	t.Parallel()
	row := mustKind(t, "authconfig")
	const ref = "cattle-global-data:github-secret"

	cells := func(t *testing.T, obj map[string]any) map[string]string {
		t.Helper()
		items := []unstructured.Unstructured{{Object: obj}}
		tbl := newTable(row, items, false)
		out := map[string]string{}
		wide := tbl.WideRows()[0]
		for i, h := range tbl.WideColumns() {
			out[h] = wide[i]
		}
		narrow := tbl.Rows()[0]
		for i, h := range tbl.Columns() {
			assert.Equal(t, out[h], narrow[i], "-o table and -o wide agree on %s", h)
		}
		return out
	}

	plain := cells(t, authConfigBase(t))
	assert.Equal(t, "githubConfig", plain["TYPE"])
	assert.Equal(t, "true", plain["ENABLED"])
	assert.Equal(t, "SHOWN-restricted", plain["ACCESS-MODE"])
	assert.Equal(t, "2", plain["ALLOWED-PRINCIPALS"])
	assert.Equal(t, "SHOWN-unlocked", plain["CLEANUP"])
	assert.Equal(t, "SHOWN-display-githubConfig", plain["DISPLAY NAME"])
	assert.Equal(t, "SHOWN-description-githubConfig", plain["DESCRIPTION"])

	shown := 0
	for _, c := range row.Columns {
		if len(c.Count) > 0 {
			obj := authConfigBase(t)
			list := obj["allowedPrincipalIds"].([]any)
			obj["allowedPrincipalIds"] = append(list, ref)
			assert.Equal(t, "3", cells(t, obj)[c.Header], "%s counts a list that holds a reference", c.Header)
			continue
		}
		obj := authConfigBase(t)
		require.NoError(t, unstructured.SetNestedField(obj, ref, c.Path...), c.Header)
		got := cells(t, obj)
		assert.Empty(t, got[c.Header], "%s shows a reference", c.Header)
		for header, value := range got {
			if header != c.Header && header != "NAME" && header != "AGE" {
				assert.Equal(t, plain[header], value, "%s changed with the reference at %s", header, c.Header)
			}
		}
		shown++
	}
	assert.Equal(t, 6, shown)
}
