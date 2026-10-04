package hwio

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The file functions have the signatures of package os and run with the
// FileDeadline. Their resource is the file system the path is on, as far
// as the path tells: the top directory ("/var", "/config", "/sys") or the
// device ("/dev/sda").

// Resource returns the resource of a path.
func Resource(path string) string {
	p := filepath.Clean(path)
	if !filepath.IsAbs(p) {
		if wd, err := os.Getwd(); err == nil {
			p = filepath.Join(wd, p)
		}
	}
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 3)
	switch {
	case parts[0] == "":
		return "/"
	case parts[0] == "dev" && len(parts) > 1:
		return "/dev/" + parts[1]
	}
	return "/" + parts[0]
}

func deadline(path string) time.Duration {
	switch Resource(path) {
	case "/sys", "/proc":
		return KernelDeadline()
	}
	return FileDeadline()
}

func file[T any](op, path string, fn func() (T, error)) (T, error) {
	return Do(Resource(path), op+" "+path, deadline(path), fn)
}

func fileErr(op, path string, fn func() error) error {
	return DoErr(Resource(path), op+" "+path, deadline(path), fn)
}

func ReadFile(name string) ([]byte, error) {
	return file("read", name, func() ([]byte, error) { return os.ReadFile(name) })
}

func WriteFile(name string, data []byte, perm fs.FileMode) error {
	return fileErr("write", name, func() error { return os.WriteFile(name, data, perm) })
}

func Open(name string) (*os.File, error) {
	return file("open", name, func() (*os.File, error) { return os.Open(name) })
}

func OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	return file("open", name, func() (*os.File, error) { return os.OpenFile(name, flag, perm) })
}

func Create(name string) (*os.File, error) {
	return file("create", name, func() (*os.File, error) { return os.Create(name) })
}

func CreateTemp(dir, pattern string) (*os.File, error) {
	return file("create", dir, func() (*os.File, error) { return os.CreateTemp(dir, pattern) })
}

func MkdirTemp(dir, pattern string) (string, error) {
	return file("mkdir", dir, func() (string, error) { return os.MkdirTemp(dir, pattern) })
}

func Rename(oldpath, newpath string) error {
	return fileErr("rename", newpath, func() error { return os.Rename(oldpath, newpath) })
}

func Remove(name string) error {
	return fileErr("remove", name, func() error { return os.Remove(name) })
}

func RemoveAll(path string) error {
	return fileErr("remove", path, func() error { return os.RemoveAll(path) })
}

func Mkdir(name string, perm fs.FileMode) error {
	return fileErr("mkdir", name, func() error { return os.Mkdir(name, perm) })
}

func MkdirAll(path string, perm fs.FileMode) error {
	return fileErr("mkdir", path, func() error { return os.MkdirAll(path, perm) })
}

func ReadDir(name string) ([]os.DirEntry, error) {
	return file("list", name, func() ([]os.DirEntry, error) { return os.ReadDir(name) })
}

func Stat(name string) (os.FileInfo, error) {
	return file("stat", name, func() (os.FileInfo, error) { return os.Stat(name) })
}

func Lstat(name string) (os.FileInfo, error) {
	return file("stat", name, func() (os.FileInfo, error) { return os.Lstat(name) })
}

func Readlink(name string) (string, error) {
	return file("readlink", name, func() (string, error) { return os.Readlink(name) })
}

func Symlink(oldname, newname string) error {
	return fileErr("symlink", newname, func() error { return os.Symlink(oldname, newname) })
}

func Link(oldname, newname string) error {
	return fileErr("link", newname, func() error { return os.Link(oldname, newname) })
}

func Chmod(name string, mode fs.FileMode) error {
	return fileErr("chmod", name, func() error { return os.Chmod(name, mode) })
}

func Chown(name string, uid, gid int) error {
	return fileErr("chown", name, func() error { return os.Chown(name, uid, gid) })
}

