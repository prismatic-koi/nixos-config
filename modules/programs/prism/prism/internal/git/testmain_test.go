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
	code := m.Run()
	restoreFileTransport()
	restore()
	os.Exit(code)
}

func TestSuiteStateIsolation_XDGInsideSuiteDir(t *testing.T) {
	stateguardtest.RequireIsolated(t, suiteStateRoot)
}
