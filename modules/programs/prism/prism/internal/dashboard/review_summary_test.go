package dashboard_test

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/dashboard"
	"github.com/prismatic-koi/prism/internal/review"
)

// ── ParseVerdict ─────────────────────────────────────────────────────────────

func TestParseVerdict(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"lowercase pass", "summary <verdict>pass</verdict> ok", "pass"},
		{"uppercase pass", "SUMMARY <VERDICT>PASS</VERDICT> OK", "pass"},
		{"mixed-case pass", "<Verdict>Pass</Verdict>", "pass"},
		{"lowercase fail", "<verdict>fail</verdict>", "fail"},
		{"uppercase fail", "<VERDICT>FAIL</VERDICT>", "fail"},
		{"no marker", "no verdict here", ""},
		{"malformed", "<verdict>maybe</verdict>", ""},
		{"pass wins over fail", "<verdict>pass</verdict> and <verdict>fail</verdict>", "pass"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dashboard.ParseVerdict(tt.in)
			if got != tt.want {
				t.Errorf("ParseVerdict(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// ── ShortAgentName ───────────────────────────────────────────────────────────

func TestShortAgentName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"review-goal", "goal"},
		{"review-code", "code"},
		{"review-security", "sec"},
		{"review-qa", "qa"},
		{"review-context", "context"},
		// Future / unknown agents fall through (strip prefix only).
		{"review-rubric", "rubric"},
		{"review-arch", "arch"},
		// Names without the prefix pass through unchanged.
		{"goal", "goal"},
	}
	for _, tt := range tests {
		got := dashboard.ShortAgentName(tt.in)
		if got != tt.want {
			t.Errorf("ShortAgentName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ── BuildReviewChildSummaries ────────────────────────────────────────────────

// canonicalShortNames mirrors the production mapping for assertions. It is
// derived from review.Agents() so the test will break (intentionally) if the
// canonical list changes without updating ShortAgentName.
func canonicalShortNames() []string {
	agents := review.Agents()
	out := make([]string, len(agents))
	for i, a := range agents {
		out[i] = dashboard.ShortAgentName(a.Name)
	}
	return out
}

// buildChildren constructs per-agent AgentSessions for every canonical review
// agent in a single round, with the supplied state and last-message map keyed
// by full agent name (e.g. "review-goal").
func buildChildren(round int, states map[string]string, lastMessages map[string]string) []dashboard.AgentSession {
	agents := review.Agents()
	out := make([]dashboard.AgentSession, 0, len(agents))
	for _, a := range agents {
		name := "repo@branch~review-" + itoaTest(round) + "-" + a.Name
		out = append(out, dashboard.AgentSession{
			Name:        name,
			AgentState:  states[a.Name],
			LastMessage: lastMessages[a.Name],
		})
	}
	return out
}

func itoaTest(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return "10"
}

func TestBuildReviewChildSummaries_CanonicalOrder(t *testing.T) {
	children := buildChildren(1, nil, nil)
	got := dashboard.BuildReviewChildSummaries(children)

	want := canonicalShortNames()
	if len(got) != len(want) {
		t.Fatalf("got %d summaries, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].AgentShortName != w {
			t.Errorf("summary[%d].AgentShortName = %q, want %q", i, got[i].AgentShortName, w)
		}
	}
}

func TestBuildReviewChildSummaries_AllPass(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "summary <verdict>PASS</verdict>"
	}
	got := dashboard.BuildReviewChildSummaries(buildChildren(1, states, msgs))
	for i, s := range got {
		if s.Verdict != dashboard.VerdictPass {
			t.Errorf("summary[%d].Verdict = %q, want pass", i, s.Verdict)
		}
	}
}

func TestBuildReviewChildSummaries_MixedPassFail(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for i, a := range agents {
		states[a.Name] = "finished"
		if i%2 == 0 {
			msgs[a.Name] = "<verdict>pass</verdict>"
		} else {
			msgs[a.Name] = "<verdict>fail</verdict>"
		}
	}
	got := dashboard.BuildReviewChildSummaries(buildChildren(1, states, msgs))
	for i, s := range got {
		var want string
		if i%2 == 0 {
			want = dashboard.VerdictPass
		} else {
			want = dashboard.VerdictFail
		}
		if s.Verdict != want {
			t.Errorf("summary[%d].Verdict = %q, want %q", i, s.Verdict, want)
		}
	}
}

func TestBuildReviewChildSummaries_TwoRunning(t *testing.T) {
	// Three children PASS-finished, two still active (one waiting, one active).
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for i, a := range agents {
		switch i {
		case 0:
			states[a.Name] = "active"
		case 1:
			states[a.Name] = "waiting"
		default:
			states[a.Name] = "finished"
			msgs[a.Name] = "<verdict>pass</verdict>"
		}
	}
	got := dashboard.BuildReviewChildSummaries(buildChildren(2, states, msgs))
	runningCount := 0
	passCount := 0
	for _, s := range got {
		switch s.Verdict {
		case dashboard.VerdictRunning:
			runningCount++
		case dashboard.VerdictPass:
			passCount++
		}
	}
	if runningCount != 2 {
		t.Errorf("runningCount = %d, want 2", runningCount)
	}
	if passCount != 3 {
		t.Errorf("passCount = %d, want 3", passCount)
	}
}

func TestBuildReviewChildSummaries_AllPending(t *testing.T) {
	// All children present but idle (no state, no message).
	got := dashboard.BuildReviewChildSummaries(buildChildren(1, nil, nil))
	for i, s := range got {
		if s.Verdict != dashboard.VerdictPending {
			t.Errorf("summary[%d].Verdict = %q, want pending", i, s.Verdict)
		}
	}
}

func TestBuildReviewChildSummaries_StartupErrorChild(t *testing.T) {
	// All present; one is in error state. The error must surface as VerdictError.
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "<verdict>pass</verdict>"
	}
	// Force the first canonical agent into error state.
	states[agents[0].Name] = "error"
	msgs[agents[0].Name] = ""

	got := dashboard.BuildReviewChildSummaries(buildChildren(1, states, msgs))
	if got[0].Verdict != dashboard.VerdictError {
		t.Errorf("summary[0].Verdict = %q, want error", got[0].Verdict)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Verdict != dashboard.VerdictPass {
			t.Errorf("summary[%d].Verdict = %q, want pass", i, got[i].Verdict)
		}
	}
}

func TestBuildReviewChildSummaries_MissingChildBecomesError(t *testing.T) {
	// Construct only two of the canonical agents — the other three slots must
	// render as VerdictError (✕) per the AC ("missing agents as ✕").
	agents := review.Agents()
	full0 := agents[0].Name
	full1 := agents[1].Name

	children := []dashboard.AgentSession{
		{
			Name:        "repo@branch~review-1-" + full0,
			AgentState:  "finished",
			LastMessage: "<verdict>pass</verdict>",
		},
		{
			Name:        "repo@branch~review-1-" + full1,
			AgentState:  "active",
			LastMessage: "",
		},
	}
	got := dashboard.BuildReviewChildSummaries(children)
	if len(got) != len(agents) {
		t.Fatalf("got %d summaries, want %d (one per canonical agent)", len(got), len(agents))
	}
	if got[0].Verdict != dashboard.VerdictPass {
		t.Errorf("summary[0].Verdict = %q, want pass", got[0].Verdict)
	}
	if got[1].Verdict != dashboard.VerdictRunning {
		t.Errorf("summary[1].Verdict = %q, want running", got[1].Verdict)
	}
	for i := 2; i < len(got); i++ {
		if got[i].Verdict != dashboard.VerdictError {
			t.Errorf("summary[%d].Verdict = %q (agent %q), want error", i, got[i].Verdict, got[i].AgentShortName)
		}
	}
}

// ── RenderReviewSummary: width-budget truncation ────────────────────────────

func TestRenderReviewSummary_FullBudgetRendersAll(t *testing.T) {
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, nil, nil))
	// Plenty of width.
	cluster, labels, tail, _ := dashboard.RenderReviewSummary(summaries, 200)
	if cluster == "" {
		t.Errorf("cluster empty at full budget")
	}
	if labels == "" {
		t.Errorf("labels empty at full budget")
	}
	if tail == "" {
		t.Errorf("tail empty at full budget")
	}
}

