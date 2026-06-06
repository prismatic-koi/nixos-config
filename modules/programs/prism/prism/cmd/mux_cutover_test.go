package cmd

// Tests for the PRISM_USE_MUX cutover plumbing (issue #2158).
//
// The gate is small and homeless-shelter clean — no DB, no tmux, no
// network. The tests pin: (1) the env var must be exactly "1" to
// enable; (2) the daemon-not-running diagnostic carries the expected
// shape; (3) surfaceDaemonError preserves non-network errors.

import (
	"errors"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/mux/client"
)

// TestMuxCutoverEnabled_StrictSentinel asserts the gate is on only
// when the env var is exactly "1". Anything else (unset, "0", "true",
// "yes", "1 ", or " 1") leaves the gate off so the operator does not
// accidentally enable the new path with a typo.
func TestMuxCutoverEnabled_StrictSentinel(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"1", true},
		{"true", false},
		{"yes", false},
		{"on", false},
		{" 1", false},
		{"1 ", false},
		{"01", false},
	}
	for _, tc := range cases {
		t.Run("env="+tc.val, func(t *testing.T) {
			t.Setenv(muxCutoverEnvVar, tc.val)
			if got := muxCutoverEnabled(); got != tc.want {
				t.Errorf("muxCutoverEnabled() with %s=%q = %v, want %v",
					muxCutoverEnvVar, tc.val, got, tc.want)
			}
		})
	}
}

// TestMuxCutoverEnabled_AbsentEnvIsDisabled covers the explicit
// "no env var at all" path. t.Setenv only restores; it does not unset
// across the whole process. Use os.Unsetenv via t.Setenv-then-empty
// to make the absence explicit.
func TestMuxCutoverEnabled_AbsentEnvIsDisabled(t *testing.T) {
	t.Setenv(muxCutoverEnvVar, "")
	if muxCutoverEnabled() {
		t.Errorf("muxCutoverEnabled() with empty env = true, want false")
	}
}

// TestDaemonNotRunningError_MentionsRecoverySteps confirms the
// diagnostic carries the canonical recovery commands so the operator
// can copy-paste from the error message. The exact wording is pinned
// (subject to AC adjustments) so a future refactor does not silently
// drop the recovery hint.
func TestDaemonNotRunningError_MentionsRecoverySteps(t *testing.T) {
	err := daemonNotRunningError("prism spawn")
	if err == nil {
		t.Fatal("daemonNotRunningError returned nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"prism spawn: prismd mux daemon is not running",
		"prismd mux start",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q\nfull message:\n%s", want, msg)
		}
	}
}

// TestPlatformSupervisorHint_LinuxAndDarwin pins both platform hint
// strings so a refactor cannot silently regress either. The Darwin
// path note (see platformSupervisorHintFor docstring) is
// load-bearing: the plist filename must match the home-manager
// `labelPrefix = "org.nix-community.home."` + service-name
// composition, otherwise Darwin operators copy-paste a path that
// does not exist on disk (PR #2175 review-context blocker).
func TestPlatformSupervisorHint_LinuxAndDarwin(t *testing.T) {
	t.Run("linux", func(t *testing.T) {
		got := platformSupervisorHintFor("linux")
		for _, want := range []string{
			"systemctl --user start prismd-mux",
			"# Linux",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("linux hint missing %q\nfull hint:\n%s", want, got)
			}
		}
	})
	t.Run("darwin", func(t *testing.T) {
		// Pin a known UID so the test does not depend on the host's
		// environment.
		t.Setenv("UID", "501")
		got := platformSupervisorHintFor("darwin")
		for _, want := range []string{
			"launchctl bootstrap user/501",
			// The full path must include the `home.` segment that
			// home-manager's launchd module hard-codes via its
			// labelPrefix. Pinning the path verbatim catches
			// regressions like PR #2175's original off-by-one
			// filename (org.nix-community.prismd-mux.plist).
			"~/Library/LaunchAgents/org.nix-community.home.prismd-mux.plist",
			"# Darwin",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("darwin hint missing %q\nfull hint:\n%s", want, got)
			}
		}
		// Defence-in-depth: assert the segment that the regression
		// dropped is present. A negative assertion on the wrong
		// filename catches the regression even if the broader
		// substring assertion is loosened in the future.
		if !strings.Contains(got, "org.nix-community.home.prismd-mux.plist") {
			t.Errorf("darwin hint dropped the load-bearing `home.` segment")
		}
		if strings.Contains(got, "org.nix-community.prismd-mux.plist") &&
			!strings.Contains(got, "org.nix-community.home.prismd-mux.plist") {
			t.Errorf("darwin hint regressed to the pre-#2175 filename")
		}
	})
	t.Run("darwin-uid-fallback", func(t *testing.T) {
		// When $UID is unset the helper substitutes the literal
		// "<uid>" placeholder so the operator can see they need to
		// fill it in. Pin the fallback so the diagnostic stays
		// informative.
		t.Setenv("UID", "")
		got := platformSupervisorHintFor("darwin")
		if !strings.Contains(got, "user/<uid>") {
			t.Errorf("darwin hint missing UID fallback placeholder: %s", got)
		}
	})
	t.Run("unknown-goos", func(t *testing.T) {
		got := platformSupervisorHintFor("plan9")
		if !strings.Contains(got, "plan9") {
			t.Errorf("unknown-GOOS hint = %q, want it to name the GOOS", got)
		}
	})
}

