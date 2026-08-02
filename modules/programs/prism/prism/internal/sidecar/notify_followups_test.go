// Tests in this file exercise the worker follow-ups convention (issue
// #2528): a worker's terminal notification to the coordinator is normally
// the fixed "has finished"/"has errored" string. When the worker's last
// completed turn contains a well-formed <follow_ups>...</follow_ups>
// section, that content is folded into the notification body instead.
//
// # Isolation contract
//
// Every test in this file constructs a sidecar.Sidecar via
// sidecartest.NewIsolated and uses session names with the "prism-test@"
// prefix so a running `go test ./internal/sidecar/...` can never deliver to
// or write through a live coordinator on the host. See sidecartest for the
// full isolation guarantees.
package sidecar

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// --- extractFollowUps / buildWorkerNotifyText unit tests ---

func TestExtractFollowUps_Absent(t *testing.T) {
	content, truncated, found := extractFollowUps("just some ordinary handoff text, no tags here")
	if found {
		t.Errorf("found = true, want false for text with no <follow_ups> tag")
	}
	if content != "" || truncated {
		t.Errorf("content=%q truncated=%v, want empty/false when not found", content, truncated)
	}
}

func TestExtractFollowUps_EmptyText(t *testing.T) {
	content, truncated, found := extractFollowUps("")
	if found || content != "" || truncated {
		t.Errorf("extractFollowUps(\"\") = (%q, %v, %v), want (\"\", false, false)", content, truncated, found)
	}
}

func TestExtractFollowUps_EmptySection(t *testing.T) {
	// AC: an empty follow-ups section is treated the same as no section.
	content, truncated, found := extractFollowUps("handoff text\n<follow_ups></follow_ups>\nmore text")
	if found {
		t.Errorf("found = true for an empty <follow_ups></follow_ups> section, want false")
	}
	if content != "" || truncated {
		t.Errorf("content=%q truncated=%v, want empty/false for empty section", content, truncated)
	}
}

func TestExtractFollowUps_WhitespaceOnlySection(t *testing.T) {
	// AC: a whitespace-only section is treated the same as no section.
	content, truncated, found := extractFollowUps("handoff text\n<follow_ups>\n   \n\t\n</follow_ups>\nmore text")
	if found {
		t.Errorf("found = true for a whitespace-only <follow_ups> section, want false")
	}
	if content != "" || truncated {
		t.Errorf("content=%q truncated=%v, want empty/false for whitespace-only section", content, truncated)
	}
}

func TestExtractFollowUps_NonEmptySection(t *testing.T) {
	text := "Some preamble.\n<follow_ups>\nFound a pre-existing bug in foo.go:42.\n</follow_ups>\nSome trailer."
	content, truncated, found := extractFollowUps(text)
	if !found {
		t.Fatal("found = false, want true for a well-formed non-empty section")
	}
	if truncated {
		t.Error("truncated = true, want false for a section under the byte cap")
	}
	want := "Found a pre-existing bug in foo.go:42."
	if content != want {
		t.Errorf("content = %q, want %q", content, want)
	}
}

func TestExtractFollowUps_UnterminatedMarker(t *testing.T) {
	// AC (edge case): a malformed/unterminated follow-ups marker does not
	// break delivery — it is treated as "no section".
	content, truncated, found := extractFollowUps("handoff text\n<follow_ups>\nnever closed")
	if found {
		t.Errorf("found = true for an unterminated <follow_ups> tag, want false")
	}
	if content != "" || truncated {
		t.Errorf("content=%q truncated=%v, want empty/false for unterminated tag", content, truncated)
	}
}

func TestExtractFollowUps_TruncatesOverCap(t *testing.T) {
	huge := strings.Repeat("x", followUpsByteCap+500)
	text := "<follow_ups>" + huge + "</follow_ups>"
	content, truncated, found := extractFollowUps(text)
	if !found {
		t.Fatal("found = false, want true")
	}
	if !truncated {
		t.Error("truncated = false, want true for a section over followUpsByteCap")
	}
	if len(content) != followUpsByteCap {
		t.Errorf("len(content) = %d, want %d (followUpsByteCap)", len(content), followUpsByteCap)
	}
}

