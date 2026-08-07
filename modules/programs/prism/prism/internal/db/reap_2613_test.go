package db_test

// reap_2613_test.go — the recorded close cause for an agent_status row
// (issue #2613).
//
// Before #2613 a row that a lifecycle path had closed carried exactly two
// readable facts: the state string and the closing time. Several paths leave
// state="error" behind, so the review report had to name more than one of them
// for the same row and could confirm none. These tests pin the record and the
// read-back that make one cause nameable.

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
)

func seedReapSession(t *testing.T, d *db.DB, session string) {
	t.Helper()
	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/"+session, "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus(%s): %v", session, err)
	}
}

func writeReasonEvent(t *testing.T, d *db.DB, session, evtType, reason string) {
	t.Helper()
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: session,
		Repo:        "prism-test",
		Worktree:    "/code/prism-test/" + session,
		Type:        evtType,
		Payload:     `{"reason":"` + reason + `"}`,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("WriteEvent(%s, %s): %v", session, evtType, err)
	}
}

// TestRecordSessionReap_RoundTrip verifies that every cause a lifecycle path
// can record reads back as exactly that cause.
func TestRecordSessionReap_RoundTrip(t *testing.T) {
	causes := []db.SessionReapCause{
		db.ReapCauseReadinessGate,
		db.ReapCauseSpawnFailure,
		db.ReapCauseMonitorTimeout,
		db.ReapCauseParentCleanup,
		db.ReapCauseCleanupCommand,
		db.ReapCauseAutoRelease,
	}

	for _, cause := range causes {
		t.Run(string(cause), func(t *testing.T) {
			d := openTestDB(t)
			session := "prism-test@2613-" + string(cause)
			seedReapSession(t, d, session)

			if err := d.RecordSessionReap(session, cause, "detail text"); err != nil {
				t.Fatalf("RecordSessionReap: %v", err)
			}

			got, err := d.SessionEndCauses([]string{session})
			if err != nil {
				t.Fatalf("SessionEndCauses: %v", err)
			}
			rec, ok := got[session]
			if !ok {
				t.Fatalf("SessionEndCauses: no record for %s", session)
			}
			if rec.Cause != cause {
				t.Errorf("Cause: got %q, want %q", rec.Cause, cause)
			}
			if rec.Detail != "detail text" {
				t.Errorf("Detail: got %q, want %q", rec.Detail, "detail text")
			}
			if !rec.Recorded() {
				t.Error("Recorded(): got false, want true")
			}
			if rec.Cause.Description() == "" {
				t.Errorf("Description() for %q is empty — every cause must render one specific sentence", cause)
			}
		})
	}
}

// TestSessionReapCauseDescriptions_NameOneCauseEach is the guard for the
// defect that opened #2613: the report named "the session was force-terminated,
// or its readiness gate failed" for a single row. A Description that contains a
// disjunction reintroduces exactly that.
func TestSessionReapCauseDescriptions_NameOneCauseEach(t *testing.T) {
	causes := []db.SessionReapCause{
		db.ReapCauseReadinessGate,
		db.ReapCauseSpawnFailure,
		db.ReapCauseMonitorTimeout,
		db.ReapCauseParentCleanup,
		db.ReapCauseCleanupCommand,
		db.ReapCauseAutoRelease,
	}
	for _, c := range causes {
		desc := c.Description()
		for _, banned := range []string{", or ", " or its ", "either "} {
			if strings.Contains(desc, banned) {
				t.Errorf("Description() for %q contains %q — a cause must name one path: %q", c, banned, desc)
			}
		}
	}
}

// TestSessionEndCauses_LatestReapWins pins the READER contract: when more than
// one session_reaped event exists, SessionEndCauses returns the most recent.
//
// This is a read-layer contract only. It is deliberately NOT a statement that
// callers may record twice — they may not. Every recording call site guards on
// ended_at, so a path that finds the row already closed records nothing. That
// guard exists precisely BECAUSE this reader takes the latest: an unguarded
// second record would overwrite a row's true cause with a false one. The
// caller-side guard is pinned by TestCleanupAgentSession_DoesNotOverwrite... in
// internal/review and TestCleanupHalfAliveSession_... in internal/session.
func TestSessionEndCauses_LatestReapWins(t *testing.T) {
	d := openTestDB(t)
	const session = "prism-test@2613-latest"
	seedReapSession(t, d, session)

	if err := d.RecordSessionReap(session, db.ReapCauseReadinessGate, "first"); err != nil {
		t.Fatalf("RecordSessionReap (first): %v", err)
	}
	// Guarantee a strictly later created_at; the ORDER BY breaks ties on
	// rowid, but an explicit gap keeps the test honest about intent.
	time.Sleep(2 * time.Millisecond)
	if err := d.RecordSessionReap(session, db.ReapCauseCleanupCommand, "second"); err != nil {
		t.Fatalf("RecordSessionReap (second): %v", err)
	}

	got, err := d.SessionEndCauses([]string{session})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	if got[session].Cause != db.ReapCauseCleanupCommand {
		t.Errorf("Cause: got %q, want %q", got[session].Cause, db.ReapCauseCleanupCommand)
	}
	if got[session].Detail != "second" {
		t.Errorf("Detail: got %q, want %q", got[session].Detail, "second")
	}
}

