//go:build !linux

package parity_test

// bwrap_guard_other_test.go — no-op requireBwrap on non-Linux platforms.
//
// On Darwin the iris bash sandbox uses sandbox-exec (not bwrap), so the
// bwrap guard is meaningless. We still define requireBwrap so parity-test
// call sites compile uniformly; it is a no-op on non-Linux. If a future
// change adds a sandbox-exec equivalent permission probe, this is where
// it would live.

import "testing"

func requireBwrap(t *testing.T) {
	t.Helper()
	// no-op on non-Linux
}
