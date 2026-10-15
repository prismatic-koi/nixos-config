package exporter_test

// Tests for the seven lifecycle and outcome counters. These reuse the
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
// to leave spawn_inputs.profile_name NULL (the default-fold AC).
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

// writeEventWithRepo writes an event with a specified repo, for testing
// empty repo folding. Most tests use writeEvent (from exporter_test.go) which
// defaults to "nixos-config".
func (h *harness) writeEventWithRepo(eventType, repo string, age time.Duration) {
	h.t.Helper()
	err := h.writeDB.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: "prism-test@exporter",
		Repo:        repo,
		Worktree:    "/tmp/prism-test",
		Type:        eventType,
		Payload:     `{"note":"test"}`,
		CreatedAt:   time.Now().Add(-age),
	})
	if err != nil {
		h.t.Fatalf("WriteEvent(%s): %v", eventType, err)
	}
}

// writeReviewVerdictEvent writes a review verdict event referencing
// instanceID, matching the shape persistReviewOutcome writes
// (internal/review/monitor.go): the SessionName/Repo/Worktree on the event
// are the review-monitor's own values, but InstanceID is always the
// REVIEWED WORKER's — the join in LifecycleEventsTailSQL resolves repo,
// agent_role, and profile from that instance_id, not from the event's own
// Repo column. repo is passed separately here only to mirror what
// persistReviewOutcome writes onto the event row itself (sess.Repo); the
// asserted label value always comes from the fixture's instance_id join.
func (h *harness) writeReviewVerdictEvent(eventType, repo, instanceID string) {
	h.t.Helper()
	iid := instanceID
	if err := h.writeDB.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: "prism-test@exporter",
		Repo:        repo,
		Worktree:    "/tmp/prism-test",
		InstanceID:  &iid,
		Type:        eventType,
		Payload:     "{}",
		CreatedAt:   time.Now(),
	}); err != nil {
		h.t.Fatalf("WriteEvent(%s): %v", eventType, err)
	}
}

// writeAgentVerdictEvent writes a PER-AGENT verdict event referencing
// instanceID — the shape writeAgentVerdictEvents produces
// (internal/review/monitor.go). The instance_id is the REVIEW AGENT's, not the
// worker's: prism_review_agent_verdicts_total's agent_role label is resolved
// from it through the sessions join in LifecycleEventsTailSQL, which is what
// makes the label name the review dimension (review-security, …) rather than
// the worker's role. repo mirrors what the writer puts on the event row; the
// asserted label value always comes from that column via repoLabel.
func (h *harness) writeAgentVerdictEvent(eventType, repo, instanceID string) {
	h.t.Helper()
	iid := instanceID
	if err := h.writeDB.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: "prism-test@exporter~review-1-agent",
		Repo:        repo,
		Worktree:    "/tmp/prism-test",
		InstanceID:  &iid,
		Type:        eventType,
		Payload:     "{}",
		CreatedAt:   time.Now(),
	}); err != nil {
		h.t.Fatalf("WriteEvent(%s): %v", eventType, err)
	}
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

// ── AC: all seven counters appear in /metrics and increase on their event ──

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

	instanceID, _ := h.spawnFixture("nixos-config", "worker", "bwrap", "max")
	h.writeReviewVerdictEvent(review.EventReviewVerdictPass, "nixos-config", instanceID)
	h.writeReviewVerdictEvent(review.EventReviewVerdictPass, "nixos-config", instanceID)
	h.writeReviewVerdictEvent(review.EventReviewVerdictFail, "nixos-config", instanceID)

	passLabels := map[string]string{"verdict": "pass", "repo": "nixos-config", "agent_role": "worker", "profile": "max"}
	if got := counterValue(t, h, exporter.MetricReviewVerdictsTotal, passLabels); got != 2 {
		t.Errorf("%s%v = %v, want 2", exporter.MetricReviewVerdictsTotal, passLabels, got)
	}
	failLabels := map[string]string{"verdict": "fail", "repo": "nixos-config", "agent_role": "worker", "profile": "max"}
	if got := counterValue(t, h, exporter.MetricReviewVerdictsTotal, failLabels); got != 1 {
		t.Errorf("%s%v = %v, want 1", exporter.MetricReviewVerdictsTotal, failLabels, got)
	}
}

