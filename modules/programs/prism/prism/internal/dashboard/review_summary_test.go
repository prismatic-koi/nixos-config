package dashboard_test

import (
	"sort"
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

// alphabeticalShortNames returns the canonical review-agent short labels in
// alphabetical order — the new display order asserted by #1802. It is
// derived from review.Agents() so the test will break (intentionally) if the
// canonical list changes without updating ShortAgentName.
func alphabeticalShortNames() []string {
	agents := review.Agents()
	out := make([]string, len(agents))
	for i, a := range agents {
		out[i] = dashboard.ShortAgentName(a.Name)
	}
	sort.Strings(out)
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

func TestBuildReviewChildSummaries_AlphabeticalOrder(t *testing.T) {
	children := buildChildren(1, nil, nil)
	got := dashboard.BuildReviewChildSummaries(children)

	want := alphabeticalShortNames()
	if len(got) != len(want) {
		t.Fatalf("got %d summaries, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].AgentShortName != w {
			t.Errorf("summary[%d].AgentShortName = %q, want %q (alphabetical order)", i, got[i].AgentShortName, w)
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
	// Mark each agent pass/fail by full name (not by index in the alphabetical
	// output order). Even canonical index → pass; odd → fail.
	for i, a := range agents {
		states[a.Name] = "finished"
		if i%2 == 0 {
			msgs[a.Name] = "<verdict>pass</verdict>"
		} else {
			msgs[a.Name] = "<verdict>fail</verdict>"
		}
	}
	// Build the expected verdict-by-short-name map to compare in alphabetical order.
	wantByShort := map[string]string{}
	for i, a := range agents {
		short := dashboard.ShortAgentName(a.Name)
		if i%2 == 0 {
			wantByShort[short] = dashboard.VerdictPass
		} else {
			wantByShort[short] = dashboard.VerdictFail
		}
	}
	got := dashboard.BuildReviewChildSummaries(buildChildren(1, states, msgs))
	for i, s := range got {
		want := wantByShort[s.AgentShortName]
		if s.Verdict != want {
			t.Errorf("summary[%d] (%s).Verdict = %q, want %q", i, s.AgentShortName, s.Verdict, want)
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
	errAgent := agents[0].Name
	errShort := dashboard.ShortAgentName(errAgent)
	states[errAgent] = "error"
	msgs[errAgent] = ""

	got := dashboard.BuildReviewChildSummaries(buildChildren(1, states, msgs))
	for _, s := range got {
		if s.AgentShortName == errShort {
			if s.Verdict != dashboard.VerdictError {
				t.Errorf("error agent %q: Verdict = %q, want error", errShort, s.Verdict)
			}
		} else {
			if s.Verdict != dashboard.VerdictPass {
				t.Errorf("agent %q: Verdict = %q, want pass", s.AgentShortName, s.Verdict)
			}
		}
	}
}

func TestBuildReviewChildSummaries_MissingChildBecomesError(t *testing.T) {
	// Construct only two of the canonical agents — the other three slots must
	// render as VerdictError (✕) per the AC ("missing agents as ✕"), in their
	// alphabetical slot.
	agents := review.Agents()
	full0 := agents[0].Name
	full1 := agents[1].Name
	present := map[string]bool{
		dashboard.ShortAgentName(full0): true,
		dashboard.ShortAgentName(full1): true,
	}

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
	// The slice must already be in alphabetical order by short name.
	for i := 1; i < len(got); i++ {
		if got[i-1].AgentShortName >= got[i].AgentShortName {
			t.Errorf("summaries not in alphabetical order: %q before %q", got[i-1].AgentShortName, got[i].AgentShortName)
		}
	}
	// Missing agents (not in `present`) must be VerdictError. The two present
	// ones keep their derived verdicts (pass / running).
	for _, s := range got {
		if !present[s.AgentShortName] {
			if s.Verdict != dashboard.VerdictError {
				t.Errorf("missing agent %q: Verdict = %q, want error", s.AgentShortName, s.Verdict)
			}
		}
	}
}

// ── RenderReviewSummary: width-budget truncation ────────────────────────────

func TestRenderReviewSummary_FullBudgetRendersLabels(t *testing.T) {
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, nil, nil))
	labels, plain := dashboard.RenderReviewSummary(summaries, 200)
	if labels == "" {
		t.Errorf("labels empty at full budget")
	}
	if plain == 0 {
		t.Errorf("plainWidth = 0 at full budget")
	}
}

func TestRenderReviewSummary_NarrowSuppressesLabels(t *testing.T) {
	// Budget smaller than the labels width must suppress the labels entirely
	// (no cluster fallback — the new design is binary).
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, nil, nil))
	labels, plain := dashboard.RenderReviewSummary(summaries, 10)
	if labels != "" {
		t.Errorf("labels should be empty at budget=10, got %q", stripANSI(labels))
	}
	if plain != 0 {
		t.Errorf("plainWidth should be 0 at budget=10, got %d", plain)
	}
}

func TestRenderReviewSummary_ExactBudgetRendersLabels(t *testing.T) {
	// At exactly the labels width, the labels should still render.
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, nil, nil))
	_, plain := dashboard.RenderReviewSummary(summaries, 200)
	if plain == 0 {
		t.Fatalf("could not measure label width")
	}
	labels, _ := dashboard.RenderReviewSummary(summaries, plain)
	if labels == "" {
		t.Errorf("labels should render when budget == plainWidth (%d)", plain)
	}
	labels2, _ := dashboard.RenderReviewSummary(summaries, plain-1)
	if labels2 != "" {
		t.Errorf("labels should be suppressed when budget == plainWidth-1 (%d)", plain-1)
	}
}

