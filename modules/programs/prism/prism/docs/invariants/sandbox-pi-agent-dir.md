# Invariant: the pi agent dir in a sandbox

<!-- doclint-ignore: settings.json, bin/git, dist/utils/shell.js, dist/core/resolve-config-value.js, mcp.json, AGENTS.md, APPEND_SYSTEM.md, SYSTEM.md, models.json, trust.json, auth.json, crashes.json, notion/auth.ts, refreshIfNeeded, migrateLegacyStore, sync.lock, sync-state.json, agent/auth.json, session-sync.ts, refreshAnthropicToken, syncSessionAuth, syncSessionAuthSync, config.json, profiles.json -->
<!--
  These names are files in the pi agent dir ~/.pi/agent or in the
  per-session auth dir at run time, or a file in the external pi
  coding-agent package (dist/utils/shell.js). They do not exist in this
  source tree. notion/auth.ts, session-sync.ts, refreshIfNeeded,
  refreshAnthropicToken, syncSessionAuth, syncSessionAuthSync, and
  migrateLegacyStore are TypeScript files and functions in
  modules/programs/prism/pi/extensions/, outside the prism Go tree.
  config.json and profiles.json are files in ~/.config/prism that Nix
  renders at run time.
-->

Issues: #3284, #3289, #3290, #3295, #3314, #3319. Code:
`internal/container/pi_agent_dir.go`,
`internal/container/pi_shared_state.go`,
`internal/container/pi_auth_session.go`, `appendPIBwrapMounts` in
`internal/container/pi_invocation.go`, sections 6a, 6c, 6d, 21f, 21g, 21h,
and 21l of `generateProfile` in `internal/container/sandbox_exec.go`,
`piNotionStore` in `cmd/agent_run.go`, `SessionProject` in
`internal/projecttrust/projecttrust.go`,
`internal/sidecar/pi_auth_sync.go`, and
`internal/container/pi_resume_cwd.go`.

## The rule

A bwrap or sandbox-exec session cannot create, change, or replace a path in
the host pi agent dir, `~/.pi/agent`. The only exceptions are the paths in
the write list below. The host `auth.json` is not in the list: each
sandboxed session has its own `auth.json`, and the sidecar copies only the
OAuth entry of the `anthropic` provider from it to the host file. See "The
per-session auth.json".

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
| `auth.json` | pi runs the key of an `api_key` entry that starts with `!` as a shell command. A sandbox writes its own `auth.json`: see "The per-session auth.json". |

Most of these entries do not exist on a normal host. If they do not exist,
pi does not read them. So a sandbox must not create a new name in the dir,
and must not change an existing one.

## The write list

pi 1.4 and the extensions write these paths during a normal session. The
list comes from the pi source in the nix store and from a `strace` of one
pi turn.

| Path | Writer and reason | bwrap | sandbox-exec |
|---|---|---|---|
| `auth.json` of the session | pi and the anthropic-oauth extension: OAuth token refresh, a write in place. | A symlink to the per-session file, in a read-write dir bind. | In the per-session agent dir (section 6d). |
| `atlassian-mcp-oauth.json` | Atlassian extension: OAuth refresh, a write in place. | Read-write file bind. | Write allowed. Create and unlink denied (section 21f). |
| `auth.json.lock`, `settings.json.lock`, `trust.json.lock`, `models-store.json.lock` | pi: proper-lockfile lock dirs. pi takes the lock on each read of the file, also at start. If pi cannot make them, it exits at start. | Private to the sandbox. | In the per-session agent dir. |
| `sessions/<encoded-cwd>/` of the worktree | pi: the session files of the session. | Read-write dir bind. | Allowed. |

