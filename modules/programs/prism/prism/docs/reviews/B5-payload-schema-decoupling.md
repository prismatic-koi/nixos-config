# B.5 — Payload schema decoupling proposal

Track B (harness) Wave 2 follow-up to [`B1-harness-transport-and-lifecycle-assumptions.md`](B1-harness-transport-and-lifecycle-assumptions.md).
This document is the schema-track lens on the architecture inventory at
[`../architecture-inventory.md`](../architecture-inventory.md), specifically
§3.13 (payload index), §7.19 (payload coupling), and §11.8 (DB layer).

This is a **proposal document**. It articulates three options for handling the
schema delta when a non-opencode harness (specifically PI per RFC #606) starts
emitting events into `agent_events`. It does **not** implement any of them —
implementation is gated on a downstream decision.

## Context recap

`internal/payload/payload.go` defines a fixed set of canonical Go structs
(`StateChange`, `MsgUser`, `MsgAssistant`, `ToolCall`, `ToolResult`,
`PermissionAsk`, `PermissionDenied`, `Thinking`, `Compaction`, `ErrorEvent`,
`SubagentStart`, `SubagentEnd`, `DoomLoopDetected`, `Audit`). The package
header is explicit: "JSON field names match the plugin's output verbatim
(camelCase)" — and the plugin in question is opencode's TypeScript
prism-hooks plugin. Every payload row written into `agent_events.payload` is a
JSON object whose keys line up exactly with one of these structs.

B.1 classified `internal/payload/payload.go` as `[opencode-only] /
[transport-agnostic]`: the schema coupling is to opencode's plugin output
shape, not to HTTP. This is important framing for B.5 — the payload schema
decision is **independent** of the transport-shape decisions B.2/B.3/B.4 are
working through. A PI adapter that speaks JSONL-over-stdio still has to
decide what shape to write into `agent_events.payload`, and that is the
question this document is about.

The consumer surface that reads `agent_events.payload` today (from the
inventory and a `grep payload\.` pass over the source tree):

- `cmd/checkin.go` — unmarshals `MsgUser`, `MsgAssistant`, `StateChange`,
  `ToolCall`, `ToolResult`, `PermissionAsk`, `PermissionDenied`, `Thinking`
  for the human-readable session timeline.
- `cmd/stats.go` — unmarshals `MsgAssistant` (for tokens, cost, duration,
  TTFT, context-window-pct, agent, model), `ToolCall` (for tool name and
  duration), `SubagentEnd` (for subagent invocation tracking), plus
  `DoomLoopDetected`, `PermissionDenied`, `PermissionAsk` for the various
  `prism stats --…` table renderers.
- `cmd/audit.go` — unmarshals `Audit` for the `prism audit` table.
- `cmd/event_doomloop_test.go` — unmarshals `DoomLoopDetected` round-trip
  in tests.
- `internal/payload/payload_test.go` — round-trip tests for every struct.
- `internal/dashboard/` — does **not** unmarshal any payload struct. The
  dashboard reads `agent_status` (and a sentinel push channel for refresh
  triggers) and never inspects `agent_events.payload`. It is therefore
  payload-shape-agnostic.
- `internal/db/db.go` — `QueryEvents`, `QueryEventsByMessageIDs`,
  `AllSessionEvents` return raw `Event` rows (the payload is opaque
  `string`); `QueryDoomLoopEvents`, `QueryPermissionEvents`,
  `QueryAuditEvents` filter by `type` (and `QueryAuditEvents` also runs a
  `JSON_EXTRACT(payload, '$.command') LIKE …` filter at SQL level — the
  only consumer that reaches into payload field names from inside SQL).

For background and motivation, see also:
- Inventory §3.13, §7.19, §11.8 (paths above).
- B.1 transport-and-lifecycle review (sibling document).
- Design issue #1072 (parent design doc for the narrow review series).
- RFC #606 (PI coding agent support — Phase 1/2/3 plan; the source of the
  imminent second harness).
- RFC #691 (multi-harness support — the broader design context).

## What PI's event shape probably looks like

`[uncertain — the worker has not read PI's source. RFC #606 describes PI as
having "JSON Lines event stream" with "25+ extension hooks including
`tool_call`, `tool_result`, `before_agent_start`, `context`,
`before_provider_request`, `message_*`, `compaction_*`, and more". The
exact field shape per event is not knowable from the prism source or RFC
#606 alone — confirming each per-event-type schema below requires reading
PI's TypeScript source, running PI in `--mode rpc`, and capturing the
emitted JSONL frames.]`

