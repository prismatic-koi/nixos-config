package exporter_test

// Tests for the cost and token counters and the account dimension.
//
// These reuse the harness from exporter_test.go. They control the two things
// the counters key on that the other tailers do not: the msg_assistant
// PAYLOAD (model, tokens, cost) and the account_name COLUMN. Both are set by
// inserting agent_events rows through a raw connection, so a test can write a
// SQL NULL account_name (which predates the column) that WriteEvent — which
// always stamps a name — cannot produce.

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/exporter"
	"github.com/prismatic-koi/prism/internal/pricing"
	"github.com/prismatic-koi/prism/internal/usage"
)

// The two accounts used across these tests, with distinct org IDs.
// account_org_id is the identity; the name is display only.
const (
	orgWork     = "org-work-1111"
	orgPersonal = "org-personal-2222"
	wsWork      = "ws-work-aaaa"
	wsPersonal  = "ws-personal-bbbb"
)

// A model that IS in the pricing table, with round per-million rates that
// make the arithmetic obvious: input $3/M, output $15/M.
const knownModel = "anthropic/claude-sonnet-4-6"

// raw returns a lazily-opened second connection for INSERTing agent_events
// rows with full control of account_name (including SQL NULL) and payload.
// modernc.org/sqlite is registered under "sqlite" by the internal/db import.
func (h *harness) raw() *sql.DB {
	h.t.Helper()
	if h.rawDB == nil {
		conn, err := sql.Open("sqlite", h.dbPath+"?_pragma=busy_timeout(5000)")
		if err != nil {
			h.t.Fatalf("open raw db: %v", err)
		}
		h.t.Cleanup(func() { _ = conn.Close() })
		h.rawDB = conn
	}
	return h.rawDB
}

// assistantOpts describes one msg_assistant row for insertAssistant.
type assistantOpts struct {
	// account is the account_name column. A nil pointer writes SQL NULL; a
	// non-nil pointer writes that exact string.
	account    *string
	instanceID string // "" leaves instance_id NULL
	// profile is the agent_events.profile_name column, stamped at write time.
	// "" writes SQL NULL (which folds to "unknown"); a non-empty value writes
	// that exact tier.
	profile    string
	model      string
	input      int64
	output     int64
	cacheRead  int64
	cacheWrite int64
	// cost is payload $.cost — the model-reported cost, used as the fallback
	// for a model absent from the pricing table.
	cost float64
}

func strptr(s string) *string { return &s }

// insertAssistant writes one msg_assistant agent_events row through the raw
// connection.
func (h *harness) insertAssistant(o assistantOpts) {
	h.t.Helper()
	payload := fmt.Sprintf(
		`{"model":%q,"inputTokens":%d,"outputTokens":%d,"cacheReadTokens":%d,"cacheWriteTokens":%d,"cost":%s}`,
		o.model, o.input, o.output, o.cacheRead, o.cacheWrite,
		fmtFloat(o.cost),
	)
	var instance any
	if o.instanceID != "" {
		instance = o.instanceID
	}
	var account any
	if o.account != nil {
		account = *o.account
	}
	var profile any
	if o.profile != "" {
		profile = o.profile
	}
	_, err := h.raw().Exec(
		`INSERT INTO agent_events (id, session_name, repo, worktree, type, payload, created_at, instance_id, account_name, profile_name)
		 VALUES (?, ?, ?, ?, 'msg_assistant', ?, ?, ?, ?, ?)`,
		uuid.New().String(), "prism-test@cost", "nixos-config", "/tmp/prism-test",
		payload, time.Now().UnixMilli(), instance, account, profile,
	)
	if err != nil {
		h.t.Fatalf("insert msg_assistant: %v", err)
	}
}

func fmtFloat(f float64) string { return fmt.Sprintf("%v", f) }

