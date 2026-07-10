package observe

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"os"
	"sort"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// ArchiveEntry is one file to write into the support-bundle tar.gz.
type ArchiveEntry struct {
	Path string // path within the archive, e.g. "manifest.json", "logs/c-1/agent/pod-1-main.log"
	Data []byte
}

// WriteArchive creates path with O_CREATE|O_EXCL and 0600, then writes
// entries as a gzip-compressed tar sorted by Path for a deterministic
// layout. An existing file — or a symlink, which O_EXCL refuses since the
// link itself exists — is VALIDATION naming the path, never truncated or
// followed. Returns the total uncompressed content size.
func WriteArchive(path string, entries []ArchiveEntry) (int64, error) {
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
	defer func() { _ = f.Close() }()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	var total int64
	for _, e := range sorted {
		if err := tw.WriteHeader(&tar.Header{Name: e.Path, Mode: 0600, Size: int64(len(e.Data))}); err != nil {
			return 0, err
		}
		if _, err := tw.Write(e.Data); err != nil {
			return 0, err
		}
		total += int64(len(e.Data))
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	return total, nil
}
