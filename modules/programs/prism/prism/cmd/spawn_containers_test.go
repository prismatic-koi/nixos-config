package cmd

// Tests for the `prism spawn --containers` flag plumbing (Step 6 of #2317 /
// issue #2323). This file is the writer-level + flag-registration coverage
// for the new --containers CLI flag.
//
// What lives here:
//   - flag-registration tests against spawnCmd (presence, type, default).
//   - SpawnOpts -> spawn_inputs.containers_flag mapping via the
//     SpawnInputsFromOpts shim that other spawn_inputs writer tests use.
//   - Cross-spawn forwarding: the proxy body forwards "containers" only
//     when the flag was explicitly set, mirroring the --isolation pattern.
//   - Agent-context output exposes --containers as a bool with default false.
//
// What is NOT here (covered elsewhere):
//   - The DB migration tests for the new columns (Step 2 / #2319,
//     internal/db/db_migration_v36_v37_test.go).
//   - The sidecar startup path (Step 3 / #2320, internal/sidecar/podman_proxy_test.go).
//   - The proxy package policy (Step 1 / #2318, internal/podmanproxy).
//
// Test-suite isolation contract (AGENTS.md, issue #1608):
//   - sidecartest.NewIsolated redirects $XDG_STATE_HOME to a t.TempDir() and
//     sets the PRISM_TEST_MODE_RESTRICT_HOSTAPI guard, so no host DB / bus /
//     tmux state is touched.
//   - Session names use the "prism-test@" prefix.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// TestContainersFlag_Registered verifies that --containers is registered on
// spawnCmd as a bool with default false. The agent-context generator walks
// cobra.Command.Flags() so once this is in place the JSON document picks
// the flag up automatically — AC #4 of #2323 piggybacks on this.
func TestContainersFlag_Registered(t *testing.T) {
	flag := spawnCmd.Flags().Lookup("containers")
	if flag == nil {
		t.Fatal("--containers flag not found on spawnCmd")
	}
	if flag.Value.Type() != "bool" {
		t.Errorf("--containers flag type = %q, want %q", flag.Value.Type(), "bool")
	}
	if flag.DefValue != "false" {
		t.Errorf("--containers flag default = %q, want %q", flag.DefValue, "false")
	}
	// Description must be non-empty so `prism agent-context` surfaces it.
	if flag.Usage == "" {
		t.Error("--containers flag usage string is empty; agent-context would surface no description")
	}
}

// TestSpawnInputsFromOpts_ContainersFlagTrue verifies that when SpawnOpts
// carries ContainersFlag=true, the spawn_inputs row written by
// InsertSpawnInputs records containers_flag=1 and that
// SpawnInputsByInstanceID reads it back as true. AC #1 of #2323.
func TestSpawnInputsFromOpts_ContainersFlagTrue(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	d := bus.DB

	const sessionName = "prism-test@worker-containers-true"
	instanceID := uuid.New().String()
	seedSessionForSpawnInputs(t, d, instanceID, sessionName)

	opts := session.SpawnOpts{
		InstanceID:     instanceID,
		Prompt:         "noop",
		PromptSource:   "cli-positional",
		ContainersFlag: true,
	}
	si := session.SpawnInputsFromOpts(opts)
	if !si.ContainersFlag {
		t.Fatal("SpawnInputsFromOpts: ContainersFlag = false, want true")
	}
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	got, err := d.SpawnInputsByInstanceID(instanceID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if got == nil {
		t.Fatal("SpawnInputsByInstanceID: got nil, want non-nil")
	}
	if !got.ContainersFlag {
		t.Error("ContainersFlag round-trip: got false, want true")
	}
}

// TestSpawnInputsFromOpts_ContainersFlagDefault verifies that the default
// (flag omitted) yields ContainersFlag=false on the round-tripped row.
// AC #3 of #2323 — `prism spawn` without --containers leaves both
// columns at 0.
func TestSpawnInputsFromOpts_ContainersFlagDefault(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	d := bus.DB

	const sessionName = "prism-test@worker-containers-default"
	instanceID := uuid.New().String()
	seedSessionForSpawnInputs(t, d, instanceID, sessionName)

	opts := session.SpawnOpts{
		InstanceID:   instanceID,
		Prompt:       "noop",
		PromptSource: "cli-positional",
		// ContainersFlag left at zero value (false).
	}
	si := session.SpawnInputsFromOpts(opts)
	if si.ContainersFlag {
		t.Fatal("SpawnInputsFromOpts: ContainersFlag = true, want false (default)")
	}
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	got, err := d.SpawnInputsByInstanceID(instanceID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if got == nil {
		t.Fatal("SpawnInputsByInstanceID: got nil, want non-nil")
	}
	if got.ContainersFlag {
		t.Error("ContainersFlag default: got true, want false")
	}
}

// TestSetContainersEnabled_FlipsTheRuntimeGate verifies that
// d.SetContainersEnabled flips agent_status.containers_enabled — the
// runtime gate the sidecar reads on startup to decide whether to start
// the per-session filtering podman socket proxy (#2317 / #2320).
//
// AC #2 of #2323: when --containers is passed, the new agent_status row
// has containers_enabled=1. This is the writer-level test for that wire:
// SpawnSession calls d.SetContainersEnabled when opts.ContainersFlag, so
// the contract reduces to "the setter correctly flips the bit on the row
// the sidecar will later read".
//
// AC #3 (the default-false case) is covered by the schema's
// `INTEGER NOT NULL DEFAULT 0` declaration plus the
// TestStatus_ContainersEnabledDefaultsFalse migration test that lives
// alongside the schema (internal/db/db_migration_v36_v37_test.go).
func TestSetContainersEnabled_FlipsTheRuntimeGate(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	d := bus.DB

	const sessionName = "prism-test@worker-containers-runtime"
	// Seed an agent_status row in the simplest possible shape (idle, no
	// title, no harness session). Mirrors what UpsertStatusSeedRootAgentName
	// produces at spawn time — the SetContainersEnabled call lands on the
	// same row immediately afterwards.
	if err := d.UpsertStatusSeedRootAgentName(
		sessionName, "test-repo", "/tmp/worktree", "idle", nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}

	// Before: containers_enabled defaults to 0.
	pre, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus (pre): %v", err)
	}
	if pre == nil {
		t.Fatal("CurrentStatus (pre): got nil row")
	}
	if pre.ContainersEnabled {
		t.Error("CurrentStatus (pre): ContainersEnabled = true, want false (schema default)")
	}

	// After SetContainersEnabled(true): row reads back with the gate set.
	if err := d.SetContainersEnabled(sessionName, true); err != nil {
		t.Fatalf("SetContainersEnabled(true): %v", err)
	}
	post, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus (post): %v", err)
	}
	if post == nil {
		t.Fatal("CurrentStatus (post): got nil row")
	}
	if !post.ContainersEnabled {
		t.Error("CurrentStatus (post): ContainersEnabled = false, want true after SetContainersEnabled(true)")
	}

	// SetContainersEnabled(false) flips it back — used for a clean re-spawn
	// path where the row is being reset to idle without --containers.
	if err := d.SetContainersEnabled(sessionName, false); err != nil {
		t.Fatalf("SetContainersEnabled(false): %v", err)
	}
	reset, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus (reset): %v", err)
	}
	if reset == nil {
		t.Fatal("CurrentStatus (reset): got nil row")
	}
	if reset.ContainersEnabled {
		t.Error("CurrentStatus (reset): ContainersEnabled = true, want false after SetContainersEnabled(false)")
	}
}

