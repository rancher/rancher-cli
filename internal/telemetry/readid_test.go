package telemetry

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// snapshot lists every path under dir with its mode, size, modification time
// (a directory's only off Windows) and contents, so a test sees any change a
// read should not make.
func snapshot(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		info, err := d.Info()
		require.NoError(t, err)
		rel, _ := filepath.Rel(dir, p)
		// NTFS updates a directory's modification time some time after a
		// child is created or closed, so a snapshot taken right after the
		// setup can read the old value; on Windows a directory's time is
		// left out, and the names below it still show a create or a remove.
		mtime := info.ModTime().UnixNano()
		if d.IsDir() && runtime.GOOS == "windows" {
			mtime = 0
		}
		line := fmt.Sprintf("%s %v %d %d", rel, info.Mode(), info.Size(), mtime)
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(p)
			require.NoError(t, err)
			line += " " + string(data)
		}
		out = append(out, line)
		return nil
	}))
	slices.Sort(out)
	return out
}

// TestReadDirectoryIDNeverWrites asserts the read-only id reader reports the
// id of a valid file and absent for every other state, without a panic and
// without changing, creating or removing anything.
func TestReadDirectoryIDNeverWrites(t *testing.T) {
	valid := testUUID()
	cases := []struct {
		name    string
		prepare func(t *testing.T, dir string)
		want    string
		ok      bool
	}{
		{name: "no telemetry directory", prepare: func(*testing.T, string) {}},
		{name: "no id file", prepare: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.Mkdir(filepath.Join(dir, "telemetry"), 0o700))
		}},
		{name: "empty file", prepare: writeID("")},
		{name: "partial file", prepare: writeID(valid[:20])},
		{name: "invalid text", prepare: writeID("not an id")},
		{name: "uppercase id", prepare: writeID("AAAAAAAA-AAAA-4AAA-AAAA-AAAAAAAAAAAA")},
		{name: "too long", prepare: writeID(valid + valid + valid + valid + valid + valid + valid + valid + valid + valid)},
		{name: "id is a directory", prepare: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "telemetry", "config-directory-id"), 0o700))
		}},
		{name: "valid id", prepare: writeID(valid), want: valid, ok: true},
		{name: "valid id with a newline", prepare: writeID(valid + "\n"), want: valid, ok: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			c.prepare(t, dir)
			before := snapshot(t, dir)
			s := openStore(t, dir)
			got, ok := s.ReadDirectoryID()
			assert.Equal(t, c.ok, ok)
			assert.Equal(t, c.want, got)
			assert.Equal(t, before, snapshot(t, dir))
		})
	}
}

func writeID(text string) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		t.Helper()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "telemetry"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry", "config-directory-id"), []byte(text), 0o600))
	}
}
