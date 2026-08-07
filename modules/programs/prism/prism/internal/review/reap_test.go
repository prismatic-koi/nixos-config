package review_test

// reap_test.go — tests for the automatic release of finished review-agent
// sessions (issue #2649).
//
// The suite is grouped by acceptance criterion:
//
//   - release          — a terminal review agent loses its tmux session and
//                        its harness port with no operator action.
//   - preservation     — the agent_status row, the sessions row, the
//                        session_groups row, and every agent_events row
//                        survive, so `prism retro` and `prism checkin` keep
//                        working.
//   - no git           — no worktree is removed and no branch is deleted.
//   - liveness safety  — a live agent is never a candidate, on either of the
//                        two arms that can make it live (#2613).
//   - grace period     — nothing is released before the window elapses.
//   - three rounds     — the leak the issue reports does not accumulate.
//   - orphaned parent  — reaping is safe when the parent has already gone.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

// reapProbe records the reaper's process-side effects so a test can assert
// both what WAS torn down and — more importantly — what was not.
type reapProbe struct {
	mu         sync.Mutex
	tmuxKilled []string
	sidecars   []string
	containers []string
}

func (p *reapProbe) killTmux(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tmuxKilled = append(p.tmuxKilled, name)
}

func (p *reapProbe) killSidecar(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sidecars = append(p.sidecars, name)
}

func (p *reapProbe) removeContainer(name, _ string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.containers = append(p.containers, name)
}

func (p *reapProbe) killed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.tmuxKilled))
	copy(out, p.tmuxKilled)
	return out
}

// installReapProbe stubs the three process-side effects for the duration of
// the test and returns the recorder.
func installReapProbe(t *testing.T) *reapProbe {
	t.Helper()
	p := &reapProbe{}
	restore := review.ReapSideEffectsForTest(p.killTmux, p.killSidecar, p.removeContainer)
	t.Cleanup(restore)
	return p
}

// seedRound registers a review group for parent and seeds `states` as its
// members, named `<parent>~review-<round>-<role-N>`. It returns the group id
// and the member session names.
//
// delivered controls the load-bearing gate: when true the group's
// delivered_at is stamped, which is what makes its terminal members
// reap-eligible at all.
func seedRound(t *testing.T, d *db.DB, parent string, round int, states []string, delivered bool) (string, []string) {
	t.Helper()
	groupID, err := d.RegisterGroupWithPR(parent, "2649", round)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}
	roles := []string{"review-goal", "review-code", "review-qa", "review-security", "review-context"}
	sessions := make([]string, 0, len(states))
	for i, state := range states {
		role := roles[i%len(roles)]
		sess := parent + "~review-" + strconv.Itoa(round) + "-" + role
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", state, nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", sess, err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", sess, err)
		}
		if _, err := d.AllocatePort(sess); err != nil {
			t.Fatalf("AllocatePort(%q): %v", sess, err)
		}
		sessions = append(sessions, sess)
	}
	if delivered {
		if err := d.SetGroupDeliveredAt(groupID); err != nil {
			t.Fatalf("SetGroupDeliveredAt: %v", err)
		}
	}
	return groupID, sessions
}

// afterGrace is a clock reading far enough past "now" that any group
// delivered during the test is outside the grace window.
func afterGrace() time.Time {
	return time.Now().Add(review.ReapGracePeriod + time.Hour)
}

// statusOf fetches a row and fails the test when it is absent — the reaper
// must never delete one.
func statusOf(t *testing.T, d *db.DB, sess string) *db.Status {
	t.Helper()
	st, err := d.CurrentStatus(sess)
	if err != nil {
		t.Fatalf("CurrentStatus(%q): %v", sess, err)
	}
	if st == nil {
		t.Fatalf("CurrentStatus(%q): row is gone — the reaper must never delete a DB row", sess)
	}
	return st
}

// ── AC: a terminal review agent is released without operator action ─────────

// TestReap_ReleasesFinishedReviewAgent is the primary functional AC: after a
// round has been delivered and the grace period has elapsed, every member's
// tmux session is killed and its harness port is returned to the pool, with
// no operator command in between.
func TestReap_ReleasesFinishedReviewAgent(t *testing.T) {
	d := openTestDB(t)
	probe := installReapProbe(t)
	parent := "nixos-config@reap-release"

	_, sessions := seedRound(t, d, parent, 1,
		[]string{"finished", "finished", "finished", "finished", "finished"}, true)

	// Every member starts with a port allocated and no ended_at — the exact
	// shape the issue reports as leaked.
	for _, sess := range sessions {
		st := statusOf(t, d, sess)
		if st.HarnessPort == nil {
			t.Fatalf("precondition: %q has no harness port allocated", sess)
		}
		if st.EndedAt != nil {
			t.Fatalf("precondition: %q is already ended", sess)
		}
	}

	res, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0)
	if err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}
	if len(res.Reaped) != len(sessions) {
		t.Fatalf("reaped %d session(s), want %d: %v", len(res.Reaped), len(sessions), res.Reaped)
	}

	for _, sess := range sessions {
		st := statusOf(t, d, sess)
		if st.HarnessPort != nil {
			t.Errorf("%q: harness port = %d after reap, want released (NULL)", sess, *st.HarnessPort)
		}
		if st.EndedAt == nil {
			t.Errorf("%q: ended_at is still NULL after reap — the concurrency slot is not returned", sess)
		}
	}

	killed := probe.killed()
	if len(killed) != len(sessions) {
		t.Errorf("tmux kills = %v, want one per member %v", killed, sessions)
	}
	if len(probe.sidecars) != len(sessions) {
		t.Errorf("sidecar kills = %v, want one per member", probe.sidecars)
	}
	if len(probe.containers) != len(sessions) {
		t.Errorf("container teardowns = %v, want one per member", probe.containers)
	}
}