// writeSnapshot writes a usage snapshot mapping an account name to an org ID
// and workspace ID, exactly as internal/usage does on the refresh path.
func (h *harness) writeSnapshot(account, orgID, workspaceID string, capturedAt time.Time) {
	h.t.Helper()
	if err := usage.NewStore(h.usageDir).Write(usage.Snapshot{
		CapturedAt:     usage.FormatCapturedAt(capturedAt),
		Account:        account,
		OrganizationID: orgID,
		WorkspaceID:    workspaceID,
	}); err != nil {
		h.t.Fatalf("write usage snapshot for %q: %v", account, err)
	}
}

// costValue reads one series of a cost/token metric after a fresh scrape.
func (h *harness) costValue(metric string, labels map[string]string) float64 {
	h.t.Helper()
	exp := h.scrape(h.exp)
	v, _ := exp.Value(metric, labels)
	return v
}

func modelLabels(orgID, provider, modelID string) map[string]string {
	return map[string]string{"account_org_id": orgID, "provider": provider, "model_id": modelID}
}

// ── AC: all three counters appear and increase as tokens are consumed ──────

func TestExporter_CostAndTokenCountersIncrease(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.writeSnapshot("work", orgWork, wsWork, time.Now())

	// 1,000,000 input + 1,000,000 output tokens of the known model:
	// cost = 1e6*3/1e6 + 1e6*15/1e6 = 3 + 15 = 18 USD.
	h.insertAssistant(assistantOpts{account: strptr("work"), model: knownModel, input: 1_000_000, output: 1_000_000})

	labels := modelLabels(orgWork, "anthropic", "claude-sonnet-4-6")
	if got := h.costValue(exporter.MetricModelCostUSDTotal, labels); got != 18 {
		t.Errorf("%s%v = %v, want 18", exporter.MetricModelCostUSDTotal, labels, got)
	}
	if got := h.costValue(exporter.MetricModelTokensTotal, withKind(labels, "input")); got != 1_000_000 {
		t.Errorf("input tokens = %v, want 1000000", got)
	}
	if got := h.costValue(exporter.MetricModelTokensTotal, withKind(labels, "output")); got != 1_000_000 {
		t.Errorf("output tokens = %v, want 1000000", got)
	}

	// A second turn increases the counters.
	h.insertAssistant(assistantOpts{account: strptr("work"), model: knownModel, input: 1_000_000})
	if got := h.costValue(exporter.MetricModelCostUSDTotal, labels); got != 21 {
		t.Errorf("%s after a second turn = %v, want 21", exporter.MetricModelCostUSDTotal, got)
	}
}

func withKind(base map[string]string, kind string) map[string]string {
	out := make(map[string]string, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out["kind"] = kind
	return out
}

// ── AC: the cost equals pricing.Cost, so the exporter and prism stats agree ─
//
// Both the exporter and prism stats' collectModelMetrics compute per-turn
// cost with pricing.Cost. This test recomputes the expected total with the
// SAME function the stats path uses, over the same events, and asserts
// equality to floating-point tolerance — the structural half of the "agree
// with prism stats" AC. The other half (a real local prism.db) is in the PR.

func TestExporter_CostEqualsPricingCost(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.writeSnapshot("work", orgWork, wsWork, time.Now())

	type turn struct{ in, out, cr, cw int64 }
	turns := []turn{
		{123456, 7890, 4444, 111},
		{2000, 300, 50, 5},
		{999999, 1, 0, 42},
	}
	var want float64
	for _, tn := range turns {
		h.insertAssistant(assistantOpts{
			account: strptr("work"), model: knownModel,
			input: tn.in, output: tn.out, cacheRead: tn.cr, cacheWrite: tn.cw,
		})
		want += pricing.Cost(knownModel,
			float64(tn.in), float64(tn.out), float64(tn.cr), float64(tn.cw), 0)
	}

	got := h.costValue(exporter.MetricModelCostUSDTotal, modelLabels(orgWork, "anthropic", "claude-sonnet-4-6"))
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("exporter cost = %v, pricing.Cost total = %v (diff %v); the two must agree", got, want, math.Abs(got-want))
	}
}

// ── AC: two accounts produce two distinct org series, neither leaking ──────

