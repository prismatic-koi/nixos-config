package main

// dashboard_test.go — pre-flight probe tests for `iris dashboard`.
//
// AC #8 of issue #1703 requires that `iris dashboard` exit non-zero with
// the canonical `systemctl --user start iris` hint when the daemon is not
// running. The synchronous pre-flight probe (dashboardPreflightProbe)
// owns that contract; this test locks the hint shape so future changes
// can't drift it out of alignment with `iris sessions list` /
// `iris prompt`.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDashboard_DaemonNotRunning points the pre-flight probe at a
// non-existent socket path and asserts the canonical error shape:
//
//   - non-nil error (mapping to a non-zero CLI exit)
//   - contains "daemon not running"
//   - contains "systemctl --user start iris"
//
// Mirrors TestPrompt_DaemonNotRunning in prompt_integration_test.go so
// both surfaces produce byte-aligned operator instructions when iris is
// down.
func TestDashboard_DaemonNotRunning(t *testing.T) {
	dir, err := os.MkdirTemp("", "iris-dashboard-no-daemon-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	// Path deliberately does not exist — fetchSessionsSnapshot stat()s
	// the path first and surfaces the canonical "socket does not exist"
	// error before any dial is attempted.
	sockPath := filepath.Join(dir, "iris.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = dashboardPreflightProbe(ctx, sockPath)
	if err == nil {
		t.Fatalf("dashboardPreflightProbe: want daemon-not-running error, got nil")
	}
	if !strings.Contains(err.Error(), "daemon not running") {
		t.Errorf("error missing 'daemon not running' wording: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "systemctl --user start iris") {
		t.Errorf("error missing 'systemctl --user start iris' hint: %q", err.Error())
	}
}
