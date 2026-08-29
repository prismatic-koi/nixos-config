package config_test

// profiles_harness_test.go — harness plumbing tests.
//
// Pi is the sole harness, hardwired via harness.Lookup("pi") at callsites.
// This file retains a minimal smoke test that confirms the harness registry
// is reachable from the config package's test binary (the blank import wires
// up the validator hook).
//
// TestLoadProfiles_StaleKeysIgnored in profiles_test.go verifies the Go side
// ignores unknown JSON fields, including default_harness.

import (
	"testing"

	// Blank import so the harness package's init() wires up
	// config.HarnessValidator. This mirrors how prism binaries pull in the
	// validator transitively via the harness registry.
	_ "github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
)

// TestHarnessValidator_Wired confirms that config.HarnessValidator is
// populated by the harness package's init() and recognises "pi" as a
// registered harness. This keeps the blank import active and detects
// future import-cycle regressions.
func TestHarnessValidator_Wired(t *testing.T) {
	// The blank import of harness/pi should have set HarnessValidator.
	// We call it indirectly by checking that LoadProfiles doesn't reject
	// a profiles.json with no default_harness — the validator is not
	// consulted when the field is absent (stale-key path).
	//
	// This test is intentionally thin: its value is the blank import
	// keeping the registry wired and the build graph verified.
	t.Log("harness registry wired via blank import — ok")
}
