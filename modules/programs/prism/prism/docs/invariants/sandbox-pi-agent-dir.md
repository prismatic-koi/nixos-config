# Invariant: the pi agent dir in a sandbox

<!-- doclint-ignore: settings.json, bin/git, dist/utils/shell.js, dist/core/resolve-config-value.js, mcp.json, AGENTS.md, APPEND_SYSTEM.md, SYSTEM.md, models.json, trust.json, auth.json, crashes.json, notion/auth.ts, refreshIfNeeded, migrateLegacyStore -->
<!--
  These names are files in the pi agent dir ~/.pi/agent at run time, or a
  file in the external pi coding-agent package (dist/utils/shell.js). They
  do not exist in this source tree. notion/auth.ts, refreshIfNeeded, and
  migrateLegacyStore are a TypeScript file and functions in
  modules/programs/prism/pi/extensions/, outside the prism Go tree.
-->

Issues: #3284, #3289, #3290. Code: `internal/container/pi_agent_dir.go`,
`internal/container/pi_shared_state.go`, `appendPIBwrapMounts` in
`internal/container/pi_invocation.go`, sections 6a, 6c, and 21f of
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
| `sessions/<encoded-cwd>/` of the worktree | pi: the session files of the session. | Read-write dir bind. | Allowed. |

prism makes the sessions dir of the worktree and the two in-place files on
the host before the sandbox starts (`preparePIAgentDir`).

The shared OAuth refresh lock and the Notion store are not in the agent dir.
See "The shared state dirs".

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
  bind. The two shared state dirs are read-write dir binds at their host
  paths.
- sandbox-exec: `PI_CODING_AGENT_DIR` is the host `~/.pi/agent`. Section 6a
  allows a read of the whole dir and a write of the exact names in the write
  list. Section 6c allows a read and a write of the two shared state dirs.
  Section 21f denies a create and an unlink of `auth.json` and
  `atlassian-mcp-oauth.json`. Without it, a session can put a symlink at that
  path, and a host pi then writes its tokens to the target. Section 21g
  denies the same for the two shared dir nodes, and a mode change of them.

## The shared state dirs

Some state must be the same for every pi session on the host, of every
isolation mode. A bwrap bind cannot share one name in a dir and keep the
rest of the dir private. So this state is in two dedicated dirs, and not in
the agent dir:

| Dir | Content | Writer |
|---|---|---|
| `~/.local/state/prism/pi-locks/` | Lock dirs only: `anthropic-oauth-refresh.lock/`, with an `owner` file in it. | The anthropic-oauth extension (`refresh-lock.ts`). |
| `~/.local/state/prism/pi-notion/` | The Notion MCP store only: `notion-mcp-oauth.json`, `notion-mcp-client.json`, `notion-mcp-oauth.json.lock/`, and `.notion-mcp.tmp.*`. | The Notion extension (`notion/auth.ts`). |

No pi session loads configuration, code, or prompts from these dirs. pi does
not know them. The two extensions read only lock dirs and the JSON store in
them. The rule of the agent dir does not apply to them.

prism agent-run makes both dirs on the host with mode 0700 before a bwrap or
sandbox-exec session starts (`EnsurePISharedStateDirs`). bwrap binds each dir
read-write at its host path. A dir bind, not a file bind: the Notion
extension renames a temp file onto its store file, and a rename onto a bind
mount fails with EBUSY.

The paths come from `$HOME`, not from `$XDG_STATE_HOME`. All three isolators
run pi with `HOME` set to the host home dir. bwrap clears `XDG_STATE_HOME`,
and sandbox-exec sets it to `<home>/.local/state`, so a path from
`XDG_STATE_HOME` can differ between the isolators.

Every sandbox can write these dirs, and processes that are not sandboxed
use them too: a host-mode pi, a pi in a terminal, and prism agent-run. So
these processes create a lock `owner` file with `O_CREAT|O_EXCL|O_NOFOLLOW`,
and read an `owner` file or a store file with `O_NONBLOCK`, as a regular
file only. A sandbox can swap a dir with an `owner` symlink in after the
`mkdir` of a holder. The create then fails with EEXIST, and no host file
changes. A FIFO at a read name does not stop the reader. In bwrap each dir
is a mount point, and section 21g of sandbox-exec denies a replace of each
dir node, so a sandbox cannot put a symlink in place of a dir.

### The OAuth refresh lock (#3289)

pi takes `auth.json.lock` in the agent dir around its own refresh. In bwrap
that lock dir is private to the sandbox. So the anthropic-oauth extension
takes `anthropic-oauth-refresh.lock` in the shared lock dir around both of
its refresh paths: the `refreshToken` handler that pi calls, and
`refreshIfNeeded`. In the lock, it reads `auth.json` again. If another
session already wrote a valid token, it uses that token and sends no
request. Else it refreshes and writes the new token before it releases the
lock.

- Stale time: a lock dir that nobody touched for 30 s belongs to a dead
  holder. A waiter removes it and refreshes. A live holder touches its lock
  dir every 5 s while it waits on the network.
- Wait time: the async path waits at most 45 s, and the sync path at most
  20 s. Then it reads `auth.json` again and does not refresh. No session
  waits forever.
- If the extension cannot use the lock dir, it refreshes with no lock, as
  before #3289.

### The Notion store (#3290)

Notion rotates the refresh token on each refresh, and a replay of a rotated
token can make Notion revoke the whole grant. So every session must use the
same store. Host-mode, bwrap, and sandbox-exec sessions all read and write
`~/.local/state/prism/pi-notion/`, under its lock dir.

The store was in the agent dir before #3290. It moves once:

- prism moves it on the host before each bwrap or sandbox-exec session
  starts (`migratePINotionStore`).
- The extension moves it at `session_start` in a session in
  `NOTION_MCP_REPOS` that is not sandboxed: a host-mode session, or a pi that
  a person starts in a terminal (`migrateLegacyStore`).

Both moves hold the old lock dir, `notion-mcp-oauth.json.lock` in the agent
dir, and the new lock dir. A pi with the extension code from before #3290
takes the old lock dir around a refresh, so a move cannot copy a token that
such a pi is about to rotate. If the store dir already has a file, the move
removes the old file. If a lock is held, the move waits for the next session
start. Until the move is done, the extension reads the old path, and does
not refresh from it.

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

Issues and other docs name these limits by number, so a number does not
change when a limit goes away. Limits 1 and 3 are resolved: #3289 and
#3290.

- **Limit 2.** bwrap: a file bind keeps the inode that the file had when the
  sandbox started. `prism account use` replaces `auth.json` with a rename, so
  a running bwrap session does not see the new account until it restarts.
- **Limit 4.** Session files: the smallest unit that both isolators can grant
  is `sessions/<encoded-cwd>/` of the worktree. Review agents run in the
  worktree of their worker, so a worker and its review agents can write the
  session files of each other. Earlier sessions on the same worktree path
  are in the same dir. Follow-up: #3291.
- **Limit 5.** Both isolators: a session can make or keep a lock dir in the
  shared lock dir or in the Notion store dir, or put a file at a lock-dir
  name. This is a denial of service, not code execution. A lock dir that its
  holder does not keep fresh is stale after 30 s, and the next session
  removes it. A session that keeps a lock dir fresh stops the refresh of
  other sessions: after the wait time they do not refresh, and a token that
  expires is not replaced.

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
- Both isolators: a session can read and replace the Notion store files. It
  can put the tokens or the client registration of another workspace in
  them. Every pi session gets the store, not only the sessions in
  `NOTION_MCP_REPOS`.
