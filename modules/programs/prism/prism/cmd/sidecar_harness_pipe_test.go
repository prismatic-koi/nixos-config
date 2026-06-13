package cmd

// Tests for harness pipe transport selection (issue #2078).
//
// The harness pipe transport must be chosen based on isolation mode, NOT on
// runtime.GOOS. Gating on GOOS broke Darwin host-mode pi sessions because
// agentPaneEnvVars (host mode) injects a unix:// URL while the sidecar was
// binding TCP — the pi-extension then retried 5× on a non-existent socket
// and gave up.
//
// These tests pin the gate to isolation mode so that a future revert to
// runtime.GOOS == "darwin" fails CI. The Darwin-only assertions in
// TestSelectHarnessPipeTransport_DarwinPinsToIsolationMode are the canonical
// regression guard for #2078.

import (
	"runtime"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// TestSelectHarnessPipeTransport covers the full matrix of (harness shape ×
// isolation mode) and runs on every platform. It is the platform-agnostic
// contract: only sandbox-exec uses TCP; every other mode uses a Unix socket;
// non-socket-pipe harnesses get neither.
func TestSelectHarnessPipeTransport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		harnessName    string
		isolationMode  config.IsolationMode
		wantSocketPipe bool
		wantTCP        bool
	}{
		// pi is the canonical socket-pipe harness.
		{
			name:           "pi + host → unix socket",
			harnessName:    "pi",
			isolationMode:  config.IsolationHost,
			wantSocketPipe: true,
			wantTCP:        false,
		},
		{
			name:           "pi + bwrap → unix socket",
			harnessName:    "pi",
			isolationMode:  config.IsolationBwrap,
			wantSocketPipe: true,
			wantTCP:        false,
		},
		{
			name:           "pi + sandbox-exec → TCP",
			harnessName:    "pi",
			isolationMode:  config.IsolationSandboxExec,
			wantSocketPipe: true,
			wantTCP:        true,
		},
		// Unknown harnesses have no shape registered → no pipe at all.
		{
			name:           "unknown harness + host → no pipe",
			harnessName:    "no-such-harness",
			isolationMode:  config.IsolationHost,
			wantSocketPipe: false,
			wantTCP:        false,
		},
		{
			name:           "unknown harness + sandbox-exec → no pipe",
			harnessName:    "no-such-harness",
			isolationMode:  config.IsolationSandboxExec,
			wantSocketPipe: false,
			wantTCP:        false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotSocketPipe, gotTCP := selectHarnessPipeTransport(tc.harnessName, tc.isolationMode)
			if gotSocketPipe != tc.wantSocketPipe {
				t.Errorf("selectHarnessPipeTransport(%q, %q): isSocketPipe = %v, want %v",
					tc.harnessName, tc.isolationMode, gotSocketPipe, tc.wantSocketPipe)
			}
			if gotTCP != tc.wantTCP {
				t.Errorf("selectHarnessPipeTransport(%q, %q): useTCP = %v, want %v",
					tc.harnessName, tc.isolationMode, gotTCP, tc.wantTCP)
			}
		})
	}
}

// TestSelectHarnessPipeTransport_DarwinPinsToIsolationMode is the explicit
// regression guard for issue #2078. It documents the Darwin-specific contract:
// the transport must depend on isolation mode, not on GOOS.
//
// On Darwin host mode, agentPaneEnvVars injects PRISM_HARNESS_PIPE=unix://…
// — so the sidecar MUST listen on a Unix socket, otherwise pi-extension
// retries forever on a non-existent path. On Darwin sandbox-exec, the
// sandbox cannot reach Unix sockets reliably, so TCP is required.
//
// If anyone reverts the gate back to runtime.GOOS == "darwin", the host case
// here will fail because useTCP will be true. That is the test's job.
func TestSelectHarnessPipeTransport_DarwinPinsToIsolationMode(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin-only regression guard for issue #2078")
	}

	// Darwin + host → Unix socket. If the gate ever regresses to
	// `if runtime.GOOS == "darwin"`, this assertion will fail because the
	// helper would return useTCP=true on Darwin regardless of isolation mode.
	isSocketPipe, useTCP := selectHarnessPipeTransport("pi", config.IsolationHost)
	if !isSocketPipe {
		t.Fatalf("Darwin host: expected pi to be a socket-pipe harness, got isSocketPipe=false")
	}
	if useTCP {
		t.Errorf("Darwin host: useTCP = true, want false — gate has regressed to GOOS check (issue #2078). " +
			"HarnessPipeSockPath must be set / HarnessPipeTCPPort must be 0 in host mode on Darwin so the " +
			"unix:// URL injected by agentPaneEnvVars matches what the sidecar listens on.")
	}

	// Darwin + sandbox-exec → TCP. agent-run reads harness_port from the DB
	// and injects PRISM_HARNESS_PIPE=tcp://…, bypassing agentPaneEnvVars.
	isSocketPipe, useTCP = selectHarnessPipeTransport("pi", config.IsolationSandboxExec)
	if !isSocketPipe {
		t.Fatalf("Darwin sandbox-exec: expected pi to be a socket-pipe harness, got isSocketPipe=false")
	}
	if !useTCP {
		t.Errorf("Darwin sandbox-exec: useTCP = false, want true — sandbox-exec cannot reach Unix sockets. " +
			"HarnessPipeTCPPort must be != 0 / HarnessPipeSockPath must be \"\" in sandbox-exec mode.")
	}
}

// TestSelectHarnessPipeTransport_LinuxPathsUnchanged guards the Linux side of
// the gate: bwrap and host both use Unix sockets on Linux. This is platform-
// agnostic logic (the helper has no GOOS branches), but the explicit Linux
// test documents that the fix did not regress the Linux paths covered by
// issue #2078's acceptance criteria.
func TestSelectHarnessPipeTransport_LinuxPathsUnchanged(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only assertion for issue #2078 AC")
	}

	for _, mode := range []config.IsolationMode{config.IsolationHost, config.IsolationBwrap} {
		isSocketPipe, useTCP := selectHarnessPipeTransport("pi", mode)
		if !isSocketPipe {
			t.Fatalf("Linux %s: expected pi to be a socket-pipe harness, got isSocketPipe=false", mode)
		}
		if useTCP {
			t.Errorf("Linux %s: useTCP = true, want false — Linux always uses Unix sockets", mode)
		}
	}
}
