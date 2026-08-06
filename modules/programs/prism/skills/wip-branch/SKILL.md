---
name: wip-branch
description: Load this skill when the user signals that the current branch is a long-lived work-in-progress (WIP) branch where external review is deferred — phrases like "WIP", "work in progress", "long-lived branch", "feature branch", "defer review", "no reviews yet", "treat as WIP", "don't run reviews", "skip review for now", or similar. Once loaded, the worker stops kicking off `prism review` after each push and ignores the git-push reminder's review instruction until the user explicitly signals readiness.
---

# WIP Branch Workflow

## When to load

Load this skill when the user indicates that the current branch is a long-lived work-in-progress branch and external review is deferred. Triggering phrases include (non-exhaustively):

- "WIP branch", "work in progress", "this is WIP"
- "long-lived branch", "multi-week branch", "feature branch"
- "defer review", "no reviews yet", "skip reviews for now"
- "don't run `prism review`", "no review yet", "review later"
- "treat as WIP", "WIP rules", "WIP mode"

Skills are cheap to load. False positives are fine — if the user mentions WIP-shaped language, load this skill.

This skill targets **workers only**. Coordinators are out of scope.

---

## What "WIP branch" means here

A WIP branch is a long-lived (often multi-week) feature branch where:

- Work happens iteratively across many sessions and many commits.
- External review (the 5-agent `prism review` sweep) is **deferred** until the user explicitly signals readiness.
- "Finished" — the end of a worker turn — is a **checkpoint**, not a ship signal. The PR is not ready to merge; it is parked.
- The PR, if open, is a **draft PR** (visual GH-UI signal + guard against accidental merge).

Default worker discipline assumes external review fires after every push. On a WIP branch that is wrong: it burns 5 reviewers' worth of compute many weeks before the branch is actually review-ready, and the reviewers will surface noise about half-done state that the user already knows about.

---

## Behavioural deltas vs default worker discipline

While this skill is active, override the default worker review flow as follows. Everything else in `worker.md` continues to apply unchanged.

### 1. Do NOT run `prism review` after pushing

After `git push`, **do not run `prism review <pr>`**. Not after the first push, not after subsequent pushes, not at the end of a long session, not before declaring a checkpoint. External review only fires when the user explicitly exits WIP (see "Exit conditions" below).

### 2. Open the PR as a draft

When opening a PR on a WIP branch for the first time, use:

```bash
gh pr create --draft --title "..." --body "..."
```

The `--draft` flag gives Ben a visual signal in the GitHub UI and blocks the merge queue from accidentally landing the PR.

If a non-draft PR already exists when this skill loads, mention it in your next message and ask whether to convert it to draft via `gh pr ready --undo` — do not flip it silently.

### 3. The loop-limit doctrine doesn't fire (document the irrelevance)

The default worker discipline says "escalate after 3 review cycles without convergence." On a WIP branch no review cycles run, so the 3-cycle limit never fires. **Do not call `prism escalate --prompt "loop-limit reached"`** — there is no loop to be stuck in. This rule exists to head off confusion, not because it is reachable.

Other escalation paths (genuine blockers, AC contradictions, out-of-scope discoveries) still apply normally — see worker.md.

### 4. Push freely; no squashing pressure

Push regularly. Many small commits is fine while the branch is WIP. The PR will be squash-merged at the end, so intermediate commit shape doesn't matter. Don't waste cycles cleaning up history mid-WIP.

### 5. Repo tests still run — no discipline relaxation

WIP means **"no external review yet"**, not "loose internal discipline". The worker.md quality gates (CI-match `go test ./... -race`, vacuous-test verification, negative-mutation per fix, `nix build .#prism` for prism-touching changes) **still apply on every meaningful change**. Knowing your own work compiles and passes is independent of whether someone else has reviewed it.

If tests fail, fix them before declaring the checkpoint. Pushing red is fine on WIP (you're allowed to park half-finished work) but you must say so explicitly in the checkpoint message.

### 6. "Finished" is a checkpoint, not a ship signal

When you finish a worker turn on a WIP branch, your closing message is a **status checkpoint**, not a handoff for merging. Phrase it as such:

- Summarise progress made this session.
- Note any blockers, open questions, or known-broken state.
- Outline likely next steps.
- **Avoid** phrases like "ready for review", "ready to merge", "good to go", "wrapping up", "PR is ready". These language patterns trigger Ben (and any downstream tooling) to treat the work as review-ready, which is wrong while WIP.

Equivalent acceptable phrasings: "Checkpoint pushed.", "Parked at <state>.", "Stopped at <decision>; next: <X>."

---

## The git-push reminder — partially obey, partially disobey

After the *first* `git push` in a session, the pi extension at `pi/extensions/prism.ts:1154` injects the canonical `GIT_PUSH_REMINDER_MESSAGE` as a steer. The reminder fires at most once per session (issue #2646) — later pushes in the same session do not trigger it again:

> "You just ran git push. If this was in the context of an open PR, first load the `prism` skill via the skill tool so you have the full async review workflow context, then run `prism review <pr-number>` to kick off the parallel review. Wait for the review-complete prompt before merging."

This reminder fires unconditionally — it is not aware of WIP context. Handle it as follows:

- **Partially obey**: treat the reminder as confirmation that the push registered. That is the only useful signal it carries while WIP is active.
- **Disobey the review instruction**: do **not** load the `prism` skill in response to the reminder. Do **not** call `prism review <pr-number>`. Do **not** wait for any review-complete prompt — none is coming, because no review will be kicked off.

The reminder arrives once, on the first push. Disobey the review instruction on that one delivery, same as above. A session that compacts or resumes does not receive a second delivery, so there is no repeated nudge to guard against.

---

## Exit conditions — leaving WIP behind

This skill stops applying when the user explicitly signals that the WIP phase is over. Trigger phrases include:

- "ready for review"
- "mark this ready"
- "exit WIP", "leave WIP", "out of WIP"
- "this is ready", "ready to merge", "ready to ship"
- "kick off review", "run the review now", "review this"

When any of these fire:

1. Run `gh pr ready` to clear the draft state on the PR.
2. Resume **standard worker discipline** from `worker.md` — that means running `prism review <pr>` now, handling PASS/FAIL per the standard flow, and treating the PR as review-ready.
3. Acknowledge the transition in your reply ("Exiting WIP. Marking PR #N ready and kicking off review.") so the user has confirmation the mode flipped.

After that point, the default worker behaviour applies — the git-push reminder becomes a real instruction again, and the next push (or the explicit exit signal itself) is followed by a real `prism review` run.
