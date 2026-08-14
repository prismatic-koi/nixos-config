package exporter_test

// Tests for the four #2702 state gauges. These reuse the harness from
// exporter_test.go; none of them touches the tail cursor, since a gauge is
// recomputed on every scrape (#2699 section 4).

import (
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/exporter"
	"github.com/prismatic-koi/prism/internal/sidecar"
)

func gaugeValue(t *testing.T, h *harness, metric string, labels map[string]string) float64 {
	t.Helper()
	exp := h.scrape(h.exp)
	v, _ := exp.Value(metric, labels)
	return v
}

// ── AC: all four gauges appear in /metrics and parse as Prometheus text ───

func TestExporter_StateGaugesAppearInMetrics(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	exp := h.scrape(h.exp)
	for _, name := range []string{
		exporter.MetricSessionsActive,
		exporter.MetricMergeQueueDepth,
		exporter.MetricMergesByStatus,
		exporter.MetricBusMessagesPending,
	} {
		exp.Family(t, name)
	}
}

// ── AC (edge case): an empty database exposes no gauge series and does
// not error ─────────────────────────────────────────────────────────────

func TestExporter_StateGaugesAreEmptyOnAnEmptyDatabase(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	exp := h.scrape(h.exp)
	for _, name := range []string{
		exporter.MetricSessionsActive,
		exporter.MetricMergeQueueDepth,
		exporter.MetricMergesByStatus,
		exporter.MetricBusMessagesPending,
	} {
		f := exp.Family(t, name)
		if len(f.Samples) != 0 {
			t.Errorf("%s has %d samples on an empty database, want 0: %+v", name, len(f.Samples), f.Samples)
		}
	}
	if h.logBuf.Len() != 0 {
		t.Errorf("an empty database logged something; the exporter must not error: %s", h.logBuf.String())
	}
}

// ── AC: prism_sessions_active reflects a session starting and ending,
// within one scrape interval ────────────────────────────────────────────

func TestExporter_SessionsActiveReflectsStartAndEnd(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	instanceID, sessionName := h.spawnFixture("nixos-config", "worker", "bwrap", "")
	if err := h.writeDB.UpsertStatusSeedRootAgentName(
		sessionName, "nixos-config", "/tmp/prism-test", "active", nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if err := h.writeDB.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}

	labels := map[string]string{"repo": "nixos-config", "agent_role": "worker", "state": "active"}
	if got := gaugeValue(t, h, exporter.MetricSessionsActive, labels); got != 1 {
		t.Fatalf("%s%v = %v while the session is active, want 1", exporter.MetricSessionsActive, labels, got)
	}

	if err := h.writeDB.SetEnded(sessionName); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	if got := gaugeValue(t, h, exporter.MetricSessionsActive, labels); got != 0 {
		t.Errorf("%s%v = %v after the session ended, want 0 — a gauge must reflect the row leaving, not just arriving", exporter.MetricSessionsActive, labels, got)
	}
}

// ── AC: prism_merge_queue_depth reflects a PR being enqueued and leaving
// the queue ────────────────────────────────────────────────────────────