func TestExporter_TwoAccountsProduceTwoDistinctOrgSeries(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.writeSnapshot("work", orgWork, wsWork, time.Now())
	h.writeSnapshot("personal", orgPersonal, wsPersonal, time.Now())

	// work spends 3 USD (1M input), personal spends 15 USD (1M output).
	h.insertAssistant(assistantOpts{account: strptr("work"), model: knownModel, input: 1_000_000})
	h.insertAssistant(assistantOpts{account: strptr("personal"), model: knownModel, output: 1_000_000})

	workLabels := modelLabels(orgWork, "anthropic", "claude-sonnet-4-6")
	personalLabels := modelLabels(orgPersonal, "anthropic", "claude-sonnet-4-6")
	if got := h.costValue(exporter.MetricModelCostUSDTotal, workLabels); got != 3 {
		t.Errorf("work org cost = %v, want 3 (must not include personal's spend)", got)
	}
	if got := h.costValue(exporter.MetricModelCostUSDTotal, personalLabels); got != 15 {
		t.Errorf("personal org cost = %v, want 15 (must not include work's spend)", got)
	}
}

// ── AC: prism_account_info emitted once per known account, value 1 ─────────

func TestExporter_AccountInfoEmittedPerKnownAccount(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.writeSnapshot("work", orgWork, wsWork, time.Now())
	h.writeSnapshot("personal", orgPersonal, wsPersonal, time.Now())

	exp := h.scrape(h.exp)
	info := exp.Family(t, exporter.MetricAccountInfo)
	if info.Type != "gauge" {
		t.Errorf("%s is a %s, want gauge", exporter.MetricAccountInfo, info.Type)
	}
	if len(info.Samples) != 2 {
		t.Fatalf("%s has %d series, want 2 (one per known account)", exporter.MetricAccountInfo, len(info.Samples))
	}
	for _, tc := range []struct{ org, account, ws string }{
		{orgWork, "work", wsWork},
		{orgPersonal, "personal", wsPersonal},
	} {
		v, ok := exp.Value(exporter.MetricAccountInfo, map[string]string{
			"account_org_id": tc.org, "account": tc.account, "workspace_id": tc.ws,
		})
		if !ok {
			t.Errorf("no prism_account_info series for org=%s account=%s ws=%s", tc.org, tc.account, tc.ws)
			continue
		}
		if v != 1 {
			t.Errorf("prism_account_info for %s = %v, want 1", tc.account, v)
		}
	}
}

// ── AC: a rename changes only the account label; no counter resets ─────────
//
// A rename writes new events under a new name and a new usage snapshot, while
// the old snapshot persists (nothing deletes it). Both names map to the same
// org ID, so the counter series for that org is continuous — its value only
// grows — and only prism_account_info's `account` label changes.

func TestExporter_RenameChangesOnlyAccountInfoAndNeverResetsACounter(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	// Before: account is "work", one turn of 1M input = 3 USD.
	h.writeSnapshot("work", orgWork, wsWork, time.Now().Add(-time.Hour))
	h.insertAssistant(assistantOpts{account: strptr("work"), model: knownModel, input: 1_000_000})

	labels := modelLabels(orgWork, "anthropic", "claude-sonnet-4-6")
	if got := h.costValue(exporter.MetricModelCostUSDTotal, labels); got != 3 {
		t.Fatalf("pre-rename org cost = %v, want 3", got)
	}

	// Rename: the account is now "work-renamed" with the SAME org ID. The new
	// snapshot is more recent; the old "work" snapshot stays on disk. A new
	// turn of 1M input is recorded under the new name.
	h.writeSnapshot("work-renamed", orgWork, wsWork, time.Now())
	h.insertAssistant(assistantOpts{account: strptr("work-renamed"), model: knownModel, input: 1_000_000})

	// The org series is continuous: 3 + 3 = 6, no reset.
	if got := h.costValue(exporter.MetricModelCostUSDTotal, labels); got != 6 {
		t.Errorf("post-rename org cost = %v, want 6 — a rename must not reset or split the counter", got)
	}

	// prism_account_info now carries the NEW name for the same org, and there
	// is still exactly one series for that org.
	exp := h.scrape(h.exp)
	info := exp.Family(t, exporter.MetricAccountInfo)
	orgSeries := 0
	for _, s := range info.Samples {
		if s.Labels["account_org_id"] == orgWork {
			orgSeries++
			if s.Labels["account"] != "work-renamed" {
				t.Errorf("prism_account_info account label = %q after rename, want work-renamed", s.Labels["account"])
			}
		}
	}
	if orgSeries != 1 {
		t.Errorf("prism_account_info has %d series for org %s after rename, want exactly 1", orgSeries, orgWork)
	}
}

