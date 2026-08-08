package exporter_test

// Tests for the six #2703 lifecycle and outcome counters. These reuse the
// harness from exporter_test.go and add a fixture helper that wires up the
// sessions / agent_status rows the label-enrichment join in
// LifecycleEventsTailSQL needs.

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/exporter"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
)

// spawnFixture wires up a sessions row (agent_role), an agent_status row
// (isolation_mode), and a spawn_inputs row (profile_name) sharing one
// instance_id, then writes a session.spawn_intent event referencing it —
// the shape LifecycleEventsTailSQL's join expects. Pass "" for profileName
// to leave spawn_inputs.profile_name NULL (issue #2720's default-fold AC).
func (h *harness) spawnFixture(repo, agentRole, isolationMode, profileName string) (instanceID, sessionName string) {
	h.t.Helper()
	instanceID = uuid.New().String()
	sessionName = "prism-test@" + instanceID[:8]

	if err := h.writeDB.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		AgentRole:   &agentRole,
		Repo:        repo,
		Worktree:    "/tmp/prism-test",
		Harness:     "pi",
		StartedAt:   time.Now(),
	}); err != nil {
		h.t.Fatalf("InsertSession: %v", err)
	}
	if err := h.writeDB.UpsertStatus(sessionName, repo, "/tmp/prism-test", "starting", nil, nil); err != nil {
		h.t.Fatalf("UpsertStatus: %v", err)
	}
	if err := h.writeDB.SetInstanceID(sessionName, instanceID); err != nil {
		h.t.Fatalf("SetInstanceID: %v", err)
	}
	if err := h.writeDB.SetIsolationMode(sessionName, isolationMode); err != nil {
		h.t.Fatalf("SetIsolationMode: %v", err)
	}

	si := db.SpawnInputs{
		InstanceID: instanceID,
		CreatedAt:  time.Now().UnixMilli(),
	}
	if profileName != "" {
		si.ProfileName = &profileName
	}
	if err := h.writeDB.InsertSpawnInputs(si); err != nil {
		h.t.Fatalf("InsertSpawnInputs: %v", err)
	}

	iid := instanceID
	if err := h.writeDB.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Repo:        repo,
		Worktree:    "/tmp/prism-test",
		InstanceID:  &iid,
		Type:        session.EventSpawnIntent,
		Payload:     "{}",
		CreatedAt:   time.Now(),
	}); err != nil {
		h.t.Fatalf("WriteEvent(spawn_intent): %v", err)
	}
	return instanceID, sessionName
}

// endFixture ends the session created by spawnFixture with endState, and
// writes the session_reaped event RecordSessionReap would write.
func (h *harness) endFixture(instanceID, sessionName, endState string) {
	h.t.Helper()
	if err := h.writeDB.UpdateSessionEnded(instanceID, endState); err != nil {
		h.t.Fatalf("UpdateSessionEnded: %v", err)
	}
	if err := h.writeDB.RecordSessionReap(sessionName, db.ReapCauseCleanupCommand, ""); err != nil {
		h.t.Fatalf("RecordSessionReap: %v", err)
	}
}

func counterValue(t *testing.T, h *harness, metric string, labels map[string]string) float64 {
	t.Helper()
	exp := h.scrape(h.exp)
	v, _ := exp.Value(metric, labels)
	return v
}

// ── AC: all six counters appear in /metrics and increase on their event ───

func TestExporter_SpawnsTotalIncrementsOnSpawnIntent(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	h.spawnFixture("nixos-config", "worker", "bwrap", "max")
	h.spawnFixture("nixos-config", "worker", "bwrap", "max")
	h.spawnFixture("nixos-config", "coordinator", "host", "")

	labels := map[string]string{"repo": "nixos-config", "agent_role": "worker", "isolation_mode": "bwrap", "profile": "max"}
	if got := counterValue(t, h, exporter.MetricSpawnsTotal, labels); got != 2 {
		t.Errorf("%s%v = %v, want 2", exporter.MetricSpawnsTotal, labels, got)
	}
	coordLabels := map[string]string{"repo": "nixos-config", "agent_role": "coordinator", "isolation_mode": "host", "profile": "default"}
	if got := counterValue(t, h, exporter.MetricSpawnsTotal, coordLabels); got != 1 {
		t.Errorf("%s%v = %v, want 1", exporter.MetricSpawnsTotal, coordLabels, got)
	}
}

// ── AC (edge-case): a NULL profile_name is labelled "default", not "" ─────

func TestExporter_SpawnsTotalLabelsNullProfileAsDefault(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	h.spawnFixture("nixos-config", "worker", "bwrap", "")

	labels := map[string]string{"repo": "nixos-config", "agent_role": "worker", "isolation_mode": "bwrap", "profile": "default"}
	if got := counterValue(t, h, exporter.MetricSpawnsTotal, labels); got != 1 {
		t.Errorf("%s%v = %v, want 1", exporter.MetricSpawnsTotal, labels, got)
	}
	emptyLabels := map[string]string{"repo": "nixos-config", "agent_role": "worker", "isolation_mode": "bwrap", "profile": ""}
	if got := counterValue(t, h, exporter.MetricSpawnsTotal, emptyLabels); got != 0 {
		t.Errorf("%s%v = %v, want 0 (NULL profile_name must fold to \"default\", not empty string)", exporter.MetricSpawnsTotal, emptyLabels, got)
	}
}