func TestRenderReviewSummary_NarrowDropsLabels(t *testing.T) {
	// Budget large enough for cluster + tail but not labels.
	// cluster=5, gap=2, tail="5/5 done"=8 → need 15 for cluster+tail.
	// labels for canonical agents = sum of "name:X" + separators.
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, nil, nil))
	tail := "5/5 done"
	_ = tail
	cluster, labels, gotTail, _ := dashboard.RenderReviewSummary(summaries, 20)
	if cluster == "" {
		t.Errorf("cluster empty at budget=20")
	}
	if labels != "" {
		t.Errorf("labels should be empty at budget=20, got %q", stripANSI(labels))
	}
	if gotTail == "" {
		t.Errorf("tail should be visible at budget=20, got empty")
	}
}

func TestRenderReviewSummary_VeryNarrowKeepsCluster(t *testing.T) {
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, nil, nil))
	// Cluster is 5 glyphs.
	cluster, labels, tail, _ := dashboard.RenderReviewSummary(summaries, 5)
	if cluster == "" {
		t.Errorf("cluster should be visible at budget=5")
	}
	if labels != "" || tail != "" {
		t.Errorf("labels/tail should be empty at budget=5; labels=%q tail=%q", labels, tail)
	}
}

func TestRenderReviewSummary_TooNarrowReturnsEmpty(t *testing.T) {
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, nil, nil))
	cluster, labels, tail, w := dashboard.RenderReviewSummary(summaries, 4)
	if cluster != "" || labels != "" || tail != "" {
		t.Errorf("all fragments must be empty at budget=4; got cluster=%q labels=%q tail=%q", cluster, labels, tail)
	}
	if w != 0 {
		t.Errorf("plainWidth = %d, want 0", w)
	}
}

