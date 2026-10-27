# Invariant: the pi agent dir in a sandbox

<!-- doclint-ignore: settings.json, bin/git, dist/utils/shell.js, dist/core/resolve-config-value.js, mcp.json, AGENTS.md, APPEND_SYSTEM.md, SYSTEM.md, models.json, trust.json, auth.json, crashes.json -->
<!--
  These names are files in the pi agent dir ~/.pi/agent at run time, or a
  file in the external pi coding-agent package (dist/utils/shell.js). They
  do not exist in this source tree.
-->

Issue: #3284. Code: `internal/container/pi_agent_dir.go`, `appendPIBwrapMounts`
in `internal/container/pi_invocation.go`, sections 6a and 21f of
`generateProfile` in `internal/container/sandbox_exec.go`, and
`internal/container/pi_resume_cwd.go`.

## The rule

A bwrap or sandbox-exec session cannot create, change, or replace a path in
the host pi agent dir, `~/.pi/agent`. The only exceptions are the paths in
the write list below.

The rule does not fully stop a worker from running code in a later session:
a sandbox can still write `auth.json`, and pi runs some values in it as a
shell command. See "Open limit: a command in auth.json".

Host-mode sessions are not sandboxed. They read and write the host dir as
before.

## Why the agent dir is read-only

A worker is not trusted. Every later session reads the agent dir: host-mode
sessions, coordinators, review agents, and a pi that the user starts in a
terminal. pi loads these entries with no project-trust check:

| Entry | Effect of a planted value |
|---|---|
| `settings.json` | Its `extensions` list names code that pi runs in each session. |
| `extensions/` | pi loads each extension in it. |
| `bin/` | pi puts it first on the `PATH` of each bash tool call (pi 1.4 `dist/utils/shell.js`). A planted `bin/git` runs in each later session. |
| `mcp.json` | The built-in MCP extension starts each stdio server command in it. |
| `SYSTEM.md`, `APPEND_SYSTEM.md`, `AGENTS.md` | Text in the system prompt. |
| `skills/`, `prompts/` | Instructions that the model loads. |
| `models.json` | Provider base URLs and keys. |
| `trust.json` | Saved project-trust decisions. |
| `auth.json` | pi runs the key of an `api_key` entry that starts with `!` as a shell command. This file stays writable: see "Open limit: a command in auth.json". |

Most of these entries do not exist on a normal host. If they do not exist,
pi does not read them. So a sandbox must not create a new name in the dir,
and must not change an existing one.

## The write list

pi 1.4 and the extensions write these paths during a normal session. The
list comes from the pi source in the nix store and from a `strace` of one
pi turn.

| Path | Writer and reason | bwrap | sandbox-exec |
|---|---|---|---|
| `auth.json` | pi and the anthropic-oauth extension: OAuth token refresh, a write in place. | Read-write file bind. | Write allowed. Create and unlink denied (section 21f). |
| `atlassian-mcp-oauth.json` | Atlassian extension: OAuth refresh, a write in place. | Read-write file bind. | Write allowed. Create and unlink denied (section 21f). |
| `auth.json.lock`, `settings.json.lock`, `trust.json.lock`, `models-store.json.lock` | pi: proper-lockfile lock dirs. pi takes the lock on each read of the file, also at start. If pi cannot make them, it exits at start. | Private to the sandbox. | Allowed on the lock-dir node only. |
| `notion-mcp-oauth.json`, `notion-mcp-client.json`, `.notion-mcp.tmp.*`, `notion-mcp-oauth.json.lock/` | Notion extension: OAuth refresh, a temp file renamed onto the store file, under a lock dir. | The token file is not in the sandbox. The client file is read-only. No refresh (limit 3). | Allowed. |
| `sessions/<encoded-cwd>/` of the worktree | pi: the session files of the session. | Read-write dir bind. | Allowed. |

prism makes the sessions dir of the worktree and the two in-place files on
the host before the sandbox starts (`preparePIAgentDir`).

All other entries are read-only in the sandbox. pi tries these writes, which
fail in a sandbox with no effect on a normal session:

- `models-store.json`: a provider catalog refresh. prism sets `PI_OFFLINE`,
  so pi does not refresh catalogs.
- `trust.json`: a saved trust decision, when the user answers a trust
  prompt. A prism session does not show the prompt.
- `crashes.json`: the crash log, when pi crashes.

## The layout of each isolator

- bwrap: `PI_CODING_AGENT_DIR` is `/run/prism/pi-agent`, a private dir in the
  tmpfs root of bwrap. Each host entry is a read-only bind in it, except
  `sessions/`, the credential files, and `*.lock`. A create in the private
  dir works, but it stays in the sandbox and goes away with it. A host lock
  dir is not bound, because pi cannot remove a stale lock from a read-only
  bind.
- sandbox-exec: `PI_CODING_AGENT_DIR` is the host `~/.pi/agent`. Section 6a
  allows a read of the whole dir and a write of the exact names in the write
  list. Section 21f denies a create and an unlink of `auth.json` and
  `atlassian-mcp-oauth.json`. Without it, a session can put a symlink at that
  path, and a host pi then writes its tokens to the target.

