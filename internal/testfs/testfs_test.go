package testfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recorder stands in for the test a helper is given. It records what the
// helper did instead of passing it to the real test. A test can then assert
// that a helper skipped or failed. Fatal, Fatalf, FailNow, Skip, Skipf and
// SkipNow stop the helper as the real ones do. That is why run calls the
// helper in a goroutine.
type recorder struct {
	testing.TB
	failed   bool
	skipped  bool
	messages []string
	cleanups []func()
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.failed = true
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}

func (r *recorder) Error(args ...any) {
	r.failed = true
	r.messages = append(r.messages, fmt.Sprint(args...))
}

func (r *recorder) Fail() { r.failed = true }

func (r *recorder) Fatal(args ...any) {
	r.Error(args...)
	r.FailNow()
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	r.FailNow()
}

func (r *recorder) FailNow() {
	r.failed = true
	runtime.Goexit()
}

func (r *recorder) Skip(args ...any) {
	r.messages = append(r.messages, fmt.Sprint(args...))
	r.SkipNow()
}

func (r *recorder) Skipf(format string, args ...any) {
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
	r.SkipNow()
}

func (r *recorder) SkipNow() {
	r.skipped = true
	runtime.Goexit()
}

func (r *recorder) Failed() bool  { return r.failed }
func (r *recorder) Skipped() bool { return r.skipped }

func (r *recorder) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }

func (r *recorder) text() string { return strings.Join(r.messages, "\n") }

// start calls fn with a recorder and waits for it. The cleanups the helper
// registered have not run yet.
func start(t *testing.T, fn func(tb testing.TB)) *recorder {
	t.Helper()
	r := &recorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(r)
	}()
	<-done
	return r
}

// finish runs the registered cleanups in reverse order, as the real test
// does when it ends.
func (r *recorder) finish() {
	for _, f := range slices.Backward(r.cleanups) {
		f()
	}
	r.cleanups = nil
}

// run is start followed by finish.
func run(t *testing.T, fn func(tb testing.TB)) *recorder {
	t.Helper()
	r := start(t, fn)
	r.finish()
	return r
}

func setGOOS(t *testing.T, value string) {
	t.Helper()
	old := goos
	goos = value
	t.Cleanup(func() { goos = old })
}

func setGeteuid(t *testing.T, value int) {
	t.Helper()
	old := geteuid
	geteuid = func() int { return value }
	t.Cleanup(func() { geteuid = old })
}

func setSymlink(t *testing.T, fn func(target, link string) error) {
	t.Helper()
	old := symlink
	symlink = fn
	t.Cleanup(func() { symlink = old })
}

func linkError(err error) error {
	return &os.LinkError{Op: "symlink", Old: "a", New: "b", Err: err}
}

func skipWhenWindowsHost(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(why)
	}
}

// TestOnWindowsFollowsTheOS asserts OnWindows is true for windows and false
// for any other value of the OS name.
func TestOnWindowsFollowsTheOS(t *testing.T) {
	for _, c := range []struct {
		goos string
		want bool
	}{{"windows", true}, {"linux", false}, {"darwin", false}} {
		t.Run(c.goos, func(t *testing.T) {
			setGOOS(t, c.goos)
			assert.Equal(t, c.want, OnWindows())
		})
	}
}

// TestSkipOnWindowsSkipsOnlyOnWindows asserts the skip happens on windows
// with the given reason and never elsewhere.
func TestSkipOnWindowsSkipsOnlyOnWindows(t *testing.T) {
	setGOOS(t, "windows")
	r := run(t, func(tb testing.TB) { tb.Helper(); SkipOnWindows(tb, "a reason") })
	assert.True(t, r.skipped)
	assert.False(t, r.failed)
	assert.Equal(t, "a reason", r.text())

	for _, other := range []string{"linux", "darwin"} {
		setGOOS(t, other)
		r := run(t, func(tb testing.TB) { tb.Helper(); SkipOnWindows(tb, "a reason") })
		assert.False(t, r.skipped, other)
		assert.False(t, r.failed, other)
	}
}

// TestSymlinkSkipsOnlyWhenWindowsRefusesThePrivilege asserts the error for a
// missing symlink privilege skips the test on windows, and every other error
// fails it there and on every other OS.
func TestSymlinkSkipsOnlyWhenWindowsRefusesThePrivilege(t *testing.T) {
	cases := []struct {
		name        string
		goos        string
		err         error
		wantSkipped bool
		wantFailed  bool
	}{
		{name: "windows without the privilege", goos: "windows", err: linkError(errPrivilegeNotHeld), wantSkipped: true},
		{name: "windows with another error", goos: "windows", err: linkError(syscall.EINVAL), wantFailed: true},
		{name: "windows with a plain error", goos: "windows", err: errors.New("boom"), wantFailed: true},
		{name: "linux without the privilege value", goos: "linux", err: linkError(errPrivilegeNotHeld), wantFailed: true},
		{name: "linux with another error", goos: "linux", err: linkError(syscall.EPERM), wantFailed: true},
		{name: "darwin with another error", goos: "darwin", err: linkError(syscall.EPERM), wantFailed: true},
		{name: "windows that works", goos: "windows"},
		{name: "linux that works", goos: "linux"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setGOOS(t, c.goos)
			setSymlink(t, func(string, string) error { return c.err })
			r := run(t, func(tb testing.TB) { tb.Helper(); Symlink(tb, "target", "link") })
			assert.Equal(t, c.wantSkipped, r.skipped, r.text())
			assert.Equal(t, c.wantFailed, r.failed, r.text())
		})
	}
}

