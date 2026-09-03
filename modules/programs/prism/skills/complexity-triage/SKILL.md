---
name: complexity-triage
description: Load this skill when preparing to spawn a worker or investigator with `prism spawn` and you need to pick the `--profile` tier — before drafting the spawn command for any non-trivial task, or when reconsidering the tier after a review round burned budget the model headroom did not need.
---

# Complexity Triage

## When to load

Load this skill when:

- Preparing to spawn a worker agent with `prism spawn` for any task that is not a trivial one-line fix. Score the task, pick the tier, pass it as `--profile <tier>`.
- Reconsidering the tier after a retro shows the previous spawn was overpowered (headroom unused, first-pass PASS, zero doom loops) or underpowered (repeated review-cycle divergence, doom loops, escalations on scope).
- Reviewing another agent's proposed spawn command and sanity-checking the tier choice against the actual complexity of the task.

Do NOT load this skill for:

- Single-line fixes, typo corrections, doc-only edits — those skip both AC generation and tier triage. Spawn on the machine default without a `--profile` override, or don't spawn at all.
- `prism quick pr` and other non-`prism spawn` flows that use fixed model routing.

---

## Scoring rubric

Before scoring, consider whether the issue should be split into more than one issue. This is a consideration, not a gate — it does not block or forbid a spawn. Some large issues are genuinely atomic, and forcing a split would fragment them into PRs that leave `main` incoherent. Name the split signals as you weigh them: an AC count above roughly 10; a schema migration; a new package or module; more than one independently shippable capability in one issue. When two or more are present, consider splitting. Record the split decision — split or not, and why — before choosing a tier.

Tier and issue size are separate axes. `prism spawn --profile` answers "how strong a model"; it does not answer "is this one issue or two". Issue #2683 is the worked example: `max` was the correct tier call, and the session still cost $13.03 — 54% of its 8-session batch's total spend — because the issue itself was oversized (13 ACs, a DB migration, a new Go package, a live model call, a 23-file diff), not because the tier was wrong.

Score the task by summing the point values of every signal that applies. Signals are additive — a task can trigger multiple simplifying signals and multiple hardening signals simultaneously.

| Signal | Points |
|---|---|
| Touches ≤ 2 files | -1 |
| Config / YAML / docs only (no application code) | -1 |
| A working reference / analogue file exists in-tree | -1 |
| Additive (add a row / entry / case) not refactor | -1 |
| ACs are all `[functional]` + `[edge-case]`, ≤ 8 items | -1 |
| Novel design decision required (no reference in-tree, tradeoffs must be reasoned about) | +2 |
| Cross-package refactor, > 4 files | +2 |
| Security-sensitive surface (auth, crypto, sandbox policy, secret handling) | +2 |
| Performance-sensitive change (hot path, latency budget, resource limits) | +1 |
| Distributed-systems reasoning required (concurrency, ordering, partial-failure semantics) | +2 |
| Failure mode is over-editing rather than under-delivering — the task edits agent-facing prose, a prompt file, or a safety-bearing file, where doing too much is the risk | +2 |

Read the issue body and the ACs before scoring. Score honestly — the rubric loses its calibration value the moment you thumb the scale.

### Score → tier mapping

| Score | Tier |
|---|---|
| ≤ -3 | `light` |
| -2 to 0 | `standard` |
| +1 to +3 | `heavy` |
| ≥ +4 | `max` |

These four are the only values this rubric returns. `profiles.json` also
defines `fable-low` and `fable-max` — opt-in A/B profiles that run
`claude-fable-5-1` on every role. They are not tiers, they are not on this
scale, and no score selects one. Reach for them only under the calibration
mechanism below.

**Overrides on top of the score:**

- Any `+2` from `security-sensitive surface` or `distributed-systems reasoning required` clamps the tier to `max` regardless of the numeric total. These are the two classes where "the model was fine, but it missed one thing" is expensive to recover from.
- Any task explicitly marked as an A/B calibration run (`prism spawn --abtest tier-a,tier-b …`) bypasses this rubric — the whole point of the A/B is to measure, not to pre-select.

---

## Worked examples

### Example 1 — CI YAML matrix-key alignment (score: -5, tier: `light`)

Task: rename the matrix key `attr` → `target` in `.github/workflows/update-flakes.yml` to match the vocabulary already used in `build-and-cache.yml`.

- Touches ≤ 2 files → **-1**
- Config / YAML / docs only → **-1**
- A working reference file exists in-tree (`build-and-cache.yml`) → **-1**
- Additive-shape change (rename, no logic added) → **-1**
- ACs are `[functional]` + `[edge-case]`, small set → **-1**
- No hardening signals apply → **0**

Total: **-5** → `light`.

### Example 2 — this issue #2404 (score: 0, tier: `standard`)

Task: overhaul profiles.nix, add a new skill, wire it into the coordinator agent, update three machine defaults.

- Touches ≤ 2 files → **no** (5+ files across `modules/` and `machines/`) → 0
- Config / docs only → **yes** (Nix + Markdown, no Go) → **-1**
- Reference file exists in-tree → **yes** (`acceptance-criteria/SKILL.md` as the SKILL template; existing profile records as the profile template) → **-1**
- Additive → partial: adds `light`, retires four names. Score the retire as neutral, the add as additive → **-1**
- ACs all `[functional]`+`[edge-case]`, ≤ 8 items → **no** (13 ACs) → 0
- Novel design decision → **no** (the tier shape and skill shape are prescribed in the issue) → 0
- Cross-package refactor, > 4 files → **no** (this is single-repo config, not cross-package) → 0
- No security / performance / distributed-systems signals → 0

Total: **-3** → borderline `light` / `standard`. Because the AC count breaches the ≤ 8 threshold and the "additive" call is soft, round up to `standard`.

