# Secret redaction in the capture path

<!-- doclint-ignore: prism.ts, makeFrameWriter, truncateString -->
<!--
  The three tokens above are cross-boundary references to the pi extension
  at modules/programs/prism/pi/extensions/prism.ts. The doclint source index
  covers TypeScript only when the repo root is available; inside the nix
  build sandbox (runChecks = true) only the prism subtree is copied in, so
  they do not resolve there. Same situation as the AGENTS.md annotation in
  podman-proxy.md. They are load-bearing references, not drift: this doc
  specifies a control whose primary half lives in that file.
-->

Issue #2589.

Prism stores every frame an agent harness emits. A command an agent runs can
print a credential to stdout or stderr — `env`, `gh auth token`, a `curl -v`
that carries an Authorization header, a failing test that dumps an argv, a tool
that echoes its own configuration on error. Before this control, that value was
written verbatim into `prism.db` and kept until the prune job removed it.

This document specifies the control that removes it.

## The two controls

Redaction runs twice. The two controls are independent on purpose: each covers
a producer the other cannot see.

| Control | Where | Covers |
|---|---|---|
| 1 — pre-socket | `prism.ts`, in `makeFrameWriter` and `truncateString` | Every frame the pi extension emits. The secret never leaves the agent process. |
| 2 — pre-INSERT | `internal/db`, in `WriteEvent`, `WriteEventReturningRowID`, and `WriteHarnessFrame` | Every row written by any producer: a harness with no redactor of its own, a hook event, an audit row, a backfill. |

Control 1 is the primary one. It is the only one that stops the secret from
crossing a process boundary. Control 2 is defence in depth: it is the last line
before the row exists, and it is harness-agnostic.

Both controls apply the same rules. The Go rules live in
`internal/payload/redact.go`; the TypeScript rules live in `prism.ts`.
`TestRedactorParityWithExtension_EnvNameRegistry` and its sibling tests read
the extension source and fail when the two registries drift.

## The two layers

### Layer 1 — value matching (primary)

The process that captures a frame already holds the literal value of every
credential environment variable. An exact match on that value has near-zero
false positives. A regular expression for `ghp_` guesses; an exact value match
knows.

The value layer reads a variable when `IsCredentialEnvName` accepts its name.
A name is accepted when it is:

- in the exact list — see `CredentialEnvNames`; or
- prefixed by an entry of `CredentialEnvPrefixes` — today only
  `PRISM_GITHUB_TOKEN_`; or
- suffixed by an entry of `CredentialEnvNameSuffixes` — `_TOKEN`, `_API_KEY`,
  `_PASSWORD`, and the rest.

The suffix heuristic exists so a credential nobody listed is still caught.
Every suffix ends the name, so `GITHUB_TOKEN_PATH` and `SOPS_AGE_KEY_FILE` do
not match: they name a file, not a secret, and redacting a path corrupts
diagnosable output for no gain.

`internal/container` derives its injection list from the same registry
(`ForwardedCredentialEnvNames`, `GitHubTokenEnvName`,
`PrismGitHubTokenEnvPrefix`), so a credential added for injection is redacted
without a second edit.

### Layer 2 — shape matching (secondary)

The shape layer covers the case where the secret is not in the environment of
the capturing process. It is defence in depth and it never replaces layer 1.

Each shape is anchored on a distinctive issuer prefix: `ghp_`, `github_pat_`,
`sk-ant-`, `sk-or-v1-`, `sk-proj-`, `xoxb-`, `AKIA`, `AIza`, `ATATT3`, and the
PEM private-key block. `CredentialShapeNames` lists them in match order.

A shape with a generic body — a bare base64 run, a JWT — is deliberately
absent. Its false-positive rate would corrupt more output than the rule
protects.

## The marker

A redacted match is replaced by a marker that names what was removed, so the
surrounding output stays diagnosable:

```
GITHUB_TOKEN=[redacted:GITHUB_TOKEN]
```

The value layer names the environment variable. The shape layer names the
shape, for example `[redacted:github-token]`. `RedactionMarker` builds the
marker; `RedactionMarkerPrefix` and `RedactionMarkerSuffix` are the brackets.

Redaction is idempotent: a marker matches no rule, so a second pass is a no-op.

## Over-redaction guards

An empty, whitespace-only, or one-character value matches at almost every
position of ordinary output. A redactor that used one would shred the output it
is supposed to protect. The value layer therefore skips a value when:

