package container

import (
	"path/filepath"

	"github.com/prismatic-koi/prism/internal/agentcred"
)

// bwrapCredentialFileBind returns the read-only bind of one credential file
// (internal/agentcred), or nil.
//
// Dst is the path that the file variable holds, the sops-nix symlink
// /run/secrets/<key> on Linux. No other bind exposes /run/secrets or
// /run/secrets.d, so this bind is the only route to the file, and a session
// with no delivery has none. Src is the concrete file that the symlink
// resolves to: bwrap binds by inode, so a sops rotation does not take the
// file away from a running session (see the SSH signing key bind in
// BuildArgs).
//
// The bind is read-only, and its Dst is a mount point. So the session cannot
// write the file, and an unlink or a rename onto it fails with EBUSY.
func bwrapCredentialFileBind(path string) []string {
	if path == "" {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil
	}
	return []string{"--ro-bind", resolved, path}
}

// bwrapCredentialBinds returns the credentials of d that bwrap can deliver,
// and the read-only binds of their files. A credential with a file that does
// not resolve is dropped, so no file variable names a file that the sandbox
// does not have. The other credentials stay. Two credentials that use one
// file get one bind.
func bwrapCredentialBinds(d agentcred.Delivery) (agentcred.Delivery, []string) {
	var kept agentcred.Delivery
	var args []string
	bound := map[string]bool{}
	for _, c := range d.Credentials {
		var binds [][]string
		ok := true
		for _, f := range c.Files {
			b := bwrapCredentialFileBind(f.Path)
			if b == nil {
				ok = false
				break
			}
			binds = append(binds, b)
		}
		if !ok {
			continue
		}
		for _, b := range binds {
			if !bound[b[2]] {
				bound[b[2]] = true
				args = append(args, b...)
			}
		}
		kept.Credentials = append(kept.Credentials, c)
	}
	return kept, args
}
