package review_test

// silent_turn_3088_test.go — issue #3088. A review agent whose run ended after
// a turn with no text and no tool call has a run_error reason. The report must
// name the agent and the stop reason.

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

const silentTurnReason = `the model ended its turn with no text and no tool call (stop reason "error", provider stop reason "refusal"): The model refused to complete the request`

func TestClassifyRound_RunErrorNamesStopReason(t *testing.T) {
	sessions := fiveAgentSessions(1)
	sec := sessions[2]
	groupData := fourPassingSiblings(sessions, sec)
	groupData[sec] = db.GroupMemberResult{SessionName: sec, State: "error", RunError: silentTurnReason}

	st := review.ClassifyRound(review.AgentsFromSessionsForTest(sessions), sessions, groupData, nil)

	if len(st.Missing) != 1 {
		t.Fatalf("Missing = %d entries, want 1: %+v", len(st.Missing), st.Missing)
	}
	m := st.Missing[0]
	if m.Session != sec {
		t.Errorf("Session = %q, want %q", m.Session, sec)
	}
	if m.Class != review.NoVerdictCrashed {
		t.Errorf("Class = %q, want %q", m.Class, review.NoVerdictCrashed)
	}
	if m.Reason != silentTurnReason {
		t.Errorf("Reason = %q, want the run_error reason %q", m.Reason, silentTurnReason)
	}
}

func TestClassifyRound_RunErrorThenClosedNamesStopReason(t *testing.T) {
	sessions := fiveAgentSessions(1)
	sec := sessions[2]

	st := review.ClassifyRoundWithCauses(
		review.AgentsFromSessionsForTest(sessions),
		sessions,
		fourPassingSiblings(sessions, sec),
		map[string]db.Status{sec: closedRow(sec)},
		map[string]db.SessionEndCause{sec: {RunError: silentTurnReason, TmuxSessionEnded: true}},
	)

	if len(st.Missing) != 1 {
		t.Fatalf("Missing = %d entries, want 1", len(st.Missing))
	}
	m := st.Missing[0]
	if m.Class != review.NoVerdictCrashed {
		t.Errorf("Class = %q, want %q", m.Class, review.NoVerdictCrashed)
	}
	if !strings.Contains(m.Reason, `stop reason "error"`) {
		t.Errorf("Reason = %q, want it to name the stop reason", m.Reason)
	}
}

func TestBuildMonitorResults_RunErrorNamesStopReason(t *testing.T) {
	agents := []review.Agent{{Name: "review-security"}}
	sess := "nixos-config@parent~review-1-review-security"
	groupData := map[string]db.GroupMemberResult{
		sess: {SessionName: sess, State: "error", RunError: silentTurnReason},
	}

	results := review.BuildMonitorResultsForTest(agents, []string{sess}, groupData)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Passed || !r.IsError {
		t.Errorf("Passed=%v IsError=%v, want false/true", r.Passed, r.IsError)
	}
	if !strings.Contains(r.Output, silentTurnReason) {
		t.Errorf("output does not carry the run_error reason: %q", r.Output)
	}
}

func TestBuildDeliveryMessage_RunErrorNamesAgentAndStopReason(t *testing.T) {
	sessions := fiveAgentSessions(1)
	sec := sessions[2]
	groupData := fourPassingSiblings(sessions, sec)
	groupData[sec] = db.GroupMemberResult{SessionName: sec, State: "error", RunError: silentTurnReason}

	msg := review.BuildDeliveryMessageForTest("42", 1, "results text", false, groupData, sessions)

	if !strings.Contains(msg, "review-security") {
		t.Errorf("report does not name the agent: %q", msg)
	}
	if !strings.Contains(msg, `stop reason "error"`) {
		t.Errorf("report does not name the stop reason: %q", msg)
	}
}
