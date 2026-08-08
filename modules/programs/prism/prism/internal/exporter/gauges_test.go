package exporter_test

// Tests for the four #2702 state gauges. These reuse the harness from
// exporter_test.go; none of them touches the tail cursor, since a gauge is
// recomputed on every scrape (#2699 section 4).

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/exporter"
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