// TestReap_ReleasesConcurrencySlot pins the mechanism by which capacity comes
// back: Isolator.Cap counts agent_status rows with ended_at IS NULL for the
// mode, so the count for the reaped mode must drop to zero.
func TestReap_ReleasesConcurrencySlot(t *testing.T) {
	d := openTestDB(t)
	installReapProbe(t)
	parent := "nixos-config@reap-slot"

	_, sessions := seedRound(t, d, parent, 1,
		[]string{"finished", "finished", "finished", "finished", "finished"}, true)
	for _, sess := range sessions {
		if err := d.SetIsolationMode(sess, "bwrap"); err != nil {
			t.Fatalf("SetIsolationMode(%q): %v", sess, err)
		}
	}

	before, err := d.ActiveSessionCountForMode("bwrap")
	if err != nil {
		t.Fatalf("ActiveSessionCountForMode: %v", err)
	}
	if before != len(sessions) {
		t.Fatalf("precondition: bwrap active count = %d, want %d", before, len(sessions))
	}

	if _, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0); err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}

	after, err := d.ActiveSessionCountForMode("bwrap")
	if err != nil {
		t.Fatalf("ActiveSessionCountForMode: %v", err)
	}
	if after != 0 {
		t.Errorf("bwrap active count after reap = %d, want 0 — the cap still counts the reaped agents", after)
	}
}

// ── AC: the agent_status row and the review history survive ─────────────────

// TestReap_PreservesRowsAndHistory covers the "never delete a DB row" AC.
// `prism retro` and the retro flow read historical review data, so the
// agent_status row, the session_groups row, and every agent_events row must
// outlive the reap.
func TestReap_PreservesRowsAndHistory(t *testing.T) {
	d := openTestDB(t)
	installReapProbe(t)
	parent := "nixos-config@reap-history"

	groupID, sessions := seedRound(t, d, parent, 1, []string{"finished", "finished"}, true)
	for i, sess := range sessions {
		seedAssistantEvent(t, d, sess, "Reviewed thoroughly.\n<verdict>PASS</verdict> "+strconv.Itoa(i))
	}

	if _, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0); err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}

	for _, sess := range sessions {
		// statusOf fails the test when the row is gone.
		st := statusOf(t, d, sess)
		if st.State != "finished" {
			t.Errorf("%q: state = %q after reap, want the recorded terminal state preserved", sess, st.State)
		}
		if st.GroupID == nil || *st.GroupID != groupID {
			t.Errorf("%q: group_id was cleared by the reap — checkin scope and retro both resolve through it", sess)
		}

		// The conversation history is what `prism checkin` renders and what
		// the retro flow reads. It must be intact.
		events, err := d.QueryEvents(sess, 10, nil, nil, []string{"msg_assistant"})
		if err != nil {
			t.Fatalf("QueryEvents(%q): %v", sess, err)
		}
		if len(events) == 0 {
			t.Errorf("%q: msg_assistant events are gone after reap — historical review data must remain readable", sess)
		}
	}

	// The group row itself must survive: db.GroupParentForMember resolves the
	// checkin tier-1 grant through it, so deleting it would silently revoke a
	// worker's right to read its own review agents.
	gi, err := d.GetGroup(groupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if gi == nil {
		t.Fatal("session_groups row was deleted by the reap")
	}
}

// TestReap_WorkerCanStillReadItsReviewAgents is the edge-case AC on the
// documented post-round read. `prism checkin <parent>~review-<N>-<agent>` is
// admitted by authz tier 1, which resolves membership through
// db.GroupParentForMember, and it renders agent_events rows. Both survive the
// reap, so the documented read keeps working after the session is gone.
func TestReap_WorkerCanStillReadItsReviewAgents(t *testing.T) {
	d := openTestDB(t)
	installReapProbe(t)
	parent := "nixos-config@reap-checkin"

	_, sessions := seedRound(t, d, parent, 1, []string{"finished"}, true)
	seedAssistantEvent(t, d, sessions[0], "Blocking: the guard is missing.\n<verdict>FAIL</verdict>")

	if _, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0); err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}

	// The tier-1 scope check.
	gotParent, found, err := d.GroupParentForMember(sessions[0])
	if err != nil {
		t.Fatalf("GroupParentForMember: %v", err)
	}
	if !found || gotParent != parent {
		t.Errorf("GroupParentForMember(%q) = (%q, %v), want (%q, true) — the worker's checkin grant was revoked by the reap",
			sessions[0], gotParent, found, parent)
	}

	// The content the read renders.
	events, err := d.QueryEvents(sessions[0], 10, nil, nil, []string{"msg_assistant"})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no msg_assistant events after reap — the worker cannot read the agent's reasoning")
	}
	if !strings.Contains(events[0].Payload, "Blocking") {
		t.Errorf("event payload = %q, want the agent's reasoning preserved verbatim", events[0].Payload)
	}
}

// ── AC: reaping never removes a worktree and never deletes a branch ─────────