func TestExporter_MergeQueueDepthReflectsEnqueueAndLeave(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	repo := "nixos-config"
	labels := map[string]string{"repo": repo}
	if got := gaugeValue(t, h, exporter.MetricMergeQueueDepth, labels); got != 0 {
		t.Fatalf("%s%v = %v before any enqueue, want 0", exporter.MetricMergeQueueDepth, labels, got)
	}

	if _, err := h.writeDB.EnqueueMerge(101, repo, "prism-test@a", "instance-a", nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	if got := gaugeValue(t, h, exporter.MetricMergeQueueDepth, labels); got != 1 {
		t.Fatalf("%s%v = %v after one enqueue, want 1", exporter.MetricMergeQueueDepth, labels, got)
	}

	if _, err := h.writeDB.EnqueueMerge(102, repo, "prism-test@b", "instance-b", nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	if got := gaugeValue(t, h, exporter.MetricMergeQueueDepth, labels); got != 2 {
		t.Fatalf("%s%v = %v after two enqueues, want 2", exporter.MetricMergeQueueDepth, labels, got)
	}

	if err := h.writeDB.TerminateMerge(101, repo, "merged", ""); err != nil {
		t.Fatalf("TerminateMerge: %v", err)
	}
	if got := gaugeValue(t, h, exporter.MetricMergeQueueDepth, labels); got != 1 {
		t.Errorf("%s%v = %v after the PR left the queue, want 1", exporter.MetricMergeQueueDepth, labels, got)
	}

	// prism_merges_by_status counts every status, watching and terminal.
	watchingLabels := map[string]string{"repo": repo, "status": "watching"}
	if got := gaugeValue(t, h, exporter.MetricMergesByStatus, watchingLabels); got != 1 {
		t.Errorf("%s%v = %v, want 1", exporter.MetricMergesByStatus, watchingLabels, got)
	}
	mergedLabels := map[string]string{"repo": repo, "status": "merged"}
	if got := gaugeValue(t, h, exporter.MetricMergesByStatus, mergedLabels); got != 1 {
		t.Errorf("%s%v = %v, want 1", exporter.MetricMergesByStatus, mergedLabels, got)
	}
}

// ── AC: prism_bus_messages_pending reflects delivery ───────────────────

func TestExporter_BusMessagesPendingReflectsDelivery(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	repo := "nixos-config"
	labels := map[string]string{"repo": repo}
	if err := h.writeDB.WriteBusMessage(db.BusMessage{
		ID:          "bus-1",
		FromSession: "prism-test@a",
		ToSession:   "prism-test@b",
		Repo:        repo,
		Text:        "hello",
	}); err != nil {
		t.Fatalf("WriteBusMessage: %v", err)
	}
	if got := gaugeValue(t, h, exporter.MetricBusMessagesPending, labels); got != 1 {
		t.Fatalf("%s%v = %v with one undelivered message, want 1", exporter.MetricBusMessagesPending, labels, got)
	}

	if err := h.writeDB.WriteBusMessageDelivered(db.BusMessage{
		ID:          "bus-2",
		FromSession: "prism-test@a",
		ToSession:   "prism-test@b",
		Repo:        repo,
		Text:        "already delivered",
	}); err != nil {
		t.Fatalf("WriteBusMessageDelivered: %v", err)
	}
	// The already-delivered message must not count toward the backlog.
	if got := gaugeValue(t, h, exporter.MetricBusMessagesPending, labels); got != 1 {
		t.Errorf("%s%v = %v after a delivered-on-write message, want 1", exporter.MetricBusMessagesPending, labels, got)
	}
}

// ── AC (edge-case): a state value outside the pinned set does not drop the
// whole scrape, and is logged once rather than per scrape ────────────────

func TestExporter_SessionsActiveFoldsUnknownStateAndLogsOnce(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	instanceID, sessionName := h.spawnFixture("nixos-config", "worker", "bwrap", "")
	// "starting" is not one of internal/agent/agent.go's AgentState
	// constants — a legal write, since checkTransition is advisory only.
	if err := h.writeDB.UpsertStatusSeedRootAgentName(
		sessionName, "nixos-config", "/tmp/prism-test", "starting", nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if err := h.writeDB.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}

	labels := map[string]string{"repo": "nixos-config", "agent_role": "worker", "state": "other"}
	if got := gaugeValue(t, h, exporter.MetricSessionsActive, labels); got != 1 {
		t.Fatalf("%s%v = %v, want 1 — an unpinned state must fold to \"other\", not be dropped or exposed verbatim", exporter.MetricSessionsActive, labels, got)
	}
	rawLabels := map[string]string{"repo": "nixos-config", "agent_role": "worker", "state": "starting"}
	if got := gaugeValue(t, h, exporter.MetricSessionsActive, rawLabels); got != 0 {
		t.Errorf("%s%v = %v, want 0 — the raw unpinned value must never be exposed as a label", exporter.MetricSessionsActive, rawLabels, got)
	}

	if n := countOccurrences(h.logBuf.String(), "outside the pinned set"); n != 1 {
		t.Errorf("log mentions the unpinned state %d times across two scrapes, want exactly 1 (logged once, not per scrape)", n)
	}

	// A second scrape with the same unpinned row must not log again.
	h.scrape(h.exp)
	if n := countOccurrences(h.logBuf.String(), "outside the pinned set"); n != 1 {
		t.Errorf("log mentions the unpinned state %d times after a second scrape, want exactly 1", n)
	}
}

func countOccurrences(haystack, needle string) int {
	n := 0
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			n++
		}
	}
	return n
}

