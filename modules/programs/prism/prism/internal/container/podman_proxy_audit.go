package container

// podman_proxy_audit.go — location and lifecycle of the per-session audit
// log of the removed podman API proxy. Prism no longer writes this log.
// Cleanup still archives and removes the log of a session that was spawned
// with the proxy.
//
// That log is the record used to reconstruct what a session asked the
// container runtime to do, so the subject of the record — the agent — must
// have no write path to it. A record its own subject can edit is not
// evidence.
//
// Two per-session directories are therefore unavailable to it:
//
//   - The session work dir, <XDG_STATE_HOME>/prism/sessions/<instanceID>/.
//     generateProfile grants the agent file-read* file-write* over
//     (subpath <sessionDir>) — see sandbox_exec.go section 6.
//   - The per-session run dir, <XDG_STATE_HOME>/prism/run/<sessionDirName>/.
//     The same clause grants it read-write through HostAPISockPath's parent
//     directory.
//
// The `podman-audit` root sits outside both:
//
//	<XDG_STATE_HOME>/prism/podman-audit/<instanceID>/podman-proxy.log
//
// No clause in the SBPL profile names that root, and bwrap binds nothing
// under it, so the agent cannot append to, rewrite, or truncate the log on
// either platform. Keep it that way: a profile grant that reaches this
// root defeats the record.
// TestGenerateProfile_PodmanProxyAuditLog_OutsideWriteGrantedSubpaths
// fails if any write-granted subpath of the profile ever covers the path.
//
// Cleanup is not free at this location. RemoveSessionWorkDir removes the
// work dir only, so the audit directory needs its own removal at session
// teardown: RemovePodmanProxyAuditDir, called from cmd/cleanup.go beside
// RemoveSessionWorkDir.

import (
	"fmt"
	"os"
	"path/filepath"
)

// podmanProxyAuditDirName is the directory under <XDG_STATE_HOME>/prism/
// that holds one audit subdirectory per session instance.
const podmanProxyAuditDirName = "podman-audit"

// podmanProxyAuditLogFileName is the basename of the audit log inside the
// per-session audit directory.
const podmanProxyAuditLogFileName = "podman-proxy.log"

// PodmanProxyAuditDirPath returns the per-session podman-proxy audit
// directory for the given instance ID:
//
//	<XDG_STATE_HOME>/prism/podman-audit/<instanceID>/
//
// The base comes from xdgStateBase, so it honours $XDG_STATE_HOME first and
// falls back to <home>/.local/state — the same order SessionWorkDirPath
// uses. A test that needs a writable audit dir sets XDG_STATE_HOME to a
// t.TempDir(), which is also what keeps the path writable in the
// homeless-shelter nix sandbox.
func PodmanProxyAuditDirPath(instanceID string) (string, error) {
	if instanceID == "" {
		return "", fmt.Errorf("container: podman-proxy audit dir: instanceID is empty")
	}
	base := xdgStateBase()
	if base == "" {
		return "", fmt.Errorf("container: podman-proxy audit dir: cannot determine state home (XDG_STATE_HOME unset and $HOME unresolved)")
	}
	return filepath.Join(base, "prism", podmanProxyAuditDirName, instanceID), nil
}

// PodmanProxyAuditLogPath returns the audit-log path for the given instance
// ID:
//
//	<XDG_STATE_HOME>/prism/podman-audit/<instanceID>/podman-proxy.log
//
// It is the single source of truth for the path: the sidecar opens it, the
// docs name it, and the tests assert on it.
func PodmanProxyAuditLogPath(instanceID string) (string, error) {
	dir, err := PodmanProxyAuditDirPath(instanceID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, podmanProxyAuditLogFileName), nil
}

// RemovePodmanProxyAuditDir removes the per-session audit directory tree
// for the given instance ID. It is idempotent: a missing directory is a
// no-op, so a session that never enabled containers is safe to pass.
//
// Every cleanup path that calls RemoveSessionWorkDir must call this too.
// The audit directory lives outside the work dir on purpose, so the
// work-dir wipe does not reach it.
func RemovePodmanProxyAuditDir(instanceID string) {
	dir, err := PodmanProxyAuditDirPath(instanceID)
	if err != nil {
		return // can't derive path — nothing to do
	}
	_ = os.RemoveAll(dir)
}