// TestReap_LeavesWorktreeAndBranchIntact is the direct falsification of the
// git AC. It builds a real repository with a real branch, points the review
// agents' rows at that worktree (which is how a review agent's row is written
// — it inherits the parent's worktree path), reaps, and asserts both survive.
//
// This is the #2638 failure mode restated as a test: descendant sessions
// inherit the parent's worktree, so any teardown that resolves a branch from
// the worktree HEAD would force-delete the PARENT's branch here.
func TestReap_LeavesWorktreeAndBranchIntact(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	d := openTestDB(t)
	installReapProbe(t)

	worktree := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = worktree
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-b", "main", ".")
	git("commit", "--allow-empty", "-m", "base")
	git("branch", "reap-victim-branch")

	marker := filepath.Join(worktree, "keep-me.txt")
	if err := os.WriteFile(marker, []byte("wip"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	parent := "nixos-config@reap-victim-branch"
	groupID, err := d.RegisterGroupWithPR(parent, "2649", 1)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}
	sess := parent + "~review-1-review-goal"
	// The agent row carries the PARENT's worktree — the inheritance that
	// #2638 turned into a branch deletion.
	if err := d.UpsertStatus(sess, "nixos-config", worktree, "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetGroupID(sess, groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}
	if err := d.SetGroupDeliveredAt(groupID); err != nil {
		t.Fatalf("SetGroupDeliveredAt: %v", err)
	}

	if _, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0); err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("worktree content is gone after reap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".git")); err != nil {
		t.Errorf("worktree .git is gone after reap: %v", err)
	}
	out, err := exec.Command("git", "-C", worktree, "branch", "--list", "reap-victim-branch").Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if !strings.Contains(string(out), "reap-victim-branch") {
		t.Errorf("branch reap-victim-branch was deleted by the reap (git branch --list output: %q)", out)
	}
}

// ── AC: reaping cannot act on an agent that is not terminal (#2613) ─────────

// TestReap_DoesNotReapLiveAgentInRunningRound covers the first and most
// important liveness arm: the round has NOT been delivered, so no member is a
// candidate — not even the member that has already finished while its
// siblings work. This is the group-level gate that makes #2613 structurally
// impossible.
func TestReap_DoesNotReapLiveAgentInRunningRound(t *testing.T) {
	d := openTestDB(t)
	probe := installReapProbe(t)
	parent := "nixos-config@reap-inflight"

	// A realistic in-flight round: one agent done, four still working.
	_, sessions := seedRound(t, d, parent, 1,
		[]string{"finished", "active", "active", "active", "active"}, false /* not delivered */)

	res, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0)
	if err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}
	if len(res.Reaped) != 0 {
		t.Fatalf("reaped %v from a round that has not been delivered — this is the #2613 failure mode", res.Reaped)
	}
	if killed := probe.killed(); len(killed) != 0 {
		t.Fatalf("killed tmux sessions %v during a running round", killed)
	}
	for _, sess := range sessions {
		st := statusOf(t, d, sess)
		if st.EndedAt != nil {
			t.Errorf("%q was ended during a running round", sess)
		}
		if st.HarnessPort == nil {
			t.Errorf("%q lost its harness port during a running round", sess)
		}
	}
}

// TestReap_DoesNotReapNonTerminalAgentInDeliveredRound covers the second
// liveness arm: the round HAS been delivered (e.g. the monitor gave up at its
// safety timeout and delivered partial results), but one member is still
// running. That member must survive; its terminal siblings are released.
//
// The `interrupted` case is included deliberately: an interrupted agent can
// still be redirected with `prism prompt` (#1495), so it is not terminal for
// the reaper any more than it is for db.terminalStates.
func TestReap_DoesNotReapNonTerminalAgentInDeliveredRound(t *testing.T) {
	for _, liveState := range []string{"active", "idle", "waiting", "interrupted", "reviewing"} {
		t.Run(liveState, func(t *testing.T) {
			d := openTestDB(t)
			probe := installReapProbe(t)
			parent := "nixos-config@reap-live-" + liveState

			_, sessions := seedRound(t, d, parent, 1,
				[]string{liveState, "finished"}, true /* delivered */)
			live, done := sessions[0], sessions[1]

			res, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0)
			if err != nil {
				t.Fatalf("ReapDeliveredReviewAgents: %v", err)
			}

			for _, name := range res.Reaped {
				if name == live {
					t.Fatalf("reaped %q while it was in state %q", live, liveState)
				}
			}
			for _, name := range probe.killed() {
				if name == live {
					t.Fatalf("killed the tmux session of %q while it was in state %q", live, liveState)
				}
			}

			liveStatus := statusOf(t, d, live)
			if liveStatus.EndedAt != nil {
				t.Errorf("%q (state %q) was ended by the reaper", live, liveState)
			}
			if liveStatus.HarnessPort == nil {
				t.Errorf("%q (state %q) lost its harness port to the reaper", live, liveState)
			}

			// The terminal sibling is still released — the guard is
			// per-session, not a blanket refusal for the whole round.
			doneStatus := statusOf(t, d, done)
			if doneStatus.EndedAt == nil {
				t.Errorf("%q is terminal and delivered but was not reaped", done)
			}
		})
	}
}

// TestReap_SkipsSessionThatIsNotAReviewAgent exercises the Go-side shape
// re-check. A session_groups row is only ever written by `prism review`, so
// this candidate cannot arise through the supported write paths; the test
// pins the defence-in-depth branch so a future session kind registered
// against session_groups is skipped rather than torn down.
func TestReap_SkipsSessionThatIsNotAReviewAgent(t *testing.T) {
	d := openTestDB(t)
	probe := installReapProbe(t)

	parent := "nixos-config@reap-shape"
	groupID, err := d.RegisterGroupWithPR(parent, "2649", 1)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}
	// Hand-write a group membership for a session whose name is not a review
	// agent: a plain worker, and an investigator child.
	impostors := []string{
		"nixos-config@some-worker",
		parent + "~investigate-prompt-size",
	}
	for _, sess := range impostors {
		if err := d.UpsertStatus(sess, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", sess, err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", sess, err)
		}
	}
	if err := d.SetGroupDeliveredAt(groupID); err != nil {
		t.Fatalf("SetGroupDeliveredAt: %v", err)
	}

	res, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0)
	if err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}
	if len(res.Reaped) != 0 {
		t.Errorf("reaped %v, want none — none of these names is a review agent", res.Reaped)
	}
	if len(res.Skipped) != len(impostors) {
		t.Errorf("skipped %v, want all %v", res.Skipped, impostors)
	}
	if killed := probe.killed(); len(killed) != 0 {
		t.Errorf("killed %v, want no tmux kills", killed)
	}
	for _, sess := range impostors {
		if st := statusOf(t, d, sess); st.EndedAt != nil {
			t.Errorf("%q was ended despite not being a review agent", sess)
		}
	}
}

// ── AC: the grace period ────────────────────────────────────────────────────