// ── #2708: prism_sidecars_live / prism_sidecars_stale ──────────────────────

// setLastSeen backdates a session's agent_status.last_seen by age, so the
// test can put it on either side of exporter.SidecarStaleThreshold without
// racing the clock.
func setLastSeen(t *testing.T, h *harness, sessionName string, age time.Duration) {
	t.Helper()
	ms := time.Now().Add(-age).UnixMilli()
	if _, err := h.raw().Exec(`UPDATE agent_status SET last_seen = ? WHERE session_name = ?`, ms, sessionName); err != nil {
		t.Fatalf("setLastSeen: %v", err)
	}
}

func TestExporter_SidecarLivenessGaugesAppearInMetrics(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	exp := h.scrape(h.exp)
	for _, name := range []string{exporter.MetricSidecarsLive, exporter.MetricSidecarsStale} {
		exp.Family(t, name)
	}
}

// ── AC (edge-case): an empty database produces no series and no error ─────

func TestExporter_SidecarLivenessGaugesAreEmptyOnAnEmptyDatabase(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	exp := h.scrape(h.exp)
	for _, name := range []string{exporter.MetricSidecarsLive, exporter.MetricSidecarsStale} {
		f := exp.Family(t, name)
		if len(f.Samples) != 0 {
			t.Errorf("%s has %d samples on an empty database, want 0: %+v", name, len(f.Samples), f.Samples)
		}
	}
	if h.logBuf.Len() != 0 {
		t.Errorf("an empty database logged something; the exporter must not error: %s", h.logBuf.String())
	}
}

// ── AC (functional): a fresh last_seen counts as live, a stale one counts
// as dead-or-wedged ─────────────────────────────────────────────────────

