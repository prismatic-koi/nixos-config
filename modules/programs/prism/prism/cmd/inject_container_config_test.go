package cmd

// Unit tests for the injectContainerConfig helper (switch.go).
//
// injectContainerConfig derives the agent role from the worktree path,
// resolves the active profile, and calls BuildConfigContent to generate
// the pi harness-config JSON. These tests exercise:
//
//   - Worker role (non-"main" directory base, parent has .bare) generates a
//     config carrying the worker slot's model from the active profile.
//   - Coordinator role ("main" directory base, parent has .bare) generates a
//     config carrying the coordinator slot's model.
//   - Non-worktree path (parent has no .bare) is treated as coordinator.
//   - An explicit Agent override is respected over DefaultAgent derivation.
//   - Missing profiles file leaves ConfigContent empty (non-fatal).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/session"
)

// makeInjectTestProfile builds a ProfilesFile with distinct worker and
// coordinator models so tests can verify role resolution.
func makeInjectTestProfile() *config.ProfilesFile {
	return &config.ProfilesFile{
		Default: "test",
		Profiles: map[string]config.ProfileEntry{
			"test": {
				"coordinator": {Provider: "anthropic", Model: "test-coordinator-model"},
				"worker":      {Provider: "anthropic", Model: "test-worker-model"},
			},
		},
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
// so the active-profile state file lookup cannot read the developer's real state.
func isolateActiveProfileState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

// parseModel extracts the top-level "model" field from a JSON blob.
func parseModel(t *testing.T, blob string) string {
	t.Helper()
	if blob == "" {
		return ""
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(blob), &cfg); err != nil {
		t.Fatalf("parseModel: invalid JSON %q: %v", blob, err)
	}
	if m, ok := cfg["model"].(string); ok {
		return m
	}
	return ""
}

// writeActiveProfile writes the named profile to the state file so
// ResolveActiveProfile picks it up.
func writeActiveProfile(t *testing.T, stateRoot, profile string) {
	t.Helper()
	prismState := filepath.Join(stateRoot, "prism")
	if err := os.MkdirAll(prismState, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prismState, "active-profile"), []byte(profile+"\n"), 0o644); err != nil {
		t.Fatalf("write active-profile: %v", err)
	}
}

// TestInjectContainerConfig_WorkerRole verifies that a non-"main" worktree
// directory (parent has .bare) causes injectContainerConfig to set ConfigContent
// to the worker slot's model.
func TestInjectContainerConfig_WorkerRole(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateRoot)
	writeActiveProfile(t, stateRoot, "test")

	root := makeBareRoot(t, "feature-branch")
	pf := makeInjectTestProfile()
	opts := session.Opts{}
	worktreePath := filepath.Join(root, "feature-branch")

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig: %v", err)
	}

	if got := parseModel(t, opts.ConfigContent); got != "test-worker-model" {
		t.Errorf("ConfigContent model = %q, want test-worker-model (worker slot)", got)
	}
}

// TestInjectContainerConfig_CoordinatorRole verifies that a worktree directory
// named "main" (parent has .bare) causes injectContainerConfig to use the
// coordinator slot.
func TestInjectContainerConfig_CoordinatorRole(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateRoot)
	writeActiveProfile(t, stateRoot, "test")

	root := makeBareRoot(t, "main")
	pf := makeInjectTestProfile()
	opts := session.Opts{}
	worktreePath := filepath.Join(root, "main")

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig: %v", err)
	}

	if got := parseModel(t, opts.ConfigContent); got != "test-coordinator-model" {
		t.Errorf("ConfigContent model = %q, want test-coordinator-model (coordinator slot)", got)
	}
}

// TestInjectContainerConfig_NonWorktreePath verifies that a non-worktree path
// (parent directory has no .bare) is treated as coordinator.
func TestInjectContainerConfig_NonWorktreePath(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateRoot)
	writeActiveProfile(t, stateRoot, "test")

	// Plain temp dir — no .bare parent.
	worktreePath := t.TempDir()
	pf := makeInjectTestProfile()
	opts := session.Opts{}

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig: %v", err)
	}

	if got := parseModel(t, opts.ConfigContent); got != "test-coordinator-model" {
		t.Errorf("ConfigContent model = %q, want test-coordinator-model (coordinator for non-worktree path)", got)
	}
}

// TestInjectContainerConfig_ExplicitAgentOverride verifies that an explicit
// opts.Agent override is respected: when opts.Agent is "coordinator", the
// coordinator slot is selected even for a non-"main" directory.
func TestInjectContainerConfig_ExplicitAgentOverride(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateRoot)
	writeActiveProfile(t, stateRoot, "test")

	root := makeBareRoot(t, "feature-branch")
	pf := makeInjectTestProfile()
	opts := session.Opts{Agent: "coordinator"}
	worktreePath := filepath.Join(root, "feature-branch")

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig: %v", err)
	}

	// "coordinator" override must win over DefaultAgent("feature-branch") = "worker".
	if got := parseModel(t, opts.ConfigContent); got != "test-coordinator-model" {
		t.Errorf("ConfigContent model = %q, want test-coordinator-model (coordinator override)", got)
	}
}

// TestInjectContainerConfig_NoProfileLeaveEmptyContent verifies that when
// no profile is active and no model/variant override is set, ConfigContent is
// empty (BuildConfigContent returns "").
func TestInjectContainerConfig_NoProfileLeaveEmptyContent(t *testing.T) {
	isolateActiveProfileState(t)

	// Profiles file with no default and no state-file profile set.
	pf := &config.ProfilesFile{
		Default: "", // no default
		Profiles: map[string]config.ProfileEntry{},
	}
	root := makeBareRoot(t, "feature-branch")
	opts := session.Opts{}
	worktreePath := filepath.Join(root, "feature-branch")

	if err := injectContainerConfig(worktreePath, pf, &opts, "test"); err != nil {
		t.Fatalf("injectContainerConfig: %v", err)
	}

	if opts.ConfigContent != "" {
		t.Errorf("ConfigContent = %q, want empty (no profile active)", opts.ConfigContent)
	}
}