// TestReap_HonoursGracePeriod asserts that a round delivered just now is not
// released, and that the same round IS released once the window has passed.
// Both halves use the same seeded state, so the only variable is the clock.
func TestReap_HonoursGracePeriod(t *testing.T) {
	d := openTestDB(t)
	probe := installReapProbe(t)
	parent := "nixos-config@reap-grace"

	_, sessions := seedRound(t, d, parent, 1, []string{"finished", "finished"}, true)

	// Inside the window: delivered_at is ~now, and the cut-off is
	// now - ReapGracePeriod, which is earlier.
	res, err := review.ReapDeliveredReviewAgents(d, "", time.Now(), 0)
	if err != nil {
		t.Fatalf("ReapDeliveredReviewAgents (inside grace): %v", err)
	}
	if len(res.Reaped) != 0 {
		t.Fatalf("reaped %v inside the grace period", res.Reaped)
	}
	if killed := probe.killed(); len(killed) != 0 {
		t.Fatalf("killed %v inside the grace period", killed)
	}
	for _, sess := range sessions {
		if st := statusOf(t, d, sess); st.EndedAt != nil {
			t.Errorf("%q was ended inside the grace period", sess)
		}
	}

	// One millisecond past the window.
	res, err = review.ReapDeliveredReviewAgents(d, "", time.Now().Add(review.ReapGracePeriod+time.Millisecond), 0)
	if err != nil {
		t.Fatalf("ReapDeliveredReviewAgents (past grace): %v", err)
	}
	if len(res.Reaped) != len(sessions) {
		t.Errorf("reaped %v past the grace period, want all of %v", res.Reaped, sessions)
	}
}

// TestReapGracePeriod_MatchesDocumentedValue pins the constant against the
// number stated in prose. Changing the constant without updating the prose
// makes the documented post-round window a lie, which is exactly the class of
// stale prose the tool-surface rule in AGENTS.md exists to prevent.
//
// These prose sites state the number. Update them with the constant:
//
//   - modules/programs/prism/skills/prism/SKILL.md
//     ("How long review-agent sessions stay live")
//   - modules/programs/prism/skills/retro/SKILL.md
//     (subsession discovery, and the "no review cycles" edge case)
//   - modules/programs/prism/prism/docs/invariants/session-lifecycle.md
//     ("Session kinds")
//   - modules/programs/prism/prism/cmd/review.go — BOTH the file header and
//     the cobra `Long` string that `prism review --help` prints.
//   - modules/programs/prism/prism/internal/review/review.go (package doc)
//   - modules/programs/prism/prism/internal/review/run.go (the Run doc
//     comment and two inline comments inside Run)
//
// Do not rely on this list. It was tried on its own and it failed twice: the
// cobra `Long` string went stale in round 1 of PR #2676, and the four
// internal/review sites in round 2. Two mechanical guards now cover the sites
// a list kept missing — TestReviewHelp_* in package cmd for the runtime help
// text, and TestNoProseClaimsSessionsPersistUntilCleanup in this package for
// Go source. The list is a pointer for the author, not the enforcement.
func TestReapGracePeriod_MatchesDocumentedValue(t *testing.T) {
	if review.ReapGracePeriod != 15*time.Minute {
		t.Errorf("ReapGracePeriod = %s, want 15m — update the four prose sites listed on this test together with the constant",
			review.ReapGracePeriod)
	}
}

// ── AC: three review cycles leave no live review sessions ───────────────────

// TestReap_ThreeRoundsLeaveNoLiveSessions is the acceptance test for the
// reported symptom. Three completed rounds on one worker produce fifteen
// review-agent sessions; once the rounds are delivered and the grace period
// has elapsed, none of them is still live.
//
// The test also asserts the intermediate state the documented window
// promises: with round 3 just delivered and rounds 1-2 past their grace, only
// round 3's five sessions remain live.
func TestReap_ThreeRoundsLeaveNoLiveSessions(t *testing.T) {
	d := openTestDB(t)
	installReapProbe(t)
	parent := "nixos-config@reap-three-rounds"

	full := []string{"finished", "finished", "finished", "finished", "finished"}
	_, round1 := seedRound(t, d, parent, 1, full, true)
	_, round2 := seedRound(t, d, parent, 2, full, true)

	liveCount := func() int {
		t.Helper()
		rows, err := d.GroupMembersForParent(parent)
		if err != nil {
			t.Fatalf("GroupMembersForParent: %v", err)
		}
		n := 0
		for _, r := range rows {
			if r.EndedAt == nil {
				n++
			}
		}
		return n
	}

	if got := liveCount(); got != len(round1)+len(round2) {
		t.Fatalf("precondition: %d live sessions, want %d", got, len(round1)+len(round2))
	}

	// Round 3 is delivered now; rounds 1 and 2 were delivered long enough ago
	// to be past their grace. Model that by sweeping with a clock just past
	// the grace period for rounds 1-2 only, then adding round 3.
	if _, err := review.ReapDeliveredReviewAgents(d, "",
		time.Now().Add(review.ReapGracePeriod+time.Second), 0); err != nil {
		t.Fatalf("ReapDeliveredReviewAgents (rounds 1-2): %v", err)
	}
	_, round3 := seedRound(t, d, parent, 3, full, true)

	if got := liveCount(); got != len(round3) {
		t.Errorf("with round 3 inside its grace window, %d sessions are live, want exactly %d (one round)",
			got, len(round3))
	}

	// Once round 3's window elapses too, nothing is left.
	if _, err := review.ReapDeliveredReviewAgents(d, "",
		time.Now().Add(review.ReapGracePeriod+time.Hour), 0); err != nil {
		t.Fatalf("ReapDeliveredReviewAgents (round 3): %v", err)
	}
	if got := liveCount(); got != 0 {
		t.Errorf("%d review sessions still live after three completed rounds, want 0", got)
	}

	// Fifteen rows must still be readable — none was deleted.
	rows, err := d.GroupMembersForParent(parent)
	if err != nil {
		t.Fatalf("GroupMembersForParent: %v", err)
	}
	if len(rows) != len(round1)+len(round2)+len(round3) {
		t.Errorf("%d agent_status rows survive, want %d — the reaper deleted rows",
			len(rows), len(round1)+len(round2)+len(round3))
	}
}

// ── AC: reaping is safe when the parent has already been cleaned up ─────────

