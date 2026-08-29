package review_test

// reap_state_precedence_2613_test.go — a recorded close cause must not
// overwrite an explanation the row's state already carries.
//
// `prism cleanup` records a `cleanup_command` cause on every row it closes,
// including rows that had already reached a terminal state. For a row in state
// "finished" the operator's cleanup says who closed the row; it does not
// explain why the verdict is missing. The state's own wording for those
// states is the accurate one and must survive.

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

func closedRowInState(sess, state string) db.Status {
	row := closedRow(sess)
	row.State = state
	return row
}

func TestClassifyRound_SelfExplainingStateOutranksRecordedCause(t *testing.T) {
	sessions := fiveAgentSessions(2)
	qa := sessions[3]
	agents := review.AgentsFromSessionsForTest(sessions)
	groupData := fourPassingSiblings(sessions, qa)

	cases := []struct {
		state    string
		wantText string
	}{
		{state: "finished", wantText: "closed before results were aggregated"},
		{state: "deleted", wantText: "session.deleted"},
	}

	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			st := review.ClassifyRoundWithCauses(agents, sessions, groupData,
				map[string]db.Status{qa: closedRowInState(qa, tc.state)},
				map[string]db.SessionEndCause{qa: {Cause: db.ReapCauseCleanupCommand}},
			)
			if len(st.Missing) != 1 {
				t.Fatalf("Missing = %d entries, want 1", len(st.Missing))
			}
			m := st.Missing[0]
			if m.Class != review.NoVerdictSessionEnded {
				t.Errorf("Class = %q, want %q", m.Class, review.NoVerdictSessionEnded)
			}
			if !strings.Contains(m.Reason, tc.wantText) {
				t.Errorf("Reason = %q, want it to contain %q", m.Reason, tc.wantText)
			}
			if strings.Contains(m.Reason, "an operator closed this session") {
				t.Errorf("the cleanup cause overwrote the state's own explanation: %q", m.Reason)
			}
		})
	}
}

// TestClassifyRound_ErrorStateStillUsesTheRecordedCause is the counterpart:
// state "error" explains nothing on its own, which is the whole defect, so the
// recorded cause must win there.
func TestClassifyRound_ErrorStateStillUsesTheRecordedCause(t *testing.T) {
	sessions := fiveAgentSessions(2)
	qa := sessions[3]

	st := review.ClassifyRoundWithCauses(
		review.AgentsFromSessionsForTest(sessions), sessions,
		fourPassingSiblings(sessions, qa),
		map[string]db.Status{qa: closedRowInState(qa, "error")},
		map[string]db.SessionEndCause{qa: {Cause: db.ReapCauseCleanupCommand}},
	)
	if len(st.Missing) != 1 {
		t.Fatalf("Missing = %d entries, want 1", len(st.Missing))
	}
	if st.Missing[0].Class != review.NoVerdictForceTerminated {
		t.Errorf("Class = %q, want %q", st.Missing[0].Class, review.NoVerdictForceTerminated)
	}
	if !strings.Contains(st.Missing[0].Reason, "prism cleanup") {
		t.Errorf("Reason = %q, want it to name the recorded cause", st.Missing[0].Reason)
	}
}
