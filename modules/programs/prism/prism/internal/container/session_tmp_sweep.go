package container

// session_tmp_sweep.go — removal of per-session TMPDIRs that no live
// session owns (#3170).
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
// test build of prism has a test database, so its live set does not hold
// the sessions of the user: a sweep of /var/tmp would remove the TMPDIR of
// each running session. A test moves the root with
// SetSessionTmpRootForTest.
func sessionTmpSweepAllowed(root string) bool {
	if root != productionSessionTmpRoot {
		return true
	}
	return runtime.GOOS == "darwin" && !testing.Testing() && stateguard.Root == ""
}

// SweepSessionTmpDirs removes each per-session TMPDIR whose name does not
// match an instance ID that liveIDs returns. It returns the paths that it
// removed.
//
// It lists the dirs first and calls liveIDs after. A session writes its
// row, with its instance ID, before its dispatcher creates its TMPDIR. So
// each dir in the list has a row that liveIDs can return.
//
// It fails closed: when liveIDs returns an error, it removes no dir and
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
func SweepSessionTmpDirs(liveIDs func() ([]string, error)) (removed []string, err error) {
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

	ids, err := liveIDs()
	if err != nil {
		return nil, fmt.Errorf("container: sweep session TMPDIRs: read live sessions: %w", err)
	}
	keep := make(map[string]bool, len(ids))
	for _, id := range ids {
		if p, pathErr := SessionTmpDirPath(id); pathErr == nil {
			keep[filepath.Base(p)] = true
		}
	}

	var errs []error
	for _, name := range candidates {
		if keep[name] {
			continue
		}
		dir := filepath.Join(root, name)
		if !ownedRealDir(dir) {
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

// ownedRealDir reports whether path, not followed, is a dir that the
// current user owns.
func ownedRealDir(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(geteuid())
}