// TestReap_SafeWhenParentAlreadyCleanedUp covers the edge-case AC. The parent
// worker's row is closed (or absent entirely); the reaper must still release
// the orphaned review agents and must not touch the parent.
func TestReap_SafeWhenParentAlreadyCleanedUp(t *testing.T) {
	t.Run("parent row closed", func(t *testing.T) {
		d := openTestDB(t)
		probe := installReapProbe(t)
		parent := "nixos-config@reap-orphan-closed"

		if err := d.UpsertStatus(parent, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus parent: %v", err)
		}
		// Close the parent the way `prism cleanup` does, BEFORE the review
		// children exist. SetEnded cascades to `<parent>~review-%` rows that
		// are present when it runs, so seeding afterwards reproduces the
		// orphan shape the AC describes: parent closed, children still live.
		if err := d.SetEnded(parent); err != nil {
			t.Fatalf("SetEnded(parent): %v", err)
		}
		_, sessions := seedRound(t, d, parent, 1, []string{"finished", "finished"}, true)
		for _, sess := range sessions {
			if st := statusOf(t, d, sess); st.EndedAt != nil {
				t.Fatalf("precondition: %q is already ended", sess)
			}
		}

		res, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0)
		if err != nil {
			t.Fatalf("ReapDeliveredReviewAgents: %v", err)
		}
		if len(res.Reaped) != len(sessions) {
			t.Errorf("reaped %v, want all orphaned children %v", res.Reaped, sessions)
		}
		for _, name := range probe.killed() {
			if name == parent {
				t.Fatalf("the reaper killed the parent session %q", parent)
			}
		}
	})

	t.Run("parent row absent", func(t *testing.T) {
		d := openTestDB(t)
		installReapProbe(t)
		parent := "nixos-config@reap-orphan-absent"

		// Never write an agent_status row for the parent at all.
		_, sessions := seedRound(t, d, parent, 1, []string{"finished"}, true)

		res, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0)
		if err != nil {
			t.Fatalf("ReapDeliveredReviewAgents: %v", err)
		}
		if len(res.Reaped) != len(sessions) {
			t.Errorf("reaped %v, want %v — a missing parent row must not block the reap", res.Reaped, sessions)
		}
		if st := statusOf(t, d, sessions[0]); st.EndedAt == nil {
			t.Errorf("%q was not ended", sessions[0])
		}
	})
}

// ── Idempotence and scoping ─────────────────────────────────────────────────

// TestReap_IsIdempotent verifies that a second sweep finds nothing: the
// `ended_at IS NULL` arm of the predicate excludes rows the first sweep
// closed, so a repeated sweep issues no tmux kill and no DB write.
func TestReap_IsIdempotent(t *testing.T) {
	d := openTestDB(t)
	probe := installReapProbe(t)
	parent := "nixos-config@reap-idempotent"

	_, sessions := seedRound(t, d, parent, 1, []string{"finished", "finished"}, true)

	first, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(first.Reaped) != len(sessions) {
		t.Fatalf("first sweep reaped %v, want %v", first.Reaped, sessions)
	}
	killsAfterFirst := len(probe.killed())

	second, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(second.Reaped) != 0 {
		t.Errorf("second sweep reaped %v, want none", second.Reaped)
	}
	if got := len(probe.killed()); got != killsAfterFirst {
		t.Errorf("second sweep issued %d extra tmux kill(s)", got-killsAfterFirst)
	}
}

// TestReap_GroupScopeLeavesOtherGroupsAlone pins the groupID parameter: the
// monitor sweeps only the round it delivered, so a sibling round that is
// eligible must be left for the unscoped sweep.
func TestReap_GroupScopeLeavesOtherGroupsAlone(t *testing.T) {
	d := openTestDB(t)
	installReapProbe(t)
	parent := "nixos-config@reap-scope"

	mineID, mine := seedRound(t, d, parent, 1, []string{"finished"}, true)
	_, theirs := seedRound(t, d, "nixos-config@other-worker", 1, []string{"finished"}, true)

	res, err := review.ReapDeliveredReviewAgents(d, mineID, afterGrace(), 0)
	if err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}
	if len(res.Reaped) != 1 || res.Reaped[0] != mine[0] {
		t.Errorf("scoped sweep reaped %v, want exactly %v", res.Reaped, mine)
	}
	if st := statusOf(t, d, theirs[0]); st.EndedAt != nil {
		t.Errorf("scoped sweep reached %q, which belongs to another group", theirs[0])
	}
}

// TestReap_UnscopedSweepCrossesParents pins the reason the pre-spawn sweep in
// `prism review` is unscoped: the concurrency cap is global, so one worker's
// leaked agents are what refuse another worker's spawn. A per-parent sweep
// would not reclaim them.
func TestReap_UnscopedSweepCrossesParents(t *testing.T) {
	d := openTestDB(t)
	installReapProbe(t)

	_, a := seedRound(t, d, "nixos-config@worker-a", 1, []string{"finished"}, true)
	_, b := seedRound(t, d, "nixos-config@worker-b", 2, []string{"finished"}, true)

	res, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0)
	if err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}
	if len(res.Reaped) != 2 {
		t.Fatalf("reaped %v, want both %v and %v", res.Reaped, a, b)
	}
	for _, sess := range append(append([]string{}, a...), b...) {
		if st := statusOf(t, d, sess); st.EndedAt == nil {
			t.Errorf("%q was not reaped by the unscoped sweep", sess)
		}
	}
}

// TestReap_NilDBReturnsError guards the caller contract: every call site
// passes a handle it has just opened, and a nil handle is a programming error
// rather than an empty result set.
func TestReap_NilDBReturnsError(t *testing.T) {
	if _, err := review.ReapDeliveredReviewAgents(nil, "", time.Now(), 0); err == nil {
		t.Error("ReapDeliveredReviewAgents(nil, ...) = nil error, want an error")
	}
}

// ── The post-delivery trigger ───────────────────────────────────────────────

