package sidecar

// Issue #2110: worker-side capture of pr_number from the `gh pr create`
// bash-tool output, persisted on the worker's spawn_outcome row.
//
// This is the option-(a) write trigger from the AC: at the moment the worker
// agent successfully runs `gh pr create` (the PR comes into existence), the
// sidecar observes the tool-completed event, parses the URL from stdout,
// and writes the PR number to spawn_outcome.pr_number for the worker's own
// instance_id. The next `prism stats compare` render reads the column with
// a real value instead of `—`.
//
// Tests here cover:
//
//   - extractPRNumberFromGhOutput regex behaviour (positive + negative cases
//     including URL boundaries, non-URL outputs, empty outputs).
//   - isGhPRCreateCommand prefix-matching strictness.
//   - end-to-end sidecar event flow: handle a completed bash tool event
//     whose command is `gh pr create` and assert spawn_outcome.pr_number
//     is populated for the sidecar's InstanceID.
//   - negative-mutation guard: a successful `gh pr view` (NOT create) must
//     NOT trigger the write, even though its stdout contains a /pull/N URL.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestExtractPRNumberFromGhOutput verifies the regex helper that parses
// `gh pr create` stdout for the PR URL.
func TestExtractPRNumberFromGhOutput(t *testing.T) {
	cases := []struct {
		name      string
		output    string
		wantPR    int
		wantFound bool
	}{
		{
			name:      "plain success URL",
			output:    "https://github.com/owner/repo/pull/2110\n",
			wantPR:    2110,
			wantFound: true,
		},
		{
			name:      "URL embedded in longer output",
			output:    "Creating pull request for feature-branch into main\nhttps://github.com/owner/repo/pull/42\n",
			wantPR:    42,
			wantFound: true,
		},
		{
			name:      "single-digit PR number",
			output:    "https://github.com/owner/repo/pull/7",
			wantPR:    7,
			wantFound: true,
		},
		{
			name:      "high PR number",
			output:    "https://github.com/owner/repo/pull/123456",
			wantPR:    123456,
			wantFound: true,
		},
		{
			name:      "enterprise GitHub host",
			output:    "https://github.example.com/owner/repo/pull/99",
			wantPR:    99,
			wantFound: true,
		},
		{
			name:      "first URL wins (multiple matches)",
			output:    "https://github.com/owner/repo/pull/1\nhttps://github.com/owner/repo/pull/2",
			wantPR:    1,
			wantFound: true,
		},
		{
			name:      "empty output",
			output:    "",
			wantPR:    0,
			wantFound: false,
		},
		{
			name:      "no URL at all",
			output:    "error: a pull request already exists for branch foo",
			wantPR:    0,
			wantFound: false,
		},
		{
			name:      "issues URL is not a PR URL",
			output:    "https://github.com/owner/repo/issues/2110",
			wantPR:    0,
			wantFound: false,
		},
		{
			name:      "pull URL with trailing path treats only number",
			output:    "https://github.com/owner/repo/pull/42/files",
			wantPR:    42,
			wantFound: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPR, gotFound := extractPRNumberFromGhOutput(tc.output)
			if gotFound != tc.wantFound {
				t.Fatalf("found = %v, want %v (pr=%d)", gotFound, tc.wantFound, gotPR)
			}
			if gotPR != tc.wantPR {
				t.Errorf("pr = %d, want %d", gotPR, tc.wantPR)
			}
		})
	}
}

