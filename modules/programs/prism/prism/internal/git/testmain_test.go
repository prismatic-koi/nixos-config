package git

import (
	"os"
	"testing"

	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

// suiteStateRoot is the directory that TestMain gives to IsolateSuite.
var suiteStateRoot string

// TestMain keeps the suite away from the real prism config of the host:
// git.OpenBare reads config.json under XDG_CONFIG_HOME, and a host-mode
// session inherits the real value (issue #3146).
//
// The fixtures use a local-path origin, so the suite allows the file
// transport. TestHostGit_FileTransportRefused turns it off again.
func TestMain(m *testing.M) {
	var restore func()
	suiteStateRoot, restore = stateguardtest.IsolateSuite()
	restoreFileTransport := AllowFileTransportForTest()
	// A host shell carries the user-git env pins (modules/programs/git.nix),
	// protocol.file.allow=never included. The fixtures run plain git against a
	// local-path origin, which would then fail with "transport 'file' not
	// allowed". GIT_CONFIG_COUNT=0 makes git read none of the inherited pins.
	os.Setenv("GIT_CONFIG_COUNT", "0")
	// CloneWorktree and ConvertToBare refuse in a sandbox. A worker runs the
	// suite in its sandbox, where PRISM_HOST_API is set, so the suite unsets
	// it, as in CI.
	os.Unsetenv("PRISM_HOST_API")
	// internal/db sets the reader of the session start records, and this
	// package cannot import internal/db. With no records, the start of the
	// bare repo record records no .bare.
	SetSessionStartSource(func(string) ([]SessionStart, error) { return nil, nil })
	code := m.Run()
	restoreFileTransport()
	restore()
	os.Exit(code)
}

func TestSuiteStateIsolation_XDGInsideSuiteDir(t *testing.T) {
	stateguardtest.RequireIsolated(t, suiteStateRoot)
}
