# Doc-lint convention

<!-- doclint-ignore: mountTypeAllowlist, bind_source_outside_allowlist:<path>, agent_status.work_dir -->
<!-- doclint-ignore: CgroupBudget -->
<!-- doclint-ignore: refs/stash, AllPackages.nix, allPackages.nix -->
<!-- doclint-ignore: AGENTS.md -->
<!--
  Every token in the doclint-ignore lists above is intentionally
  unresolvable and appears in this doc as a historical example of the
  class of drift the lint catches:

  - `mountTypeAllowlist`, `bind_source_outside_allowlist:<path>`, and
    `agent_status.work_dir` are three of the nine stale identifiers
    caught across the three review cycles of PR #2333. They are cited
    verbatim in the "Why this lint exists" section as concrete drift
    examples, so they MUST remain unresolvable — rewriting them to real
    identifiers would strip the section's point.
  - `CgroupBudget` is referenced as the annotation example in the
    "Annotation — opting out of a specific finding" section; it is a
    hypothetical field in the podman-proxy field-admission walkthrough.
  - `refs/stash`, `AllPackages.nix`, `allPackages.nix` are the same
    counter-example set carried over from the AGENTS.md doclint-ignore
    block, mentioned here as canonical worked examples of when to reach
    for the annotation.
  - `AGENTS.md` is a cross-boundary reference to the repo-root file. In
    a full checkout the basename resolves; in the nix sandbox where
    only the prism subtree is copied in, it does not exist. Same
    situation as the equivalent annotation in podman-proxy.md.
-->

This document specifies the doc-lint that verifies backticked identifier-shaped
tokens in prism's markdown docs resolve against the current source tree.

The lint is implemented in
[`internal/doclint`](../internal/doclint/) and enforced by
`TestDocsResolve` under `go test ./...` — it therefore inherits the existing
pr-gate enforcement (both the `go-tests` CI job and the homeless-shelter
`nix-build-prism-checked` job run it) via issue #2334.

## Why this lint exists

Backticked identifiers in markdown drift from the source they reference as
the code evolves — functions get renamed, fields get added/removed, files
get split. PR #2333 (the podman-proxy train's closer) went through three
review cycles of `review-code` and `review-context` catching stale
identifiers by hand: `bind_source_outside_allowlist:<path>` where the
source emits `host_bind:<path>`, `mountTypeAllowlist` where the source has
an inline `switch`, `agent_status.work_dir` where the column is
`instance_id`, and six others. Each cycle's fixup commit closed one batch
and missed another 1–3 of the same class, which the next cycle then
caught.

That pattern — cycle-by-cycle catching identifier drift — is a defining
signature of "needs a structural fix, not more review." Review cycles cost
~10 minutes × 5 agents each; a `grep` catches these in seconds and can
run on every prism-touching PR. Issue #2334 tracks the fix; this document
is its operational spec.

## What the lint checks

