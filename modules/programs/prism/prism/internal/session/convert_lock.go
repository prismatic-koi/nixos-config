package session

// convert_lock.go — the lock that keeps a session from a start in a dir
// while prism makes a .bare in it: a conversion of a plain clone to the
// bare+worktree layout (#3361), or a prism clone (#3388, clone_lock.go).
//
// A session in the dir can write the dir. prism makes <dir>/.bare there and
// runs host git in it. So the conversion or the clone holds an exclusive
// lock on the dir until it ends. Create holds a shared lock on the dir for
// the whole start. Thus a start that runs when the conversion or the clone
// asks for the lock ends first, and the conversion stops that session, or
// the clone refuses. A start during the conversion or the clone is refused.
// A start after it is refused too, because the dir then holds a .bare.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/sessionname"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// ErrConvertInProgress is in the error chain of Create when prism converts
// the dir of the session to the bare+worktree layout, or clones a repo
// into it.
var ErrConvertInProgress = errors.New("prism converts this dir to the bare+worktree layout, or clones a repo into it, so no session can start in it until that ends")

// ErrStartInBareRoot is in the error chain of Create when the dir of the
// session holds a .bare. A session in that dir can write the .bare.
var ErrStartInBareRoot = errors.New("the dir holds a .bare, so it is the root of a bare+worktree layout, and a session there can write the .bare. Start the session of a worktree of the project")

// errStartDirMissing is in the error chain of Create when the dir of the
// session is not there and is not in a bare+worktree layout.
var errStartDirMissing = errors.New("the dir does not exist")

const convertLockDirName = "convert-locks"

// convertLockPoll is the wait between two tries of LockForConvert.
var convertLockPoll = 50 * time.Millisecond

// ConvertLock is the exclusive lock of a conversion or a clone on one dir.
// The kernel releases the lock when the process ends.
type ConvertLock struct {
	f *os.File
}

// LockForConvert takes the exclusive lock on dir. Until Release, Create
// refuses each session in dir. It waits up to wait for a start that holds
// the shared lock.
func LockForConvert(dir string, wait time.Duration) (*ConvertLock, error) {
	lock, err := lockExclusive(dir, wait)
	if err != nil {
		return nil, fmt.Errorf("lock %s for the conversion: %w", dir, err)
	}
	return lock, nil
}

// lockExclusive takes the exclusive lock on dir, and waits up to wait for
// each process that holds the lock.
func lockExclusive(dir string, wait time.Duration) (*ConvertLock, error) {
	f, info, err := openConvertLock(dir)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if err := requireLockedDir(dir, info); err != nil {
				_ = f.Close()
				return nil, err
			}
			return &ConvertLock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("a session start, a conversion, or a clone in this dir did not end in %s", wait)
		}
		time.Sleep(convertLockPoll)
	}
}

// Release releases the lock. It is safe to call more than one time.
func (l *ConvertLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = l.f.Close()
	l.f = nil
}

// holdStartLock takes the shared lock on directory for a session start. The
// caller calls release when the start ends. It returns ErrConvertInProgress
// when a conversion or a clone holds the lock.
//
// A dir inside a bare+worktree layout is a worktree, and prism converts or
// clones into a project root only, so such a dir gets no lock. BareRoot
// reads only the parents of directory, so a .bare that a conversion or a
// clone makes in directory does not change this result. The clone refuses
// a target in a bare+worktree layout (lockCloneTarget).
//
// A dir that does not exist is refused: a clone makes its target, and a
// start that found no dir has no lock to hold while the clone runs.
//
// With the shared lock held, it returns ErrStartInBareRoot when directory
// holds a .bare. A start that read its row before a conversion, for example
// a `prism restore`, comes here after the conversion released the lock. A
// conversion or a clone makes the .bare only while it holds the exclusive
// lock, so this check sees the .bare of each one that ended.
func holdStartLock(directory string) (release func(), err error) {
	if git.BareRoot(directory) != "" {
		return func() {}, nil
	}
	if _, statErr := os.Stat(directory); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			statErr = errStartDirMissing
		}
		return nil, fmt.Errorf("start a session in %s: %w", directory, statErr)
	}
	f, info, err := openConvertLock(directory)
	if err != nil {
		return nil, fmt.Errorf("lock %s for the session start: %w", directory, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("start a session in %s: %w", directory, ErrConvertInProgress)
		}
		return nil, fmt.Errorf("lock %s for the session start: %w", directory, err)
	}
	if err := requireLockedDir(directory, info); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("start a session in %s: %w", directory, err)
	}
	if _, err := os.Lstat(filepath.Join(directory, ".bare")); err == nil {
		_ = f.Close()
		return nil, fmt.Errorf("start a session in %s: %w", directory, ErrStartInBareRoot)
	}
	return func() { _ = f.Close() }, nil
}