// ── AC (edge-case): a NULL profile_name on the reviewed worker is labelled
// "default", not "" ─────────────────────────────────────────────────────

func TestExporter_ReviewVerdictsTotalLabelsNullProfileAsDefault(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	instanceID, _ := h.spawnFixture("nixos-config", "worker", "bwrap", "")
	h.writeReviewVerdictEvent(review.EventReviewVerdictPass, "nixos-config", instanceID)

	labels := map[string]string{"verdict": "pass", "repo": "nixos-config", "agent_role": "worker", "profile": "default"}
	if got := counterValue(t, h, exporter.MetricReviewVerdictsTotal, labels); got != 1 {
		t.Errorf("%s%v = %v, want 1", exporter.MetricReviewVerdictsTotal, labels, got)
	}
	emptyLabels := map[string]string{"verdict": "pass", "repo": "nixos-config", "agent_role": "worker", "profile": ""}
	if got := counterValue(t, h, exporter.MetricReviewVerdictsTotal, emptyLabels); got != 0 {
		t.Errorf("%s%v = %v, want 0 (NULL profile_name must fold to \"default\", not empty string)", exporter.MetricReviewVerdictsTotal, emptyLabels, got)
	}
}

// ── AC (edge-case): an empty or whitespace-only repo on the reviewed worker
// is folded to the unknown-repo placeholder, never the empty string ──────

func TestExporter_ReviewVerdictsTotalFoldsEmptyRepoToUnknown(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	instanceID, _ := h.spawnFixture("", "worker", "bwrap", "max")
	h.writeReviewVerdictEvent(review.EventReviewVerdictPass, "", instanceID)

	labels := map[string]string{"verdict": "pass", "repo": "unknown", "agent_role": "worker", "profile": "max"}
	if got := counterValue(t, h, exporter.MetricReviewVerdictsTotal, labels); got != 1 {
		t.Errorf("%s%v = %v, want 1 (empty repo should fold to 'unknown')", exporter.MetricReviewVerdictsTotal, labels, got)
	}
}

// ── AC: the per-agent counter carries verdict, agent_role, and repo, and a
// completed round of five agents produces five increments ──────────────────

func TestExporter_ReviewAgentVerdictsTotalCountsEveryAgentOfARound(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	// One round: five review agents, each its own session and instance.
	roles := []string{"review-goal", "review-code", "review-security", "review-qa", "review-context"}
	for _, role := range roles {
		instanceID, _ := h.spawnFixture("nixos-config", role, "bwrap", "max")
		h.writeAgentVerdictEvent(review.EventReviewAgentVerdictPass, "nixos-config", instanceID)
	}

	var total float64
	for _, role := range roles {
		labels := map[string]string{"verdict": "pass", "agent_role": role, "repo": "nixos-config"}
		got := counterValue(t, h, exporter.MetricReviewAgentVerdictsTotal, labels)
		if got != 1 {
			t.Errorf("%s%v = %v, want 1", exporter.MetricReviewAgentVerdictsTotal, labels, got)
		}
		total += got
	}
	if total != 5 {
		t.Errorf("sum over the round = %v, want 5 (one increment per review agent)", total)
	}
}

// ── AC: a parseable FAIL records verdict="fail"; an IsError agent records
// verdict="error" and never "fail" ─────────────────────────────────────────

