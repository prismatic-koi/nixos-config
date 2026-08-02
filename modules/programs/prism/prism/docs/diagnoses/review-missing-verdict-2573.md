# Diagnosis — a review agent that produces no verdict is counted as a full round (issue #2573)

<!-- doclint-ignore: modules/programs/prism/skills/prism/SKILL.md -->
<!--
  The path above resolves against the same repo, but only in a full
  checkout — in the nix sandbox only the prism Go subtree is present.
  Same class of cross-boundary reference as the annotations in
  review-agent-no-verdict-1993.md and doclint.md.
-->

## Symptom

On PR #2568 the `review-qa` agent failed in all three review rounds with:

    agent session not found in group (possibly deleted mid-review)

Its four siblings completed normally in each round. The round still reported
as complete, and it consumed a review cycle each time. QA never produced a
verdict on that PR at any point.

Four verdicts plus one blank reads as "four passed". The lost dimension was
QA — the review class that produced #2561 (862 extension tests that ran on no
PR) and #2566 (a test assertion that could not fail).

## Root cause: the session was reaped, not lost

The group did not lose track of a running session. The `agent_status` row was
closed — `ended_at` was written — while the round was in flight, and the
read that builds the report drops closed rows on purpose.

`db.GroupResults` (`internal/db/groups.go`) reads:

```sql
SELECT session_name, state, COALESCE(root_agent_name, '')
FROM agent_status
WHERE group_id = ? AND ended_at IS NULL
```

The `ended_at IS NULL` filter is deliberate. It is the #1495 escape hatch:
`prism cleanup --yes --session <agent>` closes an interrupted review agent's
row without rewriting its state, and dropping the row here is what lets the
monitor finish the round instead of waiting forever.

Two facts follow, and together they produce the silent failure:

1. `db.GroupCompleted` counts a row with `ended_at` set as terminal, so the
   round completes normally. The group's `group_id` linkage is intact
   throughout — nothing is "lost".
2. Every consumer of the results read only the `GroupResults` map. A member
   that is absent from that map is therefore invisible:

   - `buildDeliveryMessage` scanned the map for no-start and stall classes,
     so an absent member matched no class and the report fell through to the
     ordinary code-FAIL header ("One or more review agents failed. Fix the
     blocking issues …").
   - `currentCycleProducedVerdicts` iterated the map's values. With four
     entries, all carrying verdicts, it returned `true`, so the round counted
     against the 3-cycle LOOP-LIMIT.
   - `CompletedReviewCyclesForParent` applied the same map-only predicate to
     historical groups, so past reaped rounds also counted.

Only `buildMonitorResults` noticed, because it walks the expected agent list.
It emitted one `IsError` entry with the "possibly deleted mid-review" text —
a guess, written when the branch was added, that never distinguished "row
deleted" from "row closed".

### Which path closed the row

Every path below writes `ended_at` and any of them produces this shape:

| Caller | State left behind |
|---|---|
| `prism event tmux-session-end` (tmux hook) | unchanged (`active`, `finished`, …) |
| sidecar `handleSessionDeleted` (harness `session.deleted`) | `deleted` |
| `cleanupHalfAliveSession` (readiness-gate failure) | `error` |
| `prism cleanup` | `interrupted` (usually) |
| `prism reset` (`MarkAllEnded`) | unchanged |
| monitor safety-timeout sweep (`forceTerminateStuckMembers`) | `error` |

The #2568 record cannot say which one fired: the report threw the row away,
so the state and the closing time were never surfaced, and the rows have
since been pruned. That gap is itself part of the defect. The report now
reads the closed row and prints its state and `ended_at`, plus the most
likely caller for that state, so the next occurrence names the path.

Row deletion — the other way to be absent — is possible but was not the
cause here: `db.Prune` only deletes rows that are already ended and
older than the retention threshold, which no in-flight review member can be.

## Fix

`ClassifyRound` in `internal/review/roundstatus.go` is now the single
classifier. It walks the **expected** agent list rather than the map that
came back, so an absent member is a first-class outcome with a class and a
reason:

| Class | Meaning | Infrastructure |
|---|---|---|
| `NoVerdictNoStart` | `startup_error` event — the agent never ran (#1222) | yes |
| `NoVerdictStalled` | `stall_error` event — ran, then went silent (#2239) | yes |
| `NoVerdictCrashed` | state `error` with neither event | yes |
| `NoVerdictSessionEnded` | row closed mid-round (this issue) | yes |
| `NoVerdictSessionUnknown` | no row could be read at all | yes |
| `NoVerdictNoOutput` | finished with no assistant message (#1995) | no |
| `NoVerdictUnparseable` | finished with no `<verdict>` tag (#1995) | no |
| `NoVerdictUnexpectedState` | non-terminal when the group completed | yes |

`RoundStatus.CountsAsCycle` is `Complete()`: a round counts against the
3-cycle limit only when every expected agent produced a parseable verdict.
The in-flight gate in `MonitorFunc` and the historical count in
`CompletedReviewCyclesForParent` both resolve to it, so the report and the
counter cannot drift. This extends the existing "rounds that do not count"
machinery (#1512, #1995, #2239) rather than adding a parallel mechanism.

The delivery message gains:

- a `Round incomplete: N of M review agents produced a verdict` header for
  any round with a missing verdict, in place of the code-FAIL header;
- an `Agents with no verdict` section naming each agent, its class, and the
  reason recorded for it;
- the targeted re-run command, e.g. `prism review 2568 --only review-qa`.

A round in which all five agents produce verdicts is unchanged, and still
consumes one cycle.
