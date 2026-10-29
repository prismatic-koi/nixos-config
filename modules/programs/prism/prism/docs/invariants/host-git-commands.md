# Invariant: host git commands

<!-- doclint-ignore: .bare/commondir, .bare/config, .bare/hooks, .bare/worktrees, core.hooksPath, core.fsmonitor, core.sshCommand, credential.helper, extensions.worktreeConfig, config.worktree, submodule.recurse, fetch.recurseSubmodules, push.recurseSubmodules, diff.ignoreSubmodules, protocol.ext.allow, GIT_CONFIG_PARAMETERS, GIT_PAGER, gitdir, commondir, rebase-merge/autostash, info/attributes, b:git_dir -->
<!-- doclint-ignore: status.submoduleSummary, protocol.file.allow, core.askPass, core.editor, sequence.editor, core.alternateRefsCommand, core.attributesFile, gpg.program, gpg.openpgp.program, gpg.x509.program, gpg.ssh.program, gpg.ssh.defaultKeyCommand, interactive.diffFilter, maintenance.auto, core.gitProxy, core.pager, diff.external, gc.recentObjectsHook, uploadpack.packObjectsHook -->
<!-- doclint-ignore: merge.default, GIT_ALLOW_PROTOCOL, protocol.allow -->
<!-- doclint-ignore: GIT_ATTR_SOURCE, GIT_PROXY_COMMAND, GIT_NO_LAZY_FETCH, GIT_SSH_COMMAND, GIT_ASKPASS, GIT_EDITOR, SSH_ASKPASS, info/attributes, .bare/info, .bare/info/attributes, objects/info/alternates, .gitattributes, imap.tunnel, man.viewer, web.browser, modules/programs/git.nix -->
<!--
  These names are git config keys, git environment variables, file names
  in a git dir, and a buffer variable of the nvim plugin fugitive. They are
  git and nvim names, not names in this source tree.
-->

Issues: #3322, #3315, #3293, #3347, #3337, #3333. Code: `internal/git/host.go`
(`OpenBare`, `OpenWorktree`, `OpenDir`, `OuterBareRoot`, `EnvForDir`,
`CloneCommand`), `internal/git/bare_record.go`, the prefix+b keybind in the tmux config of prism
(modules/programs/prism/tmux.nix, outside the prism Go tree), and the nvim
config of prism (modules/programs/prism/neovim, outside the prism Go tree).

## The rule

prism runs git on the host only through `git.Repo`. A `git.Repo` sets
`GIT_DIR` and `GIT_COMMON_DIR` to paths that prism computes on the host.
git then reads no `.git` file and no `commondir` file to find its config
and its hooks.

A `gh` or `glab` command that finds its repo with git in a dir gets the
same environment from `git.EnvForDir` or `Repo.Env`.

Every host git command also gets this command-line config, in
`GIT_CONFIG_PARAMETERS`. Command-line config has priority over every config
file, `.bare/config` included. Each row was tested on git 2.56 with a
planted value in `.bare/config` and a payload that writes a marker file.

| Key | Value | Effect |
|---|---|---|
| `core.hooksPath` | `/dev/null` | No hook from a hooks dir runs. |
| `hook.<event>.enabled` (each of the 28 events of githooks(5)) | `false` | No hook that config defines runs (`hook.<name>.command` with `hook.<name>.event`). `core.hooksPath` does not stop these hooks. A config that also sets `hook.<event>.command` defeats this pin: see "Open residual" below. |
| `core.fsmonitor` | `false` | No fsmonitor command runs. |
| `submodule.recurse`, `fetch.recurseSubmodules`, `push.recurseSubmodules` | `false`, `false`, `no` | A submodule has its own git dir in the worktree, with its own config. |
| `diff.ignoreSubmodules` | `all` | A submodule status runs git in the submodule. This is the default only: an `ignore=` entry in `.gitmodules` overrides it. `git.Stat` adds `--ignore-submodules=all` on the command line for that reason. |
| `status.submoduleSummary` | `false` | No summary runs git in a submodule. |
| `protocol.ext.allow`, `protocol.file.allow` | `never` | A second layer under `GIT_ALLOW_PROTOCOL` (below). When `GIT_ALLOW_PROTOCOL` is set, git reads no `protocol.*.allow` config. |
| `core.sshCommand` | `ssh` | The git default. git runs `ssh` from `PATH`, so SSH auth works as before. An empty value makes git run an empty command, and each SSH fetch fails. An inherited `GIT_SSH_COMMAND` still has priority, as before. |
| `credential.helper` | empty | An empty value clears the list of helpers. git reads command-line config last, so the list is empty, `credential.<url>.helper` included. A `gh` child adds its own helper after this entry, so `gh` auth works. |
| `core.askPass` | empty | No askpass program runs. git does not use `SSH_ASKPASS`. An inherited `GIT_ASKPASS` still has priority. |
| `core.editor`, `sequence.editor` | `:` | git runs no editor. An inherited `GIT_EDITOR` still has priority. |
| `core.alternateRefsCommand` | empty | No command runs for the refs of an alternate. A worker can write `objects/info/alternates`. A fetch still works. |
| `core.attributesFile` | `/dev/null` | A planted config cannot name an attributes file that selects a driver. |
| `gpg.program`, `gpg.openpgp.program` | `gpg` | The git default. These run to sign a commit (`prism quick pr`, a rebase with `commit.gpgSign`) and to verify a signature. |
| `gpg.x509.program` | `gpgsm` | The git default. |
| `gpg.ssh.program` | `ssh-keygen` | The git default. An empty value makes git run an empty command. |
| `gpg.ssh.defaultKeyCommand` | empty | No command runs when no signing key is set. The commit then fails. |
| `interactive.diffFilter` | `cat` | No filter runs on `add -p`. `cat` passes the diff through. |
| `maintenance.auto` | `false` | No auto maintenance runs after a fetch, a commit, a merge, or a rebase. Auto maintenance runs `git gc`, and `git gc` runs each value of `gc.recentObjectsHook`. No value of that key turns it off. |
| `merge.default` | a newline | With no `merge` attribute, git uses the merge driver that `merge.default` names. `GIT_ATTR_SOURCE` makes every `merge` attribute unset, so a planted `merge.default` needs no attribute. git looks for a configured driver of that name before a built-in one. So a planted `merge.text.driver` beats the value `text`, and a planted `[merge ""]` section beats an empty value. A subsection name in a config file cannot hold a newline, so no config file can define a driver with this name. git then uses the built-in text merge. |