What can be inferred from RFC #606 and the comparison summary it carries:

- PI emits structured events (it has a JSONL event stream and named hook
  points). That is consistent with payload structs being possible at all.
- PI's hook names overlap with opencode's event names (`tool_call`,
  `tool_result`, `message_*`, `compaction_*`) but are not guaranteed
  field-by-field identical. RFC #606 calls the extension API a "net upgrade
  over opencode's handful" — a richer set of hooks usually means richer
  per-event payloads, not poorer ones, but also means the field set per event
  may not align cleanly.
- PI has no built-in permission system (RFC #606 lists this as a "notable
  absence vs opencode"). That means PI does not natively emit a
  `permission_ask` / `permission_denied` event in the opencode sense. Any
  permission-style event for PI would be synthesised by a PI extension,
  which is free to choose its own field shape.
- PI has no built-in agent/persona system. That means the `agent` field on
  `MsgAssistant` (used today by checkin to detect subagent runs and by stats
  to attribute coordinator turns) does not have a natural producer in PI
  unless an extension is wired to populate it.
- PI's `compaction_*` hook may emit two events (start/end?) where opencode
  emits one. `[uncertain]`
- PI may emit token / cost / duration metadata in a different shape, or
  under different field names, or not at all for all providers. Opencode's
  shape (`inputTokens`/`outputTokens`/`cacheReadTokens`/`cacheWriteTokens`
  with `cost` reported per-turn) is one specific provider-aggregation choice.
  `[uncertain — PI's metadata shape per `before_provider_request` /
  `message_*` hook is not knowable without inspecting PI directly]`

Throughout this document, `[uncertain]` flags mark places where the worker
would need to inspect PI's source / output to confirm. They are deliberately
preserved rather than guessed — guessing here has zero cost-benefit because
the choice between A/B/C should be informed by real PI traces, not by the
worker's hypothesis of what PI emits.

## The three options

### Option A — Translate (the PI adapter normalises to opencode shape)

The PI harness adapter (a sibling of `internal/harness/opencode/`) consumes
PI's JSONL events and, for each event it intends to surface, constructs a
payload struct from `internal/payload/` and writes it to `agent_events`
with the **same** `type` value the opencode adapter would have used. The
payload schema in `internal/payload/` is unchanged. Consumers (`checkin`,
`stats`, `audit`, query helpers) are unchanged. The translation surface
lives entirely inside `internal/harness/pi/` (or wherever the adapter ends
up), and is the one place that needs to know both schemas.

The trade-off here is symmetry: every consumer is free, but the adapter
carries the full burden of the impedance mismatch. Where PI emits a field
that has no opencode equivalent (e.g. a `before_provider_request` payload
that includes the provider's request body) the adapter must drop it,
synthesise a stub, or extend the existing struct with an `omitempty` field.
Where opencode emits a field that PI does not provide (e.g. `cost` from
opencode's per-provider table), the adapter writes `0` and the consumer
silently treats it as "not available" — which already works today for
opencode pre-enrichment events. Where the conceptual event shape differs
(e.g. PI's two-event `compaction_start`/`compaction_end` collapsed into a
single opencode-style `compaction` event), the adapter has to make a
synthesis call.

#### Concrete example: `MsgAssistant` under Translate

Opencode's `MsgAssistant` payload (from `internal/payload/payload.go:54-67`):

```json
{
  "messageId": "msg_01HXYZ...",
  "text": "I'll start by reading the file...",
  "agent": "worker",
  "model": "github-copilot/claude-sonnet-4.6",
  "inputTokens": 12453,
  "outputTokens": 187,
  "cacheReadTokens": 9001,
  "cacheWriteTokens": 0,
  "durationMs": 4521,
  "ttftMs": 612,
  "contextWindowPct": 6.2,
  "cost": 0.0
}
```

A best-effort hypothetical PI emission for the same logical assistant turn
might look like `[uncertain — exact shape requires inspecting PI's source]`:

```json
{
  "type": "message_complete",
  "id": "msg-7c3a",
  "role": "assistant",
  "content": [{"type": "text", "text": "I'll start by reading..."}],
  "usage": {
    "input_tokens": 12453,
    "output_tokens": 187,
    "cache_read_input_tokens": 9001,
    "cache_creation_input_tokens": 0
  },
  "stop_reason": "end_turn",
  "model": "claude-sonnet-4-5",
  "provider": "anthropic",
  "elapsed_ms": 4521
}
```

Under Option A, the PI adapter receives that JSONL frame and writes:

```json
{
  "messageId": "msg-7c3a",
  "text": "I'll start by reading...",
  "agent": "",
  "model": "anthropic/claude-sonnet-4-5",
  "inputTokens": 12453,
  "outputTokens": 187,
  "cacheReadTokens": 9001,
  "cacheWriteTokens": 0,
  "durationMs": 4521,
  "ttftMs": 0,
  "contextWindowPct": 0.0,
  "cost": 0.0
}
```

with `type = "msg_assistant"`. Note the joins the adapter has to perform:
flattening `content[].text`, joining `provider` + `model` into the
`"providerID/modelID"` shape opencode uses, rebuilding `model` to match the
opencode convention, accepting `agent = ""` because PI has no built-in
persona concept (RFC #606 explicitly calls this out), accepting `ttftMs = 0`
because PI's JSONL may not surface time-to-first-token at the same point in
its lifecycle, accepting `contextWindowPct = 0.0` because computing it
requires knowing the model context window which is a separate lookup, and
accepting `cost = 0.0` so the stats pipeline falls back to the local
pricing table. Each accepted-zero is information that PI may genuinely have
but in a shape the opencode struct can't carry without modification — the
information loss is silent at the consumer.

### Option B — Widen (payload structs grow to accommodate both shapes)

The `internal/payload/` structs gain optional fields (or, more invasively,
union types) that hold harness-specific data alongside the existing
opencode-shaped fields. Both adapters write the union of fields they have;
neither adapter needs to translate. Consumers branch on which fields are
non-zero (or on a `Harness` discriminator they pull from the parent
`Event` row's `harness_session_id` → `agent_status.harness` join). The
schema becomes the open union of every harness's payload shape — and
remains so forever, because removing a widened field once a consumer reads
it is a breaking change.

The trade-off is the inverse of Translate: the schema absorbs the full
impedance mismatch so that no information is lost, but every consumer now
carries branching logic, and the schema gradually accumulates fields that
are populated by exactly one harness. There is also a real risk of
"semantic union" — two harnesses both populate a field but with subtly
different meanings (e.g. opencode's `durationMs` is the message wall-clock
end-to-end; a hypothetical PI `durationMs` might be just the
provider-request portion). That kind of mismatch survives the type system
but breaks every aggregation in `cmd/stats.go` silently.

#### Concrete example: `MsgAssistant` under Widen

The struct gains optional fields for both shapes:

```go
type MsgAssistant struct {
    // Existing opencode-shape fields (unchanged).
    MessageID        string  `json:"messageId"`
    Text             string  `json:"text"`
    Agent            string  `json:"agent"`
    Model            string  `json:"model"`
    InputTokens      int     `json:"inputTokens,omitempty"`
    OutputTokens     int     `json:"outputTokens,omitempty"`
    CacheReadTokens  int     `json:"cacheReadTokens,omitempty"`
    CacheWriteTokens int     `json:"cacheWriteTokens,omitempty"`
    DurationMs       int64   `json:"durationMs,omitempty"`
    TtftMs           int64   `json:"ttftMs,omitempty"`
    ContextWindowPct float64 `json:"contextWindowPct,omitempty"`
    Cost             float64 `json:"cost,omitempty"`

    // PI-shape additions (illustrative; populated only by PI adapter).
    Provider          string `json:"provider,omitempty"`
    StopReason        string `json:"stopReason,omitempty"`
    ProviderRequestMs int64  `json:"providerRequestMs,omitempty"`
}
```

Opencode emits the existing fields with `Provider`/`StopReason`/
`ProviderRequestMs` left empty. PI's adapter emits its native fields **plus**
fills in the opencode-shape fields where it can derive them (e.g. it can
populate `InputTokens` from PI's `usage.input_tokens` directly, and may set
`Provider = "anthropic"` and `Model = "claude-sonnet-4-5"` separately rather
than packing them into the `"providerID/modelID"` shape). `cmd/stats.go`
reads `InputTokens` for both — no branching needed. But anything that wanted
to surface `Provider` separately (e.g. a future `prism stats --by-provider`
view) now branches: opencode events have `Provider == ""` so they need a
fallback derivation from the slash-shaped `Model`, while PI events have
`Provider != ""`. Multiply that pattern across every event type and the
consumer surface gets noisier with every new harness. The schema becomes a
historical archaeology of harnesses-that-once-existed.

### Option C — Version (per-harness schema selected at parse time)

`agent_events` gains a `payload_version` (or, more naturally, the existing
`harness` column on `agent_status` is joined in at query time and used as
the version discriminator) column or column-equivalent. Each harness writes
its own native payload shape into `agent_events.payload`; consumers
dispatch to the right struct based on the discriminator. `internal/payload/`
splits into per-harness packages (`internal/payload/opencode/`,
`internal/payload/pi/`) plus a thin `payload.Common` interface or
generic-shaped accessor for the small subset of fields every consumer
actually needs.

The trade-off is the inverse of Widen: the schema stays clean per harness,
but every consumer becomes harness-aware (or sits behind an accessor that
hides the harness — which is just Translate-at-read-time, with the cost of
still doing the translation on every read instead of once at write). New
harnesses no longer pollute existing consumers' branches, but they do
require a new adapter package and a new dispatch arm in every consumer.
Comparison queries — "show me all assistant turns sorted by token usage,
across both harnesses" — need a normalisation pass either in the SQL layer
(`JSON_EXTRACT` per-version) or in Go, because the field paths differ.

#### Concrete example: `MsgAssistant` under Version

`internal/payload/opencode/payload.go` keeps the existing struct unchanged.
A new `internal/payload/pi/payload.go` defines:

```go
package pi

type MsgAssistant struct {
    ID         string         `json:"id"`
    Role       string         `json:"role"`
    Content    []ContentPart  `json:"content"`
    Usage      Usage          `json:"usage"`
    StopReason string         `json:"stop_reason"`
    Model      string         `json:"model"`
    Provider   string         `json:"provider"`
    ElapsedMs  int64          `json:"elapsed_ms"`
}

type ContentPart struct {
    Type string `json:"type"`
    Text string `json:"text,omitempty"`
}

type Usage struct {
    InputTokens             int `json:"input_tokens"`
    OutputTokens            int `json:"output_tokens"`
    CacheReadInputTokens    int `json:"cache_read_input_tokens"`
    CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}
```

`cmd/stats.go`'s `msg_assistant` arm becomes:

```go
case "msg_assistant":
    switch harness {
    case "opencode":
        var p opencode.MsgAssistant
        if err := json.Unmarshal([]byte(e.Payload), &p); err == nil {
            m.InputTokens += p.InputTokens
            // ... existing logic ...
        }
    case "pi":
        var p pi.MsgAssistant
        if err := json.Unmarshal([]byte(e.Payload), &p); err == nil {
            m.InputTokens += p.Usage.InputTokens
            // ... pi-specific logic ...
        }
    }
```

The wire shape is faithful to each harness, but every consumer now has a
switch (today there are roughly seven distinct unmarshal sites for
`MsgAssistant` across `cmd/checkin.go` and `cmd/stats.go`, each of which
would gain the same switch). The query layer also has to either join in
`agent_status.harness` per row or denormalise it onto `agent_events`
itself, which is a schema migration of its own.

## Per-option consumer-surface impact

Coverage: every consumer in `cmd/`, `internal/dashboard/`, and
`internal/db/` that touches payload data. Sites with no payload contact are
omitted (the dashboard, listed for completeness, does not unmarshal payload
JSON today).

| Consumer | Translate (A) | Widen (B) | Version (C) |
|---|---|---|---|
| `cmd/checkin.go` (timeline render: `MsgUser`, `MsgAssistant`, `StateChange`, `ToolCall`, `ToolResult`, `PermissionAsk`, `PermissionDenied`, `Thinking`) | Unchanged. All seven unmarshal sites continue to read the same struct. | Unchanged surface, but `MsgAssistant.Agent` may be `""` for PI-sourced events (no persona system) — the existing subagent-collapse logic at `:317-335` already handles `entryAgent == ""` by treating the entry as a root-agent entry, so this degrades gracefully. New widened fields (`Provider`, `StopReason`, …) are simply not consulted unless a future feature wants them. | Every unmarshal site grows a per-harness switch. Roughly 12-16 sites in `checkin.go` (per the `payload\.` grep). The `MsgAssistant.Agent` lookup in `isSubagentEntry` becomes a per-harness function: opencode reads `.Agent`; PI either has no equivalent or reads from a derived field; checkin must paper over the difference. |
| `cmd/stats.go` (token / cost / duration / TTFT / context-window / agent / model aggregation, plus per-tool counts and durations, subagent invocations, doom-loop / denial / ask tables) | Unchanged. `cost`, `ttftMs`, and `contextWindowPct` will be `0` for PI events — already a documented "not available" sentinel in the struct doc. The local pricing table fallback for `cost` already exists and will engage automatically. | Unchanged for the existing aggregations. Future "by-provider" / "by-stop-reason" cuts of stats become possible without further migration but require adding new aggregation arms. Risk: a `DurationMs` populated by PI with provider-request-only semantics rather than full-message semantics would silently bias `TurnDurations`. | Every aggregation arm gains a per-harness switch. The biggest concrete pain points: `m.InputTokens += p.InputTokens` becomes harness-aware; `m.ToolCalls[p.Tool]++` must reach into PI's tool-name field which may live under a different key; `coordinatorModel` detection (which keys off `p.Agent == "coordinator"`) needs a PI-equivalent (which doesn't natively exist). Roughly 7 dispatch sites; each gains a switch. |
| `cmd/audit.go` | Unchanged. PI emits `audit` only if a PI extension synthesises one (PI has no native equivalent of opencode's bash-tool audit-promotion). The fields the table renders (`SessionName`, `Command`) are easy to populate from a synthesised PI event. | Same as Translate; `Audit` is small enough that widening adds no real branching. | Per-harness `Audit` struct in each package. The `JSON_EXTRACT(payload, '$.command') LIKE …` filter in `QueryAuditEvents` (db.go:2186) is the catch — it assumes the JSON path. If PI's audit shape uses a different path the SQL filter has to be per-harness too. |
| `internal/dashboard/` | Unchanged (does not unmarshal payload). | Unchanged. | Unchanged. |
| `internal/db/QueryEvents`, `QueryEventsByMessageIDs`, `AllSessionEvents` | Unchanged. Returns opaque payload strings; consumers unmarshal. | Unchanged. | Unchanged signature, but consumers downstream of these methods must know the harness — which means either threading `harness` through every caller or doing a per-row join against `agent_status.harness` here. |
| `internal/db/QueryDoomLoopEvents` | Unchanged. | Unchanged. | Unchanged signature; doom-loop events are produced exclusively by the prism-hooks plugin (opencode-side), so unless a PI extension also emits them the per-harness split is moot. `[uncertain — whether a PI-side doom-loop detector is in scope]` |
| `internal/db/QueryPermissionEvents` | Unchanged signature. PI has no built-in permission system (RFC #606), so this query returns only opencode rows unless a PI extension synthesises permission events. | Same as Translate. | Same — but if a PI extension does emit them, the harness discriminator needs to flow through to the unmarshal site. |
| `internal/db/QueryAuditEvents` | Unchanged. The `JSON_EXTRACT(payload, '$.command')` filter at `db.go:2186` keeps working because PI events also use `command`. | Unchanged. The filter still works because both shapes put `command` at the same path. | The `JSON_EXTRACT` filter must either become harness-aware (different path per harness) or the filter has to be applied in Go after fetching all rows, which trades SQL pushdown for portability. |
| `cmd/event_doomloop_test.go`, `internal/payload/payload_test.go` | Unchanged. | Tests for new optional fields needed; existing round-trip tests still pass. | Tests fork per-harness; both packages need their own round-trip suites. |

The pattern across the table: Translate buys the fewest consumer changes
but the most adapter-side translation work and the most silent information
loss. Widen buys zero consumer changes for existing aggregations but pushes
all future-aware consumers into branching, and risks semantic-union
mismatches that the type system can't catch. Version buys faithful
per-harness schemas but multiplies every existing unmarshal site by the
number of harnesses, and forces a SQL-layer decision about the audit
`JSON_EXTRACT` filter.

## Per-event-type compatibility notes

This section walks the five event types named in the AC list. For each, the
question is: "if the PI adapter were writing this event today, how cleanly
does each option fare?" — focused on whether information is lost (Translate),
whether the schema needs widening that helps no current consumer (Widen),
or whether a fresh per-harness struct is genuinely justified (Version).

### `MsgAssistant`

Already covered in detail above as the option-comparison example, so this
note is short. The opencode-side struct carries 12 fields, several of which
(`cost`, `ttftMs`, `contextWindowPct`, `cacheReadTokens`/`cacheWriteTokens`)
exist for opencode-specific reasons and may not have direct PI analogues.
**Translate-friendly**, with documented information loss for `cost`,
`ttftMs`, `contextWindowPct`. **Widen-justified** if surfaces like
`provider` or `stopReason` become useful per-harness display fields.
**Version-heavy** because of the seven unmarshal sites — the most
expensive event type to version. `[uncertain — PI's actual per-provider
metadata shape; whether PI's TTFT exists as a discrete field]`

### `ToolCall`

Three fields: `Tool`, `Args`, `MessageID`, plus optional `DurationMs`. PI's
extension API includes a `tool_call` hook (RFC #606). The minimal shape
(tool name + args + which message it belongs to) is highly portable across
harnesses. **Translate-friendly**: a PI adapter just needs to flatten PI's
input args into the same string-shaped `args` field opencode uses (today
opencode's `marshalTruncated(input, 500)` produces a compact JSON-string
representation; PI's adapter can do the same). The `Tool` field is the
biggest semantic concern — opencode tool names (`bash`, `read`, `write`,
`edit`, `webfetch`) overlap with PI's built-in tools by name (RFC #606
lists 6/8 prism-core tools as natively present in PI), so for those tools
the field is already harmonised. For tools that exist only in one
harness, the adapter writes the harness-native name and the consumer
displays it as-is. **Widen-not-justified**: nothing material to add.
**Version-not-justified**: the schemas would be near-identical, so the
versioning machinery would buy nothing.

### `ToolResult`

Three fields: `Tool`, `Result`, `MessageID`. Even more portable than
`ToolCall` — the result is already a truncated string in opencode. PI's
`tool_result` hook should map cleanly. **Translate-friendly**;
**Widen-not-justified**; **Version-not-justified**. The only nuance is
that opencode's truncation budget (500 chars per `truncate` call in
sidecar.go:1609) is opencode-side; the PI adapter would need to apply its
own truncation to keep DB rows bounded, but that is a quality-of-service
detail not a schema decision.

### `Audit`

Five fields: `Tool`, `Command`, `SessionName`, `HarnessSessionID`,
`MessageID`. The current write site is opencode-specific — it fires only
for `part.Tool == "bash"` and only when `isHighImpactCommand(cmd)`
matches. PI has no built-in equivalent of opencode's bash-tool promotion;
audit events for PI sessions would have to be synthesised by a PI
extension that wraps PI's bash equivalent in the same high-impact-pattern
match. The struct itself is harness-agnostic in shape (every field has
direct PI analogues: PI has tool names, command strings, session names,
and message IDs). **Translate-friendly** assuming a PI extension exists to
emit them; **Widen-not-justified**; **Version-not-justified** for the
struct itself, but the `JSON_EXTRACT(payload, '$.command') LIKE …` filter
in `QueryAuditEvents` (db.go:2186) is a coupling point that any of the
three options inherits — if the field name ever changed, the SQL would
need updating. `[uncertain — whether high-impact-pattern detection should
live in the PI extension or in shared Go code; if shared, the audit event
shape needs to remain a stable interface across harnesses]`

### `PermissionAsk`

Three fields: `Tool` (a `PermissionToolName` with custom `UnmarshalJSON`
for legacy compatibility), `Patterns` (a slice), `MessageID`. **PI has no
built-in permission system.** RFC #606 calls this out explicitly: "no
built-in permission system" is a "notable absence vs opencode". So
`PermissionAsk` events for PI sessions can only be emitted if a PI
extension implements a permission flow on top of PI's `tool_call` hook
(per RFC #606's "needs adaptation" row in the compatibility table).
**Translate-friendly** if such an extension exists and synthesises the
opencode-shaped fields; **Widen-not-justified** unless PI's hypothetical
permission-extension wants to surface fields opencode doesn't (e.g. a
sandboxing decision rationale, since RFC #606 mentions PI offers OS-level
sandboxing); **Version-not-justified** if the PI-side permission flow
can be designed to produce the same shape from the start. The
`PermissionToolName.UnmarshalJSON` legacy-object handling is opencode-
plugin-specific (legacy DB rows); a PI source would never produce the
legacy shape, so that quirk is opencode-history baggage either way.
`[uncertain — whether a PI-side permission extension is in scope for
prism, and if so what its native event shape would be]`

## Cross-cutting concerns

### Field-name camelCase vs snake_case

Opencode's plugin emits camelCase (`messageId`, `inputTokens`,
`durationMs`). PI's broader ecosystem typically uses snake_case
(`message_id`, `input_tokens`, `elapsed_ms`). Under Translate, the PI
adapter normalises to camelCase at write time. Under Widen, the struct's
JSON tags are camelCase (existing) and PI's adapter converts on the way
in — same translation cost as Translate, just for a smaller subset of
fields. Under Version, each per-harness package can use the harness's
native casing — but every consumer that does cross-harness reasoning
(stats aggregation, future comparisons) has to be aware of which casing
applies to which row. This is a recurring tax under Version, but a
one-time tax (paid in the adapter) under both Translate and Widen.

### `MessageID` correlation

Many consumers correlate events by `MessageID` (e.g. `cmd/checkin.go:706,
:714` collect children of an assistant message via the `messageId` field
on `tool_call`/`tool_result`; `internal/db/db.go:QueryEventsByMessageIDs`
exists for this lookup). Whatever option is chosen, the PI adapter must
ensure that all events emitted for one logical assistant turn carry a
stable `messageId`. **`[uncertain — whether PI's hooks expose a single
stable per-turn message ID across `message_*` and `tool_*` events]`**.
If PI's events do not naturally share a message ID across turn-boundary
events, the adapter has to synthesise one. This is an adapter-side
concern that survives all three options and may be the most consequential
unknown for the Translate/Widen options specifically (Version can paper
over it more easily by simply not exposing message-ID correlation for PI
rows in the first place — at the cost of breaking the unified timeline
view).

### `harness_session_id` vs `messageId`

`agent_events.harness_session_id` is already plural-harness-aware (it was
renamed from `opencode_sid` in migration v15, per inventory §11.8 and
db.go:246-250). All three options inherit this column unchanged, so
queries that filter by harness session work for any harness. The
discriminator for "which payload schema to parse" under Version is
**`harness`** (on `agent_status`), not `harness_session_id` — the latter
is the per-session ID, the former is the per-harness label.

### The `harness` column as ambient discriminator

`agent_status.harness` already exists with default `"opencode"`. Under
Version, this is the obvious source of truth for the parse discriminator —
no new migration needed if every parse site can join against it. Under
Translate, this column is unused for parsing (because all rows are
opencode-shaped on the wire). Under Widen, it might still be useful as a
hint to the consumer about which optional fields to expect, but is not
strictly required.

### Backward compatibility with rows already in the DB

Every prism instance has `agent_events` rows accumulated up to the 90-day
prune horizon (see `Prune` at db.go and §11.8). Those rows are opencode-
shaped under all three options:

- Translate: trivially compatible — old rows match the schema, new PI rows
  are translated to match.
- Widen: trivially compatible — old rows have the new optional fields
  unset, which `omitempty` handles.
- Version: requires either backfilling a `harness = 'opencode'` value (the
  default already does this) and accepting that historical rows are all
  opencode-shaped, or treating the absence of a discriminator as
  "opencode" by convention.

None of the three options forces a destructive migration of historical
rows.

## Recommendation

**The trade-off is real and the choice should be made downstream**, after
PI's actual JSONL event shape has been observed and at least one PI session
has run end-to-end with a stub adapter. The recommendation here is shaped
by the worker's read of the inventory and B.1's transport classification,
not by a position paper that pre-commits before evidence arrives.

That said, the worker's read of the trade-offs is:

1. **Start with Translate (Option A).** It is the lowest-risk path for
   landing PI as a second harness without a payload-layer rewrite. Every
   consumer continues to work; the translation cost is concentrated in
   one place (the PI adapter) and is the kind of bounded engineering that
   can be implemented and tested in isolation. Information loss is
   acknowledged at write time and bounded by the existing `omitempty`
   /"zero means not available" convention already documented on the
   structs (`MsgAssistant.InputTokens` doc: "Zero values mean not
   available"). RFC #606's Phase 2 explicitly says checkin will read
   "limited observability — conversation text only, no fine-grained tool
   metrics" — Translate is consistent with that scope.

2. **Treat Translate as reversible.** If, after PI runs in production, a
   pattern emerges where some specific event type carries genuinely
   irreducible PI-specific information that the consumer surface wants
   (e.g. provider attribution that doesn't fit opencode's slash-shape), an
   incremental migration to Widen for *just that field* is straightforward
   and does not invalidate any of the Translate work. Translate → Widen
   is a forward-compatible upgrade because adding optional fields is non-
   breaking.

3. **Avoid Version for the foreseeable future.** Version's per-harness
   struct split is the most architecturally pure option but it imposes a
   real cost on every consumer, every test, and every future query. With
   only two harnesses in flight (opencode now, PI imminent) the
   per-harness branching tax has poor leverage. Version becomes more
   attractive at three or more harnesses, or if any harness produces
   events with semantically-different fields under the same name (the
   semantic-union risk Widen carries). Until that happens, the simpler
   options dominate.

4. **For event types that have no native PI source** (`PermissionAsk`,
   `PermissionDenied`, `Audit`, `DoomLoopDetected`), Translate's
   "synthesised by a PI extension" path is the same path the opencode
   plugin takes today — these are not native opencode events either; they
   are produced by the prism-hooks plugin which is a separate concern
   from opencode's core event stream. Symmetry is good here: each
   harness gets a prism-side extension that emits the prism schema.

5. **The one place this recommendation would flip to Widen first** is if
   PI surfaces a piece of metadata that the existing struct genuinely
   cannot represent **and** that surfacing it is required for a Phase-2
   consumer (rather than a future feature). If RFC #606 Phase 2's checkin
   really does need to display "PI's stop reason" or "PI's provider
   string", widening `MsgAssistant` is cheaper than translating PI's data
   into a lossy opencode-shape. The worker has not been able to validate
   from the source whether this is the case — a downstream call once PI
   traces are available would settle it.

The recommendation is therefore: **adopt Translate as the default path,
explicitly document the information-loss boundary in the PI adapter, and
revisit the choice per-event-type only when a concrete PI consumer demand
emerges that Translate cannot satisfy.** This keeps the schema clean for
the immediate path and preserves optionality.

## Open questions and `[uncertain]` flags

Consolidated from the per-section flags above:

- **PI's actual per-event JSONL shape.** Cannot determine without reading
  PI's source or capturing `pi --mode rpc` output. Every per-event
  paragraph above carries this uncertainty. The shapes shown for PI in the
  examples are **best-effort extrapolations** from RFC #606 and PI's
  general ecosystem conventions, **not verified facts**.
- **PI's metadata shape per provider.** Whether PI surfaces token usage,
  cost, TTFT, and context-window utilisation in a uniform shape across
  its 20+ providers is unknown from prism-side reading.
- **PI's per-turn message-ID stability.** Whether one logical assistant
  turn produces events with a consistent `messageId` field across PI's
  `message_*` / `tool_*` hooks is unknown. This affects timeline
  correlation in `cmd/checkin.go` regardless of which option is chosen.
- **PI compaction event shape.** Whether PI emits one `compaction` event
  or two (`compaction_start` / `compaction_end`) per RFC #606's reference
  to `compaction_*` hooks is unknown. Affects whether the existing
  `payload.Compaction` struct can be translated to or whether two events
  must be reduced to one.
- **PI permission-extension scope.** Whether prism intends to ship a PI
  extension that synthesises `permission_ask` / `permission_denied` /
  `audit` events, or whether PI sessions will simply not surface these
  events in `agent_events`, is a Phase-2-or-Phase-3 decision per RFC
  #606. Affects the consumer-surface impact rows for these event types.
- **PI doom-loop detection.** Whether prism-hooks-equivalent
  doom-loop detection is in scope for the PI extension is unstated in
  RFC #606. If it is, `DoomLoopDetected` becomes a cross-harness event;
  if not, it remains opencode-only and `QueryDoomLoopEvents` stays
  effectively single-harness.
- **PI persona / agent attribution.** Whether the PI extension will
  synthesise an `agent` field on `MsgAssistant` to preserve subagent-
  collapse behaviour in `cmd/checkin.go` is unknown. RFC #606 explicitly
  calls out PI's lack of native persona system as a "needs adaptation"
  item.
- **`JSON_EXTRACT(payload, '$.command')` portability.** If PI's audit
  shape ever diverges from opencode's `$.command` path under Version,
  the `QueryAuditEvents` filter has to fork. Under Translate or Widen it
  stays unified.
- **Widen's semantic-union risk specifically.** The risk that a widened
  `DurationMs` (or any other shared numeric field) carries different
  semantics per harness has no automated mitigation — it requires
  per-field documentation and per-aggregation discipline. This is
  Widen's hidden cost and is hardest to spot in code review.

## What this proposal deliberately does not do

- It does **not** propose a registry shape — that is B.2.
- It does **not** propose a sidecar-as-launcher refactor — that is B.3.
- It does **not** propose a harness-group taxonomy — that is B.4.
- It does **not** propose an archive-pipeline refactor — that is B.6.
- It does **not** ship any code. The `internal/payload/` package, the DB
  schema, and every consumer remain untouched by this PR. The deliverable
  is the proposal itself.
- It does **not** commit to one option. The worker's recommendation is
  Translate-first, but the explicit position is that this choice is
  better made downstream once PI has produced real traces.

## Related

- Inventory: [`../architecture-inventory.md`](../architecture-inventory.md) §3.13, §7.19, §11.8.
- B.1 review: [`B1-harness-transport-and-lifecycle-assumptions.md`](B1-harness-transport-and-lifecycle-assumptions.md).
- Design: [`000-design-narrow-review-series.md`](000-design-narrow-review-series.md).
- Issue: #1083. Parent design: #1072.
- RFCs: #606 (PI coding agent support), #691 (multi-harness support).
- Sibling Track B issues: #1080 (B.2), #1081 (B.3), #1082 (B.4), #1084
  (B.6).
