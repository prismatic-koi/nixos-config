package exporter_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/exporter"
	"github.com/prismatic-koi/prism/internal/metrics/metricstest"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// pruneHorizon matches the production Prune window used on the event path
// (internal/db/maintenance.go, called from cmd/event.go).
const pruneHorizon = 90 * 24 * time.Hour

type harness struct {
	t         *testing.T
	dir       string
	dbPath    string
	statePath string
	writeDB   *db.DB
	logBuf    *bytes.Buffer
	exp       *exporter.Exporter
}

// newHarness creates a fresh prism.db and an exporter over it. The exporter
// is NOT started — call start, so a test can inspect the pre-start state.
func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		t:         t,
		dir:       dir,
		dbPath:    filepath.Join(dir, "prism.db"),
		statePath: filepath.Join(dir, "exporter-state.json"),
		logBuf:    &bytes.Buffer{},
	}
	h.writeDB = sidecartest.OpenDB(t, h.dbPath)
	t.Cleanup(func() { _ = h.writeDB.Close() })
	h.exp = h.newExporter()
	return h
}

// newExporter builds a second exporter over the same database and state
// file — the restart path.
func (h *harness) newExporter() *exporter.Exporter {
	h.t.Helper()
	e, err := exporter.New(exporter.Config{
		DBPath:     h.dbPath,
		StatePath:  h.statePath,
		ListenAddr: "127.0.0.1:0",
		Logger:     log.New(h.logBuf, "", 0),
		Version:    "test-version",
	})
	if err != nil {
		h.t.Fatalf("exporter.New: %v", err)
	}
	h.t.Cleanup(func() { _ = e.Close() })
	return e
}

func (h *harness) start(e *exporter.Exporter) {
	h.t.Helper()
	if err := e.Start(context.Background()); err != nil {
		h.t.Fatalf("Start: %v", err)
	}
}

// writeEvent inserts one agent_events row with the given type and age.
func (h *harness) writeEvent(eventType string, age time.Duration) {
	h.t.Helper()
	err := h.writeDB.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: "prism-test@exporter",
		Repo:        "nixos-config",
		Worktree:    "/tmp/prism-test",
		Type:        eventType,
		Payload:     `{"note":"test"}`,
		CreatedAt:   time.Now().Add(-age),
	})
	if err != nil {
		h.t.Fatalf("WriteEvent(%s): %v", eventType, err)
	}
}

