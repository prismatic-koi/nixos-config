package sidecar

// TestMain re-exec stub interception.
//
// The host-API handlers in this package re-exec prismBinary() (host_api.go),
// which falls back to os.Executable() — in tests, THIS test binary — when
// Config.PrismBinaryPath is unset. Every current test injects the
// PrismBinaryPath seam, so no test reaches the fallback today; but one
// forgotten seam would re-exec the sidecar test binary as a full detached
// suite run — the detached-test-process landmine class. This package's binary
// does the same when invoked as `<self> prompt …`: a full suite run of tens
// of seconds.
//
// Interception, per the convention shared by internal/review,
// internal/session, internal/integration, and cmd:
//
//  1. PRISM_TEST_SUBPROCESS=1 — explicit stub re-invocation: sleep briefly
//     (so a parent can observe a live PID) and exit 0.
//  2. argv defence — re-invoked with one of this package's own re-exec argv
//     shapes (the host-API subcommands below) without the stub env var:
//     exit 0 instead of recursively running the suite. See
//     reexec_interception_test.go for the regression guard.
//
// The fake stdio harness re-invocation (PRISM_FAKE_STDIO_HARNESS) is handled
// by an init() in sidecar_stdio_test.go, which runs before TestMain and
// exits — the two interceptions compose.

import (
	"os"
	"testing"
	"time"
)

// hostAPIReExecSubcommands lists every argv[1] this package's production code
// passes to prismBinary() (the exec.CommandContext sites in host_api.go).
// Keep in sync with the `args := []string{…}` literals there.
var hostAPIReExecSubcommands = map[string]bool{
	"prompt":      true,
	"spawn":       true,
	"review":      true,
	"cleanup":     true,
	"close":       true,
	"switch":      true,
	"event":       true,
	"escalate":    true,
	"investigate": true,
}

func TestMain(m *testing.M) {
	if os.Getenv("PRISM_TEST_SUBPROCESS") == "1" {
		// We are a child process acting as a stub subprocess.
		time.Sleep(50 * time.Millisecond)
		os.Exit(0)
	}
	if len(os.Args) > 1 && hostAPIReExecSubcommands[os.Args[1]] {
		// Re-invoked as a prism subcommand without the stub env var. Exit
		// instead of recursively
		// running the suite.
		os.Exit(0)
	}

	os.Exit(m.Run())
}
