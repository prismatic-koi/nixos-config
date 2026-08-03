---
name: prism-testing
description: |
  Building, testing, and validating prism changes in and out of the nix
  sandbox. Load this skill when you change prism Go source (`modules/programs/
  prism/prism/**`), run its test suite, build `.#prism`, write a test that
  redirects stdout/stderr or exercises `sandbox-exec`, hit a homeless-shelter
  CI failure, debug an in-sandbox `nix build` or flake-CLI failure, or need
  the background story behind the escalate-do-not-work-around rules for the
  sandbox (FD exhaustion, trusted-settings, git stash). The actionable rules
  themselves live in the repo `AGENTS.md`; this skill carries the CI-gate
  detail, the testing conventions, and the incident rationale.
---

# Prism build, test, and sandbox conventions

## The homeless-shelter gate is enforced by CI

The gate runs in CI, not locally. Any PR that touches `modules/programs/prism/prism/**`, `pkgs/prism.nix`, `**/go.mod`, `**/go.sum`, or `.github/workflows/pr-gate.yml` triggers:

- the `go-tests` CI job — runs `go test ./... -race` from `modules/programs/prism/prism/` on a Linux runner with bwrap available; and
- the `nix-build-prism-checked` CI job — builds prism with `runChecks = true` so the test suite executes inside the nix sandbox (`$HOME=/homeless-shelter`).

Both jobs must pass before merge. They live in `.github/workflows/pr-gate.yml`. The required status check on `main` is `pr-gate`, which is a fan-in job that explicitly fails if either `go-tests` or `nix-build-prism-checked` did not succeed — so a failure in either is a hard block.

**Scope.** PRs that touch only non-prism files (other modules, dotfiles, docs) do **not** trigger the `go-tests` or `nix-build-prism-checked` jobs. The relaxation introduced in #1441 stands for those paths.

## Pipeline split (issue #1494)

Go test execution is split from the default `nix build` so that local `nh switch` and `nix build .#prism` are fast:

- `.#prism` — default attribute, `runChecks = false` (so `doCheck = false`). Used by `nixosConfigurations`, `darwinConfigurations`, and local `nh switch`. No Go tests run.
- `pkgs.prism.override { runChecks = true; }` — same derivation with `doCheck = true`. Runs the Go suite inside the nix sandbox so the homeless-shelter signal is preserved. Built by the `nix-build-prism-checked` CI job. Not exposed as a flake output, so `nix flake check` does not pay for the test phase on every PR.

## Local pre-PR self-check (recommended)

Before pushing a prism-touching PR, run the fast build and test first (the repo `AGENTS.md` names those two commands), then add the CI-parity checks:

```bash
# From modules/programs/prism/prism/
gofmt -l .

# From the repo root
nix build .#prism
```

