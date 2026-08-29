package sidecar

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prismatic-koi/prism/internal/agent"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// countingDashboardSink is a test double for DashboardSink that counts the
// number of times each method has been invoked.
type countingDashboardSink struct {
	pushCount  atomic.Int64
	touchCount atomic.Int64
}

func (c *countingDashboardSink) PushEvent(sessionName, state, title string) {
	c.pushCount.Add(1)
}
func (c *countingDashboardSink) TouchSentinel() {
	c.touchCount.Add(1)
}

// TestNew_DefaultDashboardSink_IsProduction verifies the regression AC:
// production behaviour is unchanged when no DashboardSink is configured. When
// the test-mode guard is not set, New() must install productionDashboardSink
// so the pushDashboardEvent+touchDashboardSentinel pair fires from
// writeStateChangeWithSID exactly as before.
//
// We do not exercise the production sink against the real filesystem here
// (that would require a real $HOME); we only assert the wiring choice.
func TestNew_DefaultDashboardSink_IsProduction(t *testing.T) {
	// Ensure the test-mode guard is NOT set so the production branch is taken.
	// The outer test process may have it unset already, but we use Setenv to
	// the empty string via Unsetenv-equivalent: t.Setenv("", "") is invalid,
	// so use os.Unsetenv and rely on t.Cleanup to restore.
	prev, had := os.LookupEnv("PRISM_TEST_MODE_RESTRICT_HOSTAPI")
	_ = os.Unsetenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", prev)
		} else {
			_ = os.Unsetenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI")
		}
	})
	// Also redirect XDG_STATE_HOME to a tempdir so even though the production
	// sink is wired in, any incidental construction-time path resolution stays
	// sandboxed. We are not calling writeStateChange here.
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	d := openTestDB(t)
	sc := New(Config{
		SessionName: "prism-test@invoker-default-sink",
		Repo:        "prism-test",
		DB:          d,
		HarnessName: "pi",
		Harness:     pih.New("", "", ""),
	})
	if _, ok := sc.cfg.DashboardSink.(productionDashboardSink); !ok {
		t.Fatalf("Config.DashboardSink: got %T, want productionDashboardSink", sc.cfg.DashboardSink)
	}
}

// TestNew_TestModeGuard_InstallsNoopSink verifies that when the
// PRISM_TEST_MODE_RESTRICT_HOSTAPI env guard is set (as sidecartest.NewIsolated
// does), New() auto-installs the no-op DashboardSink. This is the wiring AC for
// sidecartest.NewIsolated — without it, tests that bypass NewIsolated and only
// set the guard env var would still touch $XDG_STATE_HOME-derived paths via
// the state-change side-effects.
func TestNew_TestModeGuard_InstallsNoopSink(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(sidecartest.EnvRestrictHostAPI, "1")

	d := openTestDB(t)
	sc := New(Config{
		SessionName: "prism-test@invoker-noop-guard",
		Repo:        "prism-test",
		DB:          d,
		HarnessName: "pi",
		Harness:     pih.New("", "", ""),
	})
	if _, ok := sc.cfg.DashboardSink.(noopDashboardSink); !ok {
		t.Fatalf("Config.DashboardSink under test-mode guard: got %T, want noopDashboardSink", sc.cfg.DashboardSink)
	}
}

// TestNew_ExplicitDashboardSink_Preserved verifies that a caller-supplied
// DashboardSink is preserved unchanged by New() — the auto-install logic must
// not stomp on an explicit injection. This guarantees test setups can mix the
// guard env with a counting sink for assertion.
func TestNew_ExplicitDashboardSink_Preserved(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(sidecartest.EnvRestrictHostAPI, "1")

	d := openTestDB(t)
	sink := &countingDashboardSink{}
	sc := New(Config{
		SessionName:   "prism-test@invoker-explicit-sink",
		Repo:          "prism-test",
		DB:            d,
		HarnessName:   "pi",
		Harness:       pih.New("", "", ""),
		DashboardSink: sink,
	})
	if sc.cfg.DashboardSink != sink {
		t.Fatalf("Config.DashboardSink: explicit sink not preserved (got %T)", sc.cfg.DashboardSink)
	}
}

