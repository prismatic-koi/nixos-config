# sandbox-exec testing convention

<!-- doclint-ignore: auth.json, settings.json, SYSTEM.md, mcp.json -->
<!-- doclint-ignore: .bare/config, .bare/hooks/post-checkout, .bare/logs, .bare/objects, refs/heads/feature, refs/heads/main, refs/stash -->
<!--
  The names in the first list are files in the pi agent dir ~/.pi/agent at
  run time (see docs/invariants/sandbox-pi-agent-dir.md). The names in the
  second list are paths and refs in the git common dir of the test repo of
  sandbox_exec_git_write_scope_darwin_test.go. None of them exists in this
  source tree.
-->

This document codifies the testing convention for changes to the SBPL profile
generator under `internal/container/sandbox_exec.go`. It exists because four
PRs (#1015, #1016, #1017, #1018) collectively shipped a non-functional
sandbox-exec profile (the `/etc` symlink-resolution gap from #1187). Every
unit test in `sandbox_exec_test.go` was green throughout. Those tests asserted
profile string content (substring checks) but never invoked
`/usr/bin/sandbox-exec` against the generated profile to confirm it actually
loaded and ran a binary.

This issue is tracked in #1192.

## The convention

> Any change to `internal/container/sandbox_exec.go::generateProfile`,
> `Manager.PrepareSandboxExec`, `Manager.PrepareSessionWorkDir`, or
> `Manager.PrepareSessionTmpDir` must be
> accompanied by at least one Darwin-only integration test under
> `internal/integration/` that:
>
> 1. Generates the profile via `Manager.PrepareSandboxExec()` (not by
>    hand-writing SBPL text).
> 2. Invokes `/usr/bin/sandbox-exec -f <profile>` against a Nix-built
>    ad-hoc-signed test binary (for example, `bash`, `cat`, or `opencode` itself)
>    resolved to its absolute `/nix/store/...` path via `exec.LookPath` and
>    `filepath.EvalSymlinks`.
> 3. Asserts the binary exits 0.
> 4. Has a paired negative test that confirms the test catches regressions:
>    temporarily mutate the profile (for example, remove a key allow rule) and assert
>    the binary fails — proving the test is not a no-op.
>
> Apple-signed system binaries (`/bin/echo` and `/usr/bin/uname`) must
> NOT be used as test targets until the v3 profile migration tracked in
> #1190 / the v3 spike lands. They SIGABRT in dyld under the current profile
> shape, which is a separate known limitation.

String-level substring assertions on the generated profile are necessary but
**not sufficient**. Every positive integration test must have a paired
negative test that mutates the profile and asserts failure. The pair proves
that the positive test is not green by accident.

## Why this matters

#1015–#1018 each added or modified the SBPL profile generator. Each PR's tests
asserted things like *"the profile string contains `(subpath \"/nix\")`"* and
*"the profile string contains `(allow file-read* (subpath stagingHome))`"*.
Those assertions all passed. None of them invoked `/usr/bin/sandbox-exec` to
confirm the generated profile actually loaded and executed a binary
successfully. So when #1015–#1018 collectively shipped a profile that
fundamentally cannot launch any process (missing `/etc` allow), every test
was green.

The same class of bug will recur every time someone tightens the profile in
a future PR if we do not enshrine this convention.

## Test infrastructure

The Darwin-only integration tests live in
`internal/integration/sandbox_exec_*_darwin_test.go`. Shared helpers live in
`sandbox_exec_helpers_darwin_test.go`:

- `requireSandboxExec(t)` — skips when `/usr/bin/sandbox-exec` is unavailable
  (also skips inside the Nix build sandbox where SBPL apply is blocked).
- `requireNixBash(t)` — resolves `bash` to its `/nix/store/...` path via
  `exec.LookPath` + `filepath.EvalSymlinks`. Skips when bash is not a Nix
  store binary.
- `newProfileManager(t)` — builds a `container.Manager` with a minimal
  `Config` (per-test temp `Worktree`, sanitised `InstanceID`). Does not set
  `BareRoot`. Use this for negative tests against system-paths allow rules:
  the BareRoot-ancestor block emits a `(subpath "/")` metadata allow that
  is broad enough to mask the effect of removing per-subpath rules.
- `newProfileManagerWithBareRoot(t)` — variant that configures `BareRoot`
  two directory levels deep under HOME so the BareRoot-ancestor block fires
  and grants `file-test-existence`/`file-read-metadata` up to `/`. Use this
  for positive tests that resolve symlink targets under HOME (for example,
  credential reads through `$HOME/.aws/credentials` → host path).
- `augmentProfileForTest(profile)` — appends the minimum SBPL extras a
  Nix-built binary needs to start under the sandbox during testing (root
  inode literal, `/dev`, `/private/tmp` write). These are testing
  infrastructure only — they are NOT added to the production profile. The
  `/private/tmp` write hides a `/tmp` rule.
- `augmentProfileForProbe(profile)`, `writeProbePositiveProfile(t, p)`, and
  `withMutatedProbeProfile(t, m, mutate)` — the same helpers with only the
  root inode literal and `/dev`. Use them for a test of a `/tmp`,
  `/private/tmp`, user `$TMPDIR`, or per-session TMPDIR rule. A static Go
  probe needs no `/tmp` grant to start.
- `withMutatedProfile(t, m, mutate)` — generates the profile via
  `Manager.PrepareSandboxExec()`, applies `mutate(string) string` to the
  profile content (for example, remove a specific allow rule), augments it for
  testing, writes it to a temp file, and returns the path. Used in
  negative-case integration tests to confirm that removing a specific allow
  rule causes the test operation to fail.
- `writeAugmentedPositiveProfileWithLaunchDir(t, p, launchDir)` /
  `withMutatedProfileAndLaunchDir(t, m, launchDir, mutate)` — variants that
  additionally append getcwd ancestor allows for `launchDir` (see "Launch
  CWD" below). Defined in `sandbox_exec_launch_dir_darwin_test.go`.

## Launch CWD — sandboxed binaries that hard-require a resolvable CWD

`exec.Command` without `cmd.Dir` inherits the go-test binary's CWD — the
integration package dir inside the repo checkout — which no fixture profile
grants. Under deny-default, getcwd then fails inside the sandbox:

- **bash** merely warns (`shell-init: error retrieving current directory`)
  and continues — bash-based tests tolerate the hole.
- **node** dies at bootstrap (`EPERM: process.cwd failed ... uv_cwd`).
  **git** dies at startup (`fatal: Unable to read current working
  directory`) — tests built on these binaries fail before exercising the
  rule under test (this is how the #2022 playwright trio and the #2221
  GitConfigGlobalUsable test shipped without ever being host-green. The
  hole surfaced on the #2247 host run).

Production sessions never hit this: the agent's CWD is the worktree, which
the production profile grants RW, with §6b ancestor metadata up the chain.
Fixture tests using node/git must mirror that shape — a fixture problem is
never a reason to widen the production profile:

1. Set `cmd.Dir` to a directory the profile under test grants (typically
   the session work dir, covered by the §6 `(subpath "<sessionDir>")`
   rule).
2. Build the test profile with the launch-dir helper variants above, which
   append `(literal ...)` ancestor-node allows for the getcwd walk. Literal
   rules match the directory node only — never contents beneath it — so
   they cannot mask subtree-scoped negatives.
3. The launch dir itself deliberately gets no extra rule — its
   accessibility must come from the production rule under test. Strip
   negatives that disable that rule make the CWD unresolvable by design.
   Such negatives must use bash (tolerant), not node/git.
4. Keep in-sandbox commands free of deep absolute `mkdir -p` chains: each
   existing-but-ungranted ancestor returns EPERM (not EEXIST), which
   `mkdir -p` treats as fatal. Create only the leaf against an
   already-granted, host-prepped parent.
5. The ancestor extras grant the ancestor **nodes** only — never their
   children. Tools that probe children of ancestors hit EPERM: git's
   repository discovery walks up from CWD statting `.git` at each level,
   and the stat of `<parent-of-launch-dir>/.git` is fatal to git ("fatal:
   error reading …/.git" — EPERM, unlike a clean ENOENT). Set
   `GIT_CEILING_DIRECTORIES=:<parent-of-launch-dir>` in the in-sandbox env
   so discovery stops inside the launch dir (the leading empty entry skips
   symlink resolution of the ceiling path, per git(1)). Do not widen the
   extras to cover ancestor children instead — that erodes the
   masking guarantee in point 2.

## Negative test pattern

Every positive integration test must have a paired negative test. The
negative test mutates the profile to remove the rule under test, then runs
the same operation, and asserts failure. This proves the positive test is
not green by accident.

The `withMutatedProfile` helper exists to make this pattern easy to write.
The pattern below mirrors the actual
`TestSandboxExecSessionWorkDir_DeniedWithoutSubpath` in
`internal/integration/sandbox_exec_session_work_dir_darwin_test.go`:

```go
func TestSandboxExecSessionWorkDir_DeniedWithoutSubpath(t *testing.T) {
    if runtime.GOOS != "darwin" {
        t.Skip("sandbox-exec is Darwin-only")
    }
    requireSandboxExec(t)
    nixBash := requireNixBash(t)

    m := newProfileManager(t)
    sessionDir, err := m.SessionWorkDir()
    if err != nil {
        t.Fatalf("SessionWorkDir: %v", err)
    }

    // Re-target the (subpath "<sessionDir>") entry at a non-existent
    // sibling path rather than deleting the line — this keeps the SBPL
    // syntactically valid regardless of where the entry sits in its allow
    // block, and sandbox-exec silently ignores rules for non-existent
    // paths. Deleting an entire (allow ...) block instead can leave
    // orphaned (subpath ...) lines — sandbox-exec would reject the
    // malformed profile at parse time and the negative test would pass
    // for the wrong reason.
    mutatedPath := withMutatedProfile(t, m, func(p string) string {
        return strings.ReplaceAll(p,
            sbplQuoteForTest(sessionDir),
            sbplQuoteForTest(sessionDir+".prism-2213-disabled"))
    })

    // Attempt to write a file inside the work dir. Without the work-dir
    // (subpath ...) allow, the operation must fail.
    probe := filepath.Join(sessionDir, "prism-2213-write-probe-denied.tmp")
    cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
        nixBash, "-c", "echo hi > "+shQuote(probe))
    out, runErr := cmd.CombinedOutput()
    if runErr == nil {
        t.Errorf("work-dir write succeeded WITHOUT the work-dir (subpath ...) rule: %s", out)
    }
}
```

## Coverage goals

In addition to the system-paths allow set (covered first by #1187 / #1192),
integration test coverage exists for:

- **Session work dir writes** — the per-session work dir
  (`(subpath "<sessionDir>")`), plus the worktree, the git paths, and host-API
  socket dir.
- **Env-var credential reads** — sops-backed configs read through stable
  host paths (for example, `~/.config/aws/readonly-config` via `AWS_CONFIG_FILE`,
  `~/.config/kube/agents-config` via `KUBECONFIG`, the SSH keys via the
  work-dir ssh-config/gitconfig), riding the broad var-folders allow
  narrowed by the #2211 secrets.d allowlist.
- **Nix flake trusted-settings read** — the single-file read-only allow on
  `~/.local/share/nix/trusted-settings.json` that flake-CLI nix commands
  need when the target flake declares a `nixConfig` block (issue #2201).
- **Explicit denies** — `~/.aws`, `/private/etc/wireguard`,
  `/private/etc/wpa_supplicant`, `/private/etc/ssh` are blocked.
- **sops secrets.d narrowing** — the secrets.d subtree deny (read + write)
  with named require-not exceptions (issue #2211): real-tree denial of
  `github_token` and the daily-driver RSA key, allowlisted stable-chain
  reads (`~/.ssh/<key>`, `~/.config/aws/readonly-config`,
  `~/.config/kube/agents-config`), and a fake-tree counter-rotation
  simulation proving the exceptions are counter-independent (#1410/#1573).
  See `sandbox_exec_secrets_deny_darwin_test.go`.
- **The gitlab_token carve-out** — the one name #2668 adds to that
  allowlist, derived from `Config.GitLabTokenPath`: readable in-sandbox when
  the host configures it, denied when it does not, with `github_token` in
  the same generation dir denied in both cases, plus the paired negative
  that strips the exception. See `sandbox_exec_gitlab_token_darwin_test.go`.
- **The grafana config-bundle carve-out** — the one name #2746 adds to that
  allowlist, derived from `Config.GrafanaConfigPath` (the path prism injects
  as `GRAFANA_MCP_CONFIG_PATH`): readable in-sandbox when the host and role
  configure it, denied when they do not, with `github_token` in the same
  generation dir denied in both cases, plus the paired negative that strips
  the exception. See `sandbox_exec_grafana_config_darwin_test.go`.
- **The worktree path deny** — the final deny (section 21b) on unlink and
  create of the worktree and of every ancestor of it, `BareRoot` included.
  Under the production profile, a write inside the worktree works, and a
  rename or removal of the worktree or of `BareRoot` fails, also a move of
  the worktree into the per-session TMPDIR. The paired negatives strip the
  deny and replace each path with a symlink. The worktree negative moves
  the worktree into the TMPDIR, because section 6e grants only a read of
  `BareRoot`, so a move to a sibling of the worktree fails with or without
  the deny. The `BareRoot` negative also adds a write grant of `BareRoot`.
  See
  `sandbox_exec_worktree_entry_darwin_test.go`.
- **The git write scope** — the section-6e read of the project root and
  write of the git paths, the section-21i deny on a replace of the git dir
  nodes, the section-21j deny on a write of the redirect files in the
  worktree git dir, the section-21k deny on a write of the `.git` file of
  the worktree, and the dirs that Prepare makes in `.bare` (#3293). The fixture is
  a real bare+worktree layout under `HOME` with the worktrees `main` and
  `feature`, and a bare remote in the per-session TMPDIR. A Nix git runs in
  the sandbox, in the worktree. Under the production profile:
  - A worker in `feature` commits, fetches, pushes with the upstream that
    Prepare writes into `.bare/config` and no config error, rebases,
    force-pushes, and runs `git worktree list`.
  - After a push from the sandbox, the host runs a bare `git push` with
    `push.autoSetupRemote`, as a host-pane push does. The `prism review
    --rebase` pre-flight push no longer matches this: it names an explicit
    `refs/heads/<branch>` refspec and writes no upstream. Then a push and a
    pull from the sandbox work, and `.bare/config` names one upstream.
  - The same worker cannot write a file in `main`, make `.pi` in `main` or
    `.agents` at the project root, write `.bare/config` or
    `.bare/hooks/post-checkout`, move `refs/heads/main` or `refs/stash`,
    write the git dir of `main`, or move `.bare/objects`. In its own git
    dir, it cannot write `commondir`, `gitdir`, or `config.worktree`, or
    rename a file onto `commondir`. It cannot write, move, remove,
    replace, or chmod the `.git` file of its worktree. `.bare/config`,
    `refs/heads/main`, `commondir`, `gitdir`, and the `.git` file do not
    change.
  - The coordinator in `main` pulls with the upstream in `.bare/config`,
    and writes and deletes `refs/stash`. It cannot write in `feature` or
    move `refs/heads/feature`.
  - A review agent in `feature` runs `git status`, `git fetch`,
    `git diff`, and `git log`. It cannot move `refs/heads/feature`, and
    cannot write `HEAD` or the index of `feature`.

  The paired negatives:
  - `TestSandboxExecGitWriteScope_PlantsWorkWithOldGrant` changes the read
    of the project root back to the old read-write grant. The plants work.
  - `TestSandboxExecGitWriteScope_CommitFailsWithoutGitClause` points
    each `.bare` rule at a missing dir. A commit fails.
  - `TestSandboxExecGitWriteScope_FetchFailsWithoutPreparedDirs` removes
    `.bare/logs` after Prepare. A fetch fails.
  - `TestSandboxExecGitWriteScope_ObjectsReplaceableWithoutNodeDeny`
    strips section 21i. The session replaces `.bare/objects` with a
    symlink.
  - `TestSandboxExecGitWriteScope_RedirectFilesWritableWithoutDeny` strips
    section 21j. The session writes `config.worktree` and replaces
    `commondir` in its git dir.
  - `TestSandboxExecGitWriteScope_GitFileReplaceableWithoutDeny` strips
    section 21k. The session replaces the `.git` file of its worktree and
    changes its mode.
  - `TestSandboxExecGitWriteScope_CoordinatorStashFailsWithoutStashGrant`
    points the stash rules at another name. The stash write fails.
  - `TestSandboxExecGitWriteScope_WorkerMovesBranch` runs the writes of
    the review-agent test under the profile of a worker. They work.
  - `TestSandboxExecGitWriteScope_PushFailsWithUpstreamInBothFiles` moves
    the upstream from `.bare/config` into the session gitconfig. The push
    on the host writes a second upstream into `.bare/config`, and the next
    push from the sandbox fails.

  See `sandbox_exec_git_write_scope_darwin_test.go`.
- **The podman and docker exec deny** — the final deny (section 21c) on
  exec of a file named `podman`, `docker`, `.podman-wrapped`, or
  `.docker-wrapped`. Under the production profile, a copy of Nix bash
  under each name fails to run, also through a symlink with another name,
  and a copy under another name runs. The paired negative strips the deny
  and runs the copy named `podman`. See
  `sandbox_exec_container_cli_darwin_test.go`.
- **The read-only worktree deny** — the final deny (section 21d) on
  `file-write*` in the worktree, for a session with
  `Config.WorktreeReadOnly` set (a review agent or an investigator). Under
  the production profile of such a session, a read of a worktree file
  works, and a change, a create, and a remove each fail with
  "Operation not permitted". Under the profile of a worker, the same
  writes work. The paired negative strips the deny from the read-only
  profile and the writes work. See
  `sandbox_exec_worktree_readonly_darwin_test.go`.
- **Network egress** — `(allow network*)` permits outbound TCP.
- **The Unix-socket connect allowlist** — the section-17b deny on
  `network-outbound` for every Unix-socket path except an allowlist. The
  allowlist holds the per-session TMPDIR and no shared temp tree: not
  `/private/tmp`, not `/tmp`, and not the user `$TMPDIR` (#3066, #3147,
  #3150). Under the production profile:
  - A connect to a socket under the user `$TMPDIR` fails: the podman machine
    location and an nvim-shaped location.
  - A connect to each host-socket shape in `/private/tmp` fails: a socket
    that the profile does not name (the model of `macron-type.sock`), a
    tmux-shaped socket, a .NET-diagnostic-shaped socket, and the podman and
    nvim sockets of the `TMPDIR`-unset location. The same socket by its
    `/tmp` path fails too.
  - A connect to a socket in the per-session TMPDIR and a loopback TCP
    connect both succeed.
  - A move of a host socket, or of the dir that holds it, from a shared temp
    tree into the per-session TMPDIR fails, so no connect follows. The
    session has no write grant on the source.

  The paired negatives make these changes to the profile:
  - Neutralise the section-17b scope term.
  - Remove that term.
  - Add a `/private/tmp` and `/tmp` allowance to section 17b.
  - Add a write grant on the shared temp trees.

  Each negative then gives the opposite result. The probe is a CGO-disabled
  Go binary that the test builds, so no extra Nix tool is necessary. It does
  the connect, the bind, and the file operations in-process. Thus the tests
  need no shell. Plain listeners stand in for podman, tmux, nvim, and the
  other host services. The rule is path-based, and the macos-15 job (which
  fails on any SKIP) has none of these services. See
  `sandbox_exec_container_socket_darwin_test.go`.
- **The per-session TMPDIR** — the section-3 read-write grant on
  `/var/tmp/prism-<hash of the instance ID>` (both `/var` symlink forms) and
  the section-21e deny on unlink, create, mode writes, flag writes, and ACL
  writes of its node (#3150, #3170). The dispatcher sets `TMPDIR` and `MAC_CHROMIUM_TMPDIR`
  to this dir. Under the production profile:
  - The session creates, reads, renames, and removes a file in its TMPDIR,
    and binds and connects a socket at a 92-byte path there. It creates and
    removes a dir there, and changes the mode (`chmod 700`) and the flags
    (`chflags nodump`) of a file in that dir.
  - The session cannot create, rename, or remove a file in `/private/tmp`,
    `/tmp`, or the user `$TMPDIR`, and cannot move a file from there into
    its TMPDIR.
  - The session cannot read a file, create a file, or connect to a socket in
    the TMPDIR of another session.
  - The session cannot rename or remove the node of its TMPDIR.
  - The session cannot `chmod 777`, `chflags uchg`, or `chmod +a` the node
    of its TMPDIR, by either `/var` form. The mode, the flags, and the ACL of
    the node do not change. The test puts the node back after each check, so
    that one change (for example uchg) cannot make the next check fail for
    another reason.

  The paired negatives make these changes to the profile:
  - Point the TMPDIR rules at a missing path.
  - Add a write grant on the shared temp trees.
  - Point the TMPDIR rules at the TMPDIR of the other session.
  - Strip the section-21e deny.
  - Take `file-write-mode` out of the section-21e deny.
  - Take `file-write-flags` out of the section-21e deny.
  - Take `file-write-acl` out of the section-21e deny.

  Each negative then gives the opposite result. See
  `sandbox_exec_session_tmp_darwin_test.go`. The suite puts its TMPDIRs below
  `/var/tmp/pst<4 hex chars>`, not directly in `/var/tmp`. A production
  prism on the same host runs the stale-TMPDIR sweep (#3170). That sweep
  removes each `/var/tmp/prism-<hash>` dir that its database does not know.
  It does not look below the children of `/var/tmp`.
- **The harness pipe secret** — the section-6 run-dir grant gives a session
  the read of `pipe.secret` in its own run dir, and no other rule gives a
  read of the run dir of another session (#3356). On Darwin the harness pipe
  is a `127.0.0.1` TCP port that every session can reach, so this file is
  what stops a session from a handshake as another session. The tests run a
  real sidecar with a TCP pipe outside the sandbox. A pipe probe under the
  production profile reads a secret file, or takes a literal secret, sends
  `hello` over the loopback port, and prints the reply. Under the production
  profile:
  - The agent of a session reads its own secret and gets `hello_ack`.
  - Session A cannot read the secret of session B. The sidecar of B refuses
    a `hello` from A with the secret of A, and one with no secret, with the
    `unauthorized` error frame.
  - A `hello` from the sandbox of A with the secret of B, given to the probe
    as a literal, gets `hello_ack`. Thus the loopback port of B is open to
    A, and the secret is what refuses the other peers.

  The paired negative points the run-dir rule at a missing dir. The read of
  the own secret then fails. See `sandbox_exec_pipe_secret_darwin_test.go`.
- **The run dir node** — the section-6 grant on the per-session run dir,
  `$XDG_STATE_HOME/prism/run/<hash>/`, and the section-21m deny on unlink,
  create, mode writes, flag writes, and ACL writes of its node (#3373). The
  run dir holds `hostapi.sock`, `pipe.secret`, and `agent-run.log`. Under
  the production profile:
  - The session creates, reads, renames, removes, and changes the mode of a
    file in its run dir, and connects to the `hostapi.sock` there.
  - The session cannot remove or rename its run dir node. When the node is
    absent, the session cannot put a symlink to the run dir of another
    session, or a new dir, at the path.
  - The session cannot `chmod 777`, `chflags uchg`, or `chmod +a` the node.
    The mode, the flags, and the ACL of the node do not change.

  The paired negatives make these changes to the profile:
  - Point the run-dir rules at a missing path. The create and the connect
    fail.
  - Strip the section-21m deny. The session moves its run dir away and puts
    a symlink to the run dir of another session at the path. A sidecar for
    the session, as after a `prism restore`, then refuses to start
    (`internal/rundir`), and the `hostapi.sock` of the other session stays
    live and does not change.
  - Take `file-write-mode`, `file-write-flags`, or `file-write-acl` out of
    the section-21m deny. The matching change of the node works.

  See `sandbox_exec_run_dir_darwin_test.go`.
- **The pi agent dir** — the section-6a grant on `~/.pi/agent`, for pi
  sessions only: a read of the whole dir, and a write of an exact list
  (`atlassian-mcp-oauth.json` and the sessions dir of the worktree). The
  section-6d grant on the per-session agent dir, which is
  `PI_CODING_AGENT_DIR` of the session: it holds the `auth.json` of the
  session, the pi lock dirs, and a symlink to each other host entry
  (#3295). Also the section-6c grant on the shared lock dir and the
  Notion store dir, `~/.local/state/prism/pi-locks` and
  `~/.local/state/prism/pi-notion` (#3289, #3290), the final deny
  (section 21f) on create and unlink of `auth.json` and
  `atlassian-mcp-oauth.json` (#3284), the final deny (section 21g) on a
  replace or a mode change of the two shared dir nodes, and the final deny
  (section 21h) on the same for the per-session agent dir node. The test
  sets a test-owned `HOME` under the real home (`useTestHome`), so it never
  touches the real `~/.pi/agent`. Under the production profile, each write
  that pi and its extensions do in a normal session works. A write of
  `auth.json` reaches the per-session file, and the other writes reach the
  host. Each plant fails with "Operation not permitted", and the host dir
  does not change: a change of `settings.json`, a new `SYSTEM.md` or
  `mcp.json`, a file in `bin/`, a write in the sessions dir of another
  worktree, a write, a removal, or a replacement of the host `auth.json`, a
  write through a symlink in the per-session agent dir, a Notion store name
  in the agent dir, and a removal, a rename, or a mode change of a shared
  dir node or of the per-session agent dir node. The paired negatives:
  - `TestSandboxExecPIAgentDir_PlantsWorkWithOldGrant` replaces the write
    list with the old read-write grant of the whole dir and strips section
    21f. The plants work.
  - `TestSandboxExecPIAgentDir_HostAuthWritableWithOldGrant` adds the
    write grant of the host `auth.json` from #3284. The session writes a
    `!` value into it.
  - `TestSandboxExecPIAgentDir_PiWritesDeniedWithoutSessionClause` strips
    the section-6d clause. The first read or lock-dir `mkdir` in the
    per-session agent dir fails.
  - `TestSandboxExecPIAgentDir_PiWritesDeniedWithoutWriteClause` strips
    the section-6a write clause. The write of the Atlassian file fails.
  - `TestSandboxExecPIAgentDir_SharedStateWritesDeniedWithoutClause` strips
    the section-6c clause. The `mkdir` of the refresh lock dir fails.
  - `TestSandboxExecPIAgentDir_SharedDirReplaceWorksWithoutDeny` strips
    section 21g. The session can remove the lock dir node.
  - `TestSandboxExecPIAgentDir_SessionDirReplaceWorksWithoutDeny` strips
    section 21h. The session can change the mode of the per-session agent
    dir node.
  - `TestSandboxExecPIAgentDir_CredentialReplaceWorksWithoutDeny` strips
    section 21f. The session can remove `atlassian-mcp-oauth.json` and put
    a symlink at its path.

  A session that is not in `NOTION_MCP_REPOS` has an empty
  `PINotionHostDir` (#3319). Its profile has no section-6c grant of the
  Notion store dir. Section 21l denies a read of the old store files in
  `~/.pi/agent`. `TestSandboxExecPIAgentDir_NotionStoreDeniedWithoutGrant`
  shows that each read and each write of the store fails with "Operation
  not permitted". A read through the symlink in the per-session agent dir
  fails too. The rest of the agent dir stays readable. The paired
  negatives:
  - `TestSandboxExecPIAgentDir_NotionStoreReadableWithGrant` uses the
    profile of a session in the list. The same reads work.
  - `TestSandboxExecPIAgentDir_NotionLegacyReadableWithoutDeny` strips
    section 21l. The session reads the old store file.

  See `sandbox_exec_pi_agent_dir_darwin_test.go`.

Each positive case has a paired negative case that mutates the profile to
remove the specific rule under test and asserts the operation fails.

## When the convention applies

Apply this convention to any change that touches:

- `internal/container/sandbox_exec.go::generateProfile` (any rule add/remove
  or precedence change)
- `Manager.PrepareSandboxExec` (the wrapper that writes the profile and
  builds args)
- `Manager.PrepareSessionWorkDir` (work-dir layout — the generated
  configs, kube cache, and chromium Library skeleton the env redirects
  point at)
- `Manager.PrepareSessionTmpDir` (the per-session TMPDIR that `TMPDIR` and
  `MAC_CHROMIUM_TMPDIR` point at)

Pure refactors that do not change the generated SBPL output are exempt. The
integration tests must continue to pass on the refactored code, which is
the load-bearing check.

## CI execution — the macos-15 job

The Darwin-tagged tests described above need a real macOS host with a
working top-level `sandbox-exec`. Two environments look plausible. Neither
one works:

- **The ubuntu `go-tests` job** — the `//go:build darwin` tag excludes
  `sandbox_exec_*_darwin_test.go` from the Linux build entirely. These
  files never compile there.
- **A prism worker sandbox on Darwin** — `requireSandboxExec(t)` skips,
  because a nested `sandbox-exec` profile cannot apply inside an
  already-sandboxed worker. Every test in this suite reports SKIP, not
  PASS, in that environment.

A `go-tests-macos-sandbox-exec` job runs on a GitHub-hosted `macos-15`
runner instead. This is the same runner class that `build-and-cache.yml`
and `update-flakes.yml` already use. A `macos-15` runner is a bare macOS
VM, not a nested prism sandbox. A top-level `sandbox-exec` profile applies
there, so the tests execute rather than skip. See issue #2749.

The test binaries (`bash`, and `mv` with the other coreutils) must resolve
to a `/nix/store/...` path. See "Nix-store binary requirement" below. The job installs Nix with the
cachix/install-nix-action GitHub Action. Every other nix-using job in this
workflow already uses that same action. The job then runs the tests inside
`nix shell`:

```
nix shell nixpkgs#bash nixpkgs#coreutils nixpkgs#git \
  --command go test ./internal/integration/ -run '^TestSandboxExec(GitLabToken|GrafanaConfig|PIAgentDir|WorktreeEntry|WorktreeReadOnly|ContainerCLI|ContainerSocket|SessionTmp|GitWriteScope|PipeSecret|RunDir)' -race
```

The tests in `sandbox_exec_gitlab_token_darwin_test.go` and
`sandbox_exec_grafana_config_darwin_test.go` also need `TMPDIR` to resolve
under `/var/folders`.
The production secrets.d deny regexes key off that path. A bare `macos-15`
runner starts with `TMPDIR` under `/private/tmp`, so the job exports
`TMPDIR="$(getconf DARWIN_USER_TEMP_DIR)"` before it runs `go test`.

**Selector scope: eleven named families, not the whole `TestSandboxExec*`
family.** Issue #2749's problem statement names exactly two affected files:
`sandbox_exec_gitlab_token_darwin_test.go` and
`sandbox_exec_grafana_config_darwin_test.go`. Issue #3061 adds a third file,
`sandbox_exec_worktree_entry_darwin_test.go`, for the section-21b worktree
path deny. Its only extra need on a bare `macos-15` runner is Nix coreutils.
Issue #3065 adds a fourth file,
`sandbox_exec_container_cli_darwin_test.go`, for the section-21c podman and
docker exec deny. It needs only Nix bash.
Issue #3079 adds a fifth file,
`sandbox_exec_worktree_readonly_darwin_test.go`, for the section-21d
read-only worktree deny. Like the worktree path deny, it needs only Nix
bash and Nix coreutils.
Issue #3066 adds a sixth file,
`sandbox_exec_container_socket_darwin_test.go`, for the section-17b
Unix-socket connect allowlist. It needs no Nix tool. The test builds its own
CGO-disabled connect probe with the Go toolchain that the job sets up. The
test process binds the listeners. The test relies on the same
`TMPDIR`-under-`/var/folders` export as the gitlab/grafana tests. Thus its
"podman-location" socket (under `os.TempDir()`) is in the user `$TMPDIR`.
Issue #3150 adds a seventh file, `sandbox_exec_session_tmp_darwin_test.go`,
for the per-session TMPDIR. It uses the same probe and needs no Nix tool.
Issue #3284 adds an eighth file, `sandbox_exec_pi_agent_dir_darwin_test.go`,
for the section-6a pi agent dir rules and the section-21f deny. It needs Nix
bash and Nix coreutils, and a test-owned `HOME` under the real home.
Issue #3293 adds a ninth file, `sandbox_exec_git_write_scope_darwin_test.go`,
for the section-6e git write scope and the section-21i, 21j, and 21k denies. It needs Nix
bash, Nix coreutils, and Nix git. It makes its repos under the real home.
Issue #3356 adds a tenth file, `sandbox_exec_pipe_secret_darwin_test.go`,
for the harness pipe secret. It needs no Nix tool. It builds a CGO-disabled
pipe probe, and uses the connect probe of #3066, with the Go toolchain that
the job sets up. It runs a real sidecar in the test process, with a TCP pipe
on `127.0.0.1` and no host-API socket. It puts the state dir and `BareRoot`
under the real home, so the run dirs are outside the user `$TMPDIR`, which
section 2 grants read.
Issue #3373 adds an eleventh file, `sandbox_exec_run_dir_darwin_test.go`,
for the section-21m deny on the run dir node. It needs no Nix tool. It uses
the connect probe of #3066. It runs a real sidecar in the test process with
no listener. It puts the state dir under the real home, as the PipeSecret
file does.
The `-run` selector matches only the `TestSandboxExecGitLabToken_*`,
`TestSandboxExecGrafanaConfig_*`, `TestSandboxExecPIAgentDir_*`,
`TestSandboxExecWorktreeEntry_*`, `TestSandboxExecWorktreeReadOnly_*`,
`TestSandboxExecContainerCLI_*`, `TestSandboxExecContainerSocket_*`,
`TestSandboxExecSessionTmp_*`, `TestSandboxExecGitWriteScope_*`,
`TestSandboxExecPipeSecret_*`, and `TestSandboxExecRunDir_*` functions. A wider `^TestSandboxExec`
selector was tried first. PR #2785 (round-2 review) found that this
selector skips several other sandbox-exec suites on a bare `macos-15`
runner. For example, the playwright suite
skips, because `playwright-cli` is not installed by this job. A skip in
those suites turns the guard step, and therefore the required `pr-gate`
check, permanently red. The cause is a provisioning gap, not a defect in
any given PR's change. Widening the selector to cover
another `TestSandboxExec*` suite is a deliberate, separate change. First
audit that suite's own `require*`/skip preconditions. Check what a bare
`macos-15` runner actually provides: host keychain state, Xcode command
line tools, and any additional Nix-store binaries the suite needs.
Provision whatever the suite needs in this job. Add its prefix to `-run`
only after that audit.

**No-duplication mechanism.** Every test function in
`sandbox_exec_*_darwin_test.go` carries the name `TestSandboxExec*`. No
cross-platform (untagged) test in `internal/integration/` uses that
prefix. The `-run` selector on macOS therefore matches only darwin
sandbox-exec test functions. Every other integration test in the package
compiles — that is harmless, because it is Go. It does not execute,
because it never matches the selector. On the ubuntu `go-tests` job the
darwin files do not compile at all. Each test therefore runs at most once
across the whole pipeline. If you add a new darwin sandbox-exec test that
must run in this job, name its function `TestSandboxExec*`. Verify its own
preconditions against a bare `macos-15` runner first (see the
selector-scope note above), then add its prefix to the `-run` pattern.

**A SKIP must never read as a PASS.** `go test` exits 0 even when every
selected test skips. For example, the `/var/folders` `TMPDIR` guard in
`requireSandboxExec`/`requireNixBash` can trip on a future runner image
change. A green exit code alone is not enough evidence that the
sandbox-exec tests actually ran. The job's guard step parses the `-v`
output. It must fail the job if:

- any selected test reports SKIP, or
- zero selected tests ran at all. This guards against the `-run` selector
  matching nothing, including future naming drift.

This mirrors the ubuntu `go-tests` job's "Summarise skipped tests" step. It
differs in one way: it fails the job on any SKIP or on a zero-test run,
rather than only summarising them. A SKIP in this suite means the coverage
this document exists to guarantee did not run at all.

The job is gated on the same Go-relevant path filter as the ubuntu
`go-tests` job. Its result feeds the `pr-gate` fan-in check, like every
other required job.

**Sidecar owner-check tests (`internal/session`).** The job also runs the
sidecar owner-check tests of issue #3136. On Darwin, `StopSidecar` and
`KillSidecar` read the command line of a PID with the `kern.procargs2`
sysctl, not from `/proc`. The ubuntu `go-tests` job cannot run that code.
The step also runs the tests of `EnsureSidecarStopped` and of the
ended-session sweep (#3160). Both use the owner check, and the
review-sidecar leak was measured on Darwin. This step runs directly on the runner, with no `nix shell` and no
`sandbox-exec` profile:

```
go test -v ./internal/session/ -run '^(TestStopSidecar|TestKillSidecar_|TestSidecarPIDOwner|TestReadPIDArgv|TestParseProcArgs2|TestEnsureSidecarStopped|TestSweepEndedSidecars|TestRemoveOwnSidecarPIDFile)' -race -count=1
```

The selector has `TestKillSidecar_*` with the underscore, so it does not
match the `TestKillSidecarAndWait*` tests. Some of those tests skip on Darwin, and
a SKIP fails the guard. The step has its own guard step. The guard fails
the job on any SKIP, or if no `TestStopSidecar*` test runs. These tests need
no nested sandbox, so they also run inside a prism worker on Darwin. Only
that run covers the sandbox condition of #3136: the worker sandbox denies
the exec of `/bin/ps`, and the runner does not. If you add a
session test for a Darwin code path of the owner check, add its name to the
`-run` selector.

This job is CI-only test execution. It does not change worker isolation.
It does not touch `config.ValidIsolationModes`, and no worker spawn
default moves to `host` mode.

### Nix-store binary requirement

The selected test files call three Nix-store helpers. `requireNixBash`
(defined in `sandbox_exec_helpers_darwin_test.go`) resolves `bash`.
`requireNixCoreutilsDir` (defined in
`sandbox_exec_worktree_entry_darwin_test.go`) resolves `mv`, and its
directory supplies the other coreutils. `requireNixGit` (defined in
`sandbox_exec_session_work_dir_darwin_test.go`) resolves `git`. Each helper resolves its binary
through `exec.LookPath` and `filepath.EvalSymlinks`, then skips the test if
the resolved path is not under `/nix/store/`. Apple-signed and Homebrew
binaries fail with SIGABRT under the deny-default sandbox (see #1190). A
bare `macos-15` runner has no Nix installed and no `/nix/store` binaries on
`PATH` by default. Without the Nix-install step and the `nix shell`
wrapper above, the selected tests resolve `bash` to `/bin/bash` and skip.
The job's guard step catches that and fails on it. The tests still do not
actually run in that case. The job installs Nix and runs the tests
through `nix shell` for this reason. It does not rely on
whatever `bash` the bare runner image happens to ship. A future selector
widening that pulls in a suite calling `requireNixAws` or
`requireNixKubectl` must add the corresponding
`nixpkgs#` package to this `nix shell` invocation. The job adds
`nixpkgs#coreutils` for `requireNixCoreutilsDir` and `nixpkgs#git` for
`requireNixGit` in the same way.

## Out of scope

- Migrating the profile to `(version 3)` — separate spike in #1190.
- Test convention work for other isolation modes (bwrap). If
  symmetric work is warranted there, file a separate issue.

## References

- #1015, #1016, #1017, #1018 — the four PRs whose test gaps motivated this
  convention.
- #1187 — first integration test of the convention. Landed the seed pattern.
- #1190 — the v3 profile migration that this convention will continue to
  apply through.
- #1192 — this issue: codifies the convention and backfills coverage.