// ── Progress tail text ───────────────────────────────────────────────────────

func TestProgressTail_AllPass(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "<verdict>pass</verdict>"
	}
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, states, msgs))
	_, _, tail, _ := dashboard.RenderReviewSummary(summaries, 200)
	if !strings.Contains(stripANSI(tail), "all pass") {
		t.Errorf("tail = %q, want to contain 'all pass'", stripANSI(tail))
	}
}

func TestProgressTail_AllFail(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "<verdict>fail</verdict>"
	}
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, states, msgs))
	_, _, tail, _ := dashboard.RenderReviewSummary(summaries, 200)
	if !strings.Contains(stripANSI(tail), "all fail") {
		t.Errorf("tail = %q, want to contain 'all fail'", stripANSI(tail))
	}
}

func TestProgressTail_PartialCounts(t *testing.T) {
	// 2 pass, 1 fail, 2 running → terminal count = 3 → "3/5 done"
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for i, a := range agents {
		switch i {
		case 0, 1:
			states[a.Name] = "finished"
			msgs[a.Name] = "<verdict>pass</verdict>"
		case 2:
			states[a.Name] = "finished"
			msgs[a.Name] = "<verdict>fail</verdict>"
		case 3, 4:
			states[a.Name] = "active"
		}
	}
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, states, msgs))
	_, _, tail, _ := dashboard.RenderReviewSummary(summaries, 200)
	if !strings.Contains(stripANSI(tail), "3/5 done") {
		t.Errorf("tail = %q, want to contain '3/5 done'", stripANSI(tail))
	}
}

func TestProgressTail_ErrorCountsAsTerminal(t *testing.T) {
	// 1 pass, 1 error, 3 running → terminal count = 2 → "2/5 done"
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for i, a := range agents {
		switch i {
		case 0:
			states[a.Name] = "finished"
			msgs[a.Name] = "<verdict>pass</verdict>"
		case 1:
			states[a.Name] = "error"
		default:
			states[a.Name] = "active"
		}
	}
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, states, msgs))
	_, _, tail, _ := dashboard.RenderReviewSummary(summaries, 200)
	if !strings.Contains(stripANSI(tail), "2/5 done") {
		t.Errorf("tail = %q, want to contain '2/5 done'", stripANSI(tail))
	}
}

// ── Integration: collapsed group row contains cluster + labels + tail ───────

// renderCollapsedReviewRow is a small test helper that constructs a dashboard
// Shared, populates a single review-round with the supplied per-agent states
// + last messages, and returns the rendered row (ANSI-stripped) for the
// collapsed group.
func renderCollapsedReviewRow(t *testing.T, width int, states map[string]string, msgs map[string]string) string {
	t.Helper()
	agents := review.Agents()
	sessions := []dashboard.AgentSession{
		{Name: "repo@feature", AgentState: "active", AgentName: "worker"},
	}
	for _, a := range agents {
		// Only include the child if state is set (so we can test the missing-child case).
		st, hasState := states[a.Name]
		if !hasState {
			continue
		}
		sessions = append(sessions, dashboard.AgentSession{
			Name:        "repo@feature~review-1-" + a.Name,
			AgentState:  st,
			LastMessage: msgs[a.Name],
		})
	}
	d := dashboard.Shared{
		Width:    width,
		Sessions: sessions,
	}
	d = dashboard.RefilterShared(d)
	out := dashboard.DashView(d, "", false)
	plain := stripANSI(out)

	// Extract the line containing "~review-1".
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "~review-1") {
			return line
		}
	}
	t.Fatalf("no ~review-1 line in output:\n%s", plain)
	return ""
}