// TestWriteStateChange_NoopSink_NoFilesystemEffects is the headline test for
// the homeless-shelter isolation. It simulates the homeless-shelter sandbox by setting
// HOME=/homeless-shelter via t.Setenv and constructs a Sidecar via
// sidecartest.NewIsolated. It then forces a state change through the public
// writeStateChange path and asserts that:
//
//   - the call does not panic;
//   - no .dashboard.signal file exists anywhere under the simulated HOME (the
//     directory is unwritable, so any os.MkdirAll attempt would fail loudly
//     in CI; the test asserts no attempt was made by checking that the
//     directory was never created); and
//   - the dashboard sink installed on the Sidecar is the no-op sink (which is
//     what stops the side-effect at source).
//
// This is the regression guard for the footgun: if a future change removed
// the auto-install logic or routed touchDashboardSentinel around the sink,
// this test would fail.
func TestWriteStateChange_NoopSink_NoFilesystemEffects(t *testing.T) {
	// Simulate the nix-sandbox HOME. We do this BEFORE NewIsolated so that
	// any code path inside NewIsolated which (hypothetically) fell back to
	// os.UserHomeDir() would also be exposed. NewIsolated then sets
	// XDG_STATE_HOME to a tempdir, which is what production-style callers do.
	homelessShelter := "/homeless-shelter"
	t.Setenv("HOME", homelessShelter)

	invoker := "prism-test@invoker-homeless-shelter"
	bus := sidecartest.NewIsolated(t, invoker)

	// Sanity check: XDG_STATE_HOME points at the tempdir, not the simulated
	// HOME. dashboardSocketPath/touchDashboardSentinel both consult
	// XDG_STATE_HOME first, so the failure mode they guard against is the
	// fallback to $HOME/.local/state when XDG_STATE_HOME is unset. We probe
	// the fallback by also unsetting XDG_STATE_HOME on the Sidecar's
	// goroutine path: not possible here without racing, so instead we rely
	// on the no-op sink being installed and never calling the real helpers.
	if bus.XDGStateHome == "" {
		t.Fatal("sidecartest.NewIsolated: XDGStateHome empty")
	}

	cfg := Config{
		SessionName: invoker,
		Repo:        "prism-test",
		DB:          bus.DB,
		Clock:       newTestClock(),
		HarnessName: "pi",
		Harness:     pih.New("", "", ""),
	}
	sc := New(cfg)

	// AC: the no-op sink is wired in by the test-mode guard.
	if _, ok := sc.cfg.DashboardSink.(noopDashboardSink); !ok {
		t.Fatalf("under NewIsolated: DashboardSink = %T, want noopDashboardSink", sc.cfg.DashboardSink)
	}

	// Force a state change. writeStateChange dedupes against lastState, so we
	// pick a non-zero target and assert the call returns without touching the
	// filesystem. The harness session ID is nil; that path is exercised by
	// writeEvent below, which does write to the test DB — that is expected.
	sc.writeStateChange(agent.StateActive)

	// Assert the side-effect path did NOT create the .dashboard.signal file
	// under the simulated HOME. The path the production sink would have used
	// (had XDG_STATE_HOME been unset) is $HOME/.local/state/prism/bus/. We
	// also check $XDG_STATE_HOME/prism/bus/.dashboard.signal — the no-op
	// sink must not write there either.
	homeSignalDir := filepath.Join(homelessShelter, ".local", "state", "prism", "bus")
	if _, err := os.Stat(homeSignalDir); err == nil {
		t.Errorf("no-op DashboardSink leaked: created %s under simulated HOME", homeSignalDir)
	} else if !os.IsNotExist(err) && !strings.Contains(err.Error(), "permission denied") {
		// "not exist" or "permission denied" are both acceptable signals that
		// no MkdirAll was attempted. Any other error is unexpected.
		t.Errorf("unexpected Stat error on %s: %v", homeSignalDir, err)
	}

	xdgSignal := filepath.Join(bus.XDGStateHome, "prism", "bus", ".dashboard.signal")
	if _, err := os.Stat(xdgSignal); err == nil {
		t.Errorf("no-op DashboardSink leaked: created %s under XDG_STATE_HOME", xdgSignal)
	}
}

// TestWriteStateChange_ProductionSink_FiresBothHooks verifies the regression
// AC end-to-end: when the production sink is wired in (no test-mode guard),
// writeStateChangeWithSID invokes both PushEvent and TouchSentinel exactly
// once per state transition. We use a countingDashboardSink to observe the
// calls without exercising the real filesystem/socket paths.
//
// This is the structural twin of TestWriteStateChange_NoopSink_NoFilesystemEffects:
// together they assert that wiring through the sink preserves call ordering
// and call counts.
func TestWriteStateChange_ProductionSink_FiresBothHooks(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	d := openTestDB(t)
	sink := &countingDashboardSink{}
	sc := New(Config{
		SessionName:   "prism-test@invoker-production-fires",
		Repo:          "prism-test",
		Worktree:      "/tmp/test-worktree",
		DB:            d,
		Clock:         newTestClock(),
		HarnessName:   "pi",
		Harness:       pih.New("", "", ""),
		DashboardSink: sink,
	})

	sc.writeStateChange(agent.StateActive)
	if got := sink.pushCount.Load(); got != 1 {
		t.Errorf("PushEvent calls after one state change: got %d, want 1", got)
	}
	if got := sink.touchCount.Load(); got != 1 {
		t.Errorf("TouchSentinel calls after one state change: got %d, want 1", got)
	}

	// Same-state dedup: writeStateChange must NOT re-invoke the sink for a
	// repeated state (regression: state.go's lastState dedup is upstream of
	// the sink calls).
	sc.writeStateChange(agent.StateActive)
	if got := sink.pushCount.Load(); got != 1 {
		t.Errorf("PushEvent after dedup'd state change: got %d, want 1", got)
	}
	if got := sink.touchCount.Load(); got != 1 {
		t.Errorf("TouchSentinel after dedup'd state change: got %d, want 1", got)
	}

	// Distinct state transitions each fire the sink once.
	sc.writeStateChange(agent.StateFinished)
	if got := sink.pushCount.Load(); got != 2 {
		t.Errorf("PushEvent after second state change: got %d, want 2", got)
	}
	if got := sink.touchCount.Load(); got != 2 {
		t.Errorf("TouchSentinel after second state change: got %d, want 2", got)
	}
}

// TestProductionDashboardSink_PushEvent_IsNonBlocking verifies that the
// production sink's PushEvent returns promptly even when the dashboard socket
// is absent. The production sink dispatches a goroutine and returns
// immediately. The wrapper must preserve that non-blocking contract.
func TestProductionDashboardSink_PushEvent_IsNonBlocking(t *testing.T) {
	// Point XDG_STATE_HOME at a tempdir with no socket; PushEvent's
	// goroutine will dial and fail in 500ms, but the caller must return
	// immediately.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	sink := productionDashboardSink{}
	// If this blocks, the test will hang and `go test` will time it out.
	sink.PushEvent("prism-test@invoker-nonblock", "active", "title")
}
