// Package rundir opens and writes files on the host, in a dir that a sandbox
// can write. Its first user is the per-session run dir,
// $XDG_STATE_HOME/prism/run/<hash>/ (#3373).
//
// The sandbox of a session can write its run dir (the bwrap bind, the
// section-6 rule of the sandbox-exec profile). So the session can remove a
// file there and put a symlink, a FIFO, or a dir at its path. A host-side
// open that follows such a symlink writes or reads a file that the session
// chose, for example a startup file of the shell of the user.
//
// Each function here refuses a symlink at the path, and each write also
// refuses a dir of the path that is a symlink: O_NOFOLLOW checks only the
// last component of a path. The functions do not check the ancestors of that
// dir. Use them only for a dir whose parent no sandbox can replace. For the
// run dir, the sandbox-exec profile stops a replace of the node (section 21m
// of generateProfile), in bwrap the node is a mount point, and no sandbox can
// write the parent.
package rundir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ErrSymlinkDir is wrapped by the error for a dir that is a symlink.
var ErrSymlinkDir = errors.New("the dir is a symlink")

// ErrNotRegular is wrapped by the error for a path that is not a regular
// file.
var ErrNotRegular = errors.New("not a regular file")

// ErrSymlink is wrapped by the error for a path that is a symlink.
var ErrSymlink = errors.New("the path is a symlink")

// RefuseSymlink returns an error that wraps ErrSymlink when path is a
// symlink, and an error that wraps ErrSymlinkDir when the dir of path is a
// symlink. The error names the path. It returns nil for a missing path.
//
// A connect to a Unix socket follows a symlink, and no flag stops that. So a
// host-side bind or connect at a path in the run dir calls this first
// (#3379). The check and the connect are two steps: a sandbox that can
// write the dir can put a symlink between them. It is a second layer, and
// the host-API credential is the first.
func RefuseSymlink(path string) error {
	if err := RefuseSymlinkDir(filepath.Dir(path)); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("refusing %s: %w", path, ErrSymlink)
	}
	return nil
}

// RefuseSymlinkDir returns an error that wraps ErrSymlinkDir when dir is a
// symlink. It makes nothing, and it returns nil for a missing dir.
func RefuseSymlinkDir(dir string) error {
	if info, err := os.Lstat(dir); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write in %s: %w", dir, ErrSymlinkDir)
	}
	return nil
}

// EnsureDir makes dir, and each missing ancestor, with mode 0700 when dir
// does not exist. It does not change the mode of a dir that exists. It
// returns an error that wraps ErrSymlinkDir when dir is a symlink, and an
// error when dir is not a directory. It checks only dir, not its ancestors.
func EnsureDir(dir string) error {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("make dir: %w", err)
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("dir: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write in %s: %w", dir, ErrSymlinkDir)
	}
	if !info.IsDir() {
		return fmt.Errorf("refusing to write in %s: not a directory (mode %v)", dir, info.Mode())
	}
	return nil
}

// OpenAppend opens path to append, and makes it with mode 0600 when it does
// not exist. It refuses a symlink at path (the error wraps syscall.ELOOP), a
// path that is not a regular file, and a dir of path that EnsureDir refuses.
//
// O_NONBLOCK stops the open from a wait on a FIFO at path. It has no effect
// on a write to a regular file.
func OpenAppend(path string) (*os.File, error) {
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	if err := requireRegular(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// OpenRead opens path to read. It refuses a symlink at path (the error wraps
// syscall.ELOOP) and a path that is not a regular file. A log reader uses it,
// so that the host does not read and pass on a file that the session chose.
func OpenRead(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := requireRegular(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// WriteFile writes data to path with mode 0600, and replaces the file that
// is there. It makes the dir of path with EnsureDir, and refuses a dir that
// EnsureDir refuses.
//
// The data goes to a new temp file in the dir (random name, O_EXCL), and a
// rename puts it at path. The rename replaces a symlink at path. It does not
// follow it, so the write cannot truncate or change another file. The rename
// fails when path is a dir. A reader of path sees the old file or the new
// file, never a part of the new file. On an error, the file at path does not
// change, and the temp file is removed.
func WriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := EnsureDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("temp file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Rename(tmpPath, path)
	}
	if writeErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write %s: %w", path, writeErr)
	}
	return nil
}

func requireRegular(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing %s (mode %v): %w", f.Name(), info.Mode(), ErrNotRegular)
	}
	return nil
}