// TestSessionEndCauses_ReadsSidecarFailureEvents verifies that the reader also
// surfaces the sidecar's own failure events. This is the branch that fixes the
// #2610 shape: the inactivity watchdog writes stall_error and leaves ended_at
// NULL; the tmux session-closed hook then stamps ended_at without rewriting
// state. The row is dropped from GroupResults and the recorded stall must not
// be lost with it.
func TestSessionEndCauses_ReadsSidecarFailureEvents(t *testing.T) {
	d := openTestDB(t)
	const session = "prism-test@2613-stalled"
	seedReapSession(t, d, session)

	writeReasonEvent(t, d, session, "stall_error", "stalled mid-run after 4m0s (12 frame(s) received)")
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: session,
		Repo:        "prism-test",
		Worktree:    "/code/prism-test/" + session,
		Type:        "tmux_session_end",
		Payload:     `{}`,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("WriteEvent(tmux_session_end): %v", err)
	}

	got, err := d.SessionEndCauses([]string{session})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	rec := got[session]
	if rec.StallError == "" {
		t.Error("StallError: got empty, want the recorded stall reason")
	}
	if !rec.TmuxSessionEnded {
		t.Error("TmuxSessionEnded: got false, want true")
	}
	if rec.Cause != "" {
		t.Errorf("Cause: got %q, want empty — no lifecycle path recorded one", rec.Cause)
	}
}

// TestSessionEndCauses_StartupErrorRead verifies the no-start branch.
func TestSessionEndCauses_StartupErrorRead(t *testing.T) {
	d := openTestDB(t)
	const session = "prism-test@2613-nostart"
	seedReapSession(t, d, session)
	writeReasonEvent(t, d, session, "startup_error", "container never bound its port")

	got, err := d.SessionEndCauses([]string{session})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	if got[session].StartupError != "container never bound its port" {
		t.Errorf("StartupError: got %q, want the recorded reason", got[session].StartupError)
	}
}

// TestSessionEndCauses_AbsentAndEmptyInputs verifies the degraded shapes: a
// session with nothing recorded is absent from the map, and an empty input is
// not an error.
func TestSessionEndCauses_AbsentAndEmptyInputs(t *testing.T) {
	d := openTestDB(t)
	const session = "prism-test@2613-silent"
	seedReapSession(t, d, session)

	got, err := d.SessionEndCauses([]string{session, "", session})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	if _, ok := got[session]; ok {
		t.Error("SessionEndCauses returned a record for a session with nothing recorded")
	}

	empty, err := d.SessionEndCauses(nil)
	if err != nil {
		t.Fatalf("SessionEndCauses(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("SessionEndCauses(nil): got %d entries, want 0", len(empty))
	}
}

// TestRecordSessionReap_RejectsBlankInputs verifies the argument guards. The
// cause is the whole point of the record, so a blank one is an error rather
// than a silently useless row.
func TestRecordSessionReap_RejectsBlankInputs(t *testing.T) {
	d := openTestDB(t)

	if err := d.RecordSessionReap("", db.ReapCauseCleanupCommand, ""); err == nil {
		t.Error("RecordSessionReap with a blank session name: got nil error, want an error")
	}
	if err := d.RecordSessionReap("prism-test@2613-blank", "", ""); err == nil {
		t.Error("RecordSessionReap with a blank cause: got nil error, want an error")
	}
}

// TestRecordSessionReap_UnknownSessionIsNotAnError mirrors WriteEvent's
// contract: recording a reap for a session with no agent_status row still
// writes the event. A cleanup path must not fail because the row is already
// gone.
func TestRecordSessionReap_UnknownSessionIsNotAnError(t *testing.T) {
	d := openTestDB(t)
	const session = "prism-test@2613-unknown"

	if err := d.RecordSessionReap(session, db.ReapCauseCleanupCommand, ""); err != nil {
		t.Fatalf("RecordSessionReap for an unknown session: %v", err)
	}
	got, err := d.SessionEndCauses([]string{session})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	if got[session].Cause != db.ReapCauseCleanupCommand {
		t.Errorf("Cause: got %q, want %q", got[session].Cause, db.ReapCauseCleanupCommand)
	}
}

// TestRecordReapBestEffort_TolerantOfBlankInputs pins the cleanup-path helper:
// it must never panic or fail, because it runs where a lost diagnostic must
// not mask the cleanup itself.
func TestRecordReapBestEffort_TolerantOfBlankInputs(t *testing.T) {
	d := openTestDB(t)
	const session = "prism-test@2613-besteffort"
	seedReapSession(t, d, session)

	d.RecordReapBestEffort("", db.ReapCauseCleanupCommand)
	d.RecordReapBestEffort(session, "")
	d.RecordReapBestEffort(session, db.ReapCauseParentCleanup)
	d.RecordReapBestEffort(session, db.ReapCauseMonitorTimeout, "", "  ", "why")

	got, err := d.SessionEndCauses([]string{session})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	if got[session].Cause != db.ReapCauseMonitorTimeout {
		t.Errorf("Cause: got %q, want %q", got[session].Cause, db.ReapCauseMonitorTimeout)
	}
	if got[session].Detail != "why" {
		t.Errorf("Detail: got %q, want %q — the first non-blank detail wins", got[session].Detail, "why")
	}
}