// openConvertLock opens the lock file of dir in the prism state dir, which
// no sandbox can write. The name is the device and the inode of dir, so a
// symlink to dir, or a path with another letter case on a case-insensitive
// volume, gives the same lock. It also returns the Stat result of dir.
func openConvertLock(dir string) (*os.File, os.FileInfo, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, nil, fmt.Errorf("read the inode of %s", dir)
	}
	base, err := sidecarStateDir()
	if err != nil {
		return nil, nil, err
	}
	lockDir := filepath.Join(base, convertLockDirName)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, nil, err
	}
	name := fmt.Sprintf("%d-%d.lock", st.Dev, st.Ino)
	f, err := os.OpenFile(filepath.Join(lockDir, name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return f, info, nil
}

// requireLockedDir returns an error unless dir is still the dir whose lock
// the caller holds. Another dir at the path has another lock, so a start
// in it is not refused.
func requireLockedDir(dir string, locked os.FileInfo) error {
	now, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !os.SameFile(now, locked) {
		return errors.New("another dir took the place of this dir while prism took its lock")
	}
	return nil
}

// SessionsInDir returns each prism session in dir: a session with an open
// row in d, or a tmux session, whose dir is dir by file identity. Such a
// session can have started through a symlink or another letter case. d can
// be nil.
//
// A tmux session counts only when it has a prism row or a sidecar PID file.
// tmux gives a session with no start dir the cwd of the command that made
// it, so a host shell session can have dir as its path. The meta sessions
// (scratchpad, the dashboard) are host shells too. A descendant has a
// read-only worktree. None of these is in the list.
//
// When it cannot read the rows or ask tmux, it returns an error, and the
// sessions that it found. No tmux server is not an error, because then no
// tmux session runs.
func SessionsInDir(d *db.DB, dir string) ([]string, error) {
	var names []string
	var errs []error
	add := func(n string) {
		if n != "" && !sessionname.IsMeta(n) && !sessionname.IsDescendant(n) && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	if d != nil {
		rows, err := d.AllActiveStatus()
		if err != nil {
			errs = append(errs, fmt.Errorf("read the session rows: %w", err))
		}
		for _, r := range rows {
			if r.Worktree != "" && config.SameDir(r.Worktree, dir) {
				add(r.SessionName)
			}
		}
	}
	isPrismSession := func(n string) bool {
		if d != nil {
			if st, err := d.CurrentStatus(n); err == nil && st != nil {
				return true
			}
		}
		pidPath, err := SidecarPIDPath(n)
		return err == nil && lexists(pidPath)
	}
	out, err := listTmuxSessionPaths()
	if err != nil && classifyTmuxListError(err.Error()) != TmuxGone {
		errs = append(errs, fmt.Errorf("list the tmux sessions: %w", err))
	}
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			if n, p, ok := strings.Cut(line, "\t"); ok && p != "" && config.SameDir(p, dir) && isPrismSession(n) {
				add(n)
			}
		}
	}
	return names, errors.Join(errs...)
}

// listTmuxSessionPaths returns the name and the path of each tmux session,
// one session for each line, with a tab between them. Tests replace it.
var listTmuxSessionPaths = func() (string, error) {
	return tmux.Run("list-sessions", "-F", "#{session_name}\t#{session_path}")
}

func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
