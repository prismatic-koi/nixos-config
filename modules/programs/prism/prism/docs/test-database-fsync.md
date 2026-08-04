# test-database convention: open test databases with `sidecartest.OpenDB`

<!-- doclint-ignore: .github/workflows/pr-gate.yml -->
<!--
  The CI workflow file sits at the repository root, four levels above the
  prism source root. The nix sandbox build (runChecks = true) copies only the
  prism subtree, so LocateRoots returns an empty repoRoot there and the path
  has no fallback to resolve against. The reference is correct in the
  repository and resolves in the go-tests job, which has a full checkout. It
  is unresolvable only inside the sandbox. This is the same cross-boundary
  reason as the AGENTS.md annotations in docs/doclint.md and
  docs/podman-proxy.md.
-->

This document states the convention for opening a SQLite database in a test.
Before #2612, `db.Open` cost 73 `fsync` calls. That cost is invisible on
a developer host, and it grew large enough in `internal/sidecar` to fail CI
on a 10-minute timeout. The fix shipped in #2610. The paper trail is #2598.

#2612 reduced the cost of one open from 73 fsyncs to 7. The convention below
still stands: a template open costs zero, which is still less than 7.

## The fsync-per-open cost class

`db.Open` applies the declarative schema, seeds `schema_version`, and then
runs all 38 migrations. The DSN sets `journal_mode=WAL`, and SQLite keeps its
default `synchronous=FULL`, so each commit fsyncs the WAL.

Before #2612 each of those statements committed in autocommit mode, and one
`db.Open` on a fresh file cost **73 fsyncs**. Since #2612 the sequence runs
inside one transaction and the same open costs **7** fsyncs. Measure it yourself:

```
go test -c -o /tmp/pkg.test ./internal/db/
strace -f -c -e trace=fsync /tmp/pkg.test -test.run '^TestProbeFreshOpen$'
```

`TestProbeFreshOpen` in `internal/db` does one `db.Open` and nothing else, so
it is the probe to use for this number. `strace` counts the fsync syscall on
any filesystem, so the count is identical on tmpfs and on a real disk.

fsync latency is different. Latency is near zero on tmpfs, which is why the
cost is invisible on a developer host. On a CI runner, fsync latency is real
and set by disk I/O health, which is why the cost is visible.

A package that opens one database per test multiplies that number by its test
count. `internal/sidecar` opened about 700 test databases per run and paid
about 51,000 fsyncs before it ran a single assertion.

This cost is invisible on a developer host, because the test tempdir is a
tmpfs and fsync latency is near zero. It is not invisible on a CI runner.
Package wall time is CPU work plus fsync count times per-fsync latency, and
the second term is set by runner IO health, not by the code under test. When
one hosted runner degraded, each SQLite-backed package inflated 2.4x to 4.6x
and each package with no database stayed flat. `internal/sidecar` was the
longest of them, so it crossed the 600s default `go test` timeout first, and
the panic named an unrelated 2-second test that happened to be in flight.

## The convention

> Open a test database with `sidecartest.OpenDB(t, path)`. Do not call
> `db.Open` directly from a test.

`OpenDB` builds one fully-migrated database per test binary, holds its bytes
in memory, and stamps a copy at the caller's path before it calls `db.Open`.
The first call pays the cost of one fresh open. Every later call pays none,
because every statement `db.Open` runs against a migrated database is
idempotent, so SQLite starts no write transaction and writes no WAL frame.

The database the caller receives is equivalent to one from a plain `db.Open`:
same schema, same `schema_version`, same file mode. The caller owns the handle
and must close it.

The implementation is
`modules/programs/prism/prism/internal/sidecar/sidecartest/templatedb.go`.

`TestSidecarTests_UseSidecartestOpenDB` in `internal/sidecar` enforces the
convention for that package. It parses every Go file under `internal/sidecar`
and `internal/sidecar/sidecartest` and fails on a direct `db.Open` call
outside an exempt list.

