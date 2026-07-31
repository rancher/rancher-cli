package observe

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// ArchiveEntry is one file to write into the support-bundle tar.gz.
type ArchiveEntry struct {
	Path string // path within the archive, e.g. "manifest.json", "logs/c-1/agent/pod-1-main.log"
	Data []byte
}

// writeEntries writes sorted entries as a gzip-compressed tar to w,
// returning the total uncompressed content size.
func writeEntries(w io.Writer, entries []ArchiveEntry) (int64, error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	var total int64
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.Path, Mode: 0600, Size: int64(len(e.Data))}); err != nil {
			return total, err
		}
		if _, err := tw.Write(e.Data); err != nil {
			return total, err
		}
		total += int64(len(e.Data))
	}
	if err := tw.Close(); err != nil {
		return total, err
	}
	if err := gz.Close(); err != nil {
		return total, err
	}
	return total, nil
}

// WriteArchive creates path with O_CREATE|O_EXCL and 0600, then writes
// entries as a gzip-compressed tar sorted by Path for a deterministic
// layout. An existing file — or a symlink, which O_EXCL refuses since the
// link itself exists — is VALIDATION naming the path, never truncated or
// followed.
//
// A failure ANYWHERE on the write path (tar/gzip write or close) removes
// the file before returning: this invocation exclusively created it via
// O_EXCL, so nothing else can be holding it, and leaving a truncated file
// behind would make the O_EXCL refusal above permanently block every retry
// at the same path. Returns the total uncompressed content size.
func WriteArchive(path string, entries []ArchiveEntry) (int64, error) {
	return writeArchiveWith(path, entries, writeEntries)
}

// writeArchiveWith is the injectable core of WriteArchive; the write function
// is passed explicitly so tests can inject a failing writer without mutating
// package-level state.
func writeArchiveWith(path string, entries []ArchiveEntry, write func(io.Writer, []ArchiveEntry) (int64, error)) (int64, error) {
	sorted := append([]ArchiveEntry{}, entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if os.IsExist(err) {
			return 0, clierr.New(clierr.CodeValidation, fmt.Sprintf("%s already exists", path)).
				WithHint("choose a different -f path, or remove the existing file")
		}
		return 0, err
	}

	total, writeErr := write(f, sorted)
	closeErr := f.Close()

	if writeErr != nil {
		_ = os.Remove(path)
		return 0, writeErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return 0, closeErr
	}
	return total, nil
}
