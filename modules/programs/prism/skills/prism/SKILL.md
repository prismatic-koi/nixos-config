---
name: prism
description: Spawn isolated agent sessions in their own git worktrees using the prism tool. Use when the user asks to spawn an agent, delegate work to another session, run something in parallel, or work on a PR or different repo.
---

> **Note:** The prism source code and this skill file live in the `nixos-config` repository under `modules/programs/prism/`. Changes to prism itself — the Go CLI, tmux config, agents, and skills — are made there.

## Introspection layers

Prism provides three introspection layers, ordered from most to least human-readable:

1. **`--help` text** — human-readable usage, intended for humans at a terminal.
2. **`prism agent-context`** — machine-readable JSON describing the full CLI shape: every command, every flag (with type, enum values, defaults and default sources), available profiles, and cross-cutting precedence rules. **This is the layer-2 surface agents should consult when they need to discover available flags, enum values, or profile names programmatically.** It emits valid JSON to stdout and exits 0.
3. **This SKILL.md** — workflow prose, decision trees, and context that neither `--help` nor `agent-context` capture.

### Using `prism agent-context`

```bash
# Full CLI shape as JSON (excludes hidden/internal commands by default)
prism agent-context

# Include hidden/internal commands (e.g. agent-run, sidecar)
prism agent-context --include-hidden

# Discover available profiles
prism agent-context | jq '.available_profiles'

# Inspect spawn's --isolation flag (type, valid values, default source)
prism agent-context | jq '.commands.spawn.flags["--isolation"]'

# See precedence rules for profile and isolation resolution
prism agent-context | jq '.precedence'

# List all top-level commands
prism agent-context | jq '.commands | keys'
```

The document shape (top-level keys):

| Key | Description |
|---|---|
| `schema_version` | Schema version string. Currently `"1"`. Bump on breaking changes. |
| `prism_version` | Git SHA of the binary (`""` in dev builds). |
| `commands` | Map of command name → `CommandMeta` (recursive, includes subcommands). |
| `available_profiles` | Array of profile names from `~/.config/prism/profiles.json`. `[]` if missing. |
| `precedence` | Map of cross-cutting precedence chains (e.g. `profile`, `isolation`). |

Each `CommandMeta` contains:
- `description` — short description
- `flags` — map of `"--flag-name"` → `{type, values?, default?, default_source?, required?, description}`
- `subcommands` — recursive map (same shape)
- `positional_args` — array of `{name, required?}`
- `aliases` — optional array

Flag `type` is one of: `bool`, `string`, `int`, `duration`, `stringArray`, `enum`. When `type == "enum"`, the `values` array lists every valid value — use it instead of parsing help text.

# Spawning Agents with prism

`prism spawn` creates a new git worktree, starts a tmux session in it, and launches the agent harness (pi). Use it when work should be isolated, long-running, or in a different repo — rather than a subagent, which shares the current session context.

## When to use prism spawn vs a subagent

| Situation | Use |
|---|---|
| User says "spawn an agent" or "use prism" | `prism spawn` |
| Work is on a PR branch | `prism spawn --pr` or `prism pr` |
| Task is long-running / should outlive this session | `prism spawn` |
| Quick research or analysis within this repo | subagent (`@explore`, `@general`) |

## Converting a repo to bare+worktree layout

```bash
# Convert the current directory
prism convert

# Convert a specific repo
prism convert ~/code/nixos-config
```

This converts a regular git clone to the prism bare+worktree layout in-place. The working tree moves to `<repo>/<branch>/`, a bare clone is created at `<repo>/.bare`, and the index is populated immediately so `git status` is clean. This is the same operation as selecting `[convert to bare+worktree layout]` in the C-f picker.

## Basic usage

```bash
# Spawn in the current repo with a timestamped branch
prism spawn --prompt "go implement feature X and open a PR"

# Spawn on a named branch — use a short descriptive kebab-case name, not an issue number
prism spawn --branch update-plex-image --prompt "..."

# To work in a different repo, delegate via its coordinator (see "Delegating work to another repo")
prism prompt home-ops@main --prompt 'update the plex image to the latest tag and open a PR'

# Check out a PR branch and spawn a session on it (--repo is supported on prism pr)
prism pr 268 --prompt "review this PR"
prism pr 268 --repo nixos-config --prompt "review this PR"
```

## Passing prompts safely — shell escaping

The `--prompt` value passes through the caller's shell before prism receives it.
Shell metacharacters such as backticks, `$()`, `$VAR`, and double-quote contents
are interpreted by the shell and silently corrupted if you are not careful.

**Preferred approaches (safest first):**

1. **`--prompt-file <path>`** — Write the prompt to a temp file; the shell never
   touches its contents:
   ```bash
   printf 'run `gh pr view 42` and review the diff' > /tmp/prompt.txt
   prism spawn --prompt-file /tmp/prompt.txt
   ```

2. **`--prompt -` (read from stdin)** — The literal value `-` is a reserved
   sentinel that tells prism to read the prompt from stdin. Pipe or heredoc the
   prompt in. Use a quoted heredoc delimiter (`<<'EOF'`) to prevent expansion
   inside the body:
   ```bash
   prism spawn --prompt - <<'EOF'
   run `gh pr view 42` and review the diff
   EOF
   ```
   **Note:** because `-` is reserved, you cannot pass the literal string `-` as
   a prompt via `--prompt`. Use `--prompt-file` or single quotes with a
   different phrasing if your prompt content is literally a dash.

3. **Single quotes** — Wrap the value in single quotes. Single quotes prevent
   *all* shell interpolation in bash/zsh:
   ```bash
   prism spawn --prompt 'run `gh pr view 42` and review the diff'
   ```
   Single quotes cannot contain a literal `'`. For prompts with apostrophes,
   prefer option 1 or 2 instead.

**Do not** use double quotes around prompts containing backticks or `$`:
```bash
# BAD — the shell executes `gh pr view 42` and splices its output in
prism spawn --prompt "run `gh pr view 42` and summarise"
```

## All flags