// fakeReapClock is a controllable clock for the post-delivery wait. Sleeping
// advances it; Now reads it. It lets a test assert the production ordering
// (wait first, reap second) without a real 15-minute wall-clock wait.
type fakeReapClock struct {
	now   time.Time
	slept []time.Duration
	// onSleep runs at the moment the wait is requested, before the clock
	// advances — the hook a test uses to assert nothing was torn down yet.
	onSleep func()
}

func (c *fakeReapClock) Sleep(d time.Duration) {
	if c.onSleep != nil {
		c.onSleep()
	}
	c.slept = append(c.slept, d)
	// Model the wait rather than replay it: the clock jumps to d past the real
	// "now" at the moment the wait starts, which is exactly where a real sleep
	// would leave it. Advancing a clock frozen at install time instead would
	// land BEFORE any delivered_at the code under test writes in between, and
	// the sweep would then find nothing for a reason that says nothing about
	// production.
	c.now = time.Now().Add(d)
}

func (c *fakeReapClock) Now() time.Time { return c.now }

// installFakeReapClock wires a fake clock into ReapGroupAfterGrace for the
// duration of the test.
func installFakeReapClock(t *testing.T, onSleep func()) *fakeReapClock {
	t.Helper()
	c := &fakeReapClock{now: time.Now(), onSleep: onSleep}
	t.Cleanup(review.ReapClockForTest(c.Sleep, c.Now))
	return c
}

// TestReapGroupAfterGrace_WaitsThenReaps verifies the trigger the monitor
// uses: it waits the grace period first, and only then releases the round.
func TestReapGroupAfterGrace_WaitsThenReaps(t *testing.T) {
	d := openTestDB(t)
	probe := installReapProbe(t)
	parent := "nixos-config@reap-after-grace"

	groupID, sessions := seedRound(t, d, parent, 1, []string{"finished", "finished"}, true)

	// The hook fires at the moment the wait is requested. Nothing may have
	// been torn down yet — the reap must come after the wait, not before it.
	clock := installFakeReapClock(t, func() {
		if killed := probe.killed(); len(killed) != 0 {
			t.Errorf("tmux sessions %v were killed BEFORE the grace period elapsed", killed)
		}
	})

	review.ReapGroupAfterGrace(d, groupID, 0)

	if len(clock.slept) != 1 || clock.slept[0] != review.ReapGracePeriod {
		t.Errorf("waited %v, want exactly one wait of %s", clock.slept, review.ReapGracePeriod)
	}
	for _, sess := range sessions {
		if st := statusOf(t, d, sess); st.EndedAt == nil {
			t.Errorf("%q was not reaped after the grace period", sess)
		}
	}
	if len(probe.killed()) != len(sessions) {
		t.Errorf("tmux kills = %v, want one per member %v", probe.killed(), sessions)
	}
}

// TestReapGroupAfterGrace_UsesSuppliedGrace verifies the override path used by
// MonitorOpts.ReapGrace.
func TestReapGroupAfterGrace_UsesSuppliedGrace(t *testing.T) {
	d := openTestDB(t)
	installReapProbe(t)
	parent := "nixos-config@reap-custom-grace"

	groupID, sessions := seedRound(t, d, parent, 1, []string{"finished"}, true)
	clock := installFakeReapClock(t, nil)

	review.ReapGroupAfterGrace(d, groupID, 42*time.Second)

	if len(clock.slept) != 1 || clock.slept[0] != 42*time.Second {
		t.Errorf("waited %v, want exactly one wait of 42s", clock.slept)
	}
	if st := statusOf(t, d, sessions[0]); st.EndedAt == nil {
		t.Errorf("%q was not reaped after the supplied grace period", sessions[0])
	}
}

// ── The monitor wiring ──────────────────────────────────────────────────────

