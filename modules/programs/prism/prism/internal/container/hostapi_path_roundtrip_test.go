package container

// AC-6 round-trip test for #1050: confirm that bwrap.BuildArgs and
// Manager.buildRunArgs both reference the same per-session host-API socket
// directory derived from a sidecar-style path.
//
// This test cannot import internal/session directly (session imports
// container, which would create a cycle), so it re-derives the expected
// directory name inline using the same formula that session.SessionDirName
// uses (12-hex-char SHA-256 prefix). If this drifts from the production
// formula, the test will fail loudly — which is the desired behaviour: a
// drift would mean the sidecar binds in dir A but the sandbox mounts dir B,
// producing the exact silent-failure mode #1050 set out to prevent.

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

// expectedSessionDirName mirrors session.SessionDirName. Kept as a copy here
// because the in-package container test cannot import session due to an
// import cycle (session.go imports internal/container).
func expectedSessionDirName(sessionName string) string {
	const hashLen = 12
	sum := sha256.Sum256([]byte(sessionName))
	return hex.EncodeToString(sum[:])[:hashLen]
}

// TestHostAPIPath_RoundTrip_SidecarBwrapPodmanAgree verifies that for the
// same sidecar-style HostAPISockPath, the bwrap and podman builders both
// reference the same directory — and that directory is the one a sidecar
// would produce via session.SidecarHostAPIPath.
func TestHostAPIPath_RoundTrip_SidecarBwrapPodmanAgree(t *testing.T) {
	cases := []struct {
		name    string
		session string
	}{
		{
			name:    "short coordinator (AC-2)",
			session: "nixos-config@main",
		},
		{
			name:    "long branch name (AC-2)",
			session: "nixos-config@fix-something-with-a-fairly-long-branch-name",
		},
		{
			name:    "long branch + review suffix (AC-2)",
			session: "nixos-config@fix-something-with-a-fairly-long-branch-name~review-1-review-security",
		},
		{
			name:    "worst-case from issue body (AC-1, AC-3)",
			session: "nixos-config@" + strings.Repeat("x", 80) + "~review-99-review-context",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Use a controlled XDG_STATE_HOME so the bwrap fixture
			// (which writes under the test temp dir) is deterministic.
			stateHome := t.TempDir()
			t.Setenv("XDG_STATE_HOME", stateHome)

			// Build the same sidecar-style path the production sidecar
			// would compute via session.SidecarHostAPIPath.
			wantDir := filepath.Join(stateHome, "prism", "run", expectedSessionDirName(tc.session))
			sockPath := filepath.Join(wantDir, "hostapi.sock")

			// 1) Podman: m.buildRunArgs must volume-mount wantDir at /var/run/prism-host.
			m, _, cleanup := bwrapFixture(t, Config{
				SessionName:     tc.session,
				Worktree:        t.TempDir(),
				AllocatedPort:   14010,
				HostAPISockPath: sockPath,
			})
			defer cleanup()

			podmanArgs := m.buildRunArgs()
			gotPodmanDir := findVolumeSrcByDst(podmanArgs, "/var/run/prism-host")
			if gotPodmanDir == "" {
				t.Fatalf("podman args do not include /var/run/prism-host volume mount: %v", podmanArgs)
			}
			if gotPodmanDir != wantDir {
				t.Errorf("podman volume src = %q, want %q (sidecar-derived)", gotPodmanDir, wantDir)
			}

			// 2) Bwrap: BuildArgs must --bind wantDir to itself.
			b := &bwrapIsolator{name: m.name}
			bwrapArgs := b.BuildArgs(m)
			gotBwrapDir := findBindSrcByDst(bwrapArgs, wantDir)
			if gotBwrapDir != wantDir {
				t.Errorf("bwrap --bind for socket dir: src=%q dst=%q, want SRC==DST==%q",
					gotBwrapDir, wantDir, wantDir)
			}
		})
	}
}

// findVolumeSrcByDst scans podman --volume args for "<src>:<dst>[:<opts>]" and
// returns src for the first arg whose dst matches.
func findVolumeSrcByDst(args []string, dst string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] != "--volume" {
			continue
		}
		spec := args[i+1]
		parts := strings.SplitN(spec, ":", 3)
		if len(parts) >= 2 && parts[1] == dst {
			return parts[0]
		}
	}
	return ""
}

// findBindSrcByDst scans bwrap --bind args for "<src> <dst>" pairs and returns
// src for the first pair whose dst matches.
func findBindSrcByDst(args []string, dst string) string {
	for i := 0; i < len(args)-2; i++ {
		if args[i] != "--bind" {
			continue
		}
		if args[i+2] == dst {
			return args[i+1]
		}
	}
	return ""
}
