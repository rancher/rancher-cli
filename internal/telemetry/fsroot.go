package telemetry

import (
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"
)

// root wraps an os.Root. Every file operation of the package goes through
// it; each one is seen by the observer seam and by the read hook, and
// none can reach a path outside the directory the root was opened on. A
// source scan asserts that no other file calls the os package's path
// functions, os.Root, or FS. The observer is told "<operation> <path>",
// the path relative to this root.
type root struct {
	inner *os.Root
}

func observeOp(op, name string) {
	if observer != nil {
		observer(op + " " + name)
	}
}

// DirExists reports whether path is a directory the caller can stat. Any
// other result, an error included, is false. It reports one stat operation
// to the observer and reads nothing from the directory.
func DirExists(path string) bool {
	observeOp("stat", path)
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// openRoot opens a directory by its path.
func openRoot(dir string) (*root, error) {
	observeOp("openroot", ".")
	in, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &root{inner: in}, nil
}

// OpenRoot opens a directory below this root.
func (r *root) OpenRoot(name string) (*root, error) {
	observeOp("openroot", name)
	in, err := r.inner.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	return &root{inner: in}, nil
}

// Close releases the root. Closing a root is not an observed operation.
func (r *root) Close() error { return r.inner.Close() }

func (r *root) Stat(name string) (fs.FileInfo, error) {
	observeOp("stat", name)
	return r.inner.Stat(name)
}

func (r *root) Lstat(name string) (fs.FileInfo, error) {
	observeOp("lstat", name)
	return r.inner.Lstat(name)
}

func (r *root) Mkdir(name string, perm fs.FileMode) error {
	observeOp("mkdir", name)
	return r.inner.Mkdir(name, perm)
}

func (r *root) MkdirAll(name string, perm fs.FileMode) error {
	observeOp("mkdirall", name)
	return r.inner.MkdirAll(name, perm)
}

func (r *root) Remove(name string) error {
	observeOp("remove", name)
	if removeFileHook != nil {
		if err := removeFileHook(name); err != nil {
			return err
		}
	}
	return r.inner.Remove(name)
}

func (r *root) RemoveAll(name string) error {
	observeOp("removeall", name)
	if removeAllHook != nil {
		if err := removeAllHook(name); err != nil {
			return err
		}
	}
	return r.inner.RemoveAll(name)
}

func (r *root) Rename(oldname, newname string) error {
	observeOp("rename", oldname)
	if renameHook != nil {
		if err := renameHook(oldname, newname); err != nil {
			return err
		}
	}
	return r.inner.Rename(oldname, newname)
}

// Link creates newname as a hard link to the file oldname. It never replaces
// an existing newname: the error then matches fs.ErrExist.
func (r *root) Link(oldname, newname string) error {
	observeOp("link", newname)
	if linkHook != nil {
		if err := linkHook(oldname, newname); err != nil {
			return err
		}
	}
	return r.inner.Link(oldname, newname)
}

func (r *root) Chtimes(name string, atime, mtime time.Time) error {
	observeOp("chtimes", name)
	return r.inner.Chtimes(name, atime, mtime)
}

// ReadFile reads a whole file.
func (r *root) ReadFile(name string) ([]byte, error) {
	observeOp("readfile", name)
	if readHook != nil {
		if err := readHook(name); err != nil {
			return nil, err
		}
	}
	return r.inner.ReadFile(name)
}

// ReadFileMax reads a file but at most max bytes into memory. It reports
// true when the file is longer than max, with no data.
func (r *root) ReadFileMax(name string, limit int64) (data []byte, tooLarge bool, err error) {
	observeOp("readfile", name)
	if readHook != nil {
		if err := readHook(name); err != nil {
			return nil, false, err
		}
	}
	f, err := r.inner.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	data, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return nil, true, nil
	}
	return data, false, nil
}

// ReadDir lists a directory, sorted by name.
func (r *root) ReadDir(name string) ([]fs.DirEntry, error) {
	observeOp("readdir", name)
	f, err := r.inner.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}

// OpenFile opens a file; the result observes its Write, Sync and Close.
func (r *root) OpenFile(name string, flag int, perm fs.FileMode) (*file, error) {
	observeOp("openfile", name)
	f, err := r.inner.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &file{f: f, name: name}, nil
}

// file is an open file whose operations the observer sees.
type file struct {
	f    *os.File
	name string
}

func (f *file) Write(p []byte) (int, error) {
	observeOp("write", f.name)
	return f.f.Write(p)
}

func (f *file) Sync() error {
	observeOp("sync", f.name)
	if syncHook != nil {
		if err := syncHook(f.name); err != nil {
			return err
		}
	}
	return f.f.Sync()
}

func (f *file) Close() error {
	observeOp("close", f.name)
	return f.f.Close()
}