func Lchown(name string, uid, gid int) error {
	return fileErr("chown", name, func() error { return os.Lchown(name, uid, gid) })
}

func Chtimes(name string, atime, mtime time.Time) error {
	return fileErr("chtimes", name, func() error { return os.Chtimes(name, atime, mtime) })
}

func Truncate(name string, size int64) error {
	return fileErr("truncate", name, func() error { return os.Truncate(name, size) })
}

// Glob is filepath.Glob.
func Glob(pattern string) ([]string, error) {
	return file("list", pattern, func() ([]string, error) { return filepath.Glob(pattern) })
}

// EvalSymlinks is filepath.EvalSymlinks.
func EvalSymlinks(path string) (string, error) {
	return file("resolve", path, func() (string, error) { return filepath.EvalSymlinks(path) })
}

// ---- open files ----

// Sync is f.Sync.
func Sync(f *os.File) error {
	return fileErr("sync", f.Name(), f.Sync)
}

// Close is f.Close (closing a file writes what is buffered on some file
// systems).
func Close(f *os.File) error {
	return fileErr("close", f.Name(), f.Close)
}

// SyncDir syncs a directory (after a rename into it).
func SyncDir(dir string) error {
	d, err := Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return Sync(d)
}

// ReadAll reads f to its end, each read with the deadline d (0: FileDeadline).
func ReadAll(f *os.File, d time.Duration) ([]byte, error) {
	return io.ReadAll(Reader(f, d))
}

// File is an open file whose reads and writes run with a deadline.
type File struct {
	F *os.File
	D time.Duration
}

// Reader returns f with a deadline d (0: FileDeadline) on every read.
func Reader(f *os.File, d time.Duration) *File { return &File{F: f, D: d} }

// Writer returns f with a deadline d (0: FileDeadline) on every write.
func Writer(f *os.File, d time.Duration) *File { return &File{F: f, D: d} }

func (g *File) d() time.Duration {
	if g.D > 0 {
		return g.D
	}
	return FileDeadline()
}

func (g *File) Read(p []byte) (int, error) {
	return Do(Resource(g.F.Name()), "read "+g.F.Name(), g.d(), func() (int, error) { return g.F.Read(p) })
}

func (g *File) Write(p []byte) (int, error) {
	return Do(Resource(g.F.Name()), "write "+g.F.Name(), g.d(), func() (int, error) { return g.F.Write(p) })
}

func (g *File) ReadAt(p []byte, off int64) (int, error) {
	return Do(Resource(g.F.Name()), "read "+g.F.Name(), g.d(), func() (int, error) { return g.F.ReadAt(p, off) })
}

func (g *File) WriteAt(p []byte, off int64) (int, error) {
	return Do(Resource(g.F.Name()), "write "+g.F.Name(), g.d(), func() (int, error) { return g.F.WriteAt(p, off) })
}

func (g *File) Seek(off int64, whence int) (int64, error) {
	return Do(Resource(g.F.Name()), "seek "+g.F.Name(), g.d(), func() (int64, error) { return g.F.Seek(off, whence) })
}

func (g *File) Sync() error {
	return DoErr(Resource(g.F.Name()), "sync "+g.F.Name(), g.d(), g.F.Sync)
}

// Close closes the file; when its resource is stuck, the descriptor is
// closed in the background once the resource answers again.
func (g *File) Close() error {
	err := DoErr(Resource(g.F.Name()), "close "+g.F.Name(), g.d(), g.F.Close)
	var e *Error
	if errors.As(err, &e) && e.After == 0 {
		go g.F.Close()
	}
	return err
}

// WriteFileAtomic writes data to path through a synced temporary file
// (path.tmp) and a rename, then syncs the directory: after a crash the
// file is either the old or the new one. One deadline covers all of it.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return fileErr("write", path, func() error {
		tmp := path + ".tmp"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
		d, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	})
}