| Flag | Description |
|---|---|
| `--branch <name>` | Branch name for the new worktree. Defaults to a timestamp. Use a short, descriptive kebab-case name derived from the task (e.g. `update-plex-image`, `fix-login-redirect`) — never an issue number, PR number, or Jira ID. The branch name becomes the session name (e.g. `nixos-config@update-plex-image`), so it should be immediately readable in `prism sessions list` and the tmux picker without looking anything up. |
| `--pr <number>` | Fetch and check out the branch for this PR number. |
| `--prompt <text>` | Instruction passed to the agent on launch. Wrap values containing shell metacharacters in **single quotes**. The value `-` is reserved and reads from stdin (cannot pass a literal `-`). |
| `--prompt-file <path>` | Read the prompt from a file instead of passing it as an argument. Mutually exclusive with `--prompt`. A single trailing newline is stripped. |
| `--agent <name>` | Agent to use (`worker` or `plan`). Defaults to `worker`. |
| `--attach` | Switch the current tmux client to the new session instead of spawning headlessly. |
| `--reuse` | If an active session already exists on the requested branch, return its name/port/agent and exit 0 instead of failing. Without `--reuse`, spawning onto an existing branch exits non-zero and tells you to run `prism cleanup` or pass `--reuse`. |

## Behaviour

- **Headless by default**: the session is created in the background. The user can switch to it via the prism dashboard (`C-w`) or picker (`C-f`).
- **`--attach`**: use when the user wants to be taken directly to the new session.
- The prompt is sent to the agent automatically after startup — no manual input needed.
- Each spawned session gets its own worktree, so changes are isolated from other branches.

## Delegating work to another repo

When you need to delegate work to a repo you are not the coordinator for, route it through that repo's `@main` coordinator session. The coordinator has full context about that repo's conventions, open work, and branch state.

**Flow:**

1. Run `prism sessions list` and look for `<repo>@main`.
2. **Found, not in `waiting` state:** send the work request with `prism prompt <repo>@main --prompt '...'`.
3. **Found, in `waiting` state:** escalate to the user — the coordinator is blocked and expecting human input. The user needs to switch to that session and unblock it directly. Do not attempt to work around the waiting state guard.
4. **Not found:** there is no coordinator to delegate to. Escalate to the user and ask them to start a `<repo>@main` coordinator session. Note: you also cannot work around this by spawning onto `main` yourself — in the bare+worktree layout prism uses, `main` already has a worktree, so `prism spawn --branch main` will fail with a git error.

Spawning directly into a feature branch in another repo (bypassing the coordinator) should only happen when you **are** the coordinator for that repo, or when the user explicitly instructs you to.

```bash
# Check if the target repo has a coordinator session
prism sessions list

# If home-ops@main exists and is not waiting:
prism prompt home-ops@main --prompt 'Please update the plex image to the latest tag and open a PR'

# If home-ops@main exists but IS in waiting state:
# escalate to the user — they need to switch to that session and unblock it

# If home-ops@main does not exist:
# escalate to the user — ask them to start a coordinator session for home-ops
```

### Example: spawning directly as coordinator

If you **are** the coordinator for the target repo (or the user has explicitly instructed you to spawn directly), use `prism spawn`:

```bash
prism spawn \
  --branch update-plex-image \
  --prompt "find the plex container image in this repo and update it to the latest tag from dockerhub, then open a PR"
```

## Running code review

> **Scope:** `prism review` is for **worker
> agents and spawned sessions only**. Coordinator agents must never call
> `prism review` directly. When a user asks a coordinator to review a PR, the
> coordinator should use `prism pr <number> --prompt 'review this PR'` to spawn
> a session on the PR branch — that spawned session then runs `prism review`
> and reports back.

Code review is done with `prism review <pr>`, which is **async**: it spawns 5
review agents, registers a group, and returns immediately with an
acknowledgement. Results are delivered to you via a follow-up `prism prompt`
when all agents complete.

```bash
prism review <pr-number>
```

**Do NOT commit, merge, or announce completion** until the review-complete
prompt arrives. When it does, handle PASS/FAIL per the worker agent instructions.

For a synchronous flow (one-shot script, no other work to do meanwhile) pass
`--wait`:

```bash
prism review <pr-number> --wait
prism review <pr-number> --wait --json   # script-friendly
```

`--wait` blocks until the review group reaches a terminal state and exits 0
on all-PASS, non-zero on any FAIL / no-start / timeout. See the `--wait`
section above for the full contract (exit codes, Ctrl-C semantic, idempotent
observation).

### Pre-flight rebase gate (`prism review` refuses when behind `origin/main`)

Before spawning any review agents, `prism review` runs a **one-shot pre-flight
check**:

1. `git fetch origin main` (one network round-trip).
2. Strict ancestor check: `git merge-base --is-ancestor origin/main HEAD`.
3. If `origin/main` is an ancestor of `HEAD`: proceed unchanged.
4. If not: refuse, exit non-zero, **no agents spawn**, and **no cycle counter
   increment**. The error message names the number of commits behind and the
   recommended fix:

   ```
   prism review: branch is N commits behind origin/main

       git fetch origin main
       git rebase origin/main
       git push --force-with-lease

   Or rerun with --rebase to do this inline.
   ```

The `--rebase` flag is the inline opt-in fix:

```bash
prism review <pr> --rebase
```

It performs the fetch + rebase + force-push inline and then proceeds to the
review against the rebased HEAD. If the rebase produces conflicts, the rebase
is aborted, the worktree is restored to the original `HEAD`, and the command
exits non-zero — never leaves the worktree mid-rebase. Resolve conflicts
manually and re-run.

**Why this gate exists.** Reviewers regularly produce noisy findings of the
form "you should also update X" when X landed on `main` after the branch was
cut. A simple rebase makes the diff smaller and the finding disappear, but
discovering this from a FAIL verdict burns a full 5-agent cycle. The gate
catches drift in one fetch, before any agent spawns.

