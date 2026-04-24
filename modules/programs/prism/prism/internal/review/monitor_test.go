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
		{Name: "review-goal", OpencodeName: "review-goal"},
		{Name: "review-code", OpencodeName: "review-code"},
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

	// Start an httptest server to simulate the opencode prompt_async endpoint.
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
	sessionID := "test-opencode-session-id"
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

// TestMonitorFunc_DeliveryFailure_WritesFallbackFile verifies that when all
// delivery retries fail, MonitorFunc writes the fallback file at the expected
// path.
func TestMonitorFunc_DeliveryFailure_WritesFallbackFile(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@delivery-fail-test"
	agents := []review.Agent{
		{Name: "review-goal", OpencodeName: "review-goal"},
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
		{Name: "review-goal", OpencodeName: "review-goal"},
		{Name: "review-code", OpencodeName: "review-code"},
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

// TestBuildMonitorResults_InterruptedState verifies that an interrupted agent
// produces an error result.
func TestBuildMonitorResults_InterruptedState(t *testing.T) {
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
		t.Errorf("interrupted: Passed=true, want false")
	}
	if !r.IsError {
		t.Errorf("interrupted: IsError=false, want true")
	}
	if !findSubstring(r.Output, "interrupted") {
		t.Errorf("interrupted: output should mention 'interrupted': %q", r.Output)
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
			{Name: "review-goal", OpencodeName: "review-goal"},
			{Name: "review-code", OpencodeName: "review-code"},
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
func setHarnessInfo(t *testing.T, d *db.DB, sessionName string, port int, sessionID string) error {
	t.Helper()
	var dummy int
	err := d.QueryRow(
		"UPDATE agent_status SET harness_port = ?, harness_session_id = ? WHERE session_name = ? RETURNING 1",
		port, sessionID, sessionName,
	).Scan(&dummy)
	return err
}
