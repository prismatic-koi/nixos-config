package container

// Host-side state of `prism container`: the host-wide limit lock, the
// per-incarnation audit log, the podman cidfiles, the build context
// copies, and the markers of running builds.
//
//	<XDG_STATE_HOME>/prism/prism-container/host-limit.lock
//	<XDG_STATE_HOME>/prism/prism-container/audit/<instanceID>/audit.log
//	<XDG_STATE_HOME>/prism/prism-container/cid/<per-run dir>/cid
//	<XDG_STATE_HOME>/prism/prism-container/build-stage/<per-build dir>/
//	<XDG_STATE_HOME>/prism/prism-container/build-lock/<instanceID>.<suffix>.lock
//	<XDG_STATE_HOME>/prism/prism-container/build-policy.json
//
// No sandbox may write this tree. The agent is the subject of the audit
// log. Podman writes the cidfile on the host and follows a symlink at its
// path, so a sandbox that can write the cid dir can make podman overwrite
// any file of the host user. Podman build reads the context copy while the
// build runs, so a sandbox that can write the copy can put a symlink to a
// host path in it. A sandbox that can remove a build marker can hide a
// running build from the limits. A sandbox that can write the build
// signature policy can let a build read a host path through a transport. The tree sits outside the session work dir and the per-session run dir,
// which are the only state paths the sandbox-exec profile grants for write.
// bwrap binds nothing under it. Do not add a grant or a bind that reaches it.
// TestGenerateProfile_PrismContainerState_OutsideWriteGrantedSubpaths and
// TestBwrapBuildArgs_PrismContainerStateNotBound fail if one does.

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	prismContainerDirName       = "prism-container"
	prismContainerLockFileName  = "host-limit.lock"
	prismContainerAuditDirName  = "audit"
	prismContainerAuditFileName = "audit.log"
	prismContainerCIDDirName    = "cid"
	prismContainerStageDirName  = "build-stage"
	prismContainerBuildLockDir  = "build-lock"
	prismContainerBuildPolicy   = "build-policy.json"
)

// PrismContainerStateDir returns <XDG_STATE_HOME>/prism/prism-container.
func PrismContainerStateDir() (string, error) {
	base := xdgStateBase()
	if base == "" {
		return "", fmt.Errorf("container: prism-container state dir: cannot determine state home (XDG_STATE_HOME unset and $HOME unresolved)")
	}
	return filepath.Join(base, "prism", prismContainerDirName), nil
}

// PrismContainerLockPath returns the path of the host-wide limit lock file.
func PrismContainerLockPath() (string, error) {
	dir, err := PrismContainerStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, prismContainerLockFileName), nil
}

// PrismContainerCIDDirPath returns the directory that holds the per-run
// cidfile directories.
func PrismContainerCIDDirPath() (string, error) {
	dir, err := PrismContainerStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, prismContainerCIDDirName), nil
}

// PrismContainerBuildStageDirPath returns the directory that holds the
// per-build copies of the build context.
func PrismContainerBuildStageDirPath() (string, error) {
	dir, err := PrismContainerStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, prismContainerStageDirName), nil
}

// PrismContainerBuildLockDirPath returns the directory that holds one
// marker file for each running build.
func PrismContainerBuildLockDirPath() (string, error) {
	dir, err := PrismContainerStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, prismContainerBuildLockDir), nil
}

// PrismContainerBuildPolicyPath returns the path of the signature policy
// that a Linux build uses.
func PrismContainerBuildPolicyPath() (string, error) {
	dir, err := PrismContainerStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, prismContainerBuildPolicy), nil
}

// PrismContainerAuditDirPath returns the audit directory of one session
// incarnation.
func PrismContainerAuditDirPath(instanceID string) (string, error) {
	if instanceID == "" {
		return "", fmt.Errorf("container: prism-container audit dir: instanceID is empty")
	}
	dir, err := PrismContainerStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, prismContainerAuditDirName, instanceID), nil
}

// PrismContainerAuditLogPath returns the audit log path of one session
// incarnation.
func PrismContainerAuditLogPath(instanceID string) (string, error) {
	dir, err := PrismContainerAuditDirPath(instanceID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, prismContainerAuditFileName), nil
}

// PrismContainerAuditDirExists reports whether the incarnation has an audit
// directory. Every `prism container` request creates it before podman runs,
// so a missing directory means the incarnation started no container.
func PrismContainerAuditDirExists(instanceID string) bool {
	dir, err := PrismContainerAuditDirPath(instanceID)
	if err != nil {
		return false
	}
	_, statErr := os.Stat(dir)
	return statErr == nil
}

// RemovePrismContainerAuditDir removes the audit directory of one session
// incarnation. A missing directory is a no-op.
func RemovePrismContainerAuditDir(instanceID string) {
	dir, err := PrismContainerAuditDirPath(instanceID)
	if err != nil {
		return
	}
	_ = os.RemoveAll(dir)
}