func TestBuildWorkerNotifyText_NoSection_ReturnsBaseUnchanged(t *testing.T) {
	base := "Agent test@session has finished its current task"
	got := buildWorkerNotifyText(base, "test@session", "no tags in this handoff turn at all")
	if got != base {
		t.Errorf("buildWorkerNotifyText with no section = %q, want unchanged base %q", got, base)
	}
}

func TestBuildWorkerNotifyText_EmptyFinalText_ReturnsBaseUnchanged(t *testing.T) {
	base := "Agent test@session has finished its current task"
	got := buildWorkerNotifyText(base, "test@session", "")
	if got != base {
		t.Errorf("buildWorkerNotifyText with empty finalText = %q, want unchanged base %q", got, base)
	}
}

func TestBuildWorkerNotifyText_WithSection_IncludesContentSessionAndCheckin(t *testing.T) {
	base := "Agent test@session has finished its current task"
	finalText := "<follow_ups>\nSaw a flaky test in pkg/foo, unrelated to this change.\n</follow_ups>"
	got := buildWorkerNotifyText(base, "test@session", finalText)

	if !strings.Contains(got, base) {
		t.Errorf("notification %q does not contain base text %q", got, base)
	}
	if !strings.Contains(got, "Saw a flaky test in pkg/foo, unrelated to this change.") {
		t.Errorf("notification %q does not contain follow-ups content", got)
	}
	if !strings.Contains(got, "test@session") {
		t.Errorf("notification %q does not name the source session", got)
	}
	if !strings.Contains(got, "prism checkin test@session") {
		t.Errorf("notification %q does not contain a prism checkin pointer", got)
	}
	if strings.Contains(got, "truncated") {
		t.Errorf("notification %q mentions truncation for a section under the cap", got)
	}
}

func TestBuildWorkerNotifyText_TruncatedSection_NotesTruncation(t *testing.T) {
	base := "Agent test@session has finished its current task"
	huge := strings.Repeat("y", followUpsByteCap+100)
	finalText := "<follow_ups>" + huge + "</follow_ups>"
	got := buildWorkerNotifyText(base, "test@session", finalText)

	if !strings.Contains(got, "truncat") {
		t.Errorf("notification for an over-cap section does not mention truncation: %q", truncateBytes([]byte(got), 200))
	}
	if !strings.Contains(got, "prism checkin test@session") {
		t.Error("notification for a truncated section must still point at prism checkin for the full turn")
	}
}

// --- End-to-end notifyCoordinator delivery tests ---

func TestNotifyCoordinator_NoFollowUps_DeliversGenericText(t *testing.T) {
	workerSession := "prism-test@worker-nofollowups"
	coordSession := "prism-test@coordinator-nofollowups"
	repo := "prism-test"

	bus := sidecartest.NewIsolated(t, coordSession)
	seedTestCoordinator(t, bus.DB, coordSession)
	seedTestWorker(t, bus.DB, workerSession, repo)

	cfg := Config{
		SessionName: workerSession,
		Repo:        repo,
		Worktree:    "/tmp/test-worker-nofollowups",
		DB:          bus.DB,
		Clock:       newTestClock(),
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
		AgentRole:   "worker",
	}
	s := New(cfg)

	var capturedText string
	s.notifyCoordinatorDeliverFn = func(sessionName string, status *db.Status, text string, buildHTTPBody func(string, *db.Status) map[string]any, source string, deliverAs string) error {
		capturedText = text
		return nil
	}

	s.notifyCoordinator("just an ordinary handoff turn, no follow-ups section")

	want := "Agent " + workerSession + " has finished its current task"
	if capturedText != want {
		t.Errorf("notifyCoordinator delivered text=%q, want unchanged generic text %q", capturedText, want)
	}
}

