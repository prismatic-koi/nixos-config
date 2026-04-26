# B.6 — Archive-and-export pipeline decoupling from opencode raw layout

Track B (harness) Wave 2 follow-up to B.1 in the narrow architecture-review
series defined in [`000-design-narrow-review-series.md`](000-design-narrow-review-series.md)
(parent design issue #1072). This document is the harness-track lens on the
archive-and-export pipeline — inventory §7.15, §7.16, §7.17, §7.18 — viewed
through the question:

> If a session's raw on-disk session data is shaped by the harness that
> produced it (opencode's `~/.local/share/opencode/storage/{session,message,part,tool-output}/`
> tree vs PI's JSONL session files with parent/child IDs per RFC #606),
> what shape do the archive copy and the post-archive translation need to
> take so that each harness can plug in its own raw-layout knowledge
> without forking `cleanup`?

This is a **proposal document**. It contains zero implementation work. The
deliverable is the proposed adapter shape, the canonical-export-format
discussion, the call-site map, and a migration order — to be actioned in a
later implementation issue.

## Context recap

The relevant inventory entries:

- §7.15 `internal/archive/version.go` — `[opencode-only] / [structural]`.
  `exec.Command("opencode", "--version")` to capture the harness version
  string into the archive manifest.
- §7.16 `internal/archive/archive.go` — `[opencode-only] / [structural]`.
  Archive layout copies opencode's `~/.local/share/opencode/storage/` subtree
  (or its podman-per-session variant) into the archive `raw/` directory.
- §7.17 `internal/piexport/piexport.go` — `[opencode-only] / [structural]`.
  Pure function over opencode's on-disk shape: reads `raw/session.json`,
  `raw/messages/msg_*.json`, `raw/parts/msg_<id>/prt_*.json`,
  `raw/tool-output/tool_*` and writes `session.jsonl` in pi-mono v3.
- §7.18 `internal/opencode/session.go` — `[opencode-only] / [structural]`.
  `LatestSessionForDir` queries opencode's SQLite to find the most-recent
  session ID for a directory.

B.1's per-site classification adds the transport-shape dimension and ranks
all four sites as `[opencode-only] / [transport-agnostic]` — none of them
touch HTTP/SSE or stdio pipes; the coupling is purely to opencode's
on-disk *layout*. (See B.1 §7.15-§7.18 entries at
`B1-harness-transport-and-lifecycle-assumptions.md:548-566`.) That makes
this review independent of B.2/B.3/B.4 (which deal with transport shape)
and only loosely coupled to B.5 (event payload schema) — B.5 governs what
the sidecar writes to `agent_events` at runtime, while this review governs
what the cleanup path copies and translates from on-disk session storage
at session end.

For PI specifically, RFC #606 ("Pi architecture summary" → "Session
persistence") says:

> JSONL files with a tree structure (parent/child IDs for branching) —
> reviewable after the session ends

and the compatibility table calls out:

> | Event/observability layer | JSON Lines event stream (no SQLite) | Requires rearchitecting |

So PI does not have opencode's per-record JSON files under `storage/`,
and does not have an opencode-style SQLite database to query for "most
recent session for this directory."

## Current pipeline

The pipeline runs at session-cleanup time. It has three layers, all driven
from `cmd/cleanup.go`'s `runSessionArchive`:

```
runSessionArchive (cmd/cleanup.go:755)
  ├── archive.HarnessVersion()          → captures `opencode --version`
  ├── archive.PrismGitSHA()             → captures prism's own VCS revision
  └── archive.Run(archive.Params{...})  → copies opencode storage subtree
        └── piexport.Translate(archivePath)  → opencode raw → pi-mono v3 JSONL
```

### 1. Source-path resolution — opencode-shaped, isolation-mode-aware

`internal/archive/archive.go:245-274` (`resolveStorageRoot`) returns the
host-side opencode storage root by switching on `IsolationMode`:

- `host` / `bwrap` → `$HOME/.local/share/opencode/storage`
  (`archive.go:267`).
- `podman` → `$HOME/.local/share/opencode/prism-sessions/<containerName>/storage`
  (`archive.go:269-270`); the per-session subdirectory is the Darwin
  virtiofs WAL-mode workaround documented in the function comment at
  `archive.go:252-258`.
- `unknown` → returns an error (`archive.go:271-273`).

The branch that matters for this review: there is no `Harness`-aware
dispatch at all. `IsolationMode` decides where on disk to look, and the
fixed string literal `"opencode"` decides whose layout to expect once you
get there.

### 2. Archive copy — opencode-shaped subtree walk

`internal/archive/archive.go:287-388` (`copySessionFiles`) is the
opencode-layout reader. It hard-codes:

- `storage/session/<projectID>/ses_<id>.json` → `raw/session.json`
  (`archive.go:294-303`, with `findSessionFile` at `:393-408` discovering
  `<projectID>` by walking subdirectories).
- `storage/message/<harness_session_id>/*.json` → `raw/messages/*.json`
  (`archive.go:306-311`).
- `storage/part/<msg_id>/prt_*.json` → `raw/parts/<msg_id>/prt_*.json`
  (`archive.go:313-365`), with the message IDs derived from the message
  filenames just copied.
- `storage/tool-output/tool_<id>` → `raw/tool-output/tool_<id>`
  (`archive.go:367-385`), with the tool IDs discovered by parsing the
  part files for `Asset: "tool_..."` references via
  `toolOutputIDsFromPart` (`:480-508`).

Surrounding this: directory creation under a `.tmp-<instanceID>/`
staging dir (`archive.go:138, :157`), atomic rename to the final
`<startedAtISO>_<instanceID>/` directory (`:207`), manifest write
(`:201-204` → `writeManifest` at `:530-568`), agent-run.log copy when the
file exists (`:189-199`), and `ErrAlreadyExists` idempotency
(`:147-149`). All of that surrounding scaffolding is harness-agnostic —
it would survive any harness change unchanged. The `copySessionFiles`
body and the `resolveStorageRoot` body are the opencode-only halves.

### 3. Harness version capture

`internal/archive/version.go:28-34` runs `opencode --version` and trims
the trailing newline. Failure (binary missing, non-zero exit) returns
`""`, recorded as the empty `HarnessVersion` field on the manifest.

### 4. Manifest write — already harness-aware in field naming

`internal/archive/archive.go:510-528` writes a `manifest.json` whose
fields already use the harness-neutral names `harness`,
`harnessSessionId`, `harnessVersion` (the v14→v15 DB migration that
renamed `opencode_sid` to `harness_session_id` is the precedent — see
inventory §7.11 / `internal/db/db.go:246-250`). This is the part of the
archive layer that is *already* multi-harness in shape; only the
**values** captured for those fields are opencode-derived today.

### 5. Post-archive translation — opencode raw → pi-mono v3

`internal/piexport/piexport.go:43-83` (`Translate`) reads back the
archive's `raw/` subtree exactly as `archive.go` wrote it, parses
opencode's on-disk types (`ocSession`, `ocMessage`, `ocPart`,
`ocToolState` at `piexport.go:87-162`), and emits pi-mono v3 entries
(`piSessionHeader`, `piMessageEntry`, `piCustomEntry`,
`piMessage`/`piUsage`/`piCost`, content blocks at `piexport.go:170-252`).

Notable shape choices:

- The pi-mono session header has `version: 3` baked in
  (`piexport.go:172-178, :355`), aligned with the `PiMonoVersion = 3`
  constant in `archive.go:52`.
- The translator linearises opencode's per-message tool-call sequence
  into a flat parent/child chain of `message` entries
  (`buildEntries` at `piexport.go:348-492`, with `prevID` threading at
  `:363, :418, :476, :486`). pi-mono v3 supports a tree via `parentId`
  but `piexport` only writes a linear chain.
- `customType: "opencode.system"` (`piexport.go:686`) and
  `customType: "opencode.snapshot.start"` (`piexport.go:707`) are the
  only opencode-named fields that appear in pi-mono output. Everything
  else is opencode-shape input → pi-mono-shape output.

### 6. Post-archive call-back into DB

`cmd/cleanup.go:829` calls `d.UpdateSessionArchivePath(instanceID,
archivePath)` to persist the archive directory path into the `sessions`
table. `cmd/archive.go` is the consumer side: it reads
`sessions.archive_path` to answer `prism archive <session>` queries
(`cmd/archive.go:83-117`).

### 7. The unused fourth file

`internal/opencode/session.go:39-57` (`LatestSessionForDir`) is the
fourth opencode-coupled site flagged by inventory §7.18, but no
production caller invokes it — only the package's own test file
(`internal/opencode/session_test.go`). It is a "session resume" helper
for `opencode -s <id>` that pre-dates the current spawn flow and is
effectively dead code from the archive-pipeline's perspective. Treating
it as part of B.6's surface is correct because it shares the
`internal/opencode` package name and the opencode-specific storage-root
assumption; what to do about it (delete it; revive it through a
harness adapter; leave it as legacy) is a one-line decision for the
implementation issue, not a design question.

[uncertain — whether any external script or out-of-tree consumer reaches
into this package via `go-import` outside what the in-tree grep shows.
The narrow-review search scope is the prism Go module only.]

## Where the coupling concentrates

Pulling the above together, the opencode-shape coupling lives in **four
narrow places**, all on the read side of the pipeline:

1. **Storage-root resolution** — `archive.go:245-274` knows the on-disk
   path for opencode's storage tree under each isolation mode.
2. **Subtree-walk knowledge** — `archive.go:287-388` knows opencode's
   `session/<projectID>/`, `message/<sid>/`, `part/<msgID>/`,
   `tool-output/<id>` layout, and the asset-reference convention that
   threads them.
3. **Version capture** — `version.go:28-34` knows the harness binary is
   called `opencode` and accepts `--version`.
4. **Translator's input schema** — `piexport.go:87-162` is a verbatim
   transcription of opencode's on-disk JSON schema as Go structs.

The output side (pi-mono v3 JSONL emitted by `piexport.go`, the
manifest format written by `archive.go:writeManifest`, the
`archive_path` recorded into the `sessions` row, the directory layout
under `~/.local/share/prism/archive/<repo>/<startedAtISO>_<instanceID>/`)
is **not** opencode-shaped. The post-archive contract — "there is a
directory; it has `manifest.json`, `raw/`, optionally `agent-run.log`,
optionally `session.jsonl`" — is already harness-neutral in shape; only
the *contents* of `raw/` reflect the source harness's storage choices.

That is a useful asymmetry: the proposed adapter has to abstract the
**read side** for each harness, but the **write side** (archive
directory layout, manifest schema, optional translated trace file) can
remain centralised.

## Proposed `ArchiveAdapter` interface

```go
// Package harnessarchive (internal/harness/archive — name TBD; see
// "Open questions" §3) defines the per-harness adapter that the
// session-cleanup archive pipeline consults to copy a harness's raw
// on-disk session data and (optionally) translate it to a canonical
// post-archive trace format.
package harnessarchive

import "context"

// SourceParams describes the session whose raw data is to be located
// on disk. It is a strict subset of archive.Params, carrying only the
// fields the adapter needs to find the source files; the broader
// metadata used for the manifest stays on archive.Params.
type SourceParams struct {
    // SessionName is the prism session name (e.g. "nixos-config@feature").
    // Adapters that derive their on-disk path from a per-session
    // subdirectory (today: opencode under podman) consume this.
    SessionName string

    // InstanceID is the session's UUID (sessions.instance_id). Adapters
    // that key their storage by the prism instance ID rather than by
    // a harness-internal session ID consume this.
    InstanceID string

    // HarnessSessionID is the harness-assigned session identifier
    // captured by the sidecar at session-create time
    // (sessions.harness_session_id). May be empty when the harness
    // failed to start. Adapters whose on-disk layout is keyed on this
    // ID — e.g. opencode's ses_<ULID> path components — consume this.
    HarnessSessionID string

    // IsolationMode is "host", "bwrap", "sandbox-exec", or "podman".
    // Adapters whose on-disk path differs by isolation mode (today:
    // opencode under podman has a per-session subdirectory) consume
    // this.
    IsolationMode string
}

// ArchiveAdapter is the per-harness contract for the session-cleanup
// archive pipeline. Each Harness implementation provides one (or
// returns nil from a getter to opt out, in which case cleanup writes
// only manifest.json and any agent-run.log).
type ArchiveAdapter interface {
    // SourcePath returns the absolute host-side directory that the
    // harness writes its raw session data into, for the session
    // described by p. The returned path may not exist on disk yet —
    // callers are responsible for handling the empty-source case
    // (today: opencode sessions that fail before harness_session_id
    // is captured produce an empty raw/ subtree, see
    // archive.go:182-187).
    //
    // SourcePath is called once per archive run, before Archive.
    SourcePath(p SourceParams) (string, error)

    // Archive copies the harness's raw session subtree from srcPath
    // into rawDir. rawDir is the absolute path to the per-archive
    // raw/ directory, already created by the central archive runner
    // with mode 0o700.
    //
    // Implementations are responsible for the on-disk layout shape
    // they expect under srcPath. Missing files within srcPath should
    // be tolerated (best-effort), but real I/O errors (e.g. EACCES on
    // a file that does exist) must be propagated.
    //
    // Archive does not write the manifest, the agent-run log, or any
    // post-translation output — those are the central runner's
    // concern.
    Archive(ctx context.Context, srcPath, rawDir string, p SourceParams) error

    // Export performs any post-archive translation the harness wants
    // to emit alongside the raw subtree. archiveDir is the absolute
    // path to the per-archive directory (the parent of rawDir).
    //
    // The canonical post-archive trace format is pi-mono JSONL —
    // this method exists so opencode can run its raw → pi-mono v3
    // translation, while a future PI adapter can elect a no-op (its
    // raw subtree is already pi-mono-shaped at the file level) or
    // emit a normalised pi-mono v3 file under the same name. See
    // "Canonical export format" below.
    //
    // Returning nil with no file written is permitted: it means
    // "no translation produced; consume raw/ directly". Failure is
    // expected to be non-fatal at the call site (today: piexport
    // errors are logged and discarded, see cleanup.go:835-837).
    Export(ctx context.Context, archiveDir string, p SourceParams) error

    // Version returns the harness binary's reported version string
    // (e.g. opencode's "1.1.30"). Failure (binary missing, non-zero
    // exit, parse error) returns ("", nil) — this is captured into
    // the manifest as an empty HarnessVersion and is not a hard
    // failure. A non-nil error is reserved for genuine adapter bugs
    // (e.g. unimplementable on this platform).
    Version(ctx context.Context) (string, error)
}
```

### Why these four methods (and not three or five)

- **`SourcePath` is split out from `Archive`** because two callers want
  the source path without performing the copy: the existing
  `archive.go:resolveStorageRoot` is consulted for both the copy
  destination and (today implicitly) for diagnostic logging on copy
  failure. Splitting it lets the central runner decide what to do
  when the path does not exist (today: still produce a manifest with
  an empty `raw/` — see `archive.go:182-187`) without making the
  adapter responsible for that policy.
- **`Archive` and `Export` are split** because some harnesses
  (PI is the leading candidate — see next section) may want to
  archive but skip translation, and some hypothetical future
  read-only adapter may want to translate but not copy (e.g. an
  adapter that points at an existing archive). The split costs
  nothing and the join is trivial at the call site.
- **`Version` lives on the adapter rather than as a free function**
  because today's `archive.HarnessVersion()` is hard-coded to the
  string `"opencode"`. Pushing it to the adapter is the smallest
  possible change that lets a PI adapter return `pi --version` and
  a future Codex adapter return its own version string.
- **No `Cleanup` / `Validate` / `RawSchemaVersion` method.** A
  schema-version field would be useful for forward-compatibility (so
  that piexport can refuse to translate a raw subtree it does not
  understand), but today opencode does not expose a schema version
  on its storage tree and the manifest's `archiveVersion` /
  `piMonoVersion` constants (`archive.go:46-52`) cover the
  prism-side compatibility need. Defer until a second harness lands
  and the question becomes concrete.

### Justifications for departure from the AC's four-method baseline

The AC's wording allows "a justified alternative". The interface above
matches the AC's four-method count but with two differences from the
AC's literal signatures:

1. **`SourcePath(sessionName, instanceID string) (string, error)`** in
   the AC vs **`SourcePath(p SourceParams) (string, error)`** here.
   Reason: the opencode adapter needs `IsolationMode` and
   `HarnessSessionID` to resolve its source path (see
   `archive.go:265-273` for the isolation-mode switch and
   `archive.go:294-298` for the harness-session-ID lookup). Promoting
   to a struct future-proofs the adapter against further field
   additions and avoids a four-positional-arg signature.
2. **`Archive(srcPath, archivePath string) error`** in the AC vs
   `Archive(ctx, srcPath, rawDir string, p SourceParams) error` here.
   Reasons: a `context.Context` is added so a cancelled cleanup can
   stop a long-running copy (today there is no cancellation path
   inside `copySessionFiles`); the destination parameter is renamed
   `rawDir` to make explicit that adapters write into the `raw/`
   subdirectory, not the archive root (the central runner reserves
   `manifest.json`, `agent-run.log`, and `session.jsonl` as
   non-adapter-writable files); and `SourceParams` is threaded
   through so adapters that need session metadata for log lines or
   for harness-specific filename munging do not have to reconstruct it.
3. **`Export(archivePath) error`** in the AC vs
   `Export(ctx, archiveDir, p SourceParams) error` here. Same
   ctx + params justification as `Archive`.

## Canonical export format: pi-mono universal vs per-harness export

The most interesting design question this review surfaces. Two candidate
answers:

### Option A — Pi-mono v3 stays the universal canonical export format

Rationale:

- pi-mono is **literally pi's own format** — the name comes from
  `https://github.com/badlogic/pi-mono`, the upstream project that
  ships the `pi-coding-agent` binary RFC #606 adopts. Per RFC #606's
  "Pi architecture summary" → "Session persistence", PI's on-disk
  shape is "JSONL files with a tree structure (parent/child IDs for
  branching)" — which is exactly the pi-mono v3 entry stream emitted
  by `piexport` today (`piMessageEntry.ParentID *string` at
  `piexport.go:184`, custom-entry support at `:223-227`).
