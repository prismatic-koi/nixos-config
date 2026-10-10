package cmd

import (
	"strings"
	"testing"
	"time"
)

// A review agent that stalls mid-turn has no msg_assistant row (written at
// turn_end). Issue #3086: checkin must still show its frames, the call in
// progress, and the stall_error.
func TestCheckinTurns_StalledMidTurnShowsTail(t *testing.T) {
	d := openCheckinTestDB(t)
	s := "repo@main~review-2-review-security"
	base := time.Now().Add(-time.Minute)
	writeEvent(t, d, "e1", s, "turn_start", `{"type":"turn_start"}`, base)
	writeEvent(t, d, "e2", s, "tool_call", `{"name":"bash","id":"c1","args":"ls"}`, base.Add(time.Second))
	writeEvent(t, d, "e3", s, "tool_result", `{"id":"c1","success":true,"output":"ok"}`, base.Add(2*time.Second))
	writeEvent(t, d, "e4", s, "tool_call", `{"name":"read","id":"c2","args":"stuck.go"}`, base.Add(3*time.Second))
	writeEvent(t, d, "e5", s, "stall_error", `{"reason":"stalled mid-run"}`, base.Add(4*time.Second))

	out := captureStdout(t, func() {
		if err := renderCheckinTurns(s, d, nil, false); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"read: stuck.go", "[in progress: no result]", "stall_error", "model request in progress"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "in progress: no result") != 1 {
		t.Errorf("only the unresolved call must be flagged:\n%s", out)
	}
}

func TestCheckinTurns_CursorSkipsTail(t *testing.T) {
	d := openCheckinTestDB(t)
	s := "repo@main~review-2-review-security"
	writeEvent(t, d, "e1", s, "stall_error", `{"reason":"x"}`, time.Now())
	out := captureStdout(t, func() {
		if err := renderCheckinTurnsOpts(s, d, nil, false, false); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(out, "stall_error") {
		t.Errorf("tail shown despite showTail=false:\n%s", out)
	}
}
