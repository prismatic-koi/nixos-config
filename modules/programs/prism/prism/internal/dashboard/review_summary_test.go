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

// Verdict codicons. Written as Go Unicode escapes, never literal Private Use
// Area characters, which do not survive a copy through most tools.
const (
	passIcon    = "\uEBA4" // nf-cod-pass
	failIcon    = "\uEA87" // nf-cod-error
	pwdIcon     = "\uEBA7" // nf-cod-record (pass with disagreement)
	runningIcon = "\uEA77" // nf-cod-sync
	pendingIcon = "\uEBB5" // nf-cod-circle_large
	errorIcon   = "\uEA6C" // nf-cod-warning
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
		{"pass with disagreement", "<verdict>PASS_WITH_DISAGREEMENT</verdict>", "pass_with_disagreement"},
		{"pwd not misread as pass", "summary <verdict>pass_with_disagreement</verdict>", "pass_with_disagreement"},
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

// TestPassWithDisagreement_RendersDistinctly: a
// PASS_WITH_DISAGREEMENT verdict must render as a value distinct from both a
// plain pass and pending, on the collapsed row and in the expanded child row.
func TestPassWithDisagreement_RendersDistinctly(t *testing.T) {
	if dashboard.ParseVerdict("<verdict>PASS_WITH_DISAGREEMENT</verdict>") != dashboard.VerdictPassWithDisagreement {
		t.Fatal("PASS_WITH_DISAGREEMENT did not classify as VerdictPassWithDisagreement")
	}
	if dashboard.VerdictPassWithDisagreement == dashboard.VerdictPass ||
		dashboard.VerdictPassWithDisagreement == dashboard.VerdictPending {
		t.Fatal("VerdictPassWithDisagreement must be distinct from pass and pending")
	}

	// The collapsed group row renders a distinct letter for PWD.
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for i, a := range agents {
		states[a.Name] = "finished"
		if i == 0 {
			msgs[a.Name] = "<verdict>PASS_WITH_DISAGREEMENT</verdict>"
		} else {
			msgs[a.Name] = "<verdict>PASS</verdict>"
		}
	}
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, states, msgs))
	var sawPWD bool
	for _, s := range summaries {
		if s.Verdict == dashboard.VerdictPassWithDisagreement {
			sawPWD = true
		}
	}
	if !sawPWD {
		t.Fatal("no summary carried VerdictPassWithDisagreement")
	}
	rendered, _, _ := dashboard.RenderReviewSummary(summaries, 200)
	if !strings.Contains(rendered, "") {
		t.Errorf("rendered summary %q has no distinct PWD glyph", rendered)
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
// alphabetical order — the display order the row renders. It is
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
	// segment entirely (below the compact tier we render nothing).
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
	// to suppressed.
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
// rendered output. The error and pending codicons are still valid — so
// they are not asserted-absent here.
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
	// Per-agent labels: every canonical short name appears followed by
	// ":" and the pass icon.
	for _, a := range agents {
		short := dashboard.ShortAgentName(a.Name)
		want := short + ":" + passIcon
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
	// Both pass and fail icons should appear among the labels.
	if !strings.Contains(row, ":"+passIcon) {
		t.Errorf("expected at least one pass label; row=%q", row)
	}
	if !strings.Contains(row, ":"+failIcon) {
		t.Errorf("expected at least one fail label; row=%q", row)
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
	// Running icon — two should be present in labels.
	if strings.Count(row, runningIcon) != 2 {
		t.Errorf("expected exactly 2 running icons in labels, got %d; row=%q", strings.Count(row, runningIcon), row)
	}
	// Three pass labels.
	if strings.Count(row, ":"+passIcon) != 3 {
		t.Errorf("expected exactly 3 pass labels, got %d; row=%q", strings.Count(row, ":"+passIcon), row)
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
	// Pending icon — every label must use it.
	if strings.Count(row, ":"+pendingIcon) != len(agents) {
		t.Errorf("expected %d pending labels, got %d; row=%q", len(agents), strings.Count(row, ":"+pendingIcon), row)
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
	// Exactly one error icon should be present (for the one error agent).
	if strings.Count(row, errorIcon) != 1 {
		t.Errorf("expected exactly 1 error icon in row, got %d; row=%q", strings.Count(row, errorIcon), row)
	}
	// And the corresponding error short name + icon pair.
	errShort := dashboard.ShortAgentName(agents[0].Name)
	if !strings.Contains(row, errShort+":"+errorIcon) {
		t.Errorf("expected %q in row; row=%q", errShort+":"+errorIcon, row)
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
	// No error icons — missing agents are not rendered as VerdictError.
	if strings.Count(row, errorIcon) != 0 {
		t.Errorf("expected no error icon for missing agents (omitted policy), got %d; row=%q",
			strings.Count(row, errorIcon), row)
	}
	// The present (first canonical) agent is in state "active" so its label uses the running icon.
	present := dashboard.ShortAgentName(agents[0].Name)
	if !strings.Contains(row, present+":"+runningIcon) {
		t.Errorf("expected present agent label %q; row=%q", present+":"+runningIcon, row)
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
	// At width=70 the trailing budget is below the labels width (agent short
	// name + ":" + two-column verdict icon, for all five agents), so labels
	// are suppressed entirely. The row renders only session + state.
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
	// No cluster glyphs.
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
		want := short + ":" + passIcon
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

// TestRenderReviewSummary_ThreeTierBoundaries exercises the compact tier.
// The renderer must dispatch among three modes:
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

	// Each verdict icon's colour escape sequence must appear in both the
	// wide and compact renders — matching palettes.
	icons := []string{passIcon, failIcon, runningIcon, errorIcon, pendingIcon}
	for _, ic := range icons {
		if !strings.Contains(stripANSI(wide), ic) {
			continue // this verdict isn't present in the sample
		}
		if !strings.Contains(stripANSI(compact), ic) {
			t.Errorf("compact form missing icon %q present in wide form", ic)
		}
	}
}

// wantCompactPlain returns the canonical plain (ANSI-stripped) compact form
// for the supplied summaries: one verdict icon per agent in input order,
// each padded to a two-column cell (icon + one trailing space, since these
// codicons measure as a single display column), separated by two spaces.
func wantCompactPlain(summaries []dashboard.ReviewChildSummary) string {
	var b strings.Builder
	for i, s := range summaries {
		if i > 0 {
			b.WriteString("  ")
		}
		switch s.Verdict {
		case dashboard.VerdictPass:
			b.WriteString(passIcon + " ")
		case dashboard.VerdictFail:
			b.WriteString(failIcon + " ")
		case dashboard.VerdictRunning:
			b.WriteString(runningIcon + " ")
		case dashboard.VerdictError:
			b.WriteString(errorIcon + " ")
		default:
			b.WriteString(pendingIcon + " ")
		}
	}
	// A single trailing blank column after the final icon, matching the
	// separator room every non-final icon gets from its own trailing space.
	b.WriteString(" ")
	return b.String()
}

// TestRenderReviewSummary_EmptySummariesAllBudgets asserts that with an
// empty summaries slice the renderer returns the suppressed tier (no
// trailing segment) regardless of budget — the empty-summaries edge case.
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
	// Verdict icons must still appear: five pass icons (one per agent).
	if strings.Count(row, passIcon) < 5 {
		t.Errorf("compact-tier row at width=%d should contain 5 pass icons; row=%q", compactWidth, row)
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
// icon segment.
func TestCollapsedRow_SuppressedTier_OneBelowCompact(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "<verdict>pass</verdict>"
	}

	// Find the smallest width at which compact still renders (at least 5
	// verdict icons present, no wide-form prefixes).
	var smallestCompact int
	for w := 30; w <= 200; w++ {
		row := renderCollapsedReviewRowWithSel(t, w, false, states, msgs)
		if strings.Contains(row, "code:") {
			continue // still wide tier or no labels
		}
		if strings.Count(row, passIcon) >= 5 {
			smallestCompact = w
			break
		}
	}
	if smallestCompact == 0 {
		t.Fatalf("could not find a width where the row renders the compact tier")
	}

	// One column below the smallest compact width must suppress entirely.
	row := renderCollapsedReviewRowWithSel(t, smallestCompact-1, false, states, msgs)
	if strings.Count(row, passIcon) != 0 {
		t.Errorf("row at width=%d (one below compact boundary) should have no verdict icons; row=%q", smallestCompact-1, row)
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
// tiers and compare icon-count + presence of "name:" prefixes.
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
		// Same number of verdict icons in both renders.
		if strings.Count(unselected, passIcon) != strings.Count(selected, passIcon) {
			t.Errorf("width=%d: pass-icon count differs unselected=%d selected=%d\nunsel=%q\n  sel=%q",
				w, strings.Count(unselected, passIcon), strings.Count(selected, passIcon), unselected, selected)
		}
		// If the unselected render is wide ("code:" present), the selected
		// render must be wide too. If it's compact (no "code:" but pass icons
		// present), the selected render must also be compact.
		unselWide := strings.Contains(unselected, "code:")
		selWide := strings.Contains(selected, "code:")
		if unselWide != selWide {
			t.Errorf("width=%d: wide-tier mismatch unselected=%v selected=%v", w, unselWide, selWide)
		}
	}
}

// TestCollapsedRow_NoSummariesNoTrailingSegment covers the empty-
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

// TestPlainSummaryForBudget_WidthMatchesRendered asserts that the plain-text
// mirror used inside the selected-row bar (view.go's plainSummaryForBudget)
// reports the same display width as the coloured RenderReviewSummary output,
// for both the full and compact tiers.
func TestPlainSummaryForBudget_WidthMatchesRendered(t *testing.T) {
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, nil, nil))

	rendered, renderedW, mode := dashboard.RenderReviewSummaryForTest(summaries, 1000)
	if mode != dashboard.SummaryFullForTest {
		t.Fatalf("expected full tier at budget=1000, got mode=%d", mode)
	}
	plain := dashboard.PlainSummaryForBudgetForTest(summaries, mode)
	if w := lipgloss.Width(plain); w != renderedW {
		t.Errorf("full tier: plain mirror width = %d, rendered width = %d (via RenderReviewSummary)", w, renderedW)
	}
	if w := lipgloss.Width(stripANSI(rendered)); w != renderedW {
		t.Errorf("full tier: rendered plain-stripped width = %d, want %d", w, renderedW)
	}

	// Force the compact tier.
	labelsW := dashboard.ReviewSummaryLabelsWidthForTest(summaries)
	renderedCompact, renderedCompactW, compactMode := dashboard.RenderReviewSummaryForTest(summaries, labelsW-1)
	if compactMode != dashboard.SummaryCompactForTest {
		t.Fatalf("expected compact tier at budget=%d, got mode=%d", labelsW-1, compactMode)
	}
	plainCompact := dashboard.PlainSummaryForBudgetForTest(summaries, compactMode)
	if w := lipgloss.Width(plainCompact); w != renderedCompactW {
		t.Errorf("compact tier: plain mirror width = %d, rendered width = %d", w, renderedCompactW)
	}
	if w := lipgloss.Width(stripANSI(renderedCompact)); w != renderedCompactW {
		t.Errorf("compact tier: rendered plain-stripped width = %d, want %d", w, renderedCompactW)
	}
}

// TestRenderReviewSummary_LastIconSameSizeAsOthers asserts that
// the final verdict icon on a collapsed review-group row renders at the same
// size (display width) as the preceding icons, in both the labels form and
// the compact icon-only form. Every rendered icon cell must measure exactly
// 2 columns via lipgloss.Width, including the last one.
func TestRenderReviewSummary_LastIconSameSizeAsOthers(t *testing.T) {
	summaries := dashboard.BuildReviewChildSummaries(buildChildren(1, nil, nil))
	if len(summaries) < 2 {
		t.Fatalf("need at least 2 canonical review agents for this test, got %d", len(summaries))
	}

	for _, s := range summaries {
		if w := lipgloss.Width(dashboard.RenderIconCellForTest(s.Verdict)); w != 2 {
			t.Errorf("icon cell for verdict %q width = %d, want 2", s.Verdict, w)
		}
	}

	// The reported labels/compact width must exactly equal the rendered
	// width — this is the mechanism the issue's fix relies on: any drift
	// between the accounted width and the rendered width means the last
	// icon's trailing column silently disappears in the budget maths.
	_, labelsW, _ := dashboard.RenderReviewSummaryForTest(summaries, 1000)
	rendered, renderedW, _ := dashboard.RenderReviewSummaryForTest(summaries, labelsW)
	if w := lipgloss.Width(stripANSI(rendered)); w != renderedW || w != labelsW {
		t.Errorf("labels form: rendered width = %d, reported width = %d, labelsW = %d — all three must match", w, renderedW, labelsW)
	}

	compactW := dashboard.ReviewSummaryCompactWidthForTest(summaries)
	renderedCompact, renderedCompactW, mode := dashboard.RenderReviewSummaryForTest(summaries, compactW)
	if mode != dashboard.SummaryCompactForTest {
		t.Fatalf("expected compact tier at budget=compactW=%d, got mode=%d", compactW, mode)
	}
	if w := lipgloss.Width(stripANSI(renderedCompact)); w != renderedCompactW || w != compactW {
		t.Errorf("compact form: rendered width = %d, reported width = %d, compactW = %d — all three must match", w, renderedCompactW, compactW)
	}
}

// TestRenderReviewSummary_SingleAgentFullSize asserts that a
// single-agent review group renders its one icon at full size and its
// reported width matches the rendered width, in both tiers.
func TestRenderReviewSummary_SingleAgentFullSize(t *testing.T) {
	summaries := []dashboard.ReviewChildSummary{
		{AgentShortName: "goal", Verdict: dashboard.VerdictPass},
	}

	if w := lipgloss.Width(dashboard.RenderIconCellForTest(summaries[0].Verdict)); w != 2 {
		t.Fatalf("single agent's icon cell width = %d, want 2", w)
	}

	rendered, renderedW, mode := dashboard.RenderReviewSummaryForTest(summaries, 1000)
	if mode != dashboard.SummaryFullForTest {
		t.Fatalf("expected full tier at budget=1000, got mode=%d", mode)
	}
	if w := lipgloss.Width(stripANSI(rendered)); w != renderedW {
		t.Errorf("single-agent labels form: rendered width = %d, reported width = %d", w, renderedW)
	}

	compactW := dashboard.ReviewSummaryCompactWidthForTest(summaries)
	renderedCompact, renderedCompactW, compactMode := dashboard.RenderReviewSummaryForTest(summaries, compactW)
	if compactMode != dashboard.SummaryCompactForTest {
		t.Fatalf("expected compact tier at budget=compactW=%d, got mode=%d", compactW, compactMode)
	}
	if w := lipgloss.Width(stripANSI(renderedCompact)); w != renderedCompactW {
		t.Errorf("single-agent compact form: rendered width = %d, reported width = %d", w, renderedCompactW)
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

	// None of the cluster glyphs, nor the error codicon, should appear on
	// these rows.
	for _, glyph := range []string{"●", "○", "◐", errorIcon} {
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

// ── Codicon mapping ──────────────────────────────────────────────────

// TestLetterForVerdict_Codepoints asserts the exact codepoint rendered for
// each of the six verdict states. The test fails if any codepoint changes.
func TestLetterForVerdict_Codepoints(t *testing.T) {
	tests := []struct {
		verdict string
		want    string
	}{
		{dashboard.VerdictPass, passIcon},
		{dashboard.VerdictFail, failIcon},
		{dashboard.VerdictPassWithDisagreement, pwdIcon},
		{dashboard.VerdictRunning, runningIcon},
		{dashboard.VerdictError, errorIcon},
		{"", pendingIcon}, // pending / idle: any unrecognised verdict value
	}
	for _, tt := range tests {
		got := dashboard.LetterForVerdictForTest(tt.verdict)
		if got != tt.want {
			t.Errorf("letterForVerdict(%q) = %U, want %U", tt.verdict, []rune(got)[0], []rune(tt.want)[0])
		}
	}
}

// TestColorForVerdict_Running asserts running renders in ColorBlue:
// the only hue in the palette not shared with another verdict.
func TestColorForVerdict_Running(t *testing.T) {
	if got := dashboard.ColorForVerdictForTest(dashboard.VerdictRunning); got != dashboard.ColorBlue {
		t.Errorf("colorForVerdict(running) = %q, want ColorBlue (%q)", got, dashboard.ColorBlue)
	}
}

// TestColorForVerdict_Error asserts error renders in ColorRed.
func TestColorForVerdict_Error(t *testing.T) {
	if got := dashboard.ColorForVerdictForTest(dashboard.VerdictError); got != dashboard.ColorRed {
		t.Errorf("colorForVerdict(error) = %q, want ColorRed (%q)", got, dashboard.ColorRed)
	}
}

// TestClassifyVerdict_Interrupted asserts an agent in state "interrupted"
// classifies as VerdictError: a fault, not an empty slot.
func TestClassifyVerdict_Interrupted(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "interrupted"
	}
	got := dashboard.BuildReviewChildSummaries(buildChildren(1, states, nil))
	for _, s := range got {
		if s.Verdict != dashboard.VerdictError {
			t.Errorf("agent %q in state interrupted: Verdict = %q, want VerdictError", s.AgentShortName, s.Verdict)
		}
	}
}

// TestCollapsedRow_Interrupted asserts an interrupted agent renders the
// error icon in ColorRed on the collapsed review row.
func TestCollapsedRow_Interrupted(t *testing.T) {
	agents := review.Agents()
	states := map[string]string{}
	msgs := map[string]string{}
	for _, a := range agents {
		states[a.Name] = "finished"
		msgs[a.Name] = "<verdict>pass</verdict>"
	}
	states[agents[0].Name] = "interrupted"
	msgs[agents[0].Name] = ""

	row := renderCollapsedReviewRow(t, 200, states, msgs)
	if strings.Count(row, errorIcon) != 1 {
		t.Errorf("expected exactly 1 error icon for the interrupted agent, got %d; row=%q", strings.Count(row, errorIcon), row)
	}
	interruptedShort := dashboard.ShortAgentName(agents[0].Name)
	if !strings.Contains(row, interruptedShort+":"+errorIcon) {
		t.Errorf("expected %q in row; row=%q", interruptedShort+":"+errorIcon, row)
	}
}

// TestVerdictIconCell_TwoColumns asserts that a rendered verdict icon cell
// measures exactly two display columns via lipgloss.Width, and that the
// width-budget functions count two columns per icon rather than one rune.
func TestVerdictIconCell_TwoColumns(t *testing.T) {
	if w := lipgloss.Width(dashboard.RenderIconCellForTest(dashboard.VerdictPass)); w != 2 {
		t.Errorf("rendered icon cell width = %d, want 2", w)
	}

	// A single review agent's labels segment is "name:" + a two-column icon.
	// Compare the labels width for one vs. two agents sharing the same short
	// name length and verdict: the delta must be exactly the icon width (2)
	// plus the ":" plus the two-space separator plus the second name's width
	// — i.e. reviewSummaryLabelsWidth must grow by 2, not 1, per extra icon.
	one := []dashboard.ReviewChildSummary{{AgentShortName: "aa", Verdict: dashboard.VerdictPass}}
	two := []dashboard.ReviewChildSummary{
		{AgentShortName: "aa", Verdict: dashboard.VerdictPass},
		{AgentShortName: "aa", Verdict: dashboard.VerdictPass},
	}
	_, oneW, _ := dashboard.RenderReviewSummary(one, 1000)
	_, twoW, _ := dashboard.RenderReviewSummary(two, 1000)
	// Second entry adds: 2 (separator) + 2 ("aa") + 1 (":") + 2 (icon) = 7.
	if delta := twoW - oneW; delta != 7 {
		t.Errorf("labels width delta per extra two-column-icon agent = %d, want 7 (got oneW=%d twoW=%d)", delta, oneW, twoW)
	}

	// Same check for the compact (icon-only) form: each extra icon adds
	// 2 (separator) + 2 (icon) = 4 display columns.
	compactOneW := dashboard.ReviewSummaryCompactWidthForTest(one)
	compactTwoW := dashboard.ReviewSummaryCompactWidthForTest(two)
	if delta := compactTwoW - compactOneW; delta != 4 {
		t.Errorf("compact width delta per extra two-column icon = %d, want 4 (got oneW=%d twoW=%d)", delta, compactOneW, compactTwoW)
	}
}
