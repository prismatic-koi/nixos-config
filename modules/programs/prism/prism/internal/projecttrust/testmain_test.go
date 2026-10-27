package projecttrust

import (
	"os"
	"testing"

	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

// suiteStateRoot is the directory that TestMain gives to IsolateSuite.
var suiteStateRoot string

// TestMain keeps the suite away from the real prism config of the host: Load
// reads config.json and project-trust.json under XDG_CONFIG_HOME, and a
// host-mode session inherits the real value (issue #3146).
func TestMain(m *testing.M) {
	var restore func()
	suiteStateRoot, restore = stateguardtest.IsolateSuite()
	code := m.Run()
	restore()
	os.Exit(code)
}

func TestSuiteStateIsolation_XDGInsideSuiteDir(t *testing.T) {
	stateguardtest.RequireIsolated(t, suiteStateRoot)
}
