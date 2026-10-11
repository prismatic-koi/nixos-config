package review_test

// recovery_fallback_3124_test.go — issue #3124. The recovery path starts the
// fallback of an agent that ended with a refusal, from the plan the monitor
// saved.

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

func TestStartRecoveryFallbacks_StartsFallbackAfterRefusal(t *testing.T) {
	h := newFallbackHarness(t, "refusal")
	opts := review.MonitorOpts{
		GroupID:       h.groupID,
		Agents:        h.agents,
		AgentSessions: h.sessions,
		Fallback: &review.FallbackPlan{Agents: []review.AgentFallbackPlan{
			{Model: fbFirst},
			{Model: fbFirst, Fallbacks: []string{fbModelOne}},
		}},
	}
	data, _ := json.Marshal(opts)
	path := review.MonitorOptsPath(h.groupID)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })

	var got []string
	spawn := func(d *db.DB, c review.FallbackSpawnCall) error {
		got = append(got, c.Session)
		return nil
	}
	if n := review.StartRecoveryFallbacksForTest(h.d, h.groupID, spawn); n != 1 {
		t.Fatalf("started = %d, want 1", n)
	}
	want := h.sessions[1] + "-fallback-1"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("spawned %v, want [%s]", got, want)
	}
}

func TestStartRecoveryFallbacks_NoFallbackLeft(t *testing.T) {
	h := newFallbackHarness(t, "refusal")
	opts := review.MonitorOpts{
		GroupID:       h.groupID,
		Agents:        h.agents,
		AgentSessions: h.sessions,
		Fallback: &review.FallbackPlan{Agents: []review.AgentFallbackPlan{
			{Model: fbFirst}, {Model: fbFirst},
		}},
	}
	data, _ := json.Marshal(opts)
	path := review.MonitorOptsPath(h.groupID)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })
	spawn := func(d *db.DB, c review.FallbackSpawnCall) error {
		t.Errorf("unexpected spawn %s", c.Session)
		return nil
	}
	if n := review.StartRecoveryFallbacksForTest(h.d, h.groupID, spawn); n != 0 {
		t.Fatalf("started = %d, want 0", n)
	}
}
