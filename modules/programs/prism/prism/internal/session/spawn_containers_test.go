package session

// spawn_containers_test.go — SpawnSession integration coverage for the
// ContainersFlag SpawnOpts field.
//
// The mapping is two-pronged:
//   - opts.ContainersFlag=true → spawn_inputs.containers_flag=1 (written by
//     the centralised InsertSpawnInputs call inside SpawnSession).
//   - opts.ContainersFlag=true → agent_status.containers_enabled=1 (written
//     by an explicit d.SetContainersEnabled call after the seed).
//
// Both are required: the audit row records the CLI flag verbatim, and the
// runtime gate is the live signal the sidecar reads at startup to decide
// whether to start the podman socket proxy.

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestSpawnSession_ContainersFlag_TrueFlipsBothColumns verifies that a
// LayoutAgentOnly spawn with ContainersFlag=true ends with:
//
//   - spawn_inputs.containers_flag = 1 (the audit row), AND
//   - agent_status.containers_enabled = 1 (the runtime gate).
//
// Both surfaces are required: the sidecar reads containers_enabled at
// startup to decide whether to spin up the per-session filtering podman
// socket proxy; spawn_inputs.containers_flag is the immutable audit trail
// (so `prism stats compare` and `prism stats <id>` can reconstruct what
// the user asked for).
func TestSpawnSession_ContainersFlag_TrueFlipsBothColumns(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@branch-containers-on"
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "myrepo",
		Worktree:       "/worktrees/myrepo-branch",
		AgentRole:      "worker",
		Prompt:         "go",
		Layout:         LayoutAgentOnly,
		ContainersFlag: true,
		PIExtensionDir: testPIExtensionDir,
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	// Runtime gate: agent_status.containers_enabled = 1.
	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("CurrentStatus: got nil, want row")
	}
	if !st.ContainersEnabled {
		t.Error("agent_status.containers_enabled = false, want true after SpawnSession with ContainersFlag=true")
	}

	// Audit row: spawn_inputs.containers_flag = 1. Look it up via the
	// instance_id SpawnSession minted host-side.
	if st.InstanceID == nil || *st.InstanceID == "" {
		t.Fatal("instance_id missing on agent_status row; cannot look up spawn_inputs")
	}
	si, err := d.SpawnInputsByInstanceID(*st.InstanceID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if si == nil {
		t.Fatal("SpawnInputsByInstanceID: got nil, want row written by SpawnSession")
	}
	if !si.ContainersFlag {
		t.Error("spawn_inputs.containers_flag = false, want true after SpawnSession with ContainersFlag=true")
	}
}

// TestSpawnSession_ContainersFlag_DefaultLeavesBothZero verifies the
// default case — a SpawnSession invocation that does NOT set
// ContainersFlag leaves both columns at the schema default of 0.
//
// It also guards against an accidental future "default-on" regression: a
// SpawnOpts field with a non-zero default value would silently enable
// the proxy on every spawn, defeating the opt-in nature of --containers.
func TestSpawnSession_ContainersFlag_DefaultLeavesBothZero(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@branch-containers-default"
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "myrepo",
		Worktree:       "/worktrees/myrepo-branch",
		AgentRole:      "worker",
		Prompt:         "go",
		Layout:         LayoutAgentOnly,
		PIExtensionDir: testPIExtensionDir,
		// ContainersFlag deliberately omitted.
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("CurrentStatus: got nil, want row")
	}
	if st.ContainersEnabled {
		t.Error("agent_status.containers_enabled = true on default spawn; want false (opt-in only)")
	}

	if st.InstanceID == nil || *st.InstanceID == "" {
		t.Fatal("instance_id missing on agent_status row; cannot look up spawn_inputs")
	}
	si, err := d.SpawnInputsByInstanceID(*st.InstanceID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if si == nil {
		t.Fatal("SpawnInputsByInstanceID: got nil, want row written by SpawnSession")
	}
	if si.ContainersFlag {
		t.Error("spawn_inputs.containers_flag = true on default spawn; want false")
	}
}

// TestSpawnInputsFromOpts_ContainersFlag_RoundTrip verifies the writer-only
// shape (no SpawnSession): SpawnInputsFromOpts maps SpawnOpts.ContainersFlag
// onto db.SpawnInputs.ContainersFlag in both directions (true and false).
//
// Companion to the cmd-package writer tests; this one lives in
// internal/session so the test is colocated with the helper it exercises.
func TestSpawnInputsFromOpts_ContainersFlag_RoundTrip(t *testing.T) {
	for _, want := range []bool{true, false} {
		want := want
		t.Run(boolName(want), func(t *testing.T) {
			si := SpawnInputsFromOpts(SpawnOpts{
				InstanceID:     "iid-" + boolName(want),
				ContainersFlag: want,
			})
			if si.ContainersFlag != want {
				t.Errorf("spawnInputsFromOpts(ContainersFlag=%v).ContainersFlag = %v, want %v",
					want, si.ContainersFlag, want)
			}
		})
	}
}

func boolName(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// Compile-time sanity check: db.SpawnInputs.ContainersFlag exists and is a
// bool. If a future refactor flips it to int or *bool, this assignment
// fails to compile, surfacing the breakage in CI rather than silently
// breaking the writer path.
var _ = func() bool {
	var si db.SpawnInputs
	return si.ContainersFlag
}