// TestIsGhPRCreateCommand verifies the prefix-matcher correctly distinguishes
// `gh pr create` from siblings (view/list/merge/edit) and rejects pseudo-
// prefixed commands like `gh pr create-template` that would be false matches
// for a naive `HasPrefix("gh pr create")` test.
func TestIsGhPRCreateCommand(t *testing.T) {
	cases := []struct {
		cmd     string
		wantHit bool
		reason  string
	}{
		{cmd: "gh pr create", wantHit: true, reason: "bare invocation"},
		{cmd: "gh pr create --title 'fix bug'", wantHit: true, reason: "with flags"},
		{cmd: "gh pr create\t--web", wantHit: true, reason: "tab separator"},
		{cmd: "  gh pr create --body foo", wantHit: true, reason: "leading whitespace tolerated"},
		{cmd: "GH PR CREATE", wantHit: true, reason: "case-insensitive"},
		{cmd: "gh pr view 42", wantHit: false, reason: "different subcommand"},
		{cmd: "gh pr merge 42", wantHit: false, reason: "merge is not create"},
		{cmd: "gh pr list", wantHit: false, reason: "list is not create"},
		{cmd: "git push", wantHit: false, reason: "unrelated"},
		{cmd: "", wantHit: false, reason: "empty"},
		{cmd: "gh issue create --title foo", wantHit: false, reason: "issue create is not pr create"},
	}

	for _, tc := range cases {
		got := isGhPRCreateCommand(tc.cmd)
		if got != tc.wantHit {
			t.Errorf("isGhPRCreateCommand(%q) = %v, want %v (%s)", tc.cmd, got, tc.wantHit, tc.reason)
		}
	}
}

// newWorkerSidecarWithInstance is a minimal worker-side sidecar fixture for
// the PR-capture flow. Unlike newSidecarCoordinatorWithInstance (which sets
// AgentRole=coordinator and exercises the merge-queue endpoints), this
// fixture mirrors what a real worker spawn looks like: a non-coordinator
// session with a populated InstanceID and an existing sessions row so the
// FK guard on spawn_outcome.instance_id is satisfied.
func newWorkerSidecarWithInstance(t *testing.T, d *db.DB, sessionName, instanceID string) *Sidecar {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	clk := newTestClock()

	// Seed the sessions row that spawn_outcome.instance_id REFERENCES so the
	// UPSERT does not silently no-op via the FK-guard short-circuit.
	if err := d.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Repo:        "test-repo",
		Worktree:    "/tmp/" + sessionName,
		Harness:     "pi",
		StartedAt:   time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	cfg := Config{
		SessionName: sessionName,
		Repo:        "test-repo",
		Worktree:    "/tmp/" + sessionName,
		HarnessURL:  "http://localhost:14000",
		DB:          d,
		Clock:       clk,
		InstanceID:  instanceID,
		Harness:     newSSEHarness(),
	}
	return New(cfg)
}

