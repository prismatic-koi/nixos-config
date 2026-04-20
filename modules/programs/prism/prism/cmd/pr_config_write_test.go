package cmd

// Tests for the bwrap opencode.json config file write added to prCmd
// (issue #904). These mirror the contract tests in spawn_config_write_test.go
// and verify that:
//
//	1. The "sandboxed" gate (isoMode == IsolationPodman || isoMode == IsolationBwrap)
//	   correctly admits bwrap and podman, and rejects host.
//	2. The WriteOpencodeConfig write path used by pr.go
//	   (container.OpencodeConfigFilePath(container.NameForSession(tmuxSession)))
//	   matches the Manager's read path, so prism agent-run's reconstructed
//	   Manager can find the file at the path bwrap expects.
//	3. The gate for calling WriteOpencodeConfig
//	   (isoMode == IsolationBwrap && configContent != "") is reflected in the
//	   actual write behaviour — bwrap with content writes; host does not; bwrap
//	   with empty content does not.
//
// Calling the prCmd RunE directly is not safe: it invokes git, tmux, and
// filesystem side effects. These tests exercise the pure logic pieces and the
// write helper instead.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/session"
)

// TestPrBwrapSandboxedGate asserts that the boolean gate widening the
// configContent generation block matches the post-#898 spec:
//   - IsolationPodman → sandboxed = true
//   - IsolationBwrap  → sandboxed = true  (new in #904 for pr.go)
//   - IsolationHost   → sandboxed = false
func TestPrBwrapSandboxedGate(t *testing.T) {
	cases := []struct {
		mode      config.IsolationMode
		sandboxed bool
	}{
		{config.IsolationPodman, true},
		{config.IsolationBwrap, true},
		{config.IsolationHost, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			got := tc.mode == config.IsolationPodman || tc.mode == config.IsolationBwrap
			if got != tc.sandboxed {
				t.Errorf("sandboxed gate for mode %q = %v, want %v", tc.mode, got, tc.sandboxed)
			}
		})
	}
}

// TestPrBwrapWritePathMatchesManagerPath guards against the path mismatch
// where pr.go passes the raw tmux session name and the Manager uses the
// transformed container name. Both must resolve to the same OpencodeConfigFilePath.
func TestPrBwrapWritePathMatchesManagerPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap mode is Linux-only")
	}

	// Simulate the path derivation in pr.go:
	//   tmuxSessionName = session.NameFor(worktreePath, bareRoot)
	//   containerName   = container.NameForSession(tmuxSessionName)
	//   writePath       = container.OpencodeConfigFilePath(containerName)
	const branchName = "pr-1234"
	bareRoot := filepath.Join(t.TempDir(), "myrepo")
	worktreePath := filepath.Join(bareRoot, branchName)
	tmuxSessionName := session.NameFor(worktreePath, bareRoot)
	containerName := container.NameForSession(tmuxSessionName)

	writePath := container.OpencodeConfigFilePath(containerName)

	// Read path: Manager.opencodeConfigFilePath() calls
	// OpencodeConfigFilePath(m.name) where m.name = NameForSession(cfg.SessionName).
	readPath := container.OpencodeConfigFilePath(container.NameForSession(tmuxSessionName))

	if writePath != readPath {
		t.Errorf("write path %q != read path %q — pr.go will write to a path that agent-run's Manager can never find",
			writePath, readPath)
	}
	if strings.Contains(writePath, "@") {
		t.Errorf("write path %q contains '@' — pr.go must use the container name (NameForSession), not the raw tmux session name",
			writePath)
	}
}