func TestCollapsedRow_AllPass(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "<verdict>pass</verdict>"
	}
	row := renderCollapsedReviewRow(t, 200, states, msgs)
	// Five filled circles.
	if strings.Count(row, "●") != 5 {
		t.Errorf("expected 5 ● glyphs in row, got %d; row=%q", strings.Count(row, "●"), row)
	}
	if !strings.Contains(row, "all pass") {
		t.Errorf("row missing 'all pass'; row=%q", row)
	}
	// Per-agent labels: every canonical short name appears followed by :P
	for _, a := range agents {
		short := dashboard.ShortAgentName(a.Name)
		want := short + ":P"
		if !strings.Contains(row, want) {
			t.Errorf("row missing %q; row=%q", want, row)
		}
	}
}

func TestCollapsedRow_MixedPassFail(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for i, a := range agents {
		states[a.Name] = "finished"
		if i%2 == 0 {
			msgs[a.Name] = "<verdict>pass</verdict>"
		} else {
			msgs[a.Name] = "<verdict>fail</verdict>"
		}
	}
	row := renderCollapsedReviewRow(t, 200, states, msgs)
	// Glyphs: alternating ● and ○.
	if strings.Count(row, "●") < 1 || strings.Count(row, "○") < 1 {
		t.Errorf("expected both ● and ○ glyphs; row=%q", row)
	}
	// Progress tail: terminal count = 5 (all finished with verdicts) but mixed
	// → falls into "5/5 done" (because neither all-pass nor all-fail).
	if !strings.Contains(row, "5/5 done") {
		t.Errorf("row missing '5/5 done'; row=%q", row)
	}
}

func TestCollapsedRow_TwoRunning(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for i, a := range agents {
		switch i {
		case 0:
			states[a.Name] = "active"
		case 1:
			states[a.Name] = "waiting"
		default:
			states[a.Name] = "finished"
			msgs[a.Name] = "<verdict>pass</verdict>"
		}
	}
	row := renderCollapsedReviewRow(t, 200, states, msgs)
	// Two ◐ glyphs (running) and three ●.
	if strings.Count(row, "◐") != 2 {
		t.Errorf("expected 2 ◐ glyphs, got %d; row=%q", strings.Count(row, "◐"), row)
	}
	if strings.Count(row, "●") != 3 {
		t.Errorf("expected 3 ● glyphs, got %d; row=%q", strings.Count(row, "●"), row)
	}
	if !strings.Contains(row, "3/5 done") {
		t.Errorf("row missing '3/5 done'; row=%q", row)
	}
	// Running letter is ◌ — at least two should be present.
	if strings.Count(row, "◌") < 2 {
		t.Errorf("expected >=2 ◌ letters in labels, got %d; row=%q", strings.Count(row, "◌"), row)
	}
}

func TestCollapsedRow_AllPending(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "" // idle
	}
	row := renderCollapsedReviewRow(t, 200, states, nil)
	if strings.Count(row, "·") < 5 {
		t.Errorf("expected at least 5 · in row (5 glyphs + 5 letters), got %d; row=%q",
			strings.Count(row, "·"), row)
	}
	if !strings.Contains(row, "0/5 done") {
		t.Errorf("row missing '0/5 done'; row=%q", row)
	}
}

func TestCollapsedRow_StartupErrorChild(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "<verdict>pass</verdict>"
	}
	// Force the first agent into error.
	states[agents[0].Name] = "error"
	msgs[agents[0].Name] = ""

	row := renderCollapsedReviewRow(t, 200, states, msgs)
	if !strings.Contains(row, "✕") {
		t.Errorf("expected ✕ (error glyph or letter) in row; row=%q", row)
	}
	// Terminal count: 4 pass + 1 error = 5 → not uniform-pass / uniform-fail → "5/5 done"
	if !strings.Contains(row, "5/5 done") {
		t.Errorf("row missing '5/5 done'; row=%q", row)
	}
}