`gofmt -l .` matches the `gofmt check` step in `pr-gate.yml` (added in #2282) — list mode (`-l`) shows what changes without mutating; once you are ready to fix any reported files, re-run with `-w .`. The full homeless-shelter signal is then exercised by CI on the PR.

To reproduce CI's checked build locally:

```bash
# From the repo root
nix build --impure --no-link \
  --expr '(builtins.getFlake (toString ./.)).packages.x86_64-linux.prism.override { runChecks = true; }'
```

The `go-tests` job catches race conditions and integration-test failures the nix sandbox masks (e.g. tests that `t.Skip` when bwrap is unavailable). The `nix-build-prism-checked` job catches the homeless-shelter failure class.

## Test-suite isolation (issue #1608)

The test suite under `modules/programs/prism/prism/internal/sidecar/` is fully isolated from host bus / DB / tmux state:

- Tests that construct a `sidecar.Sidecar` must use `sidecartest.NewIsolated(t, ...)` which redirects `$XDG_STATE_HOME` to a `t.TempDir()` and sets the `PRISM_TEST_MODE_RESTRICT_HOSTAPI` guard.
- Test session names use the `prism-test@` prefix, never `nixos-config@main` or any other slug that matches a live coordinator on the developer's host.
- Running `go test ./...` will not deliver any notification to a live coordinator, write to the real `prism.db`, create files under `$XDG_STATE_HOME/prism/run/`, or invoke `tmux` against the host server.

If you add a new test that exercises notification delivery, use `sidecartest.NewIsolated` — do not construct a `sidecar.Config` that touches the host environment.

## Why the gate exists — the homeless-shelter failure class

The Nix build runs the test suite inside a sandbox where `$HOME=/homeless-shelter`, an intentionally unwritable path. This catches tests that touch the user's actual home directory and pass in a normal dev shell but fail in the sandbox:

- `os.MkdirAll` on a path derived from `$HOME` or an unset `$XDG_STATE_HOME`
- `os.UserHomeDir()` followed by a write
- Opening a Unix socket under `~/.local/state/...`

This is not a hypothetical: PR #1455 (`TestDeliverToSession_PiPath_DeliverAsForwarded`) introduced exactly this failure. `go test ./...` passed, `prism review` passed, the PR merged — and main went red on the next `nh switch` because the test created a directory under `$HOME`. The fix was a one-liner, but the break surfaced only in the Nix sandbox.

## In-sandbox nix validation — trusted-settings background

This repo's `flake.nix` has a `nixConfig` block (`extra-substituters` / `extra-trusted-public-keys`). Any flake-CLI nix command therefore makes nix consult `~/.local/share/nix/trusted-settings.json` in the **real** home (inside a sandbox, `XDG_DATA_HOME` points at the real `~/.local/share`). `--no-accept-flake-config` does NOT avoid the lookup.

As of issue #2201 the sandbox-exec profile grants **read-only** access to that single file, so flake-CLI commands (`nix build .#prism --dry-run`, `nix flake metadata`, the pre-PR `nix build .#prism`, …) work inside worker sandboxes. Notes:

- The allowance applies to sandboxes **spawned after the fix is deployed** (next `nh switch`). In a sandbox pre-dating it, flake CLI fails with `error: opening file "…/trusted-settings.json": Operation not permitted`. The sanctioned fallback (kept in the repo `AGENTS.md`) is the non-flake `nix-instantiate --eval` pattern, which does not process flake `nixConfig` and so sidesteps the read entirely.
- A `warning: ignoring untrusted flake configuration setting` from the flake CLI is harmless — it means the host trust list does not (yet) cover this repo's settings; eval proceeds without the extra substituters.
- Do not try to accept-and-persist flake config from inside a sandbox (e.g. answering an interactive trust prompt with "permanently mark"): the write path to `trusted-settings.json` is still denied, by design.

## When `nix build` fails inside a sandbox — the FD-exhaustion incident

The repo `AGENTS.md` states the rule: escalate via `prism escalate`, and never override `XDG_DATA_HOME`, `NIX_STORE_DIR`, `NIX_DATA_DIR`, or `HOME`. This is why.

Pointing nix's local profile / trust DB / daemon-socket linkage at an empty tempdir forces nix to bootstrap a fresh single-user store, which opens large numbers of file descriptors and adds real pressure to the host-wide FD pool. During the #2180 incident the host had no headroom to absorb that: the root cause was kitty's kitten config watcher recursively kqueue-watching `/nix/store` — one open FD per store entry — which had pre-consumed nearly the entire system-wide `kern.maxfiles` pool (see issue #2198). The env-override retries were marginal extra pressure on an already-exhausted pool, and once `kern.num_files` reaches `kern.maxfiles`, *every* process on the host that calls `open()` fails (Karabiner, Chrome, Finder, the agent's own harness) and recovery requires a reboot. This actually happened — see issue #2180 for the incident retrospective (its causal story is superseded by #2198). The guidance stands regardless of the root cause: escalate, don't work around.

The local `nix build .#prism` is a *pre-PR* check, not the authoritative gate — CI runs the homeless-shelter build (`nix-build-prism-checked`) on every prism-touching PR and that is the build that must be green for merge. A worker can push without a green local build provided the PR description says so.

## Darwin FD-exhaustion defences (#2180 class)

`modules/darwin/sysctls.nix` is the **Layer 2** (host-wide) defence against #2180-class FD exhaustion (parent: #2181). It raises `kern.maxfiles` to 524288 and `kern.maxfilesperproc` to 262144 via a boot-time launchd daemon (`RunAtLoad`, so the values survive reboots — `/etc/sysctl.conf` is not read at boot on modern macOS) paired with an activation script (so the values apply during `darwin-rebuild switch` without a reboot). Both paths are idempotent and never lower a sysctl that is already at or above target. This is headroom only — the root-cause leak is the kitten `/nix/store` watcher, tracked in #2198. Do not raise these values further to absorb that leak.

The **Layer 1** (per-process) defence pairs with it: agents spawned via the bwrap and sandbox-exec exec paths get a bounded `RLIMIT_NOFILE` — defaults soft 8192 / hard 16384, named constants `DefaultAgentMaxOpenFilesSoft` / `DefaultAgentMaxOpenFilesHard` in `internal/config` (issue #2190). The hard cap is kernel-enforced: an agent cannot raise it with `ulimit -n` from inside its sandbox. Tune per-machine via the `agentMaxOpenFilesSoft` / `agentMaxOpenFilesHard` options on `modules/programs/prism/prism-tui.nix` (rendered into config.json as `agent_max_open_files_soft` / `agent_max_open_files_hard`). Host-mode agents are deliberately uncapped and inherit the host's limits. Layer 1 makes the "one agent runs away with FDs" class structurally impossible; it is defence-in-depth, not the #2180 root-cause fix.

## sandbox-exec testing convention

Any change to `internal/container/sandbox_exec.go::generateProfile`, `Manager.PrepareSandboxExec`, or `Manager.PrepareSessionWorkDir` must be paired with a Darwin-only integration test under `internal/integration/` that invokes `/usr/bin/sandbox-exec` against a Nix-built test binary, plus a negative test that mutates the profile to prove the positive is not a no-op. Substring assertions on profile content are necessary but not sufficient. See `modules/programs/prism/prism/docs/sandbox-exec-testing.md` for the full convention and helpers (issue #1192).

## stdout-capture testing convention

Any test helper that redirects `os.Stdout` or `os.Stderr` through an `os.Pipe` must drain the read end concurrently with the function under test — otherwise a single write larger than the kernel pipe buffer (16 pages ≈ 64 KiB on Linux) deadlocks the writer until `go test`'s timeout fires. The `agent-context` JSON output (~69 KiB) is the current worst offender and the reason this gap surfaced. See `modules/programs/prism/prism/docs/stdout-capture-testing.md` for the full convention and the canonical `captureStdout` helper (issue #1798).

## argv-dump redaction convention (`internal/container`)

`bwrapIsolator.BuildArgs` embeds the output of `credentialEnvVars` in the bwrap argv, so a built argv carries live host secrets: `--setenv ANTHROPIC_API_KEY <key>`, `--setenv OPENROUTER_API_KEY <key>`, and `--setenv GITHUB_TOKEN <PAT>`. A test that formats the raw argv with `%v` writes those values to the developer terminal and to the CI log on every failure (issue #2581; observed live on the #2572 worker).

The rule for `internal/container`: no test formats a whole argv or env slice directly. Route every dump through `redactedArgs` (`argv_redact_test.go`), or `container.RedactedArgsForTest` from an external `package container_test` file. This covers sub-slices too — redact first, then slice (`redactedArgs(args)[i:]`), because slicing first can cut a `--setenv NAME VALUE` triple in half and leave the value with no flag in front of it for the helper to key on. The helper masks the VALUE and keeps the NAME (`--setenv GITHUB_TOKEN <redacted>`), leaves every other element — bind triples included — byte-identical, and preserves the length and order of the slice.

The redaction set is derived from the production names, so a credential added once is redacted everywhere without a second edit. `TestRedactedArgs_RedactsEveryCredentialEnvVarsEntry` pins that link. Do not hard-code a second copy in the test helper.

### Where the credential-name list lives (changed in #2589)

The source of truth is **`payload.ForwardedCredentialEnvNames`** in `modules/programs/prism/prism/internal/payload/redact.go`, alongside `payload.GitHubTokenEnvName` and `payload.PrismGitHubTokenEnvPrefix`.

It moved there because `internal/payload` is a stdlib-only leaf package, which is what lets three consumers share one list without an import cycle:

- credential injection — `credentialEnvVars` in `internal/container/credentials.go`;
- the argv redaction that keeps VALUES out of a test failure message (#2581);
- the capture-path redactor that keeps VALUES out of `prism.db` (`payload.Redactor`, #2589).

`credentialForwardEnvKeys`, `githubTokenEnvKey`, and `prismGitHubTokenEnvPrefix` still exist in `credentials.go`, but they are now **derived copies** of the payload names, not lists you edit. Do not convert them back into literals: that breaks the derivation, and a credential added to the literal is still injected into every sandbox while never entering the redaction registry — its value is then captured verbatim into `prism.db`, which is exactly the #2589 leak.

**When you add a credential, edit two places in the same change:**

1. `payload.ForwardedCredentialEnvNames` (or `otherCredentialEnvNames`, for a name prism does not forward but that can still be present in a host-mode agent's environment) in `internal/payload/redact.go`.
2. `CREDENTIAL_ENV_NAMES` in `modules/programs/prism/pi/extensions/prism.ts`, so the pi extension redacts it before the frame reaches the socket.

`TestRedactorParityWithExtension_EnvNameRegistry` fails if you edit only one of the two. See `modules/programs/prism/prism/docs/secret-redaction.md` for the full control.

## The git stash incident (#2202)

The repo `AGENTS.md` states the rule: worker-class sessions never use `git stash`; use a temp commit or a patch file instead. This is why.

In the bare+worktree layout the stash stack (`refs/stash` + its reflog) lives in the shared bare repo, so it is repo-wide, not per-worktree. Two sessions that stash concurrently race on a single LIFO stack — `git stash pop` takes whatever is at `stash@{0}`, which can belong to another worktree. On 2026-06-11 two concurrent workers' pops crossed and silently swapped their WIP (issue #2202). The pi extension's deny list (`BLOCKED_BASH_PATTERNS` in `modules/programs/prism/pi/extensions/prism.ts`) blocks `git stash` for worker-class agents as defence in depth; the coordinator — the single session on the main worktree — is exempt and is then the only prism writer to the shared stack.

The "verify your tests aren't no-ops" discipline (revert fix → rerun test → re-apply) is the common trigger for reaching for a stash — use a temp commit or patch file (see repo `AGENTS.md`) instead.

## The git apply silent-failure pattern (#2575 class incident)

`AGENTS.md` recommends the temp-commit pattern as the default, not the patch-file pattern, and if using patch files, requires checking the result. This is why.

`git apply` is all-or-nothing: if the patch does not apply cleanly to the current tree state, it fails and leaves the tree partially modified — it will not undo the hunks it successfully applied (unlike `git stash pop`, which refuses loudly and keeps the stash entry if any conflict arises). Worse, `git apply` reports failure only on stderr and exit status; it is silent on stdout.

One worker ran the patch-file restore leg with stderr suppressed (`2>/dev/null`), `git apply` failed to apply the patch cleanly, and the tree was left with broken code restored. The failure was caught only because a coinciding guard test for that symbol was present. Without it, the broken code would have reached review or merged.

Safeguards:
- Prefer the temp-commit pattern (`git add -A && git commit -m wip`, then `git reset --soft HEAD~1` to restore). It cannot half-apply — `git reset --soft` is atomic.
- If using patch files, **always** run `git apply --check /tmp/wip.patch` first to verify the patch applies cleanly before running `git apply /tmp/wip.patch`. Never suppress stderr of `git apply` or any tree-restoring command.
- General rule: never run any tree-modifying or tree-restoring command with stderr suppressed (`2>/dev/null`).