For each markdown file in scope (see [Scope](#scope) below), the lint:

1. Extracts every backticked span outside fenced code blocks.
2. Strips trailing punctuation and skips tokens that are placeholders
   (`<sessionName>`), URLs, CLI flags, quoted content, assignments, and so on
   — see `internal/doclint/classify.go` for the full skip matrix.
3. Classifies each surviving token into one of the identifier classes below.
4. Attempts to resolve the token against an index of the prism source tree.
5. Reports every unresolved token with the file, line, offending token, and
   the resolution rule that was attempted.

### Token classes and resolution rules

| Class | Example | Resolution rule |
|---|---|---|
| `file_path` | `` `internal/podmanproxy/policy.go` `` | `os.Stat` against `<prismRoot>/<path>` and `<repoRoot>/<path>`. |
| `file_with_member` | `` `policy.go::checkHostConfig` `` | The file must exist (bare basename resolves against the walked file index; a full relative path resolves via `os.Stat`), and the member must appear as an identifier in some indexed source file. |
| `bare_filename` | `` `proxy_test.go` `` | The basename must exist somewhere under the walked source tree. |
| `dotted` | `` `Config.MaxMemoryBytes` `` or `` `agent_status.instance_id` `` | Every segment must appear as an identifier in some indexed source file. SQL `table.column` references resolve because CREATE TABLE / SELECT strings contain the column and table names as word tokens. |
| `go_ident` | `` `checkHostConfig`, `NewIsolated` `` | Must appear as an identifier in some indexed source file. Requires mixed case (both upper- and lower-case letters) — pure lowercase words are treated as English prose and skipped. |
| `snake_case` | `` `agent_max_open_files_soft` `` | Must appear as an identifier in some indexed source file. |
| `env_var` | `` `CONTAINER_HOST` `` | Must appear as an identifier in some indexed source file. All-caps with underscores and length ≥ 3. |
| `colon_token` | `` `host_bind:<path>`, `cap_add:SYS_ADMIN` `` | The prefix (before `:`) must appear as an identifier OR as a substring of some Go string literal. When the suffix is not a placeholder or plain value, it is recursively resolved against the same rule set. |

The rules are deliberately conservative. When a token is ambiguous — for
example, a lowercase word that could be Go prose or a Go identifier —
the lint skips it. High precision beats high recall: a lint that
false-positives on unrelated PRs will get deleted.

### Scope

- `modules/programs/prism/prism/docs/*.md` — always scanned; these live
  inside the prism subtree that the nix sandbox build copies in.
- The repo-root `AGENTS.md` — scanned when it is present. Inside the nix
  sandbox (`runChecks = true`, only the prism subtree is copied in),
  it is absent and the lint skips it gracefully.

The source index that resolution runs against covers:

- Go source under `modules/programs/prism/prism/` (always).
- Nix source anywhere under the repo (walked recursively, when the repo
  root is available).
- TypeScript / JavaScript source under `modules/programs/prism/pi/` (when
  the repo root is available).

## Annotation — opting out of a specific finding

Some backticked identifiers are intentionally unresolvable — for example,
a hypothetical field used in a walkthrough (`CgroupBudget` in the podman
proxy field-admission walkthrough), a name that describes an external
system (git internals like `refs/stash`), or a deliberate counter-example
(`AllPackages.nix` used in the file-naming rule).

Two annotation directives are recognised, both as HTML comments so they
do not render in the visible doc:

### Per-token: `<!-- doclint-ignore: token1, token2 -->`

```markdown
<!-- doclint-ignore: CgroupBudget, mountTypeAllowlist -->
```

Lists tokens to exempt from the lint for this file. Whitespace inside the
list is ignored. Multiple directives per file are allowed and their lists
union together.

Best practice: add a follow-up HTML comment explaining WHY the token is
intentionally unresolvable, so future readers do not silently promote the
annotation from "hypothetical" to "vanished from source":

```markdown
<!-- doclint-ignore: CgroupBudget -->
<!-- `CgroupBudget` is a hypothetical field used in the field-admission
     walkthrough in §4. It does not exist and is not expected to. -->
```

### Per-file: `<!-- doclint-skip-file: reason -->`

```markdown
<!-- doclint-skip-file: this doc describes the external pi coding-agent RPC interface, not the prism Go source. -->
```

Opts the entire doc out of the lint. Use only for docs whose identifiers
live in a codebase outside this repository (e.g. the wire-protocol and RPC
specs for the external pi coding-agent). The reason text after the colon
is required so the exemption is self-documenting; its content is not
otherwise inspected.

Prefer per-token `doclint-ignore` over the whole-file skip. A file that
mixes in-tree and out-of-tree references should annotate the out-of-tree
tokens individually so drift on the in-tree ones still gets caught.

## Failure output

When the lint fails, `TestDocsResolve` prints one line per finding in
this shape:

```
/abs/path/to/AGENTS.md:60: unresolved `darwinConfigurations` (rule=go_ident): identifier not found anywhere in prism .go source
```

The `rule=` tag names which resolution path was tried. Use it to debug
false positives (usually the token classifies into an unexpected class)
and false negatives (usually a case-sensitivity or word-boundary issue in
the source index).

## Dual-context: full checkout vs nix sandbox

The test runs in two environments and must pass in both:

1. **Full repo checkout** — the `go-tests` CI job and local `go test ./...`.
   Both `modules/programs/prism/prism/docs/*.md` and the repo-root
   `AGENTS.md` are scanned. The index covers Go, Nix, and pi-TS source.
2. **Nix sandbox** — the `nix-build-prism-checked` CI job with
   `runChecks = true`. Only the prism subtree is copied into the build;
   `$HOME=/homeless-shelter` is unwritable. The repo-root `AGENTS.md`
   does not exist and the lint skips it gracefully. The index covers
   Go source only.

The lint locates its scan roots via `runtime.Caller`, never via `$HOME`
or `os.Getwd()`, so nothing about the environment matters other than
"can I read the source files that were copied in".

## Adding a new token class

If a new identifier shape recurs in prose and the current classifier
skips it, extend `internal/doclint/classify.go` with a new
`tokenClass`, add the resolver in `internal/doclint/resolve.go`, and
add a unit test in `classify_test.go` / `scan_test.go`. Keep the
rule conservative — err on the side of skipping ambiguous tokens.

## Out of scope

- Semantic drift ("this function does X" when it now does Y) — needs
  human review, not a grep.
- Stale identifiers in Go comments — start with markdown docs; expand
  to comments only if signal justifies it.
- External references (URLs, third-party docs, stdlib package paths).
- Cross-repo identifier resolution (e.g. into the pi coding-agent
  package). Docs that describe such interfaces use
  `<!-- doclint-skip-file -->`.

## References

- [`internal/doclint/`](../internal/doclint/) — the lint implementation.
- Issue #2334 — codifies this lint; this doc is its operational spec.
- PR #2333 — the podman-proxy Step 8 closer whose three review cycles
  of stale-identifier findings motivated the lint.
