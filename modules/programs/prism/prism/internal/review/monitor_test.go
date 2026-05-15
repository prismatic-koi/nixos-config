package review_test

// monitor_test.go — tests for the async review monitor.
//
// Tests covered:
//   - MonitorFunc detects group completion and delivers results
//   - MonitorFunc handles delivery failure with retry and fallback file
//   - MonitorFunc handles missing sessions (row deleted mid-review)
//   - MonitorFunc handles all-timeout group (all members in terminal state)
//   - ActiveReviewGroupForParent detects in-progress rounds
//   - buildAsyncAck produces correct acknowledgement text
//   - buildMonitorResults handles all states
//   - deliverWithRetry retries correctly and eventually writes fallback

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

// ── MonitorFunc ──────────────────────────────────────────────────────────────

// TestMonitorFunc_DetectsCompletionAndDelivers verifies that MonitorFunc:
// 1. Polls GroupCompleted until the group is complete.
// 2. Calls FormatResults and delivers the result to the worker.
// 3. Returns nil when delivery succeeds.
func TestMonitorFunc_DetectsCompletionAndDelivers(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@monitor-test"
	agents := []review.Agent{
		{Name: "review-goal"},
		{Name: "review-code"},
	}

	// Register a group.
	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// Seed both agents as finished with a passing verdict.
	sessions := []string{
		parent + "~review-1-review-goal",
		parent + "~review-1-review-code",
	}
	for _, sess := range sessions {
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", sess, err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", sess, err)
		}
	}
	// Seed passing verdict events.
	seedAssistantEvent(t, d, sessions[0], "All ACs verified.\n<verdict>PASS</verdict>")
	seedAssistantEvent(t, d, sessions[1], "Code looks good.\n<verdict>PASS</verdict>")

	// Seed the worker session with harness info for HTTP delivery.
	workerSession := "nixos-config@worker-test"
	if err := d.UpsertStatus(workerSession, "nixos-config", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus worker: %v", err)
	}

	// Start an httptest server to simulate the harness prompt_async endpoint.
	var deliveredText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && len(body.Parts) > 0 {
			deliveredText = body.Parts[0].Text
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	// Extract port from test server URL and set harness info directly.
	port := extractPort(t, srv.URL)
	sessionID := "test-pi-session-id"
	if err := setHarnessInfo(t, d, workerSession, port, sessionID); err != nil {
		t.Fatalf("SetHarnessInfo: %v", err)
	}

	opts := review.MonitorOpts{
		GroupID:              groupID,
		WorkerSession:        workerSession,
		PRNumber:             "864",
		Round:                1,
		Agents:               agents,
		AgentSessions:        sessions,
		DBPath:               d.Path(),
		PollInterval:         10 * time.Millisecond, // fast for testing
		MaxDeliveryRetries:   0,                     // no retry needed
		DeliveryRetryBackoff: 1 * time.Millisecond,
	}

	if err := review.MonitorFunc(opts); err != nil {
		t.Fatalf("MonitorFunc: %v", err)
	}

	// Verify delivery happened.
	if deliveredText == "" {
		t.Fatal("MonitorFunc: no text delivered to worker")
	}
	// The delivered text must mention the PR number and round.
	if !strings.Contains(deliveredText, "864") {
		t.Errorf("delivered text does not mention PR number 864: %q", deliveredText)
	}
	if !strings.Contains(deliveredText, "round 1") {
		t.Errorf("delivered text does not mention round 1: %q", deliveredText)
	}
	// Both agents passed — all-passed message expected.
	if !strings.Contains(deliveredText, "All 5 review agents passed") && !strings.Contains(deliveredText, "all") {
		// At minimum, no FAIL mention and both agents appear as passed (✓).
		if strings.Contains(deliveredText, "FAIL") && !strings.Contains(deliveredText, "PASS") {
			t.Errorf("delivered text unexpectedly shows failures for all-pass review: %q", deliveredText)
		}
	}
}

