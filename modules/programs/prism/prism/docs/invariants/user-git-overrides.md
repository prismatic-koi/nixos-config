# Invariant: the git overrides of the user session

<!-- doclint-ignore: core.alternateRefsCommand, core.askPass, core.attributesFile, core.gitProxy, diff.trustExitCode, gpg.openpgp.program, gpg.ssh.defaultKeyCommand, gpg.x509.program, log.showSignature, status.submoduleSummary -->
<!-- doclint-ignore: EDITOR, VISUAL, SSH_ASKPASS, GIT_ATTR_SOURCE, GIT_NO_LAZY_FETCH, GIT_PROXY_COMMAND, vcs_info -->
<!-- doclint-ignore: .bare/config, .bare/info, info/attributes, git.nix, tmux.nix, modules/programs/git.nix, modules/programs/git-builtin-external-diff.sh, systemd.user.sessionVariables -->
<!--
  These names are git config keys, environment variables that git and zsh
  read, paths in a git dir at run time, and Nix files and options outside
  the prism source tree. prism Go source does not use them.
-->

Issues: #3326. Related: #3325 (a nested `.git`), #3322 (prism host git),
#3337 (a detached bwrap pin), #3343 (a staged submodule with
`ignore = none`). Threat model: `docs/invariants/threat-model.md`.
Code: `modules/programs/git.nix` and
`modules/programs/git-builtin-external-diff.sh` at the repo root.

## The rule

Each git process of the user on the host gets a list of config values in
its environment, through `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_<n>`, and
`GIT_CONFIG_VALUE_<n>`. git reads this config after each config file, the
repo config included. So a repo config cannot change a key in the list.

A sandboxed session can write config that git on the host reads:

- a `.git` file or dir in a subdir of its worktree (#3325)
- a submodule that it stages in its own index, with its own git dir
- `.bare/config`, after a host rename detaches a bwrap pin (#3337)

On the host, git runs in a worktree for the shell prompt, for gitsigns in
nvim, and for each command that the owner types.

## The list

Each row was tested on git 2.56. A payload in a temp dir recorded each run.
The PR of #3326 holds the full table of tests.

| Key | Value | Effect |
|---|---|---|
| `core.fsmonitor` | `false` | No fsmonitor hook. |
| `core.hooksPath` | `/dev/null` | No hook, the hooks of `$GIT_DIR/hooks` included. |
| `core.pager` | `cat` | No pager. `GIT_PAGER` (below) also stops `pager.<command>`. |
| `core.editor` | `:` | The editor does nothing. git does not use `VISUAL`, `EDITOR`, or `vi`. |
| `sequence.editor` | `:` | Same for `git rebase -i`. |
| `core.askPass` | empty | No askpass program. git does not use `SSH_ASKPASS`. git asks on the terminal. |
| `core.alternateRefsCommand` | empty | No command. A fetch reads the alternate refs itself. |
| `gpg.ssh.defaultKeyCommand` | empty | No command. `user.signingKey` is pinned, so git does not need it. |
| `protocol.ext.allow` | `never` | git refuses an `ext::` URL. |
| `diff.external` | the wrapper | See "diff.external" below. |
| `diff.trustExitCode` | `true` | git reads the exit code of the wrapper: 0 for no difference, 1 for a difference. |
| `core.attributesFile` | `/dev/null` | A repo config cannot name an attributes file. |
| `core.sshCommand` | `ssh` | The git default. |
| `credential.helper` | empty | Clears the list of helpers. git reads it last, so the planted helpers are also cleared, `credential.<url>.helper` included. |
| `gpg.format` | `ssh` | The value of `git.nix`. |
| `gpg.ssh.program` | `ssh-keygen` | The git default. |
| `user.signingKey` | the public key in `git.nix` | The value of `git.nix`. |
| `gpg.ssh.allowedSignersFile` | the file in `git.nix` | The value of `git.nix`. |
| `gpg.program`, `gpg.openpgp.program` | `gpg` | The git default. These run to verify a signature, for example with `log.showSignature`. |
| `gpg.x509.program` | `gpgsm` | The git default. |
| `diff.ignoreSubmodules` | `all` | `git status` and `git diff` do not run git in a submodule, unless `.gitmodules` sets `ignore` (#3343). |
| `status.submoduleSummary` | `false` | No summary of a submodule. |
| `submodule.recurse` | `false` | No recursion. |
| `fetch.recurseSubmodules` | `false` | No fetch in a submodule. |
| `push.recurseSubmodules` | `no` | No push in a submodule. |

Environment variables:

| Variable | Value | Effect |
|---|---|---|
| `GIT_ATTR_SOURCE` | the empty tree | git reads no `.gitattributes` file. A planted diff or filter driver does not run. |
| `GIT_NO_LAZY_FETCH` | `1` | A partial-clone config cannot make `git status`, `git diff`, or `git blame` fetch, and run `remote.<name>.uploadpack`. |
| `GIT_PAGER` | `cat` | `GIT_PAGER` has priority over `pager.<command>` and `core.pager`. |
| `GIT_PROXY_COMMAND` | empty | git uses the first `core.gitProxy` that matches, so a later config value cannot replace a planted one. A set `GIT_PROXY_COMMAND` makes git skip the config. |

The global config sets `safe.bareRepository = explicit`. git then uses a
bare repo only when `--git-dir` or `GIT_DIR` names it. git reads this key
only from protected config, so a repo config cannot change it.

## diff.external

No value turns `diff.external` off. git runs each value, and `git diff`
stops with `external diff died` when the value is empty. So the list pins
it to `git-builtin-external-diff`. The wrapper copies the two files to a
temp dir, and runs `git diff --no-index --no-ext-diff` there. It prints the
header and the metadata lines from the arguments that git gives it.

The output of `git diff`, `git diff --cached`, `git diff HEAD`,
`git show --ext-diff`, and `git log -p --ext-diff` is the same text as the
builtin diff. The test covers a change, an added file, a deleted file, a
binary file, a mode change, a rename, and an unmerged path. Only one
difference is known: an entry with no content change (a rename or a mode
change) has no colour in its header.

## Where the overrides reach

| Process | How | Overrides |
|---|---|---|
| zsh, login shell (Linux, Darwin) | The `.zprofile` of home-manager sources `hm-session-vars.sh`. | All |
| zsh, non-login shell | The `.zshenv` of home-manager sources the same file. | All |
| A tmux pane | `tmux.nix` removes `__HM_SESS_VARS_SOURCED` from the tmux environment, so the zsh of each new pane sources the file again. | All |
| The tmux server, `run-shell`, and hooks | The environment of the process that started the server. | All from a shell. The service list from `prism-restore` (Linux). |
| nvim and gitsigns | prism types `nvim` into the zsh of the pane. nvim passes its environment to each job. | All |
| The p10k prompt | The `vcs_info` fallback runs git in the zsh of the pane. gitstatusd is libgit2, and it runs no command for a planted config. | All |
| systemd user units (Linux) | `systemd.user.sessionVariables` writes `environment.d`. | The service list |
| launchd agents and apps (Darwin) | The agent `git-env-overrides` runs `launchctl setenv` at each login. | The service list |
| A prism session in `host` mode | The tmux pane. | All |
| A bwrap or sandbox-exec session | bwrap starts with `--clearenv`. sandbox-exec keeps an allow-list. | None. The session has its own gitconfig. |

`environment.d` cannot hold an empty value, and git stops with "missing
config value" when one `GIT_CONFIG_VALUE_<n>` is absent. So the service
list holds only the entries that have a value. It does not have
`core.askPass`, `core.alternateRefsCommand`, `gpg.ssh.defaultKeyCommand`,
`credential.helper`, and `GIT_PROXY_COMMAND`. The launchd agent uses the
same list.

A shell or a tmux server that started before the switch keeps its old
environment. A new pane and a new shell get the overrides. launchd gets
them at the next login.

## A tool that sets its own config in the environment

- A tool that sets `GIT_CONFIG_COUNT` to its own count replaces the count.
  git then reads only the entries of the tool, and the overrides do not
  apply to that git process. No tool in this repo does this.
- A tool that adds its entries at the index `GIT_CONFIG_COUNT`, and adds
  one to the count, keeps all the overrides.
- A tool that uses `git -c` or `GIT_CONFIG_PARAMETERS` keeps all the
  overrides. git reads `GIT_CONFIG_PARAMETERS` after `GIT_CONFIG_COUNT`, so
  the value of the tool wins for the same key. prism host git (#3322) does
  this. Its values agree with the list.
- git removes `GIT_CONFIG_COUNT` for the upload-pack and receive-pack of a
  remote that is a local path. That side runs in the remote repo.

## Effects for the owner

- `git commit` with no `-m` stops with an empty message. `git rebase -i`
  keeps the todo list. To use an editor one time, set `GIT_EDITOR`, which
  has priority over `core.editor`.
- git uses no pager. To use one, set `GIT_PAGER` for the command.
- `GIT_ATTR_SOURCE` stops `git diff --no-index`, `git hash-object`, and
  `git check-attr` outside a repo ("cannot use --attr-source or
  GIT_ATTR_SOURCE without repo"). In a sha256 repo, each command that reads
  attributes stops ("bad --attr-source or GIT_ATTR_SOURCE"). Remove the
  variable for the command: `env -u GIT_ATTR_SOURCE git ...`.
- A partial clone cannot fetch a missing object.
- The security tests of prism host git check first that a plain git runs
  the planted payload. In a shell with the overrides, these controls fail:
  10 tests in `internal/git` and `internal/review`. CI and the agent
  sandboxes do not have the overrides.

## Known limits

- **A nested git dir.** A `cd` into a subdir whose git dir uses a key that
  is not on the list, for example a driver in `info/attributes`. The owner
  accepted this on #3325.
- **`$GIT_DIR/info/attributes`.** `GIT_ATTR_SOURCE` does not cover it. A
  driver that the same git dir defines runs. The session can write it in a
  nested git dir (#3325), in a staged submodule (#3343), or in
  `.bare/info` after a detached bwrap pin (#3337).
- **A staged submodule with `ignore = none` in `.gitmodules`.** A typed
  `git status` at the worktree root runs git in the submodule. The prompt
  and gitsigns give `--ignore-submodules`, so they do not. Fix: #3343.
- **Keys with a name that the repo chooses.** For example
  `remote.<name>.uploadpack`, `remote.<name>.url`, and
  `url.<base>.insteadOf`. They run only for a fetch, a pull, or a push. A
  session can write them only in `.bare/config` after a detached bwrap pin
  (#3337).
- **The service list.** It does not have the five entries with an empty
  value. These entries act only on a credential prompt or a credential
  helper, a fetch from a repo with alternates, a signature with no
  signing key, and a `git://` fetch. The host git of prism adds its own
  config (#3322), and #3337 tracks the keys that it does not pin.
- **A key that a later git version adds.** The list is for git 2.56. Do
  the tests again when git changes.
- **A language server in the host nvim.** It runs project code by design.
  The threat model puts it out of scope.
