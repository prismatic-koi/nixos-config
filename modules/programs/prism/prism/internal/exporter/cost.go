package exporter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/prismatic-koi/prism/internal/metrics"
	"github.com/prismatic-koi/prism/internal/pricing"
	"github.com/prismatic-koi/prism/internal/usage"
)

// cost.go — the cost and token counters of issue #2704, and the account
// dimension that is the point of this issue.
//
// # What it produces
//
//	prism_model_cost_usd_total{account_org_id,provider,model_id}        counter
//	prism_model_tokens_total{account_org_id,provider,model_id,kind}     counter
//	prism_spend_by_profile_usd_total{account_org_id,profile}            counter
//	prism_account_info{account_org_id,account,workspace_id}             gauge = 1
//
// All three counters accumulate through the same tail cursor as the rest of
// the exporter (costEventSource in sql.go), so a counter value never comes
// from a full-table aggregate over a pruned table (#2699 section 3).
//
// # Identity is the org ID, display is the name
//
// The counters carry account_org_id ONLY. The org ID is server-assigned and
// identical on every machine; the account NAME lives in the per-machine
// ~/.config/prism/accounts/ directory and can differ between navi, tui, and
// m4mac for one subscription, which would silently split it into two series
// in a fleet-wide query. So the readable name reaches Grafana only through
// the prism_account_info join metric, keyed on account_org_id.
//
// # Resolve name -> org ID at EMIT time, not accumulate time
//
// The counters accumulate keyed on the account NAME recorded on each event
// at write time (#2714). The name is mapped to an org ID only when the
// counter is emitted, from the usage snapshots (#2713). This is the whole
// reason accountCounter exists rather than a plain metrics.CounterVec:
//
//   - Accumulating on the name keeps each series' identity stable. A rename
//     writes new events under a new name but never rewrites old rows, so no
//     existing series changes identity and no counter resets. Both the old
//     and the new name resolve to the same org ID (both usage snapshots
//     persist), so the emitted org_id series is continuous across the
//     rename. Only prism_account_info's `account` label changes.
//   - Resolving the *currently active* account at scrape time and applying
//     it to every counter would be the trap: counters accumulate across
//     account switches, so that would retroactively attribute earlier spend
//     to whichever account is active now. The per-event name, stamped at
//     write time, is the only correct source.

// Metric names for the four #2704 metrics.
const (
	MetricModelCostUSDTotal      = "prism_model_cost_usd_total"
	MetricModelTokensTotal       = "prism_model_tokens_total"
	MetricSpendByProfileUSDTotal = "prism_spend_by_profile_usd_total"
	MetricAccountInfo            = "prism_account_info"
)

// TailerCostEvents is the state-file key the #2704 tailer's cursor is stored
// under. Independent of the #2700 and #2703 cursors even though all three
// tail agent_events; changing it makes a running daemon lose its place on
// these counters only.
const TailerCostEvents = "agent_events_cost"

// unknownOrgID is the org-ID label value for spend that cannot be attributed
// to a known subscription: a row with a SQL NULL account_name (written before
// #2714), or an account name with no usage snapshot and therefore no known
// org ID. Both edge cases are explicit ACs of #2704 — attribute, never drop.
const unknownOrgID = "unknown"

// Token-kind label values for prism_model_tokens_total{kind}.
const (
	tokenKindInput      = "input"
	tokenKindOutput     = "output"
	tokenKindCacheRead  = "cache_read"
	tokenKindCacheWrite = "cache_write"
)

// splitModelID splits a "provider/modelID" string into its two parts. With
// no "/", the whole string is the model and the provider is empty — the same
// rule prism stats' splitModel uses, so the two agree on the label values.
func splitModelID(model string) (provider, modelID string) {
	if i := strings.IndexByte(model, '/'); i >= 0 {
		return model[:i], model[i+1:]
	}
	return "", model
}

// accountResolver reads the usage snapshots at scrape time and answers two
// questions: what org ID does a given account NAME map to, and what is the
// set of known accounts (for prism_account_info). It holds no state beyond
// the directory path, so it always reflects the snapshots on disk right now
// — a fresh account or a rename is picked up on the next scrape.
type accountResolver struct {
	// usageDir is ~/.local/state/prism/usage. Empty disables resolution: the
	// resolver then reports no accounts and every name folds to "unknown".
	usageDir string
}

// snapshots reads every per-account usage snapshot in usageDir. current.json
// is skipped: it is a byte-identical copy of the active account's snapshot
// (see internal/usage), so counting it would double one account.
//
// Every failure — missing directory, unreadable file, malformed JSON — is
// swallowed and the file is skipped. The exporter must never fail a scrape
// because a snapshot is absent or corrupt; the effect is only that the
// affected account resolves to "unknown".
func (r *accountResolver) snapshots() []usage.Snapshot {
	if r.usageDir == "" {
		return nil
	}
	entries, err := os.ReadDir(r.usageDir)
	if err != nil {
		return nil
	}
	var out []usage.Snapshot
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == usage.CurrentFileName || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(r.usageDir, name))
		if err != nil {
			continue
		}
		var snap usage.Snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			continue
		}
		out = append(out, snap)
	}
	return out
}

