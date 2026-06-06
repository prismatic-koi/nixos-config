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
