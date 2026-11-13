# Secret redaction in the capture path

<!-- doclint-ignore: prism.ts, makeFrameWriter, truncateString, redactToolResult, CONTEXT_SHAPES -->
<!-- doclint-ignore: structuredContent, structuredContent.output, tool_execution_end, tool_execution_update -->
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

A `private-key-block` match runs from a BEGIN line to the next END line, with
any text between them. Run over a serialised document, one match can start in
one field and end in a later one, consuming the JSON structure between them.
Two failure modes follow, both silent:

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

#### Credential files

prism gives some credentials to a session as a file. The session gets the
path of the file in a variable, and no variable holds the value. Today the
only such credential is the UniFi password (issues #3472 and #3476):
`UNIFI_PASSWORD_FILE` holds the path, and the marker is
`[redacted:UNIFI_PASSWORD]`.

The value layer cannot learn such a value from the environment. So the
registry has a third part, `CredentialFileEnvNames`. It maps each path
variable to the marker name of the credential in the file. For each path
variable in the environment, the redactor reads the file and adds its
content to the value layer. The path variable itself is not a credential
name, so the output keeps the path.

The redactor reads a file with `ReadSecretFile`. It refuses a path that is
not a regular file, and a file larger than `MaxCredentialFileSize` (4096
bytes). It removes the surrounding whitespace. The content then goes
through the same over-redaction guards as a variable value. A file that
the redactor cannot read gives no value.

The three controls learn the password in these ways:

| Control | Process | How it learns the password |
|---|---|---|
| 1 and 3 | The pi extension in the session | The file that `UNIFI_PASSWORD_FILE` names. |
| 2 | The sidecar, and each other host process that writes `prism.db` | The path in `~/.config/prism/unifi.json` (`db.HostCredentialFiles`). The sidecar has no `UNIFI_PASSWORD_FILE`. |

Control 2 knows the password in every session, whatever its repo. Controls
1 and 3 know the password only in a session that gets
`UNIFI_PASSWORD_FILE`. This is a limit of host mode: a host-mode session of
another repo can read the sops path, and then only control 2 masks the
password.

prism does not give the password to a session when the password file holds
a value that the value layer cannot mask (`payload.UsableSecretValue`).

### Layer 2 — shape matching (secondary)

The shape layer covers the case where the secret is not in the environment of
the capturing process. It is defence in depth and it never replaces layer 1.

Each shape is anchored on a distinctive issuer prefix: `ghp_`, `github_pat_`,
`glpat-`, `sk-ant-`, `sk-or-v1-`, `sk-proj-`, `xoxb-`, `AKIA`, `AIza`,
`ATATT3`, and the BEGIN line of a PEM private-key block.

The shape layer has three parts, in this order:

1. The private-key pass replaces each PEM private-key block. A block starts
   at a line that matches `-----BEGIN [A-Z ]*PRIVATE KEY-----`. It stops at
   the first line after it that matches `-----END [A-Z ]*PRIVATE KEY-----`.
   Both implementations call the pass `redactPrivateKeyBlocks`.
2. The combined pass runs the issuer-prefix shapes as one regular
   expression. `CredentialShapeNames` lists them in match order.
3. The context shapes run one regular expression each. See
   [Context shapes](#context-shapes).

The private-key pass runs first because some token shapes accept `-`, for
example `glpat-[A-Za-z0-9_-]{20,255}`. In one combined expression, the
leftmost match wins. A token directly before a BEGIN line then consumes the
dashes of the line, and the key body goes out without redaction (issue #3437).
A token inside a block is part of the block, so the block marker replaces it.

A shape with a generic body — a bare base64 run, a JWT — is deliberately
absent. Its false-positive rate would corrupt more output than the rule
protects.

#### Context shapes

A context shape masks a value by the cookie name or the header name in front
of it (issue #3476). The value can have a generic body, because the name
anchors the match. The name stays in the output, and the marker replaces the
value only. A JWT or a UUID in any other context stays.

| Rule (marker name) | Context | Example output |
|---|---|---|
| `unifi-token-cookie` | The `TOKEN` cookie in a `Cookie:` or a `Set-Cookie:` header line. `TOKEN` can follow other cookies of the line. | `Set-Cookie: TOKEN=[redacted:unifi-token-cookie]; path=/` |
| `unifi-token-cookie-jar` | The `TOKEN` line of a curl cookie jar (Netscape format). | `...\tTRUE\t0\tTOKEN\t[redacted:unifi-token-cookie-jar]` |
| `csrf-token-header` | An `X-CSRF-Token:` or an `X-Updated-CSRF-Token:` header line. | `x-csrf-token: [redacted:csrf-token-header]` |

The `TOKEN` cookie holds the session token of UniFi OS, a JWT. The CSRF
header holds a UUID. A header name matches with no regard to ASCII case.
The patterns use `[Xx]` classes for this, so they need no regexp flag in
either dialect. The cookie name `TOKEN` is case-sensitive, as cookie names
are. Group 1 of each pattern is the context.

The value starts with a letter or a digit, and holds only the characters of
a token: letters, digits, and `._~+/=-`. Control 3 redacts the result of
the `read` tool too. So a shell expression or a string literal after the
name stays, for example `X-CSRF-Token: $(cat "$U/csrf")` in the `unifi`
skill. A marker stays too, so a second pass changes nothing.
`TestContextShapes_LeaveTheUniFiInstructionsUnchanged` runs the skill and
the post-deploy check of `docs/invariants/session-lifecycle.md` through the shape
layer.

The rules have these limits:

- A cookie with a `:` in its value, before `TOKEN` on the same line, stops
  the cookie rule. This limit keeps the pattern linear in V8: a scan from one
  header name stops at the colon of the next one.
- The cookie rule masks the first `TOKEN` of a line. A second `TOKEN` on the
  same line stays. A curl cookie jar holds one `TOKEN`, so curl sends one.
- A quoted value stays, for example `TOKEN="..."`. UniFi OS does not quote
  the value.
- A session value in another context is not masked. Examples are the
  verbose log of curl (`curl -v`) and a header that a script prints as JSON.
  The `unifi` repo skill tells the agent not to print headers.

`internal/payload/testdata/context-shape-cases.json` holds the output that
both implementations must give. The two regexp engines must agree on each
pattern, so the Go suite and `prism.test.ts` run each case.

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

`Redact` makes at most six linear passes over the input:

- The value layer is one `strings.Replacer`. It builds its trie once, at
  construction time, and walks the input once. Walk depth is bounded by the
  longest secret, which is a constant.
- The private-key pass searches for a BEGIN line, then for the first END line
  after it. The next search starts after that END line. If a BEGIN line has
  no END line after it, the pass stops. Each later BEGIN line ends after that
  line, so no END line can follow a later one either. Thus no part of the
  input is searched two times, and the pass is linear. The worst case is one
  scan for an END line, from the first BEGIN line to the end of the input.
- The combined pass is one RE2 regular expression. RE2 is linear in the input
  and does not backtrack.
- Each of the three context shapes is one RE2 regular expression.

Each pass of the shape layer runs behind a literal prefilter. Every shape
declares a set of trigger substrings — `ghp_`, `sk-ant-`, `-----BEGIN `, and
so on — of which at least one must be present for the pattern to match at
all. The trigger of `csrf-token-header` matches with no regard to ASCII
case, the same as its pattern. When none is present, which is the overwhelmingly common case, the shape
layer skips the pass and only the literal scans run. Measured on this repo, that takes
the throughput from about 8 MB/s to about 365 MB/s.

The prefilter is a cost optimisation, never a correctness one. It is sound
only while every trigger is a NECESSARY substring of its pattern.
`FuzzRedactShapePrefilter` pins that property: for any input, the prefiltered
shape layer and the unfiltered one must produce the same output. Add a shape
and you must add its triggers, on both sides.

No pass is quadratic in the size of a `tool_result` payload.
`TestRedact_LargePayloadCostIsNotQuadratic` and
`TestRedact_PrivateKeyPassCostIsNotQuadratic` bound the wall-clock cost of one
call on 8 MiB. `TestRedact_ContextShapeCostIsNotQuadratic` bounds it on
1 MiB, because a context shape has no literal prefix and RE2 runs at about
12 MB/s on it. Each bound separates a linear implementation from a
quadratic one without depending on the speed of the machine.

The private-key pass gives the same output as the lazy expression
`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`.
`FuzzPrivateKeyPassMatchesLazyRegexp` pins this on the Go side. The extension
does not use the lazy expression, because V8 backtracks. In V8, each BEGIN line
with no END line after it starts a scan to the end of the input, so the cost of
the lazy expression is quadratic.

The TypeScript side runs the same passes per scalar, and its private-key
pass stops at the same point. V8 backtracks, so the pattern of each context
shape is linear in V8 also. The extension tests assert a bound of
100 ms for 1 MiB of each adversarial cookie and CSRF input. Thus its worst case is also one scan for an END
line, from the first BEGIN line to the end of the scalar.

Control 1 runs on the full `content` of a tool result, before its 8 KiB byte
cut. The built-in tools of pi cap `content` at 50 KB. Control 3 also walks
`structuredContent` and `details`. The `bash` tool puts up to 1 MiB of output
in `structuredContent.output`. A tool with no cap can return more, and the cost
increases linearly with the size.

Measured on an Apple M4 host, with the best of three runs:

| Input | Time |
|---|---|
| 1 MiB with 1000 BEGIN lines and no END line, in the RSA or the one-letter form, with lines of dashes between them | 1.1 ms |
| 1 MiB with 500 BEGIN lines, one END line, and 500 BEGIN lines, with lines of dashes between them | 1.0 ms |
| 1 MiB of BEGIN lines with no END line | 1.6 ms |
| 8 MiB of BEGIN lines with no END line | 11 ms, 7.3 times the time for 1 MiB |
| A `bash` result of BEGIN lines with no END line: 50 KiB `content` and `details`, 1 MiB `structuredContent.output` | 2.3 ms |

The extension tests assert a bound of 100 ms for each 1 MiB case and for the
`bash` result. They also assert that 8 MiB takes less than 16 times as long as
1 MiB. A quadratic pass takes about 64 times as long.

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
from a shell that carries the same credentials the agents had. It also reads
the credential files of the host config (`db.HostCredentialFiles`), so it
knows the UniFi password with no variable set. Values the
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
  of the two. To add a credential that a session gets as a file, edit
  `credentialFileEnvNames` in `internal/payload/redact.go` and
  `CREDENTIAL_FILE_ENV_NAMES` in the extension instead.
  `TestRedactorParityWithExtension_CredentialFileRegistry` fails if you edit
  only one of the two. A host process knows the value only when
  `db.HostCredentialFiles` names the file. Do NOT try to add to `CredentialEnvNames` — it is a read-only
  accessor over the computed union of those lists, not a list itself. Same for
  `credentialForwardEnvKeys` in `internal/container/credentials.go`, which is a
  derived copy; turning it back into a literal breaks the derivation and a
  credential added there is injected into every sandbox but never redacted.
- Keep the shape patterns byte-identical between the two implementations. They
  are written to be valid and equivalent in both RE2 and the JavaScript
  dialect, which is what lets the parity test compare them directly. This
  includes the BEGIN and END patterns of the private-key shape
  (`PRIVATE_KEY_SHAPE` in the extension).
- The private-key pass is code, so equal patterns do not prove equal output.
  `internal/payload/testdata/private-key-cases.json` holds the output that
  both implementations must give. The Go suite and `prism.test.ts` run each
  case. If you change the private-key pass, add a case to that file.
- Do not put the private-key shape back into the combined pass. A token shape
  that accepts `-` then consumes the dashes of a BEGIN line again.
- Do not add a shape with a generic body. Precision beats recall here: a
  control that mangles ordinary output gets switched off. A value with a
  generic body is a context shape: anchor the rule on the cookie name or the
  header name, and keep the name in group 1.
- If you change a context shape, change `contextShapes` in
  `internal/payload/redact.go` and `CONTEXT_SHAPES` in the extension, and add
  a case to `internal/payload/testdata/context-shape-cases.json`.
  `TestRedactorParityWithExtension_ContextShapes` compares the two rule
  lists. Each pattern must be linear in V8: add an adversarial input for the
  rule to the cost tests of both suites.
- Never apply a shape to a serialised JSON document. Use `RedactJSON` on the
  Go side and `redactFrame` in the extension. See the scalar-at-a-time section
  above for what goes wrong.
- Keep the `tool_result` handler on the same scalar walk. Do not serialise
  `structuredContent` or `details` to redact them. If the handler returns `content`, it must
  also return `structuredContent`. If it does not, pi drops `structuredContent`.