### Example 3 — prose-only edit across five files where restraint is the task (score: -2, tier: `standard`, based on #2643)

Task: align wording across five small agent-facing prompt/skill files with a reference form already in-tree.

- Touches ≤ 2 files → **no** (5 files) → 0
- Config / YAML / docs only → **yes** → **-1**
- A working reference / analogue file exists in-tree → **yes** → **-1**
- Additive (add a row / entry / case) not refactor → **yes**, the change is wording alignment, not a structural rewrite → **-1**
- ACs are all `[functional]` + `[edge-case]`, ≤ 8 items → **yes** → **-1**
- Failure mode is over-editing rather than under-delivering — the task edits agent-facing prose files where doing too much is the risk → **+2**

Total: **-4 + 2 = -2** → `standard`.

On the old rubric (without the over-editing signal) this task scores -4, which
reads as `light` — the most `light`-looking task imaginable: prose only, five
small files, a reference form already in-tree. That reading is wrong. The
actual first hand-off on #2643 deleted a safety instruction from
`review-security.md` ("do not invent issues to fill the list") that nothing
in the issue asked it to touch, while presenting the removal as an alignment
fix. The task's difficulty was never about file count or novelty — every
simplifying signal that made it look easy (few files, docs only, reference
in-tree, additive, small AC set) is exactly what let an agent over-edit
unnoticed. The correct tier is `standard`, because the risk on this task is
doing too much, not doing too little, and a stronger model reins that in. Do
not revert this example's tier back to `light` on the reasoning that the
signals "obviously" describe an easy task — that reasoning is the failure
mode the new signal exists to catch.

### Example 4 — podman-proxy field admission (score: +4, tier: `max`)

Task: audit and admit a new `HostConfig.Foo` field to the podman-proxy policy.

- Touches ≤ 2 files → **-1**
- Novel design decision required (must classify field against threat model) → **+2**
- Security-sensitive surface → **+2**

Total: **+3** numerically → `heavy`. But the `security-sensitive` override clamps to `max`. The whole point of the six-layer default-deny model is that quiet field admissions ship CRITICALs.

---

## Calibration mechanism — `prism spawn --abtest`

The scoring rubric is a starting point, not a fixed truth. Empirical calibration uses `prism spawn --abtest`:

```bash
prism spawn --abtest light,standard --branch <branch> --prompt-file <prompt>
```

This spawns two workers on the same prompt with different profiles and records their outcomes side by side. After both finish, compare with:

```bash
prism stats compare <session-a> <session-b>
```

Signals worth tracking on the compared runs:

- **Wall time to first PR open.** A `light` run that took 3x longer than a `standard` run on the same task is a signal the tier was too weak.
- **Review-cycle count to PASS.** More cycles means the worker output needed more correction.
- **Token spend.** The whole point of stepping down a tier is cost — if `light` cost more than `standard` because of retries, the rubric miscalibrated.
- **Doom loops / escalations.** A `light` run that escalated on a task the rubric scored as `light` is a red flag on either the rubric or the score.

Feed the results back into this skill by updating the score → tier mapping thresholds or the point values on individual signals. Do not calibrate on N=1 — two or three A/B runs of similarly-scoped tasks before adjusting a threshold.

### The `fable-*` profiles

`fable-low` and `fable-max` are the A/B legs for `claude-fable-5-1`. Both put
that model on all ten roles — `fable-low` at thinking `low`, `fable-max` at
`xhigh` — so one pair measures the model against a tier and a second measures
what the effort setting is worth:

```bash
prism spawn --abtest max,fable-max --branch <branch> --prompt-file <prompt>
prism spawn --abtest fable-low,fable-max --branch <branch> --prompt-file <prompt>
```

They are opt-in only. The rubric above never returns one, so a `fable-*`
profile appears on a spawn only because a human asked for a calibration run,
or because you started one deliberately and said so in the spawn prompt.
Uniform-across-roles is the point: an A/B leg that mixed models per role
would not answer "how does this model do on this task".

---

## Override

The score is guidance, not a gate. The operator can force any tier by passing `--profile <tier>` explicitly on `prism spawn`, regardless of what the rubric returns. Reasons to override upward:

- Prior retros on similar tasks show the tier consistently underdelivers.
- The task carries reputational or coordination risk that is not captured by the rubric (e.g. first PR under a new convention that others will copy).

Reasons to override downward:

- The task is a deliberate calibration run — see `prism spawn --abtest` above.
- The operator is exploring cost / quality tradeoffs and has a specific budget in mind.

When overriding, note the override in the spawn prompt or a working-log entry so the retro can distinguish "the rubric said standard and the tier chose standard" from "the rubric said heavy and we forced standard to see what happens".

---

## Edge cases

**Score straddles a boundary.** When the score sits exactly on a boundary (e.g. -3, 0, +3), round toward the safer (higher) tier unless a `[security]` or distributed-systems signal is already forcing the choice. The cost of one tier up is small; the cost of a doom loop is not.

**No ACs available yet.** If you are scoring before ACs have been drafted (uncommon — usually the AC pass happens first), skip the AC-count signal and score on the other ten. If the resulting tier is `light`, draft the ACs first and re-score — the AC signal is often decisive at the `light` boundary.

**Task obviously does not fit the rubric.** The rubric is calibrated for engineering-flavoured tasks (code, config, docs). For tasks that are fundamentally research, analysis, or communication (write a design doc, produce a retro, summarise a bus of notifications), pick the tier from the analogous role's floor — the analytical roles (`ac`, `retro`, `investigate`) have a sonnet floor in every tier, so `light` or `standard` is usually right unless the analysis itself is genuinely hard.
