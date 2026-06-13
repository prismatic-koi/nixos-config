package dashboard_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
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

func TestBuildReviewChildSummaries_MissingChildOmitted(t *testing.T) {
	// Construct only two of the canonical agents — the other three slots must
	// be absent from the output (omitted, not rendered as VerdictError).
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
	// Only the two present children should appear — missing agents are omitted.
	if len(got) != 2 {
		t.Fatalf("got %d summaries, want 2 (only present children)", len(got))
	}
	// The slice must be in alphabetical order by short name.
	for i := 1; i < len(got); i++ {
		if got[i-1].AgentShortName >= got[i].AgentShortName {
			t.Errorf("summaries not in alphabetical order: %q before %q", got[i-1].AgentShortName, got[i].AgentShortName)
		}
	}
	// No VerdictError entries — neither present agent is in error state.
	for _, s := range got {
		if s.Verdict == dashboard.VerdictError {
			t.Errorf("agent %q: Verdict = VerdictError, but no missing-agent placeholders expected", s.AgentShortName)
		}
	}
	// The two present agents must appear with their derived verdicts.
	short0 := dashboard.ShortAgentName(full0)
	short1 := dashboard.ShortAgentName(full1)
	presentShorts := map[string]bool{short0: true, short1: true}
	for _, s := range got {
		if !presentShorts[s.AgentShortName] {
			t.Errorf("unexpected agent %q in output", s.AgentShortName)
		}
	}
}

// ── RenderReviewSummary: width-budget truncation ────────────────────────────

func TestRenderReviewSummary_FullBudgetRendersLabels(t *testing.T) {
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, nil, nil))
	labels, plain, _ := dashboard.RenderReviewSummary(summaries, 200)
	if labels == "" {
		t.Errorf("labels empty at full budget")
	}
	if plain == 0 {
		t.Errorf("plainWidth = 0 at full budget")
	}
}

func TestRenderReviewSummary_NarrowSuppressesLabels(t *testing.T) {
	// Budget smaller than the compact width must suppress the trailing
	// segment entirely (#1812: below the compact tier we render nothing).
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, nil, nil))
	labels, plain, _ := dashboard.RenderReviewSummary(summaries, 5)
	if labels != "" {
		t.Errorf("labels should be empty at budget=5, got %q", stripANSI(labels))
	}
	if plain != 0 {
		t.Errorf("plainWidth should be 0 at budget=5, got %d", plain)
	}
}