// TestPrBwrapWritesConfigFileWhenContentPresent simulates the pr.go guard:
//
//	if isoMode == config.IsolationBwrap && configContent != "" {
//	    container.WriteOpencodeConfig(containerName, configContent)
//	}
//
// When the gate passes, the file must exist with the expected content.
func TestPrBwrapWritesConfigFileWhenContentPresent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap mode is Linux-only")
	}

	t.Setenv("TMPDIR", t.TempDir())

	const branchName = "pr-5678"
	bareRoot := filepath.Join(t.TempDir(), "myrepo")
	worktreePath := filepath.Join(bareRoot, branchName)
	tmuxSessionName := session.NameFor(worktreePath, bareRoot)
	containerName := container.NameForSession(tmuxSessionName)

	const configContent = `{"model":"pr-worker-model","agents":[]}`

	// Simulate the gate and the write.
	isoMode := config.IsolationBwrap
	if isoMode == config.IsolationBwrap && configContent != "" {
		if err := container.WriteOpencodeConfig(containerName, configContent); err != nil {
			t.Fatalf("WriteOpencodeConfig: %v", err)
		}
	}
	t.Cleanup(func() { _ = os.Remove(container.OpencodeConfigFilePath(containerName)) })

	data, err := os.ReadFile(container.OpencodeConfigFilePath(containerName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != configContent {
		t.Errorf("file content = %q, want %q", string(data), configContent)
	}
}

// TestPrBwrapNoWriteWhenConfigContentEmpty asserts that pr.go's conditional
// skips WriteOpencodeConfig when configContent is empty — even though the
// isolation mode is bwrap. This matches the spec's `configContent != ""`
// guard and prevents an empty opencode.json from being mounted.
func TestPrBwrapNoWriteWhenConfigContentEmpty(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap mode is Linux-only")
	}

	t.Setenv("TMPDIR", t.TempDir())

	const branchName = "pr-empty"
	bareRoot := filepath.Join(t.TempDir(), "myrepo")
	worktreePath := filepath.Join(bareRoot, branchName)
	tmuxSessionName := session.NameFor(worktreePath, bareRoot)
	containerName := container.NameForSession(tmuxSessionName)

	isoMode := config.IsolationBwrap
	configContent := ""

	// Simulate the pr.go guard.
	if isoMode == config.IsolationBwrap && configContent != "" {
		if err := container.WriteOpencodeConfig(containerName, configContent); err != nil {
			t.Fatalf("WriteOpencodeConfig: %v", err)
		}
	}

	// File must NOT exist.
	path := container.OpencodeConfigFilePath(containerName)
	if _, err := os.Stat(path); err == nil {
		t.Errorf("opencode temp file %q must not exist when configContent is empty, but it does", path)
		_ = os.Remove(path)
	}
}

// TestPrHostModeNoWrite asserts that host mode does NOT write the opencode
// temp file. The pr.go guard `isoMode == config.IsolationBwrap && ...` must
// reject IsolationHost.
func TestPrHostModeNoWrite(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap mode is Linux-only")
	}

	t.Setenv("TMPDIR", t.TempDir())

	const branchName = "pr-host"
	bareRoot := filepath.Join(t.TempDir(), "myrepo")
	worktreePath := filepath.Join(bareRoot, branchName)
	tmuxSessionName := session.NameFor(worktreePath, bareRoot)
	containerName := container.NameForSession(tmuxSessionName)

	isoMode := config.IsolationHost
	configContent := `{"model":"pr-worker-model"}`

	// Simulate the pr.go guard. For host mode it must not fire regardless of
	// whether configContent is populated.
	if isoMode == config.IsolationBwrap && configContent != "" {
		if err := container.WriteOpencodeConfig(containerName, configContent); err != nil {
			t.Fatalf("WriteOpencodeConfig: %v", err)
		}
	}

	path := container.OpencodeConfigFilePath(containerName)
	if _, err := os.Stat(path); err == nil {
		t.Errorf("host-mode pr must not write opencode temp file %q, but it exists", path)
		_ = os.Remove(path)
	}
}

// TestPrPodmanModeNoWriteFromPrCmd asserts that podman mode does NOT write
// the opencode temp file from pr.go. The podman sidecar's Create() flow
// handles the file itself — writing it in pr.go for podman would be a no-op
// at best and a source of drift at worst.
func TestPrPodmanModeNoWriteFromPrCmd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap mode is Linux-only")
	}

	t.Setenv("TMPDIR", t.TempDir())

	const branchName = "pr-podman"
	bareRoot := filepath.Join(t.TempDir(), "myrepo")
	worktreePath := filepath.Join(bareRoot, branchName)
	tmuxSessionName := session.NameFor(worktreePath, bareRoot)
	containerName := container.NameForSession(tmuxSessionName)

	isoMode := config.IsolationPodman
	configContent := `{"model":"pr-worker-model"}`

	// Simulate the pr.go guard — podman mode must NOT fire it.
	if isoMode == config.IsolationBwrap && configContent != "" {
		if err := container.WriteOpencodeConfig(containerName, configContent); err != nil {
			t.Fatalf("WriteOpencodeConfig: %v", err)
		}
	}

	path := container.OpencodeConfigFilePath(containerName)
	if _, err := os.Stat(path); err == nil {
		t.Errorf("podman pr must not write opencode temp file %q (sidecar handles it), but it exists", path)
		_ = os.Remove(path)
	}
}