- it is shorter than `MinCredentialValueLen` (8 characters); or
- it is empty or whitespace-only, or its trimmed length is below that bound; or
- it contains `$(` — an unexpanded shell literal is a propagation bug, not a
  secret (issue #2348). Redacting it would hide the bug.

A value below the bound is left to the shape layer. Every real credential is
far longer: the shortest token prism forwards is a GitHub PAT at 40 characters.

Output that carries no credential value is written unchanged, byte for byte.

## Cost

`Redact` makes at most two linear passes over the input:

- the value layer is one `strings.Replacer`. It builds its trie once, at
  construction time, and walks the input once. Walk depth is bounded by the
  longest secret, which is a constant.
- the shape layer is one combined RE2 regular expression. RE2 is linear in the
  input and does not backtrack.

The shape layer runs behind a literal prefilter. Every shape declares a set of
trigger substrings — `ghp_`, `sk-ant-`, `-----BEGIN `, and so on — of which at
least one must be present for the pattern to match at all. When none is
present, which is the overwhelmingly common case, the regular expression is
skipped and only the literal scans run. Measured on this repo, that takes the
throughput from about 8 MB/s to about 365 MB/s.

The prefilter is a cost optimisation, never a correctness one. It is sound
only while every trigger is a NECESSARY substring of its pattern.
`FuzzRedactShapePrefilter` pins that property: for any input, the prefiltered
shape layer and the unfiltered one must produce the same output. Add a shape
and you must add its triggers, on both sides.

Neither pass is quadratic in the size of a `tool_result` payload.
`TestRedact_LargePayloadCostIsNotQuadratic` bounds the wall-clock cost of one
call on 8 MiB, which separates a linear implementation from a quadratic one
without depending on the speed of the machine.

The TypeScript side runs the equivalent two passes. Its regular-expression
engine backtracks, so the multi-line private-key shape is not linear in the
strict sense; it needs a literal `-----BEGIN … PRIVATE KEY-----` to start
scanning, and the wire budget caps a frame at 8 KiB, so the practical bound
holds.

## Frame boundaries and truncation

The pi extension truncates tool arguments, tool output, and assistant deltas to
8 KiB before it builds a frame. Redaction that ran only in the frame writer
would therefore see output that the truncator had already cut, and a secret
straddling the boundary would survive as a partial value that no later literal
match can find.

`truncateString` redacts BEFORE the byte cut, which closes that hole for all
three fields. The marker itself can still be cut by the truncation; that is
harmless, because the marker carries no secret.

Known limitation: a secret split across two SEPARATE frames — for example, a
streaming assistant response whose deltas break mid-token — is not matched by
the value layer, because neither half is the whole value. Nothing in the
current capture path reassembles deltas before storage, so this is documented
rather than fixed.

## Remediation for rows already written

The write-time control protects new rows only. Rows written before it existed
hold whatever the agents printed, and the 90-day prune is far too slow a remedy
for a live credential.

```bash
prism scrub-secrets --dry-run   # report how many rows would change
prism scrub-secrets             # rewrite them
prism scrub-secrets --json      # machine-readable report
```

The command reads the credential values out of its own environment, so run it
from a shell that carries the same credentials the agents had. Values the
environment does not hold are still covered by the shape layer; the command
prints a warning on stderr when the value layer is empty.

`ScrubSecrets` is the library entry point. It pages by rowid, one transaction
per page, so a large database does not have to fit in memory. Re-running is
safe: redaction is idempotent, so a second pass reports zero rewrites.

### What the scrub covers

| Target | Covered |
|---|---|
| `agent_events` payload column | Yes — every event, every type. |
| `harness_frames` payload column | Yes — the raw wire archive. |
| On-disk session archives | **No.** |

`prism cleanup` copies a session's harness transcript out of the worktree and
into the directory named by `sessions.archive_path`. Those files sit outside
the database and this command does not touch them. Treat an archive taken
before this control shipped as carrying whatever the session printed: delete
or rotate it separately, and rotate any credential you believe reached it.

Archives taken after this control shipped inherit the protection indirectly for
the pi harness, because the extension redacts before the value reaches the
transcript writer. That is a consequence of control 1, not a guarantee this
command provides.

## Rules for changing this code

- Never log, echo, or write a credential value anywhere — including a test
  fixture. Every value in the tests is synthetic.
- Add a new credential name to `payload.CredentialEnvNames` and to
  `CREDENTIAL_ENV_NAMES` in the extension, in the same change. The parity test
  fails if you add only one.
- Keep the shape patterns byte-identical between the two implementations. They
  are written to be valid and equivalent in both RE2 and the JavaScript
  dialect, which is what lets the parity test compare them directly.
- Do not add a shape with a generic body. Precision beats recall here: a
  control that mangles ordinary output gets switched off.