prism makes the sessions dir of the worktree and the Atlassian file on the
host before the sandbox starts (`preparePIAgentDir`).

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
  `sessions/`, `auth.json`, the Atlassian file, and `*.lock`. A create in the
  private dir works, but it stays in the sandbox and goes away with it. A
  host lock dir is not bound, because pi cannot remove a stale lock from a
  read-only bind. `auth.json` is a symlink to `/run/prism/pi-auth/auth.json`,
  and `/run/prism/pi-auth` is a read-write bind of the per-session agent dir.
  The host `auth.json` is not in the sandbox. The shared lock dir is a
  read-write dir bind at its host path. The Notion store dir is a
  read-write dir bind at its host path only in a session in
  `NOTION_MCP_REPOS`. In another session, the old Notion store files in
  `~/.pi/agent` are not bound either. See "Which sessions get the Notion
  store".
- sandbox-exec: `PI_CODING_AGENT_DIR` is the per-session agent dir. It holds
  the `auth.json` of the session, the lock dirs of pi, and a symlink to each
  other entry of `~/.pi/agent` (`LinkPIAgentDirEntries`). Section 6d allows a
  read and a write of that dir. Section 6a allows a read of `~/.pi/agent` and
  a write of the Atlassian file and of the sessions dir of the worktree, so
  a write through a symlink to another host entry fails. Section 6c allows
  a read and a write of the shared lock dir, and of the Notion store dir
  only in a session in `NOTION_MCP_REPOS`. In another session, section 21l
  denies a read of the old Notion store files in `~/.pi/agent`. Section 21f
  denies a create and an unlink of `auth.json` and of the Atlassian file in
  `~/.pi/agent`. Without it, a session can put a symlink at that path, and a
  host pi then writes its tokens to the target. Section 21g denies the same
  for the two shared dir nodes, and a mode change of them. Section 21h
  denies the same for the per-session agent dir node.

## The per-session auth.json

