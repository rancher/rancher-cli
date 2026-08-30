package observe

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
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

func addAll(t *testing.T, w *ArchiveWriter, entries []ArchiveEntry) {
	t.Helper()
	for _, e := range entries {
		require.NoError(t, w.Add(e))
	}
}

func TestArchiveWriterRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.tar.gz")

	w, err := CreateArchive(path)
	require.NoError(t, err)
	entries := []ArchiveEntry{
		{Path: "logs/c-1/agent/pod-1.log", Data: []byte("agent log")},
		{Path: "manifest.json", Data: []byte(`{"ok":true}`)},
		{Path: "clusters/c-1/cluster.json", Data: []byte(`{"state":"active"}`)},
	}
	addAll(t, w, entries)
	total, err := w.Close()
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

func TestCreateArchiveRefusesExistingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.tar.gz")
	require.NoError(t, os.WriteFile(path, []byte("pre-existing"), 0600))

	_, err := CreateArchive(path)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, path)

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "pre-existing", string(b), "an existing file must never be truncated")
}

func TestCreateArchiveRefusesSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.tar.gz")
	require.NoError(t, os.Symlink(filepath.Join(dir, "nowhere"), path))

	_, err := CreateArchive(path)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	target, err := os.Readlink(path)
	require.NoError(t, err, "the symlink itself must survive — O_EXCL refuses it, never follows it")
	assert.Equal(t, filepath.Join(dir, "nowhere"), target)
}

type failingWriter struct{ err error }

func (fw failingWriter) Write([]byte) (int, error) { return 0, fw.err }

// TestArchiveWriterRemovesFileOnWriteFailure pins the O_EXCL cleanup: a
// failure anywhere on the write path (gzip buffers small writes, so the
// injected failure may surface from Add or only once Close flushes) must
// not leave a truncated file behind, since that would block every retry at
// the same path via the existing-file refusal above. A failing underlying
// writer is injected via createArchiveWith to exercise the cleanup without
// mutating package state.
func TestArchiveWriterRemovesFileOnWriteFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.tar.gz")

	injected := errors.New("disk exploded")
	w, err := createArchiveWith(path, func(io.Writer) io.Writer { return failingWriter{err: injected} })
	require.NoError(t, err)

	addErr := w.Add(ArchiveEntry{Path: "manifest.json", Data: []byte("{}")})
	_, closeErr := w.Close()
	if addErr == nil {
		addErr = closeErr
	}
	require.ErrorIs(t, addErr, injected)

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "a failed write must remove the file it exclusively created")

	w2, err := CreateArchive(path)
	require.NoError(t, err, "the same path must be writable again once the failed attempt's file is gone")
	require.NoError(t, w2.Add(ArchiveEntry{Path: "manifest.json", Data: []byte("{}")}))
	total, err := w2.Close()
	require.NoError(t, err)
	assert.Positive(t, total)
}

func TestArchiveWriterEmptyEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.tar.gz")

	w, err := CreateArchive(path)
	require.NoError(t, err)
	total, err := w.Close()
	require.NoError(t, err)
	assert.Equal(t, int64(0), total)
	assert.Empty(t, readArchive(t, path))
}

// TestArchiveWriterStreamsBeforeClose is the memory-bound proof: entries
// reach disk as Add is called, not all at once on Close, so a collector
// feeding this writer never needs to hold the whole collection in memory —
// only the one entry it is currently adding.
func TestArchiveWriterStreamsBeforeClose(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "large.tar.gz")

	w, err := CreateArchive(path)
	require.NoError(t, err)

	const (
		entryCount = 40
		entrySize  = 1 << 20 // 1 MiB; repeated bytes compress to nearly nothing.
	)
	var sizeAfterFirst int64
	for i := range entryCount {
		require.NoError(t, w.Add(ArchiveEntry{
			Path: fmt.Sprintf("entry-%03d", i),
			Data: bytes.Repeat([]byte{byte(i)}, entrySize),
		}))
		require.NoError(t, w.gz.Flush())
		if i == 0 {
			info, err := os.Stat(path)
			require.NoError(t, err)
			sizeAfterFirst = info.Size()
		}
	}

	total, err := w.Close()
	require.NoError(t, err)
	assert.Equal(t, int64(entryCount*entrySize), total)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Positive(t, sizeAfterFirst,
		"bytes must reach disk after the first Add, before Close")
	assert.Greater(t, info.Size(), sizeAfterFirst,
		"the archive file must grow as entries stream in, not appear all at once on Close")
}

func TestArchiveWriterCloseTwiceKeepsArchive(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	w, err := CreateArchive(path)
	require.NoError(t, err)
	require.NoError(t, w.Add(ArchiveEntry{Path: "a.txt", Data: []byte("payload")}))

	total, err := w.Close()
	require.NoError(t, err)

	againTotal, againErr := w.Close()
	require.NoError(t, againErr, "a repeated Close must report the first outcome")
	assert.Equal(t, total, againTotal)
	assert.FileExists(t, path, "a repeated Close must never remove the finished archive")
}
