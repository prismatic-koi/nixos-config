# Diagnosis — review-agent "no verdict" / "no output produced" terminations (issue #1993)

## Symptom

During PR #1992's review, two of the five review agents — `review-goal` and
`review-code` — failed to emit a parseable `<verdict>` block across three
consecutive rounds. The other three agents (`review-security`, `review-qa`,
`review-context`) produced full structured output and converged on PASS.

The worker tripped the LOOP-LIMIT footer at cycle 3 even though the
substantive review had converged, because the cycle counter advanced on
rounds where two agents never emitted a verdict.

Across the three rounds the two failing agents fell into two sub-cases:

- **"no output produced"** (rounds 1, 2; also `review-code` round 3) — the
  agent terminated with `state="finished"` and **empty `LastMessage`**.
- **"no verdict found in agent output"** (round 3, `review-goal`) — the
  agent terminated with `state="finished"` and a **non-empty `LastMessage`**
  that did not contain a `<verdict>...</verdict>` tag. The leaked partial
  text was mid-analysis: `"AC6: \`host\` mode is uncapped — the diff
  doesn't introduce a cap for host. ACHIEVED."`.

The partial-text leak proves the agents are not failing to start. They run
far enough to inspect the diff and form a substantive opinion. They die
*after* doing the work, *before* writing the final `<verdict>` block.

## Mechanism

The failure path is a confluence of three pre-existing behaviours; the bug
is that the **review-cycle counter does not distinguish them** from a true
verdict-producing cycle.

### 1. How the session reaches `state="finished"` without a verdict

The PI extension (`modules/programs/prism/pi/extensions/prism.ts`) emits a
`state_change{finished}` frame only when the model's `turn_end` carries
`stopReason === "stop"` (see `resolveTurnEndSignal`,
`pi/extensions/prism.ts:1494-1506`). That signal triggers a 2 s debounce
in the sidecar (`internal/sidecar/events.go:255-318`,
`handleSessionFinished`) which then writes `StateFinished`.

For the agent to reach `finished`, the model must have completed at least
one turn cleanly with `stopReason="stop"` — i.e. the model itself decided
it was done. This is exactly the no-verdict shape: the model believes its
job is finished, but the rendered transcript does not include a
`<verdict>...</verdict>` tag.

Two ways this happens in practice:

- **Tool-only final turn**: the agent's last turn contains only tool calls
  (e.g. one last `gh pr diff` or `git log`), with no assistant text. The
  sidecar's `pipeAccum` is empty when flushed on `turn_end`, and per
  `stdioFlushPipeAccum` / the equivalent socket-pipe path
  (`internal/sidecar/sidecar.go:2425-2438`) no `msg_assistant` event is
  written when the accumulator is empty. `LastMessage` stays `""`. This
  produces the "no output produced" classification at
  `internal/review/monitor.go:355-361`.

- **Text turn ending mid-analysis**: the agent emits a final assistant
  turn with several deltas of text, then stops with `stopReason="stop"`
  before the verdict block. The accumulator has content; a `msg_assistant`
  row is written; `LastMessage` is non-empty but does not contain a
  recognised `<verdict>` tag. This produces the "no verdict found in agent
  output" classification at `internal/review/monitor.go:363-372`.

The round-3 `review-goal` text leak is the latter case.

### 2. Why `review-goal` and `review-code` are disproportionately affected

The five review-agent prompts at `modules/programs/prism/agents/review-*.md`
ask for varying amounts of analytic generation before the verdict block:

- `review-goal.md` (158 lines) — 6 process steps, "at least 5 edge cases",
  "at least 3 representative scenarios end-to-end", a non-convergence
  detection sub-routine, and the `PASS_WITH_DISAGREEMENT` decision tree.
- `review-code.md` (116 lines) — a 10-dimension review checklist, each
  dimension requiring its own analysis paragraph, plus severity-level
  classification.
- `review-security.md`, `review-qa.md`, `review-context.md` — narrower
  scopes; less generation expected per dimension; the verdict block is
  closer to the start of the output.

This is not proven causation, but it matches the observed asymmetry: the
two agents whose prompts demand the most analytic generation before the
verdict block are the two that consistently fail to reach it. The
hypotheses in the issue body — output-size cutoff, role-specific
output-heaviness — both point at the same family. A definitive
attribution requires capturing the agent's full transcript and the
underlying model's `stopReason` at termination (see "Open questions"
below).

### 3. How the cycle counter mis-classifies these rounds