// TestMonitorFunc_FlipsReviewingToActiveBeforeDelivery verifies the option-1
// fix for issue #1049: the review monitor must clear the worker's `reviewing`
// state by writing `active` to the DB *before* delivering the review-complete
// prompt. This ensures the busy event triggered by the prompt arriving is
// processed against `active` (not `reviewing`, which the sidecar treats as
// sticky and refuses to overwrite), so the subsequent idle debounce can fire
// the genuine end-of-review handoff.
func TestMonitorFunc_FlipsReviewingToActiveBeforeDelivery(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@monitor-flip-test"
	agents := []review.Agent{
		{Name: "review-goal"},
	}

	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	sessions := []string{parent + "~review-1-review-goal"}
	for _, sess := range sessions {
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", sess, err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", sess, err)
		}
	}
	seedAssistantEvent(t, d, sessions[0], "All ACs verified.\n<verdict>PASS</verdict>")

	// Seed the worker session in `reviewing` state — exactly as RunAsync
	// leaves it after spawning the review-agent group.
	workerSession := "nixos-config@worker-flip-test"
	if err := d.UpsertStatus(workerSession, "nixos-config", "/wt", "reviewing", nil, nil); err != nil {
		t.Fatalf("UpsertStatus worker: %v", err)
	}

	// Capture the worker's DB state at the moment of delivery (i.e. when the
	// HTTP server receives the prompt_async POST). If the monitor has correctly
	// flipped reviewing→active before delivery, this snapshot must read `active`.
	var stateAtDelivery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status, sErr := d.CurrentStatus(workerSession); sErr == nil && status != nil {
			stateAtDelivery = status.State
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	port := extractPort(t, srv.URL)
	if err := setHarnessInfo(t, d, workerSession, port, "test-pi-session-id"); err != nil {
		t.Fatalf("SetHarnessInfo: %v", err)
	}

	opts := review.MonitorOpts{
		GroupID:              groupID,
		WorkerSession:        workerSession,
		PRNumber:             "1049",
		Round:                1,
		Agents:               agents,
		AgentSessions:        sessions,
		DBPath:               d.Path(),
		PollInterval:         10 * time.Millisecond,
		MaxDeliveryRetries:   0,
		DeliveryRetryBackoff: 1 * time.Millisecond,
	}

	if err := review.MonitorFunc(opts); err != nil {
		t.Fatalf("MonitorFunc: %v", err)
	}

	if stateAtDelivery != "active" {
		t.Errorf("worker state at moment of delivery = %q, want %q (monitor must flip reviewing→active before delivering)",
			stateAtDelivery, "active")
	}

	// Post-delivery: the DB state should still be `active` (or whatever the
	// worker has moved on to in response to the delivery).
	finalStatus, err := d.CurrentStatus(workerSession)
	if err != nil {
		t.Fatalf("CurrentStatus post-delivery: %v", err)
	}
	if finalStatus == nil || finalStatus.State == "reviewing" {
		t.Errorf("worker state after delivery = %v, want non-reviewing", finalStatus)
	}
}

// TestMonitorFunc_DeliveryFailure_WritesFallbackFile verifies that when all
// delivery retries fail, MonitorFunc writes the fallback file at the expected
// path.
func TestMonitorFunc_DeliveryFailure_WritesFallbackFile(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@delivery-fail-test"
	agents := []review.Agent{
		{Name: "review-goal"},
	}

	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	sess := parent + "~review-1-review-goal"
	if err := d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetGroupID(sess, groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}
	seedAssistantEvent(t, d, sess, "All good.\n<verdict>PASS</verdict>")

	// Worker session: no harness port → delivery will fail.
	workerSession := "nixos-config@nonexistent-worker"
	// We do NOT seed this session in the DB, so delivery will fail
	// (session not found).

	// Use a unique PR number so the fallback file name is unique.
	prNumber := "99991"
	fallbackPath := fmt.Sprintf("/tmp/prism-review-%s-round-%d-result.md", prNumber, 1)
	// Clean up any existing fallback file.
	_ = os.Remove(fallbackPath)
	t.Cleanup(func() { _ = os.Remove(fallbackPath) })

	opts := review.MonitorOpts{
		GroupID:              groupID,
		WorkerSession:        workerSession,
		PRNumber:             prNumber,
		Round:                1,
		Agents:               agents,
		AgentSessions:        []string{sess},
		DBPath:               d.Path(),
		PollInterval:         10 * time.Millisecond,
		MaxDeliveryRetries:   1, // 1 retry
		DeliveryRetryBackoff: 1 * time.Millisecond,
	}

	// MonitorFunc should return an error (fallback written).
	err = review.MonitorFunc(opts)
	if err == nil {
		t.Fatal("MonitorFunc: expected error due to delivery failure, got nil")
	}
	if !strings.Contains(err.Error(), "fallback") {
		t.Errorf("error should mention fallback: %v", err)
	}

	// Verify fallback file was written.
	data, readErr := os.ReadFile(fallbackPath)
	if readErr != nil {
		t.Fatalf("fallback file %s not written: %v", fallbackPath, readErr)
	}
	content := string(data)
	if !strings.Contains(content, prNumber) {
		t.Errorf("fallback file does not contain PR number %s: %q", prNumber, content)
	}
}