- Therefore a PI adapter's `Export` can plausibly be much closer to
  identity than opencode's: for opencode, `Export` translates
  opencode's per-record JSON files into a single pi-mono v3 JSONL;
  for PI, `Export` may be a normalisation pass over PI's already-
  JSONL session files (e.g. canonicalising timestamps, stripping
  PI-internal fields prism does not consume) or even a no-op.
- A single canonical post-archive format means downstream tooling —
  `prism convert`, anything reading `session.jsonl`, future
  cross-harness retrospective tools — does not need to fork on
  harness identity. The manifest already records `harness` so a
  reader that *wants* harness-specific behaviour can opt in, but
  readers that just want "the conversation transcript" get one
  format.
- pi-mono's `customType` field (`piexport.go:223-227`,
  `:686, :707`) is the natural escape hatch for harness-specific
  records that do not map cleanly to pi-mono's built-in
  message/content shapes — opencode already uses
  `opencode.system` and `opencode.snapshot.start`. PI can use
  `pi.<thing>` for its own extensions without forking the format.

Recommendation: **Option A is the recommended default.** The
combination of (a) pi-mono being PI's own native shape, (b) the
existing `customType` extension point absorbing per-harness oddities,
and (c) the downstream simplification of one canonical reader format
makes this the small-step choice.

