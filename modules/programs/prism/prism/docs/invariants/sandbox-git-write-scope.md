# Invariant: the write scope of a sandbox-exec session in its repo

<!-- doclint-ignore: .bare/config, .bare/gc.pid, .bare/hooks, .bare/info, .bare/logs, .bare/logs/refs/remotes, .bare/objects, .bare/packed-refs, .bare/refs/remotes, .bare/refs/tags, .bare/worktrees, .bare/commondir, logs/HEAD, refs/remotes/origin/main, FETCH_HEAD, ORIG_HEAD, core.hooksPath -->
<!-- doclint-ignore: .agents/skills, .pi/extensions, .pi/mcp.json, .pi/settings.json -->
<!--
  These names are paths in the git common dir (.bare) of a prism repo, git
  files and config keys, and project resources that pi loads from a
  worktree. They exist in a repo at run time, not in this source tree.
-->

Issues: #3293, #3216. Follow-ups: #3317 (the object store), #3315 (bwrap),
#3322 (host-side git in a worker worktree), #3325 (a nested `.git`).
Code: `internal/container/git_write_scope.go`, sections 6, 6e, 17b, 21b,
21i, 21j, and 21k of `generateProfile` in `internal/container/sandbox_exec.go`, and
the git step of `sandboxExecIsolator.Prepare` in
`internal/container/lifecycle_dispatch.go`.

## The rule

A `sandbox-exec` session cannot create, change, or remove a file in another
worktree of its repo. It also cannot write a file at the project root, and
it cannot write a file in `.bare` that git or pi loads as code or as
configuration.

The project root is the directory that holds `.bare` and the worktrees
(`Config.BareRoot`). The common dir is `.bare` in it.

A session can write these paths in its repo, and no other path:

| Path | Session | Writer and reason |
|---|---|---|
| Its worktree, except the `.git` file in a bare+worktree layout | Each session that can write its worktree. | The work of the session. A review agent and an investigator get a read-only worktree (section 21d). |
| `.bare/objects/` | Each session. | A commit, a fetch, and a rebase write objects and packs. |
| `.bare/refs/remotes/`, `.bare/logs/refs/remotes/` | Each session. | A fetch writes each remote-tracking ref and its reflog. A push writes the ref of the pushed branch. |
| `.bare/refs/tags/` | Each session. | A fetch writes the tags that it follows. |
| `.bare/packed-refs`, with `.lock` and `.new` | Each session. | A ref delete (`git fetch --prune`) rewrites the file. Each update of a packed ref takes the lock. |
| `.bare/worktrees/<id>/` of its worktree, except `commondir`, `gitdir`, and `config.worktree` | Each session that can write its worktree. | `HEAD`, `index`, `logs/HEAD`, `FETCH_HEAD`, `ORIG_HEAD`, and `rebase-merge/`. |
| `FETCH_HEAD` in `.bare/worktrees/<id>/` of its worktree | A review agent and an investigator. | A fetch writes it. `git status`, `git diff`, and `git log` work when git cannot write the index. |
| The branch ref and the reflog of its worktree, each with `.lock` | Each session that can write its worktree. | A commit, a rebase, and a pull move the branch. |
| The stash ref and its reflog, each with `.lock` | The coordinator only. | The coordinator is the only prism session that can use `git stash`. |

A session can read the whole project root. pi and other tools read project
files in each ancestor of the worktree, for example `.agents/skills`. git
reads the git dirs of the other worktrees for `git worktree list`.

## Why the other paths are read-only

