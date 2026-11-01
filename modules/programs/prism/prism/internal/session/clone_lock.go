package session

// clone_lock.go — prism clone holds the lock of convert_lock.go on its
// target (#3388).
//
// A sandboxed session that starts in the target during a clone can write
// the .bare that the clone makes. The bare repo record accepts that .bare,
// because prism made it, so the next spawn runs host git in it. So the
// clone holds the exclusive lock on the target from before its checks to
// its end, and Create refuses each start in the target.
//
// A start that held the shared lock when the clone asked for the lock ended
// first, so its tmux session runs in the target. Its start record can come
// later, from the tmux hook. So the clone also asks tmux (SessionsInDir).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
)

func init() {
	git.SetCloneTargetLockSource(lockCloneTarget)
}

// cloneLockWait is the time that a clone waits for the lock of its target.
// Tests set it.
var cloneLockWait = 10 * time.Second

// lockCloneTarget takes the exclusive lock on dir, the target of a clone.
// With the lock held, it refuses the clone in these cases:
//
//   - dir is in a bare+worktree layout. A start there takes no start lock
//     (holdStartLock).
//   - a prism session is in dir (SessionsInDir), or prism cannot tell.
//
// When it holds the lock, it returns the release also with an error, so
// that the clone removes the dirs that it made before it releases the lock.
func lockCloneTarget(dir string) (release func(), err error) {
	lock, err := lockExclusive(dir, cloneLockWait)
	if err != nil {
		return nil, fmt.Errorf("clone into %s: lock the dir: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return lock.Release, fmt.Errorf("clone into %s: %w", dir, err)
	}
	// A start can name the target through a symlink, or by its real path.
	for _, p := range []string{dir, resolved} {
		if root := git.BareRoot(p); root != "" {
			return lock.Release, fmt.Errorf("clone into %s: the dir is in the bare+worktree layout of %s. A session start in such a dir takes no start lock, so prism cannot keep a session out of the dir during the clone", dir, root)
		}
	}
	d, err := openDBIfPresent()
	if err != nil {
		return lock.Release, fmt.Errorf("clone into %s: open the DB: %w. A session in the dir can write the .bare of the clone", dir, err)
	}
	if d != nil {
		defer d.Close()
	}
	names, err := SessionsInDir(d, dir)
	if err != nil {
		return lock.Release, fmt.Errorf("clone into %s: prism cannot tell if a session is in the dir: %w. A session in the dir can write the .bare of the clone", dir, err)
	}
	if len(names) > 0 {
		return lock.Release, fmt.Errorf("clone into %s: the session %s is in the dir: tmux runs it there, or its row has no end time. A session in the dir can write the .bare of the clone. Stop the session, or end its row with \"prism cleanup --yes --session <name>\", then run the clone again", dir, strings.Join(names, ", "))
	}
	return lock.Release, nil
}

// openDBIfPresent opens prism.db, or returns nil when there is no prism.db.
// With no prism.db, no session has a row.
func openDBIfPresent() (*db.DB, error) {
	if _, err := os.Stat(dbPath()); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return openDB()
}
