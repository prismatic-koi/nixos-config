# Invariant: host git commands

<!-- doclint-ignore: .bare/commondir, .bare/config, .bare/hooks, .bare/worktrees, core.hooksPath, core.fsmonitor, core.sshCommand, credential.helper, extensions.worktreeConfig, config.worktree, submodule.recurse, fetch.recurseSubmodules, push.recurseSubmodules, diff.ignoreSubmodules, protocol.ext.allow, GIT_CONFIG_PARAMETERS, GIT_PAGER, gitdir, commondir -->
<!--
  These names are git config keys, git environment variables, and file
  names in a git dir. They are git names, not names in this source tree.
-->

Issues: #3322, #3315, #3293. Code: `internal/git/host.go`
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
| `diff.ignoreSubmodules` | `all` | A submodule status runs git in the submodule. |
| `protocol.ext.allow` | `never` | No `ext::` remote helper runs. |

`GIT_PAGER` is `cat`, so no pager runs.

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
  subdir finds it before the real project root.

A config can set `core.fsmonitor`, `core.sshCommand`, `credential.helper`,
or a filter driver. Each runs a command. A hook runs a command. So a host
git command that lets git find its git dir runs a command that the session
wrote, with the rights of the user.

#3293 and #3315 stop a direct write to `.bare/config`, `.bare/hooks`, and
the git dirs of other worktrees. They do not stop a redirect, because the
redirect names a dir outside `.bare`.

## How prism computes the dirs

- **Bare repo.** `OpenBare(root)` uses `<root>/.bare`. It refuses a
  `.bare` that is a symlink or a file, and a `.bare/commondir`. A bare
  repo has no `commondir` file.
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
the session record, a configured project location, `--repo`, or
`OuterBareRoot`. `BareRoot(worktree)` is safe only for the top dir of a
worktree, because its walk starts at the parent.

## What prism does not change

- `core.sshCommand` and `credential.helper` keep their values. Fetch and
  push on the host use the credentials of the user, as before. The pinned
  config is `.bare/config`, which a session cannot write after #3293 and
  #3315.
- `git clone` makes a new repo, so it has no git dir to pin.
  `CloneCommand` gives it the command-line config only.

## Known limits

- **A project that is not a prism project.** Outside a prism project,
  `OpenDir` does not pin the git dir: git finds it from the dir. A session
  that runs in a plain clone can write its `.git`, so a host git command
  in that clone reads config that the session wrote.
- **Tools in the host panes.** prism starts nvim and a shell in the edit
  and term windows of a session, on the host, in the worktree. Their git
  plugins and prompts run git there, and this rule does not cover them.
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
- `internal/git/host_guard_test.go`: `TestHostGit_NoDirectGitExec` fails
  if a non-test file outside `internal/git/host.go` starts git itself. It
  also fails if a function starts `gh` or `glab` and does not call
  `git.EnvForDir` or `Repo.Env`, unless the test names the function with a
  reason. An assignment such as `cmd.Env = os.Environ()` is not a call, so
  it does not satisfy the check. `TestHostGit_GuardFindings` holds the
  negative cases.
