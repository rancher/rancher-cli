package observe

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// ArchiveEntry is one file to write into the support-bundle tar.gz.
type ArchiveEntry struct {
	// Path is the entry's name within the archive, such as "manifest.json" or
	// "logs/c-1/agent/pod-1-main.log". Only entryPath and staticEntryPath make
	// one, and both accept nothing but plain file names.
	Path entryName
	Data []byte
}

// ArchiveWriter streams entries into a gzip-compressed tar one at a time,
// so a collector holds at most the one entry currently in flight rather
// than the whole collection - a fleet-wide bundle's memory footprint stays
// bounded by the largest single entry, not the sum of everything collected.
type ArchiveWriter struct {
	file   *os.File
	gz     *gzip.Writer
	tw     *tar.Writer
	path   string
	total  int64
	err    error
	closed bool
}

// CreateArchive opens path with O_CREATE|O_EXCL and 0600 and returns a
// writer ready for Add. An existing file - or a symlink, which O_EXCL
// refuses since the link itself exists - is VALIDATION naming the path,
// never truncated or followed.
func CreateArchive(path string) (*ArchiveWriter, error) {
	return createArchiveWith(path, func(w io.Writer) io.Writer { return w })
}

// archiveExistsError is the VALIDATION error for an archive path that is
// already taken.
func archiveExistsError(path string) error {
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("%s already exists; choose a different --output-file path, or remove the existing file", path))
}

// refuseExistingArchive returns the error CreateArchive gives when anything
// exists at path, a symlink included (dangling or not), so a command can
// refuse before it sends a request. Only a successful Lstat refuses; any
// other error is left for CreateArchive to report. CreateArchive stays the
// authority when the path appears after this check.
func refuseExistingArchive(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return archiveExistsError(path)
	}
	return nil
}

// createArchiveWith is the injectable core of CreateArchive; wrap lets
// tests substitute a failing io.Writer around the underlying file without
// mutating package-level state.
func createArchiveWith(path string, wrap func(io.Writer) io.Writer) (*ArchiveWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if os.IsExist(err) {
			return nil, archiveExistsError(path)
		}
		return nil, err
	}
	gz := gzip.NewWriter(wrap(f))
	return &ArchiveWriter{file: f, gz: gz, tw: tar.NewWriter(gz), path: path}, nil
}

// Add streams one entry into the archive immediately. Once a write has
// failed, every later Add is a no-op returning that same error - a collector
// can keep calling Add without checking every return and let Close report and
// clean up the failure. A name that is not a sequence of plain file names
// (the zero entryName is one) is returned as an error and does not become
// that error: nothing is written for it, later entries are still written
// and Close does not fail.
func (w *ArchiveWriter) Add(entry ArchiveEntry) error {
	if w.err != nil {
		return w.err
	}
	if err := entry.Path.check(); err != nil {
		return err
	}
	if err := w.tw.WriteHeader(&tar.Header{Name: entry.Path.entryText, Mode: 0600, Size: int64(len(entry.Data))}); err != nil {
		w.err = err
		return err
	}
	if _, err := w.tw.Write(entry.Data); err != nil {
		w.err = err
		return err
	}
	w.total += int64(len(entry.Data))
	return nil
}

// Close finalizes the tar/gzip streams and the file. A failure here or from
// an earlier Add removes the partially written file: this invocation
// exclusively created it via O_EXCL, so nothing else can be holding it, and
// leaving a truncated file behind would permanently block every retry at
// the same path. Returns the total uncompressed content size.
func (w *ArchiveWriter) Close() (int64, error) {
	// A repeated Close must report the first call's outcome without
	// re-closing the writers: closing the finalized tar stream errors, and
	// treating that error as a write failure would remove a valid archive.
	if w.closed {
		if w.err != nil {
			return 0, w.err
		}
		return w.total, nil
	}
	w.closed = true
	if w.err == nil {
		w.err = w.tw.Close()
	}
	if w.err == nil {
		w.err = w.gz.Close()
	}
	if closeErr := w.file.Close(); w.err == nil {
		w.err = closeErr
	}
	if w.err != nil {
		_ = os.Remove(w.path)
		return 0, w.err
	}
	return w.total, nil
}

// Discard abandons the archive after a failure unrelated to Add or Close -
// a caller that cannot finish assembling the entries it already streamed -
// closing the underlying handles and removing the partial file.
func (w *ArchiveWriter) Discard() {
	_ = w.tw.Close()
	_ = w.gz.Close()
	_ = w.file.Close()
	_ = os.Remove(w.path)
}
