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
It exists because `db.Open` costs 73 `fsync` calls, that cost is invisible on
a developer host, and it grew large enough in `internal/sidecar` to fail CI
on a 10-minute timeout. The fix shipped in #2610. The paper trail is #2598.

## The fsync-per-open cost class

`db.Open` applies the declarative schema, seeds `schema_version`, and then
runs all 39 migrations. Each statement commits in autocommit mode. The DSN
sets `journal_mode=WAL`, and SQLite keeps its default `synchronous=FULL`, so
each commit fsyncs the WAL.

One `db.Open` on a fresh file costs **73 fsyncs**. Measure it yourself:

```
go test -c -o /tmp/pkg.test ./internal/sidecar/
strace -f -c -e trace=fsync /tmp/pkg.test -test.run '^TestSomething$'
```

A package that opens one database per test multiplies that number by its test
count. `internal/sidecar` opened about 700 test databases per run and paid
about 51,000 fsyncs before it ran a single assertion.

This cost is invisible on a developer host, because the test tempdir is a
tmpfs and fsync is a no-op there. It is not invisible on a CI runner. Package
wall time is CPU work plus fsync count times per-fsync latency, and the second
term is set by runner IO health, not by the code under test. When one hosted
runner degraded, each SQLite-backed package inflated 2.4x to 4.6x and each
package with no database stayed flat. `internal/sidecar` was the longest of
them, so it crossed the 600s default `go test` timeout first, and the panic
named an unrelated 2-second test that happened to be in flight.

## The convention

> Open a test database with `sidecartest.OpenDB(t, path)`. Do not call
> `db.Open` directly from a test.

`OpenDB` builds one fully-migrated database per test binary, holds its bytes
in memory, and stamps a copy at the caller's path before it calls `db.Open`.
The first call pays 73 fsyncs. Every later call pays none, because every
statement `db.Open` runs against a migrated database is idempotent, so SQLite
starts no write transaction and writes no WAL frame.

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

Two packages still carry the cost and are the next to hit the timeout. Neither
is fixed by #2610. Both are tracked in #2611:

| package | fsyncs / run | wall time on a degraded runner |
|---|---:|---:|
| `internal/db` | 41,224 | 491s against a 600s limit |
| `cmd` | 37,157 | 494s against a 600s limit |

`.github/workflows/pr-gate.yml` runs `go test -v ./... -race` with no explicit
`-timeout`, so the Go default of 10 minutes applies per package binary.

Production pays the same 73 fsyncs on every `prism` CLI invocation, because the
schema and all 39 migrations run in autocommit mode at each start. One
transaction around the open sequence cuts that to about 2. That is tracked in
#2612 and is not a correctness problem.

## References

- #2598 — the flake this convention answers.
- #2610 — the PR that added `sidecartest.OpenDB` and this document.
- #2611 — the same cost in `cmd` and `internal/db`.
- #2612 — the same cost on every `prism` CLI invocation.
- `modules/programs/prism/prism/docs/stdout-capture-testing.md` — the sibling
  convention for the pipe-buffer deadlock class, in the same shape: a
  host-hidden cost that only CI reveals.