func (h *harness) maxRowID() int64 {
	h.t.Helper()
	var maxID int64
	if err := h.writeDB.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM agent_events`).Scan(&maxID); err != nil {
		h.t.Fatalf("read max rowid: %v", err)
	}
	return maxID
}

func (h *harness) rowCount() int64 {
	h.t.Helper()
	var n int64
	if err := h.writeDB.QueryRow(`SELECT COUNT(*) FROM agent_events`).Scan(&n); err != nil {
		h.t.Fatalf("count rows: %v", err)
	}
	return n
}

// scrape drives one real HTTP GET of /metrics through the daemon's handler
// and returns the parsed exposition.
func (h *harness) scrape(e *exporter.Exporter) *metricstest.Exposition {
	h.t.Helper()
	srv := httptest.NewServer(e.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + exporter.MetricsPath)
	if err != nil {
		h.t.Fatalf("GET %s: %v", exporter.MetricsPath, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET %s = %d, want 200. Body: %s", exporter.MetricsPath, resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		h.t.Errorf("Content-Type = %q, want a text/plain exposition type", ct)
	}
	return metricstest.MustParse(h.t, string(body))
}

func eventsTotal(t *testing.T, exp *metricstest.Exposition, eventType string) float64 {
	t.Helper()
	v, ok := exp.Value(exporter.MetricAgentEventsTotal, map[string]string{"type": eventType})
	if !ok {
		return 0
	}
	return v
}

// ── AC: serves /metrics, and the output parses as Prometheus text ──────────

func TestExporter_ServesParseablePrometheusText(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.writeEvent("tool_call", 0)

	exp := h.scrape(h.exp)

	build := exp.Family(t, exporter.MetricBuildInfo)
	if build.Type != "gauge" {
		t.Errorf("%s is a %s, want gauge", exporter.MetricBuildInfo, build.Type)
	}
	if len(build.Samples) != 1 {
		t.Fatalf("%s has %d samples, want 1", exporter.MetricBuildInfo, len(build.Samples))
	}
	if got := build.Samples[0].Labels["version"]; got != "test-version" {
		t.Errorf("build_info version label = %q, want test-version", got)
	}
	if got := build.Samples[0].Labels["go_version"]; !strings.HasPrefix(got, "go1.") {
		t.Errorf("build_info go_version label = %q, want a go1.x string", got)
	}
	if build.Samples[0].Value != 1 {
		t.Errorf("build_info value = %v, want 1", build.Samples[0].Value)
	}

	events := exp.Family(t, exporter.MetricAgentEventsTotal)
	if events.Type != "counter" {
		t.Errorf("%s is a %s, want counter", exporter.MetricAgentEventsTotal, events.Type)
	}
	if events.Help == "" {
		t.Errorf("%s has no HELP text", exporter.MetricAgentEventsTotal)
	}
}

// #2700 shipped exactly two metrics; #2703 adds the six lifecycle and
// outcome counters on top. #2702 and #2706 own the rest.
func TestExporter_ShipsExactlyTheEightSpecifiedMetrics(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.writeEvent("tool_call", 0)

	exp := h.scrape(h.exp)
	want := []string{
		exporter.MetricAgentEventsTotal,
		exporter.MetricBuildInfo,
		exporter.MetricDoomLoopsTotal,
		exporter.MetricEscalationsTotal,
		exporter.MetricPermissionDeniedTotal,
		exporter.MetricReviewVerdictsTotal,
		exporter.MetricSessionsEndedTotal,
		exporter.MetricSpawnsTotal,
	}
	got := exp.FamilyNames()
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("exposed metric families %v, want exactly %v", got, want)
	}
}

// ── AC: the counter increases when new agent_events rows are written ───────

func TestExporter_CounterIncreasesWithNewRows(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	if got := eventsTotal(t, h.scrape(h.exp), "tool_call"); got != 0 {
		t.Fatalf("counter starts at %v, want 0", got)
	}

	h.writeEvent("tool_call", 0)
	h.writeEvent("tool_call", 0)
	h.writeEvent("turn_start", 0)

	exp := h.scrape(h.exp)
	if got := eventsTotal(t, exp, "tool_call"); got != 2 {
		t.Errorf("%s{type=tool_call} = %v, want 2", exporter.MetricAgentEventsTotal, got)
	}
	if got := eventsTotal(t, exp, "turn_start"); got != 1 {
		t.Errorf("%s{type=turn_start} = %v, want 1", exporter.MetricAgentEventsTotal, got)
	}

	h.writeEvent("tool_call", 0)
	if got := eventsTotal(t, h.scrape(h.exp), "tool_call"); got != 3 {
		t.Errorf("%s{type=tool_call} = %v after one more row, want 3", exporter.MetricAgentEventsTotal, got)
	}
}

// ── AC: the counter value survives a restart ──────────────────────────────

func TestExporter_CounterSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	for i := 0; i < 4; i++ {
		h.writeEvent("tool_call", 0)
	}
	if got := eventsTotal(t, h.scrape(h.exp), "tool_call"); got != 4 {
		t.Fatalf("counter = %v before restart, want 4", got)
	}
	if err := h.exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Restart: a brand new Exporter over the same database and state file.
	restarted := h.newExporter()
	h.start(restarted)

	exp := h.scrape(restarted)
	if got := eventsTotal(t, exp, "tool_call"); got != 4 {
		t.Fatalf("counter = %v after restart, want 4 — a reset to zero is exactly the failure Prometheus cannot see through", got)
	}

	// And it keeps counting forward from the restored value.
	h.writeEvent("tool_call", 0)
	if got := eventsTotal(t, h.scrape(restarted), "tool_call"); got != 5 {
		t.Errorf("counter = %v after a post-restart write, want 5", got)
	}
}

// A restart must not re-count the rows that are still in the table.
func TestExporter_RestartDoesNotDoubleCountSurvivingRows(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	for i := 0; i < 10; i++ {
		h.writeEvent("tool_call", 0)
	}
	if got := eventsTotal(t, h.scrape(h.exp), "tool_call"); got != 10 {
		t.Fatalf("counter = %v, want 10", got)
	}
	_ = h.exp.Close()

	for restart := 1; restart <= 3; restart++ {
		e := h.newExporter()
		h.start(e)
		if got := eventsTotal(t, h.scrape(e), "tool_call"); got != 10 {
			t.Fatalf("counter = %v after restart %d, want 10 — the rows are still in the table and must not be re-counted",
				got, restart)
		}
		_ = e.Close()
	}
}

// ── AC (edge case): deleting rows behind the cursor never decreases ────────
//
// This is the failure the whole design exists to prevent. It uses the real
// db.Prune, not a hand-written DELETE, so the test tracks the production
// path in internal/db/maintenance.go.

func TestExporter_PruneBehindCursorDoesNotDecreaseCounter(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	// Six old events that the 90-day prune will remove, and two recent
	// ones it will keep.
	for i := 0; i < 6; i++ {
		h.writeEvent("tool_call", 100*24*time.Hour)
	}
	h.writeEvent("tool_call", time.Minute)
	h.writeEvent("turn_start", time.Minute)

	before := h.scrape(h.exp)
	beforeToolCall := eventsTotal(t, before, "tool_call")
	beforeTurnStart := eventsTotal(t, before, "turn_start")
	if beforeToolCall != 7 || beforeTurnStart != 1 {
		t.Fatalf("counters before prune = (tool_call %v, turn_start %v), want (7, 1)", beforeToolCall, beforeTurnStart)
	}
	rowsBefore := h.rowCount()

	if err := h.writeDB.Prune(pruneHorizon); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	rowsAfter := h.rowCount()
	if rowsAfter >= rowsBefore {
		t.Fatalf("Prune removed nothing (%d rows before, %d after); the test would prove nothing", rowsBefore, rowsAfter)
	}

	after := h.scrape(h.exp)
	if got := eventsTotal(t, after, "tool_call"); got != beforeToolCall {
		t.Errorf("%s{type=tool_call} moved from %v to %v across a prune. "+
			"A counter that decreases is read by Prometheus as a process restart, and rate() then lies.",
			exporter.MetricAgentEventsTotal, beforeToolCall, got)
	}
	if got := eventsTotal(t, after, "turn_start"); got != beforeTurnStart {
		t.Errorf("%s{type=turn_start} moved from %v to %v across a prune",
			exporter.MetricAgentEventsTotal, beforeTurnStart, got)
	}

	// And it keeps counting forward from the un-moved value.
	h.writeEvent("tool_call", 0)
	if got := eventsTotal(t, h.scrape(h.exp), "tool_call"); got != beforeToolCall+1 {
		t.Errorf("%s{type=tool_call} = %v after a post-prune write, want %v",
			exporter.MetricAgentEventsTotal, got, beforeToolCall+1)
	}
}

// The severe variant: prune empties the table entirely, which frees the
// highest rowid. SQLite has no AUTOINCREMENT on agent_events, so the next
// insert reuses rowid 1 and the cursor is left stranded above it.
func TestExporter_PruneOfEveryRowDoesNotDecreaseCounterAndKeepsTailing(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	for i := 0; i < 5; i++ {
		h.writeEvent("tool_call", 100*24*time.Hour)
	}
	if got := eventsTotal(t, h.scrape(h.exp), "tool_call"); got != 5 {
		t.Fatalf("counter = %v before prune, want 5", got)
	}
	headBefore := h.maxRowID()

	if err := h.writeDB.Prune(pruneHorizon); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n := h.rowCount(); n != 0 {
		t.Fatalf("agent_events has %d rows after pruning every row, want 0", n)
	}

	if got := eventsTotal(t, h.scrape(h.exp), "tool_call"); got != 5 {
		t.Errorf("counter = %v after the table was emptied, want 5", got)
	}

	// SQLite now reuses the freed rowids.
	h.writeEvent("tool_call", 0)
	if head := h.maxRowID(); head >= headBefore {
		t.Logf("note: rowid was not reused (head %d, was %d); the clamp path is untested by this run", head, headBefore)
	}

	if got := eventsTotal(t, h.scrape(h.exp), "tool_call"); got != 6 {
		t.Errorf("counter = %v after a write into reused rowid space, want 6 — "+
			"a stranded cursor would leave the counter flat forever", got)
	}
}

// ── AC (edge case): no state file initialises at the head, no backfill ─────

func TestExporter_NoStateFileInitialisesAtHeadWithoutBackfill(t *testing.T) {
	h := newHarness(t)
	// Pre-existing history, written before the exporter ever ran.
	for i := 0; i < 7; i++ {
		h.writeEvent("tool_call", time.Hour)
	}
	if _, err := os.Stat(h.statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file exists before the first start: %v", err)
	}

	h.start(h.exp)

	exp := h.scrape(h.exp)
	if got := eventsTotal(t, exp, "tool_call"); got != 0 {
		t.Fatalf("counter = %v on a first run over a table with 7 existing rows, want 0 — history must not be backfilled", got)
	}

	// The state file must now exist and hold the head of the table, so a
	// crash before the first scrape still resumes in the right place.
	state, err := os.ReadFile(h.statePath)
	if err != nil {
		t.Fatalf("state file was not written on Start: %v", err)
	}
	if want := fmt.Sprintf(`"%s":%d`, exporter.TailerAgentEvents, h.maxRowID()); !strings.Contains(string(state), want) {
		t.Errorf("state file does not record the head cursor %s. Content: %s", want, state)
	}

	// Only rows written from now on count.
	h.writeEvent("tool_call", 0)
	if got := eventsTotal(t, h.scrape(h.exp), "tool_call"); got != 1 {
		t.Errorf("counter = %v after one new row, want 1", got)
	}
}

// ── AC (edge case): a corrupt state file is detected, logged, no crash ─────

func TestExporter_CorruptStateFileIsLoggedAndDoesNotCrash(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"truncated", `{"version":1,"cursors":{"agent_events":4`},
		{"empty", ""},
		{"not JSON", "hello"},
		{"unknown version", `{"version":424242,"cursors":{}}`},
		{"negative cursor", `{"version":1,"cursors":{"agent_events":-5}}`},
		{"negative counter value", `{"version":1,"cursors":{},"counters":{"prism_agent_events_total":{"[\"x\"]":-3}}}`},
		{"counter key is not a label tuple", `{"version":1,"cursors":{"agent_events":0},"counters":{"prism_agent_events_total":{"not-a-tuple":5}}}`},
		{"counter key has the wrong label count", `{"version":1,"cursors":{"agent_events":0},"counters":{"prism_agent_events_total":{"[\"a\",\"b\"]":5}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			for i := 0; i < 3; i++ {
				h.writeEvent("tool_call", time.Hour)
			}
			if err := os.WriteFile(h.statePath, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			// Must not crash, and must not refuse to serve.
			h.start(h.exp)
			exp := h.scrape(h.exp)

			if h.logBuf.Len() == 0 {
				t.Error("a corrupt state file was accepted silently; it must be logged")
			}
			if got := eventsTotal(t, exp, "tool_call"); got != 0 {
				t.Errorf("counter = %v after a corrupt state file, want 0 (a clean re-initialisation at the head)", got)
			}
			if _, ok := exp.Value(exporter.MetricBuildInfo, nil); !ok {
				// build_info carries labels, so match on them.
				if _, ok := exp.Value(exporter.MetricBuildInfo, map[string]string{"version": "test-version"}); !ok {
					t.Error("the daemon stopped serving after a corrupt state file")
				}
			}

			// It recovers: new rows count from now on, and the file is
			// rewritten in a usable form.
			h.writeEvent("tool_call", 0)
			if got := eventsTotal(t, h.scrape(h.exp), "tool_call"); got != 1 {
				t.Errorf("counter = %v after recovery, want 1", got)
			}
			_ = h.exp.Close()

			recovered := h.newExporter()
			h.start(recovered)
			if got := eventsTotal(t, h.scrape(recovered), "tool_call"); got != 1 {
				t.Errorf("counter = %v after restarting on the rewritten state file, want 1", got)
			}
		})
	}
}

func TestExporter_UnreadableStateFileDoesNotCrash(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0000 file is still readable")
	}
	h := newHarness(t)
	if err := os.WriteFile(h.statePath, []byte(`{"version":1,"cursors":{}}`), 0o000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	h.start(h.exp)
	if h.logBuf.Len() == 0 {
		t.Error("an unreadable state file was accepted silently; it must be logged")
	}
	h.scrape(h.exp) // must not panic or 500
}

// ── Cardinality (#2699 section 6) ─────────────────────────────────────────

func TestExporter_ExposesNoUnboundedLabel(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.writeEvent("tool_call", 0)
	// A value outside the closed set, so the bound below is exercised rather
	// than merely satisfied by a small input.
	h.writeEvent("not_a_known_event_type", 0)

	banned := []string{"session_name", "instance_id", "issue_ref", "harness_session_id", "worktree", "id"}
	exp := h.scrape(h.exp)
	for _, family := range exp.Families {
		for _, s := range family.Samples {
			for label := range s.Labels {
				for _, b := range banned {
					if label == b {
						t.Errorf("metric %s carries the unbounded label %q; see #2699 section 6", s.Name, label)
					}
				}
			}
		}
	}

	// A closed label NAME is not enough. The series count of every family
	// must be bounded by something that does not depend on the database
	// contents — agent_events.type is writable from inside a worker sandbox.
	bounds := map[string]int{
		exporter.MetricAgentEventsTotal: exporter.MaxAgentEventsSeries,
		exporter.MetricBuildInfo:        1,
		// The six #2703 counters have no closed-set enforcement of their own
		// today: repo, agent_role, isolation_mode, end_state, profile, and
		// verdict are all pre-sanctioned safe labels under #2699 section 6, so there is
		// no fold to bound them against a hostile value the way
		// agent_events.type needs one. A large-but-finite bound here still
		// catches an accidental unbounded label creeping in later.
		exporter.MetricSpawnsTotal:           1000,
		exporter.MetricSessionsEndedTotal:    1000,
		exporter.MetricReviewVerdictsTotal:   2,
		exporter.MetricEscalationsTotal:      1000,
		exporter.MetricDoomLoopsTotal:        1000,
		exporter.MetricPermissionDeniedTotal: 1000,
	}
	for name, family := range exp.Families {
		bound, ok := bounds[name]
		if !ok {
			t.Errorf("metric family %q has no declared series bound; every family needs one (#2699 section 6)", name)
			continue
		}
		if len(family.Samples) > bound {
			t.Errorf("metric %s has %d series, want at most %d", name, len(family.Samples), bound)
		}
	}

	if _, ok := exp.Value(exporter.MetricAgentEventsTotal, map[string]string{"type": exporter.OtherEventType}); !ok {
		t.Error("an unknown event type did not land in the bounded bucket")
	}
}

// ── HTTP surface ──────────────────────────────────────────────────────────

func TestExporter_MetricsRejectsWriteMethods(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	srv := httptest.NewServer(h.exp.Handler())
	defer srv.Close()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, err := http.NewRequest(method, srv.URL+exporter.MetricsPath, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, exporter.MetricsPath, resp.StatusCode)
		}
	}
}

