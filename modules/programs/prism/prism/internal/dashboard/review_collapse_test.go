package dashboard_test

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/dashboard"
)

// ── ReviewRoundKey tests ──────────────────────────────────────────────────────

func TestReviewRoundKey(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		// Per-agent sessions → return group key.
		{"nixos-config@feature~review-1-review-goal", "nixos-config@feature~review-1"},
		{"nixos-config@feature~review-1-review-code", "nixos-config@feature~review-1"},
		{"nixos-config@feature~review-1-review-context", "nixos-config@feature~review-1"},
		{"nixos-config@feature~review-1-review-qa", "nixos-config@feature~review-1"},
		{"nixos-config@feature~review-1-review-security", "nixos-config@feature~review-1"},
		{"nixos-config@feature~review-2-review-goal", "nixos-config@feature~review-2"},
		{"nixos-config@feature~review-3-review-code", "nixos-config@feature~review-3"},
		// Depth-2 label "~review-1" with NO agent suffix → not a per-agent session.
		{"nixos-config@feature~review-1", ""},
		// Non-review sessions → empty.
		{"nixos-config@main", ""},
		{"nixos-config@feature", ""},
		{"scratchpad", ""},
		// Old-shape sub-sessions (contain ~) → empty (not per-agent).
		{"nixos-config@feature~review-1~review", ""},
	}
	for _, tt := range tests {
		got := dashboard.ReviewRoundKey(tt.name)
		if got != tt.want {
			t.Errorf("ReviewRoundKey(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// ── EscalatedState tests ──────────────────────────────────────────────────────

func TestEscalatedState(t *testing.T) {
	tests := []struct {
		name   string
		states []string
		want   string
	}{
		// Empty → idle (empty string falls to default).
		{"empty", nil, ""},
		{"all idle", []string{"", ""}, ""},
		// Single states.
		{"single waiting", []string{"waiting"}, "waiting"},
		{"single error", []string{"error"}, "error"},
		{"single active", []string{"active"}, "active"},
		{"single interrupted", []string{"interrupted"}, "interrupted"},
		{"single finished", []string{"finished"}, "finished"},
		// waiting > everything.
		{"waiting beats error", []string{"error", "waiting"}, "waiting"},
		{"waiting beats active", []string{"active", "waiting"}, "waiting"},
		{"waiting beats finished", []string{"finished", "waiting"}, "waiting"},
		{"waiting beats interrupted", []string{"interrupted", "waiting"}, "waiting"},
		// error > active/interrupted/finished.
		{"error beats active", []string{"active", "error"}, "error"},
		{"error beats interrupted", []string{"interrupted", "error"}, "error"},
		{"error beats finished", []string{"finished", "error"}, "error"},
		// active > interrupted/finished.
		{"active beats interrupted", []string{"interrupted", "active"}, "active"},
		{"active beats finished", []string{"finished", "active"}, "active"},
		// interrupted > finished.
		{"interrupted beats finished", []string{"finished", "interrupted"}, "interrupted"},
		// 4 finished + 1 waiting → waiting.
		{"4 finished 1 waiting",
			[]string{"finished", "finished", "finished", "finished", "waiting"},
			"waiting"},
		// 3 active + 2 finished → active.
		{"3 active 2 finished",
			[]string{"active", "finished", "active", "active", "finished"},
			"active"},
		// All 5 finished → finished.
		{"all 5 finished",
			[]string{"finished", "finished", "finished", "finished", "finished"},
			"finished"},
		// Full precedence table.
		{"precedence: waiting first",
			[]string{"finished", "interrupted", "active", "error", "waiting"},
			"waiting"},
		{"precedence: error first when no waiting",
			[]string{"finished", "interrupted", "active", "error"},
			"error"},
		{"precedence: active first when no waiting/error",
			[]string{"finished", "interrupted", "active"},
			"active"},
		{"precedence: interrupted beats finished",
			[]string{"finished", "interrupted"},
			"interrupted"},
	}
	for _, tt := range tests {
		got := dashboard.EscalatedState(tt.states)
		if got != tt.want {
			t.Errorf("EscalatedState(%v) = %q, want %q", tt.states, got, tt.want)
		}
	}
}

// ── BuildDisplayRows tests ────────────────────────────────────────────────────

// makePerAgentSessions constructs a set of per-agent sessions for a given
// worker branch and round number.
func makePerAgentSessions(repo, branch string, round int, states map[string]string) []dashboard.AgentSession {
	agents := []string{"review-code", "review-context", "review-goal", "review-qa", "review-security"}
	var out []dashboard.AgentSession
	for _, ag := range agents {
		name := repo + "@" + branch + "~review-" + itoa(round) + "-" + ag
		state := ""
		if s, ok := states[ag]; ok {
			state = s
		}
		out = append(out, dashboard.AgentSession{Name: name, AgentState: state})
	}
	return out
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return "10" // good enough for tests up to 10 rounds
}

func TestBuildDisplayRows_DefaultCollapsed(t *testing.T) {
	// With no collapsedGroups entries (all absent = collapsed), per-agent sessions
	// should be collapsed into a single virtual group row.
	sessions := makePerAgentSessions("nixos-config", "feature", 1, map[string]string{
		"review-code":     "finished",
		"review-context":  "finished",
		"review-goal":     "finished",
		"review-qa":       "finished",
		"review-security": "finished",
	})
	// Sort first (as RefilterShared would).
	dashboard.SortDisplayed(sessions)

	rows, _ := dashboard.BuildDisplayRows(sessions, nil, "")

	// Should produce exactly 1 row: the virtual group.
	if len(rows) != 1 {
		t.Fatalf("expected 1 row (collapsed group), got %d: %v", len(rows), rowNames(rows))
	}
	if !rows[0].IsReviewGroup {
		t.Errorf("row 0 should be IsReviewGroup=true, got false (Name=%q)", rows[0].Name)
	}
	if rows[0].Name != "nixos-config@feature~review-1" {
		t.Errorf("group row Name = %q, want %q", rows[0].Name, "nixos-config@feature~review-1")
	}
	// All children finished → escalated = finished.
	if rows[0].AgentState != "finished" {
		t.Errorf("group row AgentState = %q, want %q", rows[0].AgentState, "finished")
	}
}

func TestBuildDisplayRows_ExpandedShowsChildren(t *testing.T) {
	sessions := makePerAgentSessions("nixos-config", "feature", 1, map[string]string{
		"review-goal":     "waiting",
		"review-code":     "finished",
		"review-context":  "finished",
		"review-qa":       "finished",
		"review-security": "finished",
	})
	dashboard.SortDisplayed(sessions)

	// Mark the group as expanded.
	collapsed := map[string]bool{
		"nixos-config@feature~review-1": true, // true = expanded
	}

	rows, _ := dashboard.BuildDisplayRows(sessions, collapsed, "")

	// Should produce 1 group row + 5 children = 6 rows.
	if len(rows) != 6 {
		t.Fatalf("expected 6 rows (1 group + 5 children), got %d: %v", len(rows), rowNames(rows))
	}
	if !rows[0].IsReviewGroup {
		t.Errorf("row 0 should be IsReviewGroup=true")
	}
	// The group row shows waiting (highest priority child).
	if rows[0].AgentState != "waiting" {
		t.Errorf("group row AgentState = %q, want waiting", rows[0].AgentState)
	}
	// Children should not be group rows.
	for i := 1; i <= 5; i++ {
		if rows[i].IsReviewGroup {
			t.Errorf("row %d should not be a review group (Name=%q)", i, rows[i].Name)
		}
	}
}

func TestBuildDisplayRows_EnterToggleCollapseExpand(t *testing.T) {
	sessions := makePerAgentSessions("nixos-config", "feature", 1, map[string]string{
		"review-goal": "finished",
	})
	dashboard.SortDisplayed(sessions)

	// Start collapsed (no entry in map).
	collapsed := map[string]bool{}
	rows, _ := dashboard.BuildDisplayRows(sessions, collapsed, "")
	if len(rows) != 1 {
		t.Fatalf("collapsed: expected 1 row, got %d", len(rows))
	}

	// Expand (toggle: set true in map).
	collapsed["nixos-config@feature~review-1"] = true
	rows, _ = dashboard.BuildDisplayRows(sessions, collapsed, "")
	if len(rows) != 1+len(sessions) {
		t.Fatalf("expanded: expected %d rows, got %d", 1+len(sessions), len(rows))
	}

	// Collapse again (toggle: set false in map).
	collapsed["nixos-config@feature~review-1"] = false
	rows, _ = dashboard.BuildDisplayRows(sessions, collapsed, "")
	if len(rows) != 1 {
		t.Fatalf("re-collapsed: expected 1 row, got %d", len(rows))
	}
}

func TestBuildDisplayRows_StateEscalation(t *testing.T) {
	tests := []struct {
		name      string
		states    map[string]string
		wantState string
	}{
		{
			"waiting visible in group",
			map[string]string{
				"review-goal": "waiting", "review-code": "finished",
				"review-context": "finished", "review-qa": "finished", "review-security": "finished",
			},
			"waiting",
		},
		{
			"error when no waiting",
			map[string]string{
				"review-goal": "error", "review-code": "finished",
				"review-context": "finished", "review-qa": "finished", "review-security": "finished",
			},
			"error",
		},
		{
			"active when no waiting/error",
			map[string]string{
				"review-goal": "active", "review-code": "finished",
				"review-context": "finished", "review-qa": "finished", "review-security": "finished",
			},
			"active",
		},
		{
			"interrupted beats finished",
			map[string]string{
				"review-goal": "interrupted", "review-code": "finished",
				"review-context": "finished", "review-qa": "finished", "review-security": "finished",
			},
			"interrupted",
		},
		{
			"all finished → finished",
			map[string]string{
				"review-goal": "finished", "review-code": "finished",
				"review-context": "finished", "review-qa": "finished", "review-security": "finished",
			},
			"finished",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions := makePerAgentSessions("repo", "branch", 1, tt.states)
			dashboard.SortDisplayed(sessions)
			rows, _ := dashboard.BuildDisplayRows(sessions, nil, "")
			if len(rows) == 0 {
				t.Fatal("expected at least 1 row")
			}
			if !rows[0].IsReviewGroup {
				t.Fatal("row 0 should be a review group")
			}
			if rows[0].AgentState != tt.wantState {
				t.Errorf("AgentState = %q, want %q", rows[0].AgentState, tt.wantState)
			}
		})
	}
}

func TestBuildDisplayRows_PartialRound(t *testing.T) {
	// Only 2 agents spawned (partial round after --only).
	sessions := []dashboard.AgentSession{
		{Name: "nixos-config@feature~review-1-review-code", AgentState: "finished"},
		{Name: "nixos-config@feature~review-1-review-qa", AgentState: "waiting"},
	}
	dashboard.SortDisplayed(sessions)

	rows, _ := dashboard.BuildDisplayRows(sessions, nil, "")
	if len(rows) != 1 {
		t.Fatalf("partial round collapsed: expected 1 row, got %d", len(rows))
	}
	if !rows[0].IsReviewGroup {
		t.Error("row 0 should be review group")
	}
	// waiting beats finished.
	if rows[0].AgentState != "waiting" {
		t.Errorf("AgentState = %q, want waiting", rows[0].AgentState)
	}

	// Expand it.
	collapsed := map[string]bool{"nixos-config@feature~review-1": true}
	rows, _ = dashboard.BuildDisplayRows(sessions, collapsed, "")
	if len(rows) != 3 { // 1 group + 2 children
		t.Fatalf("partial round expanded: expected 3 rows, got %d", len(rows))
	}
}

func TestBuildDisplayRows_MultiRoundIndependentCollapse(t *testing.T) {
	// Two rounds: ~review-1 (finished) and ~review-2 (active).
	var sessions []dashboard.AgentSession
	for _, ag := range []string{"review-code", "review-context", "review-goal", "review-qa", "review-security"} {
		sessions = append(sessions, dashboard.AgentSession{
			Name: "repo@branch~review-1-" + ag, AgentState: "finished",
		})
	}
	for _, ag := range []string{"review-code", "review-context", "review-goal", "review-qa", "review-security"} {
		sessions = append(sessions, dashboard.AgentSession{
			Name: "repo@branch~review-2-" + ag, AgentState: "active",
		})
	}
	dashboard.SortDisplayed(sessions)

	// Both collapsed by default.
	rows, _ := dashboard.BuildDisplayRows(sessions, nil, "")
	if len(rows) != 2 {
		t.Fatalf("both collapsed: expected 2 group rows, got %d: %v", len(rows), rowNames(rows))
	}
	if rows[0].AgentState != "finished" {
		t.Errorf("~review-1 state = %q, want finished", rows[0].AgentState)
	}
	if rows[1].AgentState != "active" {
		t.Errorf("~review-2 state = %q, want active", rows[1].AgentState)
	}

	// Expand only ~review-1.
	collapsed := map[string]bool{"repo@branch~review-1": true}
	rows, _ = dashboard.BuildDisplayRows(sessions, collapsed, "")
	// Should be: group-1, 5 children, group-2 = 7 rows.
	if len(rows) != 7 {
		t.Fatalf("expand ~review-1: expected 7 rows, got %d: %v", len(rows), rowNames(rows))
	}
	if !rows[0].IsReviewGroup || rows[0].Name != "repo@branch~review-1" {
		t.Errorf("row 0 should be ~review-1 group")
	}
	if rows[6].IsReviewGroup && rows[6].Name != "repo@branch~review-2" {
		t.Errorf("row 6 should be ~review-2 group, got %q", rows[6].Name)
	}
	// ~review-2 stays collapsed.
	if rows[6].IsReviewGroup == false {
		t.Error("row 6 should still be review group (collapsed)")
	}

	// Expand only ~review-2 (collapse ~review-1 again).
	collapsed2 := map[string]bool{"repo@branch~review-2": true}
	rows2, _ := dashboard.BuildDisplayRows(sessions, collapsed2, "")
	// Should be: group-1, group-2, 5 children = 7 rows.
	if len(rows2) != 7 {
		t.Fatalf("expand ~review-2: expected 7 rows, got %d: %v", len(rows2), rowNames(rows2))
	}
	if rows2[0].Name != "repo@branch~review-1" {
		t.Errorf("row 0 should be ~review-1, got %q", rows2[0].Name)
	}
	if rows2[1].Name != "repo@branch~review-2" {
		t.Errorf("row 1 should be ~review-2, got %q", rows2[1].Name)
	}
}

func TestBuildDisplayRows_FilterAutoExpand(t *testing.T) {
	sessions := makePerAgentSessions("nixos-config", "feature", 1, map[string]string{
		"review-goal": "waiting",
	})
	dashboard.SortDisplayed(sessions)

	// Filter matches a child session name; group should auto-expand.
	// The filter "review-goal" matches "nixos-config@feature~review-1-review-goal".
	rows, autoExpanded := dashboard.BuildDisplayRows(sessions, nil, "review-goal")
	_ = rows // auto-expand is reported in autoExpanded even if filter hides non-matching

	if !autoExpanded["nixos-config@feature~review-1"] {
		t.Errorf("expected group nixos-config@feature~review-1 to be in autoExpanded; got %v", autoExpanded)
	}
}

func TestBuildDisplayRows_FilterAutoExpandPersists(t *testing.T) {
	// Simulate the RefilterShared flow: auto-expand a group via filter, then
	// clear the filter. The group should remain expanded (not re-collapsed).
	sessions := makePerAgentSessions("nixos-config", "feature", 1, map[string]string{
		"review-goal": "waiting",
	})
	dashboard.SortDisplayed(sessions)

	d := dashboard.Shared{
		Sessions:        sessions,
		FilterActive:    true,
		FilterText:      "review-goal",
		CollapsedGroups: map[string]bool{},
	}
	d = dashboard.RefilterShared(d)

	// After filtering, CollapsedGroups should have the group marked as expanded.
	if !d.CollapsedGroups["nixos-config@feature~review-1"] {
		t.Errorf("after filter, group should be auto-expanded in CollapsedGroups; got %v", d.CollapsedGroups)
	}

	// Now clear the filter.
	d.FilterActive = false
	d.FilterText = ""
	d = dashboard.RefilterShared(d)

	// The group should still be expanded (not re-collapsed) because CollapsedGroups persists.
	if !d.CollapsedGroups["nixos-config@feature~review-1"] {
		t.Errorf("after clearing filter, group should remain expanded; got %v", d.CollapsedGroups)
	}
	// And the display should show group + children.
	hasGroup := false
	hasChildren := false
	for _, r := range d.Displayed {
		if r.IsReviewGroup {
			hasGroup = true
		} else if dashboard.ReviewRoundKey(r.Name) != "" {
			hasChildren = true
		}
	}
	if !hasGroup {
		t.Error("after clearing filter, display should include the group row")
	}
	if !hasChildren {
		t.Error("after clearing filter, display should include expanded children")
	}
}

// ── ToggleReviewGroup tests ───────────────────────────────────────────────────

func TestToggleReviewGroup(t *testing.T) {
	sessions := makePerAgentSessions("repo", "branch", 1, nil)
	dashboard.SortDisplayed(sessions)

	d := dashboard.Shared{
		Sessions:        sessions,
		CollapsedGroups: map[string]bool{},
	}
	d = dashboard.RefilterShared(d)

	// Initially collapsed: 1 group row.
	if len(d.Displayed) != 1 {
		t.Fatalf("initially collapsed: expected 1 row, got %d", len(d.Displayed))
	}

	// Toggle to expand.
	d = dashboard.ToggleReviewGroup(d, "repo@branch~review-1")
	if len(d.Displayed) != 1+len(sessions) {
		t.Fatalf("after expand: expected %d rows, got %d", 1+len(sessions), len(d.Displayed))
	}

	// Toggle to collapse.
	d = dashboard.ToggleReviewGroup(d, "repo@branch~review-1")
	if len(d.Displayed) != 1 {
		t.Fatalf("after re-collapse: expected 1 row, got %d", len(d.Displayed))
	}
}

// ── SessionColumnWidth with review group rows ─────────────────────────────────

func TestSessionColumnWidth_ReviewGroupRow(t *testing.T) {
	// A virtual group row "nixos-config@feature~review-1" has label "~review-1" (9 chars).
	// needed = d1PrefixLen(6) + indicatorW(2) + len("~review-1")(9) - treePrefixW(10) = 7.
	groupRow := dashboard.AgentSession{
		Name:          "nixos-config@feature~review-1",
		IsReviewGroup: true,
		AgentState:    "finished",
	}
	got := dashboard.SessionColumnWidth([]dashboard.AgentSession{groupRow})
	// 6 + 2 + 9 - 10 = 7 → at minimum.
	if got < 7 {
		t.Errorf("SessionColumnWidth with review group row = %d, want >= 7", got)
	}
	// Verify it's exactly the minimum (7) since 7 == sessionWMin.
	if got != 7 {
		t.Errorf("SessionColumnWidth = %d, want 7", got)
	}
}

func TestSessionColumnWidth_ReviewGroupRow_LongLabel(t *testing.T) {
	// A group row for round 10 has label "~review-10" (10 chars).
	// needed = 6 + 2 + 10 - 10 = 8.
	groupRow := dashboard.AgentSession{
		Name:          "nixos-config@feature~review-10",
		IsReviewGroup: true,
	}
	got := dashboard.SessionColumnWidth([]dashboard.AgentSession{groupRow})
	if got != 8 {
		t.Errorf("SessionColumnWidth = %d, want 8", got)
	}
}

// ── DashView integration test with review groups ──────────────────────────────

func TestDashView_ReviewGroupsCollapsedByDefault(t *testing.T) {
	// Build a session list with a worker and 5 review agents.
	// The worker is the parent; the 5 agents are per-agent sessions.
	sessions := []dashboard.AgentSession{
		{Name: "nixos-config@main", AgentState: "active", AgentName: "coordinator"},
		{Name: "nixos-config@feature", AgentState: "finished", AgentName: "worker"},
		{Name: "nixos-config@feature~review-1-review-code", AgentState: "finished"},
		{Name: "nixos-config@feature~review-1-review-context", AgentState: "finished"},
		{Name: "nixos-config@feature~review-1-review-goal", AgentState: "waiting"},
		{Name: "nixos-config@feature~review-1-review-qa", AgentState: "finished"},
		{Name: "nixos-config@feature~review-1-review-security", AgentState: "finished"},
	}

	d := dashboard.Shared{
		Width:    120,
		Sessions: sessions,
	}
	d = dashboard.RefilterShared(d)

	output := dashboard.DashView(d, "", false)
	plain := stripANSI(output)

	// The collapsed group row should appear.
	if !strings.Contains(plain, "~review-1") {
		t.Errorf("expected ~review-1 group row in output, got:\n%s", plain)
	}
	// Individual per-agent rows should NOT appear (collapsed).
	if strings.Contains(plain, "review-goal") {
		t.Errorf("per-agent review-goal row should be hidden (collapsed), got:\n%s", plain)
	}
	if strings.Contains(plain, "review-code") {
		t.Errorf("per-agent review-code row should be hidden (collapsed), got:\n%s", plain)
	}
	// The state column should show "waiting" (escalated from review-goal).
	if !strings.Contains(plain, "waiting") {
		t.Errorf("expected 'waiting' state on group row, got:\n%s", plain)
	}
}

func TestDashView_ReviewGroupExpanded(t *testing.T) {
	sessions := []dashboard.AgentSession{
		{Name: "nixos-config@feature", AgentState: "finished", AgentName: "worker"},
		{Name: "nixos-config@feature~review-1-review-code", AgentState: "finished"},
		{Name: "nixos-config@feature~review-1-review-context", AgentState: "finished"},
		{Name: "nixos-config@feature~review-1-review-goal", AgentState: "finished"},
		{Name: "nixos-config@feature~review-1-review-qa", AgentState: "finished"},
		{Name: "nixos-config@feature~review-1-review-security", AgentState: "finished"},
	}

	d := dashboard.Shared{
		Width:           120,
		Sessions:        sessions,
		CollapsedGroups: map[string]bool{"nixos-config@feature~review-1": true},
	}
	d = dashboard.RefilterShared(d)

	output := dashboard.DashView(d, "", false)
	plain := stripANSI(output)

	// Expanded: per-agent rows should be visible.
	if !strings.Contains(plain, "review-goal") {
		t.Errorf("expected review-goal row visible when expanded, got:\n%s", plain)
	}
	if !strings.Contains(plain, "review-code") {
		t.Errorf("expected review-code row visible when expanded, got:\n%s", plain)
	}
}

func TestDashView_CollapseStateDoesNotPersistAcrossInvocations(t *testing.T) {
	// Each time a new Shared is constructed (fresh open), CollapsedGroups starts
	// empty → all groups collapsed.
	sessions := []dashboard.AgentSession{
		{Name: "nixos-config@feature", AgentState: "finished"},
		{Name: "nixos-config@feature~review-1-review-goal", AgentState: "finished"},
		{Name: "nixos-config@feature~review-1-review-code", AgentState: "finished"},
	}

	// First invocation (fresh Shared, no CollapsedGroups).
	d := dashboard.Shared{
		Width:    120,
		Sessions: sessions,
	}
	d = dashboard.RefilterShared(d)

	// Expand the group.
	d = dashboard.ToggleReviewGroup(d, "nixos-config@feature~review-1")
	if len(d.Displayed) <= 2 {
		t.Fatalf("after expand: expected > 2 rows, got %d", len(d.Displayed))
	}

	// Simulate a new invocation (fresh Shared).
	d2 := dashboard.Shared{
		Width:    120,
		Sessions: sessions,
	}
	d2 = dashboard.RefilterShared(d2)

	// Should be collapsed again (only 2 rows: worker + group).
	groupCount := 0
	for _, r := range d2.Displayed {
		if r.IsReviewGroup {
			groupCount++
		}
	}
	if groupCount != 1 {
		t.Errorf("fresh invocation: expected 1 group row collapsed, got %d rows with %d groups", len(d2.Displayed), groupCount)
	}
	// No per-agent children visible.
	for _, r := range d2.Displayed {
		if !r.IsReviewGroup && dashboard.ReviewRoundKey(r.Name) != "" {
			t.Errorf("fresh invocation: per-agent child %q should be hidden", r.Name)
		}
	}
}

// ── helper ────────────────────────────────────────────────────────────────────

func rowNames(rows []dashboard.AgentSession) []string {
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Name
		if r.IsReviewGroup {
			names[i] += "(group)"
		}
	}
	return names
}
