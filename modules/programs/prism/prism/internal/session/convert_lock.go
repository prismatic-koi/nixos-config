package session

// convert_lock.go — the lock that keeps a session from a start in a plain
// clone while prism converts the clone to the bare+worktree layout (#3361).
//
// The root session of a plain clone can write the clone. The conversion
// makes <root>/.bare there and runs host git in it. So the conversion stops
// the session first, and holds an exclusive lock on the dir until the
// conversion ends. Create holds a shared lock on the dir for the whole
// start. Thus a start that runs when the conversion asks for the lock ends
// first, and the conversion then stops that session. A start during the
// conversion is refused. A start after the conversion is refused too,
// because the dir then holds a .bare.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/prismatic-koi/prism/internal/git"
)

// ErrConvertInProgress is in the error chain of Create when prism converts
// the dir of the session to the bare+worktree layout.
var ErrConvertInProgress = errors.New("prism converts this dir to the bare+worktree layout, so no session can start in it until the conversion ends")

// ErrStartInBareRoot is in the error chain of Create when the dir of the
// session holds a .bare. A session in that dir can write the .bare.
var ErrStartInBareRoot = errors.New("the dir holds a .bare, so it is the root of a bare+worktree layout, and a session there can write the .bare. Start the session of a worktree of the project")

const convertLockDirName = "convert-locks"

// convertLockPoll is the wait between two tries of LockForConvert.
var convertLockPoll = 50 * time.Millisecond

// ConvertLock is the exclusive lock of a conversion on one dir. The kernel
// releases the lock when the process ends.
type ConvertLock struct {
	f *os.File
}

// LockForConvert takes the exclusive lock on dir. Until Release, Create
// refuses each session in dir. It waits up to wait for a start that holds
// the shared lock.
func LockForConvert(dir string, wait time.Duration) (*ConvertLock, error) {
	f, err := openConvertLock(dir)
	if err != nil {
		return nil, fmt.Errorf("lock %s for the conversion: %w", dir, err)
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &ConvertLock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s for the conversion: %w", dir, err)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s for the conversion: a session start or a conversion in this dir did not end in %s", dir, wait)
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
// when a conversion holds the lock.
//
// A dir inside a bare+worktree layout is a worktree, and prism converts only
// a project root, so such a dir gets no lock. BareRoot reads only the
// parents of directory, so a .bare that the conversion makes in directory
// does not change this result. A dir that does not exist gets no lock,
// because a conversion locks only a dir that exists.
//
// With the shared lock held, it returns ErrStartInBareRoot when directory
// holds a .bare. A start that read its row before a conversion, for example
// a `prism restore`, comes here after the conversion released the lock. The
// conversion makes the .bare only while it holds the exclusive lock, so
// this check sees the .bare of each conversion that ended.
func holdStartLock(directory string) (release func(), err error) {
	if git.BareRoot(directory) != "" {
		return func() {}, nil
	}
	if _, statErr := os.Stat(directory); statErr != nil {
		return func() {}, nil
	}
	f, err := openConvertLock(directory)
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
	if _, err := os.Lstat(filepath.Join(directory, ".bare")); err == nil {
		_ = f.Close()
		return nil, fmt.Errorf("start a session in %s: %w", directory, ErrStartInBareRoot)
	}
	return func() { _ = f.Close() }, nil
}

// openConvertLock opens the lock file of dir in the prism state dir, which
// no sandbox can write. The name is the device and the inode of dir, so a
// symlink to dir, or a path with another letter case on a case-insensitive
// volume, gives the same lock.
func openConvertLock(dir string) (*os.File, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("read the inode of %s", dir)
	}
	base, err := sidecarStateDir()
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(base, convertLockDirName)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%d-%d.lock", st.Dev, st.Ino)
	return os.OpenFile(filepath.Join(lockDir, name), os.O_CREATE|os.O_RDWR, 0o600)
}
