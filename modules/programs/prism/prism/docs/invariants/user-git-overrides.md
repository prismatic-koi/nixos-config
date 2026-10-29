# Invariant: the git overrides of the user session

<!-- doclint-ignore: core.alternateRefsCommand, core.askPass, core.attributesFile, core.gitProxy, gpg.openpgp.program, gpg.ssh.defaultKeyCommand, gpg.x509.program, log.showSignature, status.submoduleSummary, interactive.diffFilter -->
<!-- doclint-ignore: diff.tool, merge.tool, man.viewer, web.browser, help.format -->
<!-- doclint-ignore: EDITOR, VISUAL, SSH_ASKPASS, GIT_ATTR_SOURCE, GIT_EDITOR, GIT_NO_LAZY_FETCH, GIT_PROXY_COMMAND, vcs_info -->
<!-- doclint-ignore: .bare/config, .bare/info, .bare/commondir, info/attributes, git.nix, tmux.nix, modules/programs/git.nix, systemd.user.sessionVariables -->
<!-- doclint-ignore: attr.tree, AGENTS.md, gitsigns.nix -->
<!--
  These names are git config keys, environment variables that git and zsh
  read, paths in a git dir at run time, and Nix files and options outside
  the prism source tree. prism Go source does not use them.
-->

Issues: #3326. Related: #3325 (a nested `.git`), #3322 (prism host git),
#3337 (prism host git on the bwrap detach route), #3346 (user git on the
bwrap detach route), #3343 (a staged submodule with `ignore = none`). Threat model:
`docs/invariants/threat-model.md`. Code: `modules/programs/git.nix` at the
repo root.

## The rule

Each git process of the user on the host gets a list of config values in
its environment, through `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_<n>`, and
`GIT_CONFIG_VALUE_<n>`. git reads this config after each config file, the
repo config included. So a repo config cannot change a key in the list.

A sandboxed session can write config that git on the host reads:

- a `.git` file or dir in a subdir of its worktree (git reads it after a
  `cd` into the subdir, #3325. The git plugins of nvim do not read it,
  #3347)
- a submodule that it stages in its own index, with its own git dir
- `.bare/config`, after a host rename detaches a bwrap pin (#3346)
- `.bare/commondir` at the project root, with no detach; a plain git at the
  project root then reads config and `info/attributes` from a dir that the
  worker controls (#3348)

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
| `interactive.diffFilter` | `cat` | No filter program on `git add -p`, `git restore -p`, `git checkout -p`, or `git reset -p`. An empty value breaks `add -p`; `cat` passes the diff through. |
| `core.attributesFile` | `/dev/null` | A repo config cannot name a global attributes file that selects a driver. |
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
| `hook.<event>.enabled` (every githooks(5) event) | `false` | No config-defined hook. git 2.56 runs `hook.<name>.command` even with `core.hooksPath = /dev/null`; this stops it per event. |

Environment variables:

| Variable | Value | Effect |
|---|---|---|
| `GIT_NO_LAZY_FETCH` | `1` | A partial-clone config cannot make `git status`, `git diff`, or `git blame` fetch, and run `remote.<name>.uploadpack`. |
| `GIT_PAGER` | `cat` | `GIT_PAGER` has priority over `pager.<command>` and `core.pager`. |
| `GIT_PROXY_COMMAND` | empty | git uses the first `core.gitProxy` that matches, so a later config value cannot replace a planted one. A set `GIT_PROXY_COMMAND` makes git skip the config. |

The global config sets `safe.bareRepository = explicit`. git then uses a
bare repo only when `--git-dir` or `GIT_DIR` names it. git reads this key
only from protected config, so a repo config cannot change it.

## Two keys that the list does not have

### `GIT_ATTR_SOURCE`

`GIT_ATTR_SOURCE` set to the empty tree makes git read no `.gitattributes`
file, so a planted diff or filter driver does not run. The list does not
use it, for two reasons found on git 2.56:

- With `GIT_ATTR_SOURCE` set to any value (the empty tree, `HEAD`, or
  `attr.tree`), `git apply`, `git apply --check`, and `git apply -R` stop
  with a segmentation fault. That breaks `git restore -p`,
  `git checkout -p`, and the patch-file WIP pattern in the repo `AGENTS.md`.
  `git apply --cached`, `git add -p`, `git am`, rebase, and merge still
  work.
- It does not cover `$GIT_DIR/info/attributes`. A driver that
  `info/attributes` selects runs even with `GIT_ATTR_SOURCE` set.

So a planted `.gitattributes` or `info/attributes` driver is a known limit,
below.

### `diff.external`

git has no config value that means "use the builtin diff". It runs each
value, and an empty value makes each `git diff` that shows a patch stop with
`external diff died`. So the list does not pin `diff.external`. A planted
`diff.external` on a typed `git diff` (and on any `--ext-diff` variant, for
example `git show --ext-diff`) is a known limit, below. Bare `git show` and
`git log -p` do not trigger it. The shell prompt and gitsigns pass
`--no-ext-diff`, so they do not run it.

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
- A partial clone cannot fetch a missing object.
- The security tests of prism host git check first that a plain git runs
  the planted payload. A host shell carries the overrides in
  `GIT_CONFIG_COUNT`, which would stop the payload and fail that control
  step. The three isolation helpers set `GIT_CONFIG_COUNT=0`, so git reads
  none of the inherited entries and `go test ./...` passes in a host shell.
  A new test that checks a planted payload against a plain git must do the
  same. See the "Tests" section of `host-git-commands.md`.

## Known limits

Each limit needs a planted config that defines the driver, the helper, or
the command. At the worktree root, a session cannot write that config:
`sandbox-exec` denies `.bare/config`, and #3346 tracks the bwrap route that
a host rename opens (the real fix is bwrap `.bare` write-protection, the
#3334 / #3337 family — `sandbox-git-write-scope.md`). A plain git at the
project root reads config from a planted `.bare/commondir`, which a bwrap
worker can write with no detach — a known OPEN in-scope route tracked by
#3348 (real fix in the #3327 / #3334 family). A session can also plant the
config in a git dir that it fully controls — a nested `.git` (#3325) or a
staged submodule (#3343).

- **A planted `.gitattributes` or `info/attributes` driver.** The list
  does not set `GIT_ATTR_SOURCE` (above), so a `diff=<name>` or
  `filter=<name>` attribute selects a driver that a planted config defines,
  and the driver runs on `git status` or `git diff`. Tracked by #3346 for
  the config write-protection.
- **A planted `diff.external`.** It runs on a typed `git diff` and on any
  `--ext-diff` variant (for example `git show --ext-diff`). Bare `git show`
  and `git log -p` do not trigger it. The prompt and gitsigns pass
  `--no-ext-diff`. Tracked by #3346.
- **The gitsigns current-line blame.** It runs `git blame --contents` on
  the working buffer. `--no-textconv` (set in `gitsigns.nix`) stops a diff
  textconv driver. A planted clean filter still runs when git normalises
  the buffer, and `git blame` has no flag for it. At the worktree root the
  filter config needs a planted `.bare/config` (#3346). gitsigns attaches
  only to the repo of the dir where nvim starts, so it does not reach a
  nested or submodule git dir (#3347). The gitsigns signs path runs its own
  diff and invokes no driver (verified on git 2.56).
- **A planted tool selector.** A typed `git help` runs the program of
  `man.viewer` and `man.<tool>.cmd`, or of `help.format=web`, `web.browser`,
  and `browser.<tool>.cmd`. A typed `git difftool` runs `diff.tool` and
  `difftool.<tool>.cmd`. A typed `git mergetool` runs `merge.tool` and
  `mergetool.<tool>.cmd`. The tool name is repo-chosen, so the list cannot
  pin the `<tool>.cmd` key. Tracked by #3346.
- **A staged submodule with `ignore = none` in `.gitmodules`.** A typed
  `git status` at the worktree root runs git in the submodule, and a driver
  in the submodule git dir runs. The prompt, gitsigns, the Telescope git
  pickers, and nvim-tree give `--ignore-submodules`, so they do not. The
  `:Git` status of fugitive does not give it. Fix: #3343.
- **A nested git dir, by `cd`.** A `cd` into a subdir whose git dir uses a
  key that is not on the list. The owner accepted this on #3325. The git
  plugins of nvim do not reach a nested git dir with no `cd` (#3347).
- **Keys with a name that the repo chooses.** For example
  `remote.<name>.uploadpack`, `remote.<name>.url`, and
  `url.<base>.insteadOf` run for a fetch, a pull, or a push; the tool
  selectors above run for a typed `git help`, `git difftool`, or
  `git mergetool`. The list cannot name them.
- **The service list.** It does not have the five entries with an empty
  value. These entries act only on a credential prompt or a credential
  helper, a fetch from a repo with alternates, a signature with no signing
  key, and a `git://` fetch. The host git of prism adds its own config
  (#3322), and #3337 tracks the keys that it does not pin.
- **A key that a later git version adds.** The list is for git 2.56. Do
  the tests again when git changes.
- **A language server in the host nvim.** It runs project code by design.
  The threat model puts it out of scope.

## The nvim panes (#3347)

A worker fully controls a nested or submodule git dir that it writes. The
overrides of this doc cannot stop a driver that the config of that git dir
defines. So the control for the nvim panes is in the nvim config: the git
plugins of the prism nvim run git only in the repo of the dir where nvim
starts. See the nvim-pane rule in `host-git-commands.md`.
