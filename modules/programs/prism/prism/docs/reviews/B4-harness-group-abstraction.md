# B.4 — Harness group abstraction: HTTPHarness vs StdioHarness shared base?

Status: proposal (no code changes).
Issue: #1082.
Track: B (harness), Wave 2.
Source corpus:
[`../architecture-inventory.md`](../architecture-inventory.md) §7,
[`B1-harness-transport-and-lifecycle-assumptions.md`](B1-harness-transport-and-lifecycle-assumptions.md)
(landed in #1096),
[`B2-harness-registry-and-transport-shape.md`](B2-harness-registry-and-transport-shape.md)
(landed in #1102), and the primary source:
`internal/harness/harness.go` (interface definition) and
`internal/harness/opencode/adapter.go` (sole existing implementation).

This is a **proposal document**. It contains zero implementation work —
no Go code is written, modified, or moved by this review.

## 1. Context

The architecture inventory (§7) and RFC #691 identify two transport shapes
in prism's harness landscape:

- **HTTP+SSE** (TransportHTTPPort) — the agent runs a long-lived HTTP
  server inside the container; the sidecar dials it on a TCP port. Health
  checks are HTTP probes. Event delivery is SSE. Prompts are POSTed.
  Current member: opencode. Likely future member: Claude Code [uncertain —
  Claude Code's HTTP API surface may differ materially from opencode's;
  shape similarity must be confirmed empirically before shared machinery
  is extracted].
- **JSONL-over-stdio** (TransportStdioPipe) — the harness runs as a child
  process whose stdin/stdout the sidecar owns. Health is "the pipe is
  open." Event delivery is JSON Lines on stdout. Prompts are written to
  stdin. Current members: none implemented. Planned members: PI (RFC #606),
  Codex [uncertain — Codex's RPC protocol has not been confirmed to be
  JSON Lines over stdio; it may share less with PI than assumed].

B.1 (#1096) classified every coupling site by transport shape. B.2
(#1097) proposed a flat registry with a `TransportShape` enum so
downstream code switches on shape rather than harness name. B.4 now asks
the next-level question: **should HTTP-shaped adapters share an embedded
`HTTPHarness` base struct, and should stdio-shaped adapters share an
embedded `StdioHarness` base struct?** Or is the flat registry sufficient
and shared bases an premature abstraction?

The B.2 proposal is explicit: "The registry is a flat lookup table with a
transport-shape tag. Whether the http-shaped registrations should later
share an embedded base struct is B.4's call." This document answers that
call.

## 2. The case FOR grouping

### 2.1 Real shared machinery for HTTP-shaped harnesses

An HTTP-shaped harness adapter (opencode today, Claude Code tomorrow)
must implement at minimum:

- **HTTP client lifecycle**: holding an `*http.Client`, setting a
  timeout, handling 20x/50x status codes
  (`internal/harness/opencode/adapter.go:44-58` — `httpClient *http.Client`
  field, `healthCheckInterval`, `defaultHealthCheckTimeout` constants).
- **Health check**: polling a known HTTP endpoint with backoff until the
  server answers 200 or ctx is cancelled
  (`internal/harness/opencode/adapter.go:121-167`, the `HealthCheck`
  method: constructs `http://127.0.0.1:%d/global/health`, loops on
  `healthCheckInterval` with a deadline). A second HTTP-shaped harness
  must implement the same loop-with-backoff pattern; the only
  harness-specific bit is the URL path (`/global/health` vs whatever
  Claude Code's health endpoint is called).
- **SSE subscription**: wiring `internal/sse.Client` into `Subscribe`
  with reconnect / backoff / heartbeat semantics
  (`internal/harness/opencode/adapter.go:444-476`). The `sse.Client`
  package is already transport-agnostic relative to harness name; the
  glue code that wires it into `Subscribe` is identical across HTTP
  harnesses except for the URL path.

If Claude Code lands as a second HTTP harness without a shared base,
these three blocks (~100 LoC each in aggregate at current opencode size)
duplicate verbatim or near-verbatim. That duplication is detectable, real,
and concentrated enough that a shared base would halve it immediately.

### 2.2 Real shared machinery for stdio-shaped harnesses

A stdio-shaped harness adapter (PI, Codex) must implement:

- **Pipe lifecycle**: holding `io.ReadCloser` (stdout) and `io.WriteCloser`
  (stdin), wiring them to a child `*exec.Cmd`, tearing them down on
  context cancellation. Neither PI nor Codex adapters exist yet, but the
  plumbing is structurally identical for any harness that speaks JSONL
  over stdio; there is no harness-specific variation in pipe setup.
- **JSONL parser / emitter**: reading newline-delimited JSON frames from
  stdout into `HarnessEvent.Data`, writing JSONL to stdin for
  `DeliverPrompt`. The framing logic (split on `\n`, decode JSON, handle
  partial reads) is identical across all stdio harnesses.
- **Child-process supervision**: owning the `*exec.Cmd`, watching for
  process exit, translating non-zero exit to a sidecar error state,
  forwarding signals. Again, no harness-specific variation in supervision;
  only the process binary and flags differ.

These are low-level mechanics that belong at the shape level, not the
per-vendor level. Omitting a shared base means PI and Codex adapters each
re-implement the same pipe-and-JSONL stack independently — the kind of
duplication that causes subtle divergence (different EOF handling,
different partial-read behaviour, different goroutine lifecycle choices)
rather than intentional per-harness specialisation.

### 2.3 Transport-shape-gated callers already need a stable contract

B.2's `TransportShape` enum gated a set of downstream decisions on
shape: whether to allocate a port, whether to publish `--port` in bwrap
argv, whether to write the readiness file, whether to call `WaitHealthy`
(B.2 §6, class-3/8 migrations). Each of those decisions is currently
handled by the caller (container manager, sidecar Run loop) consulting
`harness.ShapeOf(name)`.

A shared base struct would internalise some of that contract: an
`HTTPHarness` base could expose a `BaseURL() string` and a
`DefaultHTTPClient() *http.Client` that callers use directly instead of
constructing transport details from the shape tag alone. This tightens
the contract between adapter and caller without requiring callers to
re-implement shape-inspection logic. The benefit is modest while there is
only one HTTP harness, but grows proportionally with the number of HTTP
harnesses.

## 3. The case AGAINST grouping

### 3.1 Only one harness exists per shape today — pure YAGNI

The opencode adapter (`internal/harness/opencode/adapter.go`, 667 LoC)
is the only HTTP-shaped harness. PI and Codex are not implemented. Claude
Code is not implemented. There is nothing to share the base *with*.
Extracting `HTTPHarness` and `StdioHarness` today is writing abstraction
for zero current consumers beyond the code being abstracted. YAGNI applies
in its strongest form: the second harness does not exist, and without two
concrete instances to generalise from, the proposed shared base is a
hypothesis rather than a distillation.

### 3.2 The wrong shared base is harder to remove than no shared base

When Claude Code lands, it may turn out that its HTTP health-check path is
`/health` not `/global/health`, its SSE endpoint is different, its session
model is fundamentally different from opencode's `GET /session` → `POST
/session` split (see B.1 §§7.1, class: `CreateSession`, "[uncertain —
whether PI has a 'session' concept...]"). If the `HTTPHarness` base has
already been extracted and opencode has been migrated to embed it, adding
Claude Code in a way that *doesn't* fit the base forces a messy choice:
(a) bend Claude Code into the base's shape even where the base is wrong,
or (b) rip out the base and flatten again — now touching opencode *and*
Claude Code. A premature base doubles the cost of the inevitable
wrong-abstraction removal pass.

The same risk is higher for stdio harnesses: PI is planned but not
implemented. Codex's protocol shape is unconfirmed. [uncertain — Codex
may use a gRPC or HTTP-based protocol rather than JSON Lines over stdio;
if so, a `StdioHarness` base would immediately need a `GRPCHarness`
sibling and the taxonomy would already be wrong]. Building a `StdioHarness`
base before even one stdio adapter exists is maximally speculative.

### 3.3 Shared bases introduce structural coupling between unrelated adapters

Embedding a shared base struct couples the two (or more) adapters that
embed it: changes to the base require auditing all embedders. With a flat
registry, the opencode adapter is entirely self-contained; adding a second
harness does not require touching opencode at all. With a shared base,
every base API change is a multi-file change that touches every embedder.
Given that the two HTTP harnesses (opencode and Claude Code) are separate
vendor integrations with independent release cycles, keeping them
structurally independent reduces merge-time friction and allows each to
evolve without gate-keeping from the other.

This is not a fatal objection — standard Go embedding does not force
coupling beyond the embedded struct's API — but it is a non-zero
maintenance cost that must be weighed against the sharing benefit.

## 4. Inflection-point analysis

The question of when grouping becomes net-positive resolves into: at what
number of harnesses per shape does the shared-machinery benefit outweigh
(a) the YAGNI cost of premature extraction, (b) the wrong-abstraction
removal risk, and (c) the cross-adapter coupling cost?

**For HTTP-shaped harnesses:**

- At **1 harness** (today): no sharing benefit. The cost is all overhead.
  Net: negative. Do not group.
- At **2 harnesses** (opencode + Claude Code): health-check loop, SSE
  subscription wiring, and HTTP client lifecycle become shared. Estimated
  saving: ~150-200 LoC of near-verbatim duplication. The wrong-abstraction
  risk is manageable: opencode is already a working reference
  implementation, so Claude Code's implementation will reveal the actual
  API surface to abstract. Grouping *at the point of landing the second
  harness* — not before — avoids the speculative risk.
  Net: **becomes positive at harness 2, with grouping deferred until
  harness 2 exists**.
- At **3+ harnesses**: strong net-positive. The shared base is battle-tested
  across two concrete implementations before a third is added.

**For stdio-shaped harnesses:**

- At **0 implementations** (today): maximally speculative. Net: negative.
- At **1 implementation** (PI): the base can be extracted from the one
  existing adapter. But there is still no second adapter to validate the
  API. The wrong-abstraction risk remains high because the base is
  generalised from a single point. Net: marginally positive at best.
  The better trigger is landing the second stdio harness (Codex or
  equivalent).
- At **2 implementations** (PI + Codex): pipe lifecycle, JSONL
  framing, process supervision become concretely shared. Estimated
  saving: ~100-150 LoC. Net: **becomes positive at harness 2**. Same
  deferral principle: group at the point the second stdio harness lands,
  not before.

**Summary inflection point:** grouping becomes net-positive at 2 harnesses
per shape, and the grouping work should be done *when* the second harness
of that shape is being integrated — not speculatively ahead of it.

## 5. Recommendation

**Defer. Do not group now.**

The recommendation is: **defer grouping until the second harness of each
shape is being integrated, and re-evaluate at that point with both
concrete implementations in front of you.**

Rationale:

1. Only one HTTP harness exists (opencode). Zero stdio harnesses exist.
   The inflection point for net-positive grouping is two harnesses per
   shape. We are at one and zero respectively.
2. Claude Code's HTTP API has not been confirmed to share the
   health-check, SSE, or session-creation shape with opencode. [uncertain —
   Claude Code auth and session model are not known at the time of this
   writing; #691 §7 flags Claude Code auth as an open question]. Extracting
   a shared base before examining Claude Code's actual API would be
   speculation-driven design.
3. PI and Codex are planned but unimplemented. A `StdioHarness` base
   extracted before any stdio adapter exists has no empirical basis.
4. B.2's flat registry with `TransportShape` tags already provides the
   shape-gated branching that callers need. A shared base adds capability
   beyond what callers require today.

The question is **not** left open — the answer is "defer", not "maybe". If
the criteria in §7 are met, grouping should proceed. If they are not met,
the flat registry is the correct final shape.

## 6. If grouping is recommended: concrete Go signatures

This section is included for completeness (per the AC), though the
recommendation is deferral. If the deferral criteria in §7 are met and
both harnesses per shape exist, the following sketch represents a
reasonable starting point.

### 6.1 `HTTPHarnessBase` — shared base for HTTP+SSE harnesses

```go
// Package harness, new file harness/http_base.go (proposed — not implemented).

// HTTPHarnessBase is an embeddable struct that provides shared machinery for
// HTTP-port-shaped harness adapters. It encapsulates the HTTP client, the
// health-check polling loop, and the SSE subscription wiring.
//
// Usage: embed this struct in a concrete adapter and call its methods from
// the adapter's HealthCheck and Subscribe implementations.
//
// Adapters must supply the health URL and SSE URL at construction time.
// These are the only harness-specific bits of HTTP-shape machinery.
type HTTPHarnessBase struct {
    // baseURL is the harness's HTTP base (e.g. "http://127.0.0.1:4096").
    // The adapter constructs full endpoint URLs from this.
    baseURL    string
    httpClient *http.Client
}

// NewHTTPHarnessBase constructs the base with the given URL and HTTP client.
// Pass nil for httpClient to use a 20s-timeout default.
func NewHTTPHarnessBase(baseURL string, httpClient *http.Client) HTTPHarnessBase

// BaseURL returns the harness HTTP base URL.
func (b *HTTPHarnessBase) BaseURL() string

// DefaultHTTPClient returns the HTTP client for this harness.
func (b *HTTPHarnessBase) DefaultHTTPClient() *http.Client

// PollUntilHealthy polls healthURL with healthCheckInterval backoff until the
// endpoint returns 200 or ctx is cancelled. The healthURL is supplied by the
// concrete adapter (not the base) because the path is harness-specific.
func (b *HTTPHarnessBase) PollUntilHealthy(ctx context.Context, healthURL string) error

// SubscribeSSE wires an sse.Client to the given eventURL and returns a channel
// of HarnessEvent. It is the concrete implementation of harness.Harness.Subscribe
// for HTTP-shaped harnesses. The adapter calls this from its Subscribe method
// with the harness-specific SSE endpoint path.
func (b *HTTPHarnessBase) SubscribeSSE(ctx context.Context, eventURL string) (<-chan HarnessEvent, error)
```

### 6.2 `StdioHarnessBase` — shared base for JSONL-over-stdio harnesses

```go
// Package harness, new file harness/stdio_base.go (proposed — not implemented).

// StdioHarnessBase is an embeddable struct that provides shared machinery for
// stdio-pipe-shaped harness adapters. It encapsulates child-process lifecycle,
// JSONL framing on stdout, and fire-and-forget writes to stdin.
//
// Usage: embed this struct in a concrete adapter and call its methods from
// the adapter's Subscribe and DeliverPrompt implementations.
type StdioHarnessBase struct {
    cmd    *exec.Cmd
    stdin  io.WriteCloser
    stdout io.ReadCloser
    mu     sync.Mutex
}

// StartProcess starts the harness child process with the supplied *exec.Cmd.
// It wires stdin and stdout pipes and is expected to be called before
// Subscribe or DeliverPrompt.
func (b *StdioHarnessBase) StartProcess(cmd *exec.Cmd) error

// ReadJSONLines reads newline-delimited JSON frames from the child's stdout
// and sends them as HarnessEvent values on the returned channel. The channel
// is closed on EOF or context cancellation. Partial reads and framing errors
// are absorbed; malformed lines produce a zero-value HarnessEvent with
// Data set to the raw bytes for the adapter's ExtractEventType to reject.
func (b *StdioHarnessBase) ReadJSONLines(ctx context.Context) (<-chan HarnessEvent, error)

// WriteJSONLine marshals v to JSON and writes it followed by a newline to the
// child's stdin. Returns an error if the pipe is closed. This is the
// implementation of harness.Harness.DeliverPrompt for stdio-shaped harnesses
// (modulo per-harness JSON envelope wrapping).
func (b *StdioHarnessBase) WriteJSONLine(ctx context.Context, v any) error

// Wait waits for the child process to exit and returns its exit error.
// Should be called from the adapter's cleanup path.
func (b *StdioHarnessBase) Wait() error
```

## 7. Criteria for revisiting

Since the recommendation is deferral, the following criteria define when
the deferral should end and grouping should be re-evaluated:

1. **Second HTTP harness nears integration.** When Claude Code (or another
   HTTP+SSE harness) is being integrated and its adapter is being written,
   that is the moment to extract `HTTPHarnessBase`. Specifically: if the
   Claude Code adapter's `HealthCheck` implementation shares more than 50
   LoC verbatim with opencode's `adapter.go:121-167`, extract the base.
2. **Second stdio harness nears integration.** Same trigger for
   `StdioHarnessBase`: when PI + Codex (or PI + any other stdio harness)
   are both being integrated and their adapters share more than 50 LoC of
   pipe/JSONL code, extract the base.
3. **Single-harness shared base if the adapter exceeds ~300 LoC of
   transport boilerplate.** If a single harness adapter grows so large
   that its transport boilerplate alone is visually dominating the
   harness-specific logic, extracting an internal base within the adapter
   package (not exported, not shared with other adapters) is appropriate
   independently of the multi-harness threshold.
4. **B.2 shape-gated callers accumulating per-shape logic.** If
   downstream code (container manager, sidecar Run loop) accretes
   more than two shape-gated `if shape == TransportHTTPPort { ... }`
   blocks that each call out to per-harness shared methods, a shared
   base interface is the cleaner expression of the contract than
   a growing set of shape-tag conditionals. At that point, even with
   only one harness per shape, the base earns its existence by giving
   the shape-gated callers a typed abstraction to call rather than an
   enum-switch to maintain.

## 8. Open questions and `[uncertain]` flags

- **[uncertain] Claude Code HTTP API compatibility.** Claude Code's health
  endpoint, SSE stream format, and session-creation model are unknown at
  the time of this writing. If Claude Code uses a fundamentally different
  HTTP shape from opencode (e.g. WebSocket instead of SSE, or a
  single-endpoint prompt-reply model instead of a persistent session),
  the `HTTPHarnessBase` sketch in §6.1 would need substantial revision
  before it fits both. Cannot confirm without examining Claude Code's
  actual API surface. (RFC #691 §7 notes this as an open question.)
- **[uncertain] Codex protocol shape.** Codex's wire protocol has not
  been confirmed to be JSON Lines over stdio. It may use HTTP, gRPC, or
  some other framing. If Codex is HTTP-shaped, the `StdioHarness`
  taxonomy needs a third sibling rather than a second stdio harness
  — the "two stdio harnesses" trigger in §7 would never fire.
  Cannot confirm without Codex research.
- **[uncertain] `ContainerFactory` split in B.2 and group membership.**
  B.2's `ContainerFactory` opt-in (per `Registration`) exists because
  opencode distinguishes `New` vs `NewContainerMode`. If `HTTPHarnessBase`
  is extracted, does container-mode become a base-level flag (i.e.
  `HTTPHarnessBase.containerMode bool`) or a per-adapter constructor
  choice? This is B.4's downstream design question if grouping is
  ultimately adopted; it does not affect the deferral recommendation but
  should be resolved before implementation begins.
- **[uncertain] Whether `StdioHarness` process supervision is
  shape-level or sidecar-level.** B.3 (#1081) examines the sidecar
  lifecycle inversion required for stdio harnesses. If B.3 concludes
  that the sidecar itself owns child-process supervision (option a:
  "sidecar becomes parent of agent-run"), the `StdioHarnessBase.StartProcess`
  / `Wait` API in §6.2 may be superseded by a sidecar-level launcher
  abstraction. The `StdioHarness` base would then cover only
  JSONL framing, not process lifecycle. The sketch in §6.2 is
  indicative; the actual split depends on B.3's outcome.
- **[uncertain] `CreateSession` and stdio.** B.1 §7.1 flags
  `CreateSession(ctx) (string, error)` as `[http-only]` in opencode's
  implementation — it is a `GET /session` HTTP round-trip. For stdio
  harnesses there may be no session-creation step at all (the session is
  the process; one process == one session). If so, `CreateSession` returns
  a synthetic process-PID-based ID for stdio harnesses, or the interface
  gains a default "no-op, return a uuid" implementation. Either way this
  is not `StdioHarnessBase` territory — it is interface evolution work
  that is out of scope for this review.

## Related

- Inventory: [`../architecture-inventory.md`](../architecture-inventory.md) §7.
- B.1 (#1074, landed #1096):
  [`B1-harness-transport-and-lifecycle-assumptions.md`](B1-harness-transport-and-lifecycle-assumptions.md)
  — transport-shape classification this proposal consumes.
- B.2 (#1080, landed #1102):
  [`B2-harness-registry-and-transport-shape.md`](B2-harness-registry-and-transport-shape.md)
  — flat registry proposal; B.4 evaluates whether to add a layer above it.
- B.3 (#1081):
  [`B3-sidecar-lifecycle-for-stdio-harnesses.md`](B3-sidecar-lifecycle-for-stdio-harnesses.md)
  — sidecar lifecycle inversion; affects the process-supervision scope of
  any future `StdioHarnessBase`.
- Design doc:
  [`000-design-narrow-review-series.md`](000-design-narrow-review-series.md).
- Issue: #1082. Parent design: #1072.
- RFCs: #691 (multi-harness support), #606 (PI coding agent support).
- Sibling Track B issues: #1074 (B.1), #1080 (B.2), #1081 (B.3), #1083
  (B.5), #1084 (B.6).