// ── AC (edge): an account with no usage snapshot resolves to "unknown" ─────

func TestExporter_AccountWithNoSnapshotIsUnknownNotDropped(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	// No snapshot for "ghost": its name cannot be mapped to an org ID.
	h.insertAssistant(assistantOpts{account: strptr("ghost"), model: knownModel, input: 1_000_000})

	labels := modelLabels("unknown", "anthropic", "claude-sonnet-4-6")
	if got := h.costValue(exporter.MetricModelCostUSDTotal, labels); got != 3 {
		t.Errorf("cost under account_org_id=unknown = %v, want 3 (a nameable-but-unmapped account must not be dropped)", got)
	}
}

// ── AC (edge): a SQL NULL account_name is "unknown" ────────

func TestExporter_NullAccountNameIsUnknownAndDoesNotPanic(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	// account == nil -> SQL NULL account_name.
	h.insertAssistant(assistantOpts{account: nil, model: knownModel, input: 1_000_000})

	labels := modelLabels("unknown", "anthropic", "claude-sonnet-4-6")
	if got := h.costValue(exporter.MetricModelCostUSDTotal, labels); got != 3 {
		t.Errorf("cost under account_org_id=unknown = %v, want 3 (a NULL account_name must be attributed to unknown, not dropped)", got)
	}
}

// ── AC (edge): a model absent from the pricing table falls back to $.cost ──

func TestExporter_UnknownModelFallsBackToEventCost(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.writeSnapshot("work", orgWork, wsWork, time.Now())

	// openrouter/* is not in the pricing table. The exporter must use the
	// event-reported cost, exactly as prism stats does — not zero, not panic.
	h.insertAssistant(assistantOpts{
		account: strptr("work"), model: "openrouter/some-model",
		input: 5000, output: 500, cost: 0.4242,
	})

	labels := modelLabels(orgWork, "openrouter", "some-model")
	got := h.costValue(exporter.MetricModelCostUSDTotal, labels)
	if math.Abs(got-0.4242) > 1e-9 {
		t.Errorf("unknown-model cost = %v, want the event-reported 0.4242", got)
	}
}

// ── AC (functional): spend split by profile, read from agent_events ────────
//
// profile_name is stamped on each agent_events row at write time and
// read directly, NOT joined from spawn_inputs. So a row's tier is whatever
// was stamped on it, regardless of whether a spawn_inputs row exists.

func TestExporter_SpendByProfile(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.writeSnapshot("work", orgWork, wsWork, time.Now())

	// A worker turn stamped with profile "max" (1M input = 3 USD).
	h.insertAssistant(assistantOpts{account: strptr("work"), profile: "max", model: knownModel, input: 1_000_000})

	// A second worker turn stamped "heavy" (1M output = 15 USD).
	h.insertAssistant(assistantOpts{account: strptr("work"), profile: "heavy", model: knownModel, output: 1_000_000})

	if got := h.costValue(exporter.MetricSpendByProfileUSDTotal, map[string]string{"account_org_id": orgWork, "profile": "max"}); got != 3 {
		t.Errorf("spend under profile=max = %v, want 3", got)
	}
	if got := h.costValue(exporter.MetricSpendByProfileUSDTotal, map[string]string{"account_org_id": orgWork, "profile": "heavy"}); got != 15 {
		t.Errorf("spend under profile=heavy = %v, want 15", got)
	}
}