func TestExporter_ReviewAgentVerdictsTotalSeparatesFailFromError(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	failIID, _ := h.spawnFixture("nixos-config", "review-code", "bwrap", "max")
	h.writeAgentVerdictEvent(review.EventReviewAgentVerdictFail, "nixos-config", failIID)
	errIID, _ := h.spawnFixture("nixos-config", "review-security", "bwrap", "max")
	h.writeAgentVerdictEvent(review.EventReviewAgentVerdictError, "nixos-config", errIID)

	failLabels := map[string]string{"verdict": "fail", "agent_role": "review-code", "repo": "nixos-config"}
	if got := counterValue(t, h, exporter.MetricReviewAgentVerdictsTotal, failLabels); got != 1 {
		t.Errorf("%s%v = %v, want 1", exporter.MetricReviewAgentVerdictsTotal, failLabels, got)
	}
	errLabels := map[string]string{"verdict": "error", "agent_role": "review-security", "repo": "nixos-config"}
	if got := counterValue(t, h, exporter.MetricReviewAgentVerdictsTotal, errLabels); got != 1 {
		t.Errorf("%s%v = %v, want 1", exporter.MetricReviewAgentVerdictsTotal, errLabels, got)
	}
	// The infrastructure failure must never land in the fail bucket.
	errAsFail := map[string]string{"verdict": "fail", "agent_role": "review-security", "repo": "nixos-config"}
	if got := counterValue(t, h, exporter.MetricReviewAgentVerdictsTotal, errAsFail); got != 0 {
		t.Errorf("%s%v = %v, want 0 (an IsError agent must record verdict=\"error\", never \"fail\")", exporter.MetricReviewAgentVerdictsTotal, errAsFail, got)
	}
}

// ── AC: the round counter keeps its round-level behaviour, and the per-agent
// sum reconciles with it ───────────────────────────────────────────────────

func TestExporter_ReviewAgentVerdictsReconcileWithTheRoundCounter(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	// One round as both writers emit it: one round event on the worker's
	// instance, five per-agent events on the agents' own instances.
	workerIID, _ := h.spawnFixture("nixos-config", "worker", "bwrap", "max")
	h.writeReviewVerdictEvent(review.EventReviewVerdictFail, "nixos-config", workerIID)
	verdicts := map[string]string{
		"review-goal":     review.EventReviewAgentVerdictPass,
		"review-code":     review.EventReviewAgentVerdictFail,
		"review-security": review.EventReviewAgentVerdictPass,
		"review-qa":       review.EventReviewAgentVerdictPass,
		"review-context":  review.EventReviewAgentVerdictError,
	}
	for role, eventType := range verdicts {
		instanceID, _ := h.spawnFixture("nixos-config", role, "bwrap", "max")
		h.writeAgentVerdictEvent(eventType, "nixos-config", instanceID)
	}

	roundLabels := map[string]string{"verdict": "fail", "repo": "nixos-config", "agent_role": "worker", "profile": "max"}
	if got := counterValue(t, h, exporter.MetricReviewVerdictsTotal, roundLabels); got != 1 {
		t.Errorf("%s%v = %v, want 1 (the round counter keeps counting rounds)", exporter.MetricReviewVerdictsTotal, roundLabels, got)
	}

	exp := h.scrape(h.exp)
	var sum float64
	if family, ok := exp.Families[exporter.MetricReviewAgentVerdictsTotal]; ok {
		for _, s := range family.Samples {
			sum += s.Value
		}
	}
	if sum != 5 {
		t.Errorf("sum(%s) = %v, want 5 for one round of five agents", exporter.MetricReviewAgentVerdictsTotal, sum)
	}
}

// ── AC (edge-case): an empty or whitespace-only repo folds to the
// unknown-repo placeholder, never the empty string ─────────────────────────