The review-monitor decides whether the current cycle counts toward the
3-cycle LOOP-LIMIT threshold via `currentCycleProducedVerdicts` at
`internal/review/monitor.go:689-696`:

```go
func currentCycleProducedVerdicts(groupData map[string]db.GroupMemberResult) bool {
    for _, mr := range groupData {
        if mr.State == "finished" && mr.LastMessage != "" && mr.StartupError == "" {
            return true
        }
    }
    return false
}
```

The predicate returns `true` if **at least one** member is finished with a
non-empty assistant message. It does **not** check whether that message
contains a parseable `<verdict>` tag, and it does **not** require *every*
member to have produced a verdict.

The same lenient predicate is applied to historical groups by
`CompletedReviewCyclesForParent`
(`internal/review/monitor.go:750-815`) when counting prior cycles. The
comment at lines 794-798 explicitly endorses this:

> Even if the verdict body lacks an explicit PASS/FAIL marker (which
> AssessPassed would classify as VerdictNone), the agent still ran and
> produced output — that is a real cycle.

In PR #1992's case, the three passing agents satisfied
`currentCycleProducedVerdicts` on every round. The two failing agents'
no-verdict / no-output classification was visible inside `AgentResult`
(`IsError=true`), but `currentCycleProducedVerdicts` never consulted it.
So each round was deemed verdict-producing, the counter ticked from 1 → 2
→ 3, and at round 3 the LOOP-LIMIT footer fired.

## Failure path — file:line trace

The full failure path that produced the symptom for PR #1992:

1. PI extension emits `turn_end` with `stopReason="stop"` and (for the
   round-3 `review-goal` case) some assistant text that does not include
   `<verdict>...</verdict>` — `pi/extensions/prism.ts:1494-1506`
   (`resolveTurnEndSignal`) returns `"finished"`, the extension writes
   `state_change{state:"finished"}` at `pi/extensions/prism.ts:2116`.
2. Sidecar receives `state_change{finished}` → `handleSessionFinished` at
   `internal/sidecar/events.go:255-318` debounces 2 s then writes
   `StateFinished` to the DB at lines 313-314.
3. PI process exits; sidecar sees EOF on the pipe; connection-drop branch
   at `internal/sidecar/sidecar.go:2219-2224` flushes the accumulator. If
   the accumulator had partial text, it is written as a `msg_assistant`
   row; otherwise nothing is written. (For "no output produced" rounds
   1+2, the accumulator was empty here.)
4. Review monitor (`internal/review/monitor.go:128-138`) polls
   `GroupCompleted`; all members are in `finished` state so the group is
   complete.
5. `buildMonitorResults` at `internal/review/monitor.go:355-374` reads
   each member:
   - For empty-`LastMessage` rows: `Output = "ERROR: no output produced"`,
     `IsError = true`.
   - For non-empty-`LastMessage` rows lacking `<verdict>`: `Output =
     "ERROR: no verdict found in agent output — review output:\n" + text`,
     `IsError = true`. This is where the round-3 partial-text leak
     surfaces in the worker-facing prompt.
6. The monitor then checks `currentCycleProducedVerdicts(groupData)` at
   `internal/review/monitor.go:178`. Because at least one member (any of
   the three passing agents) is finished with non-empty `LastMessage`,
   the predicate returns `true`.
7. `CompletedReviewCyclesForParent` at
   `internal/review/monitor.go:750-815` is called to count prior
   verdict-producing cycles. After three rounds with this shape, the
   cumulative count is 3.
8. `prior+1 >= REVIEW_CYCLE_THRESHOLD` (3 ≥ 3) at
   `internal/review/monitor.go:182`, so the LOOP-LIMIT footer is appended
   to the delivery message via `buildLoopLimitFooter`
   (`internal/review/monitor.go:720-726`).
9. The worker reads the LOOP-LIMIT footer and escalates, even though the
   three substantive reviewers had agreed on PASS by round 1 or 2.

## Why this is not a true no-start

The existing `noStartSessions` machinery at
`internal/review/monitor.go:401-432` already distinguishes infrastructure
failures (container never bound its port) from code-quality verdicts. It
keys off `mr.StartupError != ""`, which is set by the sidecar's
`writeStartupError` when `WaitHealthy` or `CreateSession` fails.

The agents in this issue never hit `writeStartupError`. They started,
handshook, ran turns, and finished. `StartupError` is empty for them. So
the existing "infrastructure failure" branch in
`buildDeliveryMessage` does not apply, and the no-start exclusion in
`currentCycleProducedVerdicts` (the `StartupError == ""` check) is a
no-op for this case — it filters out the wrong category.