func TestCollapsedRow_MissingChildren(t *testing.T) {
	// Only include the first canonical agent; the remaining four slots are
	// missing → must render as ✕ and the tail must still use /5 as denominator.
	agents := review.Agents()
	states := map[string]string{
		agents[0].Name: "active",
	}
	row := renderCollapsedReviewRow(t, 200, states, nil)
	if strings.Count(row, "✕") < 4 {
		t.Errorf("expected at least 4 ✕ for missing agents, got %d; row=%q",
			strings.Count(row, "✕"), row)
	}
	if !strings.Contains(row, "/5") {
		t.Errorf("tail must still use /5 denominator; row=%q", row)
	}
}

// ── Narrow-width truncation ──────────────────────────────────────────────────

func TestCollapsedRow_NarrowDropsLabels(t *testing.T) {
	// At width=80, all the leaf-row columns are present and the session column
	// is grown to absorb surplus, leaving a small trailing budget that fits
	// the cluster + tail but not the per-agent labels.
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "<verdict>pass</verdict>"
	}
	row := renderCollapsedReviewRow(t, 80, states, msgs)
	// Cluster still visible.
	if strings.Count(row, "●") < 5 {
		t.Errorf("expected 5 ● in narrow row, got %d; row=%q", strings.Count(row, "●"), row)
	}
	// Per-agent labels (look for "goal:") should be absent at narrow widths.
	if strings.Contains(row, "goal:") {
		t.Errorf("expected per-agent labels to be dropped at narrow width; row=%q", row)
	}
}

func TestCollapsedRow_VeryNarrowKeepsClusterFloor(t *testing.T) {
	// At very narrow widths, only the original row content (label + state)
	// remains visible. The cluster never collapses while it fits, but if even
	// the cluster does not fit, the row still renders without crash.
	agents := review.Agents()
	states := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "active"
	}
	row := renderCollapsedReviewRow(t, 60, states, nil)
	// Row must at least contain the group label.
	if !strings.Contains(row, "~review-1") {
		t.Errorf("group label must remain visible at narrow width; row=%q", row)
	}
}

// ── Expanded state: parent row still shows summary ───────────────────────────

func TestCollapsedRow_ExpandedStillShowsSummary(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "<verdict>pass</verdict>"
	}

	sessions := []dashboard.AgentSession{
		{Name: "repo@feature", AgentState: "active", AgentName: "worker"},
	}
	for _, a := range agents {
		sessions = append(sessions, dashboard.AgentSession{
			Name:        "repo@feature~review-1-" + a.Name,
			AgentState:  states[a.Name],
			LastMessage: msgs[a.Name],
		})
	}

	d := dashboard.Shared{
		Width:           200,
		Sessions:        sessions,
		CollapsedGroups: map[string]bool{"repo@feature~review-1": true}, // expanded
	}
	d = dashboard.RefilterShared(d)
	out := stripANSI(dashboard.DashView(d, "", false))

	// Find the group line (the one with "~review-1" but not a per-agent suffix
	// like "review-1-review-goal").
	var groupLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "~review-1") && !strings.Contains(line, "~review-1-review-") {
			groupLine = line
			break
		}
	}
	if groupLine == "" {
		t.Fatalf("group line not found in expanded output:\n%s", out)
	}
	// Summary must still be present on the parent row when expanded.
	if strings.Count(groupLine, "●") != 5 {
		t.Errorf("expanded group row missing cluster; line=%q", groupLine)
	}
	if !strings.Contains(groupLine, "all pass") {
		t.Errorf("expanded group row missing 'all pass' tail; line=%q", groupLine)
	}
	// And per-agent child rows must be visible.
	childCount := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "~review-1-review-") {
			childCount++
		}
	}
	if childCount == 0 {
		t.Errorf("no per-agent child rows visible in expanded output:\n%s", out)
	}
}

// ── Non-review group rows visually unchanged ─────────────────────────────────

func TestNonReviewRowsUnchanged(t *testing.T) {
	// A plain top-level + a regular depth-1 child (non-review branch) must not
	// pick up any cluster glyphs.
	sessions := []dashboard.AgentSession{
		{Name: "repo@main", AgentState: "active", AgentName: "coordinator"},
		{Name: "repo@feature", AgentState: "finished", AgentName: "worker"},
	}
	d := dashboard.Shared{Width: 200, Sessions: sessions}
	d = dashboard.RefilterShared(d)
	out := stripANSI(dashboard.DashView(d, "", false))

	// None of the cluster glyphs should appear on these rows.
	for _, glyph := range []string{"●", "○", "◐", "✕"} {
		if strings.Contains(out, glyph) {
			t.Errorf("non-review rows should not contain glyph %q; output=\n%s", glyph, out)
		}
	}
}