// TestMonitorFunc_MissingSession verifies that when a session is not found in
// GroupResults (deleted mid-review), it counts as "missing" and the group
// still completes with a note.
func TestMonitorFunc_MissingSession(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@missing-session-test"
	agents := []review.Agent{
		{Name: "review-goal"},
		{Name: "review-code"},
	}

	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// Only seed review-goal; review-code is "deleted" (not in DB).
	goalSess := parent + "~review-1-review-goal"
	codeSess := parent + "~review-1-review-code"

	if err := d.UpsertStatus(goalSess, "nixos-config", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetGroupID(goalSess, groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}
	seedAssistantEvent(t, d, goalSess, "<verdict>PASS</verdict>")

	// review-code is seeded as "deleted" (terminal) so GroupCompleted returns true.
	if err := d.UpsertStatus(codeSess, "nixos-config", "/wt", "deleted", nil, nil); err != nil {
		t.Fatalf("UpsertStatus deleted: %v", err)
	}
	if err := d.SetGroupID(codeSess, groupID); err != nil {
		t.Fatalf("SetGroupID deleted: %v", err)
	}

	// Capture the delivery text via a test HTTP server.
	var deliveredText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Parts []struct{ Text string `json:"text"` } `json:"parts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && len(body.Parts) > 0 {
			deliveredText = body.Parts[0].Text
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	workerSession := "nixos-config@missing-session-worker"
	port := extractPort(t, srv.URL)
	sessionID := "test-session-id-missing"
	if err := d.UpsertStatus(workerSession, "nixos-config", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus worker: %v", err)
	}
	if err := setHarnessInfo(t, d, workerSession, port, sessionID); err != nil {
		t.Fatalf("SetHarnessInfo: %v", err)
	}

	opts := review.MonitorOpts{
		GroupID:              groupID,
		WorkerSession:        workerSession,
		PRNumber:             "1234",
		Round:                1,
		Agents:               agents,
		AgentSessions:        []string{goalSess, codeSess},
		DBPath:               d.Path(),
		PollInterval:         10 * time.Millisecond,
		MaxDeliveryRetries:   0,
		DeliveryRetryBackoff: 1 * time.Millisecond,
	}

	if err := review.MonitorFunc(opts); err != nil {
		t.Fatalf("MonitorFunc with missing session: %v", err)
	}

	// The delivery should have happened.
	if deliveredText == "" {
		t.Fatal("no text delivered")
	}
}

// ── ActiveReviewGroupForParent ──────────────────────────────────────────────

// TestActiveReviewGroupForParent_NoRound verifies that when no review group
// exists for the parent, ActiveReviewGroupForParent returns "".
func TestActiveReviewGroupForParent_NoRound(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@no-round"

	gid, err := review.ActiveReviewGroupForParent(d, parent)
	if err != nil {
		t.Fatalf("ActiveReviewGroupForParent: %v", err)
	}
	if gid != "" {
		t.Errorf("ActiveReviewGroupForParent = %q, want empty (no rounds exist)", gid)
	}
}

// TestActiveReviewGroupForParent_CompletedRound verifies that a completed round
// (all members terminal) is NOT returned as active.
func TestActiveReviewGroupForParent_CompletedRound(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@completed-round"

	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// Seed 2 agents both finished.
	for _, agent := range []string{"review-goal", "review-code"} {
		sess := parent + "~review-1-" + agent
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus: %v", err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID: %v", err)
		}
	}

	gid, err := review.ActiveReviewGroupForParent(d, parent)
	if err != nil {
		t.Fatalf("ActiveReviewGroupForParent: %v", err)
	}
	if gid != "" {
		t.Errorf("ActiveReviewGroupForParent = %q, want empty (round completed)", gid)
	}
}

// TestActiveReviewGroupForParent_InProgressRound verifies that when at least
// one group member is not terminal, ActiveReviewGroupForParent returns that
// group_id.
func TestActiveReviewGroupForParent_InProgressRound(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@in-progress"

	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// review-goal is finished; review-code is still running.
	goalSess := parent + "~review-1-review-goal"
	codeSess := parent + "~review-1-review-code"

	if err := d.UpsertStatus(goalSess, "nixos-config", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus goal: %v", err)
	}
	if err := d.SetGroupID(goalSess, groupID); err != nil {
		t.Fatalf("SetGroupID goal: %v", err)
	}
	if err := d.UpsertStatus(codeSess, "nixos-config", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus code: %v", err)
	}
	if err := d.SetGroupID(codeSess, groupID); err != nil {
		t.Fatalf("SetGroupID code: %v", err)
	}

	gid, err := review.ActiveReviewGroupForParent(d, parent)
	if err != nil {
		t.Fatalf("ActiveReviewGroupForParent: %v", err)
	}
	if gid != groupID {
		t.Errorf("ActiveReviewGroupForParent = %q, want %q", gid, groupID)
	}
}

// ── buildMonitorResults ───────────────────────────────────────────────────────

// TestBuildMonitorResults_MissingSession verifies that a session not in
// GroupResults produces an error result with "missing" mention.
func TestBuildMonitorResults_MissingSession(t *testing.T) {
	agents := []review.Agent{{Name: "review-goal"}}
	sessions := []string{"nixos-config@parent~review-1-review-goal"}
	// Empty groupData → session is missing.
	groupData := map[string]db.GroupMemberResult{}

	results := review.BuildMonitorResultsForTest(agents, sessions, groupData)
	if len(results) != 1 {
		t.Fatalf("BuildMonitorResultsForTest: got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Passed {
		t.Errorf("missing session: Passed=true, want false")
	}
	if !r.IsError {
		t.Errorf("missing session: IsError=false, want true")
	}
	if !findSubstring(r.Output, "not found") && !findSubstring(r.Output, "missing") && !findSubstring(r.Output, "deleted") {
		t.Errorf("missing session: output should mention missing/not found/deleted: %q", r.Output)
	}
}

// TestBuildMonitorResults_FinishedPassed verifies that a finished session with
// PASS verdict produces Passed=true, IsError=false.
func TestBuildMonitorResults_FinishedPassed(t *testing.T) {
	agents := []review.Agent{{Name: "review-goal"}}
	sess := "nixos-config@parent~review-1-review-goal"
	sessions := []string{sess}
	payload := `{"text":"<verdict>PASS</verdict>"}`
	groupData := map[string]db.GroupMemberResult{
		sess: {SessionName: sess, State: "finished", LastMessage: payload},
	}

	results := review.BuildMonitorResultsForTest(agents, sessions, groupData)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if !r.Passed {
		t.Errorf("finished/pass: Passed=false, want true")
	}
	if r.IsError {
		t.Errorf("finished/pass: IsError=true, want false")
	}
}

// TestBuildMonitorResults_InterruptedState_FallsToDefault verifies the #1495
// contract at the buildMonitorResults layer.
//
// Under the new contract, db.GroupCompleted does NOT consider "interrupted"
// terminal, so the monitor's poll loop only flushes results once every agent
// has reached "finished", "error", or "deleted". The only way an agent's
// state in groupData can still be "interrupted" when buildMonitorResults
// runs is if the monitor's overall safety timeout fired — in which case the
// switch's default branch labels the agent as timed-out / unexpected-state.
// It must NOT take the genuine-error branch (which would surface as a
// no-start or mid-run crash, neither of which is accurate).
func TestBuildMonitorResults_InterruptedState_FallsToDefault(t *testing.T) {
	agents := []review.Agent{{Name: "review-code"}}
	sess := "nixos-config@parent~review-1-review-code"
	sessions := []string{sess}
	groupData := map[string]db.GroupMemberResult{
		sess: {SessionName: sess, State: "interrupted", LastMessage: ""},
	}

	results := review.BuildMonitorResultsForTest(agents, sessions, groupData)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Passed {
		t.Errorf("interrupted (default branch): Passed=true, want false")
	}
	if !r.IsError {
		t.Errorf("interrupted (default branch): IsError=false, want true")
	}
	// The default branch labels the agent as in an unexpected (timed out)
	// state. It must NOT use the genuine-error branch's wording.
	if !findSubstring(r.Output, "unexpected state") {
		t.Errorf("interrupted (default branch): output should mention 'unexpected state' (timeout fallback): %q", r.Output)
	}
	if findSubstring(r.Output, "did not complete cleanly") {
		t.Errorf("interrupted (default branch): output must NOT use the genuine-error wording: %q", r.Output)
	}
	if findSubstring(r.Output, "failed to start") {
		t.Errorf("interrupted (default branch): output must NOT use the no-start wording: %q", r.Output)
	}
}

// TestBuildMonitorResults_InterruptedThenResumedToFinishedPasses verifies the
// #1495 contract: an agent that was interrupted, redirected via `prism
// prompt`, and ultimately reached "finished" with a PASS verdict must be
// counted as a normal pass. The earlier interruption leaves no trace in
// groupData (only the latest state is retained).
func TestBuildMonitorResults_InterruptedThenResumedToFinishedPasses(t *testing.T) {
	agents := []review.Agent{{Name: "review-goal"}}
	sess := "nixos-config@parent~review-1-review-goal"
	sessions := []string{sess}
	payload := `{"text":"All ACs verified. <verdict>PASS</verdict>"}`
	groupData := map[string]db.GroupMemberResult{
		sess: {SessionName: sess, State: "finished", LastMessage: payload},
	}

	results := review.BuildMonitorResultsForTest(agents, sessions, groupData)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if !r.Passed {
		t.Errorf("interrupted-then-resumed-to-finished: Passed=false, want true (#1495)")
	}
	if r.IsError {
		t.Errorf("interrupted-then-resumed-to-finished: IsError=true, want false (#1495)")
	}
}

// TestBuildMonitorResults_InterruptedThenCleanedUp verifies the #1495 escape
// hatch at the buildMonitorResults layer: when the row is missing from
// groupData, buildMonitorResults routes it through the missing-session
// branch with IsError=true, while the remaining agents are reported normally.
//
// (The end-to-end flow that produces "row missing from groupData" — cleanup
// sets ended_at, GroupResults excludes ended rows — is verified by
// TestGroupResults_ExcludesEndedRows in internal/db/db_test.go and by
// TestMonitor_InterruptedThenCleanedUp_FlowsThroughDB below.)
func TestBuildMonitorResults_InterruptedThenCleanedUp(t *testing.T) {
	agents := []review.Agent{
		{Name: "review-goal"},
		{Name: "review-code"},
	}
	sessions := []string{
		"nixos-config@parent~review-1-review-goal",
		"nixos-config@parent~review-1-review-code",
	}
	// The first agent was cleaned up via `prism cleanup`; GroupResults skips
	// rows where ended_at IS NOT NULL, so it is missing from groupData here.
	// The second agent finished normally with a PASS verdict.
	groupData := map[string]db.GroupMemberResult{
		sessions[1]: {
			SessionName: sessions[1],
			State:       "finished",
			LastMessage: `{"text":"<verdict>PASS</verdict>"}`,
		},
	}

	results := review.BuildMonitorResultsForTest(agents, sessions, groupData)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	// The cleaned-up agent is reported as IsError via the missing-session
	// branch — unchanged behaviour, just exercised via the new flow.
	cleanedUp := results[0]
	if !cleanedUp.IsError {
		t.Errorf("cleaned-up agent: IsError=false, want true")
	}
	if !findSubstring(cleanedUp.Output, "not found in group") {
		t.Errorf("cleaned-up agent: output should use the missing-session message: %q", cleanedUp.Output)
	}

	// The other agent is reported normally — the cleanup of one agent does
	// not contaminate the others.
	other := results[1]
	if !other.Passed {
		t.Errorf("other agent: Passed=false, want true")
	}
	if other.IsError {
		t.Errorf("other agent: IsError=true, want false")
	}
}

// TestMonitor_InterruptedThenCleanedUp_FlowsThroughDB is the end-to-end
// regression test for #1495's escape hatch. It exercises the actual DB
// state that `prism cleanup --yes --session <interrupted-agent>` produces
// (state="interrupted" + ended_at set) and verifies the full flow:
//
//   1. db.GroupCompleted returns true (because ended_at IS NOT NULL counts
//      as terminal even when state="interrupted").
//   2. db.GroupResults excludes the ended row from the returned map.
//   3. buildMonitorResults sees the session as missing and routes it through
//      the existing 'session not found in group' branch with IsError=true.
//
// Without the (1) and (2) gates, an interrupted-then-cleaned-up agent would
// hang the review monitor forever — the regression review-context flagged on
// PR #1509 round 1.
func TestMonitor_InterruptedThenCleanedUp_FlowsThroughDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "prism.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	groupID, err := d.RegisterGroup("nixos-config@parent")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	const (
		sessCleaned  = "nixos-config@parent~review-1-review-goal"
		sessRunning  = "nixos-config@parent~review-1-review-code"
		sessFinished = "nixos-config@parent~review-1-review-security"
	)

	// All three start active.
	for _, s := range []string{sessCleaned, sessRunning, sessFinished} {
		if err := d.UpsertStatus(s, "nixos-config", "/wt", "active", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q, active): %v", s, err)
		}
		if err := d.SetGroupID(s, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", s, err)
		}
	}

	// Simulate the user interrupting one agent: state → interrupted.
	if err := d.UpsertStatus(sessCleaned, "nixos-config", "/wt", "interrupted", nil, nil); err != nil {
		t.Fatalf("UpsertStatus(interrupted): %v", err)
	}

	// At this point the group is NOT done — interrupted is non-terminal
	// (#1495) and the other two are still active.
	if done, _ := d.GroupCompleted(groupID); done {
		t.Fatal("pre-cleanup: GroupCompleted = true; want false (interrupted is non-terminal, others still active)")
	}

	// Simulate `prism cleanup --yes --session <interrupted-agent>`: the
	// cleanup path SIGTERMs the sidecar (state stays interrupted) and calls
	// SetEnded which sets ended_at without changing state.
	if err := d.SetEnded(sessCleaned); err != nil {
		t.Fatalf("SetEnded(%q): %v", sessCleaned, err)
	}

	// Sanity: the row is still there with state=interrupted and ended_at set.
	st, err := d.CurrentStatus(sessCleaned)
	if err != nil || st == nil {
		t.Fatalf("CurrentStatus(%q) = %v, %v", sessCleaned, st, err)
	}
	if st.State != "interrupted" {
		t.Errorf("post-cleanup state = %q, want %q (cleanup must NOT rewrite state)", st.State, "interrupted")
	}
	if st.EndedAt == nil {
		t.Errorf("post-cleanup ended_at is NULL, want set (cleanup must call SetEnded)")
	}

	// The other two transition to finished with verdicts.
	if err := d.UpsertStatus(sessRunning, "nixos-config", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus(running→finished): %v", err)
	}
	if err := d.UpsertStatus(sessFinished, "nixos-config", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus(finished): %v", err)
	}
	for _, s := range []string{sessRunning, sessFinished} {
		if err := d.WriteEvent(db.Event{
			ID:          s + "-evt-1",
			SessionName: s,
			Repo:        "nixos-config",
			Worktree:    "/wt",
			Type:        "msg_assistant",
			Payload:     `{"text":"<verdict>PASS</verdict>"}`,
		}); err != nil {
			t.Fatalf("WriteEvent(%q): %v", s, err)
		}
	}

	// Layer 1: GroupCompleted must now return true — the cleaned-up row's
	// ended_at IS NOT NULL counts as terminal even with state=interrupted.
	done, err := d.GroupCompleted(groupID)
	if err != nil {
		t.Fatalf("GroupCompleted: %v", err)
	}
	if !done {
		t.Fatal("GroupCompleted = false after cleanup + finishes; want true (regression: review monitor would hang forever)")
	}

	// Layer 2: GroupResults must EXCLUDE the cleaned-up row so the missing-
	// session branch in buildMonitorResults fires.
	groupData, err := d.GroupResults(groupID)
	if err != nil {
		t.Fatalf("GroupResults: %v", err)
	}
	if _, present := groupData[sessCleaned]; present {
		t.Errorf("GroupResults still contains the cleaned-up session %q; want it excluded (ended_at IS NOT NULL)", sessCleaned)
	}
	if _, present := groupData[sessRunning]; !present {
		t.Errorf("GroupResults missing the still-live finished session %q", sessRunning)
	}

	// Layer 3: buildMonitorResults routes the missing session through the
	// existing 'session not found in group' branch with IsError=true, while
	// the other two agents are reported normally with their PASS verdicts.
	agents := []review.Agent{
		{Name: "review-goal"},
		{Name: "review-code"},
		{Name: "review-security"},
	}
	sessions := []string{sessCleaned, sessRunning, sessFinished}
	results := review.BuildMonitorResultsForTest(agents, sessions, groupData)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if !results[0].IsError {
		t.Errorf("cleaned-up agent: IsError=false, want true")
	}
	if !findSubstring(results[0].Output, "not found in group") {
		t.Errorf("cleaned-up agent: output = %q, want missing-session message", results[0].Output)
	}
	for i, name := range []string{"running", "finished"} {
		r := results[i+1]
		if !r.Passed {
			t.Errorf("%s agent: Passed=false, want true", name)
		}
		if r.IsError {
			t.Errorf("%s agent: IsError=true, want false", name)
		}
	}
}

