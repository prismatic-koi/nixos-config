package review_test

// spawn_opts_harness_pipe_test.go — regression tests for issue #2114.
//
// Both fan-out call sites in internal/review/run.go (sync Run and async
// RunAsync) build their per-reviewer session.SpawnOpts via the shared
// newReviewerSpawnOpts builder introduced by PR #2115 (issue #2097's
// post-merge consolidation). Before #2114 the builder left
// SpawnOpts.HarnessPipeSockPath unset unconditionally — which made the
// PI extension no-op out for host-mode invokers, the `--agent` flag
// never register, and pi reject `--agent review-<role> --prompt "..."`
// as `Unknown options`.
//
// These tests exercise the builder directly via the exported test shim
// review.NewReviewerSpawnOptsForTest (export_test.go) so the gate is
// covered without spinning up tmux, sidecar, or a real DB row:
//
//   - TestNewReviewerSpawnOpts_HostMode_PopulatesHarnessPipeSockPath
//     proves the gate fires when (HarnessName="pi", IsolationMode="host").
//   - TestNewReviewerSpawnOpts_BwrapMode_LeavesHarnessPipeSockPathEmpty
//     proves the gate does real work — for container-mode review
//     invokers HarnessPipeSockPath stays empty, so the existing
//     container-mode injection paths (bwrap --setenv, sandbox-exec
//     profile, podman --env) remain responsible for PRISM_HARNESS_PIPE.
//   - TestNewReviewerSpawnOpts_SandboxExecMode_LeavesHarnessPipeSockPathEmpty
//     mirrors the bwrap negative for sandbox-exec, the second container
//     mode that the review fan-out can resolve to on Darwin hosts.
//
// Because both Run and RunAsync delegate to newReviewerSpawnOpts, a
// single positive + a pair of negatives covers both fan-out paths
// simultaneously — the post-#2115 single-builder shape collapses what
// would otherwise be four separate test surfaces (sync + async × pos +
// neg) into the three below.
//
// Test-suite isolation contract (AGENTS.md, issue #1608): these tests
// are pure functions over the builder — they touch no DB, no sidecar,
// no tmux. No sidecartest.NewIsolated is required. They also do not
// use the "prism-test@" prefix for the AgentSession because they never
// hit a live coordinator surface; the session name is consumed only by
// session.SidecarHarnessPipePath which is a pure path-derivation
// helper.

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
)

// newReviewerSpawnInputForTest returns a ReviewerSpawnInputForTest
// pre-populated with everything newReviewerSpawnOpts needs to produce
// a valid SpawnOpts. Tests override IsolationMode / HarnessName as
// required to exercise each gate branch.
func newReviewerSpawnInputForTest(t *testing.T, agentSession, isolationMode, harnessName string) review.ReviewerSpawnInputForTest {
	t.Helper()
	h, err := harness.New(harnessName, "", nil, "", "")
	if err != nil {
		t.Fatalf("harness.New(%q): %v", harnessName, err)
	}
	return review.ReviewerSpawnInputForTest{
		AgentName:          "review-goal",
		AgentSession:       agentSession,
		Prompt:             "unused-in-this-test",
		AgentConfigContent: "",
		Repo:               "prism-test",
		Worktree:           "/tmp/" + agentSession,
		PromptTemplateHash: "test-template-hash",
		IsolationMode:      isolationMode,
		HarnessName:        harnessName,
		HarnessHandle:      h,
		ProfileName:        "test-profile",
	}
}

