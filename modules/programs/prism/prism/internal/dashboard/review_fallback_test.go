package dashboard

import "testing"

const fbBase = "repo@br~review-1-review-goal"

func fbSummary(t *testing.T, kids []AgentSession) ReviewChildSummary {
	t.Helper()
	s := BuildReviewChildSummaries(kids)
	if len(s) != 1 {
		t.Fatalf("want 1 summary, got %d", len(s))
	}
	return s[0]
}

func TestFallbackSummary_LatestWins(t *testing.T) {
	pass := "<verdict>PASS</verdict>"
	cases := []struct {
		name string
		kids []AgentSession
		want string
	}{
		{"pass", []AgentSession{
			{Name: fbBase, AgentState: "error"},
			{Name: fbBase + "-fallback-1", AgentState: "finished", LastMessage: pass}}, VerdictPass},
		{"fail", []AgentSession{
			{Name: fbBase + "-fallback-1", AgentState: "finished", LastMessage: "<verdict>FAIL</verdict>"},
			{Name: fbBase, AgentState: "error"}}, VerdictFail},
		{"running", []AgentSession{
			{Name: fbBase, AgentState: "error"},
			{Name: fbBase + "-fallback-1", AgentState: "active"}}, VerdictRunning},
		{"last errors", []AgentSession{
			{Name: fbBase, AgentState: "error"},
			{Name: fbBase + "-fallback-1", AgentState: "error"}}, VerdictError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fbSummary(t, c.kids).Verdict; got != c.want {
				t.Errorf("got %s want %s", got, c.want)
			}
		})
	}
}

func TestFallbackGroup_StateIgnoresReplacedAttempt(t *testing.T) {
	kids := []AgentSession{
		{Name: fbBase, AgentState: "error"},
		{Name: fbBase + "-fallback-1", AgentState: "finished", LastMessage: "<verdict>PASS</verdict>"},
	}
	for _, expanded := range []bool{false, true} {
		rows, _ := BuildDisplayRows(kids, map[string]bool{"repo@br~review-1": expanded}, "")
		if rows[0].AgentState != "finished" {
			t.Errorf("group state = %q, want finished", rows[0].AgentState)
		}
		if expanded {
			if len(rows) != 3 || rows[1].AgentState != "error" || rows[2].AgentState != "finished" {
				t.Errorf("children not kept with own state: %+v", rows)
			}
		}
	}
}
