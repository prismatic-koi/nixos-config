package sidecar

// Wire types for the host-API GET /stats compare/abtest views (issue #2098).
//
// These types are the JSON-serialisable shapes exchanged between the
// host sidecar and an in-sandbox `prism stats compare`, `prism stats abtest
// <group_id>`, and `prism stats --abtest` invocation. They live in the
// sidecar package because the `cmd` package already imports it for other
// host-API plumbing (review sentinels, etc.) and the structs are the
// authoritative wire contract — duplicating them in both packages would
// invite drift.
//
// The wire shape is intentionally raw: the server returns per-instance
// db.Session, db.SpawnOutcome, and a render-only subset of db.SpawnInputs
// rows; the client renders. This keeps the Δ / MIN / MAX computation and
// all flag behaviour (--diff-only, --axes, --include-inputs,
// --include-rubric) on the CLI side, so the table / JSON / CSV output is
// byte-identical between the direct-DB path and the proxy path. The
// render-time rather than server-time aggregation choice is documented in
// the PR body for #2098.
//
// Why SpawnInputs is exposed as a restricted struct rather than the raw
// db.SpawnInputs row: /stats is an all-roles endpoint (it predates
// /checkin's coordinator-only gate), and db.SpawnInputs carries
// PromptText, PromptSource, ModelVariantOverrides, Extras, and the
// skills / agent-prompt manifest hashes. None of those are read by the
// cmd-side renderer (cmd/stats_compare.go::inputsAxes), but `db.SpawnInputs`
// has no JSON tags so a raw embed would put the full row body — including
// the first user-turn prompt — on the all-roles wire. The architecture
// note next to the /stats handler in host_api.go states "/stats is
// aggregate counts, /db/query is row-level conversation content" — the
// prompt body is conversation content and crosses that boundary. The
// restricted StatsCompareInputsWire struct holds only the six fields the
// renderer actually consumes, so the all-roles policy AC from issue #2098
// is preserved without leaking conversation content to worker callers.

import "github.com/prismatic-koi/prism/internal/db"

// StatsCompareInputsWire is the wire-safe subset of db.SpawnInputs surfaced
// by view=compare and view=abtest. It carries exactly the fields the cmd-side
// `prism stats compare` renderer reads via cmd/stats_compare.go::inputsAxes:
// profile_name, harness_flag, isolation_flag, agent_flag, branch_flag, and
// abtest_pair_id. PromptText, PromptSource, ModelVariantOverrides, Extras,
// SkillsManifestHash, AgentPromptHash, ModelFlag, VariantFlag, PRNumber,
// HostModeFlag, IgnoreConcurrencyCap, PromptTemplateHash, and CreatedAt are
// intentionally excluded — they are not read by the renderer and would
// otherwise leak to worker callers of the all-roles /stats endpoint.
//
// Fields use JSON-tagged pointers so the encoder emits them as null when
// the underlying spawn_inputs column is NULL, matching the cmd-side
// inputsValue "—" fallback for missing data.
type StatsCompareInputsWire struct {
	ProfileName   *string `json:"profile_name,omitempty"`
	HarnessFlag   *string `json:"harness_flag,omitempty"`
	IsolationFlag *string `json:"isolation_flag,omitempty"`
	AgentFlag     *string `json:"agent_flag,omitempty"`
	BranchFlag    *string `json:"branch_flag,omitempty"`
	AbtestPairID  *string `json:"abtest_pair_id,omitempty"`
}

// StatsCompareRunWire is the per-run payload returned by the
// view=compare and view=abtest handlers. Each run carries the raw
// sessions / spawn_outcome rows and the restricted spawn_inputs subset
// (StatsCompareInputsWire) that the comparison engine needs; the
// renderer turns these into a labelled column.
//
// Label is assigned by the server in input order ("run-A", "run-B", ...)
// so the CLI does not need to know the input ordering after unmarshaling.
// Outcome and Inputs may be nil — a live (non-terminal) session has no
// outcome row, and pre-#2087 sessions have no spawn_inputs row.
type StatsCompareRunWire struct {
	Label   string                  `json:"label"`
	Session *db.Session             `json:"session"`
	Outcome *db.SpawnOutcome        `json:"outcome,omitempty"`
	Inputs  *StatsCompareInputsWire `json:"inputs,omitempty"`
}

// StatsCompareResponseWire is the top-level response envelope for the
// view=compare and view=abtest handlers.
type StatsCompareResponseWire struct {
	Runs []StatsCompareRunWire `json:"runs"`
}

// StatsAbtestListResponseWire is the response envelope for view=abtest_list,
// used by `prism stats --abtest`.
type StatsAbtestListResponseWire struct {
	Pairs []db.AbtestPairRow `json:"pairs"`
}