func TestNotifyCoordinator_WithFollowUps_DeliversBody(t *testing.T) {
	workerSession := "prism-test@worker-withfollowups"
	coordSession := "prism-test@coordinator-withfollowups"
	repo := "prism-test"

	bus := sidecartest.NewIsolated(t, coordSession)
	seedTestCoordinator(t, bus.DB, coordSession)
	seedTestWorker(t, bus.DB, workerSession, repo)

	cfg := Config{
		SessionName: workerSession,
		Repo:        repo,
		Worktree:    "/tmp/test-worker-withfollowups",
		DB:          bus.DB,
		Clock:       newTestClock(),
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
		AgentRole:   "worker",
	}
	s := New(cfg)

	var capturedText string
	s.notifyCoordinatorDeliverFn = func(sessionName string, status *db.Status, text string, buildHTTPBody func(string, *db.Status) map[string]any, source string, deliverAs string) error {
		capturedText = text
		return nil
	}

	finalText := "Handoff summary.\n<follow_ups>\nVerified a pre-existing defect in bar.go:99; left it out of scope.\n</follow_ups>\n"
	s.notifyCoordinator(finalText)

	if !strings.Contains(capturedText, "Agent "+workerSession+" has finished its current task") {
		t.Errorf("delivered text %q does not contain the generic base wording", capturedText)
	}
	if !strings.Contains(capturedText, "Verified a pre-existing defect in bar.go:99; left it out of scope.") {
		t.Errorf("delivered text %q does not contain the follow-ups content", capturedText)
	}
	if !strings.Contains(capturedText, workerSession) {
		t.Errorf("delivered text %q does not name the source session %q", capturedText, workerSession)
	}
	if !strings.Contains(capturedText, "prism checkin "+workerSession) {
		t.Errorf("delivered text %q does not include a prism checkin pointer", capturedText)
	}
}

func TestNotifyCoordinatorError_WithFollowUps_DeliversBody(t *testing.T) {
	workerSession := "prism-test@worker-errfollowups"
	coordSession := "prism-test@coordinator-errfollowups"
	repo := "prism-test"

	bus := sidecartest.NewIsolated(t, coordSession)
	seedTestCoordinator(t, bus.DB, coordSession)
	seedTestWorker(t, bus.DB, workerSession, repo)

	cfg := Config{
		SessionName: workerSession,
		Repo:        repo,
		Worktree:    "/tmp/test-worker-errfollowups",
		DB:          bus.DB,
		Clock:       newTestClock(),
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
		AgentRole:   "worker",
	}
	s := New(cfg)

	var capturedText string
	s.notifyCoordinatorDeliverFn = func(sessionName string, status *db.Status, text string, buildHTTPBody func(string, *db.Status) map[string]any, source string, deliverAs string) error {
		capturedText = text
		return nil
	}

	finalText := "<follow_ups>Left the container image name unchanged; see PR description.</follow_ups>"
	s.notifyCoordinatorError(finalText)

	if !strings.Contains(capturedText, "Agent "+workerSession+" has errored its current task") {
		t.Errorf("delivered text %q does not contain the generic error wording", capturedText)
	}
	if !strings.Contains(capturedText, "Left the container image name unchanged; see PR description.") {
		t.Errorf("delivered text %q does not contain the follow-ups content", capturedText)
	}
}

func TestNotifyCoordinatorError_NoFollowUps_DeliversGenericText(t *testing.T) {
	workerSession := "prism-test@worker-errnofollowups"
	coordSession := "prism-test@coordinator-errnofollowups"
	repo := "prism-test"

	bus := sidecartest.NewIsolated(t, coordSession)
	seedTestCoordinator(t, bus.DB, coordSession)
	seedTestWorker(t, bus.DB, workerSession, repo)

	cfg := Config{
		SessionName: workerSession,
		Repo:        repo,
		Worktree:    "/tmp/test-worker-errnofollowups",
		DB:          bus.DB,
		Clock:       newTestClock(),
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
		AgentRole:   "worker",
	}
	s := New(cfg)

	var capturedText string
	s.notifyCoordinatorDeliverFn = func(sessionName string, status *db.Status, text string, buildHTTPBody func(string, *db.Status) map[string]any, source string, deliverAs string) error {
		capturedText = text
		return nil
	}

	s.notifyCoordinatorError("")

	want := "Agent " + workerSession + " has errored its current task"
	if capturedText != want {
		t.Errorf("notifyCoordinatorError delivered text=%q, want unchanged generic text %q", capturedText, want)
	}
}
