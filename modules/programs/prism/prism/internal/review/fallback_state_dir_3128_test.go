package review_test

// fallback_state_dir_3128_test.go — issue #3128. The monitor stores the
// fallback plan and its log in the prism state dir. `prism review --wait`
// waits after a refusal only when the plan has a fallback left, and parent
// cleanup removes the stored plans.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

func writePlan(t *testing.T, opts review.MonitorOpts) string {
	t.Helper()
	path := review.MonitorOptsPath(opts.GroupID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(opts)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMonitorPaths_UnderPrismStateDir_3128(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	want := filepath.Join(state, "prism") + string(filepath.Separator)
	for _, p := range []string{review.MonitorOptsPath("g-1"), review.MonitorLogPath("42", 3)} {
		if !strings.HasPrefix(p, want) {
			t.Errorf("path %q is not under %q", p, want)
		}
	}
	if !strings.Contains(review.MonitorOptsPath("g-1"), "g-1") {
		t.Errorf("opts path %q is not keyed by the group ID", review.MonitorOptsPath("g-1"))
	}
}

// StartMonitorProcess writes the opts file and the log under the state dir.
// The fake binary exits at once, so no monitor runs.
func TestStartMonitorProcess_WritesUnderStateDir_3128(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	bin := filepath.Join(t.TempDir(), "fake-prism")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := review.MonitorOpts{GroupID: "g-start", PRNumber: "77", Round: 2}
	if err := review.StartMonitorProcess(opts, bin); err != nil {
		t.Fatalf("StartMonitorProcess: %v", err)
	}
	got, err := review.LoadMonitorOptsFromFile(review.MonitorOptsPath("g-start"))
	if err != nil || got.GroupID != "g-start" {
		t.Fatalf("opts file under state dir: %+v, %v", got, err)
	}
	if _, err := os.Stat(review.MonitorLogPath("77", 2)); err != nil {
		t.Fatalf("monitor log under state dir: %v", err)
	}
}

func TestRefusalAwaitingFallback_3128(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h := newFallbackHarness(t, "refusal")
	members, err := h.d.GroupMembersForGroup(h.groupID)
	if err != nil {
		t.Fatal(err)
	}
	results, err := h.d.GroupResults(h.groupID)
	if err != nil {
		t.Fatal(err)
	}
	plan := func(fallbacks ...string) review.MonitorOpts {
		return review.MonitorOpts{
			GroupID:       h.groupID,
			Agents:        h.agents,
			AgentSessions: h.sessions,
			Fallback: &review.FallbackPlan{Agents: []review.AgentFallbackPlan{
				{Model: fbFirst},
				{Model: fbFirst, Fallbacks: fallbacks},
			}},
		}
	}

	if review.RefusalAwaitingFallback(h.groupID, members, results) {
		t.Error("no stored plan: awaiting, want not awaiting")
	}
	writePlan(t, plan())
	if review.RefusalAwaitingFallback(h.groupID, members, results) {
		t.Error("plan with no fallbacks: awaiting, want not awaiting")
	}
	writePlan(t, plan(fbModelOne))
	if !review.RefusalAwaitingFallback(h.groupID, members, results) {
		t.Error("plan with a fallback left: not awaiting, want awaiting")
	}

	// The only fallback ran and was refused too: nothing is left.
	fb := h.sessions[1] + "-fallback-1"
	seedMember(t, h.d, h.groupID, fb, "active")
	seedRunError(t, h.d, fb, "refusal")
	members, _ = h.d.GroupMembersForGroup(h.groupID)
	results, _ = h.d.GroupResults(h.groupID)
	if review.RefusalAwaitingFallback(h.groupID, members, results) {
		t.Error("fallbacks used up: awaiting, want not awaiting")
	}
	if !review.RefusalInGroup(members, results) {
		t.Error("RefusalInGroup: false, want true")
	}
}

func TestRefusalAwaitingFallback_NonRefusal_3128(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h := newFallbackHarness(t, "max_tokens")
	writePlan(t, review.MonitorOpts{
		GroupID: h.groupID, Agents: h.agents, AgentSessions: h.sessions,
		Fallback: &review.FallbackPlan{Agents: []review.AgentFallbackPlan{
			{Model: fbFirst}, {Model: fbFirst, Fallbacks: []string{fbModelOne}},
		}},
	})
	members, _ := h.d.GroupMembersForGroup(h.groupID)
	results, _ := h.d.GroupResults(h.groupID)
	if review.RefusalAwaitingFallback(h.groupID, members, results) {
		t.Error("non-refusal run_error: awaiting, want not awaiting")
	}
}

// A missing plan starts no fallback and is not an error.
func TestStartRecoveryFallbacks_MissingPlan_3128(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h := newFallbackHarness(t, "refusal")
	spawn := func(d *db.DB, c review.FallbackSpawnCall) error {
		t.Errorf("unexpected spawn %s", c.Session)
		return nil
	}
	if n := review.StartRecoveryFallbacksForTest(h.d, h.groupID, spawn); n != 0 {
		t.Fatalf("started = %d, want 0", n)
	}
}

func TestParentCleanup_RemovesStoredPlans_3128(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := openTestDB(t)
	t.Cleanup(review.ReapSideEffectsWithStopForTest(nil, func(string) bool { return true }, nil))
	parent := "nixos-config@plan-cleanup"
	g1, _ := seedRound(t, d, parent, 1, []string{"finished"}, true)
	g2, _ := seedRound(t, d, parent, 2, []string{"finished"}, false)
	other, _ := seedRound(t, d, "nixos-config@plan-other", 1, []string{"finished"}, false)
	var paths []string
	for _, g := range []string{g1, g2, other} {
		paths = append(paths, writePlan(t, review.MonitorOpts{GroupID: g}))
	}

	review.CleanupReviewSessionsForParent(d, parent)

	for _, p := range paths[:2] {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("plan %s still exists after parent cleanup (err=%v)", p, err)
		}
	}
	if _, err := os.Stat(paths[2]); err != nil {
		t.Errorf("plan of another parent was removed: %v", err)
	}
}