// ── No cluster, no tail ──────────────────────────────────────────────────────

// TestRenderReviewSummary_NoClusterGlyphs guards against the cluster being
// reintroduced: none of the cluster glyphs (●○◐) must ever appear in the
// rendered output. ✕ is still valid as a letter for VerdictError, and · is a
// valid letter for VerdictPending — so they are not asserted-absent here.
func TestRenderReviewSummary_NoClusterGlyphs(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	// Mix of all verdict classes so any cluster path would surface a glyph.
	for i, a := range agents {
		switch i % 4 {
		case 0:
			states[a.Name] = "finished"
			msgs[a.Name] = "<verdict>pass</verdict>"
		case 1:
			states[a.Name] = "finished"
			msgs[a.Name] = "<verdict>fail</verdict>"
		case 2:
			states[a.Name] = "active"
		case 3:
			states[a.Name] = "error"
		}
	}
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, states, msgs))
	labels, _ := dashboard.RenderReviewSummary(summaries, 200)
	plain := stripANSI(labels)
	for _, glyph := range []string{"●", "○", "◐"} {
		if strings.Contains(plain, glyph) {
			t.Errorf("labels should not contain cluster glyph %q; got %q", glyph, plain)
		}
	}
}

// TestRenderReviewSummary_NoProgressTail guards against the progress tail
// ("N/5 done", "all pass", "all fail") being reintroduced.
func TestRenderReviewSummary_NoProgressTail(t *testing.T) {
	cases := []struct {
		name  string
		state string
		msg   string
	}{
		{"all pass", "finished", "<verdict>pass</verdict>"},
		{"all fail", "finished", "<verdict>fail</verdict>"},
		{"all active", "active", ""},
	}
	agents := review.Agents()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			states := map[string]string{}
			msgs := map[string]string{}
			for _, a := range agents {
				states[a.Name] = tc.state
				msgs[a.Name] = tc.msg
			}
			summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, states, msgs))
			labels, _ := dashboard.RenderReviewSummary(summaries, 200)
			plain := stripANSI(labels)
			for _, forbidden := range []string{"all pass", "all fail", "0/5", "1/5", "2/5", "3/5", "4/5", "5/5", " done"} {
				if strings.Contains(plain, forbidden) {
					t.Errorf("labels should not contain tail fragment %q; got %q", forbidden, plain)
				}
			}
		})
	}
}

// ── Integration: collapsed group row contains the alphabetical labels ───────

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
	// Per-agent labels: every canonical short name appears followed by :P.
	for _, a := range agents {
		short := dashboard.ShortAgentName(a.Name)
		want := short + ":P"
		if !strings.Contains(row, want) {
			t.Errorf("row missing %q; row=%q", want, row)
		}
	}
	// No cluster glyphs, no tail.
	for _, forbidden := range []string{"●", "○", "◐", "all pass", "5/5 done"} {
		if strings.Contains(row, forbidden) {
			t.Errorf("row should not contain %q; row=%q", forbidden, row)
		}
	}
}

