package container

import "path/filepath"

// bwrapUniFiPasswordBind returns the read-only bind of the UniFi password
// file for a session that gets the credential (internal/unifi, issues #3472
// and #3476), or nil.
//
// Dst is the path that UNIFI_PASSWORD_FILE holds, the sops-nix symlink
// /run/secrets/unifi_password on Linux. No other bind exposes /run/secrets or
// /run/secrets.d, so this bind is the only route to the password, and a
// session of another repo has none. Src is the concrete file that the symlink
// resolves to: bwrap binds by inode, so a sops rotation does not take the
// file away from a running session (see the SSH signing key bind in
// BuildArgs).
//
// The bind is read-only, and its Dst is a mount point. So the session cannot
// write the file, and an unlink or a rename onto it fails with EBUSY. An
// unresolvable path gives no bind, and BuildArgs then sets no UniFi
// variable.
func bwrapUniFiPasswordBind(path string) []string {
	if path == "" {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil
	}
	return []string{"--ro-bind", resolved, path}
}