| Path | Effect of a write |
|---|---|
| Another worktree, `main` included | In a trusted repo, the coordinator loads `.pi/extensions`, `.pi/settings.json`, and `.pi/mcp.json` from `main` at its next start (#3216). A change in another file reaches the next commit of that worktree. |
| `.agents/skills` and other files at the project root | pi loads project resources from each ancestor of the worktree. |
| `.bare/config` | A value such as `core.fsmonitor` or `core.hooksPath` runs a command in each git command of each worktree, and in prism on the host. |
| `.bare/hooks/` | git runs a hook in each worktree. prism on the host runs `git worktree add` for each spawn, and that command runs `post-checkout`. |
| `.bare/worktrees/<id>/` of another worktree | `HEAD` and `index` of another worktree decide what its next commit holds. A review agent runs in the worktree of its worker, and an investigator runs in the worktree of the session that started it, often `main`. So these two get only `FETCH_HEAD` in the git dir of their worktree. |
| The branch ref of another worktree, and the stash ref | The next pull, reset, or stash pop in that worktree writes the files of the commit that the ref names. |
| `commondir`, `gitdir`, and `config.worktree` in `.bare/worktrees/<id>/` of its own worktree | Each git command in the worktree reads them, prism on the host included. For example, the pre-flight of `prism review` runs `git fetch` in the worktree. A `commondir` that names a dir of the session gives git the config and the hooks of that dir. git does not write these files for a commit, a fetch, a push, a rebase, a pull, `git status`, or `git gc`. |
| The `.git` file of its own worktree, in a bare+worktree layout | The file names the git dir, and each git command in the worktree follows it. prism starts nvim and a shell on the host in each worktree, and gitsigns and the shell prompt run git there when the user looks at the session. A `.git` file that names a git dir of the session runs its config, for example `core.fsmonitor`, on the host. git writes this file only for `git worktree repair`. In a project with no worktrees, `.git` is the git dir, and the session writes it. |
| `.bare/gc.pid`, `.bare/info/` | `git gc` writes them, and it also rewrites the reflogs of every worktree. A session does not run `git gc`. |

## How prism decides the scope

`resolveGitWriteScope` builds the scope from three values. The bwrap
isolator uses the same function to validate its worktree git dir before it
binds it read-write (#3315).

- The project root and the worktree come from prism on the host. A session
  cannot change them.
- The branch is the path of the worktree below the project root. prism names
  a worktree after its branch when it makes it (`git.CreateWorktree`). The
  function does not read `HEAD`, because the session can write it. If the
  path is not a branch name, the session gets no branch ref.
- The worktree git dir comes from the `.git` file in the worktree, which the
  session can write. The function accepts it only when all of these are
  true:
  - The `.git` file of the worktree is a regular file, not a symlink or a
    directory.
  - The git dir is a direct child of `.bare/worktrees`, and it is a
    directory and not a symlink.
  - Its `gitdir` file is a regular file that names the `.git` file of the
    worktree. The comparison resolves the symlinks in the worktree path,
    but not the `.git` part. Otherwise a `.git` symlink to the `.git` file
    of `main` passes the check.

  The `gitdir` file of another worktree is in a git dir that the session
  cannot write. So a session cannot point its `.git` file at another git
  dir, and get write access to it at its next start. If the check fails,
  the session gets no git dir. `prism agent-run` writes the reason to its
  stderr, in the tmux pane of the session, before pi starts. The reason is
  not in `agent-run.log`.
- A project root behind a symlink gets no git dir. git writes the `.git`
  file with the resolved path, so the git dir is not below
  `<project root>/.bare/worktrees` as prism names it. The sandbox matches
  resolved paths, so the worktree grant has the same limit.

## What prism does on the host before the session starts

- `prepareGitWriteDirs` makes the parent dirs of the refs and reflogs that
  the session writes: `.bare/refs/remotes`, `.bare/refs/tags`,
  `.bare/logs/refs/remotes`, and the dirs of the branch ref and its reflog.
  The session cannot make a dir in `.bare`. If the reflog dir does not exist,
  git cannot write a ref: a fetch exits 255, and a commit exits 128. A fresh
  `git clone --bare` has no `.bare/logs`.
- `ensureGitUpstream` writes the upstream of the branch into `.bare/config`,
  with `origin` as the remote, when `.bare/config` names none. It does not
  do this for a review agent or an investigator. The generated gitconfig
  sets `push.autoSetupRemote`, and without an upstream the first `git push`
  writes it into `.bare/config`. That write fails in the session. The push
  still exits 0, but it prints `error: unable to write upstream branch
  configuration`.
- The upstream must be in `.bare/config` and in no other file. Host-side
  git in the worktree reads `.bare/config`, not the per-session
  `GIT_CONFIG_GLOBAL` file. For example, `prism review --rebase` runs
  `git push --force-with-lease` on the host. With no upstream in
  `.bare/config`, that push writes one there. If the per-session file also
  names an upstream, git then refuses each later push in the session
  ("multiple upstream branches", exit 128).
- Before the first push, `git status` reports that the upstream is gone,
  and `git pull` finds no remote branch. Both change after the first push.

## Section order in the profile

- Section 6 grants the worktree, and section 6e grants the paths above.
- Section 17b allows a Unix-socket connect in the worktree, not in the
  project root. A socket in another worktree belongs to another session.
- Section 21b denies a create and an unlink of the worktree node and each
  ancestor of it, the project root included.
- Section 21i denies a create, an unlink, and a mode, flags, or ACL change
  of each dir node that section 6e grants as a subpath. Without it, a
  session can move `.bare/objects` away and put a symlink to a dir of its
  choice at the path.
- Section 21j denies each write of `commondir`, `gitdir`, and
  `config.worktree` in the worktree git dir, which section 6e grants as a
  subpath.
- Section 21k denies each write of the `.git` file of the worktree: a
  change, a rename, a removal, a replace, and a mode change. Section 6
  grants the worktree. It is emitted only in a bare+worktree layout.
- Each deny follows every allow, because SBPL takes the later rule.
  `TestGenerateProfile_GitNodeDenyFollowsEveryAllow`,
  `TestGenerateProfile_GitRedirectFileDenyFollowsEveryAllow`, and
  `TestGenerateProfile_WorktreeGitFileDenyFollowsEveryAllow` pin the order
  of sections 21i, 21j, and 21k.

## Known limits

- **The object store (both isolators).** git needs a write of
  `.bare/objects` for a commit and a fetch. A session can replace an object
  that another worktree reads. The next checkout in that worktree, for
  example the `git pull` of the coordinator after a merge, then writes the
  replaced content. git does not check the hash of an object when it reads
  it. A path rule cannot stop this, because a fetch makes new packs with any
  name. Follow-up: #3317.
- **`packed-refs` (both isolators).** A session can rewrite a ref that has
  no loose file. After `git gc` packs the refs, that includes the branch of
  `main` until the next update of it. The object store gives a session the
  same result, so the grant adds no new effect.
- **Remote-tracking refs and tags (both isolators).** A session can change
  `refs/remotes/origin/main`. A fetch writes the correct value again. A
  session that runs `git rebase origin/main` with no fetch first uses the
  changed value, in its own worktree.
- **Other branches.** A session can move only the branch that its worktree
  path names. `git checkout -b` and `git branch <name>` fail, and so does a
  commit on another branch in the worktree. A worker stays on the branch of
  its worktree.
- **`git gc`.** A session cannot write `.bare/gc.pid`, so `git gc` and the
  auto maintenance of git stop at their start in a session. Loose objects
  then collect until a git command on the host runs `git gc`.
- **A nested `.git` in the worktree (both isolators).** Section 21k
  covers the `.git` file at the top of the worktree only. A session can
  still put a `.git` file or dir in a subdir of its worktree. git that runs
  in that subdir stops there, and reads the config of the git dir that it
  names. The nvim and the shell that prism starts on the host run git in
  the dir of the open file or of the shell. A deny of `.git` at any depth
  does not close this: a rename of a dir that already holds `.git` into
  the worktree makes no new path named `.git`. Follow-up: #3325.
- **Host-side git in a worktree (both isolators).** Neither isolator can
  write the `.git` file or the redirect files of its git dir: sandbox-exec
  denies them (sections 21j and 21k); bwrap pins them read-only over the
  read-write binds (#3315). But a host git command run with its working
  directory in a worker worktree — the `prism review` preflight `git
  fetch`/`rebase`, and the nvim and shell that prism starts there — is not
  itself hardened (pinned `GIT_DIR`/`GIT_COMMON_DIR`, hooks and fsmonitor
  off). The read-only pins stop a worker rewriting the pointer files, which
  closes the sandboxed route; the host-side hardening is the defence that
  does not depend on those pins holding. Follow-up: #3322.

## bwrap parity (#3315)

bwrap reaches the same write scope as sandbox-exec by a different mechanism:
it binds `.bare` and the worktree read-write, then layers read-only binds
over the paths the table above keeps read-only. A read-only bind blocks a
write with `EROFS`, and the mount point cannot be renamed, removed, or
replaced (`EBUSY`).

- `.bare/config`, `.bare/hooks`, `.bare/info`, and the whole
  `.bare/worktrees` tree are read-only; the session's own worktree git dir
  is re-exposed read-write on top.
- `.bare/commondir` is pinned to the benign `.` (the git dir itself), so a
  host `git --git-dir .bare …` command cannot be redirected.
- `commondir`, `gitdir`, and `config.worktree` in the own worktree git dir,
  and the worktree `.git` file, are pinned read-only over the read-write
  binds — the bwrap equivalent of sections 21j and 21k.
- On the host, the bwrap `Prepare` runs `prepareGitWriteDirs` and
  `ensureGitUpstream`, and binds only the worktree git dir that
  `resolveGitWriteScope` accepts, the same as sandbox-exec.

The object store and `packed-refs` stay writable under bwrap too, so the
"both isolators" limits above apply. See the `.bare` write scope (bwrap)
section of `session-lifecycle.md` for the bind ordering.