// TestNewReviewerSpawnOpts_HostMode_PopulatesHarnessPipeSockPath verifies
// AC: when HarnessName="pi" (socket-pipe transport) and
// IsolationMode="host", the builder pre-computes SpawnOpts.HarnessPipeSockPath
// via session.SidecarHarnessPipePath(AgentSession).
//
// This is the failure case in #2114: before the fix HarnessPipeSockPath
// was left empty for host-mode review invokers, the PI extension no-op'd,
// and pi rejected --agent/--prompt as unknown flags — leaving every
// review agent idle forever.
func TestNewReviewerSpawnOpts_HostMode_PopulatesHarnessPipeSockPath(t *testing.T) {
	const agentSession = "coord@host-mode~review-1-review-goal"

	in := newReviewerSpawnInputForTest(t, agentSession, "host", "pi")
	opts := review.NewReviewerSpawnOptsForTest(in)

	// Sanity: the input propagated as expected.
	if opts.SessionName != agentSession {
		t.Fatalf("SpawnOpts.SessionName = %q, want %q", opts.SessionName, agentSession)
	}
	if opts.HarnessName != "pi" {
		t.Errorf("SpawnOpts.HarnessName = %q, want %q", opts.HarnessName, "pi")
	}
	if opts.IsolationMode != "host" {
		t.Errorf("SpawnOpts.IsolationMode = %q, want %q", opts.IsolationMode, "host")
	}

	wantPath, err := session.SidecarHarnessPipePath(agentSession)
	if err != nil {
		t.Fatalf("session.SidecarHarnessPipePath(%q): %v", agentSession, err)
	}
	if opts.HarnessPipeSockPath != wantPath {
		t.Errorf("SpawnOpts.HarnessPipeSockPath = %q, want %q (issue #2114: host-mode pi review fan-out must populate the harness pipe sock path so agentPaneEnvVars emits PRISM_HARNESS_PIPE)",
			opts.HarnessPipeSockPath, wantPath)
	}
}

// TestNewReviewerSpawnOpts_BwrapMode_LeavesHarnessPipeSockPathEmpty
// verifies the gate does real work: when IsolationMode="bwrap" the
// builder leaves SpawnOpts.HarnessPipeSockPath empty so the existing
// container-mode injection path (bwrap.go --setenv) remains responsible
// for PRISM_HARNESS_PIPE.
//
// Without this assertion a buggy fix that unconditionally set
// HarnessPipeSockPath for all isolation modes would still pass the
// host-mode positive — this is the negative half of the gate-correctness
// proof and mirrors the discipline PR #2113 established for the
// cmd/investigate.go gate.
func TestNewReviewerSpawnOpts_BwrapMode_LeavesHarnessPipeSockPathEmpty(t *testing.T) {
	const agentSession = "coord@bwrap-mode~review-1-review-goal"

	in := newReviewerSpawnInputForTest(t, agentSession, "bwrap", "pi")
	opts := review.NewReviewerSpawnOptsForTest(in)

	if opts.IsolationMode != "bwrap" {
		t.Errorf("SpawnOpts.IsolationMode = %q, want %q", opts.IsolationMode, "bwrap")
	}

	if opts.HarnessPipeSockPath != "" {
		t.Errorf("SpawnOpts.HarnessPipeSockPath = %q, want \"\" for bwrap review fan-out — container-mode injection paths (bwrap --setenv, sandbox-exec profile, podman --env) own PRISM_HARNESS_PIPE for container sessions; pre-computing the sock path here would double-inject (issue #2114 AC).",
			opts.HarnessPipeSockPath)
	}
}

// TestNewReviewerSpawnOpts_SandboxExecMode_LeavesHarnessPipeSockPathEmpty
// is the Darwin-side mirror of the bwrap negative. The review fan-out
// inherits its IsolationMode from the invoking worker, so on a Darwin
// host with sandbox-exec workers the same gate must leave
// HarnessPipeSockPath empty and defer to the sandbox-exec profile's
// PRISM_HARNESS_PIPE injection.
func TestNewReviewerSpawnOpts_SandboxExecMode_LeavesHarnessPipeSockPathEmpty(t *testing.T) {
	const agentSession = "coord@sbx-mode~review-1-review-goal"

	in := newReviewerSpawnInputForTest(t, agentSession, "sandbox-exec", "pi")
	opts := review.NewReviewerSpawnOptsForTest(in)

	if opts.IsolationMode != "sandbox-exec" {
		t.Errorf("SpawnOpts.IsolationMode = %q, want %q", opts.IsolationMode, "sandbox-exec")
	}

	if opts.HarnessPipeSockPath != "" {
		t.Errorf("SpawnOpts.HarnessPipeSockPath = %q, want \"\" for sandbox-exec review fan-out (issue #2114 AC).",
			opts.HarnessPipeSockPath)
	}
}