func TestExporter_ReviewAgentVerdictsTotalFoldsBlankRepoToUnknown(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	emptyIID, _ := h.spawnFixture("", "review-goal", "bwrap", "max")
	h.writeAgentVerdictEvent(review.EventReviewAgentVerdictPass, "", emptyIID)
	blankIID, _ := h.spawnFixture("   ", "review-code", "bwrap", "max")
	h.writeAgentVerdictEvent(review.EventReviewAgentVerdictPass, "   ", blankIID)

	for _, role := range []string{"review-goal", "review-code"} {
		labels := map[string]string{"verdict": "pass", "agent_role": role, "repo": "unknown"}
		if got := counterValue(t, h, exporter.MetricReviewAgentVerdictsTotal, labels); got != 1 {
			t.Errorf("%s%v = %v, want 1 (a blank repo must fold to 'unknown')", exporter.MetricReviewAgentVerdictsTotal, labels, got)
		}
		emptyLabels := map[string]string{"verdict": "pass", "agent_role": role, "repo": ""}
		if got := counterValue(t, h, exporter.MetricReviewAgentVerdictsTotal, emptyLabels); got != 0 {
			t.Errorf("%s%v = %v, want 0 (never an empty repo label)", exporter.MetricReviewAgentVerdictsTotal, emptyLabels, got)
		}
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

// ── AC (edge-case): an empty repo is folded to "unknown" label ───────────

func TestExporter_LifecycleCountersFoldsEmptyRepoToUnknown(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	// Write events with empty repo and non-empty repo.
	h.spawnFixture("", "worker", "bwrap", "max")
	h.spawnFixture("nixos-config", "worker", "bwrap", "max")
	h.writeEventWithRepo("session.escalated", "", 0)
	h.writeEventWithRepo("session.escalated", "nixos-config", 0)
	h.writeEventWithRepo("doom_loop_detected", "", 0)
	h.writeEventWithRepo("doom_loop_detected", "nixos-config", 0)
	h.writeEventWithRepo("permission_denied", "", 0)
	h.writeEventWithRepo("permission_denied", "nixos-config", 0)

	// Empty repo should fold to "unknown".
	unknownLabels := map[string]string{"repo": "unknown", "agent_role": "worker", "isolation_mode": "bwrap", "profile": "max"}
	if got := counterValue(t, h, exporter.MetricSpawnsTotal, unknownLabels); got != 1 {
		t.Errorf("%s with empty repo = %v, want 1 (should fold to 'unknown')", exporter.MetricSpawnsTotal, got)
	}

	// Non-empty repo should not fold.
	nonEmptyLabels := map[string]string{"repo": "nixos-config", "agent_role": "worker", "isolation_mode": "bwrap", "profile": "max"}
	if got := counterValue(t, h, exporter.MetricSpawnsTotal, nonEmptyLabels); got != 1 {
		t.Errorf("%s with non-empty repo = %v, want 1", exporter.MetricSpawnsTotal, got)
	}

	// Check the other repo-labelled counters.
	escalationLabels := map[string]string{"repo": "unknown"}
	if got := counterValue(t, h, exporter.MetricEscalationsTotal, escalationLabels); got != 1 {
		t.Errorf("%s with empty repo = %v, want 1 (should fold to 'unknown')", exporter.MetricEscalationsTotal, got)
	}

	doomLabels := map[string]string{"repo": "unknown"}
	if got := counterValue(t, h, exporter.MetricDoomLoopsTotal, doomLabels); got != 1 {
		t.Errorf("%s with empty repo = %v, want 1 (should fold to 'unknown')", exporter.MetricDoomLoopsTotal, got)
	}

	permissionLabels := map[string]string{"repo": "unknown"}
	if got := counterValue(t, h, exporter.MetricPermissionDeniedTotal, permissionLabels); got != 1 {
		t.Errorf("%s with empty repo = %v, want 1 (should fold to 'unknown')", exporter.MetricPermissionDeniedTotal, got)
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
	instanceID, _ := h.spawnFixture("nixos-config", "worker", "bwrap", "max")
	h.writeReviewVerdictEvent(review.EventReviewVerdictPass, "nixos-config", instanceID)
	agentIID, _ := h.spawnFixture("nixos-config", "review-qa", "bwrap", "max")
	h.writeAgentVerdictEvent(review.EventReviewAgentVerdictFail, "nixos-config", agentIID)

	labels := map[string]string{"repo": "nixos-config"}
	spawnLabels := map[string]string{"repo": "nixos-config", "agent_role": "worker", "isolation_mode": "bwrap", "profile": "max"}
	verdictLabels := map[string]string{"verdict": "pass", "repo": "nixos-config", "agent_role": "worker", "profile": "max"}
	agentVerdictLabels := map[string]string{"verdict": "fail", "agent_role": "review-qa", "repo": "nixos-config"}

	before := struct {
		doomLoops, permissionDenied, escalations, verdicts, agentVerdicts, spawns float64
	}{
		doomLoops:        counterValue(t, h, exporter.MetricDoomLoopsTotal, labels),
		permissionDenied: counterValue(t, h, exporter.MetricPermissionDeniedTotal, labels),
		escalations:      counterValue(t, h, exporter.MetricEscalationsTotal, labels),
		verdicts:         counterValue(t, h, exporter.MetricReviewVerdictsTotal, verdictLabels),
		agentVerdicts:    counterValue(t, h, exporter.MetricReviewAgentVerdictsTotal, agentVerdictLabels),
		spawns:           counterValue(t, h, exporter.MetricSpawnsTotal, spawnLabels),
	}
	if before.doomLoops != 1 || before.permissionDenied != 1 || before.escalations != 1 || before.verdicts != 1 || before.agentVerdicts != 1 || before.spawns != 1 {
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
	if got := restartedValue(t, restarted, exporter.MetricReviewAgentVerdictsTotal, agentVerdictLabels); got != before.agentVerdicts {
		t.Errorf("%s = %v after restart, want %v", exporter.MetricReviewAgentVerdictsTotal, got, before.agentVerdicts)
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
	h.writeEvent(review.EventReviewAgentVerdictPass, 0)

	banned := []string{"session_name", "instance_id", "issue_ref"}
	exp := h.scrape(h.exp)
	for _, name := range []string{
		exporter.MetricSpawnsTotal, exporter.MetricSessionsEndedTotal, exporter.MetricReviewVerdictsTotal,
		exporter.MetricReviewAgentVerdictsTotal,
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

// ── #3187: increase() must see the first spawn of a new series ───────────

var firstSpawnLabels = map[string]string{"repo": "nixos-config", "agent_role": "worker", "isolation_mode": "bwrap", "profile": "max"}

func spawnSample(t *testing.T, h *harness, e *exporter.Exporter) (float64, bool) {
	t.Helper()
	v, ok := h.scrape(e).Value(exporter.MetricSpawnsTotal, firstSpawnLabels)
	return v, ok
}

func TestExporter_SpawnsTotalNewSeriesStepsFromZero(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.scrape(h.exp) // first refresh: the exporter is now live

	if _, ok := spawnSample(t, h, h.exp); ok {
		t.Fatal("series exists before any spawn: the exporter must not pre-create series")
	}
	h.spawnFixture("nixos-config", "worker", "bwrap", "max")
	if v, ok := spawnSample(t, h, h.exp); !ok || v != 0 {
		t.Fatalf("first sample = %v (exists %v), want 0", v, ok)
	}
	if v, _ := spawnSample(t, h, h.exp); v != 1 {
		t.Fatalf("second sample = %v, want 1", v)
	}
	h.spawnFixture("nixos-config", "worker", "bwrap", "max")
	if v, _ := spawnSample(t, h, h.exp); v != 2 {
		t.Fatalf("third sample = %v, want 2", v)
	}
}

func TestExporter_SpawnsTotalBacklogAfterRestartAddsNoZeroStep(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.scrape(h.exp)
	if err := h.exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Spawns that happen while the exporter is down (a host back after a
	// long absence) are replayed on restart. They must show their value at
	// once, so increase() does not count them as inside the window.
	h.spawnFixture("nixos-config", "worker", "bwrap", "max")
	h.spawnFixture("nixos-config", "worker", "bwrap", "max")
	restarted := h.newExporter()
	h.start(restarted)
	if v, _ := spawnSample(t, h, restarted); v != 2 {
		t.Fatalf("first sample after replay = %v, want 2", v)
	}
}

func TestExporter_SpawnsTotalNeverDecreasesAcrossRestart(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.scrape(h.exp)
	h.spawnFixture("nixos-config", "worker", "bwrap", "max")
	first, _ := spawnSample(t, h, h.exp) // 0
	// Restart before the series was ever exposed at its real value.
	if err := h.exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	restarted := h.newExporter()
	h.start(restarted)
	after, ok := spawnSample(t, h, restarted)
	if !ok || after < first || after != 1 {
		t.Fatalf("after restart = %v (exists %v), before = %v, want 1 and no decrease", after, ok, first)
	}
}
