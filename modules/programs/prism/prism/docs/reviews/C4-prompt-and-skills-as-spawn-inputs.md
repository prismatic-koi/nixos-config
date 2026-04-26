# C.4 — Prompt and skills as first-class spawn inputs

**Track:** C (A/B testing) — Wave 2 follow-up to C.1.
**Issue:** #1087.
**Source corpus:** `docs/architecture-inventory.md` §8.7 (per-axis variation summary, "Prompt template" and "Skills" rows); C.1's landed proposal `docs/reviews/C1-ab-readiness-and-schema-delta.md` (already on `main`).
**Related:** design doc #1072 (`docs/reviews/000-design-narrow-review-series.md`); sibling Wave 2 issues C.2 (#1085, per-role model variation) and C.3 (#1086, outcome capture).

---

## 1. Context recap

C.1's schema delta (now on `main`) adds a `spawn_inputs` table keyed on `instance_id`. Two of its columns are reserved as **placeholders for this proposal**:

```sql
-- From C1-ab-readiness-and-schema-delta.md §4.1
skills_manifest_hash    TEXT,   -- e.g. SHA-256 of the union of skill names + skill content hashes
prompt_template_hash    TEXT,   -- hash of the prompt-template the user invoked (when prompts move to templates)
```

C.1 deliberately did not specify the *shape* of those hashes — that is this document's job. C.1 also added `prompt_text` and `prompt_source` columns alongside, which capture the prompt **as delivered** (not the template that produced it). The two are complementary: `prompt_text` makes the rendered prompt recoverable as a single field; `prompt_template_hash` lets two runs of the same template (potentially with different rendered context) be recognised as instances of the same input.

Inventory §8.7 frames the gap this proposal closes:

> | Prompt template | The initial prompt is passed in the `Prompt` field; per-spawn prompt is not separately persisted as a versioned template (it lives in the assistant's first `msg_user` event). | no | none |
> | Skills | Skills load via opencode at runtime from `~/.config/opencode/skills/`. The Go side is not aware of skills as a structured concept. | no | none |

For an A/B comparison to attribute a behavioural difference to a skills delta — "run-A had the playwright-cli skill installed, run-B did not" — the skills set in effect at spawn time must be a structured, queryable field on the `sessions` (or `spawn_inputs`) row.

This document is **proposal-only**. No Go file, no SQL migration, no skills-walker code is added. The deliverable is this markdown file; implementation is a separate downstream issue Track E will file.

---

## 2. Skills manifest

### 2.1 Where the skills live

In this repo (the deployment artefact):

- `modules/programs/prism/opencode/skills/` — source of skill directories (one directory per skill, each containing a `SKILL.md` plus optional bundled scripts and references).
- `modules/programs/prism/opencode.nix:331-339` — `skillsDir` builds a single derivation that merges those source directories. The result is mounted via `xdg.configFile."opencode/skills".source = skillsDir` (`opencode.nix:1468`).
- The mounted directory at `~/.config/opencode/skills/` is therefore a **read-only nix-store path** that changes only on system rebuild. `ls -la ~/.config/opencode/skills/` shows a `dr-xr-xr-x … nobody nogroup` directory rooted in `/nix/store/<hash>-opencode-skills`.

Per-repo skills do not exist today. The repo's AGENTS.md does not document a per-project skills mechanism, and opencode's runtime (per `agentInstructions` in `opencode.nix:341-368`) loads skills exclusively from the global `~/.config/opencode/skills/` directory via the `skill` tool.

### 2.2 What "the manifest" should be

Three candidate shapes, in order of cost and fidelity:

1. **Names only.** Sorted list of top-level directory names under `~/.config/opencode/skills/`, joined and SHA-256'd. Cheap; survives `SKILL.md` content tweaks (which is wrong — a skill whose body changed should hash differently).
2. **Per-skill content hash, then a combined hash.** For each skill directory, hash the contents of every regular file recursively (sorted by relative path), then SHA-256 the concatenation `name\0file_hash\0name\0file_hash…`. The combined hash changes if any byte of any skill changes. Survives `mtime` jitter (mtime is not part of the hash).
3. **Nix-store path of the merged `skillsDir` derivation.** Read the symlink target of `~/.config/opencode/skills/` (or the `xdg.configFile` source). The path is something like `/nix/store/abc123…-opencode-skills`. Two system builds with identical skill content produce identical store paths (nix is content-addressed via input hashing). This is the cheapest shape that still distinguishes content changes — it costs one `os.Readlink` and zero file I/O.

**Proposed: shape 3 as the primary manifest hash, shape 2 as a fallback when the directory is not a nix-store path.** Rationale:

- On NixOS / Darwin-with-nix-darwin (the deployment platforms this project targets), the skills directory is *always* a nix-store path. The store path is already a content-addressed manifest hash by construction — re-deriving one in Go would be wasted work.
- On a developer machine that runs prism without the nix-managed skills mount (e.g. a contributor testing prism CLI standalone), the directory is a plain filesystem tree and shape 2 is the right form.
- The detection rule is: if `os.Readlink(~/.config/opencode/skills)` succeeds **and** the target is under `/nix/store/`, persist `nix:<basename-of-store-path>`. Otherwise walk the tree and persist `sha256:<hex>` per shape 2. The `nix:` / `sha256:` prefix tags the shape so downstream consumers can interpret the field correctly.

The column already reserved in C.1 (`spawn_inputs.skills_manifest_hash TEXT`) carries either form; the prefix is the discriminator.

### 2.3 What counts as "the same skills set" across two runs

The hash compares equal between runs iff:

- (nix shape) the merged `skillsDir` derivation produced the same store path, which happens iff every input to that derivation is byte-identical. **`mtime` and file ownership are not inputs** (nix normalises both at store-path construction time), so a system rebuild that touches no skill content produces an identical hash.
- (filesystem shape) the recursive content hash matches. Sort order is deterministic (`filepath.WalkDir` in lexical order); file content is hashed by bytes; mtime is **explicitly excluded** to avoid spurious mismatches when the file is regenerated identically.

This rule resolves the "what counts as the same" question the spawn prompt asks: **byte-equal file contents, regardless of mtime, in the same set of named skills**.

### 2.4 Where the manifest is captured

Two candidate sites, mirroring C.1's open question 12 (synchronous spawn-time write vs asynchronous on-first-event):

1. **At spawn time, in `cmd/spawn.go`** — read the skills directory before `session.SpawnSession` is called and pass the hash through `SpawnOpts` to the `spawn_inputs` insert. Pros: synchronous; the row is consistent with the rest of `spawn_inputs`; no harness involvement. Cons: another I/O step in the spawn hot path (one `Readlink` in the nix case, a tree walk in the fallback).
2. **At first-skill-load time, reported by the harness** — opencode's `skill` tool fires on demand; the sidecar's event ingest could record which skills *were actually loaded* over the lifetime of a session and roll up at session end. Pros: distinguishes "available" from "used". Cons: incomplete by design (a skill that was never invoked never appears); harness-specific (PI does not currently surface a skill-load event); cannot answer "what would have been available if asked", which is the A/B question.

**Proposed: capture at spawn time (option 1).** The A/B question is "what was the skills environment for this run", not "what did the agent end up using". The latter is interesting telemetry but it answers a different question and properly belongs alongside outcome capture (C.3), not alongside spawn inputs.

If a future iteration wants both — set-available *and* set-used — the second can land as a separate column or a separate event type (`agent_events.type = 'skill_loaded'`) without disturbing this proposal.

### 2.5 Where the manifest is stored

C.1 already reserved `spawn_inputs.skills_manifest_hash TEXT`. **No new column is needed.** This proposal specifies the value written:

- `nix:<basename>` — e.g. `nix:abc123def456-opencode-skills` — when the skills directory resolves to a nix-store path.
- `sha256:<hex>` — when the directory is a plain tree.
- `NULL` — when the directory does not exist (e.g. opencode is not installed on this host, or `XDG_CONFIG_HOME` points somewhere unexpected). **Not** an empty string and **not** the SHA-256 of an empty input — `NULL` distinguishes "not captured" from "captured and empty".

A sister table — for example `spawn_skills(instance_id, skill_name, content_hash)` — is **deliberately not proposed** here. The hash on its own is sufficient for the comparison query ("did A and B see the same skills?"); enumerating per-skill membership belongs in a follow-up if and when a query needs to ask "which skill was added between A and B?". That follow-up can join on `instance_id` without touching the C.1 / C.4 schema.

### 2.6 Mid-session skill changes — `[uncertain]`

**[uncertain — opencode's skill-loading lifecycle has not been traced for this proposal.]** The right manifest shape depends on whether skills can be loaded *and unloaded* mid-session, or whether the skills directory is read once at session start and frozen.

Two possibilities:

- **Frozen at session start.** opencode reads the skills directory at process startup, caches the list, and serves the `skill` tool from that cache for the session's lifetime. In this case the spawn-time hash is a complete and accurate manifest of "the skills the agent could have asked for". A system rebuild that swapped the skills directory mid-session would not affect the running session.
- **Re-read per `skill` tool invocation.** opencode reads the directory each time the `skill` tool fires. In this case a mid-session system rebuild could change the available skill set, and the spawn-time hash represents only the *initial* environment — not the lived one.

If the second case holds, the spawn-time hash is still useful (it pins the start-of-run environment) but should be paired with either:

- a per-`skill_loaded` event payload that records the resolved skill's content hash at load time, or
- a session-end re-hash of the directory, stored alongside the spawn-time hash, so an A/B comparison can detect "the skills environment changed during run A but not during run B".

**Resolution: the implementation issue should trace opencode's skill-loading code (a few hours of source reading) before the migration ships.** If skills are frozen, the spawn-time hash is the complete answer. If skills are dynamic, add a `skills_manifest_hash_end TEXT` column on `sessions` (not `spawn_inputs` — it is an outcome, not an input) to capture session-end state. Either way, the C.1-reserved `spawn_inputs.skills_manifest_hash` column is correct and forward-compatible.

This `[uncertain]` flag is the document's main edge-case carrier per the AC.

---

## 3. Prompt template

### 3.1 The two delivery paths today

Per C.1 §2's table row for the positional prompt and §4.1's `prompt_source` enumeration, the prompt reaches the harness through four paths:

- `cli-positional` — `prism spawn --prompt 'free text'` (or `--prompt-file`, `--prompt -`). Resolved by `cmd/prompt_input.go:70-91` `resolvePrompt`. The prompt text is *exactly* what the user supplied.
- `cli-stdin` — same as `cli-positional` but the source flag was `--prompt -`. Behaviourally identical from the harness's perspective.
- `proxy-spawn` — `internal/sidecar/sidecar.go:3107-3251` `/spawn` host-API handler. Receives a `prompt` field over JSON, forwards it as `--prompt` to the host-side `prism spawn` subprocess. The text is end-to-end identical.
- `review-fanout` — `internal/review/review.go:1247-1281` `buildReviewPrompt`. The prompt is **constructed programmatically**: a "Context for your review" block (recent commits, PR metadata, linked issues, diff) is prepended to the role-specific content, then handed to `SpawnOpts.Prompt`. Two review-fanout runs of the same PR produce *almost-identical* prompts whose only differences are timestamps and (potentially) commit-list ordering.

The first three paths share a property: **the user-supplied text is opaque from prism's perspective**. There is no template — the prompt is a literal string. C.1's `prompt_text` column already captures it verbatim.

The fourth path is genuinely templated: `buildReviewPrompt` is a function whose source code is the template, and whose output depends on inputs (`prNumber`, `prCtx`).

### 3.2 What the template hash should be

For free-form prompts (the first three paths), there is no template — only the text. Two options:

- **Option A:** Set `prompt_template_hash = SHA-256(prompt_text)`. The column then doubles as a fingerprint of the rendered prompt. Two runs with literally identical prompt text get the same hash; any character change produces a different hash. This is what C.1 implicitly assumed by naming the column `prompt_template_hash`.
- **Option B:** Set `prompt_template_hash = NULL` for free-form paths and reserve the column exclusively for genuinely templated paths (review fan-out and any future programmatic spawner). The rationale is to avoid conflating "we can identify the template that produced this prompt" with "we can fingerprint the rendered text" — the latter is what `prompt_text` already provides (just hash it on the fly when the comparison query runs).

**Proposed: Option B.** Reasons:

- Hashing `prompt_text` to populate the column is redundant; the comparison query can compute `sha256(prompt_text)` at read time at near-zero cost.
- A `NULL` `prompt_template_hash` then carries semantic content: "this prompt was supplied as text, not generated from a template". The A/B comparison query "are these two runs comparable on the prompt-template axis" becomes meaningful: `WHERE prompt_template_hash IS NOT NULL AND prompt_template_hash_a = prompt_template_hash_b` selects only template-generated pairs, where the template's intent is invariant across runs even when the rendered text differs.
- The `prompt_source` column already discriminates the paths; `prompt_template_hash` is then strictly the answer to "which template produced this", not "what is this prompt's fingerprint".

### 3.3 What the template identifier should be for the templated path

For `review-fanout`, the template is `buildReviewPrompt` in `internal/review/review.go`. Two reasonable identifiers:

1. **A stable template name.** A short string the spawner knows, e.g. `"review-fanout-v1"`. Hardcoded in `buildReviewPrompt`. Survives source refactors that do not change the rendered output. Requires a manual version bump when the template's *intent* changes; otherwise A/B comparisons across versions are silently invalid.
2. **Git SHA of the source file at spawn time.** Read `git rev-parse HEAD:internal/review/review.go` at spawn time and persist that SHA. Self-versioning — any change to the file (including refactors that do not affect output) produces a new hash. Couples the template identity to the source-tree state, which is an over-attribution but a safe one.

**Proposed: a hybrid — template name first, git SHA suffix as forensic detail.**

```
prompt_template_hash = "review-fanout:<git-sha-of-review.go-at-spawn-time>"
```

Rationale:

- The template name is what humans use to ask "are these the same template" — short, stable, queryable.
- The git SHA suffix lets a comparison query distinguish "same template name, different source" (e.g. comparing a run from before a `buildReviewPrompt` refactor with a run from after) without forcing a manual version bump.
- The `prompt_text` column is still the source of truth for the rendered prompt — the hash is for the *template*, not the *output*.

For programmatic spawners that do not yet exist (e.g. a future "spawn-N-agents-with-this-task" CLI), the same convention applies: `<template-name>:<source-sha>` where the spawner controls the template name.

### 3.4 Where the prompt is captured and stored

Already settled by C.1:

- `spawn_inputs.prompt_text TEXT` — the rendered prompt as delivered.
- `spawn_inputs.prompt_source TEXT` — `'cli-positional' | 'cli-stdin' | 'proxy-spawn' | 'review-fanout' | NULL`.
- `spawn_inputs.prompt_template_hash TEXT` — this proposal: NULL for free-form paths, `<template-name>:<source-sha>` for templated paths.

No additional column is needed. C.1's slot is sufficient.

---

## 4. Call sites that would change

The capture work touches three sites. **No code is added by this proposal** — these are the file:line locations a downstream implementation issue would edit.

### 4.1 Skills manifest capture

- `modules/programs/prism/prism/cmd/spawn.go:230-474` `runSpawn` — read `~/.config/opencode/skills/` (resolve via `XDG_CONFIG_HOME` with the standard fallback to `~/.config`), compute the manifest per §2.2, and pass the result through `SpawnOpts` (a new `SkillsManifestHash string` field) to `session.SpawnSession`. The capture site is **after** flag resolution and **before** `session.SpawnSession` is invoked at `cmd/spawn.go:465`, so the value is available when `spawn_inputs` is written.
- `modules/programs/prism/prism/internal/sidecar/sidecar.go:3107-3251` `/spawn` host-API handler — the proxy-spawn path delegates to `prism spawn` as a subprocess (`cmd := exec.Command(prismBinary(), args...)` at `:3231`). The subprocess re-runs `runSpawn` and therefore re-reads the skills directory on the host. **No proxy-side change is required** for the skills capture — the host-side spawn already covers it. The only nuance is that the proxy is running inside a container with a different `~/.config/opencode/skills/` view (or none at all), but because the actual skills read happens in the spawned subprocess on the host, the host's view is what gets captured.
- `modules/programs/prism/prism/internal/session/spawn.go:187-460` `SpawnSession` — accept a new `SpawnOpts.SkillsManifestHash` field and thread it into the `spawn_inputs` insert (the insert site itself does not exist yet — it is part of C.1's downstream implementation, not yet landed).

A new helper, e.g. `internal/skills/manifest.go` `ComputeManifest(skillsDir string) (string, error)`, would house the §2.2 logic. Single-file, pure function, no new dependencies.

### 4.2 Prompt-source capture

- `modules/programs/prism/prism/cmd/spawn.go:255-258` — `resolvePrompt(cmd)` is called here. The same call needs to return *which source* produced the prompt, not just the text. Two options: (a) extend `resolvePrompt` to return `(text, source string, err error)`; (b) add a sibling `resolvePromptSource(cmd)` that returns the discriminator. Either is mechanical. The classification is:
  - `--prompt -` → `cli-stdin`
  - `--prompt-file <path>` → `cli-positional` (or, if a stronger distinction is wanted, `cli-file` — a fourth value)
  - `--prompt <text>` → `cli-positional`
  - no prompt flag → NULL
- `modules/programs/prism/prism/internal/sidecar/sidecar.go:3107-3251` — the `/spawn` handler already has the prompt as a JSON field. It currently re-emits `--prompt <text>` to the spawned subprocess, which would re-classify the prompt as `cli-positional` on the host side. To preserve the `proxy-spawn` source distinction, the proxy needs to pass an additional flag (`--prompt-source proxy-spawn`) or the host-side `runSpawn` needs to detect that it was invoked from the proxy (e.g. via an env var the proxy sets). **Recommended:** an internal-only `--prompt-source` flag on `prism spawn`, hidden from `--help`, that overrides the auto-detection when the proxy sets it.
- `modules/programs/prism/prism/internal/review/review.go:735, :785, :985, :1017` — review fan-out constructs `SpawnOpts` directly (it does not go through the `prism spawn` CLI). It must set `SpawnOpts.PromptSource = "review-fanout"` and `SpawnOpts.PromptTemplateHash = "review-fanout:<sha>"` explicitly. The git-SHA read happens once per `Run()` invocation, not per agent.

### 4.3 Prompt-template-hash capture

- `modules/programs/prism/prism/internal/review/review.go:735, :785, :985, :1017` — as above, the review fan-out site. The template-name half is hardcoded; the SHA half is `git rev-parse HEAD:internal/review/review.go` executed in the *prism source tree* (not the user's worktree). [uncertain — at runtime, prism is a built binary and the source tree may not be available on the host. An alternative is to embed the SHA at build time via `-ldflags '-X …'` so the binary carries its own version. The implementation issue should pick.]
- All other call sites: `prompt_template_hash` defaults to NULL per §3.2 Option B. No work needed at the free-form spawn paths.

### 4.4 What does *not* change

- `internal/harness/opencode/adapter.go:361-433` `DeliverInitialPrompt` — the harness side. The prompt text reaches the harness through `cfg.InitialPrompt` and `SpawnOpts.Prompt`; no template-or-skills information is needed there. The harness is correctly oblivious to the manifest capture.
- `internal/session/spawn.go:463-479` `spawnAgentPaneEnvVars` — the agent-pane env var injection. `PRISM_INITIAL_PROMPT` continues to carry the rendered prompt; no `PRISM_SKILLS_MANIFEST` env var is proposed (the harness has no use for the hash; the DB write is the consumer).
- `internal/db/db.go` schema — C.1's migration already adds the columns this proposal targets. No new migration is needed for this proposal.

---

## 5. Agent-role system prompt — in or out of the manifest

The agent-role system prompts live at `modules/programs/prism/opencode/agents/*.md` (one per role: `worker.md`, `coordinator.md`, `review-code.md`, …). They are mounted into `~/.config/opencode/agents/` by the same nix-build pattern as the skills directory (per `opencode.nix`'s `xdg.configFile."opencode/agents"` configuration; see the agents-directory `dr-xr-xr-x … nobody nogroup` pattern matching the skills directory).

Two questions to answer:

1. **Is the role prompt versioned?** Yes — it is a checked-in file in this repo. `git log modules/programs/prism/opencode/agents/worker.md` returns a meaningful history.
2. **Does it vary per spawn?** Yes — `--agent <name>` selects which role file opencode reads. A run with `--agent worker` and a run with `--agent coordinator` see different system prompts.

### 5.1 Position: include role-prompt provenance in the manifest

The agent-role system prompt should be **captured in the spawn manifest**, but using the same nix-store-path-or-content-hash trick as the skills manifest, scoped to the *single role file* in effect.

Concretely, add one column to C.1's `spawn_inputs`:

```sql
ALTER TABLE spawn_inputs ADD COLUMN agent_prompt_hash TEXT;
-- 'nix:<basename-of-store-path>' when ~/.config/opencode/agents is a nix-store path
-- 'sha256:<hex>' as the per-file content hash fallback
-- 'git:<sha>' acceptable variant when the prompt is read from a known git ref
-- NULL when the role file does not exist or no role applies (e.g. --agent unset
-- on a non-worktree path produces no role)
```

This is a **new column** that C.1 did not reserve. Naming it `agent_prompt_hash` rather than `system_prompt_hash` keeps the field aligned with `spawn_inputs.agent_flag` (which already exists per C.1 §4.1) so the join story is obvious: "the prompt for the agent flagged in this row".

Rationale:

- Role prompts are *as much an input* as `--profile` or `--variant`. A run with `worker.md` from before a major prompt rewrite vs. after is comparing two different agents in everything but name. Without capture, the comparison silently misattributes the behavioural delta to the model / variant / skills.
- The capture is cheap. One file (the role file) — read once at spawn, hash, write. The nix-store-path case is again a single `Readlink`.
- Storing it on `spawn_inputs` (the input table) rather than `sessions` (the per-incarnation table) makes the join with the rest of the spawn intent trivial: "compare these two `spawn_inputs` rows and tell me what differed" returns the role-prompt delta automatically.

### 5.2 What about the *global* `AGENTS.md` instructions

The global agent instructions (the `agentInstructions` string in `opencode.nix:341-368`, rendered via `programs.opencode.context` to `~/.config/opencode/AGENTS.md`) are not per-role — they apply uniformly to every agent. They are *also* a versioned input that affects behaviour.

**Proposed: out of scope for the manifest, for now.** Two reasons:

- They do not vary per spawn — every spawn on a given system reads the same `AGENTS.md`. The A/B-relevant question is "did the global instructions change between A and B", which is answered by `prism_version` (already on `sessions` per C.1 §3.2) acting as a coarse proxy: a system rebuild that changed `AGENTS.md` produces a different binary version, and `prism_version` distinguishes the two.
- A separate column adds a constant value across all rows on a given system, which is cheap but low-signal. If at a later date the global instructions become *runtime-configurable* (e.g. per-profile `AGENTS.md` overrides), the column can land then with concrete signal to populate it.

If a future user wants to A/B-compare two systems with different `AGENTS.md` content, they can join on `prism_version` and inspect the build manifest. The column is a forward-compat note, not a present-day requirement.

### 5.3 Per-repo `AGENTS.md` files

This repo's `AGENTS.md` (and any sibling repos') are per-worktree, per-checkout files. They reach the agent through opencode's project-context mechanism, not through the prism spawn path. Capturing them in the manifest is **out of scope** for this proposal — `sessions.worktree` plus a downstream `git rev-parse HEAD` in that worktree gives the same provenance with no schema change. If a comparison query needs the file's exact content, a follow-up issue can add it; today the worktree path plus the git SHA of HEAD is sufficient.

---

## 6. Open questions and `[uncertain]` flags

Collected for the synthesis pass (E.1) and the implementation issue Track E will file.

1. **[uncertain] (the document's primary edge-case)** — Does opencode read `~/.config/opencode/skills/` once at session start and freeze the list, or re-read it per `skill` tool invocation? Affects whether the spawn-time hash is a complete manifest of "what the agent could have asked for" (frozen case) or only the initial environment (dynamic case). See §2.6. **Resolution: trace opencode's skill-loading code before the migration ships; if dynamic, add a session-end re-hash column on `sessions`.**
2. **[uncertain]** — Is the prism binary's source tree available at runtime for the `git rev-parse` in §3.3 / §4.3? Likely no — the binary is nix-built and the source is not co-located with the installed binary. Resolution: embed the relevant SHAs at build time via `-ldflags '-X main.reviewPromptSHA=<sha>'`. The implementation issue picks the variable surface.
3. **Open question** — Should `prompt_template_hash` for free-form prompts be NULL (Option B per §3.2) or `sha256(prompt_text)` (Option A)? This proposal recommends NULL; the synthesis pass MAY override.
4. **Open question** — Should `agent_prompt_hash` (§5.1, new column) instead live as part of a normalised side-table `spawn_role_prompts(instance_id, role, prompt_hash)` if a future iteration captures more than one role per spawn? Today only one root role applies per spawn, so a single column is sufficient. C.2's per-role model variation, when it lands, may want a sister table that this column then participates in.
5. **Open question** — The `--prompt-source` propagation across the proxy boundary (§4.2) needs a concrete mechanism: hidden `--prompt-source` flag, env var, or auto-detection from a sentinel header. Implementation issue picks.
6. **[uncertain]** — Does `~/.config/opencode/skills/` exist in container-mode sessions (where opencode runs inside a podman container with a different mount layout)? If the container does not see the host's skills directory, the host-side spawn capture may record a manifest that does not match what the container's opencode actually loaded. Likely the container mounts the host directory through (per the `xdg.configFile` mount pattern, the read-only nix-store path is bind-mountable into the container), but this should be confirmed.
7. **[uncertain]** — On a developer machine without nix-managed skills (the §2.2 shape-2 fallback case), does the recursive content hash perform acceptably for skills directories with hundreds of bundled-script files? Likely yes — the directory is typically a few KB to a few MB — but the implementation issue should put a `[]byte` budget on the walk to fail loudly if a future skill grows pathologically.

---

## 7. Summary

- **Skills manifest** (§2): persist as `spawn_inputs.skills_manifest_hash` (column already reserved by C.1). Value shape is `nix:<store-basename>` when the directory is a nix-store path (the deployment case), `sha256:<hex>` content hash as fallback. Captured **at spawn time**, in `cmd/spawn.go`, via a new `internal/skills/manifest.go` helper. Mid-session skill change semantics carry an `[uncertain]` flag pending an opencode source trace.
- **Prompt template** (§3): C.1 already captures `prompt_text` and `prompt_source`. This proposal specifies that `prompt_template_hash` is NULL for free-form prompts and `<template-name>:<source-sha>` for genuinely templated paths (today: only `review-fanout`).
- **Call sites** (§4): three change sites — `cmd/spawn.go:230-474` for skills + free-form prompt source, `internal/sidecar/sidecar.go:3107-3251` for the proxy-spawn `prompt_source` distinction, `internal/review/review.go:735/785/985/1017` for review-fanout's template hash and source. The harness adapter and `agent_events` schema are correctly untouched.
- **Agent role prompt** (§5): proposed *in* the manifest as a new `spawn_inputs.agent_prompt_hash TEXT` column (the only schema delta beyond the C.1-reserved slots). Same nix-store-or-content-hash shape as the skills manifest. The global `~/.config/opencode/AGENTS.md` and per-repo `AGENTS.md` are out of scope and tracked by `prism_version` / `worktree`+`git rev-parse` respectively.
- **Edge-case** (§2.6): the right manifest shape — spawn-time-only vs spawn-time-plus-session-end — depends on whether opencode loads skills dynamically or at session start. Flagged `[uncertain]`; resolution path is documented.

The proposal is conservative — it adds one new column (`agent_prompt_hash`) on top of C.1's slots, defines value shapes for two slots C.1 reserved (`skills_manifest_hash`, `prompt_template_hash`), and identifies the precise call sites a downstream implementation issue would touch. No Go code, no SQL migration, no implementation work is included.