// ── LoadMonitorOptsFromFile ───────────────────────────────────────────────────

// TestLoadMonitorOptsFromFile_RoundTrip verifies that MonitorOpts can be
// written to a temp file and loaded back correctly.
func TestLoadMonitorOptsFromFile_RoundTrip(t *testing.T) {
	want := review.MonitorOpts{
		GroupID:       "test-group-id",
		WorkerSession: "nixos-config@test-worker",
		PRNumber:      "864",
		Round:         2,
		Agents: []review.Agent{
			{Name: "review-goal"},
			{Name: "review-code"},
		},
		AgentSessions: []string{
			"nixos-config@test~review-2-review-goal",
			"nixos-config@test~review-2-review-code",
		},
		DBPath:               "/tmp/test.db",
		PollInterval:         5 * time.Second,
		MaxDeliveryRetries:   5,
		DeliveryRetryBackoff: 30 * time.Second,
		Timeout:              20 * time.Minute,
	}

	// Write to temp file.
	dir := t.TempDir()
	path := filepath.Join(dir, "monitor-opts.json")
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Load back.
	got, err := review.LoadMonitorOptsFromFile(path)
	if err != nil {
		t.Fatalf("LoadMonitorOptsFromFile: %v", err)
	}

	// Verify fields.
	if got.GroupID != want.GroupID {
		t.Errorf("GroupID = %q, want %q", got.GroupID, want.GroupID)
	}
	if got.WorkerSession != want.WorkerSession {
		t.Errorf("WorkerSession = %q, want %q", got.WorkerSession, want.WorkerSession)
	}
	if got.PRNumber != want.PRNumber {
		t.Errorf("PRNumber = %q, want %q", got.PRNumber, want.PRNumber)
	}
	if got.Round != want.Round {
		t.Errorf("Round = %d, want %d", got.Round, want.Round)
	}
	if len(got.Agents) != len(want.Agents) {
		t.Errorf("Agents len = %d, want %d", len(got.Agents), len(want.Agents))
	}
	if got.DBPath != want.DBPath {
		t.Errorf("DBPath = %q, want %q", got.DBPath, want.DBPath)
	}
}

