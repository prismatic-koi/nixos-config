# sandbox-exec testing convention

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
> `Manager.PrepareSandboxExec`, or `Manager.PrepareSessionWorkDir` must be
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
  infrastructure only — they are NOT added to the production profile.
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
  (`(subpath "<sessionDir>")`, the only per-session writable grant), plus
  the worktree, bare repo, and host-API socket dir.
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
- **Network egress** — `(allow network*)` permits outbound TCP.

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

Pure refactors that do not change the generated SBPL output are exempt. The
integration tests must continue to pass on the refactored code, which is
the load-bearing check.

## CI execution — the macos-15 job

The Darwin-tagged tests described above never execute unless something
provides a real macOS host with a functioning top-level `sandbox-exec`. Two
environments look plausible but don't work:

- **The ubuntu `go-tests` job** — the `//go:build darwin` tag excludes
  `sandbox_exec_*_darwin_test.go` from the Linux build entirely, so these
  files never compile there.
- **A prism worker sandbox on Darwin** — `requireSandboxExec(t)` skips
  because a nested `sandbox-exec` profile cannot be applied inside an
  already-sandboxed worker. Every test in this suite reports SKIP, not
  PASS, in that environment.

So a `go-tests-macos-sandbox-exec` job runs on a GitHub-hosted `macos-15`
runner (the same runner class `build-and-cache.yml` and
`update-flakes.yml` already use). A `macos-15` runner is a bare macOS VM,
not a nested prism sandbox, so a top-level `sandbox-exec` profile applies
and the tests execute rather than skip. See issue #2749.

The test binary (`bash`) must resolve to a `/nix/store/...` path — see
"Nix-store binary requirement" below — so the job installs Nix
(`cachix/install-nix-action`, the same action every other nix-using job in
this workflow already uses) and runs the tests inside `nix shell`:

```
nix shell nixpkgs#bash \
  --command go test ./internal/integration/ -run '^TestSandboxExec(GitLabToken|GrafanaConfig)' -race
```

**Selector scope: the two named carve-out families, not the whole
`TestSandboxExec*` family.** Issue #2749's problem statement names exactly
two affected files — `sandbox_exec_gitlab_token_darwin_test.go` and
`sandbox_exec_grafana_config_darwin_test.go` — and the `-run` selector is
scoped to match only their `TestSandboxExecGitLabToken_*` and
`TestSandboxExecGrafanaConfig_*` functions. A wider `^TestSandboxExec`
selector was tried first and found (PR #2785, round-2 review) to
deterministically SKIP several other sandbox-exec suites on a bare
`macos-15` runner — for example the playwright suite, because
`playwright-cli` is not installed by this job — which would turn the guard
step (and therefore the required `pr-gate` check) permanently red for a
provisioning gap unrelated to any individual PR's change. Widening the
selector to cover another `TestSandboxExec*` suite is a deliberate,
separate change: audit that suite's own `require*`/skip preconditions
against what a bare `macos-15` runner actually provides (host keychain
state, Xcode CLT, additional Nix-store binaries, and so on), provision
whatever it needs in this job, and only then add its prefix to `-run`.

**No-duplication mechanism.** Every test function in
`sandbox_exec_*_darwin_test.go` is named `TestSandboxExec*`, and no
cross-platform (untagged) test in `internal/integration/` uses that
prefix. So the `-run` selector on macOS matches only darwin sandbox-exec
test functions; every other integration test in the package compiles
(harmless — it's Go) but does not execute, because it never matches the
selector. On the ubuntu `go-tests` job the darwin files do not compile at
all. Each test therefore runs at most once across the whole pipeline. When
adding a new darwin sandbox-exec test that should run in this job, name its
function `TestSandboxExec*` and, once its own preconditions are verified
against a bare `macos-15` runner (see the selector-scope note above), add
its prefix to the `-run` pattern.

**A SKIP must never read as a PASS.** `go test` exits 0 even when every
selected test skips (for example, if the `/var/folders` `TMPDIR` guard in
`requireSandboxExec`/`requireNixBash` trips on a future runner image
change), so a green exit code alone is not sufficient evidence that the
sandbox-exec tests actually ran. The job's guard step parses the `-v`
output and fails the job if:

- any selected test reports SKIP, or
- zero selected tests ran at all — this guards against the `-run`
  selector matching nothing, including future naming drift.

This mirrors the ubuntu `go-tests` job's "Summarise skipped tests" step,
except it fails the job on any SKIP or on a zero-test run rather than only
summarising them — a SKIP in this suite means the coverage this document
exists to guarantee did not run at all.

The job is gated on the same Go-relevant path filter as the ubuntu
`go-tests` job, and its result feeds the `pr-gate` fan-in check like every
other required job.

This job is CI-only test execution. It does not change worker isolation:
it does not touch `config.ValidIsolationModes`, and no worker spawn
default moves to `host` mode.

### Nix-store binary requirement

`requireNixBash` (in `sandbox_exec_helpers_darwin_test.go`, the only
`requireNix*` helper either of the two selected test files calls) resolves
`bash` via `exec.LookPath` + `filepath.EvalSymlinks` and skips the test when
the resolved path is not under `/nix/store/` — Apple-signed and Homebrew
binaries SIGABRT under the deny-default sandbox (see #1190). A bare
`macos-15` runner has no Nix installed and no `/nix/store` binaries on
`PATH` by default, so without the Nix-install step and the `nix shell`
wrapper above, the selected tests would resolve `bash` to `/bin/bash` and
SKIP — which the job's guard step catches and fails on, but the tests still
would not have actually run. This is why the job installs Nix and runs the
tests through `nix shell` rather than relying on whatever `bash` the bare
runner image happens to ship. A future selector widening that pulls in a
suite calling `requireNixGit`, `requireNixAws`, `requireNixKubectl`, or
`requireNixSocat` must add the corresponding `nixpkgs#...` package to this
`nix shell` invocation.

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