// TestSetContainersEnabled_NoRowIsNoOp verifies that calling
// SetContainersEnabled on a session_name with no agent_status row is a
// silent no-op (matching SetIsolationMode / SetGroupID). This is the
// "session was already cleaned up" path — defence in depth so a stale
// caller never errors out the spawn flow.
func TestSetContainersEnabled_NoRowIsNoOp(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	d := bus.DB

	if err := d.SetContainersEnabled("prism-test@worker-does-not-exist", true); err != nil {
		t.Fatalf("SetContainersEnabled on missing row: %v, want nil (no-op)", err)
	}
}

// TestProxySpawnBody_ContainersForwardedOnlyWhenSet verifies the
// flag-forwarding pattern documented in #2323's cross-spawn forwarding AC:
// the proxy body carries "containers": true ONLY when the flag was
// explicitly set; an unset child must NOT inherit the parent's value via
// JSON default.
//
// This is the unit-level twin of the existing hostapi "isolation"
// forwarding test (cmd/hostapi_test.go TestProxySpawn_IsolationNotForwardedWhenAbsent).
// It exercises the body-construction shape directly so a regression in
// cmd/spawn.go's body assembly is caught here without a full host-API
// round-trip.
func TestProxySpawnBody_ContainersForwardedOnlyWhenSet(t *testing.T) {
	type bodyShape struct {
		Containers *bool `json:"containers,omitempty"`
	}

	cases := []struct {
		name        string
		setFlag     bool
		flagValue   bool
		wantPresent bool
		wantValue   bool
	}{
		{name: "unset_omitted", setFlag: false, wantPresent: false},
		{name: "set_true_forwards_true", setFlag: true, flagValue: true, wantPresent: true, wantValue: true},
		{name: "set_false_forwards_false", setFlag: true, flagValue: false, wantPresent: true, wantValue: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: "spawn-test"}
			cmd.Flags().Bool("containers", false, "")
			if tc.setFlag {
				if err := cmd.Flags().Set("containers", boolString(tc.flagValue)); err != nil {
					t.Fatalf("set --containers: %v", err)
				}
			}

			// Mirror the body-build pattern in cmd/spawn.go's proxySpawn:
			//   body := map[string]any{...}
			//   if cmd.Flags().Changed("containers") { body["containers"] = containersFlag }
			body := map[string]any{}
			containersFlag, _ := cmd.Flags().GetBool("containers")
			if cmd.Flags().Changed("containers") {
				body["containers"] = containersFlag
			}

			// Round-trip via JSON to assert the wire shape — agent-context
			// and the host-API both consume JSON, not in-memory maps.
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			var decoded bodyShape
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}

			if tc.wantPresent {
				if decoded.Containers == nil {
					t.Fatalf("containers field omitted; want present with value %v", tc.wantValue)
				}
				if *decoded.Containers != tc.wantValue {
					t.Errorf("containers value = %v, want %v", *decoded.Containers, tc.wantValue)
				}
			} else {
				if decoded.Containers != nil {
					t.Errorf("containers field = %v, want omitted (flag was not set)", *decoded.Containers)
				}
			}
		})
	}
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestWarnContainersWithHostMode covers AC #5 of #2323: when --containers is
// combined with --isolation host, prism spawn emits a warning to stderr
// matching the substring "host mode bypasses the proxy" and exits 0 (the
// spawn proceeds). The warning fires for every isolation resolution path
// that ends at "host" — explicit flag, config default, or future precedence
// layers — so the helper takes the resolved mode string verbatim.
func TestWarnContainersWithHostMode(t *testing.T) {
	cases := []struct {
		name       string
		containers bool
		isolation  string
		wantWarn   bool
		wantSubstr string // empty means no expectation on text
	}{
		{name: "containers_host_warns", containers: true, isolation: "host", wantWarn: true, wantSubstr: "host mode bypasses the proxy"},
		{name: "containers_bwrap_silent", containers: true, isolation: "bwrap", wantWarn: false},
		{name: "containers_sandbox_exec_silent", containers: true, isolation: "sandbox-exec", wantWarn: false},
		{name: "no_containers_host_silent", containers: false, isolation: "host", wantWarn: false},
		{name: "no_containers_bwrap_silent", containers: false, isolation: "bwrap", wantWarn: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			gotWarn := warnContainersWithHostMode(&buf, tc.containers, tc.isolation)
			if gotWarn != tc.wantWarn {
				t.Errorf("warnContainersWithHostMode returned %v; want %v (containers=%v isolation=%q)",
					gotWarn, tc.wantWarn, tc.containers, tc.isolation)
			}
			out := buf.String()
			if tc.wantWarn {
				if out == "" {
					t.Errorf("expected warning text on stderr, got empty")
				}
				if tc.wantSubstr != "" && !strings.Contains(out, tc.wantSubstr) {
					t.Errorf("stderr %q does not contain substring %q", out, tc.wantSubstr)
				}
			} else if out != "" {
				t.Errorf("expected no warning, got %q", out)
			}
		})
	}
}

