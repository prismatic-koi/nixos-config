---
name: retro
description: Load when the user asks for a retrospective, postmortem, or after-action analysis of a specific agent session (and its review cycles) — phrases like "do a retro of that session", "postmortem this", "how did `<session>` go", "what went wrong in that session", or "analyse the review cycles for that PR". Targeted analysis of one session group (worker + its `~review-N-<agent>` subsessions); not for broad-window sweeps across many sessions.
---

# Retro Skill — Targeted Session Retrospective

## When to load

Load this skill when the user asks you to:

- Do a retrospective or postmortem on a **specific** agent session.
- Analyse how a particular piece of work went — "how did `<session>` go?", "what went wrong in that PR?", "why did it take three review cycles?".
- Examine the review cycles for a worker session — what verdicts were returned, whether the worker's fixes addressed the blockers, whether the review converged.

**This skill is targeted-only.** It covers one session group: the named worker session plus its `~review-N-<agent>` subsessions. Broad-sweep analysis — scanning many sessions for patterns using `prism stats --days N` — is out of scope here. For that, use the `@retro` subagent.

---

## Data gathering

Run these commands to collect the data for analysis. Work through them in order.

### 1. Confirm the session exists and discover subsessions

```bash
prism sessions list
```

Look for the named session and any `~review-N-<agent>` subsessions attached to it. If the session is not present, see [Edge cases](#edge-cases).

### 2. Worker metrics (optional but useful)

```bash
prism stats <session>
```

Gives a quantitative summary: turn count, token usage, compaction events, tool call counts. Use this to anchor the narrative before diving into the timeline.

### 3. Full worker conversation

```bash
prism checkin <session> --verbose
```

The full tool-call timeline of the worker with complete tool arguments and results. This is the primary evidence for the analysis — read it carefully before drawing conclusions.

### 4. Review cycle detail (when cycles exist)

For each review cycle, check in on each review agent:

```bash
prism checkin <session>~review-<N>-<agent> --verbose
```

Where `<N>` is the cycle number (1, 2, 3) and `<agent>` is one of `review-goal`, `review-code`, `review-security`, `review-qa`, `review-context`.

Focus on:
- The verdict each agent returned (PASS / FAIL / ERROR / no-start).
- The specific blocking issues identified on FAIL.
- Whether the worker's next commit addressed those blockers or merely shuffled them.

---

## Analysis dimensions

Always consider all five of these dimensions, even if some are uneventful:

- **Token efficiency** — was the token spend proportionate to the value delivered? High input tokens per turn signal excessive re-reading of context. Many compactions signal the task was too large for one session.
- **Trajectory directness** — did the worker go straight to the solution, or meander? Count turns spent on dead ends versus productive progress.
- **Error recovery** — when the worker hit an error, did it adapt or repeat the same approach? Doom loops (same tool + args called 3+ times) are a red flag.
- **Tool selection** — did the worker use the right tools? Did it reach for bash when a dedicated tool existed, or over-rely on read when grep would be faster?
- **Scope discipline** — did the worker stay within the stated scope, or drift into unrequested changes? Scope creep burns tokens and confuses reviewers.

---

## Review-cycle analysis

Review cycles are a first-class concern. Work through these questions for each session that went through `prism review`:

**Cycle count and verdicts**

- How many full review cycles ran (1, 2, or 3)?
- For each cycle, what verdict did each of the five agents return?
  - **PASS** — agent ran and found no blocking issues.
  - **FAIL** — agent ran and identified at least one blocking issue.
  - **ERROR / no-start** — agent failed to start (infrastructure failure, not a code-quality verdict). Distinguish these from FAIL verdicts: no-start errors mean the review never ran, so no quality conclusions can be drawn. The correct response is to re-run, not to fix code.
- Was the final cycle an all-PASS, a partial FAIL, or a 3-cycle limit hit without convergence?

**Fix quality between cycles**

- After each FAIL cycle, did the worker's commits address the specific blocking issues the reviewers identified?
- Or did the worker make superficial changes that didn't resolve the underlying problem (verdict "shuffling")?
- If the same blocker appeared across two or more cycles, that is a convergence failure — name it explicitly.

**Doom loop detection**

- If the session hit the 3-cycle limit without convergence: was the divergence narrowing (fewer blockers each cycle) or stuck (same blockers repeating)?
- Did the worker escalate at the 3-cycle limit, or attempt a 4th cycle anyway?

---

## Output structure

Structure the retrospective output exactly as follows. Each section is mandatory; mark "N/A — [reason]" if a section genuinely does not apply (e.g. no review cycles ran).

---

**Session Retrospective: `<session-name>`**

**What happened**
Narrative reconstruction of the session from start to finish. Be specific — name the files touched, the errors encountered, the decisions made. "The worker spent 12 turns trying to fix a nix build error by repeatedly editing the same module" is more useful than "the worker had trouble with nix."

**Where it went wrong**
Identify the turning point(s). At what point did the session start struggling? What was the trigger? If the session went well, say so and explain why.

**Root causes**
Be direct. Name the actual causes. Draw from this list:

- *Bad prompt / unclear specification* — the task was underspecified or contradicted itself.
- *Missing context* — information that would have helped was not in scope or not loaded.
- *Wrong model for the task* — the model's capabilities were mismatched to the work.
- *Tool misuse* — wrong tool used repeatedly, or right tool used incorrectly.
- *Permission gap* — a needed bash command or operation was blocked.
- *Scope too large* — the task was too broad for one session; should have been split.
- *Compaction pressure* — the context window exhausted, losing important state mid-task.
- *Doom loop* — repeated the same failing action without changing approach.
- *Review-cycle divergence* — the worker could not converge on fixes that satisfied the reviewers.

List all that apply. If none apply (session went cleanly), say so.

**Review cycle analysis**
Cycle count: N
Per-cycle verdicts:
  - Cycle 1: [per-agent: PASS / FAIL / ERROR / no-start]
  - Cycle 2: [if applicable]
  - Cycle 3: [if applicable]

Fix quality: [did the worker's commits between cycles address the specific blockers, or shuffle them?]
Convergence: [converging / stuck / hit 3-cycle limit]

If no review cycles ran: "N/A — session had no review cycles."

**What would have helped**
Counterfactual: what information, instruction, or tooling would have prevented the problems? Be specific:
- A specific skill loaded at the start?
- A different permission set?
- A different prompt phrasing or tighter scope?
- Breaking the task into smaller sessions?
- A different model?

**Recommendations**
Concrete, actionable changes. Each recommendation must name a specific mechanism — not a vague improvement. Examples of acceptable mechanisms:
- "Add a rule to `worker.md`: 'When a nix build fails, always add `--show-trace` to the next attempt.'"
- "Add a bash permission for `<command>` in `opencode.nix`."
- "Create a skill at `modules/programs/prism/opencode/skills/<name>/SKILL.md` covering <topic>."
- "File an issue to implement <specific prism feature>."
- "Split tasks of type X into two sessions at the <boundary>."

"Be more careful" is not a recommendation. "Consider improving" is not a recommendation.

---

## Principles

**Be analytical, not diplomatic.**
"The worker wasted 40 turns editing the wrong file because it didn't read the module index first" is more useful than "the worker encountered some challenges with file identification." Quantify where possible: turns wasted, tokens burned on a dead end, number of times the same blocker appeared.

**Recommendations must name a specific mechanism.**
Vague advice does not change agent behaviour. A skill, a permission rule in `opencode.nix`, a prompt change, or an issue to file — one of these must be named per recommendation. If you cannot name a mechanism, the recommendation is not ready.

---

## Edge cases

**Session name not found**
If `prism sessions list` does not show the named session, output:
> "Session `<name>` not found. Use `prism sessions list` to see available sessions. If the session was cleaned up, use `prism sessions list --all` to check historical records."

Do not proceed with analysis.

**Session has no review cycles**
If no `~review-N-*` subsessions exist, the session was single-shot work (no `prism review` was run, or the first cycle passed and cleaned up). Complete the retrospective normally; mark the "Review cycle analysis" section as "N/A — session had no review cycles."

**Session hit the 3-cycle review limit**
This is a convergence failure. In the "Review cycle analysis" section, document:
- Which agent(s) failed across all three cycles.
- The specific blocking issue(s) that did not resolve.
- Whether the worker escalated at the limit (correct) or attempted a 4th cycle (escalation-avoidance).
- Root cause: classify as `review-cycle divergence` in the root causes section.

**Review-cycle no-start / infrastructure failures**
A no-start error means the review agent failed to launch — it is an infrastructure failure, not a code-quality verdict. Distinguish it from a genuine FAIL:
- **No-start (ERROR)**: agent never ran; no quality conclusions can be drawn; correct response is re-run, not code fix.
- **FAIL**: agent ran and found blocking issues; correct response is to fix the code.

In the "Review cycle analysis" section, label no-start cycles clearly as "infrastructure failure (no-start)" and exclude them from the convergence assessment (they do not count toward the 3-cycle limit).