func TestExporter_SidecarLivenessReflectsLastSeen(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	repo := "nixos-config"
	liveInstance, liveSession := h.spawnFixture(repo, "worker", "bwrap", "")
	if err := h.writeDB.UpsertStatusSeedRootAgentName(
		liveSession, repo, "/tmp/prism-test", "active", nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if err := h.writeDB.SetInstanceID(liveSession, liveInstance); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	setLastSeen(t, h, liveSession, 30*time.Second) // well inside the threshold

	staleInstance, staleSession := h.spawnFixture(repo, "worker", "bwrap", "")
	if err := h.writeDB.UpsertStatusSeedRootAgentName(
		staleSession, repo, "/tmp/prism-test", "active", nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if err := h.writeDB.SetInstanceID(staleSession, staleInstance); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	setLastSeen(t, h, staleSession, exporter.SidecarStaleThreshold+time.Minute) // well past the threshold

	labels := map[string]string{"repo": repo}
	if got := gaugeValue(t, h, exporter.MetricSidecarsLive, labels); got != 1 {
		t.Errorf("%s%v = %v, want 1 (one fresh session)", exporter.MetricSidecarsLive, labels, got)
	}
	if got := gaugeValue(t, h, exporter.MetricSidecarsStale, labels); got != 1 {
		t.Errorf("%s%v = %v, want 1 (one stale session)", exporter.MetricSidecarsStale, labels, got)
	}
}

// ── AC (edge-case): a session with ended_at set is counted in neither
// gauge ───────────────────────────────────────────────────────────────

func TestExporter_SidecarLivenessExcludesEndedSessions(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	repo := "nixos-config"
	instanceID, sessionName := h.spawnFixture(repo, "worker", "bwrap", "")
	if err := h.writeDB.UpsertStatusSeedRootAgentName(
		sessionName, repo, "/tmp/prism-test", "active", nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if err := h.writeDB.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	setLastSeen(t, h, sessionName, 30*time.Second)

	labels := map[string]string{"repo": repo}
	if got := gaugeValue(t, h, exporter.MetricSidecarsLive, labels); got != 1 {
		t.Fatalf("%s%v = %v before ending, want 1", exporter.MetricSidecarsLive, labels, got)
	}

	if err := h.writeDB.SetEnded(sessionName); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	if got := gaugeValue(t, h, exporter.MetricSidecarsLive, labels); got != 0 {
		t.Errorf("%s%v = %v after ending, want 0 — an ended session must not count as live", exporter.MetricSidecarsLive, labels, got)
	}
	if got := gaugeValue(t, h, exporter.MetricSidecarsStale, labels); got != 0 {
		t.Errorf("%s%v = %v after ending, want 0 — an ended session must not count as stale either", exporter.MetricSidecarsStale, labels, got)
	}
}

// ── AC (edge-case): a NULL or zero last_seen is handled without panicking
// and without being miscounted as live ──────────────────────────────────

func TestExporter_SidecarLivenessTreatsZeroLastSeenAsStaleNotLive(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	repo := "nixos-config"
	instanceID, sessionName := h.spawnFixture(repo, "worker", "bwrap", "")
	if err := h.writeDB.UpsertStatusSeedRootAgentName(
		sessionName, repo, "/tmp/prism-test", "active", nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if err := h.writeDB.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	// agent_status.last_seen is NOT NULL, so the reachable zero-evidence case
	// is the literal value 0 (e.g. a row seeded before any WriteEvent call),
	// not SQL NULL. Force it explicitly rather than relying on whatever the
	// seed path defaulted it to.
	if _, err := h.raw().Exec(`UPDATE agent_status SET last_seen = 0 WHERE session_name = ?`, sessionName); err != nil {
		t.Fatalf("UPDATE last_seen = 0: %v", err)
	}

	labels := map[string]string{"repo": repo}
	if got := gaugeValue(t, h, exporter.MetricSidecarsLive, labels); got != 0 {
		t.Errorf("%s%v = %v with last_seen = 0, want 0 — no positive evidence of liveness must never count as live", exporter.MetricSidecarsLive, labels, got)
	}
	if got := gaugeValue(t, h, exporter.MetricSidecarsStale, labels); got != 1 {
		t.Errorf("%s%v = %v with last_seen = 0, want 1", exporter.MetricSidecarsStale, labels, got)
	}
}

// seedSidecarSession spawns a session and writes an agent_status row for it
// in the given state, with last_seen backdated by age. A small helper shared
// by the two quiet-by-design tests below.
func seedSidecarSession(t *testing.T, h *harness, repo, state string, age time.Duration) string {
	t.Helper()
	instanceID, sessionName := h.spawnFixture(repo, "worker", "bwrap", "")
	if err := h.writeDB.UpsertStatusSeedRootAgentName(
		sessionName, repo, "/tmp/prism-test", state, nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if err := h.writeDB.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	setLastSeen(t, h, sessionName, age)
	return sessionName
}

// ── AC (edge-case, round-1 review finding): a session in a quiet-by-design
// state (idle, waiting, escalated) with a last_seen well past the
// threshold is NOT counted stale. This is the exact shape a real fleet
// session ("obsidian") surfaced against navi: ended_at IS NULL, state
// idle, last_seen 15 minutes stale, sidecar perfectly healthy — nobody was
// talking to it. ───────────────────────────────────────────────────────

func TestExporter_SidecarLivenessExcludesQuietByDesignStates(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	repo := "nixos-config"
	for _, state := range []string{"idle", "waiting", "escalated"} {
		// Well past SidecarStaleThreshold, matching the real "obsidian" shape.
		seedSidecarSession(t, h, repo, state, exporter.SidecarStaleThreshold+time.Minute)
	}

	labels := map[string]string{"repo": repo}
	if got := gaugeValue(t, h, exporter.MetricSidecarsStale, labels); got != 0 {
		t.Errorf("%s%v = %v, want 0 — a session quiet by design (idle/waiting/escalated) must never be counted stale", exporter.MetricSidecarsStale, labels, got)
	}
	if got := gaugeValue(t, h, exporter.MetricSidecarsLive, labels); got != 0 {
		t.Errorf("%s%v = %v, want 0 — a quiet-by-design session is excluded from both gauges, not counted live either", exporter.MetricSidecarsLive, labels, got)
	}
}

// ── AC (functional, inverse of the above): a session in an
// activity-expected state (active) with a last_seen past the threshold IS
// counted stale — this is the whole point of the metric, and the fix above
// must not blunt it. ─────────────────────────────────────────────────────

func TestExporter_SidecarLivenessCountsStaleActiveSession(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	repo := "nixos-config"
	seedSidecarSession(t, h, repo, "active", exporter.SidecarStaleThreshold+time.Minute)

	labels := map[string]string{"repo": repo}
	if got := gaugeValue(t, h, exporter.MetricSidecarsStale, labels); got != 1 {
		t.Errorf("%s%v = %v, want 1 — a silent active session past the threshold is the exact dead-or-wedged case this gauge exists to catch", exporter.MetricSidecarsStale, labels, got)
	}
	if got := gaugeValue(t, h, exporter.MetricSidecarsLive, labels); got != 0 {
		t.Errorf("%s%v = %v, want 0", exporter.MetricSidecarsLive, labels, got)
	}
}

// ── AC (functional): compacting and reviewing are activity-expected too,
// same as active — silence in either state past the threshold counts
// stale. ────────────────────────────────────────────────────────────────

func TestExporter_SidecarLivenessCountsStaleCompactingAndReviewing(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	repo := "nixos-config"
	seedSidecarSession(t, h, repo, "compacting", exporter.SidecarStaleThreshold+time.Minute)
	seedSidecarSession(t, h, repo, "reviewing", exporter.SidecarStaleThreshold+time.Minute)

	labels := map[string]string{"repo": repo}
	if got := gaugeValue(t, h, exporter.MetricSidecarsStale, labels); got != 2 {
		t.Errorf("%s%v = %v, want 2 (compacting + reviewing)", exporter.MetricSidecarsStale, labels, got)
	}
}

// ── AC (anti-drift guard, mirrors
// TestReviewAgentActivityWindow_MatchesWatchdog in internal/review):
// SidecarStaleThreshold must equal the sidecar's own inactivity-watchdog
// timeout, or the rationale in its doc comment ("this is the number prism
// itself already trusts") stops being true. ─────────────────────────────

func TestSidecarStaleThreshold_MatchesReviewWatchdog(t *testing.T) {
	if got, want := exporter.SidecarStaleThreshold, sidecar.DefaultReviewAgentInactivityTimeout; got != want {
		t.Errorf("exporter.SidecarStaleThreshold = %v, want %v (sidecar.DefaultReviewAgentInactivityTimeout) — the two must not drift", got, want)
	}
}

// ── #2764 AC (functional): empty or whitespace-only repo is folded to
// "unknown" placeholder. The session is not dropped — it still appears
// in the gauge.

func TestExporter_EmptyRepoLabelFoldsToUnknown(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	// Create a session with an empty repo.
	emptyRepoInstance, emptyRepoSession := h.spawnFixture("", "worker", "bwrap", "")
	if err := h.writeDB.UpsertStatusSeedRootAgentName(
		emptyRepoSession, "", "/tmp/prism-test", "active", nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if err := h.writeDB.SetInstanceID(emptyRepoSession, emptyRepoInstance); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}

	// Create a session with a whitespace-only repo.
	whitespaceInstance, whitespaceSession := h.spawnFixture("   ", "worker", "bwrap", "")
	if err := h.writeDB.UpsertStatusSeedRootAgentName(
		whitespaceSession, "   ", "/tmp/prism-test", "active", nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if err := h.writeDB.SetInstanceID(whitespaceSession, whitespaceInstance); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}

	// Create a session with a non-empty repo to verify it is not folded.
	normalInstance, normalSession := h.spawnFixture("nixos-config", "worker", "bwrap", "")
	if err := h.writeDB.UpsertStatusSeedRootAgentName(
		normalSession, "nixos-config", "/tmp/prism-test", "active", nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if err := h.writeDB.SetInstanceID(normalSession, normalInstance); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}

	// Test prism_sessions_active: both empty and whitespace-only repos fold to "unknown"
	unknownLabels := map[string]string{"repo": "unknown", "agent_role": "worker", "state": "active"}
	if got := gaugeValue(t, h, exporter.MetricSessionsActive, unknownLabels); got != 2 {
		t.Errorf("%s%v = %v, want 2 (empty + whitespace-only folded to 'unknown')",
			exporter.MetricSessionsActive, unknownLabels, got)
	}

	// Verify that the normal repo is not folded.
	normalLabels := map[string]string{"repo": "nixos-config", "agent_role": "worker", "state": "active"}
	if got := gaugeValue(t, h, exporter.MetricSessionsActive, normalLabels); got != 1 {
		t.Errorf("%s%v = %v, want 1 (non-empty repo must not be folded)",
			exporter.MetricSessionsActive, normalLabels, got)
	}

	// Verify raw empty/whitespace values are never exposed as labels.
	emptyLabels := map[string]string{"repo": "", "agent_role": "worker", "state": "active"}
	if got := gaugeValue(t, h, exporter.MetricSessionsActive, emptyLabels); got != 0 {
		t.Errorf("%s%v = %v, want 0 — raw empty repo must never be exposed as a label",
			exporter.MetricSessionsActive, emptyLabels, got)
	}
	whitespaceLabels := map[string]string{"repo": "   ", "agent_role": "worker", "state": "active"}
	if got := gaugeValue(t, h, exporter.MetricSessionsActive, whitespaceLabels); got != 0 {
		t.Errorf("%s%v = %v, want 0 — raw whitespace-only repo must never be exposed as a label",
			exporter.MetricSessionsActive, whitespaceLabels, got)
	}
}

// ── #2764 AC (functional): empty or whitespace-only repo is folded to
// "unknown" on all five repo-labelled gauges.

func TestExporter_EmptyRepoFoldsOnAllGauges(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	// Write data to all five gauges with empty repo.
	if _, err := h.writeDB.EnqueueMerge(201, "", "prism-test@empty", "instance-empty", nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	if err := h.writeDB.WriteBusMessage(db.BusMessage{
		ID:          "bus-empty",
		FromSession: "prism-test@a",
		ToSession:   "prism-test@b",
		Repo:        "",
		Text:        "test",
	}); err != nil {
		t.Fatalf("WriteBusMessage: %v", err)
	}

	emptyRepoInstance, emptyRepoSession := h.spawnFixture("", "worker", "bwrap", "")
	if err := h.writeDB.UpsertStatusSeedRootAgentName(
		emptyRepoSession, "", "/tmp/prism-test", "active", nil, nil, "worker", "pi", "bwrap",
	); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if err := h.writeDB.SetInstanceID(emptyRepoSession, emptyRepoInstance); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	setLastSeen(t, h, emptyRepoSession, 30*time.Second) // make it live

	unknownLabels := map[string]string{"repo": "unknown"}

	// Test prism_merge_queue_depth{repo="unknown"}
	if got := gaugeValue(t, h, exporter.MetricMergeQueueDepth, unknownLabels); got != 1 {
		t.Errorf("%s%v = %v, want 1", exporter.MetricMergeQueueDepth, unknownLabels, got)
	}

	// Test prism_merges_by_status{repo="unknown"}
	mergeStatusLabels := map[string]string{"repo": "unknown", "status": "watching"}
	if got := gaugeValue(t, h, exporter.MetricMergesByStatus, mergeStatusLabels); got != 1 {
		t.Errorf("%s%v = %v, want 1", exporter.MetricMergesByStatus, mergeStatusLabels, got)
	}

	// Test prism_bus_messages_pending{repo="unknown"}
	if got := gaugeValue(t, h, exporter.MetricBusMessagesPending, unknownLabels); got != 1 {
		t.Errorf("%s%v = %v, want 1", exporter.MetricBusMessagesPending, unknownLabels, got)
	}

	// Test prism_sidecars_live{repo="unknown"}
	if got := gaugeValue(t, h, exporter.MetricSidecarsLive, unknownLabels); got != 1 {
		t.Errorf("%s%v = %v, want 1", exporter.MetricSidecarsLive, unknownLabels, got)
	}

	// Test prism_sidecars_stale{repo="unknown"} (should be 0 since last_seen is fresh)
	if got := gaugeValue(t, h, exporter.MetricSidecarsStale, unknownLabels); got != 0 {
		t.Errorf("%s%v = %v, want 0 (session is not stale)", exporter.MetricSidecarsStale, unknownLabels, got)
	}
}
