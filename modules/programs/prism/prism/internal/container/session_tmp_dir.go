package container

// session_tmp_dir.go — the per-session TMPDIR of a sandbox-exec session.
//
// Each sandbox-exec session gets its own temp dir:
//
//	/var/tmp/prism-<first 12 hex chars of sha256(instance ID)>
//
// The dispatcher (cmd/agent_run_sandbox_exec_darwin.go) sets TMPDIR and
// MAC_CHROMIUM_TMPDIR to this dir. The SBPL profile grants read-write and
// Unix-socket connect on it, and on no other temp tree: /tmp, /private/tmp,
// and the user $TMPDIR under /var/folders stay closed to the session (#3150).
//
// The dir is short because a Unix socket path must fit in sun_path (104
// bytes on Darwin). The go test suite binds sockets several levels below
// TMPDIR. A dir under the session work dir or under /var/folders leaves too
// few bytes for those paths.
//
// The dir is under /var/tmp, not /tmp: macOS tmp_cleaner
// (/usr/libexec/tmp_cleaner, daily) deletes an empty dir under /tmp that
// has not changed for 3 days. The session cannot make its TMPDIR again,
// because the profile denies a create at the node (section 21e) and in the
// parent. tmp_cleaner does not clean /var/tmp.
//
// The name derives from the instance ID, so cleanup finds the dir from the
// DB row alone and no session can steer the path. /var/tmp is writable by
// every local user, so Prepare refuses a dir at that path that it did not
// make: a symlink, a file, or a dir of another user.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// sessionTmpRoot is the parent dir of every per-session TMPDIR. It is a test
// seam: production code never reassigns it. Tests point it at a test-owned
// dir, because a sandboxed test process cannot write /var/tmp and the nix
// build sandbox can have no /var/tmp.
var sessionTmpRoot = "/var/tmp"

// SetSessionTmpRootForTest points the per-session TMPDIR root at dir and
// returns a function that restores the previous root. Only tests call it.
func SetSessionTmpRootForTest(dir string) (restore func()) {
	prev := sessionTmpRoot
	sessionTmpRoot = dir
	return func() { sessionTmpRoot = prev }
}

// geteuid is a test seam for the owner check of PrepareSessionTmpDir.
var geteuid = os.Geteuid

// sessionTmpDirHashLen is the number of hex chars of the instance-ID hash in
// the dir name. 12 chars (48 bits) is the length that session.SessionDirName
// uses for the run dir.
const sessionTmpDirHashLen = 12

// SessionTmpDirPath returns the per-session TMPDIR for the instance ID.
func SessionTmpDirPath(instanceID string) (string, error) {
	if instanceID == "" {
		return "", fmt.Errorf("container: session TMPDIR: instanceID is empty")
	}
	sum := sha256.Sum256([]byte(instanceID))
	name := "prism-" + hex.EncodeToString(sum[:])[:sessionTmpDirHashLen]
	return filepath.Join(sessionTmpRoot, name), nil
}

// sessionTmpDirPath returns the per-session TMPDIR of this Manager. It keys
// on the same ID as sessionWorkDirPath: InstanceID, or the container name
// when InstanceID is unset.
func (m *Manager) sessionTmpDirPath() (string, error) {
	instanceID := m.cfg.InstanceID
	if instanceID == "" {
		instanceID = m.name
	}
	return SessionTmpDirPath(instanceID)
}

// SessionTmpDir is the exported form of sessionTmpDirPath, for the
// dispatcher and for tests.
func (m *Manager) SessionTmpDir() (string, error) {
	return m.sessionTmpDirPath()
}

// SessionTmpDirEnv returns the env pairs (K=V form) that point the sandbox
// at its per-session TMPDIR:
//
//	TMPDIR=<dir>               Go, node, bun, git, nix, and most tools
//	MAC_CHROMIUM_TMPDIR=<dir>  chromium (playwright), which ignores TMPDIR
//
// Chromium gets its temp dir from NSTemporaryDirectory(), which returns the
// user $TMPDIR under /var/folders whatever TMPDIR holds. The profile denies
// writes there, so without MAC_CHROMIUM_TMPDIR chromium cannot make its
// temp dirs.
func SessionTmpDirEnv(dir string) []string {
	return []string{
		"TMPDIR=" + dir,
		"MAC_CHROMIUM_TMPDIR=" + dir,
	}
}

// PrepareSessionTmpDir creates the per-session TMPDIR with mode 0700 and
// returns its path. A dir that this user made at the path stays, so a
// restored session keeps its TMPDIR.
//
// It fails when the path is a symlink, is not a dir, or belongs to another
// user. /tmp is writable by every local user, so any of these can be a
// plant. The session must not start with a TMPDIR that it does not own.
func (m *Manager) PrepareSessionTmpDir() (string, error) {
	dir, err := m.sessionTmpDirPath()
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("container: session TMPDIR: create %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("container: session TMPDIR: stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("container: session TMPDIR: %s is not a directory (mode %s); remove it, then start the session again", dir, info.Mode())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(geteuid()) {
		return "", fmt.Errorf("container: session TMPDIR: %s belongs to another user; remove it, then start the session again", dir)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", fmt.Errorf("container: session TMPDIR: chmod %s: %w", dir, err)
		}
	}
	return dir, nil
}

// RemoveSessionTmpDir removes the per-session TMPDIR of the instance ID. It
// is idempotent. os.RemoveAll removes a symlink at the path, not its target.
//
// Every cleanup path that calls RemoveSessionWorkDir must call this too.
func RemoveSessionTmpDir(instanceID string) {
	dir, err := SessionTmpDirPath(instanceID)
	if err != nil {
		return
	}
	_ = os.RemoveAll(dir)
}

// symlinkForms returns p and, when p is under /tmp or /var, the same path
// through the other side of the macOS /tmp -> /private/tmp or
// /var -> /private/var symlink. sandbox-exec matches some operations on the
// path as given and others on the resolved path, so a grant on a path under
// one of these symlinks lists both forms.
func symlinkForms(p string) []string {
	for _, link := range []string{"/tmp", "/var"} {
		target := "/private" + link
		switch {
		case p == link || strings.HasPrefix(p, link+"/"):
			return []string{target + strings.TrimPrefix(p, link), p}
		case p == target || strings.HasPrefix(p, target+"/"):
			return []string{p, link + strings.TrimPrefix(p, target)}
		}
	}
	return []string{p}
}
