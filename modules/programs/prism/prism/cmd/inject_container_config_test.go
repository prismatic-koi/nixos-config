package cmd

// Unit tests for the injectContainerConfig helper (switch.go).
//
// injectContainerConfig derives the agent role from the worktree path,
// looks up the role's config blob from the profiles file, and sets
// opts.ConfigContent. These tests exercise:
//
//   - Worker role (non-"main" directory base, parent has .bare) receives ContainerWorkerConfig.
//   - Coordinator role ("main" directory base, parent has .bare) receives ContainerCoordinatorConfig.
//   - Non-worktree path (parent has no .bare) receives ContainerCoordinatorConfig (fallback).
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

// makeBareRoot creates a temp dir with a .bare marker and the given subdirs.
// Returns the root path.
func makeBareRoot(t *testing.T, subdirs ...string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".bare"), []byte("gitdir"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, s := range subdirs {
		if err := os.MkdirAll(filepath.Join(root, s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestInjectContainerConfig_WorkerRole verifies that a non-"main" worktree
// directory (parent has .bare) causes injectContainerConfig to select ContainerWorkerConfig.
func TestInjectContainerConfig_WorkerRole(t *testing.T) {
	root := makeBareRoot(t, "feature-branch")
	pf := makeInjectTestProfile()
	opts := session.Opts{}
	worktreePath := filepath.Join(root, "feature-branch")

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig: %v", err)
	}

	if opts.ConfigContent != pf.ContainerWorkerConfig {
		t.Errorf("ConfigContent = %q, want %q (worker blob)",
			opts.ConfigContent, pf.ContainerWorkerConfig)
	}
}

// TestInjectContainerConfig_CoordinatorRole verifies that a worktree directory
// named "main" (parent has .bare) causes injectContainerConfig to select ContainerCoordinatorConfig.
func TestInjectContainerConfig_CoordinatorRole(t *testing.T) {
	root := makeBareRoot(t, "main")
	pf := makeInjectTestProfile()
	opts := session.Opts{}
	worktreePath := filepath.Join(root, "main")

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig: %v", err)
	}

	if opts.ConfigContent != pf.ContainerCoordinatorConfig {
		t.Errorf("ConfigContent = %q, want %q (coordinator blob)",
			opts.ConfigContent, pf.ContainerCoordinatorConfig)
	}
}

// TestInjectContainerConfig_NonWorktreePath verifies that a non-worktree path
// (parent directory has no .bare) receives the coordinator config blob so that
// build and plan mode agents are available.
func TestInjectContainerConfig_NonWorktreePath(t *testing.T) {
	// Plain temp dir — no .bare parent.
	worktreePath := t.TempDir()
	pf := makeInjectTestProfile()
	opts := session.Opts{}

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig: %v", err)
	}

	if opts.ConfigContent != pf.ContainerCoordinatorConfig {
		t.Errorf("ConfigContent = %q, want %q (coordinator blob for non-worktree path)",
			opts.ConfigContent, pf.ContainerCoordinatorConfig)
	}
}

// TestInjectContainerConfig_ExplicitAgentOverride verifies that an explicit
// opts.Agent override is respected: when opts.Agent is "coordinator", the
// coordinator blob is selected even for a non-"main" directory.
func TestInjectContainerConfig_ExplicitAgentOverride(t *testing.T) {
	root := makeBareRoot(t, "feature-branch")
	pf := makeInjectTestProfile()
	opts := session.Opts{Agent: "coordinator"}
	worktreePath := filepath.Join(root, "feature-branch")

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
	root := makeBareRoot(t, "feature-branch")
	pf := &config.ProfilesFile{
		// Both blobs are intentionally empty to simulate a profiles.json that
		// was generated without container configs.
		ContainerWorkerConfig:      "",
		ContainerCoordinatorConfig: "",
	}
	opts := session.Opts{}
	worktreePath := filepath.Join(root, "feature-branch")

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig returned error on empty blob: %v", err)
	}

	if opts.ConfigContent != "" {
		t.Errorf("ConfigContent = %q, want empty (no blob for role)", opts.ConfigContent)
	}
}
