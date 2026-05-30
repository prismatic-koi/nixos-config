package session

// Test helper: a non-empty placeholder for SpawnOpts.PIExtensionDir /
// Opts.PIExtensionDir on test fixtures that exercise spawn / Create code
// paths unrelated to the #2065 fail-fast guard.
//
// ValidatePILaunchOpts (the chokepoint) rejects an empty PIExtensionDir on
// host-mode pi launches. That guard is correct for production callers, but
// every pre-#2064 spawn-test fixture left the field unset because the gate
// did not exist. Rather than churn each fixture to set the field literally,
// they all use this single string so the intent ("test does not care about
// extension dir; satisfy the guard") is visible at the call site.
//
// The string itself is never opened or stat'd by SpawnSession / Create —
// the value travels into buildDirectAgentCmd which embeds it in the host-
// mode launch command string, and the tests that care about the command
// string assert positively on the presence of this token. Tests that
// exercise the failure path explicitly leave the field unset (see
// TestValidatePILaunchOpts and TestCreate_LayoutFull_FailsFastOnEmptyPIExtensionDir).
const testPIExtensionDir = "/test/prism-pi-extension"
