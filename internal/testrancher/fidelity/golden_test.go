package fidelity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoldenPathMapsFamilySlashesToDirs(t *testing.T) {
	t.Parallel()
	got := GoldenPath(filepath.Join("testdata", "oracle"), "auth/no-token")
	assert.Equal(t, filepath.Join("testdata", "oracle", "auth", "no-token.json"), got)
}

func TestGoldenRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	g := Golden{
		OracleImage: "rancher/rancher:v2.14.0@sha256:abc",
		Observations: []ObservationResult{
			{Name: "get", Status: 401, ContentType: "application/json", Body: map[string]any{"type": "error"}},
		},
	}
	path := GoldenPath(dir, "auth/no-token")
	require.NoError(t, SaveGolden(path, g))
	back, err := LoadGolden(path)
	require.NoError(t, err)
	assert.Equal(t, g, back)
}

func TestLoadGoldenMissingFileWrapsNotExist(t *testing.T) {
	t.Parallel()
	_, err := LoadGolden(filepath.Join(t.TempDir(), "nope.json"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDiffReportsFieldLevelMismatches(t *testing.T) {
	t.Parallel()
	base := Golden{Observations: []ObservationResult{
		{Name: "get", Status: 403, ContentType: "application/json", Body: map[string]any{"code": "Forbidden"}},
	}}
	same := Golden{OracleImage: "different-image-is-ignored", Observations: base.Observations}
	assert.Empty(t, Diff(base, same))

	status := Golden{Observations: []ObservationResult{
		{Name: "get", Status: 401, ContentType: "application/json", Body: map[string]any{"code": "Forbidden"}},
	}}
	diffs := Diff(base, status)
	require.Len(t, diffs, 1)
	assert.Contains(t, diffs[0], "status")

	count := Golden{Observations: nil}
	diffs = Diff(base, count)
	require.Len(t, diffs, 1)
	assert.Contains(t, diffs[0], "observation count")
}

func TestDiffReportsBodyNameAndContentTypeMismatches(t *testing.T) {
	t.Parallel()
	base := Golden{Observations: []ObservationResult{
		{Name: "get", Status: 200, ContentType: "application/json", Body: map[string]any{"k": "v1"}},
	}}

	body := Golden{Observations: []ObservationResult{
		{Name: "get", Status: 200, ContentType: "application/json", Body: map[string]any{"k": "v2"}},
	}}
	diffs := Diff(base, body)
	require.Len(t, diffs, 1)
	assert.Contains(t, diffs[0], "get body", "the body diff names the observation")
	assert.Contains(t, diffs[0], `"v1"`, "the want body is shown")
	assert.Contains(t, diffs[0], `"v2"`, "the got body is shown")

	name := Golden{Observations: []ObservationResult{
		{Name: "other", Status: 200, ContentType: "application/json", Body: map[string]any{"k": "v1"}},
	}}
	diffs = Diff(base, name)
	require.Len(t, diffs, 1, "a name mismatch short-circuits the remaining field checks")
	assert.Contains(t, diffs[0], "name")
	assert.Contains(t, diffs[0], `"get"`)
	assert.Contains(t, diffs[0], `"other"`)

	ct := Golden{Observations: []ObservationResult{
		{Name: "get", Status: 200, ContentType: "text/plain", Body: map[string]any{"k": "v1"}},
	}}
	diffs = Diff(base, ct)
	require.Len(t, diffs, 1)
	assert.Contains(t, diffs[0], "contentType")
}
