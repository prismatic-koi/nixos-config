# Iris parity gate (D-10)

This directory contains the end-to-end test suite that proves iris is at
parity with prism on the §10.3 feature checklist. **Closing this gate is
necessary, but not sufficient, for D-11** (the rename + dead-code-deletion
PR). D-11 remains user-gated on battle-testing per §10.4 of
`docs/daemon-mode-design.md`.

The suite implements issue #1641 ("D-10 parity gate"). The parent rollout
tracker is #1625. Depends on D-3 (#1634), D-4 (#1635), D-5 (#1636), D-6
(#1637), D-7 (#1638, #1660), D-8 (#1639), D-9 (#1640).

## §10.3 checklist → test file mapping

Each test file maps one-to-one with a checklist item so a single failing
parity item is mechanically grep-able. The file `parity_isolation_test.go`
implements the cross-cutting security and isolation assertions.

| §10.3 item                                | Test file                       |
|-------------------------------------------|---------------------------------|
| Spawn worker                              | `spawn_worker_test.go`          |
| Spawn coordinator (default-agent + bash perm) | `spawn_coordinator_test.go` |
| Deliver prompts to running sessions       | `prompt_deliver_test.go`        |
| Show the dashboard                        | `dashboard_test.go`             |
| Checkin (read conversation history)       | `checkin_test.go`               |
| Run review (5 review agents, group)       | `review_test.go`                |
| Merge queue                               | `mergequeue_test.go`            |
| Archive sessions                          | `archive_test.go`               |
| Restore sessions after reboot             | `restore_test.go`               |
| Cleanup                                   | `cleanup_test.go`               |
| Isolation + tripwire (no-prism, no-host)  | `parity_isolation_test.go`      |

## Isolation contract

Every parity test calls `iristest.NewIsolated(t)` *before* any iris code
runs. NewIsolated:

- redirects `$HOME`, `$XDG_STATE_HOME`, `$XDG_CONFIG_HOME` to subdirs of a
  `t.TempDir()` so `iris.ResolvePaths()` resolves entirely under the
  tempdir;
- sets `$IRIS_PARITY_TEST_MODE=1` — the prism binary's `main()` checks
  this env var and exits 99 with a clear error before any prism-specific
  work runs (see `prism/main.go`). Any parity test that accidentally
  invokes the prism binary therefore fails fast and visibly.

The two layered guards (tempdir XDG redirection + prism tripwire) are the
mechanisms chosen for the D-10 security ACs:

> No parity test invokes any `prism` binary, `prism` Go package, or
> `~/.local/state/prism/` filesystem path.

> No parity test reads or writes the real host paths
> `~/.local/state/iris/iris.db`, `~/.local/state/iris/iris.sock`,
> `~/.local/state/iris/run/`, or `~/code/archives/iris/`.

`parity_isolation_test.go` asserts both at suite startup so a broken
isolation is caught before any other parity test runs.

## "Spawn" without a real pi child

A real `pi --mode rpc` child process is expensive (Node startup,
extension loading, model handshake). The parity suite emulates the pi
side of the harness socket with a Go-implemented test client that dials
the harness socket, performs the `hello/hello_ack` handshake, and then
either acts as an extension (for the spawn tests) or as a shell script
launched by the supervisor (when we need the supervisor's spawn
sequence). This keeps the suite well under the 5-minute time budget
required by the AC.

## What this suite is NOT

- Not a unit test for any individual iris component — those live next to
  the code under `internal/iris/`.
- Not a "test that prism still works" — D-10 is exclusively about iris.
- Not a verdict-correctness test for the review agents — the review
  parity test stubs the agent bodies; the contract is the orchestration
  group lifecycle, not the verdict.

## Running

```bash
# From modules/programs/prism/prism/
go test ./internal/iris/parity/... -race
```

Total runtime target: < 5 minutes p99 on a free-tier Ubuntu 24.04
runner. If a single test exceeds 60 seconds it is documented in a
comment immediately above the test function.
