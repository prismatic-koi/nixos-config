package sidecar

import (
	"fmt"
	"path/filepath"

	"github.com/prismatic-koi/prism/internal/rundir"
)

// checkRunDirs returns an error when the run dir of a run-dir path in the
// config is a symlink. It makes nothing: each write makes its dir with
// rundir.EnsureDir, which checks the dir again.
func (s *Sidecar) checkRunDirs() error {
	for _, p := range []string{s.cfg.HostAPISockPath, s.cfg.HarnessPipeSockPath, s.cfg.HarnessPipeSecretPath} {
		if p == "" {
			continue
		}
		if err := rundir.RefuseSymlinkDir(filepath.Dir(p)); err != nil {
			return fmt.Errorf("sidecar: refusing to start for session %q: %w", s.cfg.SessionName, err)
		}
	}
	return nil
}
