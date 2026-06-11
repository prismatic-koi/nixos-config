# sandbox-exec testing convention

This document codifies the testing convention for changes to the SBPL profile
generator under `internal/container/sandbox_exec.go`. It exists because four
PRs (#1015, #1016, #1017, #1018) collectively shipped a non-functional
sandbox-exec profile (the `/etc` symlink-resolution gap from #1187), and every
unit test in `sandbox_exec_test.go` was green throughout — those tests asserted
profile string content (substring checks) but never invoked
`/usr/bin/sandbox-exec` against the generated profile to confirm it actually
loaded and ran a binary.

This issue is tracked in #1192.

## The convention

> Any change to `internal/container/sandbox_exec.go::generateProfile`,
> `Manager.PrepareSandboxExec`, or `Manager.PrepareSandboxExecHome` must be
> accompanied by at least one Darwin-only integration test under
> `internal/integration/` that:
>
> 1. Generates the profile via `Manager.PrepareSandboxExec()` (not by
>    hand-writing SBPL text).
> 2. Invokes `/usr/bin/sandbox-exec -f <profile>` against a Nix-built
>    ad-hoc-signed test binary (e.g. `bash`, `cat`, `opencode` itself)
>    resolved to its absolute `/nix/store/...` path via `exec.LookPath` +
>    `filepath.EvalSymlinks`.
> 3. Asserts the binary exits 0.
> 4. Has a paired negative test that confirms the test catches regressions:
>    temporarily mutate the profile (e.g. remove a key allow rule) and assert
>    the binary fails — proving the test is not a no-op.
>
> Apple-signed system binaries (`/bin/echo`, `/usr/bin/uname`, etc.) must
> NOT be used as test targets until the v3 profile migration tracked in
> #1190 / the v3 spike lands. They SIGABRT in dyld under the current profile
> shape, which is a separate known limitation.

String-level substring assertions on the generated profile are necessary but
**not sufficient**. Every positive integration test must have a paired
negative test that mutates the profile and asserts failure, proving the
positive test is not green by accident.

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
a future PR if we don't enshrine this convention.

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
  for positive tests that resolve symlink targets under HOME (e.g.
  credential reads through `$HOME/.aws/credentials` → host path).
- `augmentProfileForTest(profile)` — appends the minimum SBPL extras a
  Nix-built binary needs to start under the sandbox during testing (root
  inode literal, `/dev`, `/private/tmp` write). These are testing
  infrastructure only — they are NOT added to the production profile.
- `withMutatedProfile(t, m, mutate)` — generates the profile via
  `Manager.PrepareSandboxExec()`, applies `mutate(string) string` to the
  profile content (e.g. remove a specific allow rule), augments it for
  testing, writes it to a temp file, and returns the path. Used in
  negative-case integration tests to confirm that removing a specific allow
  rule causes the test operation to fail.

## Negative test pattern

Every positive integration test must have a paired negative test. The
negative test mutates the profile to remove the rule under test, then runs
the same operation, and asserts failure. This proves the positive test is
not green by accident.

The `withMutatedProfile` helper exists to make this pattern easy to write.
The pattern below mirrors the actual
`TestSandboxExecProfile_StagingHomeWriteDenied` in
`internal/integration/sandbox_exec_staging_home_darwin_test.go`:

```go
func TestSandboxExecProfile_StagingHomeWriteDenied(t *testing.T) {
    if runtime.GOOS != "darwin" {
        t.Skip("sandbox-exec is Darwin-only")
    }
    requireSandboxExec(t)
    nixBash := requireNixBash(t)

    m := newProfileManager(t)
    stagingHome, err := m.PrepareSandboxExecHome()
    if err != nil {
        t.Fatalf("PrepareSandboxExecHome: %v", err)
    }
    t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

    // Remove only the indented (subpath "<stagingHome>") line from the
    // staging-HOME write allow block, leaving the rest of the block
    // (worktree, bare repo, opencode shared dirs) intact so the profile
    // remains syntactically valid SBPL. The only behaviour change is
    // that writes to <stagingHome> are no longer covered by any allow.
    //
    // Removing the entire (allow ...) block instead would leave orphaned
    // (subpath ...) lines from the trailing entries — sandbox-exec would
    // reject the malformed profile at parse time and the negative test
    // would pass for the wrong reason.
    stagingHomeRule := "  (subpath " + sbplQuoteForTest(stagingHome) + ")\n"
    profilePath := withMutatedProfile(t, m, func(p string) string {
        return strings.ReplaceAll(p, stagingHomeRule, "")
    })

    // Attempt to write a file inside the staging HOME. Without the
    // staging-HOME write allow, the operation must fail.
    target := filepath.Join(stagingHome, "negative-test.tmp")
    cmd := exec.Command(sandboxExecPath, "-f", profilePath,
        nixBash, "-c", "echo hi > "+shQuote(target))
    out, runErr := cmd.CombinedOutput()
    if runErr == nil {
        t.Errorf("write to staging HOME succeeded WITHOUT the staging-HOME write allow: %s", out)
    }
}
```

## Coverage goals

In addition to the system-paths allow set (covered first by #1187 / #1192),
integration test coverage exists for:

- **Staging HOME writes** — the worktree, bare repo, host-API socket dir,
  opencode shared SQLite directory.
- **Staging-HOME credential reads** — symlink targets resolved at staging
  time (e.g. `~/.config/aws/readonly-config`, the SSH access key) accessed
  via the in-sandbox `$HOME` path.
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
- **Network egress** — `(allow network*)` permits outbound TCP.

Each positive case has a paired negative case that mutates the profile to
remove the specific rule under test and asserts the operation fails.

## When the convention applies

Apply this convention to any change that touches:

- `internal/container/sandbox_exec.go::generateProfile` (any rule add/remove
  or precedence change)
- `Manager.PrepareSandboxExec` (the wrapper that writes the profile and
  builds args)
- `Manager.PrepareSandboxExecHome` (staging HOME layout — affects which
  symlink targets the profile resolves)

Pure refactors that do not change the generated SBPL output are exempt — but
the integration tests must continue to pass on the refactored code, which is
the load-bearing check.

## Out of scope

- Migrating the profile to `(version 3)` — separate spike in #1190.
- Test convention work for other isolation modes (bwrap). If
  symmetric work is warranted there, file a separate issue.

## References

- #1015, #1016, #1017, #1018 — the four PRs whose test gaps motivated this
  convention.
- #1187 — first integration test of the convention; landed the seed pattern.
- #1190 — the v3 profile migration that this convention will continue to
  apply through.
- #1192 — this issue: codifies the convention and backfills coverage.