// TestSurfaceDaemonError_RewritesUnavailable confirms that the
// canonical ErrServerUnavailable error is replaced by the structured
// diagnostic, while other errors pass through unchanged.
func TestSurfaceDaemonError_RewritesUnavailable(t *testing.T) {
	// Nil → nil (defensive).
	if got := surfaceDaemonError("prism spawn", nil); got != nil {
		t.Errorf("surfaceDaemonError(nil) = %v, want nil", got)
	}

	// ErrServerUnavailable → rewritten.
	got := surfaceDaemonError("prism spawn", client.ErrServerUnavailable)
	if got == nil {
		t.Fatal("surfaceDaemonError(ErrServerUnavailable) = nil, want diagnostic")
	}
	if !strings.Contains(got.Error(), "prismd mux daemon is not running") {
		t.Errorf("surfaceDaemonError did not rewrite ErrServerUnavailable: %v", got)
	}

	// Wrapped ErrServerUnavailable → still rewritten (errors.Is matches
	// through wrappers).
	wrapped := errors.New("wrapper")
	wrapped = &wrapErr{msg: "wrapper", inner: client.ErrServerUnavailable}
	got = surfaceDaemonError("prism spawn", wrapped)
	if got == nil || !strings.Contains(got.Error(), "prismd mux daemon is not running") {
		t.Errorf("surfaceDaemonError(wrapped) = %v, want diagnostic", got)
	}

	// Non-network error → passes through unchanged.
	other := errors.New("something else")
	got = surfaceDaemonError("prism spawn", other)
	if got != other {
		t.Errorf("surfaceDaemonError(other) = %v, want pass-through %v", got, other)
	}
}

// wrapErr is a tiny errors.Is-friendly wrapper for the test above.
type wrapErr struct {
	msg   string
	inner error
}

func (w *wrapErr) Error() string { return w.msg }
func (w *wrapErr) Unwrap() error { return w.inner }

// TestNewMuxClient_Constructs constructs the default mux client and
// asserts the canonical socket path is populated. The Close() is
// expected to succeed on a no-op transport (DisableKeepAlives).
func TestNewMuxClient_Constructs(t *testing.T) {
	// Redirect $XDG_STATE_HOME to t.TempDir() so the canonical path
	// resolution does not touch the developer's $HOME.
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	mc, err := newMuxClient()
	if err != nil {
		t.Fatalf("newMuxClient: %v", err)
	}
	defer mc.Close()
	if mc.SocketPath() == "" {
		t.Errorf("SocketPath is empty after newMuxClient")
	}
}

// TestSwitchClientOrMuxSession_RoutesByGate pins the cutover
// invariant for the picker-driven switch entry points (the
// review-session attach in handleBareRepo and the agent pick in
// handleReviewGroupPick — PR #2175 review-context). When the gate is
// off the call lands on the tmux switch primitives; when the gate is
// on it lands on the mux client. The test substitutes both halves so
// it does not require a live tmux server or a live mux daemon.
func TestSwitchClientOrMuxSession_RoutesByGate(t *testing.T) {
	// Record which substrate fired so we can assert routing.
	var tmuxFired bool
	oldCurClient := tmuxCurrentClient
	oldCallerClient := tmuxCallerClient
	oldSwitch := tmuxSwitchClient
	oldSwitchCurrent := tmuxSwitchClientCurrent
	t.Cleanup(func() {
		tmuxCurrentClient = oldCurClient
		tmuxCallerClient = oldCallerClient
		tmuxSwitchClient = oldSwitch
		tmuxSwitchClientCurrent = oldSwitchCurrent
	})
	tmuxCurrentClient = func() (string, error) { return "fake-client", nil }
	tmuxCallerClient = func() string { return "" }
	tmuxSwitchClient = func(client, target string) error {
		tmuxFired = true
		if target != "repo@feat" {
			t.Errorf("tmux switch target = %q, want %q", target, "repo@feat")
		}
		return nil
	}
	tmuxSwitchClientCurrent = func(target string) (string, error) {
		tmuxFired = true
		return target, nil
	}

	t.Run("gate-off-uses-tmux", func(t *testing.T) {
		tmuxFired = false
		t.Setenv(muxCutoverEnvVar, "")
		if err := switchClientOrMuxSession("prism switch", "repo@feat"); err != nil {
			t.Fatalf("switchClientOrMuxSession: %v", err)
		}
		if !tmuxFired {
			t.Errorf("gate off did not route through tmux")
		}
	})

	t.Run("gate-on-surfaces-daemon-not-running", func(t *testing.T) {
		tmuxFired = false
		// Point at an unbound socket so the call fails cleanly and
		// the diagnostic surfaces. This proves the gate ROUTED to
		// the mux client and DID NOT silently fall back to tmux.
		dir := t.TempDir()
		t.Setenv("XDG_STATE_HOME", dir)
		t.Setenv(muxCutoverEnvVar, "1")

		err := switchClientOrMuxSession("prism switch", "repo@feat")
		if err == nil {
			t.Fatalf("want error against unbound daemon, got nil")
		}
		if !strings.Contains(err.Error(), "prismd mux daemon is not running") {
			t.Errorf("gate-on error did not surface daemon diagnostic: %v", err)
		}
		if tmuxFired {
			t.Errorf("gate on silently fell back to tmux — this is the load-bearing failure mode the cutover gate exists to prevent")
		}
	})
}
