---
name: prism
description: Spawn isolated agent sessions in their own git worktrees using the prism tool. Use when the user asks to spawn an agent, delegate work to another session, run something in parallel, or work on a PR or different repo.
---

> **Note:** The prism source code and this skill file live in the `nixos-config` repository under `modules/programs/prism/`. Changes to prism itself — the Go CLI, tmux config, agents, and skills — are made there.

## Introspection layers

Prism provides three introspection layers, ordered from most to least human-readable:

1. **`--help` text** — human-readable usage, intended for humans at a terminal.
2. **`prism agent-context`** — machine-readable JSON describing the full CLI shape: every command, every flag (with type, enum values, defaults and default sources), available profiles, and cross-cutting precedence rules. **This is the layer-2 surface agents must consult when they need to discover available flags, enum values, or profile names programmatically.** It emits valid JSON to stdout and exits 0.
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

`prism spawn` creates a new git worktree, starts a tmux session in it, and launches the agent harness (pi). Use it when work needs isolation, is long-running, or is in a different repo — rather than a subagent, which shares the current session context.

## When to use prism spawn vs a subagent

| Situation | Use |
|---|---|
| User says "spawn an agent" or "use prism" | `prism spawn` |
| Work is on a PR branch | `prism spawn --pr` or `prism pr` |
| Task is long-running / outlives this session | `prism spawn` |
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
| `--branch <name>` | Branch name for the new worktree. Defaults to a timestamp. Use a short, descriptive kebab-case name derived from the task (e.g. `update-plex-image`, `fix-login-redirect`) — never an issue number, PR number, or Jira ID. The branch name becomes the session name (e.g. `nixos-config@update-plex-image`), so it must be immediately readable in `prism sessions list` and the tmux picker without looking anything up. |
| `--pr <number>` | Fetch and check out the branch for this PR number. |
| `--prompt <text>` | Instruction passed to the agent on launch. Wrap values containing shell metacharacters in **single quotes**. The value `-` is reserved and reads from stdin (cannot pass a literal `-`). |
| `--prompt-file <path>` | Read the prompt from a file instead of passing it as an argument. Mutually exclusive with `--prompt`. A single trailing newline is stripped. |
| `--agent <name>` | Agent to use (`worker` or `plan`). Defaults to `worker`. |
| `--attach` | Switch the current tmux client to the new session instead of spawning headlessly. |
| `--reuse` | If an active session already exists on the requested branch, return its name/port/agent and exit 0 instead of failing. Without `--reuse`, spawning onto an existing feature branch exits non-zero and tells you to run `prism cleanup` or pass `--reuse`. For `--branch main`, reuse semantics are the default — the flag is accepted as a harmless no-op. |

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
4. **Not found:** start the coordinator yourself with `prism spawn --repo <repo> --branch main --prompt '...'`. On `--branch main` prism spawn defaults to reuse semantics, so this is idempotent: if a healthy `<repo>@main` already exists the prompt is delivered to it; if not, a detached coordinator is started on the existing main worktree and the prompt is delivered on launch. There is no need to pre-spawn and then prompt.

Only spawn directly into a feature branch in another repo (bypassing the coordinator) when you **are** the coordinator for that repo, or when the user explicitly instructs you to.

