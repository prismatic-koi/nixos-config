// Package sandboxexectest provides the shared capability gate used by the
// sandbox-exec integration tests in internal/container and
// internal/integration (issue #2203).
//
// Both test suites invoke /usr/bin/sandbox-exec directly, which only works
// when sandbox_apply is actually permitted in the current environment.
// A mere stat of the binary is not sufficient as an availability gate:
//
//   - Inside the Nix build sandbox, sandbox-exec exists but cannot nest a
//     second sandbox (NIX_BUILD_TOP is set, HOME=/homeless-shelter).
//   - Inside an outer prism sandbox-exec worker session, sandbox-exec exists
//     but macOS refuses to nest: every invocation fails with
//     "sandbox_apply: Operation not permitted".
//
// Require performs an actual sandbox-apply capability probe so tests skip
// (with an explanatory message) instead of failing for environmental reasons
// unrelated to the rule under test. The probe result is cached for the
// lifetime of the test binary — the environment cannot change mid-run.
package sandboxexectest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Path is the absolute path of Apple's sandbox-exec binary. Tests invoke it
// directly rather than via PATH so they are pinned to the Apple-shipped
// version (third-party shims under PATH are rejected by SIP and would skew
// the test signal anyway).
const Path = "/usr/bin/sandbox-exec"

var (
	probeOnce sync.Once
	// probeSkipReason is non-empty when sandbox-exec cannot be used in this
	// environment; it is the message passed to t.Skipf.
	probeSkipReason string
)

// Require skips the test when /usr/bin/sandbox-exec is not present or cannot
// actually apply an SBPL profile in this environment (Nix build sandbox, or
// nested under an outer prism sandbox-exec session). It is the shared
// availability gate for all sandbox-exec integration tests.
//
// It deliberately skips ONLY on known environmental restrictions — any other
// probe outcome leaves the tests running so genuine regressions still fail
// loudly (the probe must not over-skip).
func Require(t testing.TB) {
	t.Helper()
	probeOnce.Do(func() { probeSkipReason = probe(Path) })
	if probeSkipReason != "" {
		t.Skip(probeSkipReason)
	}
}

// probe checks whether the sandbox-exec binary at path is usable. It returns
// a non-empty skip reason when it is not, and "" when tests should proceed.
// Factored out of Require (with the path injectable) for unit testing.
func probe(path string) string {
	// The Nix build sandbox sets NIX_BUILD_TOP and sets HOME to
	// /homeless-shelter. Running sandbox-exec inside the Nix sandbox produces
	// "sandbox_apply: Operation not permitted" because the Nix sandboxed
	// builder cannot nest a second sandbox-exec invocation.
	if nixBuildTop := os.Getenv("NIX_BUILD_TOP"); nixBuildTop != "" {
		return fmt.Sprintf("skipping sandbox-exec integration test inside Nix build sandbox (NIX_BUILD_TOP=%s)", nixBuildTop)
	}

	if _, err := os.Stat(path); err != nil {
		return fmt.Sprintf("sandbox-exec not found at %s: %v", path, err)
	}

	// Detect the nested-sandbox case (test is running inside an outer prism
	// sandbox-exec session). Probe with a permissive profile and /bin/echo —
	// if sandbox_apply itself fails, every test gated on this helper would
	// fail for environmental reasons unrelated to the rule under test. Skip
	// rather than emit a misleading red.
	dir, err := os.MkdirTemp("", "sandbox-exec-probe-")
	if err != nil {
		return fmt.Sprintf("cannot create temp dir for sandbox-exec probe: %v", err)
	}
	defer os.RemoveAll(dir)

	probePath := filepath.Join(dir, "probe.sb")
	if err := os.WriteFile(probePath, []byte("(version 1)\n(allow default)\n"), 0o600); err != nil {
		return fmt.Sprintf("cannot write sandbox-exec probe profile: %v", err)
	}

	out, err := exec.Command(path, "-f", probePath, "/bin/echo", "probe-ok").CombinedOutput()
	if err != nil && strings.Contains(string(out), "sandbox_apply: Operation not permitted") {
		return fmt.Sprintf("skipping sandbox-exec integration test — nested sandbox-exec is blocked in this environment (likely running inside an outer prism sandbox-exec session): %s", strings.TrimSpace(string(out)))
	}

	// Any other probe outcome: do not skip. A flaky or unexpected probe
	// failure must not silently hide real test signal.
	return ""
}
