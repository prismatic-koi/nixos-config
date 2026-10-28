# Invariant: host git commands

<!-- doclint-ignore: .bare/commondir, .bare/config, .bare/hooks, .bare/worktrees, core.hooksPath, core.fsmonitor, core.sshCommand, credential.helper, extensions.worktreeConfig, config.worktree, submodule.recurse, fetch.recurseSubmodules, push.recurseSubmodules, diff.ignoreSubmodules, protocol.ext.allow, GIT_CONFIG_PARAMETERS, GIT_PAGER, gitdir, commondir -->
<!--
  These names are git config keys, git environment variables, and file
  names in a git dir. They are git names, not names in this source tree.
-->

Issues: #3322, #3315, #3293, #3325. Code: `internal/git/host.go`
(`OpenBare`, `OpenWorktree`, `OpenDir`, `OuterBareRoot`, `EnvForDir`,
`CloneCommand`), and the prefix+b keybind in the tmux config of prism
(modules/programs/prism/tmux.nix, outside the prism Go tree).

## The rule

prism runs git on the host only through `git.Repo`. A `git.Repo` sets
`GIT_DIR` and `GIT_COMMON_DIR` to paths that prism computes on the host.
git then reads no `.git` file and no `commondir` file to find its config
and its hooks.

A `gh` or `glab` command that finds its repo with git in a dir gets the
same environment from `git.EnvForDir` or `Repo.Env`.

Every host git command also gets this command-line config, in
`GIT_CONFIG_PARAMETERS`. Command-line config has priority over every config
file:

| Key | Value | Reason |
|---|---|---|
| `core.hooksPath` | `/dev/null` | No hook runs. |
| `core.fsmonitor` | `false` | No fsmonitor command runs. |
| `submodule.recurse`, `fetch.recurseSubmodules`, `push.recurseSubmodules` | `false`, `false`, `no` | A submodule has its own git dir in the worktree, with its own config. |
| `diff.ignoreSubmodules` | `all` | A submodule status runs git in the submodule. This is the default only: an `ignore=` entry in `.gitmodules` overrides it. `git.Stat` adds `--ignore-submodules=all` on the command line for that reason. |
| `protocol.ext.allow` | `never` | No `ext::` remote helper runs. |

`GIT_PAGER` is `cat`, so no pager runs.

Command-line config cannot stop a submodule status that a `.gitmodules`
`ignore=` entry turns on. The `.gitmodules` file is in the worktree, so the
session writes it. Only the `--ignore-submodules=all` option on the command
line has priority over it.

## Why

A sandboxed session can write its own worktree and its own git dir,
`.bare/worktrees/<own>`. It can point the git dir of a host git command at
a dir with its own config and hooks:

- `<worktree>/.git` names a git dir.
- `.bare/worktrees/<own>/commondir` names the common dir, where git reads
  `config` and `hooks/`.
- `.bare/commondir` does the same for a command with `GIT_DIR=.bare`. A
  bwrap session can write it before #3315.
- A `.bare` in a subdir of the worktree. A walk up from a cwd in that
  subdir finds it before the real project root. A sandboxed coordinator
  can also name that subdir as the project root, through the `repo` field
  of a `/spawn` request.

A config can set `core.fsmonitor`, `core.sshCommand`, `credential.helper`,
or a filter driver. Each runs a command. A hook runs a command. So a host
git command that lets git find its git dir runs a command that the session
wrote, with the rights of the user.

The command-line config below turns off hooks and fsmonitor. It cannot turn
off a filter driver, `core.sshCommand`, or `credential.helper`. For these,
the only defence is a git dir and a project root that the session cannot
write.

#3293 and #3315 stop a direct write to `.bare/config`, `.bare/hooks`, and
the git dirs of other worktrees. Sections 21j and 21k of the sandbox-exec
profile also deny a write of `<worktree>/.git` and of the redirect files of
the git dir of the session, and #3321 does the same for bwrap. See
`docs/invariants/sandbox-git-write-scope.md`. The host-side pin stays as
defence in depth: it does not depend on the sandbox rules, and it also
covers a host mode session and a `.bare` in a subdir of a worktree.

## How prism computes the dirs

- **Bare repo.** `OpenBare(root)` uses `<root>/.bare`. It refuses a
  `.bare` that is a symlink or a file, and a `.bare/commondir`. A bare
  repo has no `commondir` file. It also refuses a root inside another prism
  project: that root is in a worktree, so a session can have made its
  `.bare`.
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
  sets `core.sshCommand` or a filter driver. The command-line config does
  not turn off these keys. So host git must not run in such a root.
- `OpenBare` compares the root with each entry by file identity
  (`SameDir`). So it accepts a root that a symlink reaches, and a root
  with another letter case on a case-insensitive macOS volume.
  `projecttrust` uses the same reader.