func TestExporter_SessionsEndedTotalIncrementsOnSessionReaped(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	instanceID, sessionName := h.spawnFixture("nixos-config", "worker", "bwrap", "max")
	h.endFixture(instanceID, sessionName, "finished")

	labels := map[string]string{"repo": "nixos-config", "agent_role": "worker", "end_state": "finished"}
	if got := counterValue(t, h, exporter.MetricSessionsEndedTotal, labels); got != 1 {
		t.Errorf("%s%v = %v, want 1", exporter.MetricSessionsEndedTotal, labels, got)
	}
}

func TestExporter_ReviewVerdictsTotalIncrementsOnVerdictEvents(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	h.writeEvent(review.EventReviewVerdictPass, 0)
	h.writeEvent(review.EventReviewVerdictPass, 0)
	h.writeEvent(review.EventReviewVerdictFail, 0)

	if got := counterValue(t, h, exporter.MetricReviewVerdictsTotal, map[string]string{"verdict": "pass"}); got != 2 {
		t.Errorf("verdict=pass = %v, want 2", got)
	}
	if got := counterValue(t, h, exporter.MetricReviewVerdictsTotal, map[string]string{"verdict": "fail"}); got != 1 {
		t.Errorf("verdict=fail = %v, want 1", got)
	}
}

func TestExporter_EscalationsTotalIncrementsOnSessionEscalated(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	h.writeEvent("session.escalated", 0)
	h.writeEvent("session.escalated", 0)

	if got := counterValue(t, h, exporter.MetricEscalationsTotal, map[string]string{"repo": "nixos-config"}); got != 2 {
		t.Errorf("prism_escalations_total{repo=nixos-config} = %v, want 2", got)
	}
}

func TestExporter_DoomLoopsTotalIncrementsOnDoomLoopDetected(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	h.writeEvent("doom_loop_detected", 0)

	if got := counterValue(t, h, exporter.MetricDoomLoopsTotal, map[string]string{"repo": "nixos-config"}); got != 1 {
		t.Errorf("prism_doom_loops_total{repo=nixos-config} = %v, want 1", got)
	}
}

func TestExporter_PermissionDeniedTotalIncrementsOnPermissionDenied(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	h.writeEvent("permission_denied", 0)
	h.writeEvent("permission_denied", 0)
	h.writeEvent("permission_denied", 0)

	if got := counterValue(t, h, exporter.MetricPermissionDeniedTotal, map[string]string{"repo": "nixos-config"}); got != 3 {
		t.Errorf("prism_permission_denied_total{repo=nixos-config} = %v, want 3", got)
	}
}

// ── AC: prism_escalations_total is derived from session.escalated, not text ─

func TestExporter_EscalationsTotalIgnoresOtherEventTypes(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	h.writeEvent("msg_user", 0)
	h.writeEvent("msg_assistant", 0)
	h.writeEvent("bus_message_like_but_not_real", 0)

	if got := counterValue(t, h, exporter.MetricEscalationsTotal, map[string]string{"repo": "nixos-config"}); got != 0 {
		t.Errorf("prism_escalations_total{repo=nixos-config} = %v, want 0 (no session.escalated event was written)", got)
	}
}

// ── AC (edge-case): pruning rows behind the cursor never decreases a counter ─

func TestExporter_LifecycleCountersSurvivePruneBehindCursor(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	// Old events the 90-day prune will remove.
	for i := 0; i < 3; i++ {
		h.writeEvent("doom_loop_detected", 100*24*time.Hour)
		h.writeEvent("permission_denied", 100*24*time.Hour)
		h.writeEvent("session.escalated", 100*24*time.Hour)
	}
	// A recent one of each, which prune must keep.
	h.writeEvent("doom_loop_detected", time.Minute)
	h.writeEvent("permission_denied", time.Minute)
	h.writeEvent("session.escalated", time.Minute)

	labels := map[string]string{"repo": "nixos-config"}
	before := map[string]float64{
		exporter.MetricDoomLoopsTotal:        counterValue(t, h, exporter.MetricDoomLoopsTotal, labels),
		exporter.MetricPermissionDeniedTotal: counterValue(t, h, exporter.MetricPermissionDeniedTotal, labels),
		exporter.MetricEscalationsTotal:      counterValue(t, h, exporter.MetricEscalationsTotal, labels),
	}
	for metric, v := range before {
		if v != 4 {
			t.Fatalf("%s before prune = %v, want 4", metric, v)
		}
	}

	rowsBefore := h.rowCount()
	if err := h.writeDB.Prune(pruneHorizon); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if rowsAfter := h.rowCount(); rowsAfter >= rowsBefore {
		t.Fatalf("Prune removed nothing (%d before, %d after); the test would prove nothing", rowsBefore, rowsAfter)
	}

	for metric, wantBefore := range before {
		if got := counterValue(t, h, metric, labels); got != wantBefore {
			t.Errorf("%s moved from %v to %v across a prune; a counter must never decrease", metric, wantBefore, got)
		}
	}

	// And it keeps counting forward.
	h.writeEvent("doom_loop_detected", 0)
	if got := counterValue(t, h, exporter.MetricDoomLoopsTotal, labels); got != before[exporter.MetricDoomLoopsTotal]+1 {
		t.Errorf("%s after a post-prune write = %v, want %v", exporter.MetricDoomLoopsTotal, got, before[exporter.MetricDoomLoopsTotal]+1)
	}
}