What we need is a **third category**: ran-but-no-parseable-verdict. The
sidecar already has the signal (`AssessPassed` returns `VerdictNone` for
these rows); only the cycle-counter predicate is missing the check.

## Proposed fix (deferred to follow-up issue #1995)

The minimal-risk fix is a one-line tightening of
`currentCycleProducedVerdicts`: require the `LastMessage` to contain a
parseable verdict tag, not just be non-empty. The same tightening applies
to `CompletedReviewCyclesForParent`'s `producedVerdict` predicate.

```go
// Sketch — not applied in this PR; tracked in a follow-up issue.
func currentCycleProducedVerdicts(groupData map[string]db.GroupMemberResult) bool {
    for _, mr := range groupData {
        if mr.State != "finished" || mr.LastMessage == "" || mr.StartupError != "" {
            continue
        }
        text := ExtractAssistantText(mr.LastMessage)
        if _, kind := AssessPassed(text); kind != VerdictNone {
            return true
        }
    }
    return false
}
```

This change has consequences that go beyond cycle counting and need to be
designed deliberately:

- **`buildDeliveryMessage` framing**: when no member has a parseable
  verdict, the delivery prompt should not present the round as a normal
  review failure. The existing "all-no-start" branch's wording ("NOT a
  code-quality verdict — re-run") is close but not quite right; the
  agents *did* run, the problem is that their output was unparseable.
  A new branch describing "agents ran but produced no parseable verdict;
  re-run" is needed.
- **Test surface**: `TestCurrentCycleProducedVerdicts_PositiveCase` and
  several historical cycle-counting tests use `LastMessage:
  "{\"text\":\"<verdict>PASS</verdict>\"}"`; they would continue to pass.
  But the design choice documented at
  `internal/review/monitor.go:794-798` explicitly endorses the lenient
  behaviour, so the comment must be updated and any test asserting the
  lenient contract refactored.
- **Edge cases**: a single member with state=`finished` and a verdict tag
  is enough to consider the cycle verdict-producing — that preserves the
  "5 PASS + 0 anything-else" happy path and the "4 PASS + 1 FAIL" happy
  path with no regression. The change only affects rounds where every
  agent is **either** verdict-producing **or** terminated without a
  parseable verdict, which is the exact #1993 shape.
- **Worker-facing change**: per AC #2(b), once the counter no longer
  ticks for these rounds, the worker will be free to re-run `prism review`
  rather than escalate at round 3. The skill doc text in
  `modules/programs/prism/skills/prism/SKILL.md` may need a sentence
  clarifying that ran-but-no-verdict rounds are treated like infra
  failures for counting purposes.

## Open questions for the follow-up

1. **Why does the model stop without emitting `<verdict>`?** The
   leaked round-3 text shows the agent had concluded "ACHIEVED" on at
   least one AC. Was the model running out of output tokens, hitting a
   provider-side `length` stopReason and the PI extension's
   `resolveTurnEndSignal` mapping that to `"none"` (no state_change
   frame) — then on the next turn the model either produces nothing or
   the harness teardown beats it? Capturing the raw `stopReason` from
   `pi/extensions/prism.ts:2097` into the archived events would settle
   this.
2. **Should `review-goal` / `review-code` prompts be tightened?** A
   prompt that demands "at least 5 edge cases × walk-throughs" before
   the verdict block invites the agent to spend its output budget on
   analysis and run out before the structured output. Moving the
   `<verdict>` block to the front of the response (instead of the end)
   would make it robust against truncation, at the cost of asking the
   agent to commit before reasoning.
3. **Should `stopReason="length"` be surfaced more loudly?** Today it
   silently maps to "none" and leaves the session in `StateActive` until
   the 15 min inactivity watchdog fires. A `length`-stopped turn is
   genuinely a "ran out of budget" signal that the review-monitor could
   use to classify the round as "agent truncated" — distinct from both
   "no-start" and "ran but didn't emit verdict".

## Verification

The diagnosis above is derived from source-code trace alone; PR #1992's
review-agent archives were cleaned up before this investigation started.
A live repro on an open PR is the natural way to confirm the trace
end-to-end and to capture the `stopReason` data needed for open question
#1.

## Follow-up

The fix is tracked in #1995, which covers the predicate change, the
delivery-message framing, the skill-doc update, and the test surface.
This document is the diagnosis input to that issue.