Every host git command also gets these environment variables:

| Variable | Value | Effect |
|---|---|---|
| `GIT_PAGER` | `cat` | No pager runs. `GIT_PAGER` has priority over `core.pager` and `pager.<command>`. |
| `GIT_ATTR_SOURCE` | the empty tree | git reads no `.gitattributes` file from the worktree, the index, or the tree, so a planted filter, diff, or merge driver does not run. A pinned `Repo` only: git stops with an error when `GIT_ATTR_SOURCE` is set and there is no repository. |
| `GIT_PROXY_COMMAND` | empty | git uses the first `core.gitProxy` that matches, so command-line config cannot replace a planted value. A set `GIT_PROXY_COMMAND` makes git skip `core.gitProxy`, and an empty value means no proxy. |
| `GIT_NO_LAZY_FETCH` | `1` | A planted partial-clone config cannot make git fetch a missing object from a remote that the config names. |
| `GIT_ALLOW_PROTOCOL` | `ssh:https` | git allows only the SSH and HTTPS transports, and reads no `protocol.*.allow` config. A planted `protocol.<name>.allow` beats `protocol.allow = never`, so config cannot do this. Each other transport runs a program that a planted config can choose. The `file` transport runs `remote.<name>.uploadpack`, `remote.<name>.receivepack`, and the hooks of a local repo that a planted URL, push URL, or `url.<base>.insteadOf` names. A planted `remote.<name>.vcs` or `<name>::` URL runs the helper `git-remote-<name>`. If the name has a slash, git runs the helper as a path relative to its cwd, the worktree or `.bare`, and a worker writes both. The `git` transport runs `core.gitProxy`. |

Command-line config cannot stop a submodule status that a `.gitmodules`
`ignore=` entry turns on. The `.gitmodules` file is in the worktree, so the
session writes it. Only the `--ignore-submodules=all` option on the command
line has priority over it.

### Keys that are not in the list

These keys also run a program. Each one is safe for prism host git for the
reason given.

| Key | Reason |
|---|---|
| `filter.<driver>.*`, `diff.<driver>.textconv`, `diff.<driver>.command`, `merge.<driver>.driver`, `merge.<driver>.recursive` | The repo chooses the name, so the list cannot pin it. A filter or diff driver runs only when an attribute selects it. A merge driver runs when an attribute or `merge.default` selects it, and `merge.default` is in the list. `GIT_ATTR_SOURCE` and `core.attributesFile` remove each source of attributes that a worker can write on the config-detach route. `info/attributes` is the other source: see "`info/attributes`" below. |
| `diff.external` | git has no value that turns it off. An empty value makes each diff that writes a patch fail. prism host git writes one patch: `git diff --cached` in `prism quick pr`, and it passes `--no-ext-diff`. The other host diffs write names or numbers only, and git runs no external diff for them. |
| `core.pager`, `pager.<command>` | `GIT_PAGER` has priority. |
| `core.gitProxy` | `GIT_ALLOW_PROTOCOL` refuses the `git` transport, and `GIT_PROXY_COMMAND` (above) also turns the key off. |
| `gc.recentObjectsHook` | `maintenance.auto = false` (above). prism host git runs no `git gc`. |
| `remote.<name>.uploadpack`, `remote.<name>.receivepack`, `remote.<name>.url`, `url.<base>.insteadOf` | For an SSH remote, the remote server runs the upload-pack and receive-pack command, not the host. `GIT_ALLOW_PROTOCOL` refuses a local remote. |
| `remote.<name>.vcs`, a `<transport>::<address>` URL | `GIT_ALLOW_PROTOCOL` refuses each transport except SSH and HTTPS, so git runs no `git-remote-<name>` helper for a planted name. The HTTPS transport runs `git-remote-https` from the exec path of git. |
| `uploadpack.packObjectsHook` | git reads it only from the system, global, and command-line config. |
| `alias.*` | prism host git runs built-in commands only. An alias cannot replace a built-in command. |
| `diff.tool`, `merge.tool`, `man.viewer`, `web.browser`, `trailer.<token>.cmd`, `submodule.<name>.update`, `sendemail.*`, `imap.tunnel`, `tar.<format>.command` | Only `git difftool`, `git mergetool`, `git help`, `git interpret-trailers` (or `--trailer`), `git submodule update`, `git send-email`, `git imap-send`, and `git archive` run them. prism host git runs none of these commands. |