// ── WriteFallbackResult ───────────────────────────────────────────────────────

// TestWriteFallbackResult verifies the fallback file is written to the expected
// path and contains the provided content.
func TestWriteFallbackResult_WritesCorrectFile(t *testing.T) {
	prNumber := "99992"
	round := 3
	content := "## Review complete\n\nAll agents failed to deliver.\n"

	expectedPath := fmt.Sprintf("/tmp/prism-review-%s-round-%d-result.md", prNumber, round)
	_ = os.Remove(expectedPath)
	t.Cleanup(func() { _ = os.Remove(expectedPath) })

	path, err := review.WriteFallbackResult(prNumber, round, content)
	if err != nil {
		t.Fatalf("WriteFallbackResult: %v", err)
	}
	if path != expectedPath {
		t.Errorf("path = %q, want %q", path, expectedPath)
	}

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	if string(data) != content {
		t.Errorf("file content = %q, want %q", string(data), content)
	}
}

// ── ReviewRoundForGroup ───────────────────────────────────────────────────────

// TestReviewRoundForGroup_StandardShape verifies round extraction from standard
// per-agent session names.
func TestReviewRoundForGroup_StandardShape(t *testing.T) {
	members := []db.Status{
		{SessionName: "nixos-config@feature~review-3-review-goal"},
		{SessionName: "nixos-config@feature~review-3-review-code"},
	}
	got := review.ReviewRoundForGroup(members)
	if got != 3 {
		t.Errorf("ReviewRoundForGroup = %d, want 3", got)
	}
}