func TestExporter_ServesNothingButMetrics(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	srv := httptest.NewServer(h.exp.Handler())
	defer srv.Close()

	for _, path := range []string{"/", "/debug/pprof/", "/db", "/metrics/../etc"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s = 200; the daemon must serve only %s", path, exporter.MetricsPath)
		}
	}
}

// ── The daemon lifecycle ──────────────────────────────────────────────────

func TestExporter_RunServesOnARealPortAndShutsDownCleanly(t *testing.T) {
	h := newHarness(t)
	h.writeEvent("tool_call", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.exp.Run(ctx) }()

	addr := waitForAddr(t, h.exp)
	h.writeEvent("turn_start", 0)

	resp, err := http.Get("http://" + addr + exporter.MetricsPath)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d, want 200", resp.StatusCode)
	}
	exp := metricstest.MustParse(t, string(body))
	if got := eventsTotal(t, exp, "turn_start"); got != 1 {
		t.Errorf("counter = %v, want 1", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on a cancelled context", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return within 30s of cancellation")
	}

	// The shutdown snapshot must be on disk.
	raw, err := os.ReadFile(h.statePath)
	if err != nil {
		t.Fatalf("state file missing after shutdown: %v", err)
	}
	if !strings.Contains(string(raw), "turn_start") {
		t.Errorf("shutdown snapshot does not carry the accumulated counter: %s", raw)
	}
}

