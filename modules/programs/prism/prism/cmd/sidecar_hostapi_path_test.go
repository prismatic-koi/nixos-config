package cmd

// Tests for resolveHostAPISockPath: the sidecar resolves a host-API socket
// path in every isolation mode, host included (issue #3135).
//
// `prism spawn` and `prism restore` both start the sidecar through
// StartSidecarWithOpts, which runs `prism sidecar --isolation-mode <mode>`.
// runSidecar passes the result of resolveHostAPISockPath to
// sidecar.Config.HostAPISockPath, and Sidecar.Run binds a listener only when
// that field is non-empty. If host mode resolves an empty path again,
// `prism prompt` to a host-mode pi session fails with "host-API socket not
// found", and these tests fail.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	prismSession "github.com/prismatic-koi/prism/internal/session"
)

func TestResolveHostAPISockPath_EveryIsolationMode(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const sessionName = "prism-test@3135-every-mode"

	want, err := prismSession.SidecarHostAPIPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}

	for _, mode := range config.ValidIsolationModes {
		t.Run(string(mode), func(t *testing.T) {
			got, err := resolveHostAPISockPath(sessionName, container.CapabilitiesFor(mode), 14000)
			if err != nil {
				t.Fatalf("resolveHostAPISockPath(%s): %v", mode, err)
			}
			if got != want {
				t.Errorf("resolveHostAPISockPath(%s) = %q, want %q", mode, got, want)
			}
		})
	}
}

// TestResolveHostAPISockPath_HostModeUsesSessionRunDir pins the location: the
// host-mode socket sits in the per-session run directory, next to pipe.sock.
// A sandbox bind-mounts that directory, so every caller already knows the
// path.
func TestResolveHostAPISockPath_HostModeUsesSessionRunDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const sessionName = "prism-test@3135-run-dir"

	got, err := resolveHostAPISockPath(sessionName, container.CapabilitiesFor(config.IsolationHost), 0)
	if err != nil {
		t.Fatalf("resolveHostAPISockPath(host, port=0): %v", err)
	}
	pipePath, err := prismSession.SidecarHarnessPipePath(sessionName)
	if err != nil {
		t.Fatalf("SidecarHarnessPipePath: %v", err)
	}
	if filepath.Dir(got) != filepath.Dir(pipePath) {
		t.Errorf("host-API socket dir = %q, want the run dir of pipe.sock %q", filepath.Dir(got), filepath.Dir(pipePath))
	}
	if filepath.Base(got) != "hostapi.sock" {
		t.Errorf("host-API socket name = %q, want hostapi.sock", filepath.Base(got))
	}
}

// TestResolveHostAPISockPath_SandboxedModeRequiresPort keeps the --port
// requirement for the sandboxed modes.
func TestResolveHostAPISockPath_SandboxedModeRequiresPort(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	for _, mode := range []config.IsolationMode{config.IsolationBwrap, config.IsolationSandboxExec} {
		t.Run(string(mode), func(t *testing.T) {
			_, err := resolveHostAPISockPath("prism-test@3135-port", container.CapabilitiesFor(mode), 0)
			if err == nil || !strings.Contains(err.Error(), "--port is required") {
				t.Errorf("resolveHostAPISockPath(%s, port=0) error = %v, want --port is required", mode, err)
			}
		})
	}
}
