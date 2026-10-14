package config_test

import (
	"os"
	"testing"

	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

// suiteStateRoot is the directory that TestMain gives to IsolateSuite.
var suiteStateRoot string

// TestMain keeps the suite away from the real prism state of the host: in a
// host-mode session the test process inherits the real XDG_STATE_HOME and
// XDG_CONFIG_HOME (issue #3146).
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
