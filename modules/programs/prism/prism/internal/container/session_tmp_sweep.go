package container

// session_tmp_sweep.go — removal of per-session TMPDIRs that no session
// owns (#3170).
//
// The cleanup paths remove the TMPDIR of a session that they end. A session
// that ends on another path, for example the tmux session-closed hook, keeps
// its dir, and /var/tmp has no age-based cleaner on macOS. The sweep removes
// such a dir.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/stateguard"
)

// productionSessionTmpRoot is the value of sessionTmpRoot in production.
const productionSessionTmpRoot = "/var/tmp"

// sessionTmpDirNameRe matches the name that SessionTmpDirPath gives a dir.
var sessionTmpDirNameRe = regexp.MustCompile(fmt.Sprintf(`^prism-[0-9a-f]{%d}$`, sessionTmpDirHashLen))

// sessionTmpSweepAllowed reports whether SweepSessionTmpDirs may remove dirs
// under root.
//
// The production root is swept only on Darwin, the only platform that
// creates these dirs, and only by a production binary. A test binary or a
// test build of prism has a test database, so its rows do not hold the
// sessions of the user: a sweep of /var/tmp would remove the TMPDIR of each
// running session. A test moves the root with SetSessionTmpRootForTest.
func sessionTmpSweepAllowed(root string) bool {
	if root != productionSessionTmpRoot {
		return true
	}
	return runtime.GOOS == "darwin" && !testing.Testing() && stateguard.Root == ""
}

// SessionTmpDirOwner is a DB row that can own a per-session TMPDIR.
type SessionTmpDirOwner struct {
	// Key is the value that SessionTmpDirPath hashes: the instance ID of the
	// row, or its container name.
	Key string
	// EndedAt is nil for a live row, and the sweep keeps its dir. For an
	// ended row, the sweep keeps the dir only when the dir was created after
	// EndedAt. A respawned agent-only session runs with the ended_at of its
	// old row, and its new dir is newer than that ended_at.
	EndedAt *time.Time
}

// SweepSessionTmpDirs removes each per-session TMPDIR that no owner keeps,
// and returns the paths that it removed.
//
// It lists the dirs first and calls owners after. A session writes its row,
// with its instance ID, before its dispatcher creates its TMPDIR. So each
// dir in the list has a row that owners can return. That row is live, or it
// is an ended row that a respawn uses again (see EndedAt).
//
// It fails closed: when owners returns an error, it removes no dir and
// returns that error.
//
// It removes a dir only when all of these are true:
//
//   - the name is prism-<12 lowercase hex chars>;
//   - Lstat shows a dir, not a symlink and not a file;
//   - the current user owns it.
//
// /var/tmp is writable by every local user, so a dir that fails a check
// can be a plant, and the sweep leaves it alone. os.RemoveAll does not
// follow a symlink inside the dir: it removes the link, not its target.
// A dir that fails to be removed (for example, a file in it has the uchg
// flag) does not stop the sweep of the other dirs. The error names it.
func SweepSessionTmpDirs(owners func() ([]SessionTmpDirOwner, error)) (removed []string, err error) {
	root := sessionTmpRoot
	if !sessionTmpSweepAllowed(root) {
		return nil, nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("container: sweep session TMPDIRs: list %s: %w", root, err)
	}
	var candidates []string
	for _, e := range entries {
		if sessionTmpDirNameRe.MatchString(e.Name()) {
			candidates = append(candidates, e.Name())
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	list, err := owners()
	if err != nil {
		return nil, fmt.Errorf("container: sweep session TMPDIRs: read sessions: %w", err)
	}
	keep := make(map[string][]*time.Time, len(list))
	for _, o := range list {
		if p, pathErr := SessionTmpDirPath(o.Key); pathErr == nil {
			keep[filepath.Base(p)] = append(keep[filepath.Base(p)], o.EndedAt)
		}
	}

	var errs []error
	for _, name := range candidates {
		dir := filepath.Join(root, name)
		info, ok := ownedRealDir(dir)
		if !ok || keptBy(keep[name], info) {
			continue
		}
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", dir, rmErr))
			continue
		}
		removed = append(removed, dir)
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("container: sweep session TMPDIRs: %w", errors.Join(errs...))
	}
	return removed, nil
}

// keptBy reports whether an owner keeps the dir: a live owner, or an ended
// owner whose ended_at is older than the dir.
func keptBy(endedAts []*time.Time, info os.FileInfo) bool {
	for _, endedAt := range endedAts {
		if endedAt == nil || dirCreatedAt(info).After(*endedAt) {
			return true
		}
	}
	return false
}

// ownedRealDir returns the Lstat result of path when path, not followed, is
// a dir that the current user owns.
func ownedRealDir(path string) (os.FileInfo, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return nil, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return info, ok && st.Uid == uint32(geteuid())
}