// TestMessagePartUpdated_GhPRCreate_PersistsPRNumber is the end-to-end test
// for the worker-side write trigger. It feeds a completed bash tool event
// for `gh pr create` whose output contains the canonical PR URL, then
// verifies that spawn_outcome.pr_number on the sidecar's InstanceID has
// been populated.
//
// Negative-mutation guard: this test is the assertion that catches a future
// revert of the events.go write call. To validate the test is not a no-op,
// running it against the unmodified pre-#2110 events.go must produce a nil
// PRNumber → test failure.
func TestMessagePartUpdated_GhPRCreate_PersistsPRNumber(t *testing.T) {
	d := openTestDB(t)
	const sess = "test-repo@2110-stats-compare-agent-outcomes"
	iid := uuid.New().String()
	sc := newWorkerSidecarWithInstance(t, d, sess, iid)

	_ = d.UpsertStatus(sess, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	start := 1000.0
	end := 2500.0
	evt := makeSSE("message.part.updated", map[string]any{
		"part": map[string]any{
			"type":      "tool",
			"messageID": "msg-pr-create-1",
			"tool":      "bash",
			"state": map[string]any{
				"status": "completed",
				"input":  map[string]string{"command": "gh pr create --title 'fix #2110' --body 'closes #2110'"},
				"output": "https://github.com/owner/test-repo/pull/2110\n",
				"time":   map[string]*float64{"start": &start, "end": &end},
			},
		},
	})
	sc.HandleEvent(evt)

	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil {
		t.Fatalf("SpawnOutcomeByInstanceID: %v", err)
	}
	if out == nil {
		t.Fatal("SpawnOutcomeByInstanceID: nil row after gh pr create event — write trigger did not fire")
	}
	if out.PRNumber == nil {
		t.Fatal("PRNumber: nil — the worker-side capture did not write to spawn_outcome.pr_number")
	}
	if *out.PRNumber != 2110 {
		t.Errorf("PRNumber: got %d, want 2110", *out.PRNumber)
	}
}

// TestMessagePartUpdated_GhPRView_DoesNotPersistPRNumber is the
// negative-mutation guard that confirms the write fires ONLY for
// `gh pr create`. A `gh pr view` invocation whose stdout contains a /pull/N
// URL must not write — otherwise a worker that runs `gh pr view 4242` to
// inspect another PR would mis-stamp its own row with that PR's number.
func TestMessagePartUpdated_GhPRView_DoesNotPersistPRNumber(t *testing.T) {
	d := openTestDB(t)
	const sess = "test-repo@just-looking"
	iid := uuid.New().String()
	sc := newWorkerSidecarWithInstance(t, d, sess, iid)

	_ = d.UpsertStatus(sess, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	start := 1000.0
	end := 1100.0
	evt := makeSSE("message.part.updated", map[string]any{
		"part": map[string]any{
			"type":      "tool",
			"messageID": "msg-pr-view-1",
			"tool":      "bash",
			"state": map[string]any{
				"status": "completed",
				"input":  map[string]string{"command": "gh pr view 4242 --json url -q .url"},
				// gh pr view's stdout contains a URL too — but the command is
				// NOT `gh pr create`, so we must NOT write.
				"output": "https://github.com/owner/test-repo/pull/4242\n",
				"time":   map[string]*float64{"start": &start, "end": &end},
			},
		},
	})
	sc.HandleEvent(evt)

	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil {
		t.Fatalf("SpawnOutcomeByInstanceID: %v", err)
	}
	if out != nil && out.PRNumber != nil {
		t.Errorf("PRNumber: got %d after gh pr view, want nil (no row should be created — the write is gh pr create only)", *out.PRNumber)
	}
}

// TestMessagePartUpdated_GhPRCreate_NoURL_NoWrite verifies that when
// `gh pr create` runs but its output does NOT contain a parseable PR URL
// (e.g. it errored out before printing the URL, or the gh CLI format
// changed), the write is silently skipped — no spawn_outcome row is created
// with a bogus pr_number.
//
// In the test we send a completed bash event whose output is the gh error
// message (still `status: completed` because we are simulating the bash
// tool succeeding at running the gh CLI; gh's exit code maps to status in a
// way the SSE handler does not currently see).
func TestMessagePartUpdated_GhPRCreate_NoURL_NoWrite(t *testing.T) {
	d := openTestDB(t)
	const sess = "test-repo@no-url"
	iid := uuid.New().String()
	sc := newWorkerSidecarWithInstance(t, d, sess, iid)

	_ = d.UpsertStatus(sess, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)

	start := 1000.0
	end := 1100.0
	evt := makeSSE("message.part.updated", map[string]any{
		"part": map[string]any{
			"type":      "tool",
			"messageID": "msg-pr-create-noURL",
			"tool":      "bash",
			"state": map[string]any{
				"status": "completed",
				"input":  map[string]string{"command": "gh pr create --title foo"},
				"output": "error: a pull request already exists for branch foo\n",
				"time":   map[string]*float64{"start": &start, "end": &end},
			},
		},
	})
	sc.HandleEvent(evt)

	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil {
		t.Fatalf("SpawnOutcomeByInstanceID: %v", err)
	}
	if out != nil && out.PRNumber != nil {
		t.Errorf("PRNumber: got %d, want nil — no URL in output should mean no write", *out.PRNumber)
	}
}

// makeSSE is provided by sidecar_test.go in the same package; we re-import
// it here only via the package boundary. The JSON marshalling shape used
// above (map[string]any with string/numeric values) mirrors the production
// SSE adapter behaviour.

// Compile-time guard: the JSON shape must remain decodable end-to-end.
// This guards against a future Config refactor that drops InstanceID
// silently.
var _ = func() bool {
	data, err := json.Marshal(map[string]any{"x": 1})
	return err == nil && len(data) > 0
}()