// TestSymlinkCreatesTheLink asserts the helper creates a link that points at
// its target on this OS. A Windows runner that refuses symlinks skips the
// test.
func TestSymlinkCreatesTheLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.Mkdir(target, 0o700))
	link := filepath.Join(dir, "link")
	r := run(t, func(tb testing.TB) { tb.Helper(); Symlink(tb, target, link) })
	if r.skipped {
		t.Skip(r.text())
	}
	require.False(t, r.failed, r.text())
	got, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, target, got)
}

// TestRestrictAccessSkipsOnWindowsAndAsRoot asserts the helper skips on
// windows and for root and leaves the mode as it was in either case. The OS
// and the user are forced and the test holds on every host.
func TestRestrictAccessSkipsOnWindowsAndAsRoot(t *testing.T) {
	for _, c := range []struct {
		name string
		goos string
		euid int
		why  string
	}{
		{name: "windows", goos: "windows", euid: 1000, why: "Windows"},
		{name: "root", goos: "linux", euid: 0, why: "root"},
	} {
		t.Run(c.name, func(t *testing.T) {
			setGOOS(t, c.goos)
			setGeteuid(t, c.euid)
			path := filepath.Join(t.TempDir(), "f")
			require.NoError(t, os.WriteFile(path, nil, 0o600))
			before, err := os.Stat(path)
			require.NoError(t, err)
			r := run(t, func(tb testing.TB) { tb.Helper(); RestrictAccess(tb, path, 0o400) })
			assert.True(t, r.skipped)
			assert.False(t, r.failed)
			assert.Contains(t, r.text(), c.why)
			after, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, before.Mode(), after.Mode())
		})
	}
}

// TestRestrictAccessAppliesTheModeAndRestoresIt asserts the mode is in effect
// while the test runs and the previous mode is back once the cleanup ran.
func TestRestrictAccessAppliesTheModeAndRestoresIt(t *testing.T) {
	skipWhenWindowsHost(t, "Windows does not store Unix mode bits")
	setGOOS(t, "linux")
	setGeteuid(t, 1000)
	dir := filepath.Join(t.TempDir(), "d")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.Chmod(dir, 0o700))

	mode := func() os.FileMode {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		return info.Mode().Perm()
	}
	r := start(t, func(tb testing.TB) { tb.Helper(); RestrictAccess(tb, dir, 0o500) })
	require.False(t, r.failed, r.text())
	require.False(t, r.skipped)
	assert.Equal(t, os.FileMode(0o500), mode())
	r.finish()
	assert.Equal(t, os.FileMode(0o700), mode())
}

// TestRestrictAccessFailsOnAMissingPath asserts a path that does not exist
// fails the test and registers no cleanup.
func TestRestrictAccessFailsOnAMissingPath(t *testing.T) {
	setGOOS(t, "linux")
	setGeteuid(t, 1000)
	r := start(t, func(tb testing.TB) { tb.Helper(); RestrictAccess(tb, filepath.Join(t.TempDir(), "gone"), 0o500) })
	assert.True(t, r.failed)
	assert.False(t, r.skipped)
	assert.Empty(t, r.cleanups)
	r.finish()
}

// TestDirectoryInPlaceOfFileMakesAReadFail asserts reading the path as a file
// fails with an error other than not-exist, the parents are created and the
// path is a directory.
func TestDirectoryInPlaceOfFileMakesAReadFail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "facts.json")
	r := run(t, func(tb testing.TB) { tb.Helper(); DirectoryInPlaceOfFile(tb, path) })
	require.False(t, r.failed, r.text())
	_, err := os.ReadFile(path)
	require.Error(t, err)
	require.NotErrorIs(t, err, os.ErrNotExist)
	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	assert.True(t, info.IsDir())
}