pi 1.4 runs the key of an `api_key` entry in `auth.json` that starts with
`!` as a shell command (`resolveConfigValue`,
`dist/core/resolve-config-value.js`). pi has no setting that turns this off.
Every later pi reads the host `auth.json`: host-mode sessions,
coordinators, and a pi that a person starts in a terminal. So a sandbox
must not write that file (#3295).

Each bwrap and sandbox-exec session has its own dir on the host:

| Path | Content | The sandbox can write it |
|---|---|---|
| `<state>/prism/pi-auth/<instance id>/` | The root, mode 0700. | No. |
| `sync.lock` | The flock of the sync and of the prepare. | No. |
| `sync-state.json` | For each provider, the SHA-256 of the OAuth values that the sync saw. No token. | No. |
| `agent/` | The per-session agent dir, mode 0700. | Yes. |
| `agent/auth.json` | The `auth.json` of the session. | Yes. |

`<state>` is `$XDG_STATE_HOME`, else `~/.local/state`. prism agent-run makes
the dir again from nothing before pi starts (`PreparePIAuthSession`). It
writes `agent/auth.json` from the host file and leaves out each entry that
holds a string that starts with `!`, at any depth. A session with no
instance ID gets no per-session dir, and no `auth.json` from the host.
`prism cleanup` removes the dir (`removeSessionInstanceDirs`).

The sidecar of the session copies changes between the two files
(`SyncPIAuthSession`):

- From the session file to the host file: only the entry of the
  `anthropic` provider, and only as a `type: "oauth"` entry with a
  non-empty string `access`, a non-empty string `refresh`, and a finite
  number `expires`. A string that starts with `!` is refused. Only these
  four fields are copied. Other fields of the host entry stay. The value
  must be new: no earlier sync of the session wrote it or read it. pi writes
  back an old snapshot of the file after a refresh, and a value from that
  snapshot must not undo a newer host value. If the host entry changed too,
  the host value wins. A session value never replaces a host entry that is
  not an OAuth entry.
- The entry of each other provider is refused, and the sidecar logs the
  reason once for each value (#3314). For the `github-copilot` provider,
  the OAuth `access` value also sets the API host: pi 1.4 sends each Copilot
  request to the `proxy-ep=` host in the token. A copy of such a value lets
  a session send the conversations of a host pi to a host of its choice. So
  a `/login` in a sandbox for a provider other than `anthropic` does not
  reach the host file. Log in on the host for those providers.
- Limits on the host file: a token of more than 16 KiB is refused. A
  session adds a new provider to the host file at most two times in its
  life. Only `anthropic` can be new, so the count applies when a session
  logs in again after a logout on the host. The sync state keeps the count
  across the starts of the session. No write makes the host file larger
  than 256 KiB. So a session cannot make the host file so large that
  `CheckPIAuthFile` refuses every pi start, or that the sync refuses the
  file of each other session.
- Limits on the sync state: the session names the providers in its file,
  so the state records at most 64 providers that the host file does not
  name, and only names of the provider form. A state file that prism cannot
  read counts as an allowance that is used up. So a session cannot reset
  its count of new providers.
- From the host file to the session file: the whole host file, less each
  entry with a `!` value.

The sync treats the session file as hostile. It opens the file with
`O_NOFOLLOW` and `O_NONBLOCK`. It reads only a regular file of at most
1 MiB that holds one JSON object. Else it changes nothing on the host and
in the session file, and the sidecar logs the reason. It writes each file
with a temp file and a rename, so it never follows a symlink at the path.
Before a write, it checks that the file did not change since its read.

The sync takes the proper-lockfile lock `auth.json.lock` of the host file
when the lock is free, and does not wait for it. A host-mode pi takes that
lock first and then waits for the shared refresh lock, around its own
refresh. The sync holds the refresh lock, or runs for a caller that holds
it. A wait for the host lock then stops both, and the new token of the
sandbox does not reach the host file before the host pi refreshes with the
same refresh token. When the host lock is held, the sync reads the host file
with no lock. A file of zero bytes is then a write of pi in progress, so the
sync changes nothing and tries again later. The write is a rename after the
check that the file did not change.

The sync runs at two times:

- When the anthropic-oauth extension asks for it: `POST /pi-auth/sync` on
  `PRISM_HOST_API`. Both refresh paths (`refreshAnthropicToken` and
  `refreshIfNeeded`) call it while they hold the shared refresh lock:
  before they read `auth.json`, and after they write a new token
  (`syncSessionAuth`, `syncSessionAuthSync` in `session-sync.ts`). So a
  session that waits for the lock reads the token that the holder wrote,
  and sends no refresh request. One refresh serves all sessions. The
  endpoint does not take the lock, because its caller holds it. prism sets
  `PRISM_PI_AUTH_SYNC` to `1` only in a sandbox that has a per-session
  `auth.json`. Without it, the extension does not call the endpoint.
- On a timer, every 3 s, when one of the two files changed. The timer sync
  takes the shared refresh lock first (`TakePIRefreshLock`). If the lock is
  held, it tries again at the next tick. The timer brings `prism account
  use`, a refresh of another session, and an Anthropic login in the session
  to the other file with no restart.

The session reads the new file with no restart. In bwrap, `auth.json` is a
symlink into a dir bind, so it follows a rename. In sandbox-exec, the file
is in the dir that `PI_CODING_AGENT_DIR` names.

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

prism agent-run makes both dirs on the host with mode 0700 before each bwrap
or sandbox-exec session starts (`EnsurePISharedStateDirs`). Every sandboxed
session gets the lock dir. Only a session in `NOTION_MCP_REPOS` gets the
Notion store dir: see "Which sessions get the Notion store". bwrap binds each
dir that the session gets read-write at its host path. A dir bind, not a
file bind: the Notion extension renames a temp file onto its store file, and
a rename onto a bind mount fails with EBUSY.

The paths come from `$HOME`, not from `$XDG_STATE_HOME`. All three isolators
run pi with `HOME` set to the host home dir. bwrap clears `XDG_STATE_HOME`,
and sandbox-exec sets it to `<home>/.local/state`, so a path from
`XDG_STATE_HOME` can differ between the isolators.

Every sandbox that gets a dir can write it, and processes that are not
sandboxed use the dirs too: a host-mode pi, a pi in a terminal, and prism
agent-run. So these processes create a lock `owner` file with
`O_CREAT|O_EXCL|O_NOFOLLOW`, and read an `owner` file or a store file with
`O_NONBLOCK`, as a regular file only. A sandbox can swap a dir with an `owner` symlink in after the
`mkdir` of a holder. The create then fails with EEXIST, and no host file
changes. A FIFO at a read name does not stop the reader. In bwrap each dir
is a mount point, and section 21g of sandbox-exec denies a replace of each
dir node, so a sandbox cannot put a symlink in place of a dir.

### The OAuth refresh lock (#3289)

pi takes `auth.json.lock` in the agent dir around its own refresh. In bwrap
and sandbox-exec that lock dir is private to the session. So the
anthropic-oauth extension
takes `anthropic-oauth-refresh.lock` in the shared lock dir around both of
its refresh paths: the `refreshToken` handler that pi calls, and
`refreshIfNeeded`. In the lock, it reads `auth.json` again. If another
session already wrote a valid token, it uses that token and sends no
request. Else it refreshes and writes the new token before it releases the
lock. In a sandboxed session, `auth.json` is the file of the session, so in
the lock the extension also asks the sidecar for a sync before the read and
after the write. See "The per-session auth.json".

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
token can make Notion revoke the whole grant. So every session that uses
Notion must use the same store. Host-mode sessions, and the bwrap and
sandbox-exec sessions that get the store, all read and write
`~/.local/state/prism/pi-notion/`, under its lock dir.

The store was in the agent dir before #3290. It moves once:

- prism moves it on the host before each bwrap or sandbox-exec session
  starts (`migratePINotionStore`), also before a session that does not get
  the store.
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

### Which sessions get the Notion store (#3319)

The Notion store is a grant of the full Notion workspace. So a bwrap or
sandbox-exec session gets the store dir only when its repo is in
`NOTION_MCP_REPOS`. On the hosts of this repo, that is the obsidian vault.

prism agent-run decides on the host before pi starts (`piNotionStore`). The
session gets the store dir only when both of these conditions are true:

- `SessionProject` gives a project root for the session name and the
  worktree. This is the repo identity of the project-trust decision
  (#3216). The session name comes from the pane command line that prism
  wrote. The worktree comes from `agent_status`. The project roots come from
  `config.json`. The worktree must be a worktree root, or the project dir,
  of the repo that the session name names. So a worktree value cannot point
  the decision at another repo.
- The worktree is a dir in `NOTION_MCP_REPOS`, or is below one
  (`InPINotionRepos`). prism reads the value from `agent_env_vars` in
  `profiles.json`, with the role filter that the session env gets. The
  match is the match of the extension gate: prism resolves the symlinks and
  compares dirs by file identity.

No sandbox can write the pane command line, `prism.db`, `config.json`, or
`profiles.json`. The decision does not read the environment of the sandbox.

| Session | Notion store dir |
|---|---|
| The obsidian root session, in the vault dir. | Yes. |
| An investigator or a review agent of that session, in the same dir. | Yes, the same as its parent. Its name is the parent name with a `~` suffix, so it names the same repo. |
| A session of a repo that is not in `NOTION_MCP_REPOS`. | No. |
| Each session, when `NOTION_MCP_REPOS` is empty or not set. | No. |
| A host-mode session. | Not sandboxed. It uses the host dir, as before. |

A bwrap or sandbox-exec session that does not get the store dir cannot read
or write the store:

- bwrap: the sandbox has no bind of the store dir, and no bind of the old
  store files in the agent dir. A write at the store path stays in the
  sandbox and goes away with it.
- sandbox-exec: the profile has no section-6c grant of the store dir.
  Section 21l denies a read of the old store files in `~/.pi/agent`, which
  section 6a allows for the rest of the dir.

The extension has its own gate on `NOTION_MCP_REPOS`, on the working dir of
the session. With an empty list the extension does not limit Notion to a set
of dirs, but prism gives the store to no sandboxed session. A Notion login
in a sandboxed session that does not get the store does not reach the host
store.

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

## The pre-start check of auth.json

Before each pi start that prism makes, prism also reads the host
`auth.json` and refuses the start if a value in it starts with `!`
(`CheckPIAuthFile`, #3284). Before #3295 a sandbox had write access to
that file, so the check stops a value that such a sandbox put there. The
error names the file and the provider, and holds no value from the file. A
file that Go cannot decode as one JSON object is also refused. The check
runs here:

- host mode: `session.BuildAgentCmd`, before the pane command;
- bwrap and sandbox-exec: `prism agent-run` (`populatePIConfig`), before pi
  starts in the sandbox;
- `prism quick pr`, before its pi call.

## Known limits

Issues and other docs name these limits by number, so a number does not
change when a limit goes away. Limits 1, 2, and 3 are resolved: #3289,
#3295, and #3290.

- **Limit 4.** Session files: the smallest unit that both isolators can grant
  is `sessions/<encoded-cwd>/` of the worktree. Review agents run in the
  worktree of their worker, so a worker and its review agents can write the
  session files of each other. Earlier sessions on the same worktree path
  are in the same dir. Follow-up: #3291.
- **Limit 5.** Both isolators: a session can make or keep a lock dir in the
  shared lock dir, or put a file at a lock-dir name. A session in
  `NOTION_MCP_REPOS` can do the same in the Notion store dir. This is a
  denial of service, not code execution. A lock dir that its holder does
  not keep fresh is stale after 30 s, and the next session removes it. A
  session that keeps a lock dir fresh stops the refresh of
  other sessions: after the wait time they do not refresh, and a token that
  expires is not replaced.

Other limits:

- bwrap: a read-only bind keeps the file that a host entry named when the
  sandbox started. A change from `nh switch` is visible after a respawn. A
  host entry that appears later is not visible in a running sandbox.
- sandbox-exec: the per-session agent dir has a symlink for each host entry
  at the start. A host entry that appears later is not visible until the
  next start. A change in a linked file or dir is visible.
- Both isolators: a session can write the `anthropic` OAuth entry in its
  own `auth.json`, and the sync copies a new valid value to the host file.
  So a session can put the tokens of another Anthropic account in the host
  file, and later sessions then use that account. A session can already
  read these tokens. It cannot put an `api_key` entry, a `!` value, another
  field, or the entry of another provider in the host file.
- Both isolators: when a host pi holds the lock of the host file, the sync
  writes the host file with no lock. The host pi then writes back the file
  that it read before the sync, with its own change. For an Anthropic
  refresh this is safe: the host pi reads the file again under the refresh
  lock and keeps the new token. For a change of another provider, for
  example a GitHub Copilot refresh, the write of the host pi undoes the
  values that the sync wrote in that time. A sandbox that wrote the host
  file under the refresh lock before #3295 had the same race.
- Both isolators: the shared refresh lock covers the Anthropic OAuth
  refresh of the extension only. pi refreshes the OAuth entry of another
  provider under its own `auth.json.lock`, which is private to the sandbox.
  Two sessions can refresh such a provider at the same time. The sync does
  not copy the result to the host file, and the next sync writes the host
  value back to the session file. If the host value is expired, the session
  refreshes again at its next request. A Copilot refresh token does not
  rotate, so this is safe for Copilot. For a provider whose refresh token
  rotates, a refresh in a session makes the host refresh token invalid. Log
  in on the host again for that provider.
- Both isolators: a session can write the Atlassian file. It can put the
  tokens of another account in it, and later sessions then use that account.
- Both isolators: a session in `NOTION_MCP_REPOS` can read and replace the
  Notion store files. It can put the tokens or the client registration of
  another workspace in them. A session that is not in the list cannot read
  or write the store (#3319).