// orgIDByName maps account NAME -> org ID, for every snapshot that carries a
// non-empty org ID. A name absent from this map — including the empty name of
// a NULL account_name — resolves to "unknown".
func (r *accountResolver) orgIDByName() map[string]string {
	m := make(map[string]string)
	for _, s := range r.snapshots() {
		if s.Account != "" && s.OrganizationID != "" {
			m[s.Account] = s.OrganizationID
		}
	}
	return m
}

// orgIDForName folds an account name to its org-ID label value using a
// pre-built name->org map. It is the single point where "unknown" is
// assigned, so the two edge-case ACs (NULL account_name; a name with no
// snapshot) share one code path.
func orgIDForName(byName map[string]string, name string) string {
	if org, ok := byName[name]; ok && org != "" {
		return org
	}
	return unknownOrgID
}

// accountCounter is a labelled counter whose first exposed label is
// account_org_id, resolved at scrape time from the account NAME the value was
// accumulated under.
//
// Internally it is a metrics.CounterVec whose first label holds the raw
// account name (a stable per-series identity). Collect() translates that name
// to an org ID via the resolver and merges every series that resolves to the
// same org ID by summing them — which is what keeps a rename from resetting a
// counter. Snapshot/Restore delegate to the inner vec, so persistence keys on
// the name and a restart (or an account rename between restarts) restores the
// same series identities.
type accountCounter struct {
	name       string
	help       string
	restLabels []string
	inner      *metrics.CounterVec
	orgIDs     func() map[string]string
}

// newAccountCounter builds an account-attributed counter. restLabels are the
// labels AFTER account_org_id (e.g. {"provider","model_id"}); the emitted
// label set is account_org_id followed by restLabels. orgIDs returns the
// current name->org map, called once per Collect.
func newAccountCounter(name, help string, restLabels []string, orgIDs func() map[string]string) *accountCounter {
	innerLabels := append([]string{"account_name"}, restLabels...)
	return &accountCounter{
		name:       name,
		help:       help,
		restLabels: restLabels,
		inner:      metrics.NewCounterVec(name, help, innerLabels),
		orgIDs:     orgIDs,
	}
}

// add accumulates delta into the series for (accountName, rest...). rest must
// line up with the restLabels the counter was built with. A negative or
// non-finite delta is clamped to zero rather than returned as an error: a
// single hostile or malformed payload must never wedge the tail cursor, which
// would freeze every counter behind it.
func (a *accountCounter) add(delta float64, accountName string, rest ...string) error {
	if !(delta > 0) {
		// Covers delta <= 0 and NaN. A zero add still creates the series at
		// zero, which is harmless and keeps the series present.
		delta = 0
	}
	values := append([]string{accountName}, rest...)
	return a.inner.Add(delta, values...)
}

func (a *accountCounter) Name() string       { return a.name }
func (a *accountCounter) Help() string       { return a.help }
func (a *accountCounter) Kind() metrics.Kind { return metrics.KindCounter }

// Collect resolves the account name of every inner series to an org ID and
// merges series that share one (org_id, rest...) tuple. The emitted label
// names are account_org_id followed by restLabels.
func (a *accountCounter) Collect() []metrics.Sample {
	byName := a.orgIDs()
	labelNames := append([]string{"account_org_id"}, a.restLabels...)

	type merged struct {
		values []string
		value  float64
	}
	acc := make(map[string]*merged)
	for _, s := range a.inner.Collect() {
		// s.LabelValues[0] is the raw account name; the rest line up with
		// restLabels.
		orgID := orgIDForName(byName, s.LabelValues[0])
		out := make([]string, 0, len(s.LabelValues))
		out = append(out, orgID)
		out = append(out, s.LabelValues[1:]...)

		key := strings.Join(out, "\x00")
		if m, ok := acc[key]; ok {
			m.value += s.Value
			continue
		}
		acc[key] = &merged{values: out, value: s.Value}
	}

	samples := make([]metrics.Sample, 0, len(acc))
	for _, m := range acc {
		names := make([]string, len(labelNames))
		copy(names, labelNames)
		samples = append(samples, metrics.Sample{
			LabelNames:  names,
			LabelValues: m.values,
			Value:       m.value,
		})
	}
	return samples
}

// Snapshot implements metrics.Persistent by delegating to the inner vec, so
// the persisted keys are the raw account names — stable across a restart and
// across an account rename.
func (a *accountCounter) Snapshot() map[string]float64 { return a.inner.Snapshot() }

// Restore implements metrics.Persistent by delegating to the inner vec.
func (a *accountCounter) Restore(values map[string]float64) error { return a.inner.Restore(values) }

// accountInfoCollector emits prism_account_info: one series per distinct
// account org ID among the known snapshots, value 1, carrying the readable
// account name and workspace ID for the join.
//
// It groups by org ID rather than by snapshot file so a rename — which leaves
// the old snapshot in place beside the new one, both mapping to one org ID —
// produces exactly one series for that subscription, carrying the newest
// name. That is what makes "a rename changes only the account label" hold
// while the counter series keep their org_id identity.
type accountInfoCollector struct {
	resolver *accountResolver
}

