# stdout-capture testing convention

This document codifies the testing pattern for any helper that redirects
`os.Stdout` (or `os.Stderr`) through an `os.Pipe` in order to capture what a
command under test writes. It exists because the previous
`captureStdout` in `cmd/checkin_test.go` drained the read end of the pipe
**after** the function under test returned, and that pattern is a latent
deadlock — invisible until the captured output grows past the kernel
pipe buffer.

The fix shipped in #1797. This convention exists to keep it from
re-emerging. The paper trail is #1798.

## The kernel-pipe-buffer deadlock class

On Linux, an `os.Pipe` is backed by a kernel ring buffer of 16 pages
(≈ 64 KiB). Once the buffer fills, the next `write(2)` blocks until a
reader drains bytes from the read end. If the test helper only starts
reading **after** the function under test returns, and the function
writes more than the buffer in a single contiguous burst, the writer
deadlocks forever — `go test` then hangs until its 10-minute timeout
fires.

The worst offender in this repo is the `agent-context` JSON document
emitted by `runAgentContext` (`cmd/agent_context.go`), which is
currently ~69 KiB on `main`. `TestAgentContextCoversAllCommands`
previously only passed by happenstance of how `encoding/json`'s
`Encoder` fragments its writes against the pipe; adding a single
top-level cobra subcommand was enough to push it over the threshold
and trigger the hang.

## The convention

> Any test helper that redirects `os.Stdout` or `os.Stderr` through an
> `os.Pipe` must drain the read end of the pipe **concurrently** with
> the function under test — typically a `sync.WaitGroup` + goroutine
> that `io.Copy`s into a `bytes.Buffer` while `fn()` runs, then
> `w.Close()` + `wg.Wait()` after `fn()` returns. Do not buffer the
> output by reading only after `fn` returns.

The canonical implementation lives in
`modules/programs/prism/prism/cmd/checkin_test.go::captureStdout`. New
helpers should either reuse `captureStdout` (preferred) or follow the
same drain-in-goroutine shape. Do not duplicate the pre-#1797 pattern
in a new helper.

## When to re-verify the agent-context test under stress

`agent-context` is the largest stdout-bound command in the tree, so
its output is the canary for this deadlock class. Whenever its output
grows substantially — new always-on context fields, deeply nested
command trees, large embedded help text — re-run
`TestAgentContextCoversAllCommands` under `-race -count=20` from
`modules/programs/prism/prism/`:

```
go test ./cmd/ -run TestAgentContextCoversAllCommands -race -count=20
```

If the test ever hangs rather than failing cleanly, the helper has
regressed to the buffered-read pattern.

## Scope

This convention applies to test code only. Production stdout writers
must not change behaviour for the sake of tests. The fix is always in
the capture helper, never in the writer.

## References

- #1797 — the PR that fixed `captureStdout` with the drain-in-goroutine
  pattern.
- #1798 — this issue: codifies the convention so the fragility class
  stays visible.
- `modules/programs/prism/prism/cmd/checkin_test.go::captureStdout` —
  the canonical example of the correct pattern.
