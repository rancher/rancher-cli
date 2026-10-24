package telemetrycmd

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	rt "github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/ui"
)

// controlsEnv lets recording run in the test binary and clears the variable,
// so the developer's shell cannot change a result. A test that sets the
// variable does so after this call. Not for a parallel test.
func controlsEnv(t *testing.T) {
	t.Helper()
	t.Cleanup(telemetry.SetEnabledForTest())
	t.Setenv(telemetry.EnvTelemetry, "")
}

// execute runs a command with no arguments.
func execute(t *testing.T, cmd rt.Command) (rt.Result, error) {
	t.Helper()
	return cmd.(rt.ReadCommand).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
}

// newDir returns a fresh config directory path that does not exist yet.
func newDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config")
}

// writeConfig writes config.yaml with text, creating the directory.
func writeConfig(t *testing.T, dir, text string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(text), 0o600))
}

func readConfig(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	return string(b)
}

// uidOf returns a lowercase version 4 UUID unique to n.
func uidOf(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// eventJSON is a stored event with the structural minimum and a command.
func eventJSON(n int, hour, command, outcome string, exit int) string {
	return fmt.Sprintf(`{"event.name":"io.rancher.cli.command.run","time":"%s","schema_version":"1.0.0",`+
		`"log.record.uid":"%s","io.rancher.cli.command":"%s","io.rancher.cli.command.outcome":"%s","process.exit.code":%d,"local_format":1}`,
		hour, uidOf(n), command, outcome, exit)
}

// hourDirName is the directory name of a UTC hour.
func hourDirName(h time.Time) string { return h.UTC().Format("2006010215") }

// putFile writes a file below dir, creating its directories.
func putFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
}

// seedStore writes two events, a facts file and an id below dir/telemetry,
// and a prune marker.
func seedStore(t *testing.T, dir string) {
	t.Helper()
	h1 := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	h2 := time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC)
	putFile(t, dir, "telemetry/events/"+hourDirName(h1)+"/0001.json", eventJSON(1, h1.Format(time.RFC3339), "get project", "ok", 0))
	putFile(t, dir, "telemetry/events/"+hourDirName(h2)+"/0002.json", eventJSON(2, h2.Format(time.RFC3339), "auth login", "error", 1))
	putFile(t, dir, "telemetry/servers/0123456789abcdef0123456789abcdef.json", `{"version":"2.15.2"}`)
	putFile(t, dir, "telemetry/config-directory-id", "11111111-1111-4111-8111-111111111111")
	putFile(t, dir, "telemetry/pruned", "")
}

// snapshot lists every path under dir with its mode, size, modification time
// (a directory's only off Windows) and contents, so a test sees any change a
// command should not make. A missing dir gives a one-line snapshot.
func snapshot(t *testing.T, dir string) []string {
	t.Helper()
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return []string{"missing"}
	}
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
		line := fmt.Sprintf("%s %v %d %d", rel, info.Mode().Type(), info.Size(), mtime)
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

// names lists the paths below dir, relative and sorted; empty for a
// missing dir.
func names(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel != "." {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	slices.Sort(out)
	return out
}

func skipOnWindows(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(why)
	}
}

// dataMap returns a result's Data as the map the commands build.
func dataMap(t *testing.T, res rt.Result) map[string]any {
	t.Helper()
	m, ok := res.Data.(map[string]any)
	require.True(t, ok, "Data is %T", res.Data)
	return m
}
