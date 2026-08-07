# Diagnosis: `review-qa` reaped in two consecutive rounds (#2613)

Issue: #2613. Parent that surfaced it: `nixos-config@fix-flaky-logs-tail-test`
(PR #2610, issue #2598).

## Symptom

Rounds 3 and 4 of the review on PR #2610 closed the `review-qa`
`agent_status` row in state `error` with `ended_at` stamped. The other four
agent classes completed normally in both rounds. Both rounds reported the same
text:

    ERROR: agent produced no verdict — session ended mid-review: the
    agent_status row was closed at <ts> in state "error", so it is excluded
    from the group results — the session was force-terminated, or its
    readiness gate failed

## Finding 1: the report could not name a cause, by construction

`endedRowHint` in `internal/review/roundstatus.go` mapped the state string of a
closed row to a hint. Several paths close a row in state `error`, so the hint
for that state named two of them with an `or`. The report therefore carried a
disjunction for every row of that shape, whatever closed it.

Nothing else in the DB narrowed it. `db.GroupResults` reads
`agent_status WHERE group_id = ? AND ended_at IS NULL`, so a closed row is
dropped from the results map. The classifier read the row from
`GroupMembersForGroup` and had two facts: `state` and `ended_at`.

## Finding 2: one half of the disjunction was unreachable

A readiness-gate failure could not produce that message.

1. `gateReviewAgents` (`internal/review/readiness.go`) writes `spawnErr[i]` for
   an agent that does not signal readiness, then cleans the session away.
2. `RunAsync` (`internal/review/run.go`) built `liveAgents` / `liveSessions`
   from the indices where `spawnErr[i] == nil`, and passed only those to
   `MonitorOpts.Agents` / `MonitorOpts.AgentSessions`.
3. `ClassifyRound` walks exactly the list it is given.

So an agent reaped by the readiness gate was not in the set the classifier
examined. It never reached `classifyAbsentMember`. It surfaced only in the
synchronous Ack at `prism review` time, as
`Spawned: 4, Failed: 1 (review-qa: not ready within 30s)`.

The report named a cause that the code cannot produce for that message.

## Finding 3: the shrinking expected set was a live safety-property defect

Finding 2 has a second consequence, and it is the more serious one.

`RoundStatus.Expected` is `len(agents)`, taken from the list the monitor was
given. With a shrunk list, a five-agent round in which one agent failed its
readiness gate produced `Expected = 4`, `Missing = 0`. `Complete()` returned
true, so:

- `buildDeliveryMessage` took the `allPassed && status.Complete()` branch and
  printed **"All 5 review agents passed."** — a literal five, for four agents.
- `CountsAsCycle()` returned true, so the round consumed one of the worker's
  three review cycles.

That is the failure #2573 closed for a mid-round reap, reopened on the
spawn-time path: a dimension that was never examined read as a pass.

## Finding 4: a recorded stall could be relabelled as an unexplained reap

The sidecar's inactivity watchdog (`internal/sidecar/state.go`) sets
`state="error"` and writes a `stall_error` event. It does **not** stamp
`ended_at`, so the row stays in `GroupResults` and reports as
`stalled mid-run` — the #2239 path, working as designed.

If the tmux session then closes, `prism event tmux-session-end`
(`cmd/event.go`) stamps `ended_at` and rewrites no state. The row is now
`state="error"` with `ended_at` set. `GroupResults` drops it,
`classifyAbsentMember` takes over, and `classifyAbsentMember` did not read
`agent_events` at all. The recorded stall reason — elapsed time, frame count,
last-frame timestamp — was discarded, and the row reported as an unexplained
reap with the disjunction attached.

This is the most likely account of PR #2610 rounds 3 and 4. `review-qa` runs
the heaviest workload of the five reviewers (it ran an independent fsync
measurement on that PR), so it is the class most exposed to the inactivity
watchdog. It cannot be confirmed from the report alone, which is the point of
the issue: the report threw the evidence away.

## Finding 5: port recycling is not the cause

The issue's leading hypothesis was that the allocator can hand out a port that
is still held, and that round 4's `review-qa` failed because it drew 14006,
released shortly before by a torn-down session. Three independent measurements
close it. They live in `internal/db/port_recycling_2613_test.go`.

1. **A live listener is detected.** `portAvailable` binds
   `127.0.0.1:<port>`. `internal/sidecar/sidecar.go` binds the harness pipe at
   `127.0.0.1:<HarnessPipeTCPPort>` — the same address. A port a listener
   holds fails the probe and the allocator skips it.

2. **`TIME_WAIT` does not hold a port.** Go's `net.Listen` sets
   `SO_REUSEADDR`, so a socket in `TIME_WAIT` blocks neither the probe nor the
   sidecar's later bind. The test drives a real connection, closes it from the
   listening side so the local end lingers, and asserts that the allocator
   still offers the port and that the address is still bindable.

3. **The range cannot be taken by an ephemeral port.** The allocation range is
   14000–14999. The lowest default Linux ephemeral port is 32768, so no
   kernel-chosen local port can occupy a prism port in the window between the
   probe and the bind.

Finding 2 is decisive on its own and does not depend on any of the above: even
if a bind had failed, the resulting readiness-gate cleanup could not have
produced the message the issue quotes.

## The fix

1. **Each review-agent closing path records why.** `db.RecordSessionReap`
   writes a `session_reaped` event carrying one `SessionReapCause` before the
   row closes. Call sites: `gateReviewAgents`, the review spawn-failure
   cleanup, `cleanupHalfAliveSession` (`SpawnSession`'s layout and
   inline-readiness failures, which review agents reach through the shared
   spawn primitive), `forceTerminateStuckMembers`,
   `CleanupReviewSessionsForParent`, and `applyDBLifecycleClears`.

   The record is guarded on `ended_at`, so a path that finds the row already
   closed records nothing. `SessionEndCauses` returns the latest record, so an
   unguarded second write would overwrite a row's true cause with a false one
   — reachable when `prism cleanup` of a parent worker cascades over a row a
   readiness gate closed hours earlier. Naming a wrong cause is worse than the
   disjunction this change removes.

   This is scoped to the review-agent lifecycle, not to every path in prism
   that stamps `ended_at`, and **not** to every closed row in state `error`.
   The state and the close can come from different paths: Finding 4 above is
   exactly that case — the watchdog sets `error` and writes `stall_error`
   without stamping `ended_at`, and the tmux hook later stamps `ended_at`
   without touching state. Neither writes a `session_reaped` event, and
   neither needs to, because the classifier reads `startup_error` and
   `stall_error` ahead of the reap cause. The harness `session.deleted`
   handler is the same shape, reported from state `deleted`.

   Operator and maintenance paths record nothing and stay out of scope:
   `closeSession` and `doCleanup` in `cmd/cleanup.go`, `cmd/restore.go`,
   `cmd/switch_project.go`, `cmd/spawn.go`, and `db.MarkAllEnded`
   (`prism reset`). A row closed by one of those reports `no close cause was
   recorded for this row` — the honest answer, and what the pre-#2613 code
   could not say.

2. **The classifier reads it.** `db.SessionEndCauses` batch-reads the reap
   cause plus the sidecar's `startup_error` and `stall_error` events for the
   closed rows. `classifyAbsentMember` resolves them in causal order — the
   agent's own failure outranks the later close — and two new classes,
   `failed its readiness gate` and `force-terminated`, replace the disjunction.
   When nothing is recorded, the report says so rather than guessing.

3. **The expected set is never shrunk.** `expectedRoundSet` returns the full
   spawned set, and `RunAsync` passes it to the monitor. An agent that failed
   its readiness gate now stays in the expected set, reports as a missing
   verdict, and keeps the round incomplete.

## What did not change

The #2573 contract is intact and is now enforced on one more path:

- A round in which any agent produces no verdict is incomplete.
- A missing verdict reads as unreviewed, never as a pass.
- The round does not count toward the 3-cycle limit.

No automatic retry of a reaped agent was added. That is a design decision, not
a defect fix, and it is deferred rather than guessed at.

## References

- #2610 / #2598 — the parent that surfaced this
- #2573 — the incomplete-round contract (`review-missing-verdict-2573.md`)
- #2239 — the mid-run stall class the tmux-session-end race could erase
- #1222 — the no-start class
- #1051 — the per-agent readiness gate
