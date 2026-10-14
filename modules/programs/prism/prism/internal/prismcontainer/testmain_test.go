package prismcontainer_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

// suiteStateRoot is the directory that TestMain gives to IsolateSuite.
var suiteStateRoot string

// TestMain keeps the suite away from the real prism state of the host: in a
// host-mode session the test process inherits the real XDG_STATE_HOME and
// XDG_CONFIG_HOME, and SweepSession removes stale build copies from the real
// state dir (issue #3146).
//
// IsolateSuite also moves XDG_CONFIG_HOME, and podman reads its own config
// (the machine connections, containers.conf) from $XDG_CONFIG_HOME/containers.
// The PRISM_CONTAINER_INTEGRATION tests need the real podman, so the suite
// config dir gets a link to the containers config dir of the user. The prism
// config dir stays isolated. The restore removes the link, not its target.
func TestMain(m *testing.M) {
	containersDir := userContainersConfigDir()
	var restore func()
	suiteStateRoot, restore = stateguardtest.IsolateSuite()
	if containersDir != "" {
		if _, err := os.Stat(containersDir); err == nil {
			link := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "containers")
			if err := os.Symlink(containersDir, link); err != nil {
				fmt.Fprintf(os.Stderr, "prismcontainer: TestMain: link the containers config: %v\n", err)
				restore()
				os.Exit(1)
			}
		}
	}
	code := m.Run()
	restore()
	os.Exit(code)
}

// userContainersConfigDir returns the containers config dir of the user,
// resolved the way podman resolves it.
func userContainersConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "containers")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "containers")
	}
	return ""
}

func TestSuiteStateIsolation_XDGInsideSuiteDir(t *testing.T) {
	stateguardtest.RequireIsolated(t, suiteStateRoot)
}
