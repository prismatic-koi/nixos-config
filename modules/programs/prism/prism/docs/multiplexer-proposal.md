# Prism-native multiplexer — design proposal

**Status:** proposal. Gating decision: `x/vt` fidelity spike (see #2141). No
implementation work begins until the spike returns a proceed-verdict. Frame
the document accordingly — this is the umbrella for what will be a multi-PR
programme, and under-investment here costs more later.

**Scope:** the case for replacing prism's tmux dependency with a small,
purpose-built Go multiplexer that lives inside the prism repo, sharing the
prism binary with the existing `prismd` sidecar and CLI surfaces. Covers
motivation, proposed shape, layered architecture, integration with the
existing prism Go module, SSH remote-attach sketch, effort buckets, risks,
the gating spike, and explicit out-of-scope items.

**Audience:** the future reader deciding whether to commit to the
multi-month build (or, post-spike-stop, the future reader deciding whether
the vendor-libghostty-vt fallback is worth picking up); the workers
spawned against individual PRs in the programme once it begins; reviewers
evaluating whether a given PR is on-spec for the umbrella vision.

## 1. Motivation

This investigation began as a "should we replace tmux with herdr" question
and ended somewhere else entirely. The path is worth recapping because the
rejections inform the proposal.

**Herdr-as-drop-in (rejected).** Herdr is a Rust multiplexer with a
modern UX (sidebar nav, smooth scrolling, image rendering, integrated
SSH). On the surface it looks like a tmux replacement. In practice it
cannot replace prism's orchestration substrate. Prism is two systems
co-resident in one repo: a multiplexer-facing TUI (panes, hotkeys,
attach/detach) and an orchestration substrate (bus, merge queue, review
groups, escalation state machine, abtest, stats, sandboxing, mid-turn
steering via `prism prompt --deliver-as steer`). Only the first is
multiplexer-shaped. The second is a daemon plus a state machine that
happens to use a multiplexer for terminal IO. Herdr replaces the
multiplexer; the substrate continues to live in prism either way.

**Herdr + prism hybrid (parked).** A "herdr UI on top of a prism
orchestrator daemon" hybrid is feasible — herdr's protocol is open
enough to wire prism's bus into it as another data source — but it is
parked. The hybrid path has two structural problems: (1) herdr's pace of
change is determined by an upstream team prism does not control, so
prism's orchestration-side feature work would land against a moving
target; (2) any sidebar integration we want (per-session agent state,
review-cycle counters, escalation markers) becomes a contribution to
herdr that herdr must accept, with all the upstream-coordination cost
that implies. The hybrid is not ruled out forever; it is set aside while
the build-it-ourselves option is investigated.

**Build it ourselves (this proposal).** The remaining option is a
prism-native Go multiplexer that ships inside the prism binary. The
investigation has surfaced two pieces of evidence that make this credible
in a way it would not have been a year ago. First, prism's tmux coupling
is shockingly thin — `internal/tmux/tmux.go` is 522 LoC, called from 33
files, and the sidecar already runs as a detached subprocess outside any
tmux pane (see §4). Replacing tmux is a few-week refactor at the
boundary, not a months-long surgery through prism's guts. Second, the
Go terminal ecosystem now has the layered libraries we would need
(`creack/pty` for PTYs, `charmbracelet/x/vt` for the VT engine,
`bubbletea`/`lipgloss` for render, `wish` + `golang.org/x/crypto/ssh`
for remote attach), and two existence proofs of small Go multiplexers
written on these foundations exist (`aaronjanse/3mux`,
`Gaurav-Gosain/tuios`). The herdr layered-architecture readout (§3)
confirms that a multiplexer at the scale prism needs is a few tens of
thousands of LoC of novel code, not a hundred thousand.

The one dominant unknown is whether `charmbracelet/x/vt` has the fidelity
to host real TUIs cleanly. Herdr's answer to the same question was to
vendor Ghostty's Zig VT engine via FFI, which is circumstantial evidence
that the comparable Rust option (`alacritty_terminal`) was not good
enough. We have no equivalent evidence for `x/vt`. The spike at #2141
exists precisely to answer this; it returns a binary proceed/stop
verdict before any implementation work begins.

## 2. Proposed shape

A prism-native Go multiplexer. One binary, three long-lived process
roles, all sharing the existing `prism` Go module:

- **`prismd mux`** — the multiplexer server. One process per host (per
  user). Owns the workspace/tab/pane model, the PTY pool, the VT
  engines, the listening sockets, and the optional SSH transport. Holds
  all the long-lived state that today lives inside `tmux` itself.
- **`prismd sidecar`** — per-session sidecar. Same shape as today (see
  `internal/sidecar/sidecar.go`). The sidecar continues to run as a
  detached subprocess **outside** the multiplexer, talks to its
  worker/coordinator pane via the existing `pipe.sock` /
  `hostapi.sock` plumbing, and writes events to `prism.db`. Crucially,
  the sidecar does not change shape. The multiplexer swap is invisible
  to it.
- **`prism` CLI** — the existing user-facing CLI surface. `prism spawn`,
  `prism prompt`, `prism checkin`, `prism review`, `prism escalate`,
  `prism cleanup`, etc. The CLI gains the ability to talk to
  `prismd mux` over a local Unix socket for navigation operations that
  today shell out to `tmux switch-client`, `tmux send-keys`, etc.

The multiplexer replaces tmux entirely for prism's flows while remaining
a **general-purpose multiplexer**: nvim, zsh, lazygit, htop, claude, fzf,
pi-card all have to work, and work without visible cosmetic regressions
relative to tmux. That is the substrate requirement. Prism's
session-aware features (per-pane agent state in the sidebar,
review-cycle markers, escalation badges) are layered on top of that
substrate, not bolted into the VT engine.

**SSH remote-attach is first-class, not a v2.** The investigation
concluded that the SSH path is small (~3–5k LoC of `wish` + transport
glue) once the local mux is done, and the value is high: prism users
who attach from a laptop to a server-side prism workspace get the same
TUI as if they were local. Sketched in §5.

**Sidebar agent-state is first-class.** The sidebar is what makes
prism's TUI prism-shaped rather than tmux-with-extra-keys. Per-pane
agent state (worker/coordinator/review, current state, last turn
duration, review-cycle counter) comes from the sidecar via subscription
to the same DB the sidecar already writes to; the multiplexer renders
it. This is the structural reason a prism-native multiplexer is worth
building in the first place — the same data the sidecar writes to
`prism.db` for `prism checkin` and `prism dashboard` becomes the
sidebar's data source, with no protocol invention.

**Tmux feature parity is explicitly out of scope** (§10). We build only
the multiplexer features prism actually uses, plus the features a
general-purpose multiplexer must have to host a real TUI workflow.
Copy-mode with full vi keybinds, named buffers, hooks, the format-string
DSL, and `choose-tree` are all tmux features we do not attempt to
reimplement.

## 3. Architecture

A layered Go multiplexer. The herdr investigation (Turn 2 §A) produced
a clean readout of the layers a multiplexer-of-this-shape has to provide,
which double-checks our own decomposition. The table below shows
herdr's layer on the left, our Go equivalent on the right.

| Layer | Herdr (Rust) | Ours (Go) | Confidence |
|---|---|---|---|
| PTY allocation, child process exec, signal forwarding | `vt100`/native | `creack/pty` | High — battle-tested in 3mux, tuios, gotty. |
| VT engine: parse `\x1b[…`, maintain cell grid, attribute state, alt-screen, scroll-back | Vendored `libghostty-vt` (Ghostty's Zig engine via FFI) | `charmbracelet/x/vt` on top of `ultraviolet` | **Spike-gated.** ~30k LoC pure Go; herdr's FFI choice suggests Rust alternatives weren't enough. See #2141. |
| ANSI parsing / sequence emission | Ghostty's parser + `vte` | `charmbracelet/x/ansi` | High — prism already pulls this in as an indirect dep (`go.mod` line 17). |
| Render: turn cell grid into screen output | Custom Rust renderer | `bubbletea` + `lipgloss` | High — both are the de facto Go TUI stack; the dashboard at `cmd/dashboard.go` already uses both. |
| Workspace / tab / pane model | Custom Rust state machine | New: `internal/mux/workspace`, `internal/mux/pane`, `internal/mux/tile` | New build; tractable scope (3mux's pane model is ~1.5k LoC). |
| Socket API for the CLI to talk to the daemon | Custom Rust IPC | `net/rpc` over Unix socket, JSONL framing on top of existing prism conventions | High — prism's existing `pipe.sock` (see `pi-wire-protocol.md`) is the same shape. |
| Input dispatch (host hotkeys, prefix keys, app forwarding) | Custom | `charmbracelet/x/input` | Medium — well-tested for in-app use; multiplexer-shaped dispatch is novel. |
| Terminal capability detection (truecolor, image support, OSC reply) | Custom | `charmbracelet/x/term` | High. |
| Remote transport | Custom OpenSSH integration | `charmbracelet/wish` + `golang.org/x/crypto/ssh` | Medium — both libraries are stable; the combination is what `wish` is for. |

**Total novel LoC estimate** (Turn 2 §B from the investigator): herdr is
~145k LoC total but ~40–55k LoC of novel multiplexer logic on top of the
VT engine. Our equivalent number, factoring in that we are not
reimplementing tmux's copy-mode, format strings, or hooks (see §10), is
**~25–40k LoC of novel multiplexer logic** on top of the chosen Go
libraries. The single biggest leverage is reusing `bubbletea` / `lipgloss`
for the render layer — herdr writes its render layer from scratch in
Rust; we get a battle-tested one for free.

**Layer ownership inside the prism Go module.** All multiplexer code
lives under a new `internal/mux/` tree, parallel to the existing
`internal/tmux/`. The phased delete of `internal/tmux/` is §4. The
proposed initial layout:

```
internal/mux/
├── pty/        # creack/pty wiring; signal forwarding; resize handling
├── vt/         # x/vt host: cell-grid extraction, alt-screen, scroll-back
├── render/     # bubbletea program + lipgloss styles; sidebar widget
├── workspace/  # workspace/tab/pane model; persistence to prism.db
├── input/      # host-key dispatch; app-forwarding; OSC52 clipboard
├── socket/     # JSONL-framed Unix socket API for the CLI
├── ssh/        # wish-based remote-attach server; client-side dialer
└── testdata/   # corpus from #2141 → corpus.toml lives here post-spike
```

`internal/mux/workspace/` persists its model to **`prism.db`**, not a
flat file. The sidecar already owns the DB connection lifecycle; we
reuse it. The workspace model — sessions, tabs, panes, focus state —
is small relative to the existing `agent_events` table.

## 4. Prism integration

The structural reason this proposal is feasible: prism's coupling to
tmux is a thin shell around a small API surface, and the sidecar is
already multiplexer-independent today. The numbers, verified from
the current tree:

- **`internal/tmux/tmux.go` is 522 LoC.** Single file. The full tmux
  abstraction prism uses today.
- **33 files import it.** A `rg -l '"github.com/.*/prism/internal/tmux"'`
  across `modules/programs/prism/prism/` returns 33 matches. The
  callers span `cmd/` (CLI entry points), `internal/session/`
  (spawn/cleanup lifecycle), `internal/review/` (review group
  management), and `internal/dashboard/` (the in-tmux popup).
- **The sidecar runs detached.** `internal/sidecar/sidecar.go` is a
  long-lived process spawned **outside** any tmux session. Its inputs
  are the host-API socket (`hostapi.sock`) and the harness pipe
  (`pipe.sock`); its outputs are `prism.db` writes and `prism prompt`
  deliveries to other sidecars. Nothing in the sidecar's hot path
  knows or cares whether the user-facing TUI is tmux, herdr, or
  prismd mux. This was a deliberate design choice in
  `pi-wire-protocol.md`; it pays off here.

The 522-LoC `tmux.go` exposes about a dozen public functions —
`NewSessionDetached(name, dir)` (line 279), `SendKeys(target, keys)`
(line 260), `KillSession(name)` (line 300), plus session-existence
checks, window/pane operations, and a few format-string helpers.
Every one of these is a candidate for a corresponding
`muxClient.WorkspaceCreate(...)` / `muxClient.WorkspaceKill(...)` /
`muxClient.PaneSendInput(...)` call against the new daemon. The
migration shape:

1. **Land `internal/mux/` parallel.** Build the mux server, the CLI
   client, the socket API. Do not touch `internal/tmux/` yet.
2. **Introduce a `Multiplexer` interface** in a new package
   (`internal/multiplexer/`) with the operations the 33 callers
   actually use. Implement it twice: against `internal/tmux/`
   (existing behaviour) and `internal/mux/socket/` (the new
   daemon). Both implementations behind one interface.
3. **Migrate callers one at a time** to take a `Multiplexer`
   parameter, defaulting to the tmux implementation. The 33-file
   blast radius is large but mechanical; each migration is a small
   diff.
4. **Flip the default** to the mux implementation once feature
   parity is reached.
5. **Delete `internal/tmux/`.** Symmetric end state: one
   multiplexer abstraction, one implementation, the 522-LoC file
   gone.

**State-transition delivery flips from polling to subscription.**
Today the dashboard at `cmd/dashboard.go` polls `prism.db` for
session state changes; tmux is read-only and tells us nothing.
Once the mux owns the workspace model in its own process, the
sidecar can publish state-change events directly to the mux over
the socket and the sidebar widget redraws on receipt. Polling
goes away. The DB remains the durable record; the socket is the
notification path.

**`prism spawn` rewires from `tmux.NewSessionDetached` to
`muxClient.WorkspaceCreate`.** The existing call site is
`internal/session/spawn.go` line 1124 — a single call wrapped in the
spawn flow. The new call shape carries the same arguments
(session name, working directory) and returns the same readiness
contract (the sidecar's existing `.ready` file, written when the
harness handshake completes — see `pi-wire-protocol.md` §7.1).
No change to the sidecar's startup ordering; only the substrate
that opens the terminal changes.

**Hotkeys, prefix keys, popup overlays.** Today these are configured
in `modules/programs/prism/tmux.nix`. Under the new mux they move into
the Go binary as built-in defaults plus a user-overridable config file
under `~/.config/prism/mux.toml`. The Nix module shrinks correspondingly
— the line-count saving in `tmux.nix` is a real follow-on benefit, but
not the motivation.

**One subtle gain.** The `yankStrip` awk script in `tmux.nix` (lines
46–73) exists because tmux's `capture-pane` cannot tell that pi's card
rendering leaves a U+0020 prefix on every non-empty line. Under a
prism-native multiplexer, the yank path knows what is being yanked
(it is a sibling of the VT engine that produced the cells) and the
guard can move from "infer from cell content" to "ask the source". The
awk script disappears as a side effect of the migration. This is
emblematic of the kind of integration improvement we get for free once
the multiplexer is in-process.

## 5. SSH remote-attach

The SSH path is sketched here at planning fidelity; the implementation
issue will tighten the protocol details when it is filed. The motivating
shape comes from Turn 2 §C of the investigation.

**Server side: `wish` proxy to the local mux socket.** On the remote
host, `prismd mux serve --ssh-listen :2222` runs a `wish`-based SSH
server that proxies authenticated SSH sessions into the local mux
socket. Authentication is by SSH key, validated against the same
`~/.ssh/authorized_keys` the user already has (or a separate
`~/.config/prism/authorized_keys` if the user wants prism-specific
ACLs). `wish` handles the SSH transport — `golang.org/x/crypto/ssh`
underneath — and gives us a PTY-on-PTY model where the SSH-client
PTY is fed by the mux's VT engine.

**Client side: `prism attach --remote <host>`.** The CLI dials the
remote host's prism SSH listener over `x/crypto/ssh`, opens an
interactive session, and renders the remote workspace identically to
how it would render a local one. Keystrokes flow up; rendered cells
flow down.

**Render-encoding split: `SemanticFrame` vs `TerminalAnsi`.** Two wire
encodings for the down-link:

- **`SemanticFrame`** (preferred). The mux serialises the cell grid
  as a structured frame (cells + attributes + cursor + alt-screen
  flag) and the client renders it locally. This decouples server
  capability from client capability — the server doesn't need to
  know whether the client terminal supports truecolor or only 256
  colours; the client decides at render time. Larger frames over
  the wire but compresses well; small-payload latency-bound paths
  (typing in nvim) feel local.

- **`TerminalAnsi`** (fallback). The mux emits raw ANSI on the wire
  for clients that cannot render `SemanticFrame` (older clients,
  non-prism clients dialling the mux for diagnostics). Equivalent
  to tmux's wire today, plus our additions.

The client negotiates the encoding at connect time. Defaulting to
`SemanticFrame` for `prism attach --remote`; falling back to
`TerminalAnsi` for anything else.

**OSC52 for clipboard text.** OSC52 is a terminal-mediated clipboard
escape that already works through SSH today (most terminals understand
it). The mux emits OSC52 for any yank operation; the client terminal
forwards to the local clipboard. No clipboard daemon required.

**Image bridge via a side-channel + bracketed-paste.** Images
(`pi-card`'s embedded screenshots, lazygit's commit graph emoji, the
occasional `imv`/`viu` output) need more than ANSI. The proposed
shape: a parallel side-channel inside the SSH session ferries image
blobs from server to client; bracketed-paste markers in the cell
stream mark the rectangular region on screen where the image should
be composited. The client renders the image into that region using
its local image protocol (Kitty graphics, iTerm inline images, sixel,
or fall back to a placeholder). Implementation deferred until a
later issue; the SemanticFrame format reserves a `bracket: image-N`
attribute on cells for this purpose so the wire is forward-compatible.

**Live-handoff deliberately deferred.** "Attach a remote prism
session from a second client without disconnecting the first" is a
real multiplexer feature (tmux does this; herdr does this) but it
is a step beyond first-class remote-attach. The first SSH bucket
delivers single-client remote attach; multi-client live-handoff is
in the third effort bucket (§6).

## 6. Effort estimate

Three buckets, ordered by what unlocks what. Calendar ranges assume
one full-time engineer with focused weeks (not 20% time over a
calendar year). Confidence levels factor in the spike's outcome.

| Bucket | Scope | Calendar | LoC delta | Confidence |
|---|---|---|---|---|
| **MVP** | Local-only mux. Single workspace, multiple panes, host hotkeys, the dashboard sidebar, OSC52 yank, parity for the 33 tmux callers in prism. Delete `internal/tmux/`. | **3–6 months** | +15–25k novel; −522 in `internal/tmux/`; net ~+15–25k | Medium-high (if spike passes). The 3-vs-6 spread is dominated by the long-tail `x/vt` fidelity work and the 33-caller migration. |
| **MVP + SSH** | + remote-attach via `wish`; SemanticFrame negotiation; single-client only. | **5–9 months** total (MVP + ~2–3 incremental) | +3–5k for the SSH path | Medium. `wish` is stable; the SemanticFrame protocol is novel. |
| **+ live-handoff + images** | + multi-client live-handoff (second `prism attach --remote` joins an existing session); + image bridge for Kitty/iTerm/sixel-capable clients. | **7–12 months** total | +5–8k | Lower. Live-handoff has subtle correctness invariants (input race, focus race); the image bridge depends on continued upstream protocol stability. |

**Honesty on the `x/vt` unknown.** The 3-vs-6 month spread on MVP is
not paperwork — it is the difference between (a) `x/vt` rendering the
corpus cleanly out of the box, in which case the multiplexer is mostly
a workspace-model + render-loop + socket-API exercise, and (b) `x/vt`
having long-tail defects that the spike caught but proceed-verdict
deemed addressable, in which case real time goes into upstream
patches or local workarounds. The spike at #2141 narrows this
distribution before we commit.

**Honesty on the LoC.** "+15–25k novel LoC" is a planning estimate
based on the herdr readout (Turn 2 §A) minus the features we don't
build (Turn 2 §B, §10 here). It is not a target. The actual number
depends on how much we can lean on `bubbletea` for the render loop
and how much workspace-model complexity we accept. Both 3mux (~9k LoC
total) and tuios (~12k LoC total) are existence proofs that a working
multiplexer can be small.

**What the buckets do not include.** No tmux feature parity beyond what
prism actually uses (§10). No cross-cutting refactor of prism's
orchestration substrate (escalation, merge queue, abtest — these are
unrelated and stay as they are). No migration of the dashboard's polling
loops to subscription unless and until the mux is wired in (the polling
loop in `cmd/dashboard.go` keeps working under the tmux substrate
during the migration, which is what lets us migrate incrementally).

## 7. Risks

The three risks worth naming explicitly, plus the AGPL question.

### 7.1 `x/vt` long-tail TUI fidelity

The dominant risk. Discussed in §1, §3, §6, gated by #2141. Failure
mode: the spike returns a proceed-verdict on the 6-app corpus, but a
seventh app (or an eighth, discovered post-MVP) exposes a class of
defect that requires either (a) substantial upstream `x/vt` work,
(b) a local workaround in `internal/mux/vt/`, or (c) acceptance of a
visible regression relative to tmux for that app. Mitigations: keep
the corpus open-ended (post-spike, `corpus.toml` lives in
`internal/mux/testdata/` and grows as users find new defects); commit
to investing in upstream `x/vt` patches when defects are addressable
there; document the regression policy explicitly in the MVP
introducer's PR.

### 7.2 33-caller migration creating subtle behavioural drift

The 33 files that import `internal/tmux/` span the CLI, session
lifecycle, review-group management, and dashboard. Migrating them
one at a time behind a `Multiplexer` interface is mechanical, but
behavioural drift between the tmux implementation and the mux
implementation is plausible — tmux has idiosyncratic behaviour around
session-name validation, send-keys timing, popup overlay positioning,
and detach-on-exit semantics. Failure mode: a session that works
under tmux fails under mux in a way the test suite did not catch.
Mitigation: keep the dual-implementation phase **long** (do not rush
to delete `internal/tmux/`); add behavioural-equivalence tests at
the `Multiplexer` interface level that run both implementations and
diff outputs; ship the MVP with `PRISM_MUX_BACKEND=tmux|mux` so users
can fall back during the rollout window. Default flip is a separate
PR, not part of MVP.

### 7.3 Calendar discipline on a multi-month programme

The proposal is 3–6 months of focused work for MVP, 5–9 for MVP+SSH,
7–12 for the full stack. Failure mode: the programme drags into a
12-month MVP because each PR scope-creeps, or it ships an MVP that's
mostly working but never actually replaces tmux in `tmux.nix`. The
programme then sits half-done, paying integration cost on both
substrates simultaneously. Mitigation: every PR in the programme has
a tightly-scoped acceptance-criteria block in its issue; the
introducer PR for the `Multiplexer` interface commits to a
migration-end milestone (delete `internal/tmux/`) at issue-creation
time; the abtest framework — already in prism — runs the corpus from
#2141 on every mux PR to catch fidelity regressions during the build.

### 7.4 AGPL question (herdr's licence)

Herdr is **AGPL-3.0-or-later**. Reading herdr's source for design
inspiration, taking notes on its architecture, copying its layer
decomposition into this document — all fine. Copy-pasting Rust source
into our Go module is not. The line is:

- **Pattern cribbing is fine.** "Herdr's pane-tree is a quad-tree with
  named focus paths" is an idea; ideas are not copyrighted.
- **Source-level cribbing is not.** Translating a herdr function
  line-by-line into Go and committing it would import AGPL into the
  prism module, which is incompatible with prism's MIT licence.

If at any point during implementation a worker finds themselves
wanting to crib source from herdr's repo, they must escalate to a
licence decision before committing. The default answer is "don't" —
write the equivalent code from spec, not from upstream source.

## 8. Spike-gated

**No implementation work begins until the spike at #2141 returns a
proceed-verdict.** This is a hard gate, not a soft preference. The
programme has bucket estimates of 3–12 months; spending two weeks on
the spike to narrow the dominant unknown is the highest-leverage move
available.

The spike's artefact contract (full detail in #2141):

- **Sibling directory** at `modules/programs/prism/mux-spike/` with its
  own `go.mod` (independent Go module, no `vendorHash` interaction
  with `pkgs/prism.nix`).
- **Flake output** via `pkgs/mux-spike.nix`, modelled on
  `pkgs/battery-monitor.nix` minus the `runChecks` plumbing.
- **No new CI lane.** The spike opens real PTYs and writes capture
  artefacts, which the homeless-shelter sandbox would fail. The
  existing `go-tests` / `nix-build-prism-checked` jobs may trigger
  via the `**/go.mod` paths filter that matches the spike's `go.mod`,
  but both jobs are hard-coded to build the prism module, so spike
  code never enters the homeless-shelter sandbox.
- **Three files touched** across the spike's lifecycle: the spike
  directory, `pkgs/mux-spike.nix`, the one line in `flake.nix`.
  Symmetric deletion: stop-verdict cleanup and proceed-verdict
  cleanup both remove these three locations and leave behind only the
  surviving artefacts (`report.md`, the curated `corpus.toml`).
- **Binary surface:** `nix run .#mux-spike -- run <cmd>` (interactive
  smoke), `nix run .#mux-spike -- corpus --out <dir>` (structured
  walk), parallel `corpus/capture-via-tmux.sh` (apples-to-apples
  comparison under tmux).
- **Seed corpus:** six apps — `nvim`, `pi-card`, `lazygit`, `claude`,
  `htop`, `fzf-files` — chosen to span the fidelity surface area.
  Each entry specifies argv, settle duration, trigger keystrokes, and
  hand-grading notes.
- **Kill criterion** baked into the issue: ≥ 5/6 corpus apps in the
  top two fidelity categories (3 = functionally equivalent, 4 =
  pixel-equivalent) → proceed. ≥ 2/6 in the bottom two
  (1 = broken, 2 = visibly degraded) → stop. Edge case (3 or 4 apps
  top-two, the rest middle) → stop-and-investigate, escalate to
  coordinator.

**On proceed-verdict:** the spike code is **still deleted**. What
transfers to the multiplexer programme is `report.md` plus the
curated `corpus.toml`, which becomes
`modules/programs/prism/prism/internal/mux/testdata/corpus.toml`. The
real multiplexer lives inside the prism Go module from day one of the
MVP bucket — there is no "spike continues as a sub-project" path.

**On stop-verdict:** the proposal at this document goes back into a
"deferred / blocked on VT engine" state. The follow-up choice is
between two options, captured in a fresh issue at the time:

- **Vendor `libghostty-vt` via cgo.** Same engine as herdr, FFI dep,
  pixel-equivalent fidelity. Higher complexity floor; non-pure-Go.
- **Shelve.** Park the programme; stay on tmux indefinitely. The 522
  LoC in `internal/tmux/` and the existing `tmux.nix` continue to do
  the job they do today.

The spike PR does not pre-commit to either follow-up. The follow-up
is a coordinator decision once the verdict is in hand.

### Spike findings post-merge

After the spike's proceed-verdict landed in #2143, a hands-on smoke
test of the `run` subcommand surfaced two interactive-paint bugs that
the corpus grading process did not catch: `host.Render()`'s output
uses bare LF between rows (right-drift in raw mode) and does not pad
short rows (previous-frame residue). These were spike-level bugs in
the live-replay loop, not engine-level bugs in `x/vt` — the corpus
path drives `host.Snapshot()` rather than `host.Render()`, so the
fidelity grading was unaffected and the verdict stands. The lesson
transfers to the multiplexer programme: a corpus snapshot path
(`host.Snapshot()` + structural cell extraction) is necessary but not
sufficient for fidelity grading. Any future characterisation spike
should also drive a live render path (`host.Render()` or equivalent)
so that interactive-use bugs surface during grading rather than at
first-hands-on. The mux-spike's verdict remains valid because
cell-grid fidelity is the load-bearing question; the live-render bugs
were spike-level, not engine-level.

## 9. Rollout and reversibility

Captures the agreed strategy for how the programme would execute **if**
the spike at #2141 returns proceed and the subsequent smoke-test of the
interactive `run` path passes. Descriptive of the chosen approach, not
a commitment to start.

### 9.1 Train-to-main with feature-flagged late cutover

The programme would land as a train of atomic PRs into `main`, not on
a long-lived branch. Each PR is independently shippable, and `main`
stays coherent throughout. The reversibility property comes from two
structural facts about the architecture and one explicit gate:

1. **The bulk of the work is additive.** New code under
   `internal/mux/` (the multiplexer package), a new socket-API server,
   the workspace/tab/pane data model, the SSH transport, the sidebar
   render — none of these have callers in the existing `cmd/` or
   `internal/sidecar/` paths until the cutover. Reverting any
   individual additive PR is a `git revert` of one merge SHA with
   negligible conflict surface.
2. **Irrevocability is concentrated in 1–2 PRs.** The actual switch
   from `tmux.NewSessionDetached`
   (`internal/session/session.go:1124` and the ~33-caller surface in
   §4) to `muxClient.WorkspaceCreate` lands in a small number of
   cutover PRs. Until those merge, `main` continues to use tmux for
   every existing flow.
3. **The cutover PRs are gated behind `PRISM_USE_MUX=1`.** Both code
   paths coexist while the env var defaults to off. Reverting the
   cutover at runtime is `unset PRISM_USE_MUX`; reverting in source is
   a one-PR revert of the gating commit. The dead `internal/mux/` code
   remains in `main` as residue but does nothing.

### 9.2 Phased rollout

| Phase | What lands | Reversion cost |
|---|---|---|
| **1. Additive build** | `internal/mux/`, socket API, w/t/p model, SSH, sidebar, persistence — all unused | Per-PR `git revert`; no user-visible state |
| **2. Cutover PR(s)** | Switches `prism spawn`, `cleanup`, `switch`, `nav`, dashboard wiring behind `PRISM_USE_MUX=1` | Unset env var; revert the cutover PR(s) |
| **3. Soak** | Daily use under `PRISM_USE_MUX=1` for a set duration, exercising real workflows | Trivial: stop setting the env var |
| **4. "Are we sure" gate** | Explicit go/no-go check before phase 5 | This is the point of no easy return |
| **5. Tmux deletion** | Delete `internal/tmux/`, remove the `PRISM_USE_MUX` flag, remove the dead tmux code paths from `cmd/cleanup.go` and the rest of the 33-caller surface | Expensive — restoring tmux machinery requires reverting the deletion PR plus catching up to any subsequent changes |

The point of phase 4 is to make the irrevocable transition **a
deliberate decision**, not a quiet drift. A soak duration is set
up-front (e.g. "four weeks of `PRISM_USE_MUX=1` daily use before phase
5"); when the duration elapses, the gate is reviewed and an explicit
decision is made.

### 9.3 What "abandon" looks like at each phase

- **During phase 1:** the abandon is closing out the programme and
  either reverting the additive PRs (clean main, costs revert effort)
  or leaving them as dead code (cheaper, accumulates residue). The
  dead code is bounded — call it ~15–20 kLoC per §6's MVP estimate.
- **During phase 2 or 3:** unset `PRISM_USE_MUX`; revert the cutover
  PR(s) if the gate should also come out of `main`, otherwise it's
  runtime-free.
- **During phase 4 review:** decline to proceed. Phase 5 doesn't land.
  Phase 3's runtime-flag fallback survives indefinitely.
- **After phase 5:** the long-lived-branch reversion advantage would
  have helped here; in this strategy, post-phase-5 abandon requires
  explicit restoration of the tmux machinery. This is the deliberate
  trade-off — abandon is cheap until the soak gate, then becomes a
  real undertaking.

### 9.4 Why not a long-lived branch?

The long-lived-branch alternative was considered. It offers a `git
branch -D mux-main` clean-abandon property throughout. The reasons
against:

- Prism's `prism merge` / `prism review` / `prism escalate` tooling
  assumes `main` as the integration target. Merge queue, pre-flight
  rebase gate, and coordinator auto-discovery would all need
  base-branch awareness — a meta-project on the order of one week of
  focused prism work that would have to land before the multiplexer
  programme could use it.
- The architecture's additive shape means the train-to-main reversion
  cost is bounded throughout phases 1–3. The cleanest-abandon property
  of a long-lived branch is mainly worth its tooling cost when
  irrevocability is spread across the whole programme; here it's
  concentrated in phase 5.
- "Long-lived branch as a first-class prism capability" is a real and
  interesting question, but it is a separate meta-project that should
  be scoped on its own merits, not as a prerequisite for the
  multiplexer.

### 9.5 Tooling implications

No changes to prism's merge queue, review gate, or escalate
auto-discovery are required under the chosen strategy. The standard
coordinator workflow on `@main` handles the programme — design doc
plus atomic PRs that each squash-merge into `main`, with dependency
ordering captured as the train of PRs is filed.

## 10. Out of scope

The proposal is deliberately narrow. The following are **not** part
of any bucket in §6 and not part of the MVP definition:

### 10.1 Tmux feature parity

- **Copy-mode with full vi keybinds.** Tmux's copy-mode is a tiny
  editor; reimplementing it is a multi-week side-quest. The mux ships
  with a simpler "select, OSC52 to clipboard, exit selection mode"
  flow that covers the 95% case. Users who need vi-style navigation
  inside scroll-back can drop into `less` or `nvim` on the buffer.
- **Named buffers.** Tmux's buffer stack (paste-buffer-0,
  paste-buffer-1, ...) is not reimplemented. OSC52 to the system
  clipboard is the one clipboard the mux knows about.
- **Hooks.** Tmux's `set-hook` mechanism is a powerful extension
  point that prism does not use. The mux does not implement it.
- **Format-string DSL.** Tmux's `#{...}` format strings are a
  small templating language used in status bars and `display-message`.
  The mux's status bar is implemented in Go with explicit fields;
  there is no end-user format string DSL.
- **`choose-tree`, `choose-session`, `choose-window`.** The interactive
  pickers tmux ships are replaced by the existing `prism dashboard`
  and `prism nav` flows, which already do the prism-specific equivalent.
- **`#{pane_current_command}`-style introspection.** Where prism uses
  this today (e.g. the `sandboxedPaneGuard` check in
  `modules/programs/prism/tmux.nix`), the equivalent in the mux is a
  direct query against the workspace model. No format-string parsing.

### 10.2 The herdr-on-pi-on-prism hybrid

The hybrid path described in §1 is parked, not killed. If the spike
returns a stop-verdict and the libghostty-vt-via-cgo follow-up is also
rejected, the hybrid moves from "parked" to "active candidate". Until
either of those, the hybrid is out of scope for any work in this
programme.

### 10.3 Cross-cutting orchestration changes

The bus, merge queue, review groups, escalation state machine, abtest,
stats, sandboxing — all of these stay exactly as they are. The
multiplexer programme touches only the multiplexer-shaped layer and
its 33-caller boundary in prism. Any change to orchestration belongs
in a separate issue against the existing subsystems, not in any PR
under this programme.

### 10.4 Live-handoff (deferred)

Multi-client live-handoff — a second `prism attach --remote` joining an
existing remote session without disconnecting the first — is the third
effort bucket in §6 and is deferred until after MVP + SSH ships.
"Single-client remote attach" is sufficient for the value the SSH path
unlocks; live-handoff is a real feature but a real cost.

### 10.5 Windows / non-PTY backends

The mux assumes a POSIX PTY substrate. WSL2 works via its POSIX layer;
bare Windows (ConPTY) does not. Out of scope. Darwin and Linux are the
only supported substrates, matching prism's existing platform support.

## 11. Related

- **Spike issue (this proposal's gate):** #2141 — full artefact contract,
  corpus manifest, kill criterion, acceptance criteria.
- **Sidecar wire protocol design doc:**
  [`pi-wire-protocol.md`](./pi-wire-protocol.md) — establishes the
  sidecar-vs-multiplexer separation this proposal builds on; relevant
  §1 (architecture overview, the per-session socket model) and §2.1
  (the sidecar substrate is multiplexer-independent).
- **Existing tmux abstraction (proposed deletion target):**
  `modules/programs/prism/prism/internal/tmux/tmux.go` — 522 LoC, the
  full surface area being replaced. The 33-caller migration starts here.
- **Existing tmux config (proposed shrink target):**
  `modules/programs/prism/tmux.nix` — Nix module, ~400 LoC, currently
  configures tmux hotkeys, popup overlays, the `yankStrip` filter, and
  the `sandboxedPaneGuard`. Under the mux these move into the Go
  binary; the Nix module shrinks correspondingly.
- **Existing flake output pattern:** `pkgs/battery-monitor.nix` is the
  reference for `pkgs/mux-spike.nix` (and, post-MVP, for any mux-only
  derivation if one is needed — though by default the mux ships as
  part of the existing `prism` derivation).
- **Herdr's docs:** [`herdr.dev/docs/`](https://herdr.dev/docs/) — read
  for design inspiration. Source-level cribbing is constrained by §7.4.
- **Reference Go multiplexers:** [`aaronjanse/3mux`](https://github.com/aaronjanse/3mux)
  (~9k LoC) and [`Gaurav-Gosain/tuios`](https://github.com/Gaurav-Gosain/tuios)
  (~12k LoC) — existence proofs that a working Go multiplexer at the
  scale prism needs is in the LoC range claimed in §6.
