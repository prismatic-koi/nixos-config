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
	"strings"
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

// isolateActiveProfileState redirects $XDG_STATE_HOME to a per-test tempdir
// so the runtime active-profile lookup in injectContainerConfig (added by
// #1207) cannot leak the developer's real state file into a test fixture.
// Without this, a host with `prism profile use <name>` set could break tests
// that pass a minimal ProfilesFile lacking that profile.
func isolateActiveProfileState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

// TestInjectContainerConfig_WorkerRole verifies that a non-"main" worktree
// directory (parent has .bare) causes injectContainerConfig to select ContainerWorkerConfig.
func TestInjectContainerConfig_WorkerRole(t *testing.T) {
	isolateActiveProfileState(t)
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
	isolateActiveProfileState(t)
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
	isolateActiveProfileState(t)
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
	isolateActiveProfileState(t)
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
	isolateActiveProfileState(t)
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

// TestInjectContainerConfig_OverlaysRuntimeActiveProfile is the #1207
// behaviour gate for `prism switch`: when the runtime state file selects a
// non-default profile, injectContainerConfig must produce a blob whose
// model fields reflect the active profile, not the nix default's. Without
// this, `prism switch <project>` (a new-session entry point) would silently
// ignore `prism profile use <name>`.
func TestInjectContainerConfig_OverlaysRuntimeActiveProfile(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateRoot)
	// Write the runtime state file pointing at gemini-hybrid.
	prismState := filepath.Join(stateRoot, "prism")
	if err := os.MkdirAll(prismState, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prismState, "active-profile"), []byte("gemini-hybrid\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pf := &config.ProfilesFile{
		Default: "anthropic",
		RoleMapping: map[string][]string{
			"primary":   {"coordinator"},
			"secondary": {"worker"},
		},
		Profiles: map[string]config.ProfileEntry{
			"anthropic": {
				"coordinator": {Provider: "anthropic", Model: "anthropic/claude-opus-4-7"},
				"worker":      {Provider: "anthropic", Model: "anthropic/claude-default-worker"},
			},
			"gemini-hybrid": {
				"coordinator": {Provider: "anthropic", Model: "anthropic/claude-opus-4-7"},
				"worker":      {Provider: "google", Model: "google/gemini-runtime-worker", Thinking: "medium"},
			},
		},
		// Pre-rendered blob carrying the anthropic-default model — exactly
		// what Nix bakes from pf.Default at build time.
		ContainerWorkerConfig: `{"$schema":"https://opencode.ai/opencode.json","default_agent":"worker","model":"anthropic/claude-default-worker","agent":{"worker":{"model":"anthropic/claude-default-worker","variant":"none"}}}`,
	}

	root := makeBareRoot(t, "feature-branch")
	opts := session.Opts{}
	worktreePath := filepath.Join(root, "feature-branch")

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig: %v", err)
	}

	// The injected blob must mention the gemini model, not the anthropic one.
	if !strings.Contains(opts.ConfigContent, "google/gemini-runtime-worker") {
		t.Errorf("ConfigContent = %q\nwant overlay with google/gemini-runtime-worker (runtime active profile)", opts.ConfigContent)
	}
	if strings.Contains(opts.ConfigContent, "anthropic/claude-default-worker") {
		t.Errorf("ConfigContent still contains the nix-default model anthropic/claude-default-worker — overlay did not apply")
	}
}