func TestCollapsedRow_AlphabeticalOrder(t *testing.T) {
	// The labels must appear in alphabetical order by short label, not in
	// review.Agents() canonical order.
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "<verdict>pass</verdict>"
	}
	row := renderCollapsedReviewRow(t, 200, states, msgs)
	shorts := alphabeticalShortNames()
	// Walk through the row and find the position of each short:letter label.
	prev := -1
	for _, s := range shorts {
		idx := strings.Index(row, s+":")
		if idx < 0 {
			t.Fatalf("short label %q not in row: %q", s, row)
		}
		if idx <= prev {
			t.Errorf("label %q at idx=%d appears before previous label (idx=%d); row=%q", s, idx, prev, row)
		}
		prev = idx
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
	// Both :P and :F letters should appear among the labels.
	if !strings.Contains(row, ":P") {
		t.Errorf("expected at least one :P label; row=%q", row)
	}
	if !strings.Contains(row, ":F") {
		t.Errorf("expected at least one :F label; row=%q", row)
	}
	// No cluster, no tail.
	for _, forbidden := range []string{"●", "○", "◐", "5/5 done"} {
		if strings.Contains(row, forbidden) {
			t.Errorf("row should not contain %q; row=%q", forbidden, row)
		}
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
	// Running letter is ◌ — two should be present in labels.
	if strings.Count(row, "◌") != 2 {
		t.Errorf("expected exactly 2 ◌ letters in labels, got %d; row=%q", strings.Count(row, "◌"), row)
	}
	// Three :P letters.
	if strings.Count(row, ":P") != 3 {
		t.Errorf("expected exactly 3 :P labels, got %d; row=%q", strings.Count(row, ":P"), row)
	}
	// No tail.
	if strings.Contains(row, "3/5 done") {
		t.Errorf("row should not contain '3/5 done'; row=%q", row)
	}
}

func TestCollapsedRow_AllPending(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "" // idle
	}
	row := renderCollapsedReviewRow(t, 200, states, nil)
	// Pending letter is · — every label must use it.
	if strings.Count(row, ":·") != len(agents) {
		t.Errorf("expected %d :· labels (pending), got %d; row=%q", len(agents), strings.Count(row, ":·"), row)
	}
	// No tail.
	if strings.Contains(row, "0/5 done") {
		t.Errorf("row should not contain '0/5 done'; row=%q", row)
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
	// Exactly one ✕ should be present (the error letter for the one error agent).
	if strings.Count(row, "✕") != 1 {
		t.Errorf("expected exactly 1 ✕ in row, got %d; row=%q", strings.Count(row, "✕"), row)
	}
	// And the corresponding error short name + :✕ pair.
	errShort := dashboard.ShortAgentName(agents[0].Name)
	if !strings.Contains(row, errShort+":✕") {
		t.Errorf("expected %q in row; row=%q", errShort+":✕", row)
	}
}

func TestCollapsedRow_MissingChildren(t *testing.T) {
	// Only include the first canonical agent; the remaining four slots are
	// missing → must render as ✕ in their alphabetical slot.
	agents := review.Agents()
	states := map[string]string{
		agents[0].Name: "active",
	}
	row := renderCollapsedReviewRow(t, 200, states, nil)
	// Four missing → four ✕ letters.
	if strings.Count(row, "✕") != 4 {
		t.Errorf("expected exactly 4 ✕ for missing agents, got %d; row=%q",
			strings.Count(row, "✕"), row)
	}
	// The present (first canonical) agent is in state "active" so its label is ◌.
	present := dashboard.ShortAgentName(agents[0].Name)
	if !strings.Contains(row, present+":◌") {
		t.Errorf("expected present agent label %q; row=%q", present+":◌", row)
	}
	// And the alphabetical slot rule means every short label appears, even the
	// missing ones (with ✕).
	for _, a := range agents {
		short := dashboard.ShortAgentName(a.Name)
		if !strings.Contains(row, short+":") {
			t.Errorf("expected short label %q to appear in row; row=%q", short, row)
		}
	}
}

// ── Narrow-width truncation ──────────────────────────────────────────────────

func TestCollapsedRow_NarrowDropsLabels(t *testing.T) {
	// At width=70 the trailing budget is below the labels width
	// ("code:P  context:P  goal:P  qa:P  sec:P" = 38 runes), so labels are
	// suppressed entirely. The row renders only session + state.
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "<verdict>pass</verdict>"
	}
	row := renderCollapsedReviewRow(t, 70, states, msgs)
	// No per-agent labels.
	for _, a := range agents {
		short := dashboard.ShortAgentName(a.Name)
		if strings.Contains(row, short+":") {
			t.Errorf("expected per-agent labels to be dropped at narrow width; saw %q in row=%q", short+":", row)
		}
	}
	// No cluster glyphs (the cluster is gone in #1802).
	for _, glyph := range []string{"●", "○", "◐"} {
		if strings.Contains(row, glyph) {
			t.Errorf("narrow row should not contain cluster glyph %q; row=%q", glyph, row)
		}
	}
	// Row must still contain the group label.
	if !strings.Contains(row, "~review-1") {
		t.Errorf("narrow row missing group label; row=%q", row)
	}
}

func TestCollapsedRow_VeryNarrowNoCrash(t *testing.T) {
	// At very narrow widths, only session + state remain visible.
	agents := review.Agents()
	states := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "active"
	}
	row := renderCollapsedReviewRow(t, 50, states, nil)
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
	// Per-agent labels must still appear on the parent row when expanded.
	for _, a := range agents {
		short := dashboard.ShortAgentName(a.Name)
		want := short + ":P"
		if !strings.Contains(groupLine, want) {
			t.Errorf("expanded group row missing label %q; line=%q", want, groupLine)
		}
	}
	// No cluster, no tail.
	for _, forbidden := range []string{"●", "○", "◐", "all pass"} {
		if strings.Contains(groupLine, forbidden) {
			t.Errorf("expanded group row should not contain %q; line=%q", forbidden, groupLine)
		}
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
	// pick up any cluster glyphs or progress tails.
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
	// None of the progress-tail strings either.
	for _, tail := range []string{"all pass", "all fail", "/5 done"} {
		if strings.Contains(out, tail) {
			t.Errorf("non-review rows should not contain tail %q; output=\n%s", tail, out)
		}
	}
}
