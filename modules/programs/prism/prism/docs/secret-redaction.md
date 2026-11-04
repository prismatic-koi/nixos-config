# Secret redaction in the capture path

<!-- doclint-ignore: prism.ts, makeFrameWriter, truncateString, redactToolResult -->
<!-- doclint-ignore: structuredContent, tool_execution_end, tool_execution_update -->
<!--
  The tokens above are cross-boundary references to the pi extension at
  modules/programs/prism/pi/extensions/prism.ts, and to the pi event fields
  and event names that it handles. The doclint source index
  covers TypeScript only when the repo root is available; inside the nix
  build sandbox (runChecks = true) only the prism subtree is copied in, so
  they do not resolve there. Same situation as the AGENTS.md annotation in
  doclint.md. They are load-bearing references, not drift: this doc
  specifies a control whose primary half lives in that file.
-->

Issue #2589.

Prism stores every frame an agent harness emits. A command an agent runs can
print a credential to stdout or stderr — `env`, `gh auth token`, a `curl -v`
that carries an Authorization header, a failing test that dumps an argv, a tool
that echoes its own configuration on error. Before this control, that value was
written verbatim into `prism.db` and kept until the prune job removed it.

The same output also goes to the model. Pi puts each tool result into the
model context, into the request to the model provider, and into the pi session
transcript. Issue #3420 added a control for that copy.

This document specifies the controls that remove the value.

## The three controls

Redaction runs at three points. The controls are independent on purpose: each
covers data that the others cannot see.