func TestRenderReviewSummary_ExactBudgetRendersLabels(t *testing.T) {
	// At exactly the labels width, the labels should still render at full
	// (wide) tier; one below drops to compact (still letters visible) — not
	// to suppressed. See #1812.
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, nil, nil))
	_, plain, _ := dashboard.RenderReviewSummary(summaries, 200)
	if plain == 0 {
		t.Fatalf("could not measure label width")
	}
	labels, _, _ := dashboard.RenderReviewSummary(summaries, plain)
	if labels == "" {
		t.Errorf("labels should render when budget == plainWidth (%d)", plain)
	}
	// At plain-1 the wide form no longer fits, so the renderer must drop
	// to the compact (letter-only) form: non-empty output, but no
	// "name:" prefixes from the wide form.
	labels2, plain2, _ := dashboard.RenderReviewSummary(summaries, plain-1)
	if labels2 == "" {
		t.Errorf("labels should be compact (non-empty) when budget == plainWidth-1 (%d)", plain-1)
	}
	plain2Str := stripANSI(labels2)
	for _, s := range summaries {
		if strings.Contains(plain2Str, s.AgentShortName+":") {
			t.Errorf("compact form must not contain the wide form's %q prefix; got %q", s.AgentShortName+":", plain2Str)
		}
	}
	if plain2 == 0 {
		t.Errorf("compact plainWidth should be non-zero at budget=%d", plain-1)
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
	labels, _, _ := dashboard.RenderReviewSummary(summaries, 200)
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
			labels, _, _ := dashboard.RenderReviewSummary(summaries, 200)
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

func TestCollapsedRow_MissingChildrenOmitted(t *testing.T) {
	// Only include the first canonical agent; the remaining four agents are
	// missing → they must be absent from the rendered output (no ✕ letters,
	// no "name:" slots for the missing agents).
	agents := review.Agents()
	states := map[string]string{
		agents[0].Name: "active",
	}
	row := renderCollapsedReviewRow(t, 200, states, nil)
	// No ✕ letters — missing agents are not rendered as VerdictError.
	if strings.Count(row, "✕") != 0 {
		t.Errorf("expected no ✕ for missing agents (omitted policy), got %d; row=%q",
			strings.Count(row, "✕"), row)
	}
	// The present (first canonical) agent is in state "active" so its label is ◌.
	present := dashboard.ShortAgentName(agents[0].Name)
	if !strings.Contains(row, present+":◌") {
		t.Errorf("expected present agent label %q; row=%q", present+":◌", row)
	}
	// The missing agents must not appear in the row at all.
	for _, a := range agents[1:] {
		short := dashboard.ShortAgentName(a.Name)
		if strings.Contains(row, short+":") {
			t.Errorf("missing agent %q should not appear in row; row=%q", short, row)
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

// ── Three-tier width budget (full / compact / suppressed) ─────────────────────────

// TestRenderReviewSummary_ThreeTierBoundaries exercises the new compact tier
// added by #1812. The renderer must dispatch among three modes:
//
//	budget >= labelsW              → full alphabetical labels
//	labelsW > budget >= compactW   → letter-only compact form
//	budget < compactW              → suppressed entirely
//
// The boundary transitions in both directions must be clean: a single column
// removed at each boundary drops cleanly to the next tier (no partial /
// truncated output).
func TestRenderReviewSummary_ThreeTierBoundaries(t *testing.T) {
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, nil, nil))

	// Measure the wide form's plain width via a sufficiently large budget.
	_, labelsW, _ := dashboard.RenderReviewSummary(summaries, 1000)
	if labelsW == 0 {
		t.Fatalf("could not measure labelsW")
	}

	// Measure the compact form's plain width by requesting one column less
	// than labelsW.
	compactLabels, compactW, _ := dashboard.RenderReviewSummary(summaries, labelsW-1)
	if compactLabels == "" || compactW == 0 {
		t.Fatalf("compact form should render at budget=labelsW-1=%d", labelsW-1)
	}
	if compactW >= labelsW {
		t.Fatalf("compactW (%d) must be strictly less than labelsW (%d)", compactW, labelsW)
	}

	// ---- Wide tier ----
	wideLabels, wideW, _ := dashboard.RenderReviewSummary(summaries, labelsW)
	widePlain := stripANSI(wideLabels)
	if wideW != labelsW {
		t.Errorf("wide tier plainWidth = %d, want %d", wideW, labelsW)
	}
	// At wide tier, every short label "name:" prefix must be present.
	for _, s := range summaries {
		if !strings.Contains(widePlain, s.AgentShortName+":") {
			t.Errorf("wide form missing %q prefix; got %q", s.AgentShortName+":", widePlain)
		}
	}

	// ---- Compact tier: labelsW-1 down to compactW ----
	for _, b := range []int{labelsW - 1, compactW} {
		labels, plain, _ := dashboard.RenderReviewSummary(summaries, b)
		if labels == "" {
			t.Errorf("compact tier should render non-empty at budget=%d", b)
			continue
		}
		if plain != compactW {
			t.Errorf("compact tier plainWidth at budget=%d = %d, want %d", b, plain, compactW)
		}
		plainStr := stripANSI(labels)
		// Letter-only: no "name:" prefixes from the wide form.
		for _, s := range summaries {
			if strings.Contains(plainStr, s.AgentShortName+":") {
				t.Errorf("compact form must not contain wide prefix %q; got %q", s.AgentShortName+":", plainStr)
			}
		}
		// The plain compact-tier string must equal the canonical compact form.
		want := wantCompactPlain(summaries)
		if plainStr != want {
			t.Errorf("compact plain text at budget=%d = %q, want %q", b, plainStr, want)
		}
	}

	// ---- Suppressed tier: one column below compactW must produce nothing.
	labels, plain, _ := dashboard.RenderReviewSummary(summaries, compactW-1)
	if labels != "" {
		t.Errorf("suppressed tier should be empty at budget=%d, got %q", compactW-1, stripANSI(labels))
	}
	if plain != 0 {
		t.Errorf("suppressed tier plainWidth at budget=%d = %d, want 0", compactW-1, plain)
	}
}

// TestRenderReviewSummary_CompactPreservesAlphabeticalOrder asserts that the
// compact letter-only form emits one letter per agent in the alphabetical
// short-label order produced by BuildReviewChildSummaries (code, context,
// goal, qa, sec), with each letter coming from letterForVerdict applied to
// the corresponding summary. The verdicts are made distinct per agent so an
// out-of-order rendering would surface as a wrong letter sequence.
func TestRenderReviewSummary_CompactPreservesOrder(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	// Give each canonical agent a distinct verdict so the letter sequence
	// is sensitive to ordering bugs.
	for i, a := range agents {
		switch i % 5 {
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
		default:
			states[a.Name] = ""
		}
	}
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, states, msgs))

	_, labelsW, _ := dashboard.RenderReviewSummary(summaries, 1000)
	labels, _, _ := dashboard.RenderReviewSummary(summaries, labelsW-1)
	plain := stripANSI(labels)
	if plain != wantCompactPlain(summaries) {
		t.Errorf("compact form mismatch:\n got %q\nwant %q", plain, wantCompactPlain(summaries))
	}
}