### Option B — Each harness exports to its own format under a per-harness filename

For example: `session.opencode.jsonl` from opencode adapter (current
pi-mono v3 contents); `session.pi.jsonl` from PI adapter (PI's native
JSONL, pass-through); future `session.codex.jsonl` for Codex.

Rationale (against):

- Doubles the number of formats downstream consumers must parse.
- The `customType` mechanism in pi-mono already handles the cases
  where a harness needs to emit something that does not fit the
  base shape — there is no current evidence a per-harness format
  buys richer expressiveness than `customType`-extended pi-mono
  does.
- Loses the benefit of pi-mono being literally PI's native shape:
  for the harness most likely to be the PI adapter's use case,
  Option B would mean *not* using pi-mono even though pi-mono is
  pi's own format.

Where Option B *would* become attractive: if a future harness
emerges whose on-disk shape is sufficiently alien that pi-mono +
`customType` cannot express it without losing semantically
significant structure. That is a hypothetical the implementation
issue does not need to pre-solve.

### Recommendation

Adopt **Option A**: pi-mono v3 remains the canonical post-archive
trace format. The adapter's `Export` method is **what produces
that file**, with each harness free to choose how (translation,
pass-through, normalisation, no-op + caller falls back to raw).

[uncertain — whether PI's actual on-disk JSONL shape is field-for-
field compatible with pi-mono v3, or whether even a "pass-through"
PI adapter would still need a canonicalisation step (e.g. for
timestamp formats, parent-ID conventions, custom-entry encoding).
This requires examining the pi-mono upstream source (referenced
from RFC #606 but not vendored in this repo) and prototyping
against an actual PI session, neither of which is in scope for
this proposal.]

## Call sites that consume the adapter

The adapter would be selected by harness name and consumed by the
central cleanup-time archive runner. The full call chain:

| Site | File:line | Today's role | Adapter-consumer role |
|---|---|---|---|
| `runSessionArchive` build of `archive.Params` | `cmd/cleanup.go:783-822` | Constructs the opencode-shaped `archive.Params` directly. | Selects the `ArchiveAdapter` for `sess.Harness`; the central runner threads `SourceParams` to it. |
| Harness-version capture | `cmd/cleanup.go:794` (`archive.HarnessVersion()`) → `internal/archive/version.go:28-34` | Hard-codes `exec.Command("opencode", "--version")`. | Becomes `adapter.Version(ctx)`. The free-function shim can be retained as a thin wrapper for backwards compatibility during migration. |
| Source-path resolution | `internal/archive/archive.go:236-240` (`resolveStorageRoot`) | Switches on `IsolationMode` and assumes opencode storage layout. | Becomes `adapter.SourcePath(p)`. The isolation-mode switch moves *inside* the opencode adapter (where it semantically belongs); other adapters can be isolation-mode-agnostic. |
| Subtree copy | `internal/archive/archive.go:182-187` (`copySessionFiles` invocation) → `:287-388` | Hard-codes opencode's `session/`, `message/`, `part/`, `tool-output/` layout. | Becomes `adapter.Archive(ctx, srcPath, rawDir, p)`. The 100-line `copySessionFiles` body moves into the opencode adapter package. |
| Post-archive translation | `cmd/cleanup.go:835-837` (`piexport.Translate(archivePath)`) → `internal/piexport/piexport.go:43-83` | Hard-codes opencode raw → pi-mono v3 translation. | Becomes `adapter.Export(ctx, archiveDir, p)`. The opencode adapter's `Export` wraps `piexport.Translate`; a future PI adapter's `Export` does its own thing. |
| Archive-path persistence | `cmd/cleanup.go:829` (`d.UpdateSessionArchivePath`) → `internal/db/db.go:1889-1904` (approx — `UpdateSessionArchivePath`) | Writes `sessions.archive_path`. | **Unchanged.** The post-archive contract — "the archive directory exists at this path" — is harness-neutral. |
| Archive-path read-back | `cmd/archive.go:83-117` (`runArchive` and helpers) | Reads `sessions.archive_path` and prints it. | **Unchanged.** Harness-neutral consumer of `archive_path`. |
| `LatestSessionForDir` (opencode SQLite query) | `internal/opencode/session.go:39-57` | Resume-helper for `opencode -s <id>`; not currently called from any production code path (only its own tests at `internal/opencode/session_test.go`). | **Out of scope for the immediate refactor.** Decide in the implementation issue whether to delete (preferred — dead code), retain as opencode-package legacy, or move behind a `HarnessAdapter.LatestSessionForDir(dir)` extension method (only if a real caller resurfaces). |
| `prism convert` glossary entry | inventory `architecture-inventory.md:2025` describes `cmd/convert.go` as "opencode raw archive → pi-mono trace via `internal/piexport`". | The inventory text is wrong — `cmd/convert.go` is the bare+worktree converter and does not import `piexport`. | Documentation-only fix, **out of scope here**, but worth flagging to the inventory owner. |

The non-tabular sites that survive the migration unchanged:

- The archive directory layout (`<archiveRoot>/<repo>/<startedAtISO>_<instanceID>/`)
  and atomicity scaffolding (`archive.go:120-212`).
- The manifest schema (`archive.go:510-528`) — already harness-neutral
  in field naming.
- The agent-run.log copy (`archive.go:189-199`) — applies to any
  bwrap/sandbox-exec session regardless of harness, since it captures
  the bwrap-side stdout/stderr, not harness-specific data.
- The `archive.ErrAlreadyExists` idempotency contract (`archive.go:38-42,
  :147-149`) and the four cleanup call sites that handle it
  (`cmd/cleanup.go:247, :526, :586, :658`).
- The per-archive `manifest.json`'s `harness`, `harnessSessionId`,
  `harnessVersion` fields (`archive.go:518-520, :546-548`) — already
  multi-harness in name, just opencode-derived in current values.

## Recommended migration order

The migration is best done in three implementation steps, each
self-contained and independently mergeable. None of them changes
runtime behaviour for opencode sessions.

### Step 1 — Carve out the adapter interface and the opencode adapter

Create `internal/harness/<archive-package-name>/` (working name:
`internal/harnessarchive`; see "Open questions" §3 for the naming
discussion).

- Define the `ArchiveAdapter` interface and `SourceParams` struct as
  proposed above.
- Move `archive.go:resolveStorageRoot`, `archive.go:containerNameForSession`,
  `archive.go:copySessionFiles`, `archive.go:findSessionFile`,
  `archive.go:copyDirFlat`, `archive.go:listDirEntries`,
  `archive.go:toolOutputIDsFromPart` into a new
  `internal/harness/opencode/archive.go` (or a sub-package thereof).
  The opencode adapter implements `ArchiveAdapter.SourcePath` and
  `ArchiveAdapter.Archive` using these functions.
- Move `internal/archive/version.go:HarnessVersion` body into the
  opencode adapter as `Version`. Keep the package-level
  `archive.HarnessVersion()` free function as a thin shim that calls
  the opencode adapter, to avoid touching `cmd/cleanup.go:794` in
  this step.
- Move `internal/piexport.Translate` invocation into the opencode
  adapter's `Export`. The piexport package itself stays put — it is
  a valid implementation detail of the opencode adapter, and a
  future PI adapter's `Export` would not consume it.
- The central `archive.Run` body shrinks to: resolve `archiveRoot`
  (still XDG-derived), call `adapter.SourcePath`, create temp dir,
  call `adapter.Archive`, copy agent-run.log, write manifest, atomic
  rename, call `adapter.Export`.

This step is contained to the archive layer plus a minimal adapter
for opencode. No cleanup-side changes.

### Step 2 — Wire the adapter through cleanup and remove the shims

- `cmd/cleanup.go:runSessionArchive` looks up the adapter by
  `sess.Harness` (today: always `"opencode"`) and threads it
  explicitly into `archive.Run`. The shim `archive.HarnessVersion()`
  is removed in favour of `adapter.Version(ctx)`.
- The hard-coded `Harness: sess.Harness` field on `archive.Params`
  remains — it is what gets persisted to the manifest — but the
  adapter selection no longer depends on `sess.Harness == "opencode"`
  having any source-coupling consequence.
- This step exposes the "what does the archive runner do when no
  adapter is registered for a harness?" policy question (today:
  N/A because there is only one harness). Recommend: log + skip,
  same as today's "no instance_id → skip archive" path
  (`cleanup.go:756-758`).

