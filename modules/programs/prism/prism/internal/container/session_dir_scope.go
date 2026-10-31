package container

import (
	"errors"
	"fmt"

	"github.com/prismatic-koi/prism/internal/config"
)

// ErrSessionDirHoldsProjectRoots is in the error chain when prism refuses a
// sandboxed session in a dir that holds prism project roots (#3381).
var ErrSessionDirHoldsProjectRoots = errors.New("a sandboxed session there can write each project root below it, and the .bare (config, hooks, info) of each project. Host git then runs a driver or a hook that the session wrote. Start the session in a project root or in a worktree of a project")

// CheckSandboxSessionDir returns an error that wraps
// ErrSessionDirHoldsProjectRoots when dir is a project location, a dir above
// a project location, or a dir above a project_specific root
// (config.HoldsProjectRoots).
//
// The sandbox of a session can write its dir. Each .bare that host git
// accepts is in a project root, so a session in such a dir can write each
// of them. This includes a .bare that a later prism clone makes, which no
// read-only bind made at the start can cover. So prism refuses the session.
// It reads config.json on each call, because a long-running process must
// see a project location that a switch added.
func CheckSandboxSessionDir(dir string) error {
	entry, location, ok := config.LoadFresh().HoldsProjectRoots(dir)
	if !ok {
		return nil
	}
	if location {
		return fmt.Errorf("start a sandboxed session in %s: the dir is the project location %s (project_locations in config.json), or a dir above it: %w",
			dir, entry, ErrSessionDirHoldsProjectRoots)
	}
	return fmt.Errorf("start a sandboxed session in %s: the dir is above the project root %s (project_specific in config.json): %w",
		dir, entry, ErrSessionDirHoldsProjectRoots)
}