// ── AC (edge-case): every counter survives an exporter restart ────────────

func TestExporter_LifecycleCountersSurviveRestart(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	h.writeEvent("doom_loop_detected", 0)
	h.writeEvent("permission_denied", 0)
	h.writeEvent("session.escalated", 0)
	h.writeEvent(review.EventReviewVerdictPass, 0)
	h.spawnFixture("nixos-config", "worker", "bwrap", "max")

	labels := map[string]string{"repo": "nixos-config"}
	spawnLabels := map[string]string{"repo": "nixos-config", "agent_role": "worker", "isolation_mode": "bwrap", "profile": "max"}
	verdictLabels := map[string]string{"verdict": "pass"}

	before := struct {
		doomLoops, permissionDenied, escalations, verdicts, spawns float64
	}{
		doomLoops:        counterValue(t, h, exporter.MetricDoomLoopsTotal, labels),
		permissionDenied: counterValue(t, h, exporter.MetricPermissionDeniedTotal, labels),
		escalations:      counterValue(t, h, exporter.MetricEscalationsTotal, labels),
		verdicts:         counterValue(t, h, exporter.MetricReviewVerdictsTotal, verdictLabels),
		spawns:           counterValue(t, h, exporter.MetricSpawnsTotal, spawnLabels),
	}
	if before.doomLoops != 1 || before.permissionDenied != 1 || before.escalations != 1 || before.verdicts != 1 || before.spawns != 1 {
		t.Fatalf("unexpected pre-restart values: %+v", before)
	}

	if err := h.exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	restarted := h.newExporter()
	h.start(restarted)

	if got := restartedValue(t, restarted, exporter.MetricDoomLoopsTotal, labels); got != before.doomLoops {
		t.Errorf("%s = %v after restart, want %v", exporter.MetricDoomLoopsTotal, got, before.doomLoops)
	}
	if got := restartedValue(t, restarted, exporter.MetricPermissionDeniedTotal, labels); got != before.permissionDenied {
		t.Errorf("%s = %v after restart, want %v", exporter.MetricPermissionDeniedTotal, got, before.permissionDenied)
	}
	if got := restartedValue(t, restarted, exporter.MetricEscalationsTotal, labels); got != before.escalations {
		t.Errorf("%s = %v after restart, want %v", exporter.MetricEscalationsTotal, got, before.escalations)
	}
	if got := restartedValue(t, restarted, exporter.MetricReviewVerdictsTotal, verdictLabels); got != before.verdicts {
		t.Errorf("%s = %v after restart, want %v", exporter.MetricReviewVerdictsTotal, got, before.verdicts)
	}
	if got := restartedValue(t, restarted, exporter.MetricSpawnsTotal, spawnLabels); got != before.spawns {
		t.Errorf("%s = %v after restart, want %v", exporter.MetricSpawnsTotal, got, before.spawns)
	}
}

func restartedValue(t *testing.T, e *exporter.Exporter, metric string, labels map[string]string) float64 {
	t.Helper()
	h := &harness{t: t, exp: e}
	exp := h.scrape(e)
	v, _ := exp.Value(metric, labels)
	return v
}

// ── AC (security): no metric carries session_name, instance_id, or issue_ref ─

func TestExporter_LifecycleCountersCarryNoUnboundedLabel(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	h.spawnFixture("nixos-config", "worker", "bwrap", "max")
	h.writeEvent("session.escalated", 0)
	h.writeEvent("doom_loop_detected", 0)
	h.writeEvent("permission_denied", 0)
	h.writeEvent(review.EventReviewVerdictPass, 0)

	banned := []string{"session_name", "instance_id", "issue_ref"}
	exp := h.scrape(h.exp)
	for _, name := range []string{
		exporter.MetricSpawnsTotal, exporter.MetricSessionsEndedTotal, exporter.MetricReviewVerdictsTotal,
		exporter.MetricEscalationsTotal, exporter.MetricDoomLoopsTotal, exporter.MetricPermissionDeniedTotal,
	} {
		family, ok := exp.Families[name]
		if !ok {
			continue
		}
		for _, s := range family.Samples {
			for label := range s.Labels {
				for _, b := range banned {
					if label == b {
						t.Errorf("metric %s carries the unbounded label %q", name, label)
					}
				}
			}
		}
	}
}