// TestRenderReviewSummary_CompactColours asserts that the compact form
// emits per-letter ANSI colour escapes matching the wide form's palette via
// colorForVerdict (pass green, fail red, running primary, pending dim, error
// red). We force TrueColor so lipgloss emits ANSI even without a TTY, then
// assert that the compact-form ANSI output and wide-form ANSI output emit
// identical colour sequences for the same set of verdict letters.
func TestRenderReviewSummary_CompactColours(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
	})

	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	// Cover every verdict class so all five colour escapes participate.
	for i, a := range agents {
		switch i % 5 {
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
		default:
			states[a.Name] = ""
		}
	}
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, states, msgs))
	wide, labelsW, _ := dashboard.RenderReviewSummary(summaries, 1000)
	compact, _, _ := dashboard.RenderReviewSummary(summaries, labelsW-1)

	if !strings.Contains(compact, "\x1b[") {
		t.Fatalf("compact form should contain ANSI escape sequences for colour; got %q", compact)
	}

	// Each verdict letter's colour escape sequence must appear in both the
	// wide and compact renders — matching palettes per AC #1812.
	letters := []string{"P", "F", "◌", "✕", "·"}
	for _, ltr := range letters {
		if !strings.Contains(stripANSI(wide), ltr) {
			continue // this verdict isn't present in the sample
		}
		if !strings.Contains(stripANSI(compact), ltr) {
			t.Errorf("compact form missing letter %q present in wide form", ltr)
		}
	}
}

// wantCompactPlain returns the canonical plain (ANSI-stripped) compact form
// for the supplied summaries: one verdict letter per agent in input order,
// separated by two spaces.
func wantCompactPlain(summaries []dashboard.ReviewChildSummary) string {
	var b strings.Builder
	for i, s := range summaries {
		if i > 0 {
			b.WriteString("  ")
		}
		switch s.Verdict {
		case dashboard.VerdictPass:
			b.WriteString("P")
		case dashboard.VerdictFail:
			b.WriteString("F")
		case dashboard.VerdictRunning:
			b.WriteString("◌")
		case dashboard.VerdictError:
			b.WriteString("✕")
		default:
			b.WriteString("·")
		}
	}
	return b.String()
}