// ── AC (functional): a coordinator (no spawn_inputs row) is its real tier ──
//
// The bug this issue fixes: a coordinator is never spawned, so it has no
// spawn_inputs row. The old join-based query missed and folded ALL its spend
// to "default". Now profile_name is stamped on the event itself, so the
// coordinator's spend lands under its real tier even with no spawn_inputs row
// and no instance_id. It must NOT appear under "default".

func TestExporter_CoordinatorSpendAttributedToRealTierNotDefault(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.writeSnapshot("work", orgWork, wsWork, time.Now())

	// A coordinator turn: no instance_id, no spawn_inputs row, but the profile
	// "heavy" was resolved and stamped at write time (1M input = 3 USD).
	h.insertAssistant(assistantOpts{account: strptr("work"), profile: "heavy", model: knownModel, input: 1_000_000})

	if got := h.costValue(exporter.MetricSpendByProfileUSDTotal, map[string]string{"account_org_id": orgWork, "profile": "heavy"}); got != 3 {
		t.Errorf("coordinator spend under profile=heavy = %v, want 3", got)
	}
	if got := h.costValue(exporter.MetricSpendByProfileUSDTotal, map[string]string{"account_org_id": orgWork, "profile": "default"}); got != 0 {
		t.Errorf("spend under profile=default = %v, want 0 (coordinator spend must not fold to default)", got)
	}
}

// ── AC (edge): a NULL profile_name folds to "unknown" ──────

func TestExporter_NullProfileNameFoldsToUnknownNotEmpty(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.writeSnapshot("work", orgWork, wsWork, time.Now())

	// profile == "" -> SQL NULL profile_name.
	h.insertAssistant(assistantOpts{account: strptr("work"), model: knownModel, input: 1_000_000})

	if got := h.costValue(exporter.MetricSpendByProfileUSDTotal, map[string]string{"account_org_id": orgWork, "profile": "unknown"}); got != 3 {
		t.Errorf("spend under profile=unknown = %v, want 3 (NULL profile_name must fold to the explicit placeholder)", got)
	}
	if got := h.costValue(exporter.MetricSpendByProfileUSDTotal, map[string]string{"account_org_id": orgWork, "profile": ""}); got != 0 {
		t.Errorf("spend under empty profile label = %v, want 0 (NULL must never be emitted as an empty label)", got)
	}
}

// ── AC (edge): pruning rows behind the cursor does not decrease a counter ──

func TestExporter_CostCountersSurvivePruneBehindCursor(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.writeSnapshot("work", orgWork, wsWork, time.Now())

	// Old turns the 90-day prune will remove, and a recent one it keeps. All
	// are already counted through the cursor.
	for i := 0; i < 4; i++ {
		h.insertAssistantAged(assistantOpts{account: strptr("work"), model: knownModel, input: 1_000_000}, 100*24*time.Hour)
	}
	h.insertAssistant(assistantOpts{account: strptr("work"), model: knownModel, input: 1_000_000})

	labels := modelLabels(orgWork, "anthropic", "claude-sonnet-4-6")
	before := h.costValue(exporter.MetricModelCostUSDTotal, labels)
	if before != 15 { // 5 turns * 3 USD
		t.Fatalf("pre-prune cost = %v, want 15", before)
	}

	rowsBefore := h.rowCount()
	if err := h.writeDB.Prune(pruneHorizon); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if h.rowCount() >= rowsBefore {
		t.Fatalf("Prune removed nothing; the test would prove nothing")
	}

	if got := h.costValue(exporter.MetricModelCostUSDTotal, labels); got != before {
		t.Errorf("cost moved from %v to %v across a prune; a counter must never decrease", before, got)
	}

	// And it keeps counting forward.
	h.insertAssistant(assistantOpts{account: strptr("work"), model: knownModel, input: 1_000_000})
	if got := h.costValue(exporter.MetricModelCostUSDTotal, labels); got != before+3 {
		t.Errorf("cost after a post-prune turn = %v, want %v", got, before+3)
	}
}

