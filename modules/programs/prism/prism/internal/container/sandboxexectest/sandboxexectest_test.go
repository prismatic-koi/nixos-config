package sandboxexectest

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestProbe_NixBuildSandbox verifies that the probe reports the Nix build
// sandbox restriction when NIX_BUILD_TOP is set, regardless of platform.
func TestProbe_NixBuildSandbox(t *testing.T) {
	t.Setenv("NIX_BUILD_TOP", "/nix/var/nix/builds/test")
	reason := probe(Path)
	if reason == "" {
		t.Fatal("probe: want non-empty skip reason inside Nix build sandbox, got \"\"")
	}
	if !strings.Contains(reason, "Nix build sandbox") {
		t.Errorf("probe: skip reason should name the Nix build sandbox; got: %q", reason)
	}
}

// TestProbe_BinaryAbsent verifies that the probe reports a missing
// sandbox-exec binary (the non-Darwin / stripped-CI case) with a skip
// reason, exactly as the old stat-only gate did.
func TestProbe_BinaryAbsent(t *testing.T) {
	// Ensure the NIX_BUILD_TOP branch does not fire first (this test also
	// runs inside the Nix checked build, where it is set).
	t.Setenv("NIX_BUILD_TOP", "")
	missing := filepath.Join(t.TempDir(), "no-such-sandbox-exec")
	reason := probe(missing)
	if reason == "" {
		t.Fatal("probe: want non-empty skip reason for absent binary, got \"\"")
	}
	if !strings.Contains(reason, "not found") {
		t.Errorf("probe: skip reason should say the binary was not found; got: %q", reason)
	}
}
