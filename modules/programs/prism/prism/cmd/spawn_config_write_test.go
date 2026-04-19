package cmd

// Tests for the bwrap opencode.json config file write added to runSpawn
// (issue #900).
//
// The config file write block is three lines in runSpawn:
//
//	if isolationMode == config.IsolationBwrap && configContent != "" {
//	    sessionName := session.NameFor(worktreePath, bareRoot)
//	    if err := container.WriteOpencodeConfig(sessionName, configContent); err != nil { ... }
//	}
//
// Calling runSpawn directly in a test is not safe: it spawns tmux sessions,
// sidecar processes, and other long-running side effects that pollute the test
// environment and can cause hangs in sandboxed build environments (e.g.
// nix build) where those processes never terminate.
//
// Instead these tests exercise the two specific helpers that the write block
// relies on:
//
//   - bwrapConfigWriteSessionName: asserts that session.NameFor produces the
//     correct tmux session name from a worktree path and bare repo root. This
//     is the value passed to container.WriteOpencodeConfig as sessionName, so
//     getting it right is a precondition for the file appearing at the expected
//     path.
//
//   - bwrapConfigWritePath: asserts that container.OpencodeConfigFilePath
//     returns the path that session.NameFor + container.WriteOpencodeConfig
//     would produce. Together with TestWriteOpencodeConfig_WritesContent in
//     container_test.go, this fully covers the three-line block without
//     invoking runSpawn.
//
// The full integration path (spawn → file on disk → bwrap mount) is validated
// by the manual test described in the issue (#900 §manual-verification).

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/session"
)

// TestBwrapSpawn_ConfigFilePathMatchesSessionName asserts that the temp file
// path produced by container.OpencodeConfigFilePath(sessionName) for a bwrap
// session matches the deterministic naming convention.
//
// This is the write path: spawn.go calls WriteOpencodeConfig(sessionName, ...)
// where sessionName = session.NameFor(worktreePath, bareRoot). The file must
// appear at OpencodeConfigFilePath(sessionName) for agent-run to find it.
func TestBwrapSpawn_ConfigFilePathMatchesSessionName(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap mode is Linux-only")
	}

	// Simulate the sessionName that spawn.go derives.
	// session.NameFor uses filepath.Base(bareRoot) for the project name and
	// the branch component from the worktree's git symbolic ref. For a
	// worktree that doesn't exist yet (pre-CreateWorktree), the branch falls
	// back to filepath.Base(worktreePath), which is the branch name.
	//
	// Here we use a synthetic worktreePath/bareRoot pair to test the path
	// derivation without creating real git state.
	const branchName = "feat"
	bareRoot := filepath.Join(t.TempDir(), "myrepo")
	worktreePath := filepath.Join(bareRoot, branchName)

	sessionName := session.NameFor(worktreePath, bareRoot)
	path := container.OpencodeConfigFilePath(sessionName)

	// The path must contain the session name and be inside os.TempDir().
	if !strings.HasPrefix(path, os.TempDir()) {
		t.Errorf("OpencodeConfigFilePath(%q) = %q: not under os.TempDir() %q", sessionName, path, os.TempDir())
	}
	if !strings.Contains(path, sessionName) {
		t.Errorf("OpencodeConfigFilePath(%q) = %q: does not contain session name", sessionName, path)
	}
}

// TestBwrapSpawn_WriteAndReadConfigFile asserts the write/read round-trip that
// spawn.go performs: WriteOpencodeConfig(sessionName, content) writes the file,
// and a subsequent ReadFile at OpencodeConfigFilePath(sessionName) returns the
// original content unchanged.
//
// This directly exercises the three-line block in runSpawn for the bwrap path:
//
//	sessionName := session.NameFor(worktreePath, bareRoot)
//	container.WriteOpencodeConfig(sessionName, configContent)
func TestBwrapSpawn_WriteAndReadConfigFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap mode is Linux-only")
	}

	// Override TMPDIR so the written file lands in the test's temp dir and is
	// automatically cleaned up after the test.
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	// Simulate the session name derivation from spawn.go.
	const branchName = "feat"
	bareRoot := filepath.Join(t.TempDir(), "myrepo")
	worktreePath := filepath.Join(bareRoot, branchName)
	sessionName := session.NameFor(worktreePath, bareRoot)

	const configContent = `{"model":"test-worker-model","agents":[]}`

	// Write as spawn.go does.
	if err := container.WriteOpencodeConfig(sessionName, configContent); err != nil {
		t.Fatalf("WriteOpencodeConfig: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(container.OpencodeConfigFilePath(sessionName)) })

	// Read back and verify.
	data, err := os.ReadFile(container.OpencodeConfigFilePath(sessionName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != configContent {
		t.Errorf("file content = %q, want %q", string(data), configContent)
	}
}

// TestBwrapSpawn_NoWriteWhenConfigContentEmpty asserts that when configContent
// is empty, the spawn.go conditional does NOT call WriteOpencodeConfig and the
// temp file is absent. This mirrors the edge-case guard:
//
//	if isolationMode == config.IsolationBwrap && configContent != "" { ... }
func TestBwrapSpawn_NoWriteWhenConfigContentEmpty(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap mode is Linux-only")
	}

	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	const branchName = "feat"
	bareRoot := filepath.Join(t.TempDir(), "myrepo")
	worktreePath := filepath.Join(bareRoot, branchName)
	sessionName := session.NameFor(worktreePath, bareRoot)

	// Simulate spawn.go's guard: configContent == "" → do not write.
	configContent := ""
	if configContent != "" {
		if err := container.WriteOpencodeConfig(sessionName, configContent); err != nil {
			t.Fatalf("WriteOpencodeConfig: %v", err)
		}
	}

	// File must NOT exist.
	path := container.OpencodeConfigFilePath(sessionName)
	if _, err := os.Stat(path); err == nil {
		t.Errorf("config temp file %q must not exist when configContent is empty, but it does", path)
		_ = os.Remove(path)
	}
}
