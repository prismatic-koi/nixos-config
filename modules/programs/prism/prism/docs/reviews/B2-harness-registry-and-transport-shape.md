# B.2 — Harness construction site refactor: registry plus transport-shape declaration

Status: proposal (no code changes).
Issue: #1080.
Track: B (harness), Wave 2.
Source corpus:
[`../architecture-inventory.md`](../architecture-inventory.md) §7,
[`B1-harness-transport-and-lifecycle-assumptions.md`](B1-harness-transport-and-lifecycle-assumptions.md)
(landed in #1096), and the four primary call sites cited in #1080
(`cmd/sidecar.go:288-301`, `cmd/spawn.go:148`,
`internal/sidecar/sidecar.go:3101-3219`,
`internal/session/sidecar.go:215-340`).

This is a **proposal document**. It contains zero implementation work —
no Go code is written, modified, or moved by this review. The Go signatures
below are *proposed shapes* the future implementation work would consume.

## 1. Context

Today, `prism` has exactly one harness implementation (opencode). The
`harness.Harness` interface (`internal/harness/harness.go:23-106`) was
deliberately sized for one tenant, and the construction sites that turn an
identifier into an adapter are correspondingly small:

- **Adapter construction** is hard-coded at `cmd/sidecar.go:282-297`: the
  sidecar branches on `useContainerHarness` to call either
  `opencode.NewContainerMode(...)` or `opencode.New(...)`. The package name
  `opencode` is named directly. There is no indirection.
- **Spawn allow-list** is hard-coded at `cmd/spawn.go:148, :247-253`:
  `--harness` defaults to `"opencode"` and any other value is rejected.
  The same allow-list is duplicated in the host-API `/spawn` proxy at
  `internal/sidecar/sidecar.go:3137-3143` and in the test plumbing at
  `cmd/spawn_harness_test.go`.
- **Construction-only-for-side-effects sites** call `opencode.New("", nil, "", "")`
  purely to read `RuntimeEnv()` / `ConfigEnvVar()` / `EffectiveModel()`
  off the adapter. There are seven of these in `cmd/`:
  `cmd/sidecar.go:180`, `cmd/spawn.go:425`, `cmd/agent_run.go:190`,
  `cmd/agent_run.go:555`, `cmd/restore.go:297`, `cmd/switch.go:1090`,
  `cmd/pr.go:164`, `cmd/review.go:207`. Each one is morally a registry
  lookup keyed on the session's `harness` column.

B.1 (#1096) added a second classification axis on top of the inventory's
`opencode-only / multi-harness via interface / harness-agnostic` lens:
**transport shape** (`http-only`, `transport-agnostic`, `opencode-only`,
`uncertain`). B.1's central observation is that the existing `Harness`
interface bakes HTTP-server-shape into two of its method signatures
(`HealthCheck(ctx, port int)` and the `port` plumbing around it) and into
the lifecycle of three more (`WaitHealthy` → `OnReady` → `--port`/`--publish`).
B.2 consumes that classification: the registry's `TransportShape` enum is
the place where "this harness needs port plumbing" vs "this harness needs
pipe plumbing" gets declared, so downstream code stops branching on
`harness == "opencode"` and starts branching on shape.

This proposal deliberately does **not**:

- Propose a sidecar-as-launcher refactor (B.3 — #1081).
- Propose a harness-group abstraction (HTTPHarness / StdioHarness shared
  base) (B.4 — #1082).
- Propose a payload schema (B.5 — #1083) or archive layout (B.6 — #1084).
- Modify the `Harness` interface itself. Re-shaping `HealthCheck` to remove
  the `port` parameter is B.1-derived work that the registry consumers will
  motivate but does not own.

The registry is a **flat lookup table with a transport-shape tag**. Whether
the http-shaped registrations should later share an embedded base struct is
B.4's call.

## 2. Method

The proposal is structured as:

- §3 — proposed `harness.TransportShape` enum and per-value semantics.
- §4 — proposed `harness.Registry` Go signatures.
- §5 — per-call-site migration table for every §7 site that currently
  hard-codes opencode by name.
- §6 — `harness string` → `harness.TransportShape` migrations: sites that
  currently take a name string but should take a transport-shape value.
- §7 — open questions and `[uncertain]` flags.

Each migration row carries a file:line citation. The file:line numbers are
taken from the inventory and re-verified against the working tree at the
B.1 landing commit (`946dda9a`). The proposal does not invent new
abstractions where existing ones suffice — for instance, the registry uses
the existing `harness.Harness` interface as its return type rather than
introducing a separate `RegisteredHarness` wrapper.

## 3. Proposed `TransportShape` enum

```go
// Package harness, additions to harness.go.

// TransportShape declares the wire-level shape a harness uses to talk to
// its agent runtime. It is a registration-time property of each harness
// adapter: code that does not need to know the harness name (container
// manager, sidecar lifecycle, agent-pane command builder) consults the
// shape instead.
//
// The enum is closed: registration with an unknown TransportShape value
// fails at init time. Adding a new value is a deliberate change that
// requires updating every consumer that switches on the value.
type TransportShape string

const (
    // TransportHTTPPort declares that the harness runs a long-lived
    // HTTP server inside its container (or process) and the sidecar
    // dials it on a TCP port. Health checks are HTTP probes against
    // a known endpoint. Event delivery is server-sent events (SSE) or
    // long-polling. Prompts are POSTed; the response status code is the
    // delivery acknowledgement. Examples: opencode (today),
    // Claude Code (planned).
    TransportHTTPPort TransportShape = "http-port"

    // TransportStdioPipe declares that the harness runs as a child
    // process whose stdin/stdout the sidecar (or sidecar-equivalent)
    // owns. Wire format is JSON Lines (or another framed stream).
    // Health is the process being alive and the pipe being open.
    // Prompts are written to stdin fire-and-forget — the OS write
    // succeeds or fails, but there is no transport-level
    // acknowledgement that the harness has processed the prompt.
    // Examples: PI (planned, RFC #606), Codex (likely future).
    TransportStdioPipe TransportShape = "stdio-pipe"

    // TransportFallbackScreenScrape declares that the harness has no
    // structured wire protocol and the sidecar must observe the
    // agent's behaviour by reading its TTY output (typically via a
    // capture pipe attached to the container's PTY). Prompt delivery
    // is by writing to the harness's controlling TTY rather than to
    // any structured channel. Health is "the process is alive".
    // This is the safety-net shape for harnesses that ship before
    // their structured protocol is stable, and for any future harness
    // whose vendor declines to expose a structured API.
    TransportFallbackScreenScrape TransportShape = "fallback-screen-scrape"
)
```

Why three values, and why not more? The B.1 issue specification
(#1080's "Background" section) names `{http-port, stdio-pipe,
fallback-screen-scrape}` explicitly as the minimum set. The first two
correspond to the two known harness models (opencode HTTP, PI JSONL).
The third exists because the design doc (#1072) and RFC #691 leave the
door open for additional harnesses (Claude Code, Codex) whose protocol
shape may not be fully known at integration time, and a screen-scrape
fallback is the only universally-applicable shape. Adding more values
later (e.g. `grpc-stream`, `unix-socket-rpc`) is a deliberate enum
extension; the proposal does not preempt that.

`TransportShape` is a `string`-based enum (not an `int` or a
generated-constant block) for two reasons that match the existing
`config.IsolationMode` precedent (`internal/config/config.go:18-43`):
the value is persistable in the DB without a translation table, and
it is human-readable in logs and JSON requests. The cost is that the
zero value (`""`) is not a meaningful shape; the registry's
`Register` call rejects it.

## 4. Proposed `Registry` Go signatures

The registry lives in the existing `internal/harness` package. It is a
package-level singleton initialised at process start by adapter packages'
`init()` functions, mirroring the standard-library `database/sql.Register`
pattern.

```go
// Package harness, additions to harness.go (or a new harness/registry.go).

// Factory constructs a harness.Harness adapter for a single sidecar
// session. The arguments are the cross-harness inputs the registry
// promises to provide to every adapter:
//
//   - endpoint: the transport-specific endpoint hint. For
//     TransportHTTPPort this is the URL the sidecar dials (e.g.
//     "http://localhost:4096"). For TransportStdioPipe this is the
//     path to the harness binary (or a launch spec — TBD by B.3).
//     For TransportFallbackScreenScrape this is the path to the TTY
//     capture pipe. The factory treats it as opaque per its shape.
//   - httpClient: optional. Non-nil when the caller wants to inject
//     a test client; nil means the adapter uses its default. Ignored
//     by stdio and screen-scrape adapters.
//   - agentRole: "worker" | "coordinator" | "" (review subagents
//     pass empty here; their role is set later via env vars).
//   - agentModel: model identifier (e.g. "anthropic/claude-sonnet-4-6").
//     Empty means "let the adapter resolve from its config".
//
// Factory is intentionally narrow: it returns a Harness, not a
// concrete adapter type. Sites that today reach for opencode-specific
// methods (none exist outside the opencode package itself) cannot
// continue to do so through the registry.
type Factory func(endpoint string, httpClient *http.Client, agentRole, agentModel string) Harness

// Registration is the data an adapter package supplies at registration
// time. The Name is the user-visible identifier (matches the --harness
// flag value and the agent_status.harness column). Shape is the
// declared transport shape; consumers may switch on it without
// knowing the name.
type Registration struct {
    Name    string
    Shape   TransportShape
    Factory Factory

    // ContainerFactory, when non-nil, is the factory used in container
    // mode. It exists because the opencode adapter today exposes
    // opencode.New / opencode.NewContainerMode as two separate
    // constructors that differ in CreateSession and DeliverInitialPrompt
    // semantics. Other harnesses (stdio especially) probably need a
    // single Factory; ContainerFactory is opt-in.
    //
    // When nil, Lookup-style consumers in container mode fall back to
    // Factory.
    ContainerFactory Factory
}

// Register adds a Registration to the global registry. It is intended
// to be called from an adapter package's init() function:
//
//     // in internal/harness/opencode/register.go
//     func init() {
//         harness.MustRegister(harness.Registration{
//             Name:             "opencode",
//             Shape:            harness.TransportHTTPPort,
//             Factory:          opencode.New,
//             ContainerFactory: opencode.NewContainerMode,
//         })
//     }
//
// Register returns an error for: empty Name, empty/unknown Shape,
// nil Factory, or duplicate Name. MustRegister panics on the same
// conditions and is the form intended for init().
func Register(reg Registration) error
func MustRegister(reg Registration)

// Lookup returns the Registration for a given harness name, or
// (Registration{}, false) if no harness with that name is registered.
// Callers that want a usable harness use New / NewContainer instead;
// Lookup is for the spawn allow-list and for tests.
func Lookup(name string) (Registration, bool)

// Names returns the registered harness names in deterministic order
// (sorted ascending). Used by:
//   - the --harness flag's validation error message ("valid: opencode, pi"),
//   - the host-API /spawn handler's allow-list,
//   - dashboard / stats display.
func Names() []string

// New constructs a host-mode harness adapter for the named harness.
// Returns an error if the name is not registered. The endpoint /
// httpClient / agentRole / agentModel arguments are forwarded to the
// registered Factory verbatim.
//
// Replaces the hard-coded `opencode.New(opencodeURL, nil, agentRole, agentModel)`
// call at cmd/sidecar.go:296 and the construction-only-for-side-effects
// `opencode.New("", nil, "", "")` calls in cmd/spawn.go, cmd/agent_run.go,
// cmd/restore.go, cmd/switch.go, cmd/pr.go, cmd/review.go.
func New(name, endpoint string, httpClient *http.Client, agentRole, agentModel string) (Harness, error)

// NewContainer constructs a container-mode harness adapter for the
// named harness. If the registration has a ContainerFactory it is used;
// otherwise Factory is used. Returns an error if the name is not
// registered.
//
// Replaces the hard-coded `opencode.NewContainerMode(...)` call at
// cmd/sidecar.go:294.
func NewContainer(name, endpoint string, httpClient *http.Client, agentRole, agentModel string) (Harness, error)

// ShapeOf returns the declared TransportShape for the named harness,
// or ("", false) if not registered. Convenience over Lookup for sites
// that only need the shape (e.g. session.StartSidecarWithOpts deciding
// whether to allocate a port — see §6 below).
func ShapeOf(name string) (TransportShape, bool)
```

Notes on the shape:

- **Singleton, not injected.** Mirrors `database/sql.Register`. The
  alternative — passing a `*Registry` value through every call site —
  was considered and rejected: it would touch every `SpawnOpts` /
  `Config` / `StartSidecarOpts` struct in the corpus and add no test
  power that a per-test `harness.ResetForTest()` (analogous to existing
  test helpers in `internal/db`) does not already give.
- **`Factory` is a function type, not an `interface{ Build(...) Harness }`.**
  The factory's contract is a single call; no need for an extra
  named-method indirection.
- **Allow-list lookup is `Lookup`, not a separate `IsRegistered`.**
  Callers that want a boolean check the second return value.
- **No `Unregister`.** Process-lifetime registration only. Tests that
  need to swap registrations use a `ResetForTest(t *testing.T)` helper
  (proposed; not built here) which records the current set in `t.Cleanup`.
- **No transport-shape registry beyond the per-harness tag.** Container
  manager and agent-pane command builder consult `ShapeOf(name)` for the
  one or two shape-dependent decisions they make (port allocation,
  publish flag, command terminator); they do not consult a separate
  "shape adapter". B.4 may revisit this if shape-shared machinery grows
  large enough to warrant a `TransportShapeAdapter` interface; today it
  does not.

### 4.1 Validation behaviour for the spawn allow-list

The hard-coded allow-list `{"opencode"}` at three sites
(`cmd/spawn.go:251-253`, `internal/sidecar/sidecar.go:3140-3143`,
`cmd/spawn_harness_test.go`) becomes a single `harness.Lookup(name)` /
`harness.Names()` pair. Validation messages constructed off `Names()`
stay sorted (so "valid harnesses: opencode, pi" reads consistently
regardless of init order).

The host-API `/spawn` handler's allow-list at
`internal/sidecar/sidecar.go:3140-3143` becomes:

```go
// Replaces the hard-coded `if req.Harness != "opencode" { ... }` block.
if _, ok := harness.Lookup(req.Harness); !ok {
    writeError(w, http.StatusBadRequest, fmt.Sprintf(
        "unknown harness %q: valid harnesses: %s",
        req.Harness, strings.Join(harness.Names(), ", ")))
    return
}
```

The CLI `--harness` flag declaration stays a free-form string at the
cobra level (`cmd/spawn.go:148`); validation is performed in `runSpawn`
via `harness.Lookup` after `cmd.Flags().GetString("harness")`. The
default value (today `"opencode"`) stays "opencode" until it is changed
by a deliberate config decision; the registry does not pick a default.

## 5. Per-call-site migration table

This section enumerates every §7 site that today hard-codes opencode by
name (string literal, package-import-with-direct-construction, or both)
and proposes the registry-consuming replacement. Sites are grouped by
inventory subsection. The "current shape" column quotes or paraphrases
the existing code; the "proposed shape" column names the registry call
that replaces it.

### 5.1 §7.1 `internal/harness/harness.go` (interface)

The interface itself is opencode-agnostic; no migration needed in this
file. The new `TransportShape` enum and the `Registry` API are
*additions* to the package.

### 5.2 §7.2 `internal/sidecar/sidecar.go`

| Site | Current shape | Proposed shape |
|---|---|---|
| `internal/sidecar/sidecar.go:123` | `OpencodeURL string` field on `Config` (the field name names opencode) | Rename to `HarnessEndpoint string`. The value is still the HTTP URL today; for stdio harnesses it would carry the harness binary path or a launch spec (B.3 owns the launch-spec format). The rename is mechanical; the field is still set by `cmd/sidecar.go`. |
| `internal/sidecar/sidecar.go:3124` | `Harness string \`json:"harness"\`` on the `/spawn` request struct | Stays. The wire field is the user-visible name. Validation switches to `harness.Lookup`. |
| `internal/sidecar/sidecar.go:3137-3143` | Hard-coded `if req.Harness != "opencode"` check | `if _, ok := harness.Lookup(req.Harness); !ok { ... }` (see §4.1 above). The default-to-opencode behaviour at `:3137-3139` stays as a backwards-compat shim until B.6 / E.1 decides it should warn-and-pass-through. |

### 5.3 §7.3 `cmd/sidecar.go`

This is the **canonical adapter-construction site** the issue spec
calls out. Migration is the largest single change in this proposal.

| Site | Current shape | Proposed shape |
|---|---|---|
| `cmd/sidecar.go:180` | `agentModel := opencode.New("", nil, agentRole, "").EffectiveModel(agentRole)` — calls opencode.New just to read `EffectiveModel` | `agentModel, err := harness.New(harnessName, "", nil, agentRole, "").EffectiveModel(agentRole)` where `harnessName` comes from a new `--harness` flag on `sidecar` (today the sidecar has no such flag — see §6 below). Until the flag exists, the call site uses `harnessName := "opencode"` as a temporary literal. |
| `cmd/sidecar.go:282-297` | `var h *opencode.Adapter; if useContainerHarness { h = opencode.NewContainerMode(opencodeURL, nil, agentRole, agentModel) } else { h = opencode.New(opencodeURL, nil, agentRole, agentModel) }` | `var h harness.Harness; if useContainerHarness { h, err = harness.NewContainer(harnessName, opencodeURL, nil, agentRole, agentModel) } else { h, err = harness.New(harnessName, opencodeURL, nil, agentRole, agentModel) }` — same `harnessName` source as the row above. The `*opencode.Adapter` typed variable becomes the `harness.Harness` interface; the only call against `h` outside the interface is `h.RuntimeEnv()` at `:303`, which is interface-satisfying already. |
| `cmd/sidecar.go:288-291` | Comment: "Build the harness adapter — only opencode supports container mode today" | Update comment to match the registry call. The "only opencode" note becomes ground truth maintained by registration: `harness.NewContainer(name, ...)` returns a non-nil error if the named harness has no `ContainerFactory`. |
| `cmd/sidecar.go:294, :296` | Direct import of `opencode` package | Delete the import; the registry's `New` / `NewContainer` return `harness.Harness` so the file no longer needs the opencode package directly. The opencode package is still imported transitively via its `init()` registration — see §5.10. |

### 5.4 §7.4 `cmd/spawn.go`

| Site | Current shape | Proposed shape |
|---|---|---|
| `cmd/spawn.go:148` | `spawnCmd.Flags().String("harness", "opencode", "Agent harness to use (currently only 'opencode' is supported)")` | Stays as a flag declaration. The doc string is reworded to reference `harness.Names()` dynamically, e.g. "Agent harness to use; one of: " + strings.Join(harness.Names(), ", "). At cobra-init time `harness.Names()` may not yet have run all `init()` functions, so the doc is finalised lazily — a `cobra.OnInitialize` callback that re-sets the flag's `Usage` field is the cheapest fix; alternatively, the doc string stays generic and validation prints the names. |
| `cmd/spawn.go:251-253` | `if harnessFlag != "opencode" { return fmt.Errorf("unknown harness %q: only 'opencode' is supported in this version of prism", harnessFlag) }` | `if _, ok := harness.Lookup(harnessFlag); !ok { return fmt.Errorf("unknown harness %q: valid harnesses: %s", harnessFlag, strings.Join(harness.Names(), ", ")) }` |
| `cmd/spawn.go:425` | `h := opencode.New("", nil, "", "")` constructed only to read `ConfigEnvVar()` and `RuntimeEnv()` | `h, err := harness.New(harnessFlag, "", nil, "", "")` — the harness name now comes from `harnessFlag` (already in scope). The error path returns the same "unknown harness" error already produced by the validation block at `:251-253`, so in practice err is unreachable; the proposal still threads it for safety. |

### 5.5 §7.6 `cmd/review.go`

| Site | Current shape | Proposed shape |
|---|---|---|
| `cmd/review.go:53` | `reviewCmd.Flags().String("harness", "opencode", "Runtime harness to use for review agents")` | Same as `cmd/spawn.go:148`: stays as a flag, doc updated. |
| `cmd/review.go:88` | `allAgents := agentsForHarness(harnessFlag)` — currently dispatches a hard-coded list keyed on the literal string "opencode" | `agentsForHarness` stays, but its body switches on `harness.ShapeOf(name)` (or a new per-harness review-agent registration if the agent list legitimately differs across harnesses). The function rename is out of scope for B.2; the proposal flags only that the lookup needs to consult the registry. |
| `cmd/review.go:207` | `h := opencode.New("", nil, "", "")` for `RuntimeEnv` / `ConfigEnvVar` | `h, err := harness.New(harnessFlag, "", nil, "", "")`. |

### 5.6 §7.7 `cmd/agent_run.go`

| Site | Current shape | Proposed shape |
|---|---|---|
| `cmd/agent_run.go:190` | `agentRunHarness := opencode.New("", nil, "", "")` to populate `RuntimeEnv` for the bwrap container config | `agentRunHarness, err := harness.New(harnessName, "", nil, "", "")` where `harnessName` comes from `status.Harness` (the persisted `agent_status.harness` column, already in scope as the `status` variable). |
| `cmd/agent_run.go:555` | Second `opencode.New("", nil, "", "")` site (sandbox-exec path) | Same migration as `:190`. |
| `cmd/agent_run.go:159-164` | Port resolution from `status.HarnessPort` to feed into `ctrCfg.AllocatedPort` | `[transport-shape gated]`. See §6. The port resolution should only happen when `harness.ShapeOf(status.Harness) == TransportHTTPPort`; for stdio harnesses there is no port to resolve and `AllocatedPort` stays zero. |
| `cmd/agent_run.go:189-205` (comment block: "Populate harness-specific runtime env vars for the bwrap sandbox … bwrap.go's BuildArgs appends --prompt to the opencode invocation") | The comment names opencode | Update to name "the harness" generically; the `--prompt` append is a `bwrap.go` concern (see §5.10). |

### 5.7 §7.8 `cmd/restore.go`

| Site | Current shape | Proposed shape |
|---|---|---|
| `cmd/restore.go:297` | `restoreHarness := opencode.New("", nil, "", "")` for env-var hydration | `restoreHarness, err := harness.New(status.Harness, "", nil, "", "")` — the harness name comes from the persisted DB row that restore is reconstructing. |

### 5.8 §7.10 sibling commands (`cmd/switch.go`, `cmd/pr.go`)

| Site | Current shape | Proposed shape |
|---|---|---|
| `cmd/switch.go:1090` | `switchHarness := opencode.New("", nil, "", "")` | `switchHarness, err := harness.New(harnessName, "", nil, "", "")` — `harnessName` from `status.Harness` (the session being switched into). |
| `cmd/pr.go:164` | `prHarness := opencode.New("", nil, "", "")` | `prHarness, err := harness.New(harnessName, "", nil, "", "")` — `harnessName` from `status.Harness` (the worker session whose PR is being inspected). |

(`cmd/checkin.go`, `cmd/stats.go`, `cmd/prompt.go`, `cmd/list_sessions.go`,
`cmd/sessions.go` from inventory §7.10 do not construct a harness adapter
directly; they only display the persisted `harness` column. No migration
needed beyond display-string formatting, which the registry does not
own.)

### 5.9 §7.11 `internal/db/db.go`

| Site | Current shape | Proposed shape |
|---|---|---|
| `internal/db/db.go:169` | `harness TEXT NOT NULL DEFAULT 'opencode'` on `agent_status` | The default literal stays. The DB layer does not consult the registry — keeping `'opencode'` as the schema default avoids cross-package init coupling. A separate migration that drops the default once a non-opencode harness ships is a B.4 / E.1 decision. |
| `internal/db/db.go:218-225` | Migration v7→v8 added `harness_port` | `[uncertain]` — see §6 and §7. The column is HTTP-shape-only; whether it stays NULLable or moves to a per-shape side table is a schema-evolution question owned by B.5 (#1083). B.2 does not migrate the column. |

### 5.10 §7.14 `internal/container/container.go`, `internal/container/bwrap.go`, `internal/container/sandbox_exec.go`

These files contain the largest concentration of opencode-by-name
references in the corpus. Most are *transport-shape* coupling, not
construction coupling, so they migrate via §6 below rather than via
the registry directly. The construction-time ones:

| Site | Current shape | Proposed shape |
|---|---|---|
| `internal/container/sandbox_exec.go:147-172` | `args := []string{"sandbox-exec", "-f", profilePath, "opencode"}` — opencode binary name hard-coded as the sandbox-exec entry | The binary name belongs to the harness adapter via `ContainerCommand()` (which today returns `"opencode --port 4096 --hostname 0.0.0.0"`). The sandbox-exec wrapper should take the binary from the harness's `ContainerCommand()` rather than hard-coding `"opencode"`. The migration here is "stop hard-coding the literal" — the registry plays the indirect role of supplying a `Harness` whose `ContainerCommand()` is the source of truth. |
| `internal/container/bwrap.go:608-635` | bwrap argv terminator: `"opencode", "--port", fmt.Sprintf("%d", opencodePort), "--hostname", "127.0.0.1"`, plus conditional `--prompt` append | `[transport-shape gated]`. See §6 row "bwrap argv terminator". The `--port`/`--hostname` flags are TransportHTTPPort-shape-only; for TransportStdioPipe the bwrap argv ends differently (no port, no hostname, possibly no `--prompt`). The literal `"opencode"` becomes the harness's binary identifier, sourced from `ContainerCommand()`. |
| `internal/container/container.go:41-45` | `ContainerPort` constant doc: "port opencode serve listens on inside the container" | Comment update only. The constant value stays (it is the inside-container port for all TransportHTTPPort harnesses), but the doc reads "port the HTTP-port-shaped harness listens on inside the container". For stdio harnesses the constant is unused. |
| `internal/container/container.go:1115-1116` (`portBinding` and `--publish`) | Hard-coded port-publishing | `[transport-shape gated]`. See §6. Skip `--publish` when the harness's transport shape is not `TransportHTTPPort`. |

### 5.11 §7.15 `internal/archive/version.go`

| Site | Current shape | Proposed shape |
|---|---|---|
| `internal/archive/version.go:29` | `exec.Command("opencode", "--version").Output()` | Out of scope for B.2 (this is a static-binary-name reference, not a harness construction). The migration is owned by B.6 (#1084 — archive pipeline decoupling). The proposal flags only that the registry's `Names()` is the natural input to the eventual fix. |

### 5.12 §7.16, §7.17, §7.18 — archive / piexport / opencode internal packages

These files (`internal/archive/archive.go`, `internal/piexport/piexport.go`,
`internal/opencode/session.go`) work over opencode's on-disk storage
layout, not its harness adapter. They do not construct a harness
adapter and do not consult the registry. B.6 (#1084) owns the
storage-layout decoupling.

### 5.13 §7.19 `internal/payload/payload.go`

Schema package; not a construction site. B.5 (#1083) owns the schema
decoupling; B.2 has nothing to migrate here.

### 5.14 §7.20, §7.21, §7.22, §7.23

| Subsection | Why no migration in B.2 |
|---|---|
| §7.20 review pipeline | Review dispatch goes via the bus / host-API, neither of which constructs a harness. The harness-name forwarding (`cmd/review.go:53`, `:88`, `:207`) is migrated under §5.5. |
| §7.21 mergequeue watcher | Notifications via the prism bus and host-API; harness construction happens at the receiving end (already covered by `cmd/sidecar.go:282-297`). |
| §7.22 `cmd/clipboard.go` | Storage-layout coupling, not construction. B.6 owns. |
| §7.23 `cmd/prompt.go` | Sends a prompt via the host-API, which routes to the harness's `DeliverPrompt`. The harness has already been constructed by the receiving sidecar; `cmd/prompt.go` does not construct one. No migration. |

### 5.15 Adapter-package registration (new file)

The current opencode adapter package exposes `New` and `NewContainerMode`
as exported constructors. Registration adds a new file (proposed:
`internal/harness/opencode/register.go`) with a single `init()`:

```go
package opencode

import "github.com/prismatic-koi/prism/internal/harness"

func init() {
    harness.MustRegister(harness.Registration{
        Name:             "opencode",
        Shape:            harness.TransportHTTPPort,
        Factory:          func(endpoint string, c *http.Client, role, model string) harness.Harness {
            return New(endpoint, c, role, model)
        },
        ContainerFactory: func(endpoint string, c *http.Client, role, model string) harness.Harness {
            return NewContainerMode(endpoint, c, role, model)
        },
    })
}
```

The `cmd/` packages that today import `internal/harness/opencode`
directly switch to importing the package for its side effect only:

```go
import _ "github.com/prismatic-koi/prism/internal/harness/opencode"
```

A future `internal/harness/pi/register.go` does the same with
`Shape: harness.TransportStdioPipe`. The `cmd/` import set grows by
one blank import per harness; no other change.

## 6. `harness string` → `harness.TransportShape` migrations

These sites currently take or carry a harness *name* string but their
real dependency is the *shape*. Migrating them to `TransportShape`
removes the indirection through the registry on every call and lets
the shape decision happen at the boundary where the shape is known.

The `--port` / `--publish` / readiness-file / `OpencodeURL` plumbing
that B.1 catalogued (B.1 §3, classes 3 / 4 / 8) all hangs off these
shape-gated decisions. The actual code to drop the port plumbing for
stdio harnesses is B.3's deliverable, not B.2's; what B.2 owns is
identifying the call sites where the shape needs to be **consulted**.

| Site | Today | Tomorrow | B.1 class |
|---|---|---|---|
| `internal/session/sidecar.go:213` (`Port int` field on `StartSidecarOpts`) | Always populated; sidecar always allocates a port | Populated only when `harness.ShapeOf(opts.Harness) == TransportHTTPPort`. Field name stays; semantics document "zero for non-HTTP-port shapes". | Class 3 |
| `internal/session/sidecar.go:301` (`opencodeURL := fmt.Sprintf("http://localhost:%d", opts.Port)`) | URL constructed unconditionally | Constructed only for `TransportHTTPPort`. For other shapes the field is empty and the sidecar does not pass `--opencode-url`. The local-variable name `opencodeURL` should rename to `harnessEndpoint` (matches §5.2's `HarnessEndpoint` field). | Class 3 |
| `internal/session/sidecar.go:321, :338` (`cmdArgs = append(cmdArgs, "--port", strconv.Itoa(opts.Port))`) | `--port` always appended in container/bwrap modes | Appended only for `TransportHTTPPort`. The sidecar's own `--port` flag (`cmd/sidecar.go:95`) stays as a flag but its required-ness becomes shape-dependent. | Class 3 |
| `cmd/sidecar.go:95, :198-199, :215, :239-241` (`--port` flag declaration and validation) | `--port` is required in podman and bwrap modes | Required iff the in-process registry says the resolved harness has `TransportHTTPPort`. The sidecar needs a way to know its own harness; add `--harness` to `sidecarCmd.Flags()` (currently absent — see §7) and have `runSidecar` look up the shape. | Class 3 |
| `cmd/sidecar.go:256-280` (the `onReady` closure that writes `sidecar.ready`) | Always writes the readiness file in podman mode | Writes only when the harness's shape requires the agent pane to wait for a separately-running process — i.e. `TransportHTTPPort`. For stdio shapes the sidecar is the launcher and the readiness file is unneeded. (B.1 §3 class 4 carries an `[uncertain]` flag here for PI specifically; B.3 owns the resolution.) | Class 4 |
| `internal/sidecar/sidecar.go:412` (`mgr.WaitHealthy(ctx)`) | Always invoked in container mode | Invoked only when shape is `TransportHTTPPort` (the call dials an HTTP probe). For stdio shapes B.3 will replace this with a process-alive / pipe-open check. | Class 2 |
| `internal/sidecar/sidecar.go:609-704` (bwrap-mode startup-connect-timeout goroutine) | Watches for "the harness never bound to its port" | Watches for "the harness never produced its first event" (a harness-shape-agnostic phrasing). The trigger condition is shape-dependent; B.3 owns the rewrite. B.2 flags only the call site. | Class 2 |
| `internal/container/container.go:997, :1115-1116` (port binding and `--publish`) | Always present when `cfg.AllocatedPort != 0` | Present only when the harness shape is `TransportHTTPPort`. The container manager needs the shape; either as a new `cfg.HarnessShape TransportShape` field on `container.Config` (preferred — keeps the manager pure) or via a registry lookup keyed on `cfg.Harness` (today the manager has no harness-name field, so adding `HarnessShape` is the smaller change). | Class 8 |
| `internal/container/bwrap.go:608-635` (bwrap argv terminator with `--port`/`--hostname`/`--prompt`) | Always appends `--port`, `--hostname`, conditionally `--prompt` | Shape-dependent. For `TransportHTTPPort`: as today. For `TransportStdioPipe`: drop `--port` and `--hostname`; whether `--prompt` survives is harness-specific (the harness adapter's `ContainerCommand()` plus a future "supports CLI initial-prompt" capability declaration tells the wrapper). For `TransportFallbackScreenScrape`: drop everything; the bwrap wrapper invokes the harness binary as named by `ContainerCommand()`. | Class 8 |
| `cmd/agent_run.go:159-164` (port resolution from `status.HarnessPort`) | Always resolves a port | Resolved only when shape is `TransportHTTPPort`. For other shapes `port` stays zero; downstream `ctrCfg.AllocatedPort` is zero; the bwrap argv terminator already gates on this. | Class 8 |
| `internal/sidecar/sidecar.go:123` (`OpencodeURL string` field on sidecar `Config`) | Field name and value both HTTP-shape | Rename to `HarnessEndpoint string`. For `TransportHTTPPort` the value is still a URL; for `TransportStdioPipe` it carries the harness binary path or launch spec (B.3 owns the format). | Class 3 |
| `cmd/sidecar.go:91` (`--opencode-url` flag declaration) | Flag is opencode-named | Rename to `--harness-endpoint`. Keep `--opencode-url` as a deprecated alias until B.4 / E.1 says otherwise. | Class 3 |
| `internal/db/db.go:218-225` (`harness_port` column added in v7→v8 migration) | Column populated whenever a port is allocated | Populated only for `TransportHTTPPort` sessions; NULL for others. No schema change in B.2; B.5 / E.1 own whether the column moves to a per-shape side table. | Class 3 |

## 7. Open questions and `[uncertain]` flags

- **`cmd/sidecar.go` lacks a `--harness` flag today.** §6 above proposes
  adding one so the sidecar can look up its own shape via
  `harness.ShapeOf`. The shape question (HTTP-port vs stdio-pipe vs
  screen-scrape) determines whether `--port` is required, whether
  `OpencodeURL` is a URL or a launch spec, and whether the readiness
  file is written. Today the sidecar is started by
  `internal/session/sidecar.go:StartSidecarWithOpts` which has all the
  context to pass `--harness`; the missing flag is mechanical. **B.2
  notes the gap; the actual flag addition is implementation work.**
- **`[uncertain]` for `TransportFallbackScreenScrape`.** No real
  harness uses this shape today. The semantics in §3 are best-guess.
  When a real fallback harness lands (Codex prototype? early Claude
  Code?), the registration might want capability declarations beyond
  `TransportShape` alone (e.g. "supports prompt-via-stdin" /
  "supports prompt-via-CLI-flag"). B.4 is the natural home for
  capability granularity beyond a single enum value.
- **`[uncertain]` for stdio harnesses' "endpoint" string.** The
  `Factory` signature passes `endpoint string`. For HTTP this is a
  URL, unambiguously. For stdio the value could be the harness binary
  path, or a JSON-serialised launch spec, or an opaque token the
  factory unpacks. B.3 (sidecar lifecycle inversion) owns the answer;
  B.2 picks `string` to match HTTP and leaves the format unspecified
  beyond "the registered factory understands it".
- **`[uncertain]` for whether `ContainerFactory` is the right shape.**
  The opencode adapter's split between `New` / `NewContainerMode` is
  about `CreateSession` (POST vs GET) and `DeliverInitialPrompt`
  (write vs no-op). For stdio harnesses neither distinction may apply
  — the sidecar owns the launch unconditionally. The proposal keeps
  `ContainerFactory` as opt-in (nil for harnesses that do not
  distinguish) but flags that B.4's harness-group abstraction may
  fold this into a per-shape default rather than a per-harness opt-in.
- **`[uncertain]` for `agentsForHarness` (`cmd/review.go:88, :306`).**
  Today this returns a fixed five-agent list keyed on the literal
  string "opencode". Whether the review-agent set is harness-specific
  or transport-shape-specific (or harness-agnostic with a per-harness
  override) is a Track-C / B.6 decision. B.2's migration in §5.5 is
  the minimum surgery — replace the literal string switch with a
  registry consult — without committing to where the per-harness
  agent list lives.
- **`[uncertain]` for `internal/db/db.go`'s `harness TEXT NOT NULL
  DEFAULT 'opencode'`.** The schema default is the only place in the
  proposal where a literal "opencode" survives. Removing it would
  mean every session insert must pass an explicit harness value.
  Whether to keep the default (cheap, biased toward today's reality)
  or drop it (clean, requires every caller to pass a value) is a
  schema-evolution question owned by B.4 / E.1.
- **CLI doc strings need lazy formatting.** §5.4 notes that
  `cmd/spawn.go:148` and `cmd/review.go:53` would benefit from
  doc strings that name the registered harnesses dynamically, but
  cobra builds the flag set before all `init()` functions have run.
  The cheapest fix is `cobra.OnInitialize(func(){ updateUsage() })`;
  acceptable alternatives include keeping the doc string generic and
  emitting names only on validation error. B.2 prefers the latter
  to avoid coupling cobra init order to the registry.

## 8. What this proposal deliberately does not do

- It does **not** modify the `harness.Harness` interface. The
  `HealthCheck(ctx, port int)` signature surfaced by B.1 stays
  untouched here — its rework is downstream of B.3 (sidecar lifecycle
  inversion makes the `port` parameter movable).
- It does **not** introduce a `HTTPHarness` / `StdioHarness` shared
  base. That is B.4's deliberate question; B.2 keeps the registry flat.
- It does **not** propose a sidecar-as-launcher refactor. B.3 owns
  this; B.2 only flags the call sites where the lifecycle assumption
  bleeds into shape-gated decisions (§6, classes 2 and 4).
- It does **not** touch the payload schema (`internal/payload/`).
  B.5 owns that.
- It does **not** touch the archive / piexport pipeline. B.6 owns
  that. The `exec.Command("opencode", "--version")` in
  `internal/archive/version.go:29` is named in §5.11 only for
  cross-reference.
- It does **not** ship Go code. Every signature in §3, §4 is a
  proposed shape, not a committed implementation. The implementation
  work is the next wave (Track E synthesis will sequence it).

## Related

- Inventory: [`../architecture-inventory.md`](../architecture-inventory.md) §7.
- B.1 (#1074, landed #1096):
  [`B1-harness-transport-and-lifecycle-assumptions.md`](B1-harness-transport-and-lifecycle-assumptions.md)
  — classification this proposal consumes.
- Design doc:
  [`000-design-narrow-review-series.md`](000-design-narrow-review-series.md).
- Issue: #1080. Parent design: #1072.
- RFCs: #691 (multi-harness support), #606 (PI coding agent support).
- Sibling Track B issues: #1074 (B.1, landed), #1081 (B.3), #1082
  (B.4, held until B.2 lands), #1083 (B.5), #1084 (B.6).
