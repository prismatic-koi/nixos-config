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
// db.Session, db.SpawnOutcome, and db.SpawnInputs rows and the client
// renders. This keeps the Δ / MIN / MAX computation and all flag
// behaviour (--diff-only, --axes, --include-inputs, --include-rubric)
// on the CLI side, so the table/JSON/CSV output is byte-identical
// between the direct-DB path and the proxy path. The render-time
// rather than server-time aggregation choice is documented in the PR
// body for #2098.

import "github.com/prismatic-koi/prism/internal/db"

// StatsCompareRunWire is the per-run payload returned by the
// view=compare and view=abtest handlers. Each run carries the raw
// sessions / spawn_outcome / spawn_inputs rows that the comparison
// engine needs; the renderer turns these into a labelled column.
//
// Label is assigned by the server in input order ("run-A", "run-B", ...)
// so the CLI does not need to know the input ordering after unmarshaling.
// Outcome and Inputs may be nil — a live (non-terminal) session has no
// outcome row, and pre-#2087 sessions have no spawn_inputs row.
type StatsCompareRunWire struct {
	Label   string           `json:"label"`
	Session *db.Session      `json:"session"`
	Outcome *db.SpawnOutcome `json:"outcome,omitempty"`
	Inputs  *db.SpawnInputs  `json:"inputs,omitempty"`
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