// insertAssistantAged inserts a msg_assistant row with a created_at in the
// past, so db.Prune removes it.
func (h *harness) insertAssistantAged(o assistantOpts, age time.Duration) {
	h.t.Helper()
	payload := fmt.Sprintf(
		`{"model":%q,"inputTokens":%d,"outputTokens":%d,"cacheReadTokens":%d,"cacheWriteTokens":%d,"cost":%s}`,
		o.model, o.input, o.output, o.cacheRead, o.cacheWrite, fmtFloat(o.cost),
	)
	var account any
	if o.account != nil {
		account = *o.account
	}
	_, err := h.raw().Exec(
		`INSERT INTO agent_events (id, session_name, repo, worktree, type, payload, created_at, account_name)
		 VALUES (?, ?, ?, ?, 'msg_assistant', ?, ?, ?)`,
		uuid.New().String(), "prism-test@cost", "nixos-config", "/tmp/prism-test",
		payload, time.Now().Add(-age).UnixMilli(), account,
	)
	if err != nil {
		h.t.Fatalf("insert aged msg_assistant: %v", err)
	}
}

// ── AC (edge): every counter survives an exporter restart ──────────────────

func TestExporter_CostCountersSurviveRestart(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.writeSnapshot("work", orgWork, wsWork, time.Now())
	h.insertAssistant(assistantOpts{account: strptr("work"), model: knownModel, input: 1_000_000, output: 1_000_000})

	labels := modelLabels(orgWork, "anthropic", "claude-sonnet-4-6")
	before := h.costValue(exporter.MetricModelCostUSDTotal, labels)
	if before != 18 {
		t.Fatalf("pre-restart cost = %v, want 18", before)
	}

	if err := h.exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	restarted := h.newExporter()
	h.start(restarted)

	rh := &harness{t: t, exp: restarted, usageDir: h.usageDir}
	exp := rh.scrape(restarted)
	if v, _ := exp.Value(exporter.MetricModelCostUSDTotal, labels); v != before {
		t.Errorf("cost = %v after restart, want %v — a reset is exactly what Prometheus cannot see through", v, before)
	}

	// It keeps counting forward from the restored value.
	h.insertAssistant(assistantOpts{account: strptr("work"), model: knownModel, input: 1_000_000})
	if v, _ := rh.scrape(restarted).Value(exporter.MetricModelCostUSDTotal, labels); v != before+3 {
		t.Errorf("cost = %v after a post-restart turn, want %v", v, before+3)
	}
}

// ── AC (security): the cost metrics carry no unbounded label ───────────────

func TestExporter_CostMetricsCarryNoUnboundedLabel(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	h.writeSnapshot("work", orgWork, wsWork, time.Now())
	h.insertAssistant(assistantOpts{account: strptr("work"), model: knownModel, input: 1_000_000})

	banned := []string{"session_name", "instance_id", "issue_ref", "harness_session_id", "worktree", "id"}
	exp := h.scrape(h.exp)
	for _, name := range []string{
		exporter.MetricModelCostUSDTotal, exporter.MetricModelTokensTotal,
		exporter.MetricSpendByProfileUSDTotal, exporter.MetricAccountInfo,
	} {
		family, ok := exp.Families[name]
		if !ok {
			continue
		}
		for _, s := range family.Samples {
			for label := range s.Labels {
				for _, b := range banned {
					if label == b {
						t.Errorf("metric %s carries the unbounded label %q (#2699 section 6)", name, label)
					}
				}
			}
		}
	}
}

// The usage dir may not exist at all (a fresh machine). The exporter must
// still serve — every account then folds to "unknown".
func TestExporter_MissingUsageDirYieldsUnknownNotError(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)
	if _, err := os.Stat(h.usageDir); !os.IsNotExist(err) {
		t.Fatalf("usage dir already exists; this test needs it absent: %v", err)
	}
	h.insertAssistant(assistantOpts{account: strptr("work"), model: knownModel, input: 1_000_000})

	labels := modelLabels("unknown", "anthropic", "claude-sonnet-4-6")
	if got := h.costValue(exporter.MetricModelCostUSDTotal, labels); got != 3 {
		t.Errorf("cost with no usage dir = %v under account_org_id=unknown, want 3", got)
	}
}
