# Invariant: the write scope of a sandbox-exec session in its repo

<!-- doclint-ignore: .bare/config, .bare/gc.pid, .bare/hooks, .bare/info, .bare/logs, .bare/logs/refs/remotes, .bare/objects, .bare/packed-refs, .bare/refs/remotes, .bare/refs/tags, .bare/worktrees, .bare/commondir, logs/HEAD, refs/remotes/origin/main, FETCH_HEAD, ORIG_HEAD, core.hooksPath -->
<!-- doclint-ignore: .agents/skills, .pi/extensions, .pi/mcp.json, .pi/settings.json -->
<!-- doclint-ignore: .bare/refs, .bare/HEAD, refs/heads/main, refs/stash, detach_mounts, branch.autoSetupMerge -->
<!-- doclint-ignore: .bare/packed-refs.lock, refs/heads, .bare/refs/heads/main -->
<!--
  The extra .bare/* and refs/* names are git paths in the common dir at run
  time, not files in this source tree. .bare/packed-refs.lock is the lock
  file git creates in the .bare root on every ref update.
-->
<!--
  These names are paths in the git common dir (.bare) of a prism repo, git
  files and config keys, and project resources that pi loads from a
  worktree. They exist in a repo at run time, not in this source tree.
-->

Issues: #3293, #3216. Follow-ups: #3317 (the object store), #3315 (bwrap
code-execution paths), #3334 (the permanent bwrap refs vector — a per-session
common dir or reftable), #3337 (the config-detach host-git route — pin the
keys in `hostGitConfig`), #3327 (the achievable bwrap narrowing —
`FETCH_HEAD` only for a read-only session), #3326 (user git env hardening),
#3325 (a nested `.git`). Host-side git: #3322, in
`docs/invariants/host-git-commands.md`. #3327 found that bwrap cannot bind
`.bare` read-only to match the sandbox-exec write scope — see the "bwrap"
section.
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
  `GIT_CONFIG_GLOBAL` file. For example, a bare `git push` from a host pane
  with no upstream in `.bare/config` writes one there. If the per-session
  file also names an upstream, git then refuses each later push in the
  session ("multiple upstream branches", exit 128). The `prism review
  --rebase` push names an explicit `refs/heads/<branch>` refspec, so it
  writes no upstream.
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
- **Host-side git in a worktree (both isolators).** sandbox-exec denies the
  `.git` file and the redirect files of its git dir (sections 21j and 21k);
  bwrap pins them read-only over the read-write binds (#3315). And #3322
  (#3324) hardens the host side: every host git command prism runs sets
  `GIT_DIR` and `GIT_COMMON_DIR` itself and runs with hooks and fsmonitor
  off, and `OpenBare` refuses a repo that has a `.bare/commondir` file, so a
  redirect in a worker worktree does not reach a host command. See
  `docs/invariants/host-git-commands.md`.

## bwrap (#3315)

bwrap closes the direct-write code-execution paths (`.bare/config`,
`.bare/hooks`, `.bare/info`, the other worktrees) and the in-sandbox redirect
paths (the own worktree's pointer files and `<worktree>/.git`), by a
different mechanism than the SBPL profile: it binds `.bare` and the worktree
read-write, then layers read-only binds over those paths. A read-only bind
blocks a write with `EROFS`, and the mount point cannot be renamed, removed,
or replaced from inside the sandbox (`EBUSY`). It does not pin the top-level
`.bare/commondir` (see the last bullet); that route stays open to host git
that prism does not run.

- `.bare/config`, `.bare/hooks`, `.bare/info`, and the whole
  `.bare/worktrees` tree are read-only; the session's own worktree git dir
  is re-exposed read-write on top.
- `commondir`, `gitdir`, and `config.worktree` in the own worktree git dir,
  and the worktree `.git` file, are pinned read-only over the read-write
  binds — the bwrap equivalent of sections 21j and 21k.
- On the host, the bwrap `Prepare` runs `ensureGitUpstream` and binds only
  the worktree git dir that `resolveGitWriteScope` accepts, the same as
  sandbox-exec. It does not run `prepareGitWriteDirs`: under bwrap the ref
  and reflog parent dirs are inside the read-write `.bare` bind, so git
  creates them itself.
- The top-level `.bare/commondir` is NOT pinned. A benign commondir file is
  not a no-op: any commondir file makes git treat the project root (which
  holds a `.git` gitfile pointing at `.bare`) as a work tree, so a git
  command run there behaves destructively. A worker can still create
  `.bare/commondir`. For prism's OWN host git this is at most a denial of
  service: #3322's (#3324) `OpenBare` refuses a repo that has a
  `.bare/commondir` file, and prism's host git sets `GIT_COMMON_DIR`. But a
  plain `git` that prism does NOT run — a human shell, or another tool at
  the project root — follows `<BareRoot>/.git` -> `.bare` -> the planted
  `.bare/commondir` and reads the `config` and `hooks` the worker wrote. So
  this stays a host code-execution route for a plain `git` that prism does
  not run. #3326 (user git env hardening, OPEN) will turn off the
  code-execution config keys for every user git process, so a planted
  `config` runs no command even when a plain host `git` reads it; until it
  lands the route is open. Binding `.bare` read-only — which would stop a
  worker writing any bare-dir file — is not an option; see "Why bwrap cannot
  match this scope" below for why.

### Why bwrap cannot match this scope (a permanent isolator difference)

bwrap does not reach the sandbox-exec write scope for the files inside
`.bare`, and bind mounts cannot achieve it. This is a permanent difference
between the two isolators, not a follow-up. #3327 set out to close it by
binding `.bare` read-only and binding read-write only the directories a
session writes. That design is not implementable, for two reasons found
during #3327 and both reproduced with `strace` on git 2.56.

1. **Bind mounts are directory-granular; SBPL is per-access.** A bwrap mount
   makes a path read-only in one of two ways: a read-only bind of the path
   itself (a file pin), or a read-only bind of a parent directory. A
   read-only parent protects every child, so it cannot keep `config`
   read-only and a sibling writable. A file pin is file-granular but
   detachable (reason 2 below). sandbox-exec has neither limit: its SBPL
   profile is a path filter that the kernel evaluates at each access, not a
   mount, so it allows `.bare/packed-refs.lock` and denies `.bare/config` in
   the same directory, and no rename can detach a rule.

2. **git forces `.bare` to be writable, so the read-only-parent route is
   closed.** The files ref backend creates `.bare/packed-refs.lock` in the
   `.bare` root on every ref update — every commit and every rebase, even
   for a loose ref with no `packed-refs` file. So a read-only `.bare` makes
   every commit print `error: unable to create .bare/packed-refs.lock`
   (non-fatal — the loose ref still updates) and makes every ref deletion
   fail (fatal): `git fetch --prune` and `git branch -d` exit non-zero and
   leave the ref. A read-only `.bare` therefore breaks the worker commit and
   the coordinator `git fetch --prune`. `.bare` must stay writable.

With `.bare` writable, the only protection left for `.bare/config` (and
`.bare/hooks`, `.bare/info`, the pointer files) is a read-only bind of the
file over it — what #3315 does. Since Linux 3.18 a host-side rename onto, or
unlink of, a bound file calls `detach_mounts` on that dentry and removes the
pin in every running bwrap session, across namespaces. prism renames
`.bare/config` host-side (`ensureGitUpstream` at spawn, `git worktree add -b`
through `branch.autoSetupMerge`, `git branch -d` at cleanup), so a concurrent
spawn or cleanup can re-open `.bare/config` read-write for a running worker.
(This is reasoned from the kernel source; it is not verified on a live
kernel, because a worker cannot start a nested bwrap.)

The same two reasons mean bwrap cannot isolate the refs. `.bare/refs` and
`.bare/logs` must be writable for a commit, and a writable `refs/heads` lets
a worker write a loose `refs/heads/main` (or any other live worktree branch)
that shadows or moves it, write `refs/stash`, create a branch, and write
`.bare/gc.pid` and `.bare/HEAD`. A read-only pin on a live branch ref is
detached by the next host-side update of that ref. So the table rows for the
branch ref and the stash ref, and the "Other branches" and "`git gc`" known
limits above, hold for sandbox-exec but not for bwrap. This is the permanent
refs vector; the structural fix is #3334.

One narrowing is NOT blocked by the two reasons and is achievable. A bwrap
review agent or investigator gets its whole own worktree git dir read-write
(`HEAD`, the index, the branch ref) today, where sandbox-exec grants only
`FETCH_HEAD` (#3293 AC). The own git dir sits under the read-only
`.bare/worktrees` bind, so a read-only session need only skip the read-write
re-bind of its own git dir and take a read-write bind of `FETCH_HEAD` that
the host creates first — no `.bare`-root write is involved. That needs Go
code, so it is the remaining #3327 work, not this doc change. #3327 stays
open for it.

### The two residual vectors and their tracking

1. **A planted `.bare/config` or hook — an OPEN host-code-execution residual
   (#3337).** The in-sandbox pins of #3315 block the in-sandbox write path,
   but under bwrap they are detachable (above), so they are not a reliable
   control. The host-side controls are partial today:
   - #3322 makes every git command prism runs set `GIT_DIR` and
     `GIT_COMMON_DIR`, run with hooks and fsmonitor off, and refuse a repo
     that has a `.bare/commondir` file. But #3322 does NOT pin
     `core.sshCommand`, `credential.helper`, or a filter driver — those keep
     the user's values so fetch and push work (`host-git-commands.md`).
   - So today, if a concurrent spawn or cleanup renames `.bare/config` and
     detaches the pin, a worker can write `core.sshCommand` (or
     `credential.helper`, or a filter driver) into `.bare/config`, and
     prism's own host git — for example the `prism review --rebase` fetch
     and push — reads it through `GIT_COMMON_DIR` and runs the command. That
     is host code execution. It is fixable host-side (pin those keys in
     `hostGitConfig`) and is tracked by #3337.
   - #3326 (OPEN) separately hardens every USER git process (the shell
     prompt and gitsigns in a worktree pane) against a planted `config`, by
     turning off the code-execution config keys with env overrides. Until
     #3326 lands a user git in a pane is also exposed; and even after it,
     #3326's own known limits remain (URL-scoped credential helpers, a
     remote-named transport key, and a filter driver reached through an
     attributes file).

2. **A worker can move `.bare/refs/heads/main` or another worktree's branch
   ref — in scope, tracked by #3334.** This changes another session or the
   coordinator's local view, which is in the threat model, and bind mounts
   cannot block it. The structural fix is #3334: give each bwrap worker its
   own git common dir (or use the reftable backend), so `.bare/refs` is not
   shared, and prism reconciles the worker's own commits into `.bare` at
   push or review time. The practical reach today is limited — the
   coordinator merges through GitHub, not the local ref, and `git pull
   --ff-only` in `main` rejects a non-fast-forward planted ref — but it is
   still in scope. #3334 holds the design.

### Do not bind `.bare` read-only for bwrap (regression note)

It was tried in #3327 and does not work. git needs `.bare/packed-refs.lock`
in the `.bare` root on every commit, so a read-only `.bare` breaks the worker
commit (an `error:` line on every commit) and the coordinator `git fetch
--prune` (a hard failure). The object store and
`packed-refs` stay writable under bwrap, as under sandbox-exec, so the "both
isolators" limits above apply. See the `.bare` write scope (bwrap) section of
`session-lifecycle.md` for the bind ordering.

## User git on the host (#3326)

Each git process of the user on the host gets config in its environment.
git reads this config after each config file, so a planted `.bare/config`,
a nested git dir, or a staged submodule cannot change the keys in the list.
The list turns off the keys that run a program, ignores submodules and
`.gitattributes`, and pins the keys of SSH commit signing. It applies to the
shell prompt and gitsigns in a worktree pane, and to a plain `git` at the
project root (the `.bare/commondir` route above).

The list, the tests, where the variables reach, and the known limits are in
`docs/invariants/user-git-overrides.md`.
