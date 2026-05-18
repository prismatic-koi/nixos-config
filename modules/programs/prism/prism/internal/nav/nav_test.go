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

func TestIsTopLevel(t *testing.T) {
	cases := []struct {
		name    string
		s       dashboard.AgentSession
		topLvl  bool
	}{
		{"plain", dashboard.AgentSession{Name: "scratchpad"}, true},
		{"main", dashboard.AgentSession{Name: "nixos-config@main"}, true},
		{"branch", dashboard.AgentSession{Name: "nixos-config@feature"}, false},
		{"depth2", dashboard.AgentSession{Name: "nixos-config@feature~review-1-review-goal"}, false},
		{"review-group", dashboard.AgentSession{Name: "nixos-config@feature~review-1", IsReviewGroup: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nav.IsTopLevel(c.s); got != c.topLvl {
				t.Errorf("IsTopLevel(%q) = %v, want %v", c.s.Name, got, c.topLvl)
			}
		})
	}
}

func TestVerticalTargets_FiltersTerminalAndNonLive(t *testing.T) {
	sessions := makeSessions(t,
		[2]string{"alpha@main", "active"},
		[2]string{"alpha@feature", "active"}, // depth-1: not top-level
		[2]string{"beta@main", "finished"},   // terminal state: excluded
		[2]string{"gamma@main", "idle"},
		[2]string{"gamma@feature~review-1-review-goal", "active"}, // depth-2: excluded
		[2]string{"delta@main", "active"},                          // not live: excluded
		[2]string{"scratchpad", "idle"},
	)
	live := liveSet("alpha@main", "alpha@feature", "gamma@main", "gamma@feature~review-1-review-goal", "scratchpad")

	got := nav.VerticalTargets(sessions, live)
	want := []string{"alpha@main", "gamma@main", "scratchpad"}
	if !equalSlice(got, want) {
		t.Errorf("VerticalTargets = %v, want %v", got, want)
	}
}

func TestVerticalTargets_TerminalStatesExcluded(t *testing.T) {
	for _, state := range []string{"finished", "deleted", "interrupted"} {
		sessions := makeSessions(t,
			[2]string{"alpha@main", "active"},
			[2]string{"beta@main", state},
		)
		got := nav.VerticalTargets(sessions, alwaysLive)
		if len(got) != 1 || got[0] != "alpha@main" {
			t.Errorf("state=%q: got %v, want [alpha@main]", state, got)
		}
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