// TestSnapshotReportsEveryChange asserts a changed file mode (the write bit),
// size, contents, a created name and a removed name each change the snapshot,
// and an untouched tree gives the same snapshot twice.
func TestSnapshotReportsEveryChange(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sub", "f")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
	require.NoError(t, os.WriteFile(file, []byte("aa"), 0o600))
	before := Snapshot(t, dir)
	require.Equal(t, before, Snapshot(t, dir))

	changes := []struct {
		name  string
		apply func()
		undo  func()
	}{
		{
			name:  "mode",
			apply: func() { require.NoError(t, os.Chmod(file, 0o400)) },
			undo:  func() { require.NoError(t, os.Chmod(file, 0o600)) },
		},
		{
			name:  "contents of the same size",
			apply: func() { require.NoError(t, os.WriteFile(file, []byte("bb"), 0o600)) },
			undo:  func() { require.NoError(t, os.WriteFile(file, []byte("aa"), 0o600)) },
		},
		{
			name:  "size",
			apply: func() { require.NoError(t, os.WriteFile(file, []byte("aaa"), 0o600)) },
			undo:  func() { require.NoError(t, os.WriteFile(file, []byte("aa"), 0o600)) },
		},
		{
			name:  "a created name",
			apply: func() { require.NoError(t, os.WriteFile(filepath.Join(dir, "new"), nil, 0o600)) },
			undo:  func() { require.NoError(t, os.Remove(filepath.Join(dir, "new"))) },
		},
		{
			name:  "a removed name",
			apply: func() { require.NoError(t, os.Remove(file)) },
			undo:  func() { require.NoError(t, os.WriteFile(file, []byte("aa"), 0o600)) },
		},
	}
	for _, c := range changes {
		c.apply()
		assert.NotEqual(t, before, Snapshot(t, dir), c.name)
		c.undo()
	}
}

// TestSnapshotUsesSlashesAndRelativePaths asserts every line starts with the
// slash-separated path relative to the directory.
func TestSnapshotUsesSlashesAndRelativePaths(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "a", "b"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "b", "f"), []byte("x"), 0o600))
	var paths []string
	for _, line := range Snapshot(t, dir) {
		paths = append(paths, strings.Fields(line)[0])
	}
	assert.Equal(t, []string{".", "a", "a/b", "a/b/f"}, paths)
}

// TestSnapshotMissingDirectory asserts a directory that does not exist, and
// one below a plain file, give the one line "missing" on every OS.
func TestSnapshotMissingDirectory(t *testing.T) {
	assert.Equal(t, []string{"missing"}, Snapshot(t, filepath.Join(t.TempDir(), "gone")))

	file := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	assert.Equal(t, []string{"missing"}, Snapshot(t, filepath.Join(file, "below")))
}

// TestSnapshotShowsASymlinkTarget asserts a symlink's line carries its target.
func TestSnapshotShowsASymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	Symlink(t, "first", filepath.Join(dir, "link"))
	var line string
	for _, l := range Snapshot(t, dir) {
		if strings.HasPrefix(l, "link ") {
			line = l
		}
	}
	assert.True(t, strings.HasSuffix(line, " -> first"), line)
}

// TestSnapshotLeavesADirectoryTimeOutOnWindowsOnly asserts a changed
// directory time changes the snapshot unless the OS is windows.
func TestSnapshotLeavesADirectoryTimeOutOnWindowsOnly(t *testing.T) {
	for _, c := range []struct {
		goos      string
		wantEqual bool
	}{{"windows", true}, {"linux", false}} {
		t.Run(c.goos, func(t *testing.T) {
			if !c.wantEqual {
				skipWhenWindowsHost(t, "NTFS may return a stale directory time")
			}
			setGOOS(t, c.goos)
			dir := t.TempDir()
			sub := filepath.Join(dir, "sub")
			require.NoError(t, os.Mkdir(sub, 0o700))
			require.NoError(t, os.Chtimes(sub, time.Unix(1_000_000, 0), time.Unix(1_000_000, 0)))
			before := Snapshot(t, dir)
			require.NoError(t, os.Chtimes(sub, time.Unix(2_000_000, 0), time.Unix(2_000_000, 0)))
			after := Snapshot(t, dir)
			if c.wantEqual {
				assert.Equal(t, before, after)
			} else {
				assert.NotEqual(t, before, after)
			}
		})
	}
}

// TestTreeListsSlashPathsBelowTheDirectory asserts the names are sorted,
// slash-separated, relative and leave the directory itself out.
func TestTreeListsSlashPathsBelowTheDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "b", "c"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b", "c", "f"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), nil, 0o600))
	assert.Equal(t, []string{"a", "b", "b/c", "b/c/f"}, Tree(t, dir))
	assert.Empty(t, Tree(t, t.TempDir()))
}

// TestRequireOwnerOnlyOnUnix asserts a file with no group or other
// permission bit passes and one that a group can read fails.
func TestRequireOwnerOnlyOnUnix(t *testing.T) {
	skipWhenWindowsHost(t, "Windows does not store Unix mode bits")
	setGOOS(t, "linux")
	file := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	for _, c := range []struct {
		name       string
		mode       os.FileMode
		wantFailed bool
	}{
		{name: "owner only", mode: 0o600},
		{name: "group readable", mode: 0o640, wantFailed: true},
		{name: "other readable", mode: 0o604, wantFailed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.NoError(t, os.Chmod(file, c.mode))
			r := run(t, func(tb testing.TB) { tb.Helper(); RequireOwnerOnly(tb, file) })
			assert.Equal(t, c.wantFailed, r.failed, r.text())
		})
	}
}
