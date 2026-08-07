package cmd

// retro_cycles_cli_test.go — end-to-end CLI coverage for `prism retro
// <train-session>` (issue #2584): the positional argument wires through
// ResolveSessionArg and review.AssembleReviewCycles to render section 3.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

func withRetroTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	SetTestDBPath(path)
	t.Cleanup(func() {
		SetTestDBPath("")
		d.Close()
	})
	return d
}

// TestRetroCLI_TrainArgument_NoReviewCycles verifies the edge-case AC end to
// end: `prism retro <train-session>` for a train with no session_groups rows
// states plainly that no review cycles ran, without erroring.
func TestRetroCLI_TrainArgument_NoReviewCycles(t *testing.T) {
	d := withRetroTestDB(t)
	const name = "retro-cli-repo@solo-worker"
	if err := d.InsertSession(db.Session{
		InstanceID:  "11111111-1111-1111-1111-111111111111",
		SessionName: name,
		Repo:        "retro-cli-repo",
		Worktree:    "/wt",
		Harness:     "pi",
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	rootCmd.SetArgs([]string{"retro", name})
	out := captureStdout(t, func() {
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("rootCmd.Execute: %v", err)
		}
	})

	if !strings.Contains(out, "no review cycles ran") {
		t.Errorf("expected 'no review cycles ran'; got:\n%s", out)
	}
}

// TestRetroCLI_TrainArgument_UnknownSession verifies an unresolvable train
// argument surfaces as a plain error rather than a panic or a silent
// empty-table render.
func TestRetroCLI_TrainArgument_UnknownSession(t *testing.T) {
	withRetroTestDB(t)

	rootCmd.SetArgs([]string{"retro", "no-such-session-at-all"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("rootCmd.Execute: got nil error, want an error for an unresolvable train argument")
	}
	if !strings.Contains(err.Error(), "retro:") {
		t.Errorf("error = %q, want it prefixed with \"retro:\"", err.Error())
	}
}

// TestRetroCLI_TrainArgument_WithReviewCycles verifies the core AC end to
// end: the review cycle's per-agent cost, turn count, and verdict render.
func TestRetroCLI_TrainArgument_WithReviewCycles(t *testing.T) {
	d := withRetroTestDB(t)
	const name = "retro-cli-repo@reviewed-worker"
	if err := d.InsertSession(db.Session{
		InstanceID:  "22222222-2222-2222-2222-222222222222",
		SessionName: name,
		Repo:        "retro-cli-repo",
		Worktree:    "/wt",
		Harness:     "pi",
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	groupID, err := d.RegisterGroupWithPR(name, "7", 1)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}
	const agentSess = "retro-cli-repo@reviewed-worker~review-1-review-goal"
	if err := d.UpsertStatus(agentSess, "retro-cli-repo", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetGroupID(agentSess, groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}
	if err := d.SetEnded(agentSess); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	if err := d.WriteEvent(db.Event{
		ID:          "retro-cli-evt-1",
		SessionName: agentSess,
		Repo:        "retro-cli-repo",
		Worktree:    "/wt",
		Type:        "msg_assistant",
		Payload:     `{"text":"<verdict>PASS</verdict>","outputTokens":42,"cost":0.75}`,
	}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	rootCmd.SetArgs([]string{"retro", name})
	out := captureStdout(t, func() {
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("rootCmd.Execute: %v", err)
		}
	})

	for _, want := range []string{"round 1", "review-goal", "PASS", "$0.75"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q; got:\n%s", want, out)
		}
	}
}

// TestRetroCLI_TrainArgument_JSON verifies --json with a train argument emits
// the review_cycles field alongside the base report fields.
func TestRetroCLI_TrainArgument_JSON(t *testing.T) {
	d := withRetroTestDB(t)
	const name = "retro-cli-repo@json-worker"
	if err := d.InsertSession(db.Session{
		InstanceID:  "33333333-3333-3333-3333-333333333333",
		SessionName: name,
		Repo:        "retro-cli-repo",
		Worktree:    "/wt",
		Harness:     "pi",
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	rootCmd.SetArgs([]string{"retro", name, "--json"})
	out := captureStdout(t, func() {
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("rootCmd.Execute: %v", err)
		}
	})

	for _, want := range []string{`"train":"retro-cli-repo@json-worker"`, `"review_cycles":[]`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected json output to contain %q; got:\n%s", want, out)
		}
	}
}