### Step 3 — Land the second adapter (PI)

Out of scope for B.6's deliverable, but the obvious next step.
Prerequisites:

- A PI sidecar exists and writes a `sessions.harness = "pi"` row
  with a populated `harness_session_id`. (Per RFC #606 Phase 2 ACs.)
- The PI on-disk shape is known well enough to write
  `piAdapter.Archive` and `piAdapter.Export`. RFC #606 Phase 2's
  "`prism checkin` for pi sessions reads from pi's JSONL session
  files" AC implies this knowledge will exist by then.

[uncertain — exact PI on-disk path layout (the directory under
`~/.pi/agent/` or wherever PI persists session files; the per-
session filename convention; whether there is one JSONL file per
session or one per branch). This needs examining the upstream
pi-mono source or a running PI session and is the central
unknown blocking the PI adapter's `Archive` implementation. The
adapter interface itself is robust to this unknown — it is
exactly the kind of layout-coupling the interface exists to
encapsulate.]

## Open questions and `[uncertain]` flags

Consolidated from the per-section flags above plus design choices
the implementation issue will need to make:

1. **Where does the adapter package live?** Three plausible homes:
   - `internal/harness/archive` — alongside the `Harness` interface
     (`internal/harness/harness.go`); creates a new sub-package.
   - `internal/archive/adapter` — alongside the central archive
     runner; awkward because the package name `archive` is already
     taken at the parent level.
   - `internal/harnessarchive` — top-level peer package.
   - The opencode adapter itself either co-locates with
     `internal/harness/opencode/adapter.go` (the existing harness
     adapter) — adding `internal/harness/opencode/archive.go`
     alongside — or lives at `internal/harness/opencode/archive/`.
     The trade-off: co-located keeps everything-opencode together,
     but enlarges `internal/harness/opencode/`'s surface; a
     sub-package is cleaner but more directories.
2. **Should `archive.Params` itself shrink to non-harness-specific
   fields and grow a `HarnessParams` sidecar struct?** Currently
   `archive.Params` carries `HarnessSessionID`, `HarnessVersion`,
   and (implicitly via `IsolationMode`) the host-side path
   discriminator. After Step 2 these are still needed *somewhere*,
   but the question is whether they live on the central `Params`
   (and the adapter pulls them out) or on a separate
   `HarnessParams` (and the central runner forwards it to the
   adapter). The interface above takes the former approach via
   `SourceParams` because it is simpler; revisit if a second
   adapter strains the shape.
3. **`Version` synchronisation cost.** Today
   `archive.HarnessVersion()` shells out to `opencode --version`
   on every cleanup. For a PI adapter this means `pi --version`
   per cleanup, which is fine but worth noting. Caching across a
   single `prism cleanup` invocation that processes many sessions
   could be added if it matters.
4. **`LatestSessionForDir` decision** — delete vs retain vs adapter-
   ise. Recommend: delete. It is currently uncalled by any
   production code (only its own tests reference it), so deleting
   is risk-free pending a real caller. If a caller ever re-emerges,
   add it as an `ArchiveAdapter` extension (`OpenSessionResolver`
   sub-interface or similar) at that point.
5. **PI on-disk shape** — see Step 3 above. The single biggest
   unknown blocking the PI adapter's `Archive` implementation;
   does not block the adapter *interface* shape.
6. **Whether pi-mono v3 needs a v4 to absorb a "harness" field on
   the session header.** Today the archive's `manifest.json`
   carries `harness` but `session.jsonl`'s `piSessionHeader`
   (`piexport.go:172-178`) does not. A reader of `session.jsonl`
   alone cannot tell which harness produced it without consulting
   the sibling `manifest.json`. Whether to widen the pi-mono
   schema, add a custom entry at the start of the JSONL, or rely on
   manifest co-location is a downstream-tooling question; defer
   until a multi-harness reader exists.
7. **Inventory error in §`prism convert`** — the inventory glossary
   at `architecture-inventory.md:2025` says `cmd/convert.go` does
   the opencode-raw → pi-mono translation via `internal/piexport`;
   in fact `cmd/convert.go` is unrelated (bare+worktree converter)
   and `piexport.Translate` is invoked only from
   `cmd/cleanup.go:835`. Worth a one-line inventory fix in a
   future docs PR; out of scope here.

## What this proposal deliberately does not do

- It does **not** change opencode-session behaviour. After the
  proposed migration, an opencode session's archive copy and
  pi-mono export must be byte-for-byte identical to today's.
- It does **not** propose changes to the `Harness` interface
  (B.2 / B.3 / B.4 territory).
- It does **not** propose changes to the runtime event payload
  schema (B.5 territory).
- It does **not** propose any change to the archive directory
  layout, the `manifest.json` schema, the `session.jsonl` v3
  schema, the `archive_path` DB column, or the `prism archive`
  CLI surface.
- It does **not** ship the PI adapter — only the interface and the
  migration order such that a PI adapter can land additively in a
  later issue once PI's on-disk shape is known.

## Related

- Inventory: [`../architecture-inventory.md`](../architecture-inventory.md)
  §7.15-§7.18.
- B.1: [`B1-harness-transport-and-lifecycle-assumptions.md`](B1-harness-transport-and-lifecycle-assumptions.md)
  §7.15-§7.18 entries.
- Design: [`000-design-narrow-review-series.md`](000-design-narrow-review-series.md)
  Track B row B.6.
- Issue: #1084. Parent design: #1072.
- RFCs: #691 (multi-harness support), #606 (PI coding agent support;
  see "Pi architecture summary" → "Session persistence" for PI's
  JSONL-with-parent/child-IDs shape, and "Compatibility summary" for
  the "no SQLite" callout).
- Sibling Track B issues: #1080 (B.2), #1081 (B.3), #1082 (B.4),
  #1083 (B.5).