| Control | Where | Covers |
|---|---|---|
| 1 — pre-socket | `prism.ts`, in `makeFrameWriter` and `truncateString` | Every frame the pi extension emits. The secret never leaves the agent process. |
| 2 — pre-INSERT | `internal/db`, in `WriteEvent`, `WriteEventReturningRowID`, and `WriteHarnessFrame` | Every row written by any producer: a harness with no redactor of its own, a hook event, an audit row, a backfill. |
| 3 — tool result | `prism.ts`, in the `tool_result` handler, `redactToolResult` | The tool result that pi keeps: the model context, the request to the model provider, and the pi session transcript. See [Control 3](#control-3--the-tool-result-that-pi-keeps). |

## Redact one scalar at a time, never a serialised document

Every control redacts each string scalar and each object key ON ITS OWN. No
control runs a pattern over a serialised JSON document. This is a correctness
rule, not a style preference.

The `private-key-block` shape has a `[\s\S]*?` body. Run over a serialised
document, one match can start in one field and end in a later one, consuming
the JSON structure between them. Two failure modes follow, both silent:

```
{"a":"-----BEGIN A PRIVATE KEY-----","b":[1,"-----END A PRIVATE KEY-----"]}
  ->  {"a":"[redacted:private-key-block]"]}            invalid JSON

{"args":{"a":"-----BEGIN K PRIVATE KEY-----","z":"-----END K PRIVATE KEY-----"}}
  ->  {"args":{"a":"[redacted:private-key-block]"}}     field z silently gone
```

Redaction runs at INSERT, so the intact original is never stored; and
`prism scrub-secrets` rewrites historical rows in place. The corruption would
be permanent in both paths.

`RedactJSON` is the entry point on the Go side; `redactFrame` is its
equivalent in the extension. Neither field in either example above holds a
complete PEM block, so the correct answer for both is to leave the document
exactly as written — and that is what they do. A complete block inside one
field is still redacted.

Use `Redact` (the flat pass) only for text that is not a structured document:
a single decoded string value, or a payload that does not parse as JSON. There
are no delimiters to protect in either case.

Controls 1 and 3 are the primary ones. They run in the agent process, before
the secret leaves it. Control 1 protects the copy that goes to the prism
socket. Control 3 protects the copy that goes to the model provider and to the
transcript. Control 2 is defence in depth: it is the last line before the row
exists, and it is harness-agnostic.

All three controls apply the same rules. The Go rules live in
`internal/payload/redact.go`. The TypeScript rules live in `prism.ts`, and
controls 1 and 3 share them.
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
(`ForwardedCredentialEnvNames`, `GitHubTokenEnvName`, `GitLabTokenEnvName`,
`PrismGitHubTokenEnvPrefix`), so a credential added for injection is redacted
without a second edit.

Two of those names are not forwarded verbatim from the host environment:
prism resolves the GitHub and GitLab tokens host-side, from the sops file the
config names, and injects the VALUE (`ResolveGitHubToken`,
`ResolveGitLabToken`). They are in the registry for the same reason as the
rest: the capturing process holds the value, so the value layer can remove
it.

### Layer 2 — shape matching (secondary)

The shape layer covers the case where the secret is not in the environment of
the capturing process. It is defence in depth and it never replaces layer 1.

Each shape is anchored on a distinctive issuer prefix: `ghp_`, `github_pat_`,
`glpat-`, `sk-ant-`, `sk-or-v1-`, `sk-proj-`, `xoxb-`, `AKIA`, `AIza`,
`ATATT3`, and the PEM private-key block. `CredentialShapeNames` lists them in
match order.

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

The TypeScript side runs the equivalent two passes per scalar. Its
regular-expression engine backtracks, so the multi-line private-key shape is
not linear. A match attempt starts only at a literal
`-----BEGIN … PRIVATE KEY-----`. Input with many of these lines and no END
line costs quadratic time.

Controls 1 and 3 both run on the full output of a tool, before any truncation.
The built-in tools of pi cap that output at 50 KB. At that size, the worst case
measured 14 ms. A tool with no cap can return more: 1 MiB of these lines
measured 5.4 seconds. Ordinary output of 1 MiB measured 5 ms.

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

## Control 3 — the tool result that pi keeps

Controls 1 and 2 redact a copy of each tool result. Pi keeps the original. It
puts the original into the model context, into the request to the model
provider, and into the pi session transcript.

Pi fires the `tool_result` event after a tool runs and before pi builds the
tool result message. A handler of this event can return a replacement result.
Pi then uses the replacement for the model context, for the transcript, and
for the `tool_execution_end` event. The extension handles the event with
`redactToolResult`, which does these steps:

1. It redacts each text item of `content` on its own. It does not change an
   image item.
2. It redacts `structuredContent` and `details` with the walk of
   `redactFrame`: each string scalar and each object key on its own. It never
   serialises the value.
3. If no value changed, it returns no result. Pi then keeps the tool result
   unchanged, byte for byte.
4. If a value changed, it returns `content` and `structuredContent` together.
   Pi drops `structuredContent` when a handler replaces `content` alone. The
   built-in `bash` and `read` tools set `structuredContent`.
5. It returns `details` only when `details` changed. Otherwise pi keeps the
   original `details`.

The handler applies to the result of every tool: the built-in tools, the tools
of other extensions, and MCP tools. The extension registers the handler in each
prism session, also when `PRISM_HARNESS_PIPE` is not set. Outside prism, the
extension does nothing, so no redaction occurs.

An agent cannot read, from tool output, the value of a credential that the
value layer or the shape layer matches. It sees the marker, for example
`[redacted:GITHUB_TOKEN]`. A command that refers to the
variable continues to work, because the shell expands the variable. The value
does not go through the tool output.

### What control 3 covers

| Data | Covered | Where pi sends the data |
|---|---|---|
| Text items in `content` | Yes | The model context, the provider request, and the transcript. |
| String scalars and keys in `structuredContent` | Yes | Programmatic callers, for example codemode scripts. Pi does not send it to the model or write it to the transcript. |
| Image items in `content` | No. They pass through unchanged. | The model context and the transcript. |
| String scalars and keys in `details` | Yes | The transcript and the terminal. Pi does not send it to the model. When `bash`, `read`, `grep`, `find`, or `ls` truncates its output, it puts the shown text in `details.truncation.content`. The `edit` tool puts a diff of the file in `details`. |
| Text that the model writes: assistant messages and tool-call arguments | No | The model context and the transcript. |
| User messages, and messages that an extension adds | No | The model context and the transcript. |
| A credential that neither layer matches: an encoded value, for example the base64 Authorization header that `curl -v` prints for Basic auth, or a password that is not in the environment and has no known shape | No | Each place that the tool result goes to. |
| The output of a command that the user runs with `!` or `!!` | No | The transcript. Pi also sends the output of a `!` command to the model as a user message. It keeps the output of a `!!` command out of the model context. |
| The streaming output of a running tool (`tool_execution_update`) | No | The terminal only. |
| The file where `bash` saves its full output when it truncates the output | No | The disk. A `read` of that file is a tool result, so control 3 redacts what the model gets from it. |

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
| On-disk session archives | **No.** Control 3 covers part of an archive. See below. |

`prism cleanup` copies a session's harness transcript out of the pi sessions
root — `$PI_CODING_AGENT_DIR/sessions/`, or `~/.pi/agent/sessions/` when that
variable is unset — into the directory named by `sessions.archive_path`. Those
files sit outside the database and `prism scrub-secrets` does not touch them.

Pi's own `SessionManager` writes the transcript JSONL. `Archive` in
`internal/harness/pi/archive.go` is a byte-copy of the file pi wrote. Control 1
redacts a COPY of the tool result on its way to the prism socket. It does not
change the transcript. Control 3 replaces the tool result before pi writes it,
so the transcript holds the marker in place of the value.

An archive can still hold a credential in these places:

- A transcript that a session wrote before control 3 existed (issue #3420).
  Every part of it can hold a credential.
- A credential that neither layer matches, in any part of a transcript.
  Examples are an encoded value, and a password that is not in the environment.
- The parts of a transcript that control 3 does not cover: assistant
  messages, tool-call arguments, user messages, the output of a `!` or `!!`
  command, and messages that an extension adds. See
  [What control 3 covers](#what-control-3-covers).

Treat every archive, old or new, as able to hold a credential. Delete or rotate
it separately, and rotate any credential you believe reached it.

## Rules for changing this code

- Never log, echo, or write a credential value anywhere — including a test
  fixture. Every value in the tests is synthetic.
- To add a credential name, edit two places in the same change:
  1. `ForwardedCredentialEnvNames` in `internal/payload/redact.go` — or
     `otherCredentialEnvNames`, for a name prism does not forward but that can
     still be present in a host-mode agent's environment. A name prism
     resolves host-side and injects as a value gets its own exported
     constant instead (`GitHubTokenEnvName`, `GitLabTokenEnvName`), which
     `credentialEnvNameList` folds into the same union.
  2. `CREDENTIAL_ENV_NAMES` in the extension.

  `TestRedactorParityWithExtension_EnvNameRegistry` fails if you edit only one
  of the two. Do NOT try to add to `CredentialEnvNames` — it is a read-only
  accessor over the computed union of those lists, not a list itself. Same for
  `credentialForwardEnvKeys` in `internal/container/credentials.go`, which is a
  derived copy; turning it back into a literal breaks the derivation and a
  credential added there is injected into every sandbox but never redacted.
- Keep the shape patterns byte-identical between the two implementations. They
  are written to be valid and equivalent in both RE2 and the JavaScript
  dialect, which is what lets the parity test compare them directly.
- Do not add a shape with a generic body. Precision beats recall here: a
  control that mangles ordinary output gets switched off.
- Never apply a shape to a serialised JSON document. Use `RedactJSON` on the
  Go side and `redactFrame` in the extension. See the scalar-at-a-time section
  above for what goes wrong.
- Keep the `tool_result` handler on the same scalar walk. Do not serialise
  `structuredContent` or `details` to redact them. If the handler returns `content`, it must
  also return `structuredContent`. If it does not, pi drops `structuredContent`.