## The cwd check on resume

A session can write the session files in the sessions dir of its own
worktree. pi runs a resumed session in the `cwd` from the header of the
session file, not in the cwd that prism gives it. So before prism passes
`--session <id>` to pi, `CheckPIResumeCwd` reads every `*.jsonl` entry in
that dir.

pi has more than one header parser. `findById` and `open` scan up to 1 MiB
and skip blank lines, lines that are not JSON, and JSON null. The `list`
path, which pi uses for a prefix match, reads the whole file and also splits
lines on a carriage return. pi selects by the id in the header, not by the
file name. So the check fails closed. It stops the resume unless each entry
meets all of these conditions:

- It is a regular file.
- It is empty, or its first line is one JSON object that Go decodes with no
  error, with no carriage return, and of 64 KiB or less.

pi writes every session file this way, and for such a file each pi parser
takes the first line as the header. Then:

- A header with type `session` must have a string id.
- Each header whose id starts with the id must have the worktree as its
  `cwd`.
- One header must have the exact id, so that pi does not search the
  sessions dirs of other projects.
- The id must not be a value that pi reads as a file path.

When the check fails:

- `prism restore` prints `restore "<session>": not resuming pi session <id>:
  <reason>` and starts a fresh conversation in the worktree.
- The launchers (`ResolvePIResumeSession`, which bwrap, sandbox-exec, and
  host mode call) do the same check and write the reason to the agent-run
  log.

The check runs just before pi starts. A session in the same worktree that
runs at that time can still change a file after the check (limit 4).

## Open limit: a command in auth.json

pi 1.4 sends the key of each `type: "api_key"` entry in `auth.json` through
`resolveConfigValue` (`dist/core/resolve-config-value.js`). A value that
starts with `!` runs as a shell command. pi has no setting that turns this
off. A sandbox must write `auth.json` for the OAuth refresh, so it can plant
such a value. The next pi that reads the file runs the command.

Before each pi start that prism makes, prism reads the host `auth.json` and
refuses the start if a value in it starts with `!` (`CheckPIAuthFile`). The
error names the file and the provider, and holds no value from the file. A
file that Go cannot decode as one JSON object is also refused. The check
runs here:

- host mode: `session.BuildAgentCmd`, before the pane command;
- bwrap and sandbox-exec: `prism agent-run` (`populatePIConfig`), before pi
  starts in the sandbox;
- `prism quick pr`, before its pi call.

The check covers only the pi starts that prism makes. It does not cover a pi
that a person starts in a terminal, or a pi that already runs and reads
`auth.json` again. Follow-up: #3295 (high priority), a per-session
`auth.json` for each sandbox and a host-side sync of oauth entries only.

## Known limits

1. bwrap: the four pi lock dirs are private to the sandbox. The
   `auth.json` refresh lock no longer serialises a bwrap session with other
   sessions. Two sessions can refresh the Anthropic token at the same time
   with the same refresh token. Follow-up: #3289 (high priority).
2. bwrap: a file bind keeps the inode that the file had when the sandbox
   started. `prism account use` replaces `auth.json` with a rename, so a
   running bwrap session does not see the new account until it restarts.
3. bwrap: Notion is not available. Notion rotates the refresh token on each
   refresh, and the Notion extension refreshes at session start. In bwrap,
   the extension cannot save the new token, because a rename onto a bind
   mount fails with EBUSY. The host store then keeps a token that Notion
   rotated away, and the next refresh with it can make Notion revoke the
   whole grant. So `notion-mcp-oauth.json` is not in a bwrap sandbox
   (`piAgentBwrapHiddenFiles`): the extension finds no tokens and sends no
   refresh.
   - At each start of a bwrap session in the vault, the extension shows
     "Notion MCP: no auth tokens. Use /login-notion to authenticate."
     Ignore this message in a bwrap session. Do not run `/login-notion`
     there: the tokens stay in the private dir and go away with the
     sandbox.
   - On navi and tui, the `obsidian` session runs in bwrap. The Notion grant
     lapses 30 days after this change merges, unless a host-mode session in
     the vault refreshes it.
   - Follow-up: #3290 (high priority).
4. Session files: the smallest unit that both isolators can grant is
   `sessions/<encoded-cwd>/` of the worktree. Review agents run in the
   worktree of their worker, so a worker and its review agents can write the
   session files of each other. Earlier sessions on the same worktree path
   are in the same dir. Follow-up: #3291.

Other limits:

- bwrap: a read-only bind keeps the file that a host entry named when the
  sandbox started. A change from `nh switch` is visible after a respawn. A
  host entry that appears later is not visible in a running sandbox.
- Both isolators: a session can write the credential files. It can put the
  tokens of another account in them, and later sessions then use that
  account. A session can already read these tokens. For a command value in
  `auth.json`, see "Open limit: a command in auth.json".
- sandbox-exec: a session can make a lock-dir name, or keep a lock dir, so
  that the lock of a later session fails. This is a denial of service, not
  code execution.
- sandbox-exec: a session can replace the Notion store files, because the
  extension writes them with a rename.
