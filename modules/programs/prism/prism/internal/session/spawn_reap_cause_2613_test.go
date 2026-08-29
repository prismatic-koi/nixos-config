package session

// spawn_reap_cause_2613_test.go — cleanupHalfAliveSession must record why it
// closed the row.
//
// This helper is on the review-agent lifecycle: internal/review's spawn loop
// calls SpawnSession, so a review agent that fails its layout step or
// SpawnSession's own inline readiness gate is closed here, in state "error".
// A closed row in that state with no recorded cause reads as
// "force-terminated, or its readiness gate failed", which the round report
// cannot explain.

import (
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

func openReapTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestCleanupHalfAliveSession_RecordsTheCause(t *testing.T) {
	cases := []struct {
		name   string
		cause  db.SessionReapCause
		detail []string
		want   string
	}{
		{
			name:   "readiness gate",
			cause:  db.ReapCauseReadinessGate,
			detail: []string{"not ready within 30s"},
			want:   "not ready within 30s",
		},
		{
			name:  "layout failure",
			cause: db.ReapCauseSpawnFailure,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := openReapTestDB(t)
			const session = "prism-test@2613~review-1-review-qa"
			if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/x", "idle", nil, nil); err != nil {
				t.Fatalf("UpsertStatus: %v", err)
			}

			cleanupHalfAliveSession(d, session, "", tc.cause, tc.detail...)

			causes, err := d.SessionEndCauses([]string{session})
			if err != nil {
				t.Fatalf("SessionEndCauses: %v", err)
			}
			got, ok := causes[session]
			if !ok {
				t.Fatalf("no close cause recorded for %s — the round report cannot name why the row closed", session)
			}
			if got.Cause != tc.cause {
				t.Errorf("Cause = %q, want %q", got.Cause, tc.cause)
			}
			if tc.want != "" && got.Detail != tc.want {
				t.Errorf("Detail = %q, want %q", got.Detail, tc.want)
			}

			// The row must still be closed in state "error" — recording the
			// cause must not change the cleanup's own behaviour.
			st, err := d.CurrentStatus(session)
			if err != nil {
				t.Fatalf("CurrentStatus: %v", err)
			}
			if st == nil || st.State != "error" || st.EndedAt == nil {
				t.Errorf("row after cleanup: state=%v ended_at set=%v, want state=error with ended_at set", st, st != nil && st.EndedAt != nil)
			}
		})
	}
}

// TestCleanupHalfAliveSession_DoesNotOverwriteAnEarlierCause pins the
// ended_at guard. db.SetEnded will not re-close an already-closed row, so this
// helper must not record a cause claiming that it did — SessionEndCauses
// returns the latest record, so an unguarded second write replaces the row's
// true cause with a false one.
func TestCleanupHalfAliveSession_DoesNotOverwriteAnEarlierCause(t *testing.T) {
	d := openReapTestDB(t)
	const session = "prism-test@2613-halfalive~review-1-review-qa"
	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/x", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	cleanupHalfAliveSession(d, session, "", db.ReapCauseReadinessGate, "not ready within 30s")
	// A second pass over the same, now-closed row.
	cleanupHalfAliveSession(d, session, "", db.ReapCauseSpawnFailure, "some later failure")

	causes, err := d.SessionEndCauses([]string{session})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	if causes[session].Cause != db.ReapCauseReadinessGate {
		t.Errorf("Cause = %q, want %q — a pass that did not close the row must not overwrite the true cause",
			causes[session].Cause, db.ReapCauseReadinessGate)
	}
}
