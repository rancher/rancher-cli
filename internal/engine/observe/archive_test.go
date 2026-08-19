package observe

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readArchive(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	tr := tar.NewReader(gz)

	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		b, err := io.ReadAll(tr)
		require.NoError(t, err)
		out[hdr.Name] = string(b)
	}
	return out
}

func TestWriteArchiveDeterministicLayout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.tar.gz")

	entries := []ArchiveEntry{
		{Path: "logs/c-1/agent/pod-1.log", Data: []byte("agent log")},
		{Path: "manifest.json", Data: []byte(`{"ok":true}`)},
		{Path: "clusters/c-1/cluster.json", Data: []byte(`{"state":"active"}`)},
	}

	total, err := WriteArchive(path, entries)
	require.NoError(t, err)
	assert.Equal(t, int64(len("agent log")+len(`{"ok":true}`)+len(`{"state":"active"}`)), total)

	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		// On Windows os.FileInfo.Mode() synthesizes mode bits regardless of
		// the real NTFS DACL (hardenACL is the enforcement there), so the
		// 0600 assertion is meaningless on that platform.
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}

	contents := readArchive(t, path)
	assert.Equal(t, "agent log", contents["logs/c-1/agent/pod-1.log"])
	assert.Equal(t, `{"ok":true}`, contents["manifest.json"])
	assert.JSONEq(t, `{"state":"active"}`, contents["clusters/c-1/cluster.json"])
}

func TestWriteArchiveRefusesExistingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.tar.gz")
	require.NoError(t, os.WriteFile(path, []byte("pre-existing"), 0600))

	_, err := WriteArchive(path, []ArchiveEntry{{Path: "manifest.json", Data: []byte("{}")}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, path)

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "pre-existing", string(b), "an existing file must never be truncated")
}

func TestWriteArchiveRefusesSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.tar.gz")
	require.NoError(t, os.Symlink(filepath.Join(dir, "nowhere"), path))

	_, err := WriteArchive(path, []ArchiveEntry{{Path: "manifest.json", Data: []byte("{}")}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	target, err := os.Readlink(path)
	require.NoError(t, err, "the symlink itself must survive — O_EXCL refuses it, never follows it")
	assert.Equal(t, filepath.Join(dir, "nowhere"), target)
}

// TestWriteArchiveRemovesFileOnMidWriteFailure pins the O_EXCL cleanup: a
// failure on the write path (tar/gzip write or close) must not leave a
// truncated file behind, since that would block every retry at the same
// path via the existing-file refusal above. A failing writer is injected
// via writeArchiveWith to exercise the cleanup without mutating package state.
func TestWriteArchiveRemovesFileOnMidWriteFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.tar.gz")

	injected := errors.New("disk exploded")
	_, err := writeArchiveWith(path, []ArchiveEntry{{Path: "manifest.json", Data: []byte("{}")}},
		func(io.Writer, []ArchiveEntry) (int64, error) { return 0, injected })
	require.ErrorIs(t, err, injected)

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "a failed write must remove the file it exclusively created")

	total, err := WriteArchive(path, []ArchiveEntry{{Path: "manifest.json", Data: []byte("{}")}})
	require.NoError(t, err, "the same path must be writable again once the failed attempt's file is gone")
	assert.Positive(t, total)
}

func TestWriteArchiveEmptyEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.tar.gz")

	total, err := WriteArchive(path, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(0), total)
	assert.Empty(t, readArchive(t, path))
}