### `info/attributes`

`GIT_ATTR_SOURCE` does not cover `$GIT_COMMON_DIR/info/attributes`, and no
config key or environment variable turns off this file. A driver that
`info/attributes` selects runs in prism host git. On the config-detach route
the sandbox stops the write: `sandbox-exec` denies a write in `.bare/info`,
and bwrap binds `.bare/info` read-only as a directory. A rename of
`.bare/config` does not detach that bind. A session that makes its own
`.bare` at a project root also writes `info/attributes`. On the host,
`OpenBare` refuses a `.bare` that prism did not make (see "The bare repo
record" below).

### Open residual: a config hook with a friendly name (#3349)

git 2.56 applies `hook.<event>.enabled = false` to the whole event only when
no config entry uses the event name as a hook name. A planted
`hook.<event>.command` makes the event name a hook name. The pin then turns
off only that hook, and a planted hook with another name for the same event
runs. No command-line value turns off a hook whose name prism does not know.
This is open, and #3349 tracks it. The fix is a `.bare/config` that a worker
cannot write (#3327, #3334).

### The config-detach route

A host rename of `.bare/config` detaches the read-only bwrap pin, and a
worker can then write `.bare/config` (#3337, `sandbox-git-write-scope.md`).
The list and the environment above close this route for prism host git: a
planted `.bare/config` runs no command on a host fetch, push, worktree add,
checkout, commit, rebase, or config command. The one exception is the
config hook of #3349 above. A planted `.bare/config` can still name
another SSH or HTTPS URL for a remote. That runs no program on the host,
and the refs of `.bare` are outside this rule ("Known limits").

## Why

A sandboxed session can write its own worktree and its own git dir,
`.bare/worktrees/<own>`. It can point the git dir of a host git command at
a dir with its own config and hooks:

- `<worktree>/.git` names a git dir.
- `.bare/worktrees/<own>/commondir` names the common dir, where git reads
  `config` and `hooks/`.
- `.bare/commondir` does the same for a command with `GIT_DIR=.bare`. A
  bwrap session can write it: #3315 deliberately does not pin the top-level
  `.bare/commondir` (no commondir content is a no-op — any such file makes
  git treat the project root as a work tree). bwrap cannot bind `.bare`
  read-only to stop the write (it breaks commit — see
  `sandbox-git-write-scope.md`); the host-side control is `OpenBare`
  refusing a repo that has a `.bare/commondir` file.
- A `.bare` in a subdir of the worktree. A walk up from a cwd in that
  subdir finds it before the real project root. A sandboxed coordinator
  can also name that subdir as the project root, through the `repo` field
  of a `/spawn` request.

A config can set `core.fsmonitor`, `core.sshCommand`, `credential.helper`,
or a filter driver. Each runs a command. A hook runs a command. So a host
git command that lets git find its git dir runs a command that the session
wrote, with the rights of the user.

The command-line config and the environment above turn off these keys in
the common config. They cannot turn off a driver that a planted
`info/attributes` selects, or the config hook of #3349. For these, the only
defence is a git dir and a project root that the session cannot write.

#3293 and #3315 stop a direct write to `.bare/config`, `.bare/hooks`, and
the git dirs of other worktrees. Sections 21j and 21k of the sandbox-exec
profile also deny a write of `<worktree>/.git` and of the redirect files of
the git dir of the session, and #3321 does the same for bwrap. See
`docs/invariants/sandbox-git-write-scope.md`. The host-side pin stays as
defence in depth: it does not depend on the sandbox rules, and it also
covers a host mode session and a `.bare` in a subdir of a worktree.

## How prism computes the dirs

- **Bare repo.** `OpenBare(root)` resolves the symlinks in `root` one
  time. Each check below and `GIT_DIR` use the resolved path, so a symlink
  that changes after the checks does not move git to another dir. It uses
  `<root>/.bare`. It refuses a `.bare` that is a symlink or a file, and a
  `.bare/commondir`. A bare repo has no `commondir` file. It also refuses a
  root inside another prism project: that root is in a worktree, so a
  session can have made its `.bare`. On the host, it also refuses a `.bare`
  that prism did not make (see "The bare repo record" below).
- **Worktree.** `OpenWorktree(root, worktree)` does not read
  `<worktree>/.git`. The git dir is the entry in `.bare/worktrees` whose
  `gitdir` file points back at `<worktree>/.git`. A session can write the
  `gitdir` file of its own entry only. If two entries point at the
  worktree, `<worktree>/.git` chooses one of them.
- **Commondir file.** git reads refs through the `commondir` file of the
  git dir, also when `GIT_COMMON_DIR` is set. Config and hooks obey
  `GIT_COMMON_DIR`. So a planted `commondir` cannot run a command, but it
  moves the refs. `OpenWorktree` refuses a git dir whose `commondir` file
  does not name the bare repo.
- **A dir that can be in a worktree.** `OpenDir(dir)` finds the project
  root with `OuterBareRoot`: the outermost dir at or above `dir` that has
  a `.bare`. A session cannot make a `.bare` above the project root. In a
  prism project, an error from `OpenWorktree` is returned. prism does not
  fall back to a command that is not pinned.
- **`extensions.worktreeConfig`.** With this key on, git also reads
  `config.worktree` from the git dir, which the session can write.
  `OpenBare` refuses the repo when the common config turns the key on.

A caller gets the project root from a source that a session cannot write:
the session record, a project name under a configured project location, or
`OuterBareRoot`. `BareRoot(worktree)` is safe only for the top dir of a
worktree, because its walk starts at the parent.

The `repo` field of a `/spawn` request comes from the session. The handler
accepts only a plain project name: no path separator, not `.` or `..`, and
no leading `-` or `~`. `resolveRepo` in `cmd/spawn.go` refuses a root
inside another project, for a path that a user types on the host too.

## Project roots

Issue: #3330. Code: `IsProjectRoot` and `SameDir` in
`internal/config/project_roots.go`, `OpenBare` in `internal/git/host.go`,
and `switchPathForSession` in `internal/sidecar/switch_path.go`.

- `OpenBare(root)` refuses a root that is not a prism project root in
  config.json. A prism project root is a dir in a `project_locations`
  entry, or a `project_specific` entry. The error wraps
  `ErrNotProjectRoot`.
- A sandboxed session can write dirs outside the project roots, for
  example under `~/.npm`. It can make a `.bare` there with a config that
  defines a filter driver and an `info/attributes` that selects it. The
  environment of host git does not turn off that driver. So host git must
  not run in such a root.
- `OpenBare` resolves the symlinks in the root first. Then it compares the
  resolved root with each entry by file identity (`SameDir`). So it
  accepts a root under a symlinked project location, and a root with
  another letter case on a case-insensitive macOS volume. A root that is
  itself a symlink passes only if the dir that it names is a project root.
  `projecttrust` uses the same reader.
- `OpenWorktree`, `OpenDir`, and `EnvForDir` call `OpenBare`, so they
  refuse such a root too. `CloneWorktree` and `ConvertToBare` refuse it
  before the clone. A refused `CloneWorktree` removes each dir that it
  made, also the parents of the target.
- A sandbox cannot read config.json. So in a sandbox, `OpenBare` uses the
  default project roots, `~/code` and `~/documents/obsidian`. If a repo is
  under another root, each prism command that opens it in a sandbox stops
  with `ErrNotProjectRoot`. Examples are `prism quick pr` and the
  `--rebase` steps of `prism review`. A prism command on the host reads
  config.json, so the host is not affected.

`/switch` gives a dir to `prism switch --path`, which runs host git in it.
The handler takes that dir from the project roots and from host git, not
from `agent_status`. The `/event` proxy writes the `worktree` column of
the row of its own session, so a sandboxed session can set that value.

- For `<repo>@<branch>`, the dir is the worktree of the project root named
  `<repo>` that `session.NameFor` gives that name. For a name with no
  branch, the dir is a project root with that name and no `.bare`. A
  descendant gets the dir of its root session.
- The reach of `/switch` is each project root with no `.bare`, and each
  listed worktree that `OpenDir` gives as the work tree. The dir does not
  need a row in the DB. `prism switch` makes the session when it does not
  exist. So a coordinator can start a session in each of these dirs. Only
  a caller with the coordinator role can call `/switch`.
- A session can write the `gitdir` file of its own entry in
  `.bare/worktrees`. Then `git worktree list` lists a path that the
  session chose. `/switch` accepts a listed path only if it is in the
  project root and `OpenDir` gives it as the work tree. It refuses a path
  whose first part below the project root is `.bare`: the session can
  write its own git dir there, and no prism worktree is in `.bare`.
- If no dir has the name, `/switch` returns HTTP 404. If two dirs have
  it, `/switch` returns HTTP 500 and names the two dirs. A dir that git
  lists two times counts one time. A session can point the `gitdir` file
  of its own entry at another worktree, and git then lists that worktree
  two times.

Tests: `TestOpenBare_RefusesRootOutsideProjectRoots` plants a `.bare`
with a smudge filter that `info/attributes` selects, outside the project
roots. It makes sure that the plant runs with plain pinned git and does
not run with prism.
`TestOpenBare_AcceptsConfiguredProjectRoots` covers the symlink and the
letter case. `TestCloneWorktree_RefusesTargetOutsideProjectRoots` covers
the dirs of a refused clone. `TestSwitchPathForSession`,
`TestSwitchPathForSession_RedirectedOwnEntry`,
`TestWorktreeRootIn_RefusesPathInBare`,
`TestSwitchPathForSession_DirListedTwice`, and
`TestHostAPI_Switch_IgnoresTheAgentStatusWorktree` cover `/switch`.
`TestSwitch_TwoStepPathToAWritableDir_3330` runs the two steps of the
issue from end to end.

## The bare repo record

Issue: #3333. Code: `internal/git/bare_record.go`, and the record step of
`CloneWorktree` and `ConvertToBare` in `internal/git/git.go`.

The sandboxed root session of a plain clone runs in its project root, and
its sandbox can write that root. So the session can make `<root>/.bare`
with a filter driver that `info/attributes` selects, and with a config
hook (#3349). That root is a prism project root, so the project-root
check accepts it. A later `prism spawn` runs `git worktree add` in it on
the host. `GIT_ATTR_SOURCE` and the command-line config do not stop that
driver or that hook.

- On the host, `OpenBare` runs git only in a `.bare` that prism made.
  For each other `.bare`, it returns an error that wraps
  `ErrNotPrismBare`. Each command that opens the root through
  `OpenBare` refuses it too.
- `CloneWorktree` and `ConvertToBare` record each `.bare` that they
  make. An entry holds the resolved path and the inode of the dir. The
  record is the file `bare-repos.json` in the prism state dir, the dir
  of `prism.db`. No sandbox can write that dir.
- `OpenBare` accepts a `.bare` only if an entry names the same dir with
  the same inode. A copy of a `.bare` has another inode. A new `.bare` at
  the path of a removed project has another inode too.
- If an entry names the dir with another inode, the entry is stale, and
  `OpenBare` removes it. So a session cannot make one `.bare` after
  another until one gets the recorded inode.
- A failed `CloneWorktree` and a rollback of `ConvertToBare` remove the
  entry before they remove the `.bare`, because a new dir can get the
  inode of the removed dir.
- prism never makes a raw bare git dir as a root (`IsRawBareGitDir`), so
  no entry names one. On the host, `OpenBare` refuses such a root.
- `CloneWorktree` and `ConvertToBare` refuse in a sandbox and change
  nothing. The error wraps `ErrBareInSandbox`. A sandbox cannot write the
  record, so the host refuses a `.bare` that a sandbox makes. Run
  `prism convert` and `prism clone` in a shell on the host.

If there is no record file, prism starts a new record. It records each
`.bare` in a project root of config.json, so a project that prism made
before the record existed stays usable. It skips a root whose `.git` is a
directory, because that root is a plain clone. A `.bare` that a session
makes after the start is not in the record.

A sandbox cannot read the prism state dir. So in a sandbox, prism does
not use the record, and `OpenBare` accepts a `.bare` that is not in it.
In a sandbox, git runs with the rights of the session, so such a `.bare`
gives the session nothing more. prism finds a sandbox with
`sandboxenv.IsInsideSandbox`, which reads `PRISM_HOST_API`. The sidecar
sets that variable only in a sandboxed session.

If `OpenBare` refuses a `.bare` that you made, do these steps:

1. Stop each session that runs in a plain clone.
2. Remove `bare-repos.json` from the prism state dir.
3. Run the prism command again. prism starts a new record.

Host git also reads no `.gitattributes` file (`GIT_ATTR_SOURCE`, #3337).
The record covers the two routes that `GIT_ATTR_SOURCE` does not cover:
`info/attributes` and the config hook of #3349.

Tests: `TestOpenBare_RefusesBareThatPrismDidNotMake` plants a `.bare` in
a plain clone in a project location, with a smudge filter that
`info/attributes` selects and a config hook. It makes sure that the plant
runs with plain pinned git and does not run with prism.
`TestBareRecord_StartsFromProjectRoots`,
`TestBareRecord_ReplacedBareRefused`,
`TestBareRecord_ConvertRecordsAndRollbackForgets`, and
`TestBareRecord_NotUsedInSandbox`, and
`TestBareRecord_NoBareMadeInSandbox` cover the start, the inode, the
rollback, and the sandbox. `TestOpenBare_ChecksTheResolvedRoot` covers the
one resolve of the root. `OpenBare` gives a resolved common dir, and a
work tree keeps the path of the caller.
`TestPreflight_HostGit_RebaseThroughSymlinkedParent` makes sure that the
`--rebase` pre-flight still finds the branch of the session when the path
goes through a symlink. The fixtures of `cmd` and `internal/container`
make `.bare` with plain git, so their `TestMain` calls
`git.AllowUnrecordedBareForTest`.

## The `/review` rebase pre-flight (#3323, #3335)

`prism review` runs a pre-flight that fetches the base ref and checks that it
is an ancestor of `HEAD`. With `--rebase` it also rebases the worktree onto
the base and force-pushes.

**Where the rebase steps run (#3335).** The base-ref fetch and the
ancestor check always run on the host, through a pinned `git.Repo`. The
`--rebase` steps — fetch, rebase, and force-push — run where the session has
write rights:

- **A sandboxed session (bwrap, sandbox-exec).** The steps run in the session
  sandbox, not on the host. The in-sandbox `prism review` runs them with the
  worker's own git (`cmd/review.go` calls `review.Preflight` with
  `Rebase: true` in the container), then proxies the review to the host with
  no rebase, so the host subprocess runs only the non-rebase pre-flight. The
  host-API `/review` handler refuses a rebase request — every `/review`
  caller is sandboxed, because the sidecar injects `PRISM_HOST_API` only into
  a sandboxed session. So a planted `rebase-merge/autostash`, `head-name`, or
  `orig-head` applies at the worker's own trust level: it cannot make the
  host run a command or move a host-owned local branch such as `main`. This
  closes the success-path and abort-path races that #3323 left open on the
  host.
- **A host-mode session.** There is no separate sandbox, so the steps run in
  the session's own environment, which is the host. A host-mode session is
  not isolated from the host, so this is not a trust boundary.

The host-direct path enforces this split: when `--rebase` is set it reads the
parent session's isolation mode and refuses unless the mode is `host` (it
fails closed when the mode cannot be read). This covers the host `term`
window of a sandboxed session, a shell on the host with no `PRISM_HOST_API`,
from which `prism review --rebase` would otherwise reach the host-direct path.

The in-sandbox rebase resolves the project root with the default project
locations (`~/code`, `~/documents/obsidian`), because a sandbox cannot read
config.json (see "Project roots" above). A repo under a non-default
`project_locations` entry would make the in-sandbox `--rebase` refuse with a
"not a prism project root" error. No host in this repo sets a non-default
location, so nothing breaks today.

The rules below bound the host-mode `--rebase` steps, and the non-rebase
pre-flight that runs on the host for every session:

- It runs only in a prism worktree. For a session that is not in a prism
  worktree (a plain clone, a bare session dir), `git.OpenDir` does not pin
  the git dir and runs no git command, so the pre-flight refuses and runs no
  host git command in that dir.
- It refuses a worktree whose git dir already holds `rebase-merge`,
  `rebase-apply`, `sequencer`, `MERGE_HEAD`, `CHERRY_PICK_HEAD`, or
  `REVERT_HEAD`. A session can plant a `rebase-merge` dir with an `autostash`
  file, and `git rebase --abort` applies that stash.
- With `--rebase` it does not run `git rebase --abort`. On a conflict it
  removes the rebase state dir, re-attaches `HEAD` to the branch, and runs
  `reset --hard`. It applies no stash on this abort path, so a planted
  `autostash` file does not run through it.
- With `--rebase` it pushes the branch of the session record — the path of
  the worktree below the project root — not the branch that `HEAD` names. A
  session can write `HEAD` in its own git dir, so the push names the branch
  with an explicit `refs/heads/<branch>` refspec.
- With `--rebase`, it refuses when it cannot derive that branch from the
  worktree path, and when `HEAD` is not on that branch. The rebase acts on
  `HEAD`, so this keeps the rebase, the push, and the restore on one branch;
  prism never falls back to a bare `git push` that follows `HEAD`. This guard
  holds against a host-mode session only, which is not a trust boundary. The
  old race in the window between the `HEAD` check and the start of
  `git rebase` no longer reaches a host rebase, because a sandboxed session
  runs no rebase on the host.

Code: `internal/review/preflight.go`, `cmd/review.go` (the sandboxed
container branch runs the rebase in the sandbox and proxies with no rebase;
the host-direct path refuses `--rebase` for a non-host session), and the
sidecar `/review` handler in `internal/sidecar/host_api.go` (it refuses a
rebase request). Tests:
`internal/review/preflight_host_git_test.go`,
`cmd/review_rebase_sandbox_3335_test.go`, and
`TestHostAPI_Review_RebaseRefusedOnHost`.

## What prism does not change

- Fetch and push on the host use SSH auth, as before. git runs `ssh` from
  `PATH`, and `ssh` reads the SSH config and the agent of the user. The
  global git config of the owner sets no `core.sshCommand` and an empty
  `credential.helper` (`modules/programs/git.nix`), so the pins give the
  same values.
- A `gh` command adds its own credential helper on the command line, after
  the empty `credential.helper`, so `gh` auth works.
- An inherited `GIT_SSH_COMMAND`, `GIT_ASKPASS`, or `GIT_EDITOR` keeps
  priority over the pins. A worker cannot set the environment of prism on
  the host.
- `git clone` makes a new repo, so it has no git dir to pin.
  `CloneCommand` gives it the command-line config only.

## The nvim panes (#3347)

prism starts nvim on the host, in the worktree of each session. This
section is about the git plugins of that nvim. The root of the plugins is
in git-root.nix, and the plugin configs are in plugins/, both in
modules/programs/prism/neovim.

### The nvim-pane rule

The git plugins of the prism nvim run a git command that can start a
filter, a diff driver, or an external diff in one repo only: the repo of
the dir where nvim starts. This repo is the root. In a prism session, the
root is the worktree.

- In a subdir of the root, no plugin runs such a command with the git dir
  of a `.git` dir or a `.git` file. A nested repo and a staged submodule
  both have such a `.git`.
- At the root, the `git status` and `git diff` of gitsigns, Telescope, and
  nvim-tree give `--ignore-submodules`. So git does not run in a submodule.
- No Telescope git command runs a diff driver or `diff.external`, at the
  root too.

### Why the nvim-pane rule exists

A worker can write a `.git` dir or a `.git` file in any subdir of its
worktree. It can also stage a submodule, with its own git dir, in its own
index. The worker controls the config of these git dirs. That config can
define a filter driver, a diff driver, or `diff.external`. An
`info/attributes` file in the same git dir selects the driver. The
overrides of `user-git-overrides.md` do not turn off these keys.

By default, gitsigns, lualine, and fugitive find the git dir from the dir
of the buffer. Telescope finds it from the nvim cwd. nvim-tree finds it
from each dir that it shows. So a planted driver runs on the host when the
owner opens a file in the subdir, opens the file tree, or opens a git
picker. The owner does no `cd`. This route works on `bwrap` and
`sandbox-exec`, because the worker writes in its own worktree.

### How the plugins use the root

At startup, git-root.nix walks up from the startup dir to the first dir
that holds a `.git` entry. That dir is the root. The module reads the git
dir of the root from its `.git` entry. A later `:cd`, or a cwd change by
nvim-tree, does not change the root.

| Plugin | Control |
|---|---|
| gitsigns | The automatic attach is off. An autocmd attaches each buffer with the git dir and the work tree of the root, on the git command line. So git does no discovery. A file in a nested repo or a submodule is not in the index of the root, so gitsigns does not attach to it. |
| lualine | The `diff` component reads the counts from gitsigns. It runs no git. |
| Telescope | Each git picker gets the root as its cwd, and the git dir and the work tree of the root. `diff`, `show`, `log`, and `stash show` get `--no-ext-diff` and `--no-textconv`. `diff` and `status` get `--ignore-submodules=all`. |
| nvim-tree | It runs `git status` only when the toplevel is the root. Its `git status` gets `--ignore-submodules=all`. |
| fugitive | Each buffer has `b:git_dir` set to the git dir of the root. So `:Git` acts on the root. |

If nvim starts outside a git repo, there is no root. Then gitsigns attaches
to no buffer, the Telescope git pickers refuse, nvim-tree shows no git
status, and fugitive is off.

The Telescope and nvim-tree controls replace a private function of the
plugin. If a plugin update removes that function, the control fails
closed. The Telescope git pickers refuse and show an error. nvim-tree
turns off its git integration and shows an error at startup.

### Limits of the nvim-pane rule

- **Read-only git outside the root.** lualine `branch` reads the `HEAD`
  file of the nearest git dir of the buffer. For a reftable repo, it runs
  `git symbolic-ref` and `git rev-parse` in that git dir. nvim-tree runs
  `git rev-parse` in each dir that it shows, to find the toplevel. These
  commands run no hook, filter, diff driver, or external diff.
- **The root repo.** The rule does not change what git reads in the root.
  A planted driver at the root needs a planted `.bare/config`, the route of
  #3346. The `:Git` status of fugitive runs `git status` with no
  `--ignore-submodules` option. So a staged submodule with `ignore = none`
  makes it run git in the submodule. #3353 tracks this route. It is the
  route of #3343, through fugitive.
- **A typed command.** A `:cd`, a `:Gitsigns attach`, and a git command in
  `:!` or `:terminal` use git discovery. Each is a step by the owner, the
  same as a `cd` in the shell (see `user-git-overrides.md`).
- **A language server in the host nvim.** It runs project code by design.
  The threat model puts it out of scope.

### Tests of the nvim-pane rule

The tests are manual. They use a scratch bare+worktree layout, git 2.56,
nvim 0.12, and the plugin versions of this branch. A nested repo, a staged
submodule with `ignore = none`, and a symlink to a repo outside the
worktree each hold a planted clean filter, textconv driver, and
`diff.external`. Each step first runs the nvim config of `main` and makes
sure that the payload runs. Then it runs the config with this rule and
makes sure that the payload does not run. The PR of #3347 holds the full
table.

## Known limits

- **A `.bare` that prism made, in a root that a session can write.**
  `prism convert`, and the convert entry of `prism switch`, run while the
  sandboxed root session of the plain clone runs. That session can write
  the new `.bare` until prism stops it. The record does not stop this,
  because prism made that `.bare`. #3361 tracks this route.
- **The start of the record (#3364).** The start records each `.bare` in
  a project root, so it also records a `.bare` that a session planted
  before the start. This route is in scope. The root session of a plain
  clone can plant the `.bare` and replace its `.git` dir with a `.git`
  file. It has the coordinator role, so it can then call `/spawn`, which
  runs `git worktree add` on the host. The start skips a plain clone whose
  `.git` dir is still there. #3364 tracks the fix: start the record from
  the DB start records of the sessions, which a session cannot write, or
  from a one-time confirmation of each `.bare` by the owner.
- **A moved, copied, or restored project.** The `.bare` has another inode
  or another path, so `OpenBare` refuses it. Do the steps in "The bare
  repo record" above.

- **A project that is not a prism project.** Outside a prism project,
  `OpenDir` does not pin the git dir: git finds it from the dir. A session
  that runs in a plain clone can write its `.git`, so a host git command
  in that clone reads config that the session wrote.
- **Tools in the host panes.** prism starts nvim and a shell in the edit
  and term windows of a session, on the host, in the worktree. The
  `git.Repo` rule does not cover them. #3326 hardens the git environment of
  these panes: it turns off the code-execution config keys for every user
  git process (see `user-git-overrides.md`). A planted driver that a
  `.gitattributes` at the worktree root selects is a known limit (#3346).
  The git plugins of nvim obey the nvim-pane rule above (#3347). A `cd`
  into a subdir that holds a nested git dir, and then a git command, is a
  step by the owner and an accepted limit (see `user-git-overrides.md`).
- **Refs.** A session can write the refs of `.bare`, and a host command
  acts on those refs. For example, a session can change a remote-tracking
  ref, and a later `git rebase origin/<base>` with no fetch first uses the
  changed value. This rule is about commands that run, not about ref values.
  The `--rebase` pre-flight runs on the host only for a host-mode session;
  there it pushes the branch of the session record, not the branch that
  `HEAD` names (#3323, #3335, in the `/review` pre-flight section above). A
  sandboxed session runs its `--rebase` steps in its own sandbox.
- **A remote that is not SSH or HTTPS.** prism host git fetches and
  pushes `origin` only. `origin` comes from the URL of `prism clone` or the
  clone prompt of `prism switch`, or from the `origin` of a repo that
  `prism convert` converts. If that URL is a local path, an `http://` URL,
  or a `git://` URL, the clone or the fetch stops with
  `transport '<name>' not allowed`.
- **No auto maintenance.** prism host git starts no `git gc`. User git and
  bwrap sessions still start it. A `sandbox-exec` session cannot start it,
  so loose objects collect until a user git command on the host starts it.
- **A SHA-256 repo.** `GIT_ATTR_SOURCE` names the SHA-1 empty tree. In a
  SHA-256 repo, a host git command that reads attributes stops with
  `bad --attr-source or GIT_ATTR_SOURCE`. It runs no driver.
- **`git apply`.** On git 2.56, `git apply` stops with a segmentation
  fault when `GIT_ATTR_SOURCE` is set (`user-git-overrides.md`). prism
  host git and the `gh` commands of prism run no `git apply`.
- **A key that a later git version adds.** The list is for git 2.56. Do
  the tests again when git changes.

## Tests

- `internal/git/host_test.go`: one test for each redirect. Each test first
  runs a plain git command with the plant and makes sure that the payload
  runs. Then it runs the prism command and makes sure that the payload does
  not run. The payload writes only to a marker in `t.TempDir()`.
- `internal/review/preflight_host_git_test.go`: the `/review` pre-flight
  with `--rebase` on a bare+worktree layout, with a `.git` redirect and a
  `commondir` redirect, and `runGH` with a stub `gh` that runs git. It also
  covers #3323: the refusal of a non-worktree session, the refusal of a
  pre-existing rebase or merge state, the autostash chain (control: `git
  rebase --abort` runs the planted filter; fix: the pre-flight refuses it),
  and the stash-free conflict restore. It also carries the #3335
  control (`TestPreflight_HostGit_SuccessPathChainReproducesOnHostRebase`): a
  host git rebase reads a planted `head-name`, `orig-head`, and `autostash`
  mid-rebase and moves `main` and applies the stash — the escalation the
  routing guards keep off the host.
- `cmd/review_rebase_sandbox_3335_test.go` and
  `TestHostAPI_Review_RebaseRefusedOnHost` (#3335): the sandboxed
  `prism review --rebase` runs the rebase in the sandbox and proxies with no
  rebase; the host-API `/review` handler refuses a rebase request and spawns
  no `prism review` subprocess for it; the host-direct path refuses
  `--rebase` for a sandboxed (non-host) session; and `rebaseInSandbox` runs
  the PR-state check before the rebase.
- `TestHostAPI_Spawn_RepoMustBeAPlainName` and
  `TestResolveRepo_RefusesRootInsideProject`: the `/spawn` handler refuses
  a `repo` value that is a path, and `resolveRepo` refuses a root inside
  another project. `TestHostGit_NestedRootRefused` plants a `.bare` with a
  smudge filter in a worktree subdir and makes sure that `OpenBare` refuses
  it.
- `internal/git/host_config_pins_test.go` (#3337): one test for each class
  of key in the list. Each test plants the keys in `.bare/config`, runs a
  plain git command to make sure that the payload runs, and then runs the
  prism command. The commands are a fetch, a push, a worktree add, a
  checkout, a status, a diff, a commit, a signed commit, a rebase with a
  conflict, and a config command. For `core.sshCommand` and the signing
  programs, a stand-in program on `PATH` makes sure that git runs the
  pinned program. `TestHostGit_PlantedMergeDefault` plants
  `merge.default` with no attribute, and also the driver names `text` and
  `""`. `TestHostGitConfig_DocListsEveryKey` makes sure that this doc lists
  each key. `TestStagedDiff_NoExternalDiff` covers the `--no-ext-diff` of
  `prism quick pr`. `TestHostGit_PlantedRemoteHelper` plants a helper by a
  relative path and on `PATH`, and allows its transport in config.
- The test fixtures of `cmd`, `internal/git`, `internal/review`, and
  `internal/sidecar` use a local origin. Their `TestMain` calls
  `git.AllowFileTransportForTest`. `TestHostGit_FileTransportRefused`
  turns the file transport off again and makes sure that git refuses it.
- `internal/git/host_guard_test.go`: `TestHostGit_NoDirectGitExec` fails
  if a non-test file outside `internal/git/host.go` starts git itself. It
  also fails if a function starts `gh` or `glab` and does not call
  `git.EnvForDir` or `Repo.Env`, unless the test names the function with a
  reason. An assignment such as `cmd.Env = os.Environ()` is not a call, so
  it does not satisfy the check. `TestHostGit_GuardFindings` holds the
  negative cases.