The matcher resolves the import path, not the identifier text, so an aliased
import (`prismdb "…/internal/db"`) and a dot import are both detected.
`db.OpenReadOnly` is not a hit: it does no writes and costs no fsync. The
matcher works on one file at a time and does not resolve types, so a local
variable that shadows the package name produces a false hit. A false hit is a
visible failure, not a silent hole.

`TestDirectDBOpens_Matcher` calls the same matcher function the guard runs,
over a table of samples that includes the aliased and dot-import forms. A
break in the matcher fails that test, so the guard cannot go silently blind.

## The two exceptions

Call `db.Open` directly in these two cases only:

1. **The test drives the migrations.** `internal/db` opens databases at old
   schema versions on purpose. A pre-migrated template defeats the point.
2. **The test asserts on `db.Open` itself.** For example, a test of the
   pre-flight probe or of the error text on an unwritable path.

Both cases are the point of the test, not incidental setup.

## Scope

This convention applies to test code. Production code is unchanged: the prism
database keeps `synchronous=FULL` and keeps its per-commit durability. Do not
relax durability in production to make a test suite faster.

## Known remaining exposure

Two packages carried the cost and were the next to hit the timeout. Neither was
fixed by #2610. Both are tracked in #2611. The two tables show different
measurements because the second one includes the tests that PR #2629 adds:

| package | fsyncs / run before #2610 | wall time on a degraded runner |
|---|---:|---:|
| `internal/db` | 41,224 | 491s against a 600s limit |
| `cmd` | 37,157 | 494s against a 600s limit |

`.github/workflows/pr-gate.yml` runs `go test -v ./... -race` with no explicit
`-timeout`, so the Go default of 10 minutes applies per package binary.

#2612 reduced both. The second table is measured on one developer host with a
real disk (for fsync latency measurement):

| package | before #2612 | after #2612 |
|---|---:|---:|
| `internal/db` | 44,730 | 27,902 |
| `cmd` | 37,670 | 4,540 |

`cmd` drops by a factor of 8, because almost all of its cost was `db.Open`.
`internal/db` drops by less than half, because most of its remaining cost is
not `db.Open` at all: its tests write rows directly, and each of those writes
is its own commit. Its migration tests also seed databases at old schema
versions on purpose, and those take the autocommit open path by design. Re-scope
#2611 against these numbers rather than the pre-#2612 ones.

## Why one open still costs 7 fsyncs, not 2

Of the 7, about 3 belong to the DSN and not to the open sequence: SQLite
creates a new file in rollback-journal mode and the `journal_mode=WAL` pragma
then converts it, which commits through the rollback journal. The rest is the
single WAL commit of the batched sequence. A second open of the same file
costs zero, as it did before #2612.

## What #2612 did not batch

Four migrations rebuild a table (`DROP TABLE` plus `ALTER TABLE ... RENAME
TO`) and toggle `PRAGMA foreign_keys` around the rebuild: v8→v9, v23→v24,
v25→v26 and v37→v38. `PRAGMA foreign_keys` is a silent no-op inside a
transaction, so a rebuild batched into the open transaction would run with
foreign-key enforcement still on. That raises no error on an empty database,
so a suite built from fresh files would stay green and the break would appear
only on a populated database.

`batchableOpen` in `internal/db/db.go` therefore probes the database before it
opens the transaction. When one of those four still has work to do, the whole
open falls back to autocommit and behaves exactly as it did before #2612.
`TestRebuildMigrationSet_MatchesProbe` fails if a new migration rebuilds a
table without being added to that probe.

## References

- #2598 — the flake this convention answers.
- #2610 — the PR that added `sidecartest.OpenDB` and this document.
- #2611 — the same cost in `cmd` and `internal/db`.
- #2612 — the same cost on every `prism` CLI invocation.
- `modules/programs/prism/prism/docs/stdout-capture-testing.md` — the sibling
  convention for the pipe-buffer deadlock class, in the same shape: a
  host-hidden cost that only CI reveals.