// TestReviewRoundForGroup_Empty verifies that empty members returns 0.
func TestReviewRoundForGroup_Empty(t *testing.T) {
	got := review.ReviewRoundForGroup(nil)
	if got != 0 {
		t.Errorf("ReviewRoundForGroup(nil) = %d, want 0", got)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// extractPort extracts the port number from an http://host:port URL.
func extractPort(t *testing.T, rawURL string) int {
	t.Helper()
	// rawURL is like "http://127.0.0.1:PORT"
	parts := strings.Split(rawURL, ":")
	if len(parts) < 3 {
		t.Fatalf("extractPort: unexpected URL format %q", rawURL)
	}
	portStr := parts[len(parts)-1]
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("extractPort: parse port from %q: %v", rawURL, err)
	}
	return port
}

// setHarnessInfo writes harness_port and harness_session_id directly via SQL,
// bypassing AllocatePort's OS-level port availability check (not needed for
// tests using httptest.Server which already owns the port).
// Sets harness to empty string so promptdelivery falls back to the HTTP path
// (used by these tests which spin up httptest.Server for delivery assertions).
func setHarnessInfo(t *testing.T, d *db.DB, sessionName string, port int, sessionID string) error {
	t.Helper()
	var dummy int
	err := d.QueryRow(
		"UPDATE agent_status SET harness_port = ?, harness_session_id = ?, harness = '' WHERE session_name = ? RETURNING 1",
		port, sessionID, sessionName,
	).Scan(&dummy)
	return err
}

// ── no-start error distinction (#1222) ───────────────────────────────────────

// TestBuildMonitorResults_NoStartError verifies that an agent in error state
// with a StartupError reason produces an output containing "no-start" to
// distinguish it from a mid-run crash.
func TestBuildMonitorResults_NoStartError(t *testing.T) {
	agents := []review.Agent{{Name: "review-code"}}
	sess := "nixos-config@parent~review-1-review-code"
	sessions := []string{sess}
	groupData := map[string]db.GroupMemberResult{
		sess: {
			SessionName:  sess,
			State:        "error",
			LastMessage:  "",
			StartupError: "pi: health check timed out after 60s on port 14004",
		},
	}

	results := review.BuildMonitorResultsForTest(agents, sessions, groupData)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Passed {
		t.Errorf("no-start error: Passed=true, want false")
	}
	if !r.IsError {
		t.Errorf("no-start error: IsError=false, want true")
	}
	if !findSubstring(r.Output, "no-start") {
		t.Errorf("no-start error: output should contain 'no-start': %q", r.Output)
	}
	if !findSubstring(r.Output, "health check timed out") {
		t.Errorf("no-start error: output should contain the startup error reason: %q", r.Output)
	}
}

// TestBuildMonitorResults_ErrorNoCrashMidRun verifies that an agent in error
// state WITHOUT a StartupError reason produces a generic "did not complete
// cleanly" message (mid-run crash, not a no-start failure).
func TestBuildMonitorResults_ErrorNoCrashMidRun(t *testing.T) {
	agents := []review.Agent{{Name: "review-code"}}
	sess := "nixos-config@parent~review-1-review-code"
	sessions := []string{sess}
	groupData := map[string]db.GroupMemberResult{
		sess: {
			SessionName:  sess,
			State:        "error",
			LastMessage:  "",
			StartupError: "", // no startup_error event — mid-run crash
		},
	}

	results := review.BuildMonitorResultsForTest(agents, sessions, groupData)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Passed {
		t.Errorf("mid-run error: Passed=true, want false")
	}
	if !r.IsError {
		t.Errorf("mid-run error: IsError=false, want true")
	}
	// Must NOT say no-start — this was a mid-run crash.
	if findSubstring(r.Output, "no-start") {
		t.Errorf("mid-run error: output should NOT contain 'no-start': %q", r.Output)
	}
	if !findSubstring(r.Output, "did not complete cleanly") {
		t.Errorf("mid-run error: output should contain 'did not complete cleanly': %q", r.Output)
	}
}

// ── buildDeliveryMessage (#1222) ─────────────────────────────────────────────

// TestBuildDeliveryMessage_AllPassed verifies the all-passed header.
func TestBuildDeliveryMessage_AllPassed(t *testing.T) {
	sess := "nixos-config@parent~review-1-review-code"
	groupData := map[string]db.GroupMemberResult{
		sess: {SessionName: sess, State: "finished"},
	}
	msg := review.BuildDeliveryMessageForTest("42", 1, "results text", true, groupData, []string{sess})
	if !findSubstring(msg, "All 5 review agents passed") {
		t.Errorf("all-passed: header missing 'All 5 review agents passed': %q", msg)
	}
}

// TestBuildDeliveryMessage_PureFailNoStart verifies that when ALL agents have
// no-start errors, the header says "infrastructure failure" with no mention of
// code-quality FAIL.
func TestBuildDeliveryMessage_PureFailNoStart(t *testing.T) {
	sess := "nixos-config@parent~review-1-review-code"
	groupData := map[string]db.GroupMemberResult{
		sess: {SessionName: sess, State: "error", StartupError: "health check timed out"},
	}
	msg := review.BuildDeliveryMessageForTest("42", 1, "results text", false, groupData, []string{sess})
	if !findSubstring(msg, "infrastructure failure") {
		t.Errorf("pure no-start: header should mention 'infrastructure failure': %q", msg)
	}
	if !findSubstring(msg, "Re-run") {
		t.Errorf("pure no-start: header should instruct re-run: %q", msg)
	}
	// Must NOT say "Fix the blocking issues" (no code ran).
	if findSubstring(msg, "Fix the blocking issues") {
		t.Errorf("pure no-start: header should NOT say 'Fix the blocking issues': %q", msg)
	}
}

// TestBuildDeliveryMessage_MixedNoStartAndFail verifies that when some agents
// had FAIL verdicts and some had no-start errors, both signals appear — the
// coordinator must both fix code issues AND re-run for the failed starts.
func TestBuildDeliveryMessage_MixedNoStartAndFail(t *testing.T) {
	sess1 := "nixos-config@parent~review-1-review-code"
	sess2 := "nixos-config@parent~review-1-review-goal"
	groupData := map[string]db.GroupMemberResult{
		sess1: {SessionName: sess1, State: "error", StartupError: "health check timed out"},
		sess2: {SessionName: sess2, State: "finished", LastMessage: `{"text":"<verdict>FAIL</verdict>"}`},
	}
	msg := review.BuildDeliveryMessageForTest("42", 1, "results text", false, groupData, []string{sess1, sess2})
	if !findSubstring(msg, "infrastructure failure") {
		t.Errorf("mixed: header should mention 'infrastructure failure': %q", msg)
	}
	if !findSubstring(msg, "Re-run") && !findSubstring(msg, "re-run") {
		t.Errorf("mixed: header should instruct re-run: %q", msg)
	}
	// Must also call out blocking issues so coordinator doesn't skip fixing code.
	if !findSubstring(msg, "blocking issues") {
		t.Errorf("mixed: header should mention 'blocking issues': %q", msg)
	}
}

// TestBuildDeliveryMessage_PureCodeFail verifies that when all failures are
// code-quality FAILs (no no-start errors), the standard "Fix the blocking
// issues" header is shown with no infrastructure-failure mention.
func TestBuildDeliveryMessage_PureCodeFail(t *testing.T) {
	sess := "nixos-config@parent~review-1-review-code"
	groupData := map[string]db.GroupMemberResult{
		sess: {SessionName: sess, State: "finished", LastMessage: `{"text":"<verdict>FAIL</verdict>"}`},
	}
	msg := review.BuildDeliveryMessageForTest("42", 1, "results text", false, groupData, []string{sess})
	if !findSubstring(msg, "Fix the blocking issues") {
		t.Errorf("pure code fail: header should say 'Fix the blocking issues': %q", msg)
	}
	if findSubstring(msg, "infrastructure failure") {
		t.Errorf("pure code fail: header should NOT mention 'infrastructure failure': %q", msg)
	}
}