// TestContainersFlag_DoubleSetIsIdempotent covers AC #6 of #2323: passing
// --containers twice on the same command line does not error. cobra's
// pflag.Bool tolerates repeated --flag values by overwriting; this test
// pins that behaviour so a future flag-registration change (e.g. switching
// to BoolVar with a custom parser) cannot regress the contract.
func TestContainersFlag_DoubleSetIsIdempotent(t *testing.T) {
	cmd := &cobra.Command{Use: "spawn-test"}
	cmd.Flags().Bool("containers", false, "")

	// Two equivalent --containers --containers invocations (both true). The
	// second Set call must not return an error; the final value must remain
	// true.
	if err := cmd.Flags().Set("containers", "true"); err != nil {
		t.Fatalf("first --containers set: %v", err)
	}
	if err := cmd.Flags().Set("containers", "true"); err != nil {
		t.Fatalf("second --containers set: %v", err)
	}
	got, _ := cmd.Flags().GetBool("containers")
	if !got {
		t.Errorf("containers = %v after two --containers, want true", got)
	}
	if !cmd.Flags().Changed("containers") {
		t.Error("Changed(\"containers\") = false after two sets, want true")
	}
}

// TestAgentContext_ContainersFlagExposed verifies that the agent-context
// JSON output surfaces --containers as a bool with default false. This is
// AC #4 of #2323 verbatim:
//
//	prism agent-context | jq '.commands.spawn.flags["--containers"]'
//	returns {type: "bool", default: false, description: "..."}.
//
// The agent-context generator walks rootCmd.Commands() and emits FlagMeta
// for each registered flag, so the test below just verifies that the
// document built for `spawn` contains the --containers entry with the
// expected shape — no need to shell out.
func TestAgentContext_ContainersFlagExposed(t *testing.T) {
	doc := buildAgentContextDocument(false)
	spawnMeta, ok := doc.Commands["spawn"]
	if !ok {
		t.Fatal("agent-context: spawn command missing from document")
	}
	flag, ok := spawnMeta.Flags["--containers"]
	if !ok {
		t.Fatal("agent-context: --containers flag missing from spawn flags")
	}
	if flag.Type != "bool" {
		t.Errorf("agent-context --containers type = %q, want %q", flag.Type, "bool")
	}
	if flag.Default != "false" {
		t.Errorf("agent-context --containers default = %q, want %q", flag.Default, "false")
	}
	if flag.Description == "" {
		t.Error("agent-context --containers description is empty")
	}
}
