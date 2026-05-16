package main

// tmux_isolation_test.go — test scaffolding for isolating iris CLI tests
// from any tmux server running on the developer host.
//
// Background (issue #1733). Several iris CLI tests clear
// $IRIS_SESSION_NAME and $PRISM_SESSION_NAME expecting
// lookupIrisParentSession() to return "" (the "cannot determine calling
// session" path). That helper has a third fallback — tmux.CurrentSession()
// — which shells out to `tmux display-message -p "#{session_name}"`. On a
// developer host with a running tmux server, that command succeeds even
// when $TMUX is unset (tmux walks its socket directory and picks up the
// default server), so the test inherits a host session name instead of the
// empty string it expected. The test passes in CI because GH runners have
// no tmux server up.
//
// isolateHostTmux redirects tmux.TmuxBin to a non-existent path for the
// duration of the test. tmux.CurrentSession() then fails with an exec
// error and lookupIrisParentSession() returns "" as the test expects.
//
// Production code is unaffected — TmuxBin is a package-level variable
// reset by t.Cleanup, and the redirect lives entirely in test scaffolding.

import (
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/tmux"
)

// isolateHostTmux points tmux.TmuxBin at a non-existent binary so any
// fallback to tmux.CurrentSession() during the test fails with an exec
// error rather than picking up a real tmux server running on the
// developer's host. Restored on t.Cleanup.
//
// Use from any iris CLI test that clears $IRIS_SESSION_NAME and
// $PRISM_SESSION_NAME and relies on the env-empty path through
// lookupIrisParentSession().
func isolateHostTmux(t *testing.T) {
	t.Helper()
	orig := tmux.TmuxBin
	tmux.TmuxBin = filepath.Join(t.TempDir(), "no-such-tmux-binary")
	t.Cleanup(func() { tmux.TmuxBin = orig })
}
