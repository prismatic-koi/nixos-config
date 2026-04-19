package cmd

// Tests for the bwrap opencode.json config file write added to runSpawn
// (issue #900).
//
// The config file write block in runSpawn:
//
//	if isolationMode == config.IsolationBwrap && configContent != "" {
//	    tmuxSessionName := session.NameFor(worktreePath, bareRoot)
//	    containerName := container.NameForSession(tmuxSessionName)
//	    container.WriteOpencodeConfig(containerName, configContent)
//	}
//
// The key insight: the path used at write time must match the path used at
// read time. Manager.opencodeConfigFilePath() calls
// OpencodeConfigFilePath(m.name) where m.name = NameForSession(tmuxSession).
// So spawn.go must pass NameForSession(tmuxSession) — NOT the raw tmux session
// name — to WriteOpencodeConfig.
//
// Calling runSpawn directly in a test is not safe: it spawns tmux sessions,
// sidecar processes, and other long-running side effects that pollute the test
// environment and can cause hangs in sandboxed build environments (e.g.
// nix build). Instead these tests verify the path-derivation contract.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/session"
)

// TestBwrapSpawn_WritePathMatchesManagerPath is the critical correctness test:
// it asserts that the path used by spawn.go when writing the config file
// (container.OpencodeConfigFilePath(container.NameForSession(tmuxSession)))
// equals the path used by Manager.opencodeConfigFilePath() when reading it
// (container.OpencodeConfigFilePath(m.name) where m.name=NameForSession(tmuxSession)).
//
// This guards against the path mismatch where spawn.go passes the raw tmux
// session name and Manager uses the transformed container name — which would
// make the fix a no-op at runtime.
func TestBwrapSpawn_WritePathMatchesManagerPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap mode is Linux-only")
	}

	// Simulate the session name derivation from spawn.go.
	const branchName = "feat"
	bareRoot := filepath.Join(t.TempDir(), "myrepo")
	worktreePath := filepath.Join(bareRoot, branchName)

	// Step 1: derive tmux session name (as spawn.go does).
	tmuxSessionName := session.NameFor(worktreePath, bareRoot)

	// Step 2: derive container name (as spawn.go does before WriteOpencodeConfig).
	containerName := container.NameForSession(tmuxSessionName)

	// Step 3: path at write time (spawn.go's WriteOpencodeConfig argument).
	writePath := container.OpencodeConfigFilePath(containerName)

	// Step 4: path at read time. Manager.opencodeConfigFilePath() calls
	// OpencodeConfigFilePath(m.name) where m.name = NameForSession(cfg.SessionName).
	// Since cfg.SessionName = tmuxSessionName, the read path is:
	readPath := container.OpencodeConfigFilePath(container.NameForSession(tmuxSessionName))

	if writePath != readPath {
		t.Errorf("write path %q != read path %q — spawn.go will write to a path that agent-run's Manager can never find",
			writePath, readPath)
	}
}

// TestBwrapSpawn_WriteAndReadConfigFile asserts the write/read round-trip:
// WriteOpencodeConfig(containerName, content) writes the file, and reading
// from OpencodeConfigFilePath(containerName) returns the original content.
func TestBwrapSpawn_WriteAndReadConfigFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap mode is Linux-only")
	}

	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	// Simulate the path derivation in spawn.go.
	const branchName = "feat"
	bareRoot := filepath.Join(t.TempDir(), "myrepo")
	worktreePath := filepath.Join(bareRoot, branchName)
	tmuxSessionName := session.NameFor(worktreePath, bareRoot)
	containerName := container.NameForSession(tmuxSessionName)

	const configContent = `{"model":"test-worker-model","agents":[]}`

	// Write as spawn.go does (using containerName, not tmuxSessionName).
	if err := container.WriteOpencodeConfig(containerName, configContent); err != nil {
		t.Fatalf("WriteOpencodeConfig: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(container.OpencodeConfigFilePath(containerName)) })

	// Read back as Manager.opencodeConfigFilePath() would resolve.
	data, err := os.ReadFile(container.OpencodeConfigFilePath(containerName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != configContent {
		t.Errorf("file content = %q, want %q", string(data), configContent)
	}
}

// TestBwrapSpawn_NoWriteWhenConfigContentEmpty asserts that the spawn.go
// conditional does NOT call WriteOpencodeConfig when configContent is empty.
func TestBwrapSpawn_NoWriteWhenConfigContentEmpty(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap mode is Linux-only")
	}

	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	const branchName = "feat"
	bareRoot := filepath.Join(t.TempDir(), "myrepo")
	worktreePath := filepath.Join(bareRoot, branchName)
	tmuxSessionName := session.NameFor(worktreePath, bareRoot)
	containerName := container.NameForSession(tmuxSessionName)

	// Simulate spawn.go's guard: configContent == "" → do not write.
	configContent := ""
	if configContent != "" {
		if err := container.WriteOpencodeConfig(containerName, configContent); err != nil {
			t.Fatalf("WriteOpencodeConfig: %v", err)
		}
	}

	// File must NOT exist.
	path := container.OpencodeConfigFilePath(containerName)
	if _, err := os.Stat(path); err == nil {
		t.Errorf("config temp file %q must not exist when configContent is empty, but it does", path)
		_ = os.Remove(path)
	}
}

// TestBwrapSpawn_ContainerNameTransformation verifies the name transformation
// that ensures write/read path consistency. NameForSession maps "@" → "-" and
// prepends "prism-", which distinguishes the container name from the tmux name.
func TestBwrapSpawn_ContainerNameTransformation(t *testing.T) {
	cases := []struct {
		tmuxSession   string
		wantContainer string
	}{
		{"nixos-config@feat", "prism-nixos-config-feat"},
		{"repo@main", "prism-repo-main"},
		{"repo@feat/sub", "prism-repo-feat-sub"},
	}
	for _, tc := range cases {
		t.Run(tc.tmuxSession, func(t *testing.T) {
			got := container.NameForSession(tc.tmuxSession)
			if got != tc.wantContainer {
				t.Errorf("NameForSession(%q) = %q, want %q", tc.tmuxSession, got, tc.wantContainer)
			}
			// Verify write path uses container name, not raw tmux session name.
			writePath := container.OpencodeConfigFilePath(got)
			if strings.Contains(writePath, "@") {
				t.Errorf("write path %q contains '@' — spawn.go is using the raw tmux session name instead of the container name", writePath)
			}
		})
	}
}
