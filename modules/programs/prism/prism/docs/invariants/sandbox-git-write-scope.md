# Invariant: the write scope of a sandbox-exec session in its repo

<!-- doclint-ignore: .bare/config, .bare/gc.pid, .bare/hooks, .bare/logs, .bare/logs/refs/remotes, .bare/objects, .bare/packed-refs, .bare/refs/remotes, .bare/refs/tags, .bare/worktrees, logs/HEAD, refs/remotes/origin/main, FETCH_HEAD, ORIG_HEAD, core.hooksPath -->
<!-- doclint-ignore: .agents/skills, .pi/extensions, .pi/mcp.json, .pi/settings.json -->
<!--
  These names are paths in the git common dir (.bare) of a prism repo, git
  files and config keys, and project resources that pi loads from a
  worktree. They exist in a repo at run time, not in this source tree.
-->

Issues: #3293, #3216. Follow-ups: #3317 (the object store), #3315 (bwrap).
Code: `internal/container/git_write_scope.go`, sections 6, 6e, 17b, 21b,
and 21i of `generateProfile` in `internal/container/sandbox_exec.go`, and
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
| Its worktree | Each session that can write its worktree. | The work of the session. A review agent and an investigator get a read-only worktree (section 21d). |
| `.bare/objects/` | Each session. | A commit, a fetch, and a rebase write objects and packs. |
| `.bare/refs/remotes/`, `.bare/logs/refs/remotes/` | Each session. | A fetch writes each remote-tracking ref and its reflog. A push writes the ref of the pushed branch. |
| `.bare/refs/tags/` | Each session. | A fetch writes the tags that it follows. |
| `.bare/packed-refs`, with `.lock` and `.new` | Each session. | A ref delete (`git fetch --prune`) rewrites the file. Each update of a packed ref takes the lock. |
| `.bare/worktrees/<id>/` of its worktree | Each session that can write its worktree. | `HEAD`, `index`, `logs/HEAD`, `FETCH_HEAD`, `ORIG_HEAD`, and `rebase-merge/`. |
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
| `.bare/gc.pid`, `.bare/info/` | `git gc` writes them, and it also rewrites the reflogs of every worktree. A session does not run `git gc`. |

## How prism decides the scope

`resolveGitWriteScope` builds the scope from three values. The bwrap
isolator can use the same function (#3315).

- The project root and the worktree come from prism on the host. A session
  cannot change them.
- The branch is the path of the worktree below the project root. prism names
  a worktree after its branch when it makes it (`git.CreateWorktree`). The
  function does not read `HEAD`, because the session can write it. If the
  path is not a branch name, the session gets no branch ref.
- The worktree git dir comes from the `.git` file in the worktree, which the
  session can write. The function accepts it only when it is a direct child
  of `.bare/worktrees`, it is a directory and not a symlink, and its `gitdir`
  file is a regular file that names the `.git` file of the worktree. The
  `gitdir` file of another worktree is in a git dir that the session cannot
  write. So a session cannot point its `.git` file at another git dir, and
  get write access to it at its next start. If the check fails, the session
  gets no git dir, and the agent-run log gives the reason.

## What prism does on the host before the session starts

- `prepareGitWriteDirs` makes the parent dirs of the refs and reflogs that
  the session writes: `.bare/refs/remotes`, `.bare/refs/tags`,
  `.bare/logs/refs/remotes`, and the dirs of the branch ref and its reflog.
  The session cannot make a dir in `.bare`. If the reflog dir does not exist,
  git cannot write a ref: a fetch exits 255, and a commit exits 128. A fresh
  `git clone --bare` has no `.bare/logs`.
- `writeGitconfigToDir` writes the upstream of the branch into the
  per-session `GIT_CONFIG_GLOBAL` file, with `origin` as the remote. The
  generated gitconfig sets `push.autoSetupRemote`, and without an upstream
  the first `git push` writes it into `.bare/config`. That write fails in the
  session. The push still exits 0, but it prints `error: unable to write
  upstream branch configuration`. prism writes the section only when
  `.bare/config` names no upstream for the branch, because git refuses to
  push a branch with an upstream in two files.

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
- Each deny follows every allow, because SBPL takes the later rule.
  `TestGenerateProfile_GitNodeDenyFollowsEveryAllow` pins the order of
  section 21i.

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
- **`git gc`.** A session cannot write `.bare/gc.pid`, so `git gc` and the
  auto maintenance of git stop at their start in a session. Loose objects
  then collect until a git command on the host runs `git gc`.
- **bwrap.** bwrap binds the whole `.bare` read-write. A bwrap session can
  write `.bare/config` and `.bare/hooks`. Follow-up: #3315.