// runMonitorForReapTest drives MonitorFunc against a seeded, already-terminal
// group and returns once the review-complete prompt has been delivered. It
// stands up the httptest harness the delivery path needs, mirroring the setup
// in monitor_test.go.
func runMonitorForReapTest(t *testing.T, d *db.DB, parent string, sessions []string, groupID string, reapAfterDelivery bool) {
	t.Helper()

	workerSession := parent
	if err := d.UpsertStatus(workerSession, "nixos-config", "/wt", "reviewing", nil, nil); err != nil {
		t.Fatalf("UpsertStatus worker: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	if err := setHarnessInfo(t, d, workerSession, extractPort(t, srv.URL), "reap-test-pi-session"); err != nil {
		t.Fatalf("setHarnessInfo: %v", err)
	}

	agents := make([]review.Agent, 0, len(sessions))
	for _, sess := range sessions {
		idx := strings.LastIndex(sess, "-review-")
		agents = append(agents, review.Agent{Name: "review" + sess[idx+len("-review"):]})
	}

	opts := review.MonitorOpts{
		GroupID:              groupID,
		WorkerSession:        workerSession,
		PRNumber:             "2649",
		Round:                1,
		Agents:               agents,
		AgentSessions:        sessions,
		DBPath:               d.Path(),
		PollInterval:         10 * time.Millisecond,
		MaxDeliveryRetries:   0,
		DeliveryRetryBackoff: time.Millisecond,
		ReapAfterDelivery:    reapAfterDelivery,
	}
	if err := review.MonitorFunc(opts); err != nil {
		t.Fatalf("MonitorFunc: %v", err)
	}
}

// TestMonitorFunc_ReapsRoundAfterDelivery is the end-to-end wiring test for
// the "on round completion" trigger: a monitor that delivers a round then
// releases that round's agent sessions once the grace period elapses. This is
// the path RunAsync configures for every production round.
func TestMonitorFunc_ReapsRoundAfterDelivery(t *testing.T) {
	d := openTestDB(t)
	probe := installReapProbe(t)
	parent := "nixos-config@reap-monitor"

	groupID, sessions := seedRound(t, d, parent, 1, []string{"finished", "finished"}, false)
	for _, sess := range sessions {
		seedAssistantEvent(t, d, sess, "Looks good.\n<verdict>PASS</verdict>")
	}

	// The clock hook asserts the ordering that matters: at the moment the
	// wait is requested, the review-complete prompt must already have been
	// delivered — delivered_at is the gate the reap predicate reads, so a
	// reap scheduled before that write could never fire.
	installFakeReapClock(t, func() {
		gi, err := d.GetGroup(groupID)
		if err != nil || gi == nil {
			t.Fatalf("GetGroup during wait: %v", err)
		}
		// delivered_at is the gate the reap predicate reads. If the wait were
		// scheduled before that write, the sweep that follows it could never
		// find a candidate.
		if gi.DeliveredAt == nil {
			t.Errorf("group %s has no delivered_at at the moment the wait starts — the reap is scheduled before the gate it depends on", groupID)
		}
		if len(probe.killed()) != 0 {
			t.Errorf("tmux sessions %v were killed before the grace period elapsed", probe.killed())
		}
	})

	runMonitorForReapTest(t, d, parent, sessions, groupID, true)

	for _, sess := range sessions {
		if st := statusOf(t, d, sess); st.EndedAt == nil {
			t.Errorf("%q was not reaped after the monitor delivered its round", sess)
		}
	}
	if got := len(probe.killed()); got != len(sessions) {
		t.Errorf("tmux kills = %v, want one per member %v", probe.killed(), sessions)
	}
	// The worker session itself is never a candidate.
	for _, name := range probe.killed() {
		if name == parent {
			t.Fatalf("the monitor's reap killed the parent worker %q", parent)
		}
	}
	if st := statusOf(t, d, parent); st.EndedAt != nil {
		t.Errorf("the monitor's reap ended the parent worker %q", parent)
	}
}

// TestMonitorFunc_NoReapWhenNotRequested pins the opt-in: a MonitorOpts
// without ReapAfterDelivery neither waits nor tears anything down, so every
// pre-existing MonitorFunc test keeps its original behaviour and its original
// runtime.
func TestMonitorFunc_NoReapWhenNotRequested(t *testing.T) {
	d := openTestDB(t)
	probe := installReapProbe(t)
	parent := "nixos-config@reap-monitor-optout"

	groupID, sessions := seedRound(t, d, parent, 1, []string{"finished"}, false)
	seedAssistantEvent(t, d, sessions[0], "Looks good.\n<verdict>PASS</verdict>")

	clock := installFakeReapClock(t, nil)

	runMonitorForReapTest(t, d, parent, sessions, groupID, false)

	if len(clock.slept) != 0 {
		t.Errorf("monitor waited %v with ReapAfterDelivery unset, want no wait", clock.slept)
	}
	if killed := probe.killed(); len(killed) != 0 {
		t.Errorf("monitor killed %v with ReapAfterDelivery unset, want no teardown", killed)
	}
	if st := statusOf(t, d, sessions[0]); st.EndedAt != nil {
		t.Errorf("%q was ended with ReapAfterDelivery unset", sessions[0])
	}
}

// ── The reap must not silence the LOOP-LIMIT footer ─────────────────────────

// seedVerdictRound seeds one complete, verdict-producing round for parent:
// every member finished with a parseable `<verdict>` tag. It returns the
// group id and the member session names.
func seedVerdictRound(t *testing.T, d *db.DB, parent string, round int, verdict string) (string, []string) {
	t.Helper()
	groupID, sessions := seedRound(t, d, parent, round,
		[]string{"finished", "finished", "finished", "finished", "finished"}, true)
	for _, sess := range sessions {
		seedAssistantEvent(t, d, sess,
			"Reviewed round "+strconv.Itoa(round)+".\n<verdict>"+verdict+"</verdict>")
	}
	return groupID, sessions
}

// TestCompletedReviewCycles_SurviveTheReap is the regression guard for the
// interaction between the automatic release (#2649) and the LOOP-LIMIT footer
// (#1512).
//
// CompletedReviewCyclesForParent counts a past round only when every member
// produced a parseable verdict. Its read used to be db.GroupResults, which
// drops rows with ended_at set. The release stamps ended_at on every member of
// a delivered round, so once it runs, every past round read as "produced no
// verdicts" and the count collapsed to zero.
//
// The consequence was not cosmetic: the footer is what tells a worker it has
// reached three cycles and must stop and escalate. A worker whose rounds were
// released before the next round completed would never have been told.
//
// The history the count needs is intact after a release — the release deletes
// no agent_status row and no agent_events row. Only the read had to change.
func TestCompletedReviewCycles_SurviveTheReap(t *testing.T) {
	d := openTestDB(t)
	installReapProbe(t)
	parent := "nixos-config@reap-loop-limit"

	// Three complete, non-converged rounds — the shape that must trip the
	// LOOP-LIMIT footer.
	for round := 1; round <= 3; round++ {
		seedVerdictRound(t, d, parent, round, "FAIL")
	}

	before, err := review.CompletedReviewCyclesForParent(d, parent, "")
	if err != nil {
		t.Fatalf("CompletedReviewCyclesForParent (before reap): %v", err)
	}
	if before != 3 {
		t.Fatalf("precondition: counted %d cycles before the reap, want 3", before)
	}

	res, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0)
	if err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}
	if len(res.Reaped) != 15 {
		t.Fatalf("reaped %d sessions, want all 15", len(res.Reaped))
	}

	after, err := review.CompletedReviewCyclesForParent(d, parent, "")
	if err != nil {
		t.Fatalf("CompletedReviewCyclesForParent (after reap): %v", err)
	}
	if after != 3 {
		t.Errorf("counted %d cycles after the reap, want 3 — releasing a round must not erase it from the cycle count, or the LOOP-LIMIT footer stops firing",
			after)
	}
}