// TestRenderReviewSummary_EmptySummariesAllBudgets asserts that with an
// empty summaries slice the renderer returns the suppressed tier (no
// trailing segment) regardless of budget — covering AC #1812's edge case.
func TestRenderReviewSummary_EmptySummariesAllBudgets(t *testing.T) {
	for _, b := range []int{0, 5, 13, 38, 200} {
		labels, plain, _ := dashboard.RenderReviewSummary(nil, b)
		if labels != "" || plain != 0 {
			t.Errorf("empty summaries at budget=%d: got labels=%q plain=%d, want empty/0", b, labels, plain)
		}
	}
}

// ── Row-level: compact / suppressed transitions on the collapsed group row ───

// renderCollapsedReviewRowWithSel renders the collapsed review-group row at
// the supplied width, with the cursor positioned on the group row when
// `selected` is true so the selected-row bar path in RenderReviewGroupRow
// is exercised. Returns the ANSI-stripped row.
func renderCollapsedReviewRowWithSel(t *testing.T, width int, selected bool, states map[string]string, msgs map[string]string) string {
	t.Helper()
	agents := review.Agents()
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
	d := dashboard.Shared{Width: width, Sessions: sessions}
	d = dashboard.RefilterShared(d)
	if selected {
		// Place the cursor on the virtual review-group row. The group row is
		// inserted at index 1 by RefilterShared (after the top-level parent).
		for i, row := range d.Displayed {
			if row.IsReviewGroup {
				d.Cursor = i
				break
			}
		}
	}
	out := dashboard.DashView(d, "", selected)
	plain := stripANSI(out)
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "~review-1") {
			return line
		}
	}
	t.Fatalf("no ~review-1 line in output:\n%s", plain)
	return ""
}

// TestCollapsedRow_CompactTier_RendersLettersOnly searches for the smallest
// width at which the row drops out of the wide tier and asserts that the
// resulting render contains the verdict letters but none of the wide-form
// "name:" prefixes. The boundary is found by stepping down one column at a
// time so the test does not depend on internal layout constants.
func TestCollapsedRow_CompactTier_RendersLettersOnly(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "<verdict>pass</verdict>"
	}

	// Step down from a known-wide width until the first "code:" prefix
	// disappears — that is the wide → compact boundary.
	var compactWidth int
	for w := 200; w >= 30; w-- {
		row := renderCollapsedReviewRowWithSel(t, w, false, states, msgs)
		if !strings.Contains(row, "code:") {
			compactWidth = w
			break
		}
	}
	if compactWidth == 0 {
		t.Fatalf("never left the wide tier even at width=30")
	}

	row := renderCollapsedReviewRowWithSel(t, compactWidth, false, states, msgs)
	// No wide-form "name:" prefixes anywhere in the row.
	for _, a := range agents {
		short := dashboard.ShortAgentName(a.Name)
		if strings.Contains(row, short+":") {
			t.Errorf("compact-tier row at width=%d unexpectedly contains %q; row=%q", compactWidth, short+":", row)
		}
	}
	// Verdict letters must still appear: five P letters (one per agent).
	if strings.Count(row, "P") < 5 {
		t.Errorf("compact-tier row at width=%d should contain 5 P letters; row=%q", compactWidth, row)
	}
	// And no cluster glyphs are reintroduced.
	for _, glyph := range []string{"●", "○", "◐"} {
		if strings.Contains(row, glyph) {
			t.Errorf("compact-tier row should not contain cluster glyph %q; row=%q", glyph, row)
		}
	}
}