func waitForAddr(t *testing.T, e *exporter.Exporter) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if addr := e.Addr(); addr != "" {
			return addr
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("exporter did not bind a listener within 30s")
	return ""
}

// Start must be idempotent, or a second call against a corrupt state file
// would zero a counter that is already serving. That is a counter decrease,
// which is the one thing this design exists to prevent.
func TestExporter_StartIsIdempotentAndNeverZeroesALiveCounter(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	for i := 0; i < 3; i++ {
		h.writeEvent("tool_call", 0)
	}
	if got := eventsTotal(t, h.scrape(h.exp), "tool_call"); got != 3 {
		t.Fatalf("counter = %v, want 3", got)
	}

	// Replace the state file underneath the running daemon with one that
	// LOADS cleanly but whose counter values cannot be applied — a key with
	// the wrong label count. That is the branch that reaches
	// resetPersistedMetricsLocked, so it is the branch where a second Start
	// would zero a live counter.
	unusable := `{"version":1,"cursors":{"` + exporter.TailerAgentEvents +
		`":0},"counters":{"` + exporter.MetricAgentEventsTotal +
		`":{"[\"a\",\"b\"]":5}}}`
	if err := os.WriteFile(h.statePath, []byte(unusable), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := h.exp.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if got := eventsTotal(t, h.scrape(h.exp), "tool_call"); got != 3 {
		t.Fatalf("counter = %v after a second Start, want 3 — a second Start must never zero a live counter", got)
	}

	// And it still counts forward.
	h.writeEvent("tool_call", 0)
	if got := eventsTotal(t, h.scrape(h.exp), "tool_call"); got != 4 {
		t.Errorf("counter = %v after a further write, want 4", got)
	}
}

func TestExporter_RefreshBeforeStartIsRefused(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 5; i++ {
		h.writeEvent("tool_call", time.Hour)
	}
	if err := h.exp.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh before Start succeeded; an unpositioned tailer would backfill all of history")
	}
}