**Cycle-counter contract.** Gate failures (behind-main refusal, fetch failure,
missing `origin/main`, rebase conflict abort) **do not increment** the
review-cycle counter. They are the same category as "round already in
progress" / pure-infrastructure failures / ran-but-no-parseable-verdict
rounds (#1995): no full set of parseable verdicts was produced, so the
round does not count. A worker that hits the gate three times in a row
and then runs three real reviews still has all three real cycles
available before the LOOP-LIMIT footer fires.

Rounds that **do not count** toward the 3-cycle limit:

- **Pre-flight gate refusals** — behind-main, fetch failure, missing
  `origin/main`, rebase conflict abort. No agents spawned.
- **Round-already-in-progress refusals** — a prior review is still
  active for the same parent. No agents spawned.
- **Pure-infrastructure failures** — every agent failed to start
  (container never bound its port). Header mentions "infrastructure
  failure".
- **Ran-but-no-parseable-verdict rounds** (#1995) — one or more agents
  reached `finished` state without emitting a parseable
  `<verdict>PASS</verdict>` / `<verdict>FAIL</verdict>` tag (e.g.
  truncated mid-analysis or ended on a tool-only turn). Header says
  **"One or more review agents ran but produced no parseable verdict"**
  and explicitly tells the worker to re-run. This is NOT a code-quality
  FAIL — re-run, do not escalate.

In each of these cases the correct action is to re-run `prism review`
(after fixing any orthogonal blocking issues another agent surfaced),
not to escalate.

**Design notes:**

- **Strict ancestor check, not loose.** A "files-touched-in-common" variant
  sounds clever but breaks on renames, deletes, and cross-cutting helper
  introductions. Strict `is-ancestor` is unambiguous and cheap.
- **Refuse-by-default + opt-in `--rebase`, not auto-rebase.** A `review` verb
  that silently mutates the branch is a footgun if the worker has uncommitted
  work or local-only commits. Default refusal forces a deliberate choice.
- **One-shot at the start, not continuous.** `main` can advance during a
  review run; we do not chase that. The gate is a snapshot at review-spawn
  time, consistent with how CI works.
- **Same gate in host-direct and container-routed paths.** A container worker
  routes through `/review` on the host sidecar; the gate runs on the host
  side, and the refusal streams back to the container worker as a non-zero
  review exit — same UX as a host-direct refusal.

### Handling no-start errors in review-complete prompts

When a review-complete prompt says **"One or more review agents failed to start
(infrastructure failure)"**, treat it as a failed review run — not a
code-quality FAIL verdict. The agents never ran, so no conclusions about the PR
quality can be drawn. Re-run `prism review <pr>` to retry the infrastructure
that failed. Do not treat a no-start error the same as a FAIL verdict from a
review agent that ran and found issues.

Signs of a no-start error in the per-agent findings:
- `**Verdict:** ERROR`
- Output contains `ERROR: agent failed to start (no-start):`
- The delivery message header mentions "infrastructure failure" and instructs you to re-run

### Handling ran-but-no-parseable-verdict in review-complete prompts

When a review-complete prompt says **"One or more review agents ran but
produced no parseable verdict"** (#1995), treat it the same way you treat a
no-start error: re-run `prism review <pr>` rather than escalate. The agents
did run — the problem is that one or more of them terminated in `finished`
state without emitting a parseable `<verdict>PASS</verdict>` /
`<verdict>FAIL</verdict>` tag (e.g. truncated mid-analysis, ended on a
tool-only turn). This is **not** a code-quality FAIL and the round **does
not count** toward the 3-cycle limit.

Signs of a ran-but-no-parseable-verdict round:
- The per-agent output for at least one agent contains
  `ERROR: no verdict found in agent output` or `ERROR: no output produced`
- The delivery message header explicitly says the agent(s) ran but produced
  no parseable verdict and instructs you to re-run

If any other agent in the same round surfaced genuine blocking issues, fix
those before re-running.

If no review-complete prompt arrives within 30 minutes, check progress with:

```bash
prism checkin <session>~review-<N>-review-goal
```

## Investigator agents

Use `prism investigate` to spawn a read-only research session from within a prism session. Investigators are well-suited to tasks like tracing call chains, mapping symptoms to a file:line, or surveying scope before spawning a worker. They are denied in the worker deny list — only coordinators can use them.

### Spawning

```bash
prism investigate --prompt "question"
prism investigate --prompt-file /tmp/question.txt
prism investigate --name my-analysis --prompt "question"
```

### Flags

| Flag | Description |
|---|---|
| `--prompt <text>` | Research question. Mutually exclusive with `--prompt-file`. |
| `--prompt-file <path>` | Read the question from a file. Mutually exclusive with `--prompt`. |
| `--name <slug>` | Human-readable slug for the session name. When provided, the session is named `<invoker>~investigate-<slug>` exactly. Only `[a-z0-9-]` allowed, max 40 chars, no leading/trailing dash. When omitted, the slug is derived automatically from the prompt text. |

One of `--prompt` or `--prompt-file` is required. The command returns a session name within ~2 seconds and exits 0. There is no `--wait` flag — `prism investigate` is inherently async.

### Session-naming convention

Spawned sessions are named `<invoker>~investigate-<slug>`. When `--name` is provided, `<slug>` is the supplied name exactly. When `--name` is omitted, `<slug>` is a short kebab-case token derived automatically from the prompt text (word-boundary truncated at 30 chars).

### Per-turn notification contract

After each investigator turn that produces output, the sidecar delivers a body-bearing notification to the invoking session. Each notification includes:

- **Header:** `From investigator session: <name>` — use this to route the notification to the correct open question when multiple investigators are running concurrently.
- **Body:** the investigator's findings for that turn.
- **Steering hint:** `Reply with: prism prompt <name> --prompt '...'` — follow-up steering is done via `prism prompt`.

### Cleanup responsibility

Investigators do not self-terminate. When the investigation is complete and the findings consumed, the coordinator must run:

```bash
prism cleanup --yes --session <inv-session>
```

### Constraint

`prism investigate` must be run from within a prism session (errors if no invoker is detectable). Workers have `prism investigate` in their deny list; only coordinators may use it.

---

## Merge queue (coordinators only)

> **Coordinators only.** Worker agents, container worker agents, bwrap worker agents, and review agents all have `prism merge` and `prism merge *` denied in their bash deny lists. If you are not a coordinator agent, skip this section.

The merge queue is a local serial FIFO queue running in the coordinator's sidecar process. The sidecar polls the head of the queue every 45 seconds; only one PR is in flight at a time. The watcher's lifetime equals the coordinator session's lifetime — there is no persistent daemon.

A queued PR moves through states keyed off GitHub's `mergeStateStatus`: `watching` → `merged` / `failed` / `cancelled` / `abandoned`. See issue #783 for the full state machine.

### Command surface

| Command | Description |
|---|---|
| `prism merge <pr>` | Enqueue a PR. Returns within ~2 seconds. Idempotent — safe to call on an already-queued PR. |
| `prism merges` | Show the queue scoped to the current coordinator session (alias for `prism merges list`). |
| `prism merges list` | Same as above. |
| `prism merges list --failed` | Show only failed entries. |
| `prism merges list --abandoned` | Show entries left behind by a previous coordinator incarnation. |
| `prism merges list --all` | Include terminal-state history (last 7 days). |
| `prism merges cancel <pr>` | Remove a `watching` entry from the queue. |

Add `--json` to any `prism merges` / `prism merges list` invocation (including with `--failed`, `--abandoned`, or `--all`) to get a JSON array of merge-queue entries instead of the table — use this when scripting or polling.

### `--wait` for synchronous flows (#1500)

`prism merge`, `prism review`, and `prism spawn` accept `--wait` for cases where the agent's workflow is genuinely synchronous — for example, a one-shot script that wants to merge a PR and then immediately deploy. With `--wait`, the command blocks until the underlying job reaches a terminal state and exits non-zero on any non-success terminal.

| Command | Terminal definition | Default `--timeout` |
|---|---|---|
| `prism merge <pr> --wait` | merged / failed / cancelled / abandoned (in `pending_merges`) | `30m` |
| `prism review <pr> --wait` | All review agents reached `finished`/`error`/`deleted` (group complete) | `20m` |
| `prism spawn ... --wait` | Spawned agent reached `finished` / `error` / `interrupted` / `deleted` for its first turn | `10m` |

**When to prefer `--wait` vs the notification path:**

- Prefer `--wait` when you have **no other useful work** to do until the job lands — a one-shot script, a deploy that depends on the merge, or a review that gates further commits.
- Prefer the **notification path** (no `--wait`) when you have **other tasks in flight**. Coordinators with several PRs in flight should never `--wait` — `prism merge <pr>` returns immediately and the merge-queue watcher delivers a notification when each PR lands. Same shape for `prism review` and `prism spawn`.
- Add `--json` when scripting: `prism merge <pr> --wait --json` emits a single JSON object on stdout (no human-readable chatter) so consumers can `jq` the status.

**Idempotent observation.** `prism merge <pr> --wait` on an already-merged PR returns immediately with the merged status — safe to call any number of times.

**Ctrl-C semantic.** Killing a `--wait` invocation (Ctrl-C, SIGTERM) interrupts the **local** wait only — the underlying merge / review / spawn keeps running. To recover the result later, re-run the same command (or `prism merges list` / `prism reviews list` / `prism checkin <session>`).

**Inside a sandbox.** `--wait` works identically inside and outside a bwrap / podman / sandbox-exec sandbox. The CLI auto-detects `PRISM_HOST_API` and routes its terminal-state probes through the sidecar's read-only wait endpoints, so the host's prism.db is the source of truth in either case.

**Exit codes.** `--wait` paths use exit codes that distinguish failure modes:

| Exit | Meaning |
|---|---|
| `0` | terminal success (merged / all-PASS / finished) |
| `2` | terminal failure (failed CI, any-FAIL, error state) |
| `3` | local `--timeout` elapsed (the underlying job continues) |
| `4` | user interrupted with Ctrl-C (the underlying job continues) |

### Reviews ledger: `prism reviews list`

Reviews now have a dedicated ledger surface analogous to `prism merges`. Use it instead of `prism sessions list | grep '~review-N-'` (fragile, missing group metadata).

```bash
prism reviews            # alias for `prism reviews list`
prism reviews list       # all review groups, newest first
prism reviews list --json
```

Each row carries: PR number (when derivable from the parent branch), parent (worker) session, agent sessions, group state (`in-progress` / `completed` / `empty`), round number, and `started_at` timestamp.

### Notification contract

When a queued PR reaches a terminal outcome, the watcher delivers a bus notification to the coordinator session. The text is:

| Outcome | Notification text |
|---|---|
| Merged (with archive path) | ``PR #N merged. Archive: <archive_path>. Run `git pull` in @main and `prism cleanup` the worker session.`` |
| Merged (no archive path yet) | ``PR #N merged. Run `git pull` in @main and `prism cleanup` the worker session.`` |
| Merge conflicts | `PR #N has merge conflicts — worker rebase needed` |
| CI failure | `PR #N CI failed — needs worker fix` |
| Closed without merging | `PR #N was closed without merging — removed from queue` |
| Review required | `PR #N is blocked — human reviewer approval required before merge` |
| Changes requested | `PR #N is blocked — reviewer requested changes — fix and re-request review` |
| Other merge failure | `PR #N merge failed: <error>` |
| Coordinator session ended while watching | Row transitions to `abandoned` — surfaces via `prism merges list --abandoned` only; no live notification. |

### Action table

When a merge-queue notification arrives, treat it as high-priority (same as a worker finished-notification):

| Notification | Action |
|---|---|
| `merged` | `git pull` in @main, then `prism cleanup --yes --session <worker-session>` |
| `merge conflicts` | `prism prompt <worker-session>` asking it to rebase onto main and push; re-enqueue with `prism merge <pr>` after the worker finishes |
| `CI failed` | `prism prompt <worker-session>` asking it to investigate the failed check, fix, and push; re-enqueue with `prism merge <pr>` |
| `closed without merging` | Usually nothing — the PR was closed deliberately. Investigate if unexpected. |
| `blocked — human reviewer approval required` | Request a human review on the PR (e.g. via `gh pr review --request <user>`). Once approved, re-enqueue with `prism merge <pr>`. |
| `blocked — reviewer requested changes` | `prism prompt <worker-session>` asking it to address the reviewer's requested changes and re-request review on the PR. Once the reviewer re-approves, re-enqueue with `prism merge <pr>`. |
| `merge failed: <error>` | Read the error, decide whether to retry (`prism merge <pr>`) or escalate to the user. |
| `abandoned` (via `--abandoned` listing) | A new coordinator decides whether to re-enqueue with `prism merge <pr>`. |

### Why workers cannot invoke it

Worker agents, container worker agents, bwrap worker agents, and review agents all have `prism merge` and `prism merge *` in their bash deny lists. Only coordinator agents have it allowed. This is by security design: only coordinators arbitrate merge order. Do not attempt to work around the deny list.

## Example: reviewing a PR (manual spawn)

```bash
# PR in the current repo
prism pr 268 --prompt "review this PR and summarise the changes"

# PR in a different repo (--repo is supported on prism pr)
prism pr 268 --repo nixos-config --prompt "review this PR and summarise the changes"
```

## Example: create a ticket then spawn an agent to action it

When the user asks you to create a ticket (e.g. Jira) and then spawn an agent to work on it:

1. Create the ticket using the appropriate MCP tool and capture the ticket ID (e.g. `PROJ-123`)
2. Derive a short, descriptive kebab-case branch name from the ticket title — not the ticket ID
3. Spawn the agent — pass the ticket ID in the prompt so the agent can look it up; the Atlassian MCP is available in the spawned session

```bash
# After creating ticket PROJ-123 ("Update plex image to latest tag"):
prism spawn \
  --branch update-plex-image \
  --prompt "Please take a look at PROJ-123, cover off the work required, and open a pull request."
```

## Lifecycle: cleaning up after a merge

When you spawn a session and later merge its PR yourself, you are responsible for cleaning up the worktree and session. The spawned agent cannot do this — it would be tearing down its own environment.

`prism spawn` prints the session name when running headlessly:

```
session "nixos-config@update-plex" created
```

Note down the session name from that output. Once you have merged the PR, clean up:

```bash
prism cleanup --yes --session "nixos-config@update-plex"
```

`prism cleanup --yes --session <name>` will:
- Remove the git worktree
- Force-delete the local branch (relies on the orchestrator-trust contract: call this only after confirming the PR is merged)
- Kill the tmux session, redirecting any attached client to `scratchpad`
- Mark the session's `agent_status` row ended (it drops out of `prism sessions list`) and release its port allocation. The row itself is **not** deleted — re-spawning on the same branch name re-seeds it to `idle`, which succeeds for any prior terminal state (`error`, `finished`, or `interrupted`).

Only call this after you have confirmed the PR is merged. The `--yes` path always force-deletes the branch — it does not check whether the branch is reachable from main, because squash-merges produce a different SHA on main than the branch tip.

Pi sessions block `git worktree prune` and `git worktree remove` at the extension layer. When recovering from a failed spawn, use `prism cleanup` — do not reach for git plumbing.

## Querying prism state — prefer `--json` for scripting

Every list-style and lookup-style prism subcommand supports a `--json` flag that emits a single JSON document to stdout — keys are snake_case, timestamps are RFC 3339, empty lists are `[]` (never null, never absent), and any informational/progress text routes to stderr. **When you need to parse prism output programmatically, always pass `--json`.** Screen-scraping tabular human-readable output is fragile and burns tokens.

| Command | `--json` shape |
|---|---|
| `prism sessions list --json` | array of session-status objects |
| `prism sessions list --all --json` | array of session-status objects across all repos |
| `prism checkin <session> --json` | session + events object |
| `prism stats --json` (and `prism stats <id> --json`) | rows mirroring the host-API |
| `prism merges --json` (and `prism merges list --json`, `--failed --json`, `--abandoned --json`, `--all --json`) | array of merge-queue entries |
| `prism audit --json` | object with `events` array, `truncated` bool, optional `hint` |
| `prism sessions status --json` | object keyed by state (`active`, `waiting`, `idle`, `finished`, `error`) with integer counts (mutually exclusive with `--tmux-format`) |
| `prism profile list --json` | array of profile objects |
| `prism profile show [name] --json` | single profile object describing the slot table |
| `prism archive <session> --all --json` | array of archive-entry objects (instance_id, started_at, archive_path) |

## Checking in on a running session

Use `prism sessions list` to see all active agent sessions with their state and current task title:

```bash
prism sessions list          # human-readable table
prism sessions list --json   # JSON array (use this when scripting)
```

Use `prism checkin <session>` to read the recent conversation history for a session, sourced from the prism DB. The default output is a rich narrative view: assistant messages, state changes, and tool call one-liners interleaved chronologically. Pass `--json` when you need to parse the events programmatically.

```bash
prism checkin nixos-config@update-plex
```

```
checkin: nixos-config@update-plex

state: finished

[18:48:35] assistant
I'll fix both issues from the review.
  → edit: container.go ✓
  → edit: sidecar.go ✓
  → bash: go build ./... ✓
  → bash: go test ./internal/... ok (9.2s)

[18:49:16] assistant
Tests pass. Committing and pushing.
  → bash: git commit -m "fix: capture elapsed..." ✓
  → bash: git push ✓

[18:50:25] ● finished

── end of event log ──
```

**Tool one-liner format**: `→ <tool>: <key_arg> <result_summary>`

- **bash** — key arg is the command (first ~80 chars); result is first meaningful output line, `✓` for empty stdout, `✗ <message>` on error
- **read** — key arg is the file path; result is `✓ (N lines)`
- **edit / write** — key arg is the file path; result is `✓` or `✗`
- **task** — key arg is the description; result is `✓` or `✗`
- **glob / grep** — key arg is the pattern; result is `N matches` or `no matches`
- **todowrite** — result is `✓` (key arg omitted)

**State changes** appear inline as `● <state>` (e.g. `● finished`) with a timestamp.

**User messages** (`▶ user`) are visually distinct from assistant turns so coordinator prompts and bus notifications are easy to spot.

With no argument, `prism checkin` lists available sessions and exits with a hint.

See [Debugging a running or stuck session](#debugging-a-running-or-stuck-session) for `prism checkin` flag reference and a full decision tree for diagnosing issues.

## Sending a follow-up prompt to a running session

Use `prism prompt <session> --prompt <text>` to send a follow-up message to the agent in a session that is already running (or has finished and is waiting for input):

```bash
# Simple prompt — single quotes prevent shell interpolation
prism prompt nixos-config@update-plex --prompt 'looks good, go ahead and open a PR'

# Prompt with backticks or complex content — use --prompt-file
printf 'run `make test` and fix any failures' > /tmp/p.txt
prism prompt nixos-config@update-plex --prompt-file /tmp/p.txt

# Or via stdin
prism prompt nixos-config@update-plex --prompt - <<'EOF'
run `make test` and fix any failures
EOF
```

The same shell-escaping conventions that apply to `prism spawn` apply here —
see [Passing prompts safely](#passing-prompts-safely--shell-escaping) above.

### `prism prompt` flags

| Flag | Description |
|---|---|
| `--prompt <text>` | Prompt text to send. Supports `-` to read from stdin. |
| `--prompt-file <path>` | Read prompt from a file. Mutually exclusive with `--prompt`. |
| `--deliver-as <mode>` | Delivery mode for socket-pipe (PI) sessions: `steer` (default), `followUp`, or `nextTurn`. Has no effect for HTTP-transport sessions. |

**Delivery modes** (relevant for PI / socket-pipe sessions):

| Mode | Behaviour |
|---|---|
| `steer` **(default)** | Injects the prompt mid-turn so the agent sees it immediately, even during a long tool-call sequence. Use for coordinator mid-flight corrections and scope changes. |
| `followUp` | Queues the prompt as the next user turn, delivered after the current turn completes. Use when the message is informational and does not need to interrupt the current turn. |
| `nextTurn` | Alias for `followUp`; the sidecar's own default when `deliver_as` is absent from the request body. |

The CLI validates the mode client-side before making any network call. An invalid value exits non-zero with a message listing the accepted values.

The prompt is delivered directly via HTTP to the session. The session must exist and have an active harness port — use `prism sessions list` to check first.

### Waiting state guard

`prism prompt` will **refuse** to send a prompt if the target session is in `waiting` state. A `waiting` agent has paused and is expecting direct input from the user — injecting a programmatic prompt would corrupt the input field.

If you encounter this error, **escalate to the user**. Do not attempt to work around the guard. The user must switch to the session themselves (via `C-f` or `C-w`) and respond directly.

```
session "nixos-config@update-plex" is waiting for user input

The agent has paused and is expecting a direct response from the user.
Please switch to that session and respond there, or escalate to the user
so they can address it directly.

  prism checkin nixos-config@update-plex   — inspect the current state
  (C-f or C-w)                             — switch to the session in tmux
```

## Escalating to your coordinator with `prism escalate`

Workers that hit a decision they cannot make alone (3-cycle review-limit reached, AC contradiction, scope ambiguity, infrastructure block) should use `prism escalate` rather than crafting a `prism prompt` to the coordinator by hand and stopping. The command resolves the right coordinator, delivers the message, transitions the calling session into a new `escalated` state, and emits a `session.escalated` bus event — in one step, with no redundant "has finished" notification.

### Surface

```bash
# Auto-discover the same-repo coordinator and deliver the prompt.
prism escalate --prompt "3-cycle review limit reached on PR #1234. Agent failing: review-security. Decision needed: option A (relax) or option B (rework)."

# Read the body from stdin for multi-line prompts:
prism escalate --prompt - <<'EOF'
PR #1234 cycle 3:
  - review-goal, review-code, review-qa, review-context: PASS
  - review-security: FAIL on the same blocker as cycle 2

Proposed resolution: relax permission check to coordinator-only.
Decision needed: yes / no.
EOF

# Override discovery and target a specific session by name:
prism escalate --to nixos-config@main --prompt "..."
```

### State machine

```
active ──prism escalate──▶ escalated
escalated ──any turn_start (from any source)──▶ active
```

- `escalated` is a new value alongside `active` / `idle` / `finished` / `reviewing`. It surfaces in `prism sessions list` so a glance shows which workers are paused awaiting guidance.
- The transition out is triggered by **any** incoming `turn_start`, not specifically `prism prompt`. A human who pokes at the worker via tmux clears the flag too.
- While in `escalated`, the sidecar suppresses the "has finished" notification. The `session.escalated` bus event is the notification.

### Discovery rules

| Situation | Behaviour |
|---|---|
| Exactly one same-repo coordinator candidate | Auto-discover, send to it. |
| Multiple same-repo coordinator candidates | Refuse without `--to`; list candidates and exit non-zero. **State does NOT transition.** |
| No coordinator candidate found | Still transition into `escalated`; record `no coordinator found, please wait for a human to come check on you` in the worker's own log. The worker stays paused. |
| `--to <session>` set but session unknown | Exit non-zero. **State does NOT transition.** |

A same-repo coordinator candidate is any active (ended_at IS NULL) row in the same repo whose `root_agent_name = 'coordinator'`, OR a legacy row literally named `<repo>@main` with NULL `root_agent_name`.

### Bus event

- New event type `session.escalated`, distinct from `session.finished`. Existing handlers that subscribe only to `session.finished` continue to receive nothing for escalations.
- Payload carries: `source` (calling worker), `target` (coordinator session, empty when none), `prompt` (body), `pr_numbers` (open PRs whose head matches the worker's branch), `branch`, `head_sha`, `verdicts` (last review-cycle verdicts when discoverable), `occurred_at` (RFC3339).
- The same payload is also written into the calling session's own event log as type `escalation` so `prism checkin <self>` shows the escalation context inline.

### Success signal — the `OK` line is the verification

On successful delivery `prism escalate` prints exactly one line to **stdout**, mirrored to **stderr** so combined-stream capture (e.g. a bash tool that merges both) always surfaces it:

```
prism escalate: OK delivered to <target> (delivery_id=<uuid>)
```

The `OK` token is the first whitespace-delimited word after `escalate: ` so callers can grep for it as the unambiguous success signal. **Do not re-run `prism escalate` to verify delivery** — the `OK` line is the verification. Re-running is the bug pattern issue #2018 fixed: a worker that interpreted a `(no output)` capture as failure and re-ran produced two distinct deliveries to the coordinator before the sender-side guard was added.

The `--json` flag emits a single line to stdout instead:

```json
{"delivered_to": "<target>", "delivery_id": "<uuid>", "replayed": false}
```

In `--json` mode the human-readable line is NOT emitted on stdout (mutual exclusion); it may still be mirrored to stderr for log capture. On error, `--json` emits `{"error": "<message>"}` to stderr and exits non-zero.

The success signal reaches the caller identically from a direct-host invocation and from inside a bwrap / sandbox-exec / podman container: the container path's sidecar proxy captures the host-side child's stdout and stderr separately and re-emits them on the matching local streams, so the `OK` line lands on the container's stdout (and the mirror on stderr) byte-for-byte the same as a host invocation. `--json` is forwarded to the host child via the proxy request body, so the JSON envelope is also surfaced end-to-end.

### Sender-side idempotency — re-runs within 5 minutes are a no-op

Running `prism escalate` a second time within 5 minutes with the same `(calling session, target, prompt text)` triple as a previously-delivered escalation is a **no-op that exits 0**. The replay invocation:

- does NOT write a new `bus_messages` row
- does NOT write new `escalation` or `session.escalated` rows to `agent_events`
- does NOT re-deliver the prompt to the coordinator's sidecar
- does NOT re-transition `agent_status.state` (it stays `escalated`)

The replay emits a distinct success line so the operator/agent can tell it was deduped:

```
prism escalate: OK already delivered to <target> (delivery_id=<prior>, age=<duration>)
```

The `<duration>` is the time since the prior `sent_at`, formatted as `Ns` / `Nm` / `Nh`. The `--json` form is `{"delivered_to": "<target>", "delivery_id": "<prior>", "replayed": true, "age_seconds": <int>}`.

The dedup query is scoped to `from_session = self` exactly. A different worker in the same repo sending the same prompt to the same coordinator is a distinct escalation and lands as normal.

The dedup guard additionally requires the calling session's `current_state` to still be `escalated`. If an incoming turn_start unstuck the worker between the two invocations, the second call is a genuine re-escalation: it transitions back to `escalated` and re-delivers.

### Delivery guarantee — exactly-once with optional replay marker

The escalation prompt is delivered to the coordinator's harness **exactly once** per `prism escalate` invocation. The sidecar's `/prompt` handler is idempotent: each delivery carries a `delivery_id` (UUID minted by the sender), and repeats whose ID has been seen recently are dropped before they reach the harness pipe — the dedup set is bounded (LRU, capacity 256, in-memory per sidecar). Senders that retry with the same `delivery_id` see `{"replayed":true}` in the response so the retry is observable, not silent.

The one path that produces a second copy is the reconnect-replay case for AC #7: if the coordinator's PI extension is disconnected from its sidecar when an escalation arrives, the sidecar buffers the delivery and flushes it on the next successful handshake. The replayed prompt frame carries `replay: true` so the coordinator can distinguish it from a fresh signal. The buffer is bounded (capacity 16); under a long partition the oldest entries are dropped FIFO with a log line.

**Coordinator-side handling.** Coordinators receiving `prism prompt`-style frames do not need to deduplicate — the sidecar guarantees exactly-once for the same delivery_id. If you see `replay: true` on a prompt frame (visible in the assistant-side prompt body once the PI runtime exposes it; for now, observable only in raw frame archives), the delivery is a buffered resume of a partition-window escalation. Treat it informationally: the original was already accepted, this is the post-reconnect notification of that earlier acceptance.

This contract supersedes the pre-fix behaviour where `prism escalate` could deliver the same prompt body multiple times under load (issue #1685). Sender-side double-invocation (a worker re-running `prism escalate` because the success signal was unclear) is covered by the idempotency guard above; see issue #2018.

### When to use `prism escalate` vs `prism prompt`

- **`prism escalate`** — you are a worker handing a question or decision to the coordinator and pausing your turn until you hear back. Use this whenever you would have otherwise stopped after sending a hand-crafted `prism prompt`.
- **`prism prompt`** — you are sending an informational follow-up to a running session and either continuing your work (sender keeps going) or expect no response (e.g. delivering a review-complete prompt). Workers prompting their own coordinator should usually be using `prism escalate` instead.

### Out of scope (v1)

- Cross-repo escalation — v1 is single-repo; auto-discovery is repo-scoped.
- Escalation receipts back to the worker — the worker discovers the coordinator's response by being prompted.
- Re-escalation timeouts.
- Dashboard panel for `escalated` sessions — surfaced via `prism sessions list` and the bus only.

## Worker terminal-state notifications

When a worker session reaches a terminal state, the coordinator that spawned it receives a body-bearing prompt notification. This is the signal the coordinator uses to know that a delegated task has finished and is ready for review / cleanup / merge.

### Wording (verbatim)

| Terminal state | Notification body |
|---|---|
| `finished` (clean exit) | `Agent <name> has finished its current task` |
| `error` (crash / restart-exhausted) | `Agent <name> has errored its current task` |

The wording is fixed so coordinators can pattern-match on either string.

### Delivery contract

- **Exactly-once with replay marker (issue #1695).** Each notification carries a `delivery_id` (UUID minted by the sender). The receiving sidecar dedups repeats by ID before they reach the harness pipe. Retried deliveries with the same ID see `{"replayed":true}` in the response so retries are observable, not silent.
- **Delivery mode is `followUp`.** The notification queues behind any in-flight turn on the coordinator side so it doesn't interleave with an active assistant turn.
- **Suppressed while escalated.** A worker in the `escalated` state has already informed the coordinator via `session.escalated`; a subsequent "has finished" notification would be a false signal (the worker is paused awaiting guidance, not done). The state clears on any incoming turn_start, after which a normal finish notifies as usual.

## Debugging a running or stuck session

Use this decision tree when a session appears stuck, produces no output, or fails unexpectedly.

**Step 1 — Session state check:**

```bash
prism sessions list          # human-readable table
prism sessions list --json   # parseable when scripting
```

Examine the `state` column (`active`, `waiting`, `idle`, `finished`, `error`), the port, and the `last_seen` timestamp. If a session has a DB row but no live tmux session, it may be a zombie (DB row without a live process). Proceed to step 2.

**Step 2 — Recent activity:**

```bash
prism checkin <session>
```

Reads the last 10 turns from the prism DB as a rich narrative view. Use `--verbose` for full tool args/results when something looks off. See [`prism checkin` flags](#prism-checkin-flags) below for all options.

**Step 3 — Sidecar logs:**

```bash
prism logs <session>
```

The raw sidecar log is where container startup errors, timing traces, and stderr from failed `podman run` commands land. This is the most informative diagnostic for infrastructure failures. See [`prism logs`](#prism-logs) below for full flag documentation.

**Step 4 — Source cross-reference:**

When a log or event message includes a `file:line` reference, read that location directly in the Go source under `modules/programs/prism/prism/` to understand the exact code path.

### `prism checkin` flags

| Flag | Description |
|---|---|
| `--last <N>` | Number of message turns to show (default 10) |
| `--from <id>` | Show N events forward from this event ID |
| `--before <id>` | Show N events backward from this event ID |
| `--verbose` / `-v` | Full forensic output: full tool args and full results with no truncation |
| `--types <list>` | Orthogonal event-type filter — routes to the raw-event path (e.g. `--types audit`, `--types state_change`). Not needed for the narrative view; tool calls are included by default. |
| `--all` | (no-arg mode only) List all sessions across all repos |

### `prism logs`

The `prism logs` command streams the raw sidecar log for a session to stdout.

```bash
prism logs <session>              # full sidecar log to stdout
prism logs <session> --tail N     # last N lines only
prism logs <session> --follow     # stream new lines as they arrive (ends ~5s after terminal state)
prism logs <session> -f           # alias for --follow
prism logs <session> --harness-events     # raw PI JSONL frames (P5.LOGS / #1218)
```

Works identically from a host shell and from inside a coordinator container. When `PRISM_HOST_API` is set (container mode), `prism logs` proxies through the host-API Unix socket — no special handling required. The output is the raw log and can be piped to `grep` / `rg`.

#### `--deliver=<sink>` — route the artifact directly

The `--deliver` flag short-circuits the usual "pipe stdout into something" two-step. Three sinks are supported:

```bash
prism logs <session>                                      # stdout (default)
prism logs <session> --deliver=stdout                     # explicit stdout
prism logs <session> --deliver=file:/tmp/sidecar.log      # atomic write to a file
prism logs <session> --deliver=webhook:https://example.com/triage
prism logs <session> --harness-events --deliver=webhook:https://example.com/frames
```

- `file:<path>` writes via tempfile + rename so a failed deliver cannot leave a half-written file. On success it prints `{"delivered_to":"file:<path>","bytes":N}` to stdout.
- `webhook:<url>` POSTs the content with `Content-Type: text/plain` (or `application/x-ndjson` for `--harness-events`). On success it prints `{"delivered_to":"webhook:<url>","status":<code>}`. A 4xx or 5xx response is surfaced as a non-zero exit with a JSON object containing `status` and a truncated `body`. The local log file is read on demand so a failed delivery never modifies the source.
- Unknown schemes (`s3:...`, `mailto:...`, etc.) are refused with the valid set listed (principle 3 of #1497).
- `--deliver` and `--follow` are mutually exclusive: delivery captures a snapshot.

### Common failure signatures

A lookup table of log patterns, their causes, and remediation hints:

- **`statfs <path>: no such file or directory`** — podman was told to bind-mount a path that does not exist on the host. Common cause: a container-internal path (e.g. `/workspace`) leaked into a host-side `podman run` invocation. Check the preceding log line for the full `podman run` command. See incident #751 for a historical example.

- **`exit status 125`** — the `podman container create` (or `podman run`) command failed at the OS level. The actual cause is on the preceding line(s) in the log — read upward from this line to find it.

- **`container did not become ready within 120s`** — the container started but the sidecar never reached the ready state. Usual causes: a misconfigured `harness-config.json` (agent not declared, malformed JSON), a missing bind-mount, or a missing `--agent` flag value. Check the sidecar log for the `podman run` command line and any JSON parse errors.

- **Session rows present in `prism sessions list` but no events in `prism checkin`** — the container either never started or died immediately after creation. Run `prism logs <session>` to see the full podman command line and its stderr output.

- **Session name doesn't match expected shape** (e.g. `~review` where `~review-1-review-code` is expected) — the agent-list construction produced the wrong agent names, or the `--agent` flag value is incorrect. Check the container's `harness-config.json` for the `agent` block contents and the sidecar log for the `--agent` flag value used in the command line.

- **Zombie DB rows (session in `prism sessions list` but no live tmux session)** — a previous session's process died without cleaning up DB state. Use `prism cleanup --yes --session <name>` to mark the stale row ended (it drops out of `prism sessions list`) and release any dangling port allocation. The row is not deleted; re-spawning on the same branch name re-seeds it to `idle`.

### Escalation

If two diagnostic cycles (`prism checkin` + `prism logs`) do not clarify the issue, **escalate to the user** rather than continuing to probe in circles. Document what you observed in each cycle and what remains unclear. Do not run a third diagnostic cycle on your own.

### `prism feedback` — record CLI friction

Use `prism feedback` to record short notes about CLI rough edges — flags rejected for the wrong reason, error messages that don't enumerate, race conditions in async paths. Each entry is appended as one JSON object per line to `$XDG_STATE_HOME/prism/feedback.jsonl` (defaults to `~/.local/state/prism/feedback.jsonl`).

```bash
prism feedback "the --tier flag rejects 'enterprise' but the docs list it as valid"
echo "feedback from a script" | prism feedback -        # read from stdin
prism feedback list                                       # human-readable list
prism feedback list --json                                # JSON array of all entries
prism feedback list --days 7                              # only entries from the last 7 days
prism feedback prune --days 90 --yes                      # drop entries older than 90 days
```

Each entry includes `timestamp` (RFC 3339), `text`, `session` (the value of `PRISM_SESSION_NAME` if set), and `prism_version`. Optional fields cover `cwd` and other contextual hints.

**Upstream POST (opt-in).** When the `PRISM_FEEDBACK_ENDPOINT` environment variable is set (or a `feedback_endpoint` field is present in `~/.config/prism/config.json`), each new entry is POSTed to the configured URL after being written locally. The local record is the source of truth: if the upstream POST fails, the local entry is unaffected and the failure is reported in the success message. Env var wins over config.

**Pruning safety (principle 1).** `prism feedback prune` requires `--yes` to confirm — omitting it errors instead of prompting. This matches the rest of the prism CLI's no-implicit-confirmations stance.
