package cmd

// Unit tests for the injectContainerConfig helper (switch.go).
//
// injectContainerConfig derives the agent role from the worktree path,
// looks up the role's config blob from the profiles file, and sets
// opts.ConfigContent. These tests exercise:
//
//   - Worker role (non-"main" worktree under a bare repo) receives ContainerWorkerConfig.
//   - Coordinator role ("main" worktree under a bare repo) receives ContainerCoordinatorConfig.
//   - Non-worktree path receives ContainerCoordinatorConfig but with no --agent flag.
//   - An explicit Agent override is respected over the DefaultAgent derivation.
//   - Empty config blob (role present but no config set) leaves ConfigContent empty
//     and does not return an error.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/session"
)

// makeInjectTestProfile builds a ProfilesFile with known worker and coordinator
// blobs for inject tests.
func makeInjectTestProfile() *config.ProfilesFile {
	return &config.ProfilesFile{
		ContainerWorkerConfig:      `{"model":"test-worker-model"}`,
		ContainerCoordinatorConfig: `{"model":"test-coordinator-model"}`,
	}
}

// setupBareWorktree creates a temporary bare repo with a worktree for testing.
// Returns the bare repo path and the worktree path.
func setupBareWorktree(t *testing.T, branchName string) (bareRoot, worktreePath string) {
	t.Helper()
	tmpDir := t.TempDir()
	bareRoot = filepath.Join(tmpDir, "myrepo")
	worktreePath = filepath.Join(bareRoot, branchName)

	// Create bare repo structure with .bare marker.
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	// Create .bare marker in parent directory to simulate a prism bare repo.
	if err := os.MkdirAll(filepath.Join(bareRoot, ".bare"), 0755); err != nil {
		t.Fatalf("mkdir .bare: %v", err)
	}

	return bareRoot, worktreePath
}

// TestInjectContainerConfig_WorkerRole verifies that a non-"main" worktree
// directory under a bare repo causes injectContainerConfig to select ContainerWorkerConfig.
func TestInjectContainerConfig_WorkerRole(t *testing.T) {
	pf := makeInjectTestProfile()
	opts := session.Opts{}
	_, worktreePath := setupBareWorktree(t, "feature-branch")

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig: %v", err)
	}

	if opts.ConfigContent != pf.ContainerWorkerConfig {
		t.Errorf("ConfigContent = %q, want %q (worker blob)",
			opts.ConfigContent, pf.ContainerWorkerConfig)
	}
}

// TestInjectContainerConfig_CoordinatorRole verifies that a worktree directory
// named "main" under a bare repo causes injectContainerConfig to select ContainerCoordinatorConfig.
func TestInjectContainerConfig_CoordinatorRole(t *testing.T) {
	pf := makeInjectTestProfile()
	opts := session.Opts{}
	_, worktreePath := setupBareWorktree(t, "main")

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig: %v", err)
	}

	if opts.ConfigContent != pf.ContainerCoordinatorConfig {
		t.Errorf("ConfigContent = %q, want %q (coordinator blob)",
			opts.ConfigContent, pf.ContainerCoordinatorConfig)
	}
}

// TestInjectContainerConfig_NonWorktreePath verifies that a non-worktree path
// (no .bare in parent) receives the coordinator blob but no --agent flag.
func TestInjectContainerConfig_NonWorktreePath(t *testing.T) {
	pf := makeInjectTestProfile()
	opts := session.Opts{}
	// Use a path that is not under a bare repo (no .bare in parent).
	worktreePath := "/some/regular/repo"

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig: %v", err)
	}

	// Non-worktree paths should receive coordinator config.
	if opts.ConfigContent != pf.ContainerCoordinatorConfig {
		t.Errorf("ConfigContent = %q, want %q (coordinator blob for non-worktree)",
			opts.ConfigContent, pf.ContainerCoordinatorConfig)
	}
	// Agent should be empty (no --agent flag passed).
	if opts.Agent != "" {
		t.Errorf("Agent = %q, want %q (no --agent flag for non-worktree)", opts.Agent, "")
	}
}

// TestInjectContainerConfig_ExplicitAgentOverride verifies that an explicit
// opts.Agent override is respected: when opts.Agent is "coordinator", the
// coordinator blob is selected even for a non-"main" directory.
func TestInjectContainerConfig_ExplicitAgentOverride(t *testing.T) {
	pf := makeInjectTestProfile()
	opts := session.Opts{Agent: "coordinator"}
	_, worktreePath := setupBareWorktree(t, "feature-branch")

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig: %v", err)
	}

	// "coordinator" override must win over DefaultAgent("feature-branch") = "worker".
	if opts.ConfigContent != pf.ContainerCoordinatorConfig {
		t.Errorf("ConfigContent = %q, want %q (coordinator blob from explicit override)",
			opts.ConfigContent, pf.ContainerCoordinatorConfig)
	}
}

// TestInjectContainerConfig_EmptyBlobNoError verifies that when the profiles
// file exists but the relevant role config blob is empty, ConfigContent is left
// empty and no error is returned.
func TestInjectContainerConfig_EmptyBlobNoError(t *testing.T) {
	pf := &config.ProfilesFile{
		// Both blobs are intentionally empty to simulate a profiles.json that
		// was generated without container configs.
		ContainerWorkerConfig:      "",
		ContainerCoordinatorConfig: "",
	}
	opts := session.Opts{}
	_, worktreePath := setupBareWorktree(t, "feature-branch")

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig returned error on empty blob: %v", err)
	}

	if opts.ConfigContent != "" {
		t.Errorf("ConfigContent = %q, want empty (no blob for role)", opts.ConfigContent)
	}
}
