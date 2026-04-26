# D.1 — Function-Size Hot-Spots: Decomposition Proposal

## 1. Context recap

This document is the deliverable for [issue #1088](https://github.com/prismatic-koi/nixos-config/issues/1088),
part of the narrow architecture-review series designed in [#1072](https://github.com/prismatic-koi/nixos-config/issues/1072).

The inventory (§5.2 of `docs/architecture-inventory.md`) identifies four functions ≥ 100 LoC
in the top tier:

| LoC  | Function                          | File:Line                                    |
|------|-----------------------------------|----------------------------------------------|
| 1054 | `(*Sidecar).hostAPIHandler`       | `internal/sidecar/sidecar.go:2628`           |
| 683  | `Open`                            | `internal/db/db.go:288`                      |
| 555  | `(*Manager).buildRunArgs`         | `internal/container/container.go:988`        |
| 488  | `(*bwrapIsolator).BuildArgs`      | `internal/container/bwrap.go:152`            |

Each is a refactoring candidate — not a correctness issue. The goal is to propose
decomposition strategies that break the functions along their natural seams **without
changing observable behaviour**. No code changes are made in this document.

Interaction notes from the design doc (#1072):
- `BuildArgs` may shrink as part of **A.2**'s shared-helper extraction (bwrap/podman
  duplication audit).
- `hostAPIHandler` may shrink as part of **B.3**'s lifecycle changes (sidecar lifecycle
  for stdio harnesses).
- The synthesis pass **E.1** will decide whether to action D.1 standalone or fold it
  into other tracks.

---

## 2. `(*Sidecar).hostAPIHandler` — `internal/sidecar/sidecar.go:2628` (~1054 LoC)

### Current shape

`hostAPIHandler` constructs and returns an `http.Handler` (a `*http.ServeMux`).
The entire body is one long factory function. At the top it defines four closure
helpers (`writeJSON`, `writeError`, `requirePost`, `requireGet`,
`requireCoordinator`, `prismBinary`); then it registers eleven route handlers
inline via `mux.HandleFunc`. Each handler is a 30–150 LoC anonymous function
that validates its inputs, does permission checks, executes a `prism` subprocess
or DB operation, and writes a response.

The routes (seams by route) are:

| Route               | Approx start line |
|---------------------|-------------------|
| `GET /list-sessions`    | 2699 |
| `GET /checkin`          | 2743 |
| `GET /logs`             | 2904 |
| `POST /prompt`          | 3011 |
| `POST /spawn`           | 3107 |
| `POST /review`          | 3283 |
| `POST /cleanup`         | 3476 |
| `POST /switch`          | 3533 |
| `POST /merge`           | 3580 |
| `GET /merges`           | 3628 |
| `POST /merges/cancel`   | 3663 |

### Natural seams

**Seam 1 — Helper closure extraction (lines 2631–2694).**
Five helper closures are defined at the top of `hostAPIHandler`:
`writeJSON` (2632), `writeError` (2644), `requirePost` (2650), `requireGet` (2660),
`requireCoordinator` (2672), `prismBinary` (2685).
All five are purely functional utilities with no side-effects on the `Sidecar`
receiver beyond reading `s.cfg`. They could be extracted as package-level or
method helpers so that the route handlers call them by name instead of closing
over a local variable.

**Seam 2 — Route handler grouping by subsystem (lines 2699–3709).**
The eleven routes fall naturally into four subsystem groups:
- **Session inspection** (`/list-sessions`, `/checkin`, `/logs`) — read-only DB
  or filesystem queries that do not invoke subprocesses.
- **Lifecycle actions** (`/prompt`, `/spawn`, `/cleanup`, `/switch`) — coordinator-
  only actions that delegate to `prism` subprocesses.
- **Review** (`/review`) — the most complex single handler; streams subprocess
  stdout, manages an exec context, writes a pass/fail sentinel.
- **Merge queue** (`/merge`, `/merges`, `/merges/cancel`) — coordinator-only DB
  operations over `pending_merges`.

**Seam 3 — The `/checkin` assistant-turn merge logic (lines 2834–2882).**
Inside the `/checkin` handler, the "no `--types` filter" branch has an elaborate
multi-step procedure: fetch assistant events, collect their `messageId` values,
fetch child tool-call events by message IDs, determine a time window, fetch user
events and filter them, then merge everything into a sorted timeline. This block
(~50 LoC) is entirely self-contained and could be extracted regardless of anything
else.

**Seam 4 — The `/review` subprocess-streaming body (lines 3366–3471).**
The `/review` handler's post-validation body (~105 LoC) handles:
(a) CWD resolution from the DB,
(b) subprocess environment assembly,
(c) `cmd.StdoutPipe` setup and streaming with `bufio.Scanner`,
(d) sentinel writing.
Steps (a)–(d) are independent of the routing machinery and would be testable in
isolation.

**Seam 5 — The `/spawn` log-args duplication (lines 3174–3230).**
`/spawn` builds `args` and then unconditionally builds a parallel `logArgs` slice
by repeating the same conditional logic with `"<omitted>"` substituted for the
prompt. This duplication is a mini-seam: a helper that accepts a flag for whether
to redact would eliminate the repetition.

### Proposed sub-functions

```
// hostAPIHelpers groups the per-handler utility closures as a struct
// so handlers can receive it by value rather than closing over multiple
// separate variables.
type hostAPIHelpers struct {
    writeJSON(w http.ResponseWriter, status int, v any)
    writeError(w http.ResponseWriter, status int, msg string)
    requirePost(w http.ResponseWriter, r *http.Request) bool
    requireGet(w http.ResponseWriter, r *http.Request) bool
    requireCoordinator(w http.ResponseWriter, operation string) bool
    prismBinary() string
}

// registerSessionInspectionRoutes mounts /list-sessions, /checkin, /logs.
func (s *Sidecar) registerSessionInspectionRoutes(mux *http.ServeMux, h hostAPIHelpers)

// registerLifecycleRoutes mounts /prompt, /spawn, /cleanup, /switch.
func (s *Sidecar) registerLifecycleRoutes(mux *http.ServeMux, h hostAPIHelpers)

// registerReviewRoute mounts /review.
func (s *Sidecar) registerReviewRoute(mux *http.ServeMux, h hostAPIHelpers)

// registerMergeQueueRoutes mounts /merge, /merges, /merges/cancel.
func (s *Sidecar) registerMergeQueueRoutes(mux *http.ServeMux, h hostAPIHelpers)

// buildCheckinTimeline assembles the merged event timeline for a /checkin
// response when no explicit types filter is given.
func buildCheckinTimeline(
    db interface{ QueryEvents(...); QueryEventsByMessageIDs(...) },
    session string, limit int,
    beforePtr, afterPtr *string,
) ([]db.Event, error)

// streamReviewSubprocess runs the prism-review subprocess, streams its stdout
// line-by-line to w via http.Flusher, and writes the pass/fail sentinel.
func (s *Sidecar) streamReviewSubprocess(
    ctx context.Context,
    w http.ResponseWriter,
    args []string,
    sessionName string,
)

// buildSpawnArgs returns (execArgs, logArgs) for a /spawn request where
// logArgs has the prompt value redacted.
func buildSpawnArgs(req spawnRequest, ownRepo string) (execArgs []string, logArgs []string)
```

### Independence flag

**Wait for B.3** (sidecar lifecycle for stdio harnesses).

B.3 will determine whether the sidecar becomes the launcher of stdio-harness agent
processes, which would likely add new route handlers to `hostAPIHandler` or
restructure when the mux is constructed. Decomposing the function now and then
re-shaping the decomposition for B.3's output creates churn. The sub-function
boundaries proposed above are stable across harness shapes, but the extraction is
more valuable after B.3 confirms the final route surface.

The `buildCheckinTimeline` extraction is safe to do standalone at any time — it has
no dependency on B.3.

### Observable-behaviour warnings

- **None for the structural splitting** (route grouping, sub-function extraction).
  The HTTP behaviour — method restrictions, permission checks, response shapes — is
  identical if the extracted sub-functions are wired up correctly.
- **`buildSpawnArgs` note**: the current `/spawn` handler logs with
  `log.Printf("sidecar: host-API /spawn: prism %s", strings.Join(logArgs, " "))` at
  line 3230. If the log-arg generation is moved into a helper, the log line format
  must be preserved verbatim or downstream log-parsing would break.
- **`buildCheckinTimeline` note**: the current merge sort uses insertion-sort at
  lines 2875–2879. If the extracted helper uses a different sort (e.g. `slices.SortFunc`)
  the ordering of same-timestamp events would differ — this is an edge case but is
  observable if callers depend on the tie-breaking order.

---

## 3. `Open` — `internal/db/db.go:288` (~683 LoC)

### Current shape

`Open` is the single constructor for `*DB`. It:
1. Creates parent directories and opens the SQLite connection (288–297).
2. Applies three PRAGMAs: WAL mode, busy-timeout, foreign-keys (298–317).
3. Runs the declarative schema block (319–323).
4. Seeds `schema_version=11` on a fresh database (325–342).
5. Reads the current version and runs a chain of `if version == N { ... }` migration
   blocks, one per version, covering v1→v2 through v19→v20 (344–966).
6. Returns `&DB{conn: conn, path: path}` (969).

Each migration block is self-contained: it applies one or more SQL statements, then
bumps `version` in memory and in the DB. The chain currently covers 19 migration
steps.

### Natural seams

**Seam 1 — Connection setup vs schema management (lines 288–323).**
The first ~35 lines of `Open` (directory creation, `sql.Open`, the three PRAGMAs,
and the schema block) form a complete connection-setup phase. Everything after line
323 is migration management. These two phases have no data dependency on each other
except through `conn` — the schema block must run before migrations read
`schema_version`, but the call is a single `conn.Exec(schema)` and the boundary is
crisp.

**Seam 2 — Fresh-database seeding vs migration chain (lines 325–342 vs 344–966).**
Seeding `schema_version=11` for fresh databases is a three-line block (332–341)
that precedes the migration chain. It is logically distinct: it handles the "empty
DB" case so the migration chain always begins from a known numeric version. These
can be separated into a seed helper and a migrate helper.

**Seam 3 — Individual migration functions (lines 350–966).**
Each `if version == N` block is structurally identical: it runs one or more SQL
statements via `conn.Exec`, closes `conn` and returns an error on failure, and
bumps `version`. They are fully independent of each other. The most complex —
v8→v9 (lines 464–549) — uses an inner `func() error` closure to wrap a
`PRAGMA foreign_keys = OFF` + transaction + rename dance; this closure is already
self-contained. Every migration could be a function with the signature:

```
func migrateVNtoVN1(conn *sql.DB) error
```

**Seam 4 — The v8→v9 table-recreation pattern (lines 464–549).**
Migration v8→v9 is already the most complex individual step (85 LoC). It contains
its own inline documentation of the SQLite rename-and-recreate pattern. Its inner
closure is a distinct extraction opportunity regardless of whether all other
migrations are extracted.

### Proposed sub-functions

```
// openAndConfigure opens the SQLite connection and applies the baseline PRAGMAs
// and schema block. Returns the configured *sql.DB.
func openAndConfigure(path string) (*sql.DB, error)

// seedSchemaVersionIfEmpty inserts schema_version=11 when the table is empty.
// This is the fresh-database case; on existing DBs it is a no-op.
func seedSchemaVersionIfEmpty(conn *sql.DB) error

// runMigrations applies all pending migrations from the current schema_version
// up to the maximum known version. Returns the final version on success.
func runMigrations(conn *sql.DB) (int, error)

// Individual migration functions — one per step:
func migrateV1toV2(conn *sql.DB) error   // add agent_name, model_id
func migrateV2toV3(conn *sql.DB) error   // add root_agent_name, root_model_id
func migrateV3toV4(conn *sql.DB) error   // add opencode_port
func migrateV4toV5(conn *sql.DB) error   // add host_mode
func migrateV5toV6(conn *sql.DB) error   // add instance_id, to_instance_id
func migrateV6toV7(conn *sql.DB) error   // add failed_at to bus_messages
func migrateV7toV8(conn *sql.DB) error   // add harness columns
func migrateV8toV9(conn *sql.DB) error   // add session_groups + FK (rename-recreate)
func migrateV9toV10(conn *sql.DB) error  // add isolation_mode
func migrateV10toV11(conn *sql.DB) error // drop legacy opencode_port/opencode_sid
func migrateV11toV12(conn *sql.DB) error // add active coordinator unique index
func migrateV12toV13(conn *sql.DB) error // one-shot cleanup of malformed review sessions
func migrateV13toV14(conn *sql.DB) error // backfill last_seen
func migrateV14toV15(conn *sql.DB) error // rename opencode_sid → harness_session_id
func migrateV15toV16(conn *sql.DB) error // introduce sessions table
func migrateV16toV17(conn *sql.DB) error // no-op bridge
func migrateV17toV18(conn *sql.DB) error // fix negative started_at
func migrateV18toV19(conn *sql.DB) error // introduce pending_merges table
func migrateV19toV20(conn *sql.DB) error // add idx_pending_merges_status_session

// Open is then a thin orchestrator:
//   conn, err := openAndConfigure(path)
//   seedSchemaVersionIfEmpty(conn)
//   runMigrations(conn)   ← dispatches to the individual migrate* functions
//   return &DB{conn, path}, nil
```

`runMigrations` could be driven by a registration table:

```
type migrationFn func(*sql.DB) error

var migrations = []migrationFn{
    nil,           // index 0 — unused
    migrateV1toV2,
    migrateV2toV3,
    // … up to migrateV19toV20
}
```

This makes adding a new migration a two-line change: write the function and append
it to the slice.

### Independence flag

**Safe to do standalone.**

`Open` is a self-contained constructor with no dependency on Track A, B, or C
work. The migration chain grows monotonically as new features land; extracting
individual migration functions now reduces the cost of every future migration
addition. There is no risk of conflict with other tracks — DB schema changes
(e.g. from B.3 or A.2) simply add new entries to the slice.

The one practical sequencing note: if a new migration (v20→v21) lands before
this decomposition, it should be written as a standalone function from the start,
not as another `if version == N` block in the monolith.

### Observable-behaviour warnings

- **Error-message format.** Every migration block currently wraps errors as
  `fmt.Errorf("db: migration vN→vN+1: %w", err)`. If the extracted functions
  return bare errors and the wrapper is applied by `runMigrations`, the error
  string seen by callers changes. Callers that log or test against the exact
  error text would break. The wrapper must remain in the individual functions or
  be applied at the same call depth as today.
- **`conn.Close()` on error.** The current code calls `conn.Close()` inside each
  migration block before returning the error. If error-handling moves to
  `runMigrations`, it must replicate this close — or `Open` must `defer conn.Close()`
  and reassign on success. Either approach changes when the connection is closed
  on error paths; this is not visible to normal callers (they get `nil, err`)
  but is relevant to tests that check for leaked file descriptors.
- **Version variable mutation.** Each `if version == N` block ends with
  `version = N+1`. When extracted, the `runMigrations` loop must maintain the
  version counter correctly. The dispatch-table approach above makes this implicit
  (loop index = version).
- **`v8→v9` PRAGMA ordering.** Migration v8→v9 disables `foreign_keys` globally
  with `PRAGMA foreign_keys = OFF` at line 483, runs in a transaction, then
  re-enables at line 541. Any extracted wrapper for v8→v9 must preserve this
  ordering; wrapping it carelessly in a helper that re-enables FK at the wrong
  point would change constraint-checking behaviour for the duration of the
  migration.

---

## 4. `(*Manager).buildRunArgs` — `internal/container/container.go:988` (~555 LoC)

### Current shape

`buildRunArgs` builds the `podman run` argv for a container session. It is a
pure append-accumulation function: it resolves paths, conditionally appends
`--volume`, `--env`, `--mount`, `--memory`, etc. flags to a `[]string`, and
returns it. There are no conditionals that change the overall control flow —
just a long sequence of "if path exists, add flag".

The function has a clear top-to-bottom structure:

1. **Baseline flags** (1089–1097): `run`, `--detach`, `--tty`, `--name`.
2. **Core mounts** (1107–1134): network, port, worktree, opencode state,
   opencode cache, bun cache, Claude dir, Nix cache.
3. **Conditional overlays** (1136–1200): auth.json overlay, MCP auth, AWS
   files (config, credentials, SSO, CLI), kube config, clipboard.
4. **Darwin credentials file** (1202–1209): `claudeCredentialsFilePath()`.
5. **Static env vars and working directory** (1211–1263): Nix daemon socket,
   `NIX_CONFIG`, `TERM`, prism context vars (`PRISM_SPAWN_PATH`,
   `PRISM_BARE_ROOT`, `PRISM_SESSION_NAME`), `--workdir`.
6. **Host-API transport** (1265–1294): conditional TCP vs Unix socket env var
   and volume mount.
7. **Opencode config allowlist** (1296–1355): iterates over a `configAllowlist`
   slice and adds `--mount type=bind` or `--volume` for each entry.
8. **SSH keys** (1357–1412): access key, signing key pair + allowed_signers,
   known_hosts, generated SSH config, generated gitconfig.
9. **Git mounts** (1417–1450): bare repo, worktree private state, gitdir
   overlay, wtGitdir overlay — conditional on `BareRoot` and `WorktreeGitDir`.
10. **Credentials env vars** (1452–1455): `credentialEnvVars()` call.
11. **Profile agent env vars** (1457–1481): sorted `cfg.AgentEnvVars` with
    `sandboxMountedByDefault` suppression.
12. **Opencode.json** (1483–1490): mount generated config file.
13. **Resource caps** (1492–1504): `--memory`, `--memory-swap`, `--pids-limit`.
14. **Image and command** (1506–1526): `Image`, `opencode --port ... --hostname`.
15. **Runtime env vars** (1518–1520): `cfg.RuntimeEnv` loop.
16. **Agent role and initial prompt** (1528–1539): `--agent`, `--prompt`.

### Natural seams

**Seam 1 — Baseline container flags (lines 1089–1105).**
The `podman run --detach --tty --name` prefix plus the optional
`--label prism.instance-id` block. Self-contained; nothing below depends on
it except that `args` must start with these elements.

**Seam 2 — Core volume mounts (lines 1107–1134).**
The unconditional mounts: network, port, worktree, opencode state, opencode
cache, bun cache, Claude dir, Nix cache. These are always present regardless
of any config field.

**Seam 3 — Conditional overlay mounts (lines 1136–1209).**
auth.json, MCP auth, AWS (four separate conditional checks), kube config,
clipboard cache, Darwin credentials. All follow the same pattern: `if path
exists → append volume flag`. This block is the clearest extraction candidate
— it groups all the "mount this if it exists on the host" logic.

**Seam 4 — Opencode config allowlist mounts (lines 1296–1355).**
The `configAllowlist` slice iteration already has a named variable and a
comment explaining the allowlist rationale. It could be extracted as
`buildOpencodeConfigMounts(cfg, isReviewContainer) []string` with minimal
refactoring.

**Seam 5 — SSH and git mounts (lines 1357–1450).**
SSH key resolution + git bare-repo mounts form a coherent group that deals
exclusively with the version-control toolchain. Note that `buildRunArgs` calls
`m.writeGitconfig` (not shown here, but noted in the function comments at
line 1379) immediately before `buildRunArgs` in `Create` — the two phases
are already conceptually adjacent.

### Proposed sub-functions

```
// appendBaselineContainerFlags appends the initial podman run flags
// (--detach, --tty, --name, optional instance-id label) to args.
func appendBaselineContainerFlags(args []string, name, instanceID string) []string

// appendCoreVolumeMounts appends the unconditional volume mounts
// (network, port, worktree, opencode state, cache dirs, claude, nix).
func appendCoreVolumeMounts(args []string, cfg Config, portBinding, worktreeMount, opencodeStateMount string) []string

// appendConditionalOverlayMounts appends mounts that are conditional on
// host-path existence: auth.json, mcp-auth, AWS files, kube config, clipboard,
// darwin credentials.
func appendConditionalOverlayMounts(args []string, cfg Config, m *Manager) []string

// appendHostAPITransport appends the host-API socket/TCP env var and optional
// socket directory volume mount.
func appendHostAPITransport(args []string, cfg Config) []string

// appendOpencodeConfigMounts appends the opencode config allowlist mounts
// (AGENTS.md, plugins, skills, command, tui.json, .gitignore, agents/).
func appendOpencodeConfigMounts(args []string, cfg Config, opencodeConfigDir string) []string

// appendSSHAndGitMounts appends SSH key mounts, generated SSH config and
// gitconfig, and the bare-repo / worktree-private-state mounts.
func appendSSHAndGitMounts(args []string, cfg Config, m *Manager) []string

// appendCredentialsAndEnvVars appends credential env vars, profile agent env
// vars, runtime env vars, and resource caps.
func appendCredentialsAndEnvVars(args []string, cfg Config, m *Manager) []string

// appendImageAndCommand appends the image name and the opencode command with
// --port, --hostname, --agent, and --prompt flags.
func appendImageAndCommand(args []string, cfg Config) []string
```

`buildRunArgs` becomes an orchestrator that calls each helper in sequence:

```
func (m *Manager) buildRunArgs() []string {
    args := []string{}
    args = appendBaselineContainerFlags(args, m.name, cfg.InstanceID)
    args = appendCoreVolumeMounts(args, cfg, ...)
    args = appendConditionalOverlayMounts(args, cfg, m)
    args = appendHostAPITransport(args, cfg)
    args = appendOpencodeConfigMounts(args, cfg, opencodeConfigDir)
    args = appendSSHAndGitMounts(args, cfg, m)
    args = appendCredentialsAndEnvVars(args, cfg, m)
    args = appendImageAndCommand(args, cfg)
    return args
}
```

### Independence flag

**Wait for A.2** (bwrap/sandbox-exec/podman duplication audit).

A.2 reads `bwrap.go` and `container.go` side-by-side to identify what is
incidentally the same vs necessarily different. Section 5 (`BuildArgs` and
`buildRunArgs`) below shows that the two functions share several structural
groups — conditional overlays, opencode config allowlist, SSH keys, credentials
env vars, resource caps, agent/prompt suffix — with only the flag syntax
differing (`--volume` vs `--bind`, `--env` vs `--setenv`).

Extracting `buildRunArgs` helpers independently of A.2's findings risks
duplicating work: if A.2 concludes that `appendConditionalOverlayMounts` and
`appendSSHAndGitMounts` should be shared helpers (with an interface parameter
for flag generation), decomposing them into podman-only functions first
and then restructuring them again is avoidable churn.

`appendBaselineContainerFlags` is safe to extract standalone — it is
entirely podman-specific (`podman run --detach --tty --name`) with no bwrap
counterpart.

### Observable-behaviour warnings

- **No functional changes** are introduced by extracting append-only helpers.
  The only risk is argument-order bugs — the caller must pass values in the
  same order the monolith accumulated them, because some later mounts depend
  on earlier mounts being present (e.g. the opencode state directory must exist
  as a mount before the auth.json overlay can shadow a path inside it).
- **`appendConditionalOverlayMounts` / auth.json overlay**: the current code
  appends the auth.json overlay only if `os.Stat` succeeds (line 1149). The
  extracted helper must preserve this conditional — unconditionally mounting
  a non-existent file would cause `podman run` to fail.
- **Log line in `buildRunArgs` callers**: `buildRunArgs` itself has no log
  calls. Its caller (`Create`) logs the redacted argv at line ~837:
  `log.Printf("container: creating container: %s", strings.Join(redactArgs(args), " "))`.
  The extracted helpers do not affect this log line.

---

## 5. `(*bwrapIsolator).BuildArgs` — `internal/container/bwrap.go:152` (~488 LoC)

### Current shape

`BuildArgs` builds the `bwrap` argv for a sandboxed session. Like `buildRunArgs`,
it is a pure append-accumulation function. The bwrap-specific structure maps
closely to `buildRunArgs` but with different flag syntax and different security
properties (namespace flags, `--clearenv`, Dst==Src path mounts).

Structural groups:

1. **Baseline namespace flags** (180–188): `--clearenv`, `--unshare-pid`,
   `--unshare-uts`, `--proc /proc`, `--dev /dev`, `--tmpfs /tmp`,
   `--die-with-parent`.
2. **System binary roots** (201–209): unconditional `--ro-bind` for `/nix`,
   `/etc`, `/run/current-system`, `/bin`, `/run/wrappers`.
3. **Security: sensitive /etc subtree shadowing** (211–239): conditional
   `--tmpfs` over `/etc/wireguard` and `/etc/wpa_supplicant`.
4. **Per-user nix profiles** (241–254): conditional `--ro-bind` for
   `~/.nix-profile` and `~/.local/state/nix/profile`.
5. **Worktree** (256–261): `--bind cfg.Worktree cfg.Worktree`.
6. **Bare repo and worktree private git state** (263–279): conditional
   `--bind` for `bareDir` and `WorktreeGitDir`.
7. **Personal data dirs** (281–310): `~/.claude` (unconditional), `~/.mcp-auth`
   (conditional), opencode data dir, Nix daemon socket dir, `~/.cache/nix`.
8. **Config remaps** (312–430): AWS readonly-config, kube agents-config, SSH
   keys (access key, signing key pair + allowed_signers, known_hosts), generated
   SSH config and gitconfig, opencode.json, opencode config allowlist, opencode
   plugin cache, bun transpiler cache.
9. **Additional AWS mounts** (454–471): credentials, SSO cache dir, CLI cache dir.
10. **Clipboard staging dir** (473–480): conditional `--ro-bind`.
11. **Environment variables** (482–576): credentials, profile agent env vars,
    `NIX_CONFIG`, `TERM`, `COLORTERM`, standard sandbox env (`standardSandboxEnvArgs`),
    prism context vars, runtime env vars, host-API env var (and socket dir bind).
12. **Working directory and terminator** (604–638): `--chdir`, `-- opencode
    --port --hostname --agent --prompt`.

### Natural seams

**Seam 1 — Baseline namespace and system roots (lines 180–254).**
Everything before the per-session mounts: namespace isolation flags, system
root ro-binds, /etc security shadowing, nix profiles. These are entirely
host-topology-driven (not config-driven) and could be expressed as a single
`appendBwrapBaselineNamespace(args []string, home string) []string` helper.

**Seam 2 — Per-session data mounts (lines 256–310).**
Worktree, bare repo, personal state dirs (`.claude`, `.mcp-auth`, opencode data,
Nix daemon socket, Nix cache). These are `cfg`-driven Dst==Src binds with no
path remapping.

**Seam 3 — Config and credential remaps (lines 312–480).**
AWS config, kube config, SSH keys, generated configs, opencode allowlist, plugin
caches, additional AWS dirs, clipboard. This group shares substantial structure
with `buildRunArgs`'s conditional overlay section — it is the primary A.2 seam.

**Seam 4 — Environment variable injection (lines 482–601).**
`credentialEnvVars()`, profile env vars, NIX_CONFIG, TERM, COLORTERM,
`standardSandboxEnvArgs()`, prism context vars, runtime env vars, host-API env.
The bwrap translation (`--setenv K V` vs `--env K=V`) is the only syntactic
difference from the podman path's env-var block.

**Seam 5 — Terminator (lines 604–638).**
`--chdir` and `-- opencode --port --hostname [--agent] [--prompt]`. This is the
final argv suffix and is structurally independent of everything above it.

### Proposed sub-functions

```
// appendBwrapNamespaceFlags appends the baseline namespace and system-root
// flags that are unconditional and host-topology-driven.
func appendBwrapNamespaceFlags(args []string, home string) []string

// appendBwrapSessionMounts appends per-session data-dir binds (worktree,
// bare repo, claude dir, mcp-auth, opencode data, nix socket, nix cache).
func appendBwrapSessionMounts(args []string, cfg Config) []string

// appendBwrapConfigAndCredentialMounts appends the remapped config and
// credential mounts (AWS, kube, SSH keys, generated configs, opencode
// allowlist, plugin caches, clipboard).
func appendBwrapConfigAndCredentialMounts(args []string, cfg Config, m *Manager) []string

// appendBwrapEnvVars appends all --setenv flags (credentials, profile env
// vars, NIX_CONFIG, TERM, COLORTERM, standard sandbox env, prism context
// vars, runtime env vars, host-API).
func appendBwrapEnvVars(args []string, cfg Config, m *Manager) []string

// appendBwrapTerminator appends --chdir and the -- opencode command suffix.
func appendBwrapTerminator(args []string, cfg Config) []string
```

`BuildArgs` becomes an orchestrator:

```
func (b *bwrapIsolator) BuildArgs(m *Manager) []string {
    args := []string{}
    args = appendBwrapNamespaceFlags(args, home)
    args = appendBwrapSessionMounts(args, cfg)
    args = appendBwrapConfigAndCredentialMounts(args, cfg, m)
    args = appendBwrapEnvVars(args, cfg, m)
    args = appendBwrapTerminator(args, cfg)
    return args
}
```

### Independence flag

**Wait for A.2** (bwrap/sandbox-exec/podman duplication audit) — same reasoning
as `buildRunArgs`.

A.2 will identify which groups between `BuildArgs` and `buildRunArgs` are
structurally identical modulo flag syntax. If A.2 concludes that "conditional
overlay mounts" and "SSH key mounts" should be shared helpers, the right
extraction target is a shared `mountSpec` type or a helper-table approach,
not two identical-logic functions. Extracting `BuildArgs` helpers before A.2
lands would pre-empt that decision.

`appendBwrapNamespaceFlags` is safe to extract standalone — it has no podman
counterpart and no A.2 dependency.

### Observable-behaviour warnings

- **`--clearenv` position.** The `--clearenv` flag is the very first element in
  `args` (line 181 appends it first). Any extracted helper that touches baseline
  flags must preserve this ordering; bwrap processes flags left-to-right and
  `--clearenv` must precede all `--setenv` calls.
- **`--tmpfs` ordering after `/etc` ro-bind (line 236–238).** The bwrap
  security-shadowing works by applying `--tmpfs /etc/wireguard` *after* the
  `/etc` ro-bind. bwrap applies mounts in the order they appear on the argv. If
  `appendBwrapNamespaceFlags` is split in a way that separates the `/etc` bind
  from the `/etc/wireguard` tmpfs, the shadowing would break silently — the
  wireguard secrets would be visible inside the sandbox.
- **`standardSandboxEnvArgs()` call site.** At line 547, `standardSandboxEnvArgs()`
  is called to inject PATH, HOME, USER, LOGNAME, LANG, LC_ALL, SHELL. If
  `appendBwrapEnvVars` is extracted, it must call this helper in the same position
  relative to the other `--setenv` calls; changing the order would change which
  PATH the sandbox sees if there were duplicate `--setenv PATH` calls (there are
  not currently, but the risk is worth noting).
- **Host-API socket dir bind inside env-var block (lines 596–601).** The
  `cfg.HostAPISockPath` branch in the env-var section also appends a
  `--bind sockDir sockDir` flag (a *mount*, not a `--setenv`). This is mixed into
  the env-var group because the bind must precede the `--setenv PRISM_HOST_API`
  that references it. If `appendBwrapEnvVars` is extracted, it must continue to
  emit this bind flag in the same relative position — it is not a pure env-var
  helper.

---

## 6. Open questions

- **[uncertain] Shared helper extraction scope (A.2 dependency).** `buildRunArgs`
  and `BuildArgs` share substantial structure in their conditional-overlay and
  SSH-key sections. Should the extracted helpers live in a shared file (e.g.
  `internal/container/mount_helpers.go`) with a flag-generation parameter, or
  remain separate per-isolator? This question is open until A.2 completes its
  side-by-side audit.

- **[uncertain] `hostAPIHelpers` struct vs method approach.** The helper closures
  in `hostAPIHandler` (`writeJSON`, `writeError`, etc.) currently close over the
  `Sidecar` receiver. If they are promoted to methods on `*Sidecar`, the receiver
  becomes implicitly available — no struct parameter needed. If they are promoted
  to package-level functions, the `Sidecar` fields they need must be passed
  explicitly. The right shape depends on whether any of these helpers would be
  useful outside `hostAPIHandler` (currently they would not be).

- **[uncertain] Migration dispatch table vs sequential if-chain.** The proposed
  `runMigrations` function with a `[]migrationFn` slice is cleaner but changes
  the calling convention: instead of each migration block closing over `conn`
  directly, each `migrateVNtoVN1(conn)` receives it as a parameter. If migration
  functions need additional context (e.g. a logger, a DB path) in the future, the
  function signature would need to change uniformly. The current if-chain only
  needs to change for the specific migration being added. The dispatch table is
  strictly better only if there is a strong desire for migration-count visibility
  or table-driven testing.

- **[uncertain] `Open` error-close semantics.** The current code calls
  `conn.Close()` inside every migration error path. A restructured `Open` with
  `defer conn.Close()` + reassign on success is cleaner but subtly changes when
  finalizers are triggered. Tests that check `*sql.DB` liveness at specific points
  may notice. This is worth auditing against the test suite before changing.

- **[uncertain] `buildCheckinTimeline` sort stability.** The insertion-sort in
  the checkin handler is not stable in the general case — it relies on
  `CreatedAt.Before()` which uses strict ordering. For events at the exact same
  millisecond (common for events emitted in quick succession), the relative order
  is determined by the merge order of the three input slices. If this ordering is
  load-bearing for any client, changing to `slices.SortStableFunc` would be a
  safer extraction, but that changes the sort algorithm and is observable.

- **[uncertain] `appendBwrapTerminator` opencode port fallback.** Lines 621–624
  in `BuildArgs` fall back to `ContainerPort` when `cfg.AllocatedPort == 0`.
  The comment notes this is "for the theoretical case where AllocatedPort is
  unset". If `appendBwrapTerminator` is extracted, this fallback must be preserved.
  Whether this fallback should be removed (it masks a misconfigured session row)
  is an open question for a future cleanup issue.
