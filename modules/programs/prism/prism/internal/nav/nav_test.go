package nav_test

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/dashboard"
	"github.com/prismatic-koi/prism/internal/nav"
)

// makeSessions builds a sorted slice of AgentSession values from a list of
// (name, state) pairs. The slice is sorted with dashboard.SortDisplayed so
// the navigable spine order matches what the dashboard renders.
func makeSessions(t *testing.T, entries ...[2]string) []dashboard.AgentSession {
	t.Helper()
	ss := make([]dashboard.AgentSession, 0, len(entries))
	for _, e := range entries {
		ss = append(ss, dashboard.AgentSession{Name: e[0], AgentState: e[1]})
	}
	dashboard.SortDisplayed(ss)
	return ss
}

// alwaysLive is a liveCheck that returns true for every session name. Used in
// the pure tests where we want to focus on ordering and state, not liveness.
func alwaysLive(string) bool { return true }

// neverLive is a liveCheck that returns false for every session name.
func neverLive(string) bool { return false }

// liveSet returns a liveCheck that reports true only for the given names.
func liveSet(names ...string) func(string) bool {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(name string) bool { return set[name] }
}

func TestParseDirection(t *testing.T) {
	for _, ok := range []string{"up", "down", "left", "right"} {
		if _, err := nav.ParseDirection(ok); err != nil {
			t.Errorf("ParseDirection(%q) returned err: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "north", "Up", "UP", "u", "forward"} {
		if _, err := nav.ParseDirection(bad); err == nil {
			t.Errorf("ParseDirection(%q): expected error, got nil", bad)
		}
	}
}

func TestIsSpineRow(t *testing.T) {
	cases := []struct {
		name  string
		s     dashboard.AgentSession
		want  bool
	}{
		// Top-level rows: included.
		{"plain", dashboard.AgentSession{Name: "scratchpad"}, true},
		{"main", dashboard.AgentSession{Name: "nixos-config@main"}, true},
		// Depth-1 child branches: now included (issue #1800).
		{"branch", dashboard.AgentSession{Name: "nixos-config@feature"}, true},
		{"dashboard-slim", dashboard.AgentSession{Name: "nixos-config@dashboard-slim"}, true},
		// Depth-2 review-agent children: excluded.
		{"depth2", dashboard.AgentSession{Name: "nixos-config@feature~review-1-review-goal"}, false},
		// Virtual review-group rows: excluded.
		{"review-group", dashboard.AgentSession{Name: "nixos-config@feature~review-1", IsReviewGroup: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nav.IsSpineRow(c.s); got != c.want {
				t.Errorf("IsSpineRow(%q) = %v, want %v", c.s.Name, got, c.want)
			}
		})
	}
}

// TestVerticalTargets_FiltersNonLive verifies that liveness is the only
// runtime filter applied to the spine — sessions in any agent state
// (including "finished", which means the turn is over but the session is
// alive) are included as long as their tmux session is live. Depth-2 and
// review-group rows are still excluded by IsSpineRow. See issue #1839.
func TestVerticalTargets_FiltersNonLive(t *testing.T) {
	sessions := makeSessions(t,
		[2]string{"alpha@main", "active"},
		[2]string{"alpha@feature", "active"},                       // depth-1: included (issue #1800)
		[2]string{"beta@main", "finished"},                         // finished but live: included (issue #1839)
		[2]string{"gamma@main", "idle"},
		[2]string{"gamma@feature~review-1-review-goal", "active"},  // depth-2: excluded
		[2]string{"delta@main", "active"},                          // not live: excluded
		[2]string{"scratchpad", "idle"},
	)
	live := liveSet("alpha@main", "alpha@feature", "beta@main", "gamma@main", "gamma@feature~review-1-review-goal", "scratchpad")

	got := nav.VerticalTargets(sessions, live)
	want := []string{"alpha@main", "alpha@feature", "beta@main", "gamma@main", "scratchpad"}
	if !equalSlice(got, want) {
		t.Errorf("VerticalTargets = %v, want %v", got, want)
	}
}

// TestVerticalTargets_Depth1Included_BasicRegression covers the core
// regression from issue #1800: a fixture with `nixos-config@main` and a
// depth-1 child `nixos-config@dashboard-slim` (no review groups) must yield
// both names in dashboard order, and the cycle must connect them.
func TestVerticalTargets_Depth1Included_BasicRegression(t *testing.T) {
	sessions := makeSessions(t,
		[2]string{"nixos-config@main", "active"},
		[2]string{"nixos-config@dashboard-slim", "active"},
	)
	got := nav.VerticalTargets(sessions, alwaysLive)
	want := []string{"nixos-config@main", "nixos-config@dashboard-slim"}
	if !equalSlice(got, want) {
		t.Fatalf("VerticalTargets = %v, want %v", got, want)
	}
	if next, ok := nav.ResolveVertical("nixos-config@main", nav.DirDown, got); !ok || next != "nixos-config@dashboard-slim" {
		t.Errorf("down from @main: got (%q,%v), want (nixos-config@dashboard-slim,true)", next, ok)
	}
	if next, ok := nav.ResolveVertical("nixos-config@dashboard-slim", nav.DirDown, got); !ok || next != "nixos-config@main" {
		t.Errorf("down from @dashboard-slim (wrap): got (%q,%v), want (nixos-config@main,true)", next, ok)
	}
	if next, ok := nav.ResolveVertical("nixos-config@main", nav.DirUp, got); !ok || next != "nixos-config@dashboard-slim" {
		t.Errorf("up from @main (wrap): got (%q,%v), want (nixos-config@dashboard-slim,true)", next, ok)
	}
	if next, ok := nav.ResolveVertical("nixos-config@dashboard-slim", nav.DirUp, got); !ok || next != "nixos-config@main" {
		t.Errorf("up from @dashboard-slim: got (%q,%v), want (nixos-config@main,true)", next, ok)
	}
}

// TestVerticalTargets_DashboardOrdering verifies that the spine is ordered
// per dashboard.SortDisplayed: within each repo `@main` first then other
// branches alphabetically; repos ordered alphabetically.
func TestVerticalTargets_DashboardOrdering(t *testing.T) {
	sessions := makeSessions(t,
		[2]string{"other-repo@main", "active"},
		[2]string{"nixos-config@feat-a", "active"},
		[2]string{"nixos-config@main", "active"},
	)
	got := nav.VerticalTargets(sessions, alwaysLive)
	want := []string{"nixos-config@main", "nixos-config@feat-a", "other-repo@main"}
	if !equalSlice(got, want) {
		t.Errorf("VerticalTargets = %v, want %v", got, want)
	}
}

// TestVerticalTargets_ExcludesReviewGroupAndDepth2 verifies that review-group
// virtual rows and depth-2 review-agent children are not part of the up/down
// spine even though they appear in dashboard rendering.
func TestVerticalTargets_ExcludesReviewGroupAndDepth2(t *testing.T) {
	parent := "repo@feature"
	rgName := parent + "~review-1"
	d2Name := parent + "~review-1-review-goal"
	sessions := makeSessions(t,
		[2]string{"repo@main", "active"},
		[2]string{parent, "active"},
		[2]string{d2Name, "active"},
	)
	// Inject a virtual review-group row (these are synthesised by the
	// dashboard layer, not present in the DB; build it directly here).
	sessions = append(sessions, dashboard.AgentSession{Name: rgName, IsReviewGroup: true, AgentState: "active"})
	dashboard.SortDisplayed(sessions)

	got := nav.VerticalTargets(sessions, alwaysLive)
	want := []string{"repo@main", parent}
	if !equalSlice(got, want) {
		t.Errorf("VerticalTargets = %v, want %v", got, want)
	}
}

// TestVerticalTargets_Depth1OnlyRepo verifies the edge case where a repo has
// only depth-1 children (no `@main` sibling). Those children must still be
// rendered in the spine.
func TestVerticalTargets_Depth1OnlyRepo(t *testing.T) {
	sessions := makeSessions(t,
		[2]string{"alpha@main", "active"},
		[2]string{"orphan@feature-x", "active"},
		[2]string{"orphan@feature-y", "active"},
	)
	got := nav.VerticalTargets(sessions, alwaysLive)
	// Expect alpha@main, then orphan repo's depth-1 children in alpha order.
	want := []string{"alpha@main", "orphan@feature-x", "orphan@feature-y"}
	if !equalSlice(got, want) {
		t.Errorf("VerticalTargets = %v, want %v", got, want)
	}
	// Confirm they cycle.
	if next, ok := nav.ResolveVertical("orphan@feature-x", nav.DirDown, got); !ok || next != "orphan@feature-y" {
		t.Errorf("down from orphan@feature-x: got (%q,%v), want (orphan@feature-y,true)", next, ok)
	}
}

// TestVerticalTargets_MainOnlyRepo verifies the edge case where a repo has
// only `@main` and no depth-1 children — unchanged from previous behaviour.
func TestVerticalTargets_MainOnlyRepo(t *testing.T) {
	sessions := makeSessions(t,
		[2]string{"alpha@main", "active"},
		[2]string{"beta@main", "active"},
	)
	got := nav.VerticalTargets(sessions, alwaysLive)
	want := []string{"alpha@main", "beta@main"}
	if !equalSlice(got, want) {
		t.Errorf("VerticalTargets = %v, want %v", got, want)
	}
}

// TestVerticalTargets_DepthOneStateAgnostic verifies that depth-1 children
// are included in the spine regardless of their AgentState, as long as
// their tmux session is live. Previously these were excluded for terminal
// states; per issue #1839 that filter was wrong ("finished" means turn
// complete, not session ended).
func TestVerticalTargets_DepthOneStateAgnostic(t *testing.T) {
	for _, state := range []string{"finished", "deleted", "interrupted", "idle", "active"} {
		sessions := makeSessions(t,
			[2]string{"repo@main", "active"},
			[2]string{"repo@feature", state},
		)
		got := nav.VerticalTargets(sessions, alwaysLive)
		want := []string{"repo@main", "repo@feature"}
		if !equalSlice(got, want) {
			t.Errorf("depth-1 state=%q: got %v, want %v", state, got, want)
		}
	}
}

// TestVerticalTargets_TopLevelStateAgnostic mirrors the depth-1 variant:
// top-level rows in any agent state are included as long as they are live.
// Issue #1839.
func TestVerticalTargets_TopLevelStateAgnostic(t *testing.T) {
	for _, state := range []string{"finished", "deleted", "interrupted", "idle", "active"} {
		sessions := makeSessions(t,
			[2]string{"alpha@main", "active"},
			[2]string{"beta@main", state},
		)
		got := nav.VerticalTargets(sessions, alwaysLive)
		want := []string{"alpha@main", "beta@main"}
		if !equalSlice(got, want) {
			t.Errorf("top-level state=%q: got %v, want %v", state, got, want)
		}
	}
}

// TestVerticalTargets_FinishedAndActive_BothNavigable is the positive
// regression for issue #1839. Two sessions, one `active`, one `finished`,
// both live: both must be in the spine and C-j-style ResolveVertical must
// switch between them in both directions (and wrap).
func TestVerticalTargets_FinishedAndActive_BothNavigable(t *testing.T) {
	sessions := makeSessions(t,
		[2]string{"alpha@main", "active"},
		[2]string{"beta@main", "finished"},
	)
	targets := nav.VerticalTargets(sessions, alwaysLive)
	want := []string{"alpha@main", "beta@main"}
	if !equalSlice(targets, want) {
		t.Fatalf("VerticalTargets = %v, want %v", targets, want)
	}
	// Down from alpha lands on beta.
	if next, ok := nav.ResolveVertical("alpha@main", nav.DirDown, targets); !ok || next != "beta@main" {
		t.Errorf("down from alpha: got (%q,%v), want (beta@main,true)", next, ok)
	}
	// Down from beta wraps to alpha.
	if next, ok := nav.ResolveVertical("beta@main", nav.DirDown, targets); !ok || next != "alpha@main" {
		t.Errorf("down from beta (wrap): got (%q,%v), want (alpha@main,true)", next, ok)
	}
	// Up from alpha wraps to beta.
	if next, ok := nav.ResolveVertical("alpha@main", nav.DirUp, targets); !ok || next != "beta@main" {
		t.Errorf("up from alpha (wrap): got (%q,%v), want (beta@main,true)", next, ok)
	}
	// Up from beta lands on alpha.
	if next, ok := nav.ResolveVertical("beta@main", nav.DirUp, targets); !ok || next != "alpha@main" {
		t.Errorf("up from beta: got (%q,%v), want (alpha@main,true)", next, ok)
	}
}

func TestResolveVertical_BasicAndWrap(t *testing.T) {
	targets := []string{"alpha@main", "beta@main", "gamma@main"}
	cases := []struct {
		current string
		dir     nav.Direction
		want    string
		ok      bool
	}{
		{"alpha@main", nav.DirDown, "beta@main", true},
		{"beta@main", nav.DirDown, "gamma@main", true},
		{"gamma@main", nav.DirDown, "alpha@main", true}, // wrap forward
		{"alpha@main", nav.DirUp, "gamma@main", true},   // wrap back
		{"beta@main", nav.DirUp, "alpha@main", true},
		{"gamma@main", nav.DirUp, "beta@main", true},
	}
	for _, c := range cases {
		got, ok := nav.ResolveVertical(c.current, c.dir, targets)
		if ok != c.ok || got != c.want {
			t.Errorf("ResolveVertical(%q,%q) = (%q,%v), want (%q,%v)", c.current, c.dir, got, ok, c.want, c.ok)
		}
	}
}

func TestResolveVertical_NoOpCases(t *testing.T) {
	// Zero entries.
	if _, ok := nav.ResolveVertical("alpha@main", nav.DirDown, nil); ok {
		t.Errorf("empty targets: expected no-op")
	}
	// Single entry — should be a no-op even when current matches.
	if _, ok := nav.ResolveVertical("alpha@main", nav.DirDown, []string{"alpha@main"}); ok {
		t.Errorf("single target: expected no-op")
	}
	// Current not in the list.
	if _, ok := nav.ResolveVertical("missing@main", nav.DirDown, []string{"alpha@main", "beta@main"}); ok {
		t.Errorf("current not in list: expected no-op")
	}
	// Non-vertical direction passed.
	if _, ok := nav.ResolveVertical("alpha@main", nav.DirLeft, []string{"alpha@main", "beta@main"}); ok {
		t.Errorf("non-vertical direction: expected no-op")
	}
}

// TestResolveReviewContext_FromDepth2Agent: current is a depth-2 review-agent
// session; the cycle should be anchored on its parent and round irrespective
// of what other sessions exist.
func TestResolveReviewContext_FromDepth2Agent(t *testing.T) {
	current := "repo@main~review-2-review-code"
	sessions := makeSessions(t,
		[2]string{"repo@main", "active"},
		[2]string{current, "active"},
	)
	cyc, ok := nav.ResolveReviewContext(current, sessions, alwaysLive)
	if !ok {
		t.Fatalf("expected review context, got false")
	}
	if cyc.Parent != "repo@main" {
		t.Errorf("parent = %q, want %q", cyc.Parent, "repo@main")
	}
	if cyc.Round != 2 {
		t.Errorf("round = %d, want 2", cyc.Round)
	}
	canon := nav.CanonicalAgentNames()
	if len(cyc.AgentNames) != len(canon) {
		t.Errorf("AgentNames length = %d, want %d", len(cyc.AgentNames), len(canon))
	}
}

// TestResolveReviewContext_FromParentLowestRound: current is the parent of
// two active review rounds; the cycle must anchor on the lowest-numbered one.
func TestResolveReviewContext_FromParentLowestRound(t *testing.T) {
	parent := "repo@main"
	sessions := makeSessions(t,
		[2]string{parent, "active"},
		[2]string{parent + "~review-2-review-goal", "active"},
		[2]string{parent + "~review-3-review-goal", "active"},
	)
	cyc, ok := nav.ResolveReviewContext(parent, sessions, alwaysLive)
	if !ok {
		t.Fatalf("expected review context, got false")
	}
	if cyc.Round != 2 {
		t.Errorf("round = %d, want 2 (lowest active)", cyc.Round)
	}
	if cyc.Parent != parent {
		t.Errorf("parent = %q, want %q", cyc.Parent, parent)
	}
}

// TestResolveReviewContext_DeadRoundsSkipped: round 1 exists but has no live
// children; round 2 is live. Anchor must skip to 2.
func TestResolveReviewContext_DeadRoundsSkipped(t *testing.T) {
	parent := "repo@main"
	r1Child := parent + "~review-1-review-goal"
	r2Child := parent + "~review-2-review-goal"
	sessions := makeSessions(t,
		[2]string{parent, "active"},
		[2]string{r1Child, "finished"},
		[2]string{r2Child, "active"},
	)
	cyc, ok := nav.ResolveReviewContext(parent, sessions, liveSet(parent, r2Child))
	if !ok {
		t.Fatalf("expected review context, got false")
	}
	if cyc.Round != 2 {
		t.Errorf("round = %d, want 2 (round 1 has no live child)", cyc.Round)
	}
}

// TestResolveReviewContext_NoneWhenStandaloneTopLevel: current is a top-level
// session with no review children; left/right must be no-ops.
func TestResolveReviewContext_NoneWhenStandaloneTopLevel(t *testing.T) {
	sessions := makeSessions(t,
		[2]string{"repo@main", "active"},
		[2]string{"other@main", "active"},
	)
	if _, ok := nav.ResolveReviewContext("repo@main", sessions, alwaysLive); ok {
		t.Errorf("expected no review context, got one")
	}
}

// TestResolveReviewContext_NoneWhenDepth1Branch: current is a depth-1 branch
// session (not a review agent); left/right must be no-ops.
func TestResolveReviewContext_NoneWhenDepth1Branch(t *testing.T) {
	sessions := makeSessions(t,
		[2]string{"repo@main", "active"},
		[2]string{"repo@feature", "active"},
	)
	if _, ok := nav.ResolveReviewContext("repo@feature", sessions, alwaysLive); ok {
		t.Errorf("expected no review context for plain depth-1 branch")
	}
}

func TestResolveLateral_FullCycleWrap(t *testing.T) {
	parent := "repo@main"
	cyc := nav.ReviewCycle{
		Parent:     parent,
		AgentNames: nav.CanonicalAgentNames(),
		Round:      1,
	}
	expected := append([]string{parent}, makeChildNames(parent, 1, cyc.AgentNames)...)

	// right walks forward through the whole cycle and wraps back to parent.
	cur := parent
	for i := 0; i < len(expected); i++ {
		next, ok := nav.ResolveLateral(cur, nav.DirRight, cyc, alwaysLive)
		if !ok {
			t.Fatalf("right step %d: ok=false from %q", i, cur)
		}
		want := expected[(i+1)%len(expected)]
		if next != want {
			t.Errorf("right step %d: got %q, want %q", i, next, want)
		}
		cur = next
	}
	// left walks backward through the whole cycle.
	cur = parent
	for i := 0; i < len(expected); i++ {
		next, ok := nav.ResolveLateral(cur, nav.DirLeft, cyc, alwaysLive)
		if !ok {
			t.Fatalf("left step %d: ok=false from %q", i, cur)
		}
		// After step i (0-indexed) starting from index 0 going left, we are
		// at index (-(i+1) mod len). Add len before mod to keep it positive.
		want := expected[((-1-i)%len(expected)+len(expected))%len(expected)]
		if next != want {
			t.Errorf("left step %d: got %q, want %q", i, next, want)
		}
		cur = next
	}
}

func TestResolveLateral_RightFromParentIsReviewGoal(t *testing.T) {
	parent := "repo@main"
	cyc := nav.ReviewCycle{Parent: parent, AgentNames: nav.CanonicalAgentNames(), Round: 1}
	got, ok := nav.ResolveLateral(parent, nav.DirRight, cyc, alwaysLive)
	want := parent + "~review-1-" + nav.CanonicalAgentNames()[0]
	if !ok || got != want {
		t.Errorf("right from parent: got (%q,%v), want (%q,true)", got, ok, want)
	}
}

func TestResolveLateral_LeftFromParentIsLastLiveEntry(t *testing.T) {
	parent := "repo@main"
	agents := nav.CanonicalAgentNames()
	cyc := nav.ReviewCycle{Parent: parent, AgentNames: agents, Round: 1}

	// Case A: all agents live → left from parent goes to last agent.
	allNames := []string{parent}
	for _, a := range agents {
		allNames = append(allNames, parent+"~review-1-"+a)
	}
	got, ok := nav.ResolveLateral(parent, nav.DirLeft, cyc, liveSet(allNames...))
	wantLast := parent + "~review-1-" + agents[len(agents)-1]
	if !ok || got != wantLast {
		t.Errorf("left from parent (all live): got (%q,%v), want (%q,true)", got, ok, wantLast)
	}

	// Case B: only the first agent is live → left from parent should skip
	// straight to that first (and only) live agent, since it is also the
	// last live entry preceding the parent in the cycle.
	firstChild := parent + "~review-1-" + agents[0]
	got, ok = nav.ResolveLateral(parent, nav.DirLeft, cyc, liveSet(parent, firstChild))
	if !ok || got != firstChild {
		t.Errorf("left from parent (only first agent live): got (%q,%v), want (%q,true)", got, ok, firstChild)
	}
}

func TestResolveLateral_SkipsDeadEntries(t *testing.T) {
	parent := "repo@main"
	agents := nav.CanonicalAgentNames() // [review-goal, review-code, review-security, review-qa, review-context]
	cyc := nav.ReviewCycle{Parent: parent, AgentNames: agents, Round: 1}

	// Live set: parent + review-goal + review-security (skip review-code).
	goal := parent + "~review-1-" + agents[0]
	security := parent + "~review-1-" + agents[2]
	live := liveSet(parent, goal, security)

	// right from review-goal must skip review-code and land on review-security.
	got, ok := nav.ResolveLateral(goal, nav.DirRight, cyc, live)
	if !ok || got != security {
		t.Errorf("right from goal: got (%q,%v), want (%q,true)", got, ok, security)
	}
	// left from review-security must skip review-code and land on review-goal.
	got, ok = nav.ResolveLateral(security, nav.DirLeft, cyc, live)
	if !ok || got != goal {
		t.Errorf("left from security: got (%q,%v), want (%q,true)", got, ok, goal)
	}
}

func TestResolveLateral_NoOpWhenContractsBelowTwo(t *testing.T) {
	parent := "repo@main"
	cyc := nav.ReviewCycle{Parent: parent, AgentNames: nav.CanonicalAgentNames(), Round: 1}
	// Only `parent` is live; the cycle contracts to a single entry.
	got, ok := nav.ResolveLateral(parent, nav.DirRight, cyc, liveSet(parent))
	if ok {
		t.Errorf("contracted-to-one cycle: expected no-op, got %q", got)
	}
}

func TestResolveLateral_RejectsNonLateralDirection(t *testing.T) {
	parent := "repo@main"
	cyc := nav.ReviewCycle{Parent: parent, AgentNames: nav.CanonicalAgentNames(), Round: 1}
	for _, d := range []nav.Direction{nav.DirUp, nav.DirDown} {
		if _, ok := nav.ResolveLateral(parent, d, cyc, alwaysLive); ok {
			t.Errorf("dir=%q: expected no-op", d)
		}
	}
}

// TestResolveReviewContext_AllChildrenDead: parent has only finished/dead
// children — no live tmux session for any review-N — so no review context
// applies and left/right must be no-ops.
func TestResolveReviewContext_AllChildrenDead(t *testing.T) {
	parent := "repo@main"
	r1Child := parent + "~review-1-review-goal"
	sessions := makeSessions(t,
		[2]string{parent, "active"},
		[2]string{r1Child, "finished"},
	)
	if _, ok := nav.ResolveReviewContext(parent, sessions, neverLive); ok {
		t.Errorf("expected no review context when no child is live")
	}
}

// TestCanonicalAgentNames_MatchesReviewAgents: smoke test guaranteeing the
// nav cycle stays in sync with the canonical review.Agents() list — this is
// the "do not hardcode it twice" invariant from the implementation notes.
func TestCanonicalAgentNames_MatchesReviewAgents(t *testing.T) {
	got := nav.CanonicalAgentNames()
	if len(got) == 0 {
		t.Fatalf("CanonicalAgentNames returned empty slice")
	}
	for _, name := range got {
		if !strings.HasPrefix(name, "review-") {
			t.Errorf("agent name %q does not start with %q", name, "review-")
		}
	}
}

// helpers

func makeChildNames(parent string, round int, agents []string) []string {
	out := make([]string, len(agents))
	for i, a := range agents {
		out[i] = parent + "~review-" + itoa(round) + "-" + a
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