```bash
# Check if the target repo has a coordinator session
prism sessions list

# If home-ops@main exists and is not waiting:
prism prompt home-ops@main --prompt 'Please update the plex image to the latest tag and open a PR'

# If home-ops@main exists but IS in waiting state:
# escalate to the user — they need to switch to that session and unblock it

# If home-ops@main does not exist:
# one shot: ensure the coordinator exists, then tell it the task.
prism spawn --repo home-ops --branch main \
  --prompt 'Please update the plex image to the latest tag and open a PR'
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
> coordinator must use `prism pr <number> --prompt 'review this PR'` to spawn
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

The review-complete prompt includes a one-line summary header followed by a
`## Per-agent findings` section with structured fields: verdict, extracted
`<summary>` content, and extracted `<blocking_issues>` content. No file is
written to `/tmp` — use `prism checkin <session>~review-<N>-<agent>` to read
the full agent reasoning if needed, where `<session>` is your own session
name. That read is in scope for a worker; every other target is not — see
[Who can check in on what](#who-can-check-in-on-what). All 5 agents must
pass. On FAIL, fix every
blocking issue, commit, push, and re-run per the targeted-rerun condition in
`worker.md` — a fix in one area can create issues in another. After 3 full
review cycles without convergence, stop and escalate to the coordinator via
`prism escalate`; do not run a 4th cycle.

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

### Pre-flight rebase gate (`prism review` refuses when behind the PR's base branch)

Before spawning any review agents, `prism review` runs a **one-shot pre-flight
check** against the PR's actual base ref (resolved via
`gh pr view <pr> --json baseRefName`; falls back silently to `origin/main`
when the lookup fails or no PR is discoverable — #2304):

1. `git fetch origin <base>` (one network round-trip).
2. Strict ancestor check: `git merge-base --is-ancestor origin/<base> HEAD`.
3. If `origin/<base>` is an ancestor of `HEAD`: proceed unchanged.
4. If not: refuse, exit non-zero, **no agents spawn**, and **no cycle counter
   increment**. The error message names the number of commits behind and the
   recommended fix. For a PR targeting `main` the rendered message looks
   like:

   ```
   prism review: branch is N commits behind origin/main

       git fetch origin main
       git rebase origin/main
       git push --force-with-lease

   Or rerun with --rebase to do this inline.
   ```

   For a PR targeting (say) `eks-pipeline`, the `main` references are
   substituted with the resolved base ref:

   ```
   prism review: branch is N commits behind origin/eks-pipeline

       git fetch origin eks-pipeline
       git rebase origin/eks-pipeline
       git push --force-with-lease
   ```

The `--rebase` flag is the inline opt-in fix:

```bash
prism review <pr> --rebase
```

It performs the fetch + rebase + force-push inline against the resolved base
ref (not hardcoded `origin/main`) and then proceeds to the review against the
rebased HEAD. If the rebase produces conflicts, the rebase is aborted, the
worktree is restored to the original `HEAD`, and the command exits non-zero —
never leaves the worktree mid-rebase. Resolve conflicts manually and re-run.

**Why this gate exists.** Reviewers regularly produce noisy findings of the
form "you should also update X" when X landed on the base branch after the
feature branch was cut. A simple rebase makes the diff smaller and the
finding disappear, but discovering this from a FAIL verdict burns a full
5-agent cycle. The gate catches drift in one fetch, before any agent spawns.

**Cycle-counter contract.** Gate failures (behind-base refusal, fetch failure,
missing `origin/<base>`, rebase conflict abort) **do not increment** the
review-cycle counter. They are the same category as "round already in
progress" / pure-infrastructure failures / ran-but-no-parseable-verdict
rounds (#1995): no full set of parseable verdicts was produced, so the
round does not count. A worker that hits the gate three times in a row
and then runs three real reviews still has all three real cycles
available before the LOOP-LIMIT footer fires.

Rounds that **do not count** toward the 3-cycle limit:

- **Pre-flight gate refusals** — behind-base, fetch failure, missing
  `origin/<base>`, rebase conflict abort. No agents spawned.
- **Round-already-in-progress refusals** — a prior review is still
  active for the same parent. No agents spawned.
- **Pure-infrastructure failures** — every agent failed to start
  (no frames received — e.g. the container never bound its port) and/or
  stalled mid-run (the agent ran, then stopped producing frames — #2239).
  Header mentions "infrastructure failure".
- **Incomplete rounds** (#2573) — ANY agent produced no verdict, even when
  the other four did. Header says **"Round incomplete: N of M review
  agents produced a verdict"** and the report carries an **"Agents with
  no verdict"** section. Re-run the named agents; the round does not
  count.
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

### Pre-flight formatter gate (`prism review` refuses on unformatted Go/Nix files)

After the rebase gate passes, `prism review` runs a second one-shot pre-flight
check, in the same shape: it diffs the touched files against the resolved base
ref (the same `<remote>/<branch>` the rebase gate just fetched and verified),
and runs the relevant formatter against any touched Go or Nix files:

1. Touched `.go` files → `gofmt -l <files>`.
2. Touched `.nix` files → `nixfmt --check <files>`.
3. If everything checked is clean (or no Go/Nix files were touched): proceed
   unchanged.
4. If not: refuse, exit non-zero, **no agents spawn**, and **no cycle counter
   increment**. The error names the offending files and the exact fix command:

   ```
   prism review: formatting check failed — refusing to spawn review agents

   Go files not gofmt-clean:
     internal/foo/bar.go

   Fix with:

       gofmt -w internal/foo/bar.go

   Then commit, push, and re-run 'prism review <pr>'.
   ```

**Why this gate exists (#2556).** PR #2552 burned a review cycle when a
five-agent round discovered a missing newline at end of file — a `gofmt`
violation `gofmt -l .` reports for free in about a second, and one `pr-gate`
CI would have blocked anyway. Using an LLM review round to discover a
deterministic, CI-checkable defect wastes cycles against the 3-cycle limit
for no reliability gain — one agent out of five caught it; had none caught
it, CI would have blocked the merge regardless.

**Cycle-counter contract.** Same as the rebase gate: the formatter gate runs
before any review-agent session is spawned and before any DB rows are
written for the round, so a refusal here is structurally incapable of
incrementing the review-cycle counter (`NextRoundNumber` counts per-agent
session rows the gate never creates). Add formatter-gate refusals to the
"rounds that do not count" list above — the correct action is to run the
printed fix command, commit, push, and re-run `prism review`, not to
escalate.

**Fail-open on a missing formatter binary.** If `gofmt` or `nixfmt` is not on
`PATH`, that language's check is skipped with a progress warning rather than
blocking the review. A review that cannot run because a tool is absent is
worse than a review that runs without the gate — this is deliberate, not a
gap to harden.

**Same gate in host-direct and container-routed paths.** Like the rebase
gate, this runs inside the host-side `prism review` subprocess regardless of
which path invoked it, so a container worker sees the identical refusal.

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

### Handling mid-run stalls in review-complete prompts

A **mid-run stall** is the sibling failure class to a no-start (#2239): the
agent started and did real work — inbound frames flowed — then went silent
long enough to trip the inactivity watchdog. The report distinguishes it
from a no-start so you can tell "never ran" from "ran, then wedged".

Treat a stall the same way you treat a no-start: it is an **infrastructure
failure to re-run**, not a code-quality FAIL, and the round does **not**
count toward the 3-cycle limit. One caveat: repeated stalls under concurrent
load can indicate provider rate/subscription limits — if the same agent
stalls across multiple consecutive rounds, escalate to the coordinator
instead of burning further rounds on blind re-runs.

Signs of a mid-run stall:
- `**Verdict:** ERROR`
- Output contains `ERROR: agent stalled mid-run after <elapsed> (<n>
  frame(s) received, last at <t>)`
- The delivery message header says "stalled mid-run" and mentions
  "infrastructure failure"

### Handling an incomplete round (an agent produced no verdict)

A round is **incomplete** when any of the five agents produced no verdict —
whatever the cause: it never started, it stalled, it crashed, or its session
was closed mid-round. The remaining verdicts are NOT the result of the
round: the missing agent's dimension was never examined (#2573).

The most dangerous shape is four PASS plus one blank, because it reads as
"four passed". It is not. Read a missing verdict as unreviewed, never as a
pass.

Signs of an incomplete round:
- The header says **"Round incomplete: N of M review agents produced a
  verdict"**
- The report carries an **"Agents with no verdict"** section that names each
  affected agent, its class, and the reason recorded for it
- The section ends with the re-run command to use — see the two cases below

What to do:

1. Fix any blocking issues the agents that DID run reported.
2. Re-run with the command the report prints. Which command that is depends
   on the round, and the report applies the rule for you:
   - **An agent that ran returned FAIL** — the report prints the FULL re-run
     (`prism review <pr>`) and refuses the targeted form. Your fix changes
     the code the other agents reviewed, so their verdicts are stale.
   - **No agent returned FAIL** — the report prints the targeted command
     (`prism review <pr> --only <agents>`) with the caveat that it holds only
     while you push nothing else. Push any change beyond formatter output,
     comments, or documentation and you must re-run the full set instead.
     This is the targeted-rerun condition (#2530 / #2557) stated in the
     worker agent instructions; the report cannot evaluate it on its own,
     because it cannot see your inter-cycle diff.
3. The round does **not** count toward the 3-cycle limit — the report says
   so explicitly.

One class needs a different response: **"session ended mid-review"** means
the agent's `agent_status` row was closed while the round was running (the
tmux session ended, the harness deleted the session, or a cleanup path ran).
The report names the state the row was left in and the time it closed. If
the same agent is reaped in two consecutive rounds, escalate to the
coordinator rather than re-running a third time — a repeatable reap is a
platform fault, not a flake.

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

`<session>` is the session that ran `prism review`. A worker may read the review agents of its own session and nothing else — see [Who can check in on what](#who-can-check-in-on-what).

## Investigator agents

Use `prism investigate` to spawn a read-only research session from within a prism session. Investigators are well-suited to tasks like tracing call chains, mapping symptoms to a file:line, or surveying scope before spawning a worker. Only coordinators can spawn them. A sandboxed caller meets the host-API `/investigate` endpoint, which calls `requireCoordinator` and returns HTTP 403 to every other caller. A `host`-mode caller has no socket and takes the direct CLI path, where `requireInvestigateCoordinator` in `cmd/investigate.go` refuses it with a non-zero exit.

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

`prism investigate` must be run from within a prism session (errors if no invoker is detectable). Only coordinators can use it. There are two enforcement points, one per route. A sandboxed caller meets the host-API role gate — `requireCoordinator` on `/investigate` in `internal/sidecar/host_api.go` — which answers a worker with HTTP 403. A `host`-mode caller has no socket, so it meets the direct-CLI guard — `requireInvestigateCoordinator` in `cmd/investigate.go` — which refuses with a non-zero exit. Neither is a bash deny list: no entry in `BLOCKED_BASH_PATTERNS` (`pi/extensions/prism.ts`) matches any prism verb.

---

## Merge queue (coordinators only)

> **Coordinators only.** The whole verb family is coordinator-only: the host-API `/merge`, `/merges`, and `/merges/cancel` endpoints each call `requireCoordinator`, so `prism merge`, `prism merges list`, and `prism merges cancel` return HTTP 403 for worker agents, container worker agents, bwrap worker agents, and review agents alike. If you are not a coordinator agent, skip this section.

The merge queue is a local serial FIFO queue running in the coordinator's sidecar process. The sidecar polls the head of the queue every 30 seconds; only one PR is in flight at a time. The watcher's lifetime equals the coordinator session's lifetime — there is no persistent daemon.

`prism merge <pr>` is **async, poll-and-notify** (issue #2420, same shape as `prism review`): the command emits a synchronous initial-state message describing the detected state and what will happen next, then — for non-terminal states — hands off to the background poller. When the outcome is decided, the watcher delivers a `prism prompt` to the coordinator. For terminal states at invocation (already merged, closed without merge, or merge conflict) the command exits immediately without enqueueing.

**Core rule (#2420):** `prism merge` NEVER squash-merges without a positive signal. On a repo with no branch protection at all, the watcher waits for a human to review, approve, or merge — it does not auto-merge just because GitHub reports the PR as mergeable. Branch-protection presence is a description of the required gates, not a licence.

A queued PR moves through states keyed off GitHub's `mergeStateStatus`: `watching` → `merged` / `failed` / `cancelled` / `abandoned`. See issue #783 for the original state machine and issue #2420 for the current async / no-silent-auto-merge redesign.

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

Every command in this table is coordinator-only on both routes — see [Why workers cannot invoke it](#why-workers-cannot-invoke-it) for the two enforcement points.

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
- Prefer the **notification path** (no `--wait`) when you have **other tasks in flight**. Coordinators with several PRs in flight must never `--wait` — `prism merge <pr>` returns immediately and the merge-queue watcher delivers a notification when each PR lands. Same shape for `prism review` and `prism spawn`.
- Add `--json` when scripting: `prism merge <pr> --wait --json` emits a single JSON object on stdout (no human-readable chatter) so consumers can `jq` the status.

**Idempotent observation.** `prism merge <pr> --wait` on an already-merged PR returns immediately with the merged status — safe to call any number of times.

**Ctrl-C semantic.** Killing a `--wait` invocation (Ctrl-C, SIGTERM) interrupts the **local** wait only — the underlying merge / review / spawn keeps running. To recover the result later, re-run the same command (or `prism merges list` / `prism reviews list` / `prism checkin <session>`).

**Inside a sandbox.** `--wait` works identically inside and outside a bwrap / sandbox-exec sandbox. The CLI auto-detects `PRISM_HOST_API` and routes its terminal-state probes through the sidecar's read-only wait endpoints, so the host's prism.db is the source of truth in either case.

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

### Invocation-time state-table messages (#2420)

At invocation, `prism merge <pr>` emits a synchronous message that names the detected state and — for non-terminal states — what the poller will do next. The full state table:

| Detected state | Synchronous message | Then |
|---|---|---|
| PR already merged (out-of-band) | `PR #N already merged. Please clean up the branch and worktree.` | Exit; no poller starts. |
| PR closed without merge | `PR #N closed without merge. No action required from you; a human closed this. Please clean up the branch and worktree.` | Exit; no poller starts. |
| Merge conflict | `PR #N has conflicts. Worker needs to rebase.` | Exit non-zero; no poller starts. |
| Branch protection configured, all gates green | `PR #N ready. Merging now.` | Enqueue; watcher squash-merges on next tick, then notifies success. |
| Branch protection configured, required checks pending | `PR #N waiting on N check(s): [names]. Standing by; will merge when green. No action required from you.` | Enqueue; watcher polls. |
| Branch protection configured, awaiting approval | `PR #N requires human approval. No action required from you — do not request reviewers, do not add approvers, just wait. Will merge automatically when approved and checks pass, or notify if merged out-of-band.` | Enqueue; watcher polls. |
| NO branch protection configured | `PR #N has no branch protection configured. Not auto-merging. Waiting for a human to review and either approve the PR or merge it themselves. No action required from you.` | Enqueue; watcher polls silently and NEVER auto-merges. |

### Poll-time notification contract

During polling, the watcher fires a coordinator notification only for the terminal events below. Every other observed state — new commits pushed, review-changes-requested, checks still running, and unprotected-repo waits — results in a silent continuation. The initial-invocation message already told the coordinator what to wait for.

One CI outcome is terminal (issue #2525). When `mergeStateStatus` is `BLOCKED`, every required check has concluded, and at least one concluded in a failure state (`FAILURE`, `TIMED_OUT`, `CANCELLED`, `ACTION_REQUIRED`), nothing resolves the PR without a new push. The watcher terminates the row and notifies. A failing check outside the repo's required-checks list does NOT trigger this, and neither does a required check that is still queued, still running, or absent from `statusCheckRollup`.

| Outcome (during polling) | Notification text |
|---|---|
| Prism-driven merge succeeded (with archive path) | `PR #N merged. Archive: <archive_path>. Please clean up the branch and worktree.` |
| Prism-driven merge succeeded (no archive path yet) | `PR #N merged. Please clean up the branch and worktree.` |
| Reconciled: gh mutation errored but PR is MERGED (with archive path) | `PR #N merged. (Reconciled — gh mutation errored but PR is MERGED.) Archive: <archive_path>. Please clean up the branch and worktree.` |
| Reconciled: gh mutation errored but PR is MERGED (no archive path yet) | `PR #N merged. (Reconciled — gh mutation errored but PR is MERGED.) Please clean up the branch and worktree.` |
| Out-of-band merge (merger named) | `PR #N merged out-of-band (merged by @<login> at <mergedAt>). Please clean up the branch and worktree.` |
| Out-of-band merge (merger unknown) | `PR #N merged out-of-band. Please clean up the branch and worktree.` |
| PR closed without merging (mid-poll) | `PR #N closed without merge. Please clean up the branch and worktree.` |
| BLOCKED, required check concluded in failure | `PR #N CI failed: <check names>. Worker needs to fix and push. No merge will happen until then.` |
| Genuine `gh pr merge` failure (rare fallback) | `PR #N merge failed: <error>` |
| Coordinator session ended while watching | Row transitions to `abandoned` — surfaces via `prism merges list --abandoned` only; no live notification. |

Every *completion* notification contains the exact phrase **"Please clean up the branch and worktree"** and does NOT imply prism performed the cleanup itself — the coordinator does the cleanup, prism does not.

The CI-failure notification deliberately omits that phrase. Nothing merged and nothing closed, so the branch and worktree must survive for the worker to push the fix.

### Action table

When a merge-queue notification arrives, treat it as high-priority (same as a worker finished-notification):

| Notification | Action |
|---|---|
| `PR #N merged. ...` (prism-driven or reconciled) | `git pull` in @main, then `prism cleanup --yes --session <worker-session>` |
| `PR #N merged out-of-band. ...` | `git pull` in @main, then `prism cleanup --yes --session <worker-session>`. Prism did NOT perform the merge, but the branch/worktree are still yours to clean up. |
| `PR #N closed without merge. ...` | `prism cleanup --yes --session <worker-session>`. Usually nothing else — the PR was closed deliberately. Investigate if unexpected. |
| `PR #N CI failed: <check names>. ...` | Do NOT clean up — the branch is still needed. `prism prompt <worker-session>` with the failed check names so the worker fixes and pushes. Re-run `prism merge <pr>` after the fix lands; the row is terminal, so it does not resume on its own. |
| `PR #N merge failed: <error>` | Read the error, decide whether to retry (`prism merge <pr>`) or escalate to the user. |
| `abandoned` (via `--abandoned` listing) | A new coordinator decides whether to re-enqueue with `prism merge <pr>`. |

### Why workers cannot invoke it

The host-API role gate refuses every non-coordinator session — worker agents, container worker agents, bwrap worker agents, and review agents. `/merge`, `/merges`, and `/merges/cancel` each call `requireCoordinator` (`internal/sidecar/host_api.go`), which answers HTTP 403 with `workers cannot perform merge`. A session that runs outside a sandbox has no socket to route through, so each verb carries a second coordinator guard on its direct CLI path: the coordinator-only branch in `cmd/merge.go` for `prism merge`, and `requireMergesCoordinator` in `cmd/merges.go` for both `prism merges list` and `prism merges cancel` (issue #2608). The read-only list verb is guarded as well, so the role boundary does not change with the caller's isolation mode. This is by security design: only coordinators arbitrate merge order. Do not attempt to work around either check.

The `prism merge` verb is gated at the host API, not by a bash deny list — no entry in `BLOCKED_BASH_PATTERNS` (`pi/extensions/prism.ts`) matches any prism verb. Audit that half of the boundary in `host_api.go`.

The worker→merge boundary has a second enforcement point. `BLOCKED_BASH_PATTERNS` blocks `gh pr merge` and `gh pr review --approve` / `-a` / `--request-changes` / `-r` for worker-class roles (entries `gh-pr-merge` and `gh-pr-review-approve`, issue #2410), which closes the `gh` bypass of the same separation. Plain `gh pr review` and `gh pr review --comment` stay allowed. So: the deny list holds no prism verb, but it does hold the two `gh` complements to this boundary.

The other four entries — `git worktree prune`, `git worktree remove`, `nix build` with an env override, `git stash` — guard a different class of hazard: damage that reaches past the caller's own view. Sibling sessions' worktrees are not bind-mounted into your sandbox, the FD pool is host-wide, and the stash stack lives in the shared bare repo, so it is repo-wide rather than per-worktree (#2202). None of the four is about the coordinator/worker role boundary.

Role scoping is a separate axis from that split: **three** of the six entries carry `appliesToRole: isWorkerClassRole` — the two `gh` entries and `git-stash`. The coordinator is exempt from the stash block because, with every worker-class session denied, it is then the only prism writer to the shared stack. The `git worktree` and `nix build` entries are unscoped: their hazards apply to the coordinator too.

### `git worktree list` reports live sibling worktrees as `prunable` — this is not data loss

A coordinator's sandbox mounts only `.bare` and the coordinator's own
worktree. Sibling worktrees — the worktrees of workers and review agents
running in the same repo — are not bind-mounted in. When you run `git
worktree list` inside the sandbox, `git` resolves each sibling's gitdir
pointer, finds nothing at that path, and reports it as `prunable` (or, in
`--porcelain` output, `prunable gitdir file points to non-existent
location`).

That report describes what `git` can see inside the sandbox. It does not
describe the host. The worktree can be live, mounted, and in active use by
its own session at that exact moment.

**Do not treat a `prunable` sibling worktree as evidence of data loss, and do
not run `git worktree prune` or `git worktree remove` in response.** Both
commands are already blocked for every role by `BLOCKED_BASH_PATTERNS` (see
above), so the command itself will fail — but do not let the false read lead
you to escalate a non-existent incident or instruct a worker to redo work
that was never lost.

To check whether a sibling worktree is live, use `prism sessions list`
instead of `git worktree list`. `prism sessions list` reads prism's own
session state, not the sandbox's filesystem view, so it reports the true
status of every session in the repo, mounted or not.

A worktree that is genuinely stale — its session closed or cleaned up — is
still distinguishable: `prism sessions list` will not show a live session for
it at all, where a merely-unmounted worktree still has a live session entry.
`prunable` from `git` answers only "is this mounted here"; `prism sessions
list` answers "is this session alive". Use the second question to decide
whether a worktree needs cleanup.

See also the "Worktree tracking and the health check" section in `coordinator.md` (`modules/programs/prism/agents/coordinator.md`) — the same guidance is stated there for the coordinator's monitoring flow, and the two must not contradict each other.

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
3. Spawn the agent — pass the ticket ID in the prompt so the agent can look it up. The Atlassian MCP is reachable in the spawned session, but a worker registers only `activate_atlassian` and must call it before its first Jira operation (issue #2532); `agents/worker.md` and the `atlassian` skill both spell this out, so no extra prompt wording is needed.

```bash
# After creating ticket PROJ-123 ("Update plex image to latest tag"):
prism spawn \
  --branch update-plex-image \
  --prompt "Please take a look at PROJ-123, cover off the work required, and open a pull request."
```

## Lifecycle: cleaning up after a merge

When you spawn a session and later merge its PR yourself, you are responsible for cleaning up the worktree and session. The spawned agent cannot do this — doing so tears down its own environment.

`prism spawn` prints the session name when running headlessly:

```
session "nixos-config@update-plex" created
```

Note down the session name from that output. Two commands tear down a session, with different defaults:

| Command | Default behaviour | When to use |
|---|---|---|
| `prism close --yes --session <name>` | Smart-decide: soft-close if an open PR exists for the branch, otherwise hard-cleanup. | Default. Safe for parked WIP branches and merged work alike. Bound to `prefix+q`. |
| `prism cleanup --yes --session <name>` | Always destructive (remove worktree + force-delete branch). | Scripted coordinator workflows that know the work is finished and want a guaranteed hard cleanup. |

### `prism close` — smart-decide

```bash
prism close --yes --session "nixos-config@update-plex"
```

Decision tree (issue #2179):

1. Coordinator session (`root_agent_name == "coordinator"` or `@main`) → soft close.
2. Non-worktree session (no `@`) → soft close.
3. Worker worktree session: probe `gh pr list --head <branch>`:
   - any PR is OPEN → soft close (preserve worktree + branch)
   - all PRs MERGED/CLOSED → hard cleanup
   - no PR found → hard cleanup
   - probe error / timeout / `gh` missing / unauthenticated → soft close (fail-safe)

The fail-safe is deliberate: a spurious soft close costs one extra `prism close --remove-worktree` later; a spurious hard cleanup destroys uncommitted work. The probe is bounded by a 5-second context timeout so a hung GitHub API cannot wedge the tmux popup.

Force flags override the decision (mutually exclusive):

- `--keep-worktree` — always soft close (paranoid mode for long-lived WIP branches).
- `--remove-worktree` — always hard cleanup ("I'm done with this branch").

`prism close --yes` writes nothing to stdout/stderr on the happy path, making it safe to bind to a tmux popup. The `--json` envelope is identical to `prism cleanup`'s and is still emitted when `--json` is passed.

**Soft close preserves pi conversation history (issue #2371).** The transcript JSONL in the pi sessions root (`$PI_CODING_AGENT_DIR/sessions/<encoded-cwd>/` when the env var is set, else `~/.pi/agent/sessions/<encoded-cwd>/`) is what pi's interactive `/resume` reads — a soft close never deletes it, so a later session on the same worktree can `/resume` the old conversation. The DB resume pointer (`agent_status.harness_session_id`) and any buffered `pending_replay_deliveries` are still cleared, unconditionally — even when the archive step fails — so a re-spawn on the same session name always starts a fresh conversation (#2035); returning to the old one is an explicit operator action via `/resume`. (The DB clear alone carries the #2035 defence: spawn only passes `--session <id>` when the DB value is non-empty. Empirically confirmed by pi's mid-session transcript rollover — stale rollover JSONLs survive every close and have never caused a dud auto-resume.) Hard cleanup still archives and then removes the transcript from the sessions root, gated on the archive actually preserving a copy (#2336).

### `prism cleanup` — always destructive

```bash
prism cleanup --yes --session "nixos-config@update-plex"
```

`prism cleanup --yes --session <name>` will:
- Remove the git worktree
- Force-delete the local branch (relies on the orchestrator-trust contract: call this only after confirming the PR is merged)
- Kill the tmux session, redirecting any attached client to `scratchpad`
- Mark the `agent_status` row as ended (stamps `ended_at`, releases the harness port, and clears the pi `harness_session_id` so the next spawn starts a fresh conversation)

For parity with `prism close`, `prism cleanup` also accepts `--keep-worktree`, which downgrades it to a soft close even on a worker session (transcript preserved — see `prism close` above). Without that flag the command keeps its pre-#2179 always-destructive default, so scripted coordinator workflows that call `prism cleanup --yes --session <X>` after a merge are unaffected.

The `agent_status` row itself is preserved — it is not deleted. Re-spawning on the same branch name reuses the row: `tmux-session-start` re-seeds it to `idle`, which the state machine accepts from any non-`deleted` terminal state (`error`, `finished`, `interrupted`). Long-term retention is handled by the 90-day `Prune` job.

The `--json` envelope reports per-resource outcomes so you can verify the bookkeeping ran:

```json
{
  "session": "nixos-config@update-plex",
  "worktree_removed": "/code/nixos-config/update-plex",
  "branch_deleted": "update-plex",
  "session_killed": true,
  "ended_at_stamped": true,
  "harness_port_released": true,
  "harness_session_id_cleared": true
}
```

Each of `ended_at_stamped`, `harness_port_released`, `harness_session_id_cleared` is `true` on success (or idempotent no-op — the row is in the cleaned-up state), or a string describing the failure on error. On **hard cleanups**, `harness_session_id_cleared` has two extra skip-report value classes: `"skipped: archive failed: <err>"` when the resume-linkage sever did not run because the session-archive step failed (#2219), and `"skipped: transcript missing"` when the archive step ran but the harness adapter reported no transcript was actually copied — the manifest-only case where the on-disk transcript pi expected to archive was missing or unlocatable (#2336). In both classes the hard sever is skipped intentionally so the on-disk transcript and the resume pointer are left intact for a later cleanup re-run; the hard sever runs after the archive (it deletes the same transcript JSONL the archive copies) and only fires when the adapter reports `copied == true`. On **soft closes** the sever is DB-only and unconditional (#2371): the transcript is never deleted, so there is nothing for those gates to protect, and `harness_session_id_cleared` reports `true` whenever the DB clear ran — even when the archive step failed (the archive failure still surfaces as a stderr warning). If the DB cannot be opened, all three carry the same failure description so the operator can tell the bookkeeping was NOT attempted. Re-running cleanup on an already-ended session is idempotent: `ended_at_stamped` and `harness_port_released` still report `true`, the row stays ended, and the command exits 0 — though when the first run already wrote the archive, a hard re-run reports `harness_session_id_cleared` as `"skipped: archive failed: …"` (the archive directory already exists) while a soft re-close reports `true` (the no-op DB clear ran again); nothing is lost or re-deleted either way.

Only call this after you have confirmed the PR is merged. The `--yes` path always force-deletes the branch — it does not check whether the branch is reachable from main, because squash-merges produce a different SHA on main than the branch tip.

Pi sessions block `git worktree prune` and `git worktree remove` at the extension layer. When recovering from a failed spawn, use `prism cleanup` — do not reach for git plumbing.

## A/B-test workflow: `prism spawn --abtest`, `prism stats compare`, merge decision, cleanup

An A/B test spawns two sibling sessions on the same prompt with different model profiles (or other configuration), lets both run to completion, then compares the two outcomes to decide which one to merge.

The workflow is:

1. **Spawn the pair.** `prism spawn --abtest <profile-a>,<profile-b> --prompt '<the prompt>'` creates two sessions that share a single `abtest_pair_id` in `spawn_inputs`. Each leg runs in its own worktree on its own branch and opens its own PR when it finishes.
2. **Wait for both workers to finish.** Each session lands in a terminal state (`finished`, `error`, or `interrupted`). You can watch them in `prism sessions list`; both legs will surface terminal-state notifications via the usual coordinator-notification surface (see *Worker terminal-state notifications* below).
3. **Run `prism stats compare` for the merge-decision data.** Once both legs have transitioned to terminal state — *before* you merge either PR — compare them:

   ```bash
   prism stats compare <instance-id-A> <instance-id-B>
   # or, if you minted the pair via --abtest, by the shared pair id:
   prism stats abtest <pair-id>
   # machine-readable when scripting the winner decision:
   prism stats compare <instance-id-A> <instance-id-B> --json | jq .
   ```

   The output carries the `Spawn Inputs` block (profile, harness, isolation, agent role, branch, abtest_pair_id) plus per-axis aggregates (tokens, cost, tool calls, durations, end_state). The aggregates are available *between* terminal-state transition and `prism cleanup` — they no longer require cleanup to materialise (issue #2102). Use the cost / duration / msg_assistant axes alongside the quality of the produced PRs to pick a winner.
4. **Merge the winner, close the loser.** Standard merge / close flow on the two PRs.
5. **Cleanup both sessions.** `prism cleanup --yes --session <winner>` and `prism cleanup --yes --session <loser>`. Cleanup persists the `spawn_outcome` row for long-term querying via `prism stats --group-by profile|model|...`; until then the row is computed on the fly from `agent_events` whenever `prism stats compare` is run.

Notes:

- `prism stats compare` shows `—` for aggregate axes while a session is still in progress (state `active`, `idle`, or `reviewing`). The aggregates only stabilise at terminal transition.
- The `Spawn Inputs` block surfaces whatever the writer captured at spawn time. Pre-#2087 sessions can have a partial row — missing columns render as `—` rather than collapsing the whole block.
- Use `--json` (preferred) or the equivalent `--format json` for machine-readable output (e.g. when scripting the winner decision); the `spawn_inputs` object carries the same fields shown in the table. On error, both surfaces emit a single-line `{"error":"..."}` JSON envelope to stderr (no cobra usage dump) — script the failure path against the JSON contract too, not by parsing human-readable text (issue #2099).

## Querying prism state — prefer `--json` for scripting

Every list-style and lookup-style prism subcommand supports a `--json` flag that emits a single JSON document to stdout — keys are snake_case, timestamps are RFC 3339, empty lists are `[]` (never null, never absent), and any informational/progress text routes to stderr. **When you need to parse prism output programmatically, always pass `--json`.** Screen-scraping tabular human-readable output is fragile and burns tokens.

| Command | `--json` shape |
|---|---|
| `prism sessions list --json` | array of session-status objects |
| `prism sessions list --all --json` | array of session-status objects across all repos |
| `prism checkin <session> --json` | session + events object |
| `prism stats --json` (and `prism stats <id> --json`) | rows mirroring the host-API |
| `prism stats compare --json <runs...>` (alias for `--format json`) | `{runs:[...], diffs:{spawn_inputs:[...], spawn_outcome:[...]}}` |
| `prism stats abtest <group_id> --json` | same shape as `stats compare --json` |
| `prism stats --abtest --json` | `{pairs:[...]}` — abtest pair listing |
| `prism merges --json` (and `prism merges list --json`, `--failed --json`, `--abandoned --json`, `--all --json`) | array of merge-queue entries |
| `prism audit --json` | object with `events` array, `truncated` bool, optional `hint` |
| `prism sessions status --json` | object keyed by state (`active`, `waiting`, `idle`, `finished`, `error`) with integer counts (mutually exclusive with `--tmux-format`) |
| `prism profile list --json` | array of profile objects |
| `prism profile show [name] --json` | single profile object describing the slot table |
| `prism archive <session> --all --json` | array of archive-entry objects (instance_id, started_at, archive_path) |

## Querying the prism database directly: `prism db`

`prism db` is the read-only SQL and schema-introspection surface over
`prism.db` (issue #1467). Reach for it when the curated views (`prism
stats`, `prism checkin`) do not answer the question and you need raw SQL.

| Subcommand | Purpose |
|---|---|
| `prism db tables` | Print a sorted list of user table names (excludes `sqlite_*`). Use this first to discover the schema. |
| `prism db schema [table]` | Print `CREATE TABLE` / `CREATE INDEX` statements. With no argument, every table and index; with a table name, just that table's DDL. |
| `prism db query [SQL \| -]` | Run a single read-only SQL statement. Pass the SQL as an argument, or `-` to read it from stdin (useful for multi-line queries). Every write statement is rejected by SQLite's `?mode=ro`; only one statement per invocation is accepted. |

All three accept `--json` for structured output, and `db query` additionally
accepts `--timeout` (default 5s).

```bash
prism db tables
prism db schema spawn_outcome
prism db query "select session_name, tokens_input_total from spawn_outcome limit 5"
prism db query --json - <<'EOF'
select count(*) from sessions where repo = 'nixos-config'
EOF
```

**`prism db` works correctly inside a sandbox.** When `PRISM_HOST_API` is
set, every subcommand proxies through the host-API socket to the real
`prism.db` on the host, so results are identical inside and outside a
sandbox.

### The `sqlite3` sandbox trap — do not run `sqlite3` against the DB path directly

Inside a sandbox, running `sqlite3` directly against the database path
(e.g. `sqlite3 ~/.local/state/prism/prism.db "select * from sessions"`) does
**not** reach the host database. The sandboxed filesystem has no real data
at that path, so SQLite silently creates an empty shadow database there and
queries it. Every query returns **zero rows for every table** and
**no error at all** — a wrong answer that looks exactly like a right one
(an empty repo, or a session that does not exist).

Always use `prism db query` / `prism db schema` / `prism db tables` instead
of invoking `sqlite3` directly. They route through the host-API proxy from
inside a sandbox and read the real database; raw `sqlite3` does not.

## Retrospectives over a window of work: `prism retro`

`prism retro` reports a retrospective over the **trains** of work started in a
window (issue #2583). A train is one unit of work rolled up as a single row:

- a worker session plus its `~review-N-<agent>` children;
- a coordinator session plus the `~investigate-<slug>` sessions it spawned;
- a solo investigator (invoked by a worker), reported on its own, never folded
  into that worker's train;
- one leg of an A/B pair (`prism spawn --abtest`), never merged with its
  partner leg.

Train membership resolves through `session_groups.parent_session` (the durable
foreign key), not session-name parsing. `group_id` never surfaces — you type
session names, never a UUID.

| Flag | Effect |
|---|---|
| (none) | Last 24 hours, current repo, all trains. |
| `--since <date>` | Explicit window start (ISO 8601 or `YYYY-MM-DD`). Wins over `--days`. |
| `--days N` | Relative window: the last N days. |
| `--repo <name>` | Scope to another repo instead of the current one. |
| `--json` | Machine-readable form: snake_case keys, RFC 3339 timestamps, empty collections as `[]`. |

```bash
prism retro                     # last 24h, current repo
prism retro --days 7            # last 7 days
prism retro --since 2026-08-02  # explicit window start
prism retro --repo obsidian     # another repo
prism retro --json | jq .       # scriptable
```

The output has three sections:

- **Window totals** — output, input, cache-read and cache-write token volumes
  for the window, plus the **context-re-read share** (cache-read tokens as a
  percentage of total token volume). Token volume is the primary axis; cost is
  secondary and is `$0.00` under subscription profiles, never the only figure
  shown.
- **Trains** — one row per train: root session name, kind, profile tier,
  review-cycle count, total token volume, window share, and cost.
- **Waste signals** — `doom_loop_count`, `tool_error_count`,
  `permission_ask_count`, `permission_denied_count`, summed over the window.
  A window with sessions but no occurrences shows explicit zeros; a window
  whose sessions have no recorded `spawn_outcome` data shows **unavailable**,
  which is distinct from a recorded zero.

Token counts render as plain integers with thousands separators (e.g.
`11,297,191`), never in scientific notation. A NULL token field counts as
zero, and the output states that. An empty window prints an explicit message
and exits 0.

**`prism retro` works correctly inside a sandbox.** Like `prism db` and
`prism stats compare`, it proxies through the host-API socket when
`PRISM_HOST_API` is set, so it reads the real host `prism.db` and renders
identical output inside and outside a sandbox — never the empty shadow
database.

> Per-train review-cycle detail and fixed-overhead accounting are separate,
> later parts of the `prism retro` work (tracking issue #2529) and are not in
> this command yet.

## Checking in on a running session

Use `prism sessions list` to see all active agent sessions with their state and current task title:

```bash
prism sessions list          # human-readable table
prism sessions list --json   # JSON array (use this when scripting)
```

Use `prism checkin <session>` to read the recent conversation history for a session, sourced from the prism DB. The default output is a rich narrative view: assistant messages, state changes, and tool call one-liners interleaved chronologically. Pass `--json` when you need to parse the events programmatically. Which sessions you can read depends on your role — see [Who can check in on what](#who-can-check-in-on-what) below.

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

### Who can check in on what

`prism checkin` is scoped per caller. Three tiers apply (issue #2587), and both routes out of the verb are gated (issue #2619). A sandboxed session reaches the host-API `/checkin` endpoint and gets HTTP 403 for anything outside its tier. A `host`-mode session has no socket, so the CLI applies the same predicate against the DB directly and exits non-zero. The predicate is one shared copy, `authz.AuthorizeCheckin`, with the caller passed in.

| Your role | You can check in on |
|---|---|
| Worker, review agent, investigator, or any session whose role is not a coordinator | The review agents of your OWN session only — `<your-session>~review-<N>-<agent>`, for every round `<N>` |
| Coordinator | Every session in your own repo, plus the coordinator of another repo |
| Coordinator of a privileged repo | Every session in every repo, including another coordinator's workers and review agents |

Notes that matter in practice:

- **A worker cannot check in on itself.** `prism checkin <self>` is refused. The grant covers the review agents of your session, and you are not one of them.
- **The worker scope is DB-backed.** The predicate resolves the target through `session_groups.parent_session` and admits it only when that parent is the caller's session name. A name that merely looks like `<your-session>~review-1-review-code` is not enough: a review agent whose group row was deleted is refused.
- **Earlier rounds stay in scope.** Each round registers its own group row against the same parent, so round 1 is as readable as round 3.
- **Tier 3 is a coordinator with a troubleshooting privilege, not a superuser.** The privileged repos are declared in the prism NixOS module (`nx.programs.prism.checkin.privilegedRepos`, default `[ "nixos-config" ]`) and rendered to a file no sandbox can read or write. The privilege covers `prism checkin` alone — not `prism db query`, not `prism spawn`, not `prism merge` — and every access it admits writes an audit row.
- **Read the audit rows with `prism audit`.** The command works from a host shell and from inside a sandbox, and is coordinator-only on both routes (issue #2627). `$XDG_STATE_HOME/prism` is deliberately never bound into a sandbox, so a sandboxed caller cannot open the prism DB. When `PRISM_HOST_API` is set, `prism audit` proxies the read to the host API's `GET /audit` instead (issue #2618). That endpoint is coordinator-only, and it applies the `type = 'audit'` filter server-side, so it returns no `agent_events` row of any other type. A worker that runs `prism audit` inside a sandbox gets HTTP 403. A `host`-mode session has no socket and reads the DB directly; `requireAuditCoordinator` in `cmd/audit_permission.go` applies the same coordinator-only rule there and a refusal is a non-zero exit naming the caller, rather than an HTTP 403. `audit` is the fourth member of the both-routes-gated set that #2597 (`investigate`), #2608 (`merges`), and #2619 (`checkin`) belong to.
- **A `host`-mode session gets the same answer by a different path.** It has no socket, so the host API never sees the call; `authorizeDirectCheckin` in `cmd/checkin_permission.go` applies the same tiers and a refusal is a non-zero exit rather than an HTTP 403. A tier-3 read writes the same audit row, tagged `prism-cli` instead of `prism-host-api` (issue #2619). The gate is not a containment boundary — a host-mode session can read `prism.db` with `sqlite3` and bypass the verb — it exists for correct behaviour for a cooperative caller and route consistency.
- **The caller must have a session identity.** On the direct route the caller comes from `PRISM_SESSION_NAME`, else the current tmux session. `prism checkin` from a plain terminal outside tmux is refused with an error naming `PRISM_SESSION_NAME`, the same way `prism merges` refuses one.

If you need conversation history outside your tier, ask the coordinator with `prism escalate`. Do not attempt to work around the gate.

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

`prism prompt` will **refuse** to send a prompt if the target session is in `waiting` state. A `waiting` agent has paused and is expecting direct input from the user — injecting a programmatic prompt corrupts the input field.

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

Workers that hit a decision they cannot make alone (3-cycle review-limit reached, AC contradiction, scope ambiguity, infrastructure block) must use `prism escalate` rather than crafting a `prism prompt` to the coordinator by hand and stopping. The command resolves the right coordinator, delivers the message, transitions the calling session into a new `escalated` state, and emits a `session.escalated` bus event — in one step, with no redundant "has finished" notification.

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
escalated ──turn_start (after the escalating turn ends)──▶ active
```

- `escalated` is a new value alongside `active` / `idle` / `finished` / `reviewing`. It surfaces in `prism sessions list` so a glance shows which workers are paused awaiting guidance.
- **Same-turn frames do not clear it** (issue #2255): `prism escalate` runs as a bash tool call mid-turn, and the agent loop emits further `turn_start` frames before the escalating turn ends. The sidecar arms a same-turn guard when the escalate succeeds and suppresses those frames' state writes, so the rest of the escalating turn cannot clobber the `escalated` state. The guard releases when the turn fully ends (finished debounce), when a prompt is delivered to the session, or on a terminal exit.
- After the escalating turn has ended, the transition out is triggered by **any** incoming `turn_start`, not specifically `prism prompt`. A human who pokes at the worker via tmux clears the flag too.
- While in `escalated`, the sidecar suppresses the "has finished" notification — including for a `session_shutdown` terminal exit. The `session.escalated` bus event is the notification.

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
- The same payload is also written into the calling session's own event log as type `escalation`, so a `prism checkin` of the escalating session shows the escalation context inline. The escalating worker cannot read it that way — `prism checkin <self>` is refused (see [Who can check in on what](#who-can-check-in-on-what)) — the row is there for the coordinator and for the user.

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

In `--json` mode the human-readable line is NOT emitted on stdout (mutual exclusion); it can still be mirrored to stderr for log capture. On error, `--json` emits `{"error": "<message>"}` to stderr and exits non-zero.

The success signal reaches the caller identically from a direct-host invocation and from inside a bwrap / sandbox-exec sandbox: the sandbox path's sidecar proxy captures the host-side child's stdout and stderr separately and re-emits them on the matching local streams, so the `OK` line lands on the container's stdout (and the mirror on stderr) byte-for-byte the same as a host invocation. `--json` is forwarded to the host child via the proxy request body, so the JSON envelope is also surfaced end-to-end.

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

This contract supersedes the pre-fix behaviour where `prism escalate` delivered the same prompt body multiple times under load (issue #1685). Sender-side double-invocation (a worker re-running `prism escalate` because the success signal was unclear) is covered by the idempotency guard above; see issue #2018.

### When to use `prism escalate` vs `prism prompt`

- **`prism escalate`** — you are a worker handing a question or decision to the coordinator and pausing your turn until you hear back. Use this instead of stopping after sending a hand-crafted `prism prompt`.
- **`prism prompt`** — you are sending an informational follow-up to a running session and either continuing your work (sender keeps going) or expect no response (e.g. delivering a review-complete prompt). Workers prompting their own coordinator are usually better served by `prism escalate` instead.

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
- **Suppressed while escalated.** A worker in the `escalated` state has already informed the coordinator via `session.escalated`; a subsequent "has finished" notification is a false signal (the worker is paused awaiting guidance, not done). The state clears on any incoming turn_start, after which a normal finish notifies as usual.

## Debugging a running or stuck session

Use this decision tree when a session appears stuck, produces no output, or fails unexpectedly.

**Step 1 — Session state check:**

```bash
prism sessions list          # human-readable table
prism sessions list --json   # parseable when scripting
```

Examine the `state` column (`active`, `waiting`, `idle`, `finished`, `error`), the port, and the `last_seen` timestamp. If a session has a DB row but no live tmux session, it can be a zombie (DB row without a live process). Proceed to step 2.

**Step 2 — Recent activity:**

```bash
prism checkin <session>
```

Reads the last 10 turns from the prism DB as a rich narrative view. Use `--verbose` for full tool args/results when something looks off. See [`prism checkin` flags](#prism-checkin-flags) below for all options, and [Who can check in on what](#who-can-check-in-on-what) for which sessions your role can reach.

**Step 3 — Sidecar logs:**

```bash
prism logs <session>
```

The raw sidecar log is where sandbox startup errors, timing traces, and stderr from failed launch commands land. This is the most informative diagnostic for infrastructure failures. See [`prism logs`](#prism-logs) below for full flag documentation.

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

- **`statfs <path>: no such file or directory`** — the sandbox was told to bind-mount a path that does not exist on the host. Check the preceding log line for the full launch command. See incident #751 for a historical example.

- **`startup-connect timeout fired`** — the session started but the sidecar never received the first SSE event from the agent. Usual causes: a misconfigured `harness-config.json` (agent not declared, malformed JSON), a missing bind-mount, or a missing `--agent` flag value. Check the sidecar log for the launch command line and any JSON parse errors.

- **Session rows present in `prism sessions list` but no events in `prism checkin`** — the agent process either never started or died immediately after creation. Run `prism logs <session>` to see the full launch command line and its stderr output.

- **Session name doesn't match expected shape** (e.g. `~review` where `~review-1-review-code` is expected) — the agent-list construction produced the wrong agent names, or the `--agent` flag value is incorrect. Check the session's `harness-config.json` for the `agent` block contents and the sidecar log for the `--agent` flag value used in the command line.

- **Zombie DB rows (session in `prism sessions list` but no live tmux session)** — a previous session's process died without cleaning up DB state. Use `prism cleanup --yes --session <name>` to end the row (stamps `ended_at`, releases any dangling port, clears the pi resume linkage) so it drops out of the active-session view. The row itself is preserved; re-spawning on the same branch name reuses it by re-seeding `state` back to `idle`.

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