func TestExporter_NewValidatesConfig(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(dir, "prism.db")
	d := sidecartest.OpenDB(t, dbFile)
	t.Cleanup(func() { _ = d.Close() })

	base := exporter.Config{
		DBPath:     dbFile,
		StatePath:  filepath.Join(dir, "exporter-state.json"),
		ListenAddr: "127.0.0.1:0",
	}
	for _, tc := range []struct {
		name   string
		mutate func(*exporter.Config)
	}{
		{"no DB path", func(c *exporter.Config) { c.DBPath = "" }},
		{"no state path", func(c *exporter.Config) { c.StatePath = "" }},
		{"no listen address", func(c *exporter.Config) { c.ListenAddr = "" }},
		{"negative poll interval", func(c *exporter.Config) { c.PollInterval = -time.Second }},
		{"missing database", func(c *exporter.Config) { c.DBPath = filepath.Join(dir, "absent.db") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			e, err := exporter.New(cfg)
			if err == nil {
				_ = e.Close()
				t.Fatal("New succeeded on an invalid config")
			}
		})
	}
}

func TestDefaultStatePath_SitsBesideTheDatabase(t *testing.T) {
	got := exporter.DefaultStatePath("/home/ben/.local/state/prism/prism.db")
	want := "/home/ben/.local/state/prism/" + exporter.StateFileName
	if got != want {
		t.Errorf("DefaultStatePath = %q, want %q", got, want)
	}
}

// The default port and path are what #2701 wires its Alloy scrape to. A
// change here needs a matching change there.
func TestExporter_DefaultsAreTheOnesTheAlloyScrapeWillPointAt(t *testing.T) {
	if exporter.DefaultPort != 19891 {
		t.Errorf("DefaultPort = %d; #2701 points its Alloy scrape at 19891", exporter.DefaultPort)
	}
	if exporter.MetricsPath != "/metrics" {
		t.Errorf("MetricsPath = %q, want /metrics", exporter.MetricsPath)
	}
	if exporter.DefaultListenHost != "127.0.0.1" {
		t.Errorf("DefaultListenHost = %q; the endpoint is unauthenticated and must stay on loopback", exporter.DefaultListenHost)
	}
}