func (c *accountInfoCollector) Name() string       { return MetricAccountInfo }
func (c *accountInfoCollector) Kind() metrics.Kind { return metrics.KindGauge }
func (c *accountInfoCollector) Help() string {
	return "One per known prism account: maps the account org ID to its readable name and workspace ID. Always 1; the labels carry the value."
}

func (c *accountInfoCollector) Collect() []metrics.Sample {
	// Group snapshots by org ID (empty -> "unknown"), keeping the one with
	// the latest captured_at as the representative. captured_at is RFC3339 in
	// UTC (usage.FormatCapturedAt), so a lexicographic max is a chronological
	// max. On a tie the greater account name wins, purely for determinism.
	type rep struct {
		account    string
		workspace  string
		capturedAt string
	}
	byOrg := make(map[string]*rep)
	for _, s := range c.resolver.snapshots() {
		orgID := s.OrganizationID
		if orgID == "" {
			orgID = unknownOrgID
		}
		cand := rep{account: s.Account, workspace: s.WorkspaceID, capturedAt: s.CapturedAt}
		cur, ok := byOrg[orgID]
		if !ok {
			byOrg[orgID] = &cand
			continue
		}
		if cand.capturedAt > cur.capturedAt ||
			(cand.capturedAt == cur.capturedAt && cand.account > cur.account) {
			byOrg[orgID] = &cand
		}
	}

	labelNames := []string{"account_org_id", "account", "workspace_id"}
	samples := make([]metrics.Sample, 0, len(byOrg))
	for orgID, r := range byOrg {
		names := make([]string, len(labelNames))
		copy(names, labelNames)
		samples = append(samples, metrics.Sample{
			LabelNames:  names,
			LabelValues: []string{orgID, r.account, r.workspace},
			Value:       1,
		})
	}
	return samples
}

// costCounters holds the three account-attributed counters and the dispatch
// the tailer applies to each costEvent.
type costCounters struct {
	modelCost      *accountCounter
	modelTokens    *accountCounter
	spendByProfile *accountCounter
}

// newCostCounters constructs, registers, and returns the three #2704
// counters and the prism_account_info gauge. resolver is shared by all four
// so they read one consistent view of the accounts on each scrape.
func newCostCounters(reg *metrics.Registry, resolver *accountResolver) *costCounters {
	cc := &costCounters{
		modelCost: newAccountCounter(
			MetricModelCostUSDTotal,
			"Total USD cost of agent turns observed by the exporter, by account org ID, provider, and model.",
			[]string{"provider", "model_id"},
			resolver.orgIDByName,
		),
		modelTokens: newAccountCounter(
			MetricModelTokensTotal,
			"Total tokens consumed by agent turns observed by the exporter, by account org ID, provider, model, and token kind (input|output|cache_read|cache_write).",
			[]string{"provider", "model_id", "kind"},
			resolver.orgIDByName,
		),
		spendByProfile: newAccountCounter(
			MetricSpendByProfileUSDTotal,
			"Total USD cost of agent turns observed by the exporter, by account org ID and spawn profile.",
			[]string{"profile"},
			resolver.orgIDByName,
		),
	}
	reg.MustRegister(cc.modelCost)
	reg.MustRegister(cc.modelTokens)
	reg.MustRegister(cc.spendByProfile)
	reg.MustRegister(&accountInfoCollector{resolver: resolver})
	return cc
}

// apply is the tailcursor apply function for the cost tailer. Every counter
// value comes from this one tailed row, never from an aggregate.
func (cc *costCounters) apply(ev costEvent) error {
	// A row with no model carries no attributable spend. prism stats'
	// collectModelMetrics skips these too, so skipping keeps the two in
	// agreement.
	if ev.Model == "" {
		return nil
	}
	provider, modelID := splitModelID(ev.Model)

	// USE pricing.Cost, never ModelCosts directly: it folds in the
	// event-reported-cost fallback for models absent from the table (e.g.
	// openrouter/*), which is what keeps this figure equal to prism stats'.
	cost := pricing.Cost(ev.Model, ev.InputTokens, ev.OutputTokens, ev.CacheRead, ev.CacheWrite, ev.EventCost)

	account := ev.AccountName // raw name; "" for a NULL account_name row.

	if err := cc.modelCost.add(cost, account, provider, modelID); err != nil {
		return err
	}
	if err := cc.modelTokens.add(ev.InputTokens, account, provider, modelID, tokenKindInput); err != nil {
		return err
	}
	if err := cc.modelTokens.add(ev.OutputTokens, account, provider, modelID, tokenKindOutput); err != nil {
		return err
	}
	if err := cc.modelTokens.add(ev.CacheRead, account, provider, modelID, tokenKindCacheRead); err != nil {
		return err
	}
	if err := cc.modelTokens.add(ev.CacheWrite, account, provider, modelID, tokenKindCacheWrite); err != nil {
		return err
	}
	if err := cc.spendByProfile.add(cost, account, ev.ProfileName); err != nil {
		return err
	}
	return nil
}
