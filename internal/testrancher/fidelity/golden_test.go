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