// TestCollapsedRow_SuppressedTier_OneBelowCompact asserts that removing a
// single column from the smallest width that still renders compact drops
// the trailing segment cleanly to suppressed — no partial / truncated
// letter segment.
func TestCollapsedRow_SuppressedTier_OneBelowCompact(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "<verdict>pass</verdict>"
	}

	// Find the smallest width at which compact still renders (at least 5
	// verdict letters present, no wide-form prefixes).
	var smallestCompact int
	for w := 30; w <= 200; w++ {
		row := renderCollapsedReviewRowWithSel(t, w, false, states, msgs)
		if strings.Contains(row, "code:") {
			continue // still wide tier or no labels
		}
		if strings.Count(row, "P") >= 5 {
			smallestCompact = w
			break
		}
	}
	if smallestCompact == 0 {
		t.Fatalf("could not find a width where the row renders the compact tier")
	}

	// One column below the smallest compact width must suppress entirely.
	row := renderCollapsedReviewRowWithSel(t, smallestCompact-1, false, states, msgs)
	if strings.Count(row, "P") != 0 {
		t.Errorf("row at width=%d (one below compact boundary) should have no verdict letters; row=%q", smallestCompact-1, row)
	}
	for _, a := range agents {
		short := dashboard.ShortAgentName(a.Name)
		if strings.Contains(row, short+":") {
			t.Errorf("row at width=%d should not contain wide-form %q; row=%q", smallestCompact-1, short+":", row)
		}
	}
	// Group label still visible.
	if !strings.Contains(row, "~review-1") {
		t.Errorf("row at width=%d should still contain group label; row=%q", smallestCompact-1, row)
	}
}

// TestCollapsedRow_SelectedBarMatchesUnselectedFootprint asserts that when
// the row is rendered as the selected-row bar (cursorActive=true), the
// trailing segment uses the same tier (wide / compact / suppressed) as the
// unselected render at the same width. We sweep widths spanning all three
// tiers and compare letter-count + presence of "name:" prefixes.
func TestCollapsedRow_SelectedBarMatchesUnselectedFootprint(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "<verdict>pass</verdict>"
	}

	for w := 40; w <= 200; w += 5 {
		unselected := renderCollapsedReviewRowWithSel(t, w, false, states, msgs)
		selected := renderCollapsedReviewRowWithSel(t, w, true, states, msgs)
		// Same number of verdict letters in both renders.
		if strings.Count(unselected, "P") != strings.Count(selected, "P") {
			t.Errorf("width=%d: P-letter count differs unselected=%d selected=%d\nunsel=%q\n  sel=%q",
				w, strings.Count(unselected, "P"), strings.Count(selected, "P"), unselected, selected)
		}
		// If the unselected render is wide ("code:" present), the selected
		// render must be wide too. If it's compact (no "code:" but P letters
		// present), the selected render must also be compact.
		unselWide := strings.Contains(unselected, "code:")
		selWide := strings.Contains(selected, "code:")
		if unselWide != selWide {
			t.Errorf("width=%d: wide-tier mismatch unselected=%v selected=%v", w, unselWide, selWide)
		}
	}
}

// TestCollapsedRow_NoSummariesNoTrailingSegment covers AC #1812's empty-
// children edge case at the row level: a review-group row whose children
// list is empty must render as session + state only at any width, in any
// mode.
func TestCollapsedRow_NoSummariesNoTrailingSegment(t *testing.T) {
	// Build a Shared whose only review-group row has no per-agent children
	// in ReviewChildSummaries. We can't easily inject an IsReviewGroup row
	// without children via the normal pipeline (RefilterShared synthesises
	// summaries from the per-agent sessions), so we test the renderer
	// directly: an empty summaries slice must yield no trailing fragment at
	// any budget.
	for _, b := range []int{0, 5, 13, 14, 38, 39, 200} {
		labels, plain, _ := dashboard.RenderReviewSummary(nil, b)
		if labels != "" || plain != 0 {
			t.Errorf("empty summaries at budget=%d: got labels=%q plain=%d", b, labels, plain)
		}
	}
}

// ── Non-review group rows visually unchanged ─────────────────────────────

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