- `OpenWorktree`, `OpenDir`, and `EnvForDir` call `OpenBare`, so they
  refuse such a root too. `CloneWorktree` and `ConvertToBare` refuse it
  before the clone.
- A sandbox cannot read config.json. So in a sandbox, `OpenBare` uses the
  default project roots, `~/code` and `~/documents/obsidian`.

`/switch` gives a dir to `prism switch --path`, which runs host git in it.
The handler takes that dir from the project roots and from host git, not
from `agent_status`. The `/event` proxy writes the `worktree` column of
the row of its own session, so a sandboxed session can set that value.

- For `<repo>@<branch>`, the dir is the worktree of the project root named
  `<repo>` that `session.NameFor` gives that name. For a name with no
  branch, the dir is a project root with that name and no `.bare`. A
  descendant gets the dir of its root session.
- A session can write the `gitdir` file of its own entry in
  `.bare/worktrees`. Then `git worktree list` lists a path that the
  session chose. `/switch` accepts a listed path only if it is in the
  project root and `OpenDir` gives it as the work tree.
- If no dir has the name, `/switch` returns HTTP 404. If two dirs have
  it, `/switch` returns HTTP 500 and names the two dirs.

Tests: `TestOpenBare_RefusesRootOutsideProjectRoots` plants a `.bare`
with `core.sshCommand` outside the project roots, and makes sure that the
plant runs with plain pinned git and does not run with prism.
`TestOpenBare_AcceptsConfiguredProjectRoots` covers the symlink and the
letter case. `TestSwitchPathForSession`,
`TestSwitchPathForSession_RedirectedOwnEntry`, and
`TestHostAPI_Switch_IgnoresTheAgentStatusWorktree` cover `/switch`.
`TestSwitch_TwoStepPathToAWritableDir_3330` runs the two steps of the
issue from end to end.

## What prism does not change

- `core.sshCommand` and `credential.helper` keep their values. Fetch and
  push on the host use the credentials of the user, as before. The pinned
  config is `.bare/config`, which a session cannot write after #3293 and
  #3315.
- `git clone` makes a new repo, so it has no git dir to pin.
  `CloneCommand` gives it the command-line config only.

## Known limits

- **The `--rebase` pre-flight and `git rebase --abort`.** A session can
  write `rebase-merge` in its own git dir, with an `autostash` commit. The
  host `git rebase` then fails, and the pre-flight runs `git rebase
  --abort`, which applies the autostash. The stash apply runs `git status`.
  If `.gitmodules` sets `ignore = none` for a gitlink, that status runs git
  in the submodule, and a filter driver from the submodule config runs on
  the host. The pin and the command-line config do not stop this.
  Follow-up: #3323.
- **A project that is not a prism project.** Outside a prism project,
  `OpenDir` does not pin the git dir: git finds it from the dir. A session
  that runs in a plain clone can write its `.git`, so a host git command
  in that clone reads config that the session wrote.
- **Tools in the host panes.** prism starts nvim and a shell in the edit
  and term windows of a session, on the host, in the worktree. Their git
  plugins and prompts run git there, and this rule does not cover them. A
  nested `.git` in a worktree subdir reaches them. Follow-up: #3325.
- **Refs.** A session can write the refs of `.bare`, and a host command
  acts on those refs. For example, the `--rebase` pre-flight pushes the
  branch that `HEAD` of the session names. This rule is about commands
  that run, not about ref values.

## Tests

- `internal/git/host_test.go`: one test for each redirect. Each test first
  runs a plain git command with the plant and makes sure that the payload
  runs. Then it runs the prism command and makes sure that the payload does
  not run. The payload writes only to a marker in `t.TempDir()`.
- `internal/review/preflight_host_git_test.go`: the `/review` pre-flight
  with `--rebase` on a bare+worktree layout, with a `.git` redirect and a
  `commondir` redirect, and `runGH` with a stub `gh` that runs git.
- `TestHostAPI_Spawn_RepoMustBeAPlainName` and
  `TestResolveRepo_RefusesRootInsideProject`: the `/spawn` handler refuses
  a `repo` value that is a path, and `resolveRepo` refuses a root inside
  another project. `TestHostGit_NestedRootRefused` plants a `.bare` with a
  smudge filter in a worktree subdir and makes sure that `OpenBare` refuses
  it.
- `internal/git/host_guard_test.go`: `TestHostGit_NoDirectGitExec` fails
  if a non-test file outside `internal/git/host.go` starts git itself. It
  also fails if a function starts `gh` or `glab` and does not call
  `git.EnvForDir` or `Repo.Env`, unless the test names the function with a
  reason. An assignment such as `cmd.Env = os.Environ()` is not a call, so
  it does not satisfy the check. `TestHostGit_GuardFindings` holds the
  negative cases.