// TestCompletedReviewCycles_ReapedRoundWithoutVerdictsStillDoesNotCount pins
// the other direction: the wider read must not start counting rounds that
// never produced a full set of verdicts. An incomplete round stays
// non-counting after it is released, exactly as it was before.
func TestCompletedReviewCycles_ReapedRoundWithoutVerdictsStillDoesNotCount(t *testing.T) {
	d := openTestDB(t)
	installReapProbe(t)
	parent := "nixos-config@reap-loop-limit-incomplete"

	// One good round, and one where a single agent emitted no verdict.
	seedVerdictRound(t, d, parent, 1, "FAIL")
	_, round2 := seedRound(t, d, parent, 2,
		[]string{"finished", "finished", "finished", "finished", "finished"}, true)
	for _, sess := range round2[:4] {
		seedAssistantEvent(t, d, sess, "Reviewed.\n<verdict>FAIL</verdict>")
	}
	// round2[4] finishes with no assistant message at all — no verdict.

	before, err := review.CompletedReviewCyclesForParent(d, parent, "")
	if err != nil {
		t.Fatalf("CompletedReviewCyclesForParent (before reap): %v", err)
	}
	if before != 1 {
		t.Fatalf("precondition: counted %d cycles before the reap, want 1", before)
	}

	if _, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0); err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}

	after, err := review.CompletedReviewCyclesForParent(d, parent, "")
	if err != nil {
		t.Fatalf("CompletedReviewCyclesForParent (after reap): %v", err)
	}
	if after != 1 {
		t.Errorf("counted %d cycles after the reap, want 1 — an incomplete round must stay non-counting", after)
	}
}

// TestReap_RecordsAutoReleaseCause verifies that the release records why it
// closed the row (#2613). Without it, a coordinator asking why a review-agent
// row is closed is told that nothing recorded why — the exact gap #2613 exists
// to remove, on what is now the most common closer of these rows.
func TestReap_RecordsAutoReleaseCause(t *testing.T) {
	d := openTestDB(t)
	installReapProbe(t)
	parent := "nixos-config@reap-cause"

	_, sessions := seedRound(t, d, parent, 1, []string{"finished", "error"}, true)

	if _, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0); err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}

	causes, err := d.SessionEndCauses(sessions)
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	for _, sess := range sessions {
		rec, ok := causes[sess]
		if !ok {
			t.Fatalf("%q: no close cause recorded", sess)
		}
		if rec.Cause != db.ReapCauseAutoRelease {
			t.Errorf("%q: Cause = %q, want %q", sess, rec.Cause, db.ReapCauseAutoRelease)
		}
		if !rec.Recorded() {
			t.Errorf("%q: Recorded() = false, want true", sess)
		}
		if rec.Detail == "" {
			t.Errorf("%q: Detail is empty, want the delivery time and the grace period", sess)
		}
	}
}

// TestReap_CauseIsRecordedBeforeTheRowCloses pins the ordering the #2613
// contract requires: a reader that sees a closed row must also see the cause.
// A cause written after ended_at leaves a window in which the row reads as
// closed with nothing to explain it.
//
// The assertion runs INSIDE the closing write, which is the only point where
// the ordering is falsifiable. An earlier version of this test asserted either
// side of the sweep and passed with the two writes swapped — it pinned the end
// state, not the order.
func TestReap_CauseIsRecordedBeforeTheRowCloses(t *testing.T) {
	d := openTestDB(t)
	installReapProbe(t)
	parent := "nixos-config@reap-cause-order"
	_, sessions := seedRound(t, d, parent, 1, []string{"finished"}, true)
	sess := sessions[0]

	observed := false
	t.Cleanup(review.ReapSetEndedForTest(func(handle *db.DB, name string) error {
		if name == sess {
			observed = true
			// At the instant the row is about to close, the cause must
			// already be on the trail...
			causes, err := handle.SessionEndCauses([]string{name})
			if err != nil {
				t.Errorf("SessionEndCauses during close: %v", err)
			} else if causes[name].Cause != db.ReapCauseAutoRelease {
				t.Errorf("at the moment %q closes, Cause = %q, want %q — the cause must be recorded BEFORE ended_at is stamped",
					name, causes[name].Cause, db.ReapCauseAutoRelease)
			}
			// ...and the row must not be closed yet.
			if st := statusOf(t, handle, name); st.EndedAt != nil {
				t.Errorf("%q was already closed when the closing write ran", name)
			}
		}
		return handle.SetEnded(name)
	}))

	if _, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0); err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}
	if !observed {
		t.Fatal("the closing write never ran for the seeded session — the assertion above never executed")
	}
	if st := statusOf(t, d, sess); st.EndedAt == nil {
		t.Errorf("%q: ended_at is NULL, want the row closed", sess)
	}
}

// TestReap_SkipsCauseWhenTheRowIsAlreadyClosed pins the ended_at guard on the
// cause write. The candidate query filters on ended_at IS NULL at QUERY time;
// a parent cleanup or a second concurrent sweep can close the row before this
// sweep's write lands. SessionEndCauses returns the latest event, so an
// unguarded write would overwrite a true parent_cleanup with a close this
// sweep did not perform.
func TestReap_SkipsCauseWhenTheRowIsAlreadyClosed(t *testing.T) {
	d := openTestDB(t)
	parent := "nixos-config@reap-cause-raced"
	_, sessions := seedRound(t, d, parent, 1, []string{"finished"}, true)
	sess := sessions[0]

	// Simulate the race: another path closes the row and records its own cause
	// between the candidate query and this sweep's write. The container hook is
	// the last step before the cause write, so it is the right injection point.
	t.Cleanup(review.ReapSideEffectsForTest(nil, nil, func(name, _ string) {
		if name != sess {
			return
		}
		d.RecordReapBestEffort(name, db.ReapCauseParentCleanup, "raced ahead of the release")
		if err := d.SetEnded(name); err != nil {
			t.Fatalf("SetEnded during simulated race: %v", err)
		}
	}))

	if _, err := review.ReapDeliveredReviewAgents(d, "", afterGrace(), 0); err != nil {
		t.Fatalf("ReapDeliveredReviewAgents: %v", err)
	}

	causes, err := d.SessionEndCauses([]string{sess})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	if causes[sess].Cause != db.ReapCauseParentCleanup {
		t.Errorf("Cause = %q, want %q — the release must not claim a close another path performed",
			causes[sess].Cause, db.ReapCauseParentCleanup)
	}
}
