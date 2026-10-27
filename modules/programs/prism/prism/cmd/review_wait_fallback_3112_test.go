package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestReviewWait_FallbackReplacesRefusedSession covers issue #3112: a session
// that a fallback session replaced does not fail the round.
func TestReviewWait_FallbackReplacesRefusedSession(t *testing.T) {
	refused := "nixos-config@x~review-1-review-security"
	fallback := refused + "-fallback-1"
	members := map[string]db.GroupMemberResult{
		refused:  {SessionName: refused, State: "error", RunError: "refused", RunErrorRawStopReason: "refusal"},
		fallback: {SessionName: fallback, State: "finished", LastMessage: `{"text":"<verdict>PASS</verdict>"}`},
	}
	all := []db.Status{{SessionName: refused, State: "error"}, {SessionName: fallback, State: "finished"}}

	var err error
	out := captureStdout(t, func() {
		err = emitReviewWaitTerminalAgg("42", "g1", all, members, true)
	})
	if err != nil {
		t.Fatalf("emitReviewWaitTerminalAgg: %v", err)
	}
	var got reviewWaitJSON
	if jErr := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); jErr != nil {
		t.Fatalf("output is not JSON: %v\n%s", jErr, out)
	}
	if got.Verdict != "PASS" || len(got.Agents) != 1 || got.Agents[0].Session != fallback {
		t.Errorf("got verdict %q agents %+v, want PASS with only the fallback session", got.Verdict, got.Agents)
	}
}

// TestReviewWaitDone_WaitsForFallbackAfterRefusal covers the window between
// a refusal and the fallback spawn: the group is complete, but --wait must
// not report it until the settle period has passed.
func TestReviewWaitDone_WaitsForFallbackAfterRefusal(t *testing.T) {
	var since time.Time
	t0 := time.Unix(1000, 0)

	if reviewWaitDone(true, true, &since, t0) {
		t.Fatal("done at the refusal, want a wait for the fallback")
	}
	if reviewWaitDone(true, true, &since, t0.Add(reviewWaitFallbackSettle-time.Second)) {
		t.Fatal("done before the settle period passed")
	}
	if !reviewWaitDone(true, true, &since, t0.Add(reviewWaitFallbackSettle)) {
		t.Fatal("not done after the settle period, want done")
	}
}

// TestReviewWaitDone_NoWaitWithoutFallbackLeft covers issue #3128: when no
// fallback can start, a complete group is done at once.
func TestReviewWaitDone_NoWaitWithoutFallbackLeft(t *testing.T) {
	var since time.Time
	t0 := time.Unix(1000, 0)
	if !reviewWaitDone(true, false, &since, t0) {
		t.Fatal("no fallback left: not done, want done")
	}
	if !since.IsZero() {
		t.Fatal("settle timer started without a fallback left")
	}
}
