# Agents Project Guidance: nixos-config

This document provides guidance for AI agents on how to interact with this NixOS configuration repository.

## Application Configuration Lives in This Repo

Many applications — including prism, zsh, neovim, and others — are configured as Nix modules in this repository rather than having config files in `~/.config/` or other dotfile locations. **Before reading external config files or fetching docs to understand how an application is configured, search this repo first.**

For example:
- pi (agent runtime) configuration → `modules/programs/prism/pi.nix`
- Custom agent markdown files → `modules/programs/prism/agents/`
- Skills → `modules/programs/prism/skills/`
- Agent files at `~/.config/prism/agents/` and skills at `~/.config/prism/skills/` — **never edit these directly, they are overwritten on every switch**

If you need to understand how something is configured, `grep` or `glob` within the working directory before reaching outside it.

## Project Overview

- **Primary Goal:** This repository manages the personal NixOS configurations for multiple machines, all intended for a single user (`prismatic-koi`).
- **Design Philosophy:**
    - Configurations are managed with Nix Flakes.
    - Home Manager and NixOS options are often configured together within the same module for simplicity.
    - The system aims for impermanence, with state managed via `impermanence`.
    - Secrets are managed with `sops-nix` and an age key.
    - The `unstable` channel is preferred for packages. Overlays are used to pin packages to `stable` or other versions only when necessary.

## Prism

Prism is a tmux-based AI development environment that is developed and configured within this repository. Its source code and configuration are located under `modules/programs/prism/`:

- **Go CLI source**: `modules/programs/prism/prism/` — the `prism` binary (spawn, checkin, prompt, dashboard, etc.)
- **Tmux configuration**: `modules/programs/prism/tmux.nix`
- **Pi agent configuration**: `modules/programs/prism/pi.nix`
- **Custom agents**: `modules/programs/prism/agents/`
- **Skills**: `modules/programs/prism/skills/`

**Isolation modes.** The valid prism isolation modes are `bwrap` (Linux), `sandbox-exec` (Darwin), and `host` — the source of truth is `config.ValidIsolationModes` in `modules/programs/prism/prism/internal/config/config.go`. Podman is **not** a prism isolation mode; it was removed some time ago (issue #2189 cleaned up the stale references). The only remaining `podman` strings in prism source are legacy DB-row fallbacks (and the schema migrations that backfill them) which convert old `isolation_mode='podman'` rows to bwrap at read time — do not reintroduce podman as a mode, and do not "fix" those fallbacks without a dedicated audit. (Podman the container runtime is still used on these machines — `modules/programs/podman.nix` — that is unrelated to prism isolation.)

When making changes to prism Go source, always build and test before committing:

```bash
# From modules/programs/prism/prism/
go build ./...
go test ./...
```

This is faster than a full nix build and should be the first check for any prism code change.

**Prism Go-source and `.nix` changes: the homeless-shelter gate is enforced by CI.**

The gate now runs in CI, not locally. Any PR that touches `modules/programs/prism/prism/**`, `pkgs/prism.nix`, `**/go.mod`, `**/go.sum`, or `.github/workflows/pr-gate.yml` triggers:

- the `go-tests` CI job — runs `go test ./... -race` from `modules/programs/prism/prism/` on a Linux runner with bwrap available; and
- the `nix-build-prism-checked` CI job — builds prism with `runChecks = true` so the test suite executes inside the nix sandbox (`$HOME=/homeless-shelter`).

Both jobs must pass before merge. They live in `.github/workflows/pr-gate.yml`. The required status check on `main` is `pr-gate`, which is a fan-in job that explicitly fails if either `go-tests` or `nix-build-prism-checked` did not succeed — so a failure in either is a hard block.

**Pipeline split (issue #1494).** Go test execution is split from the default `nix build` so that local `nh switch` and `nix build .#prism` are fast:

- `.#prism` — default attribute, `runChecks = false` (so `doCheck = false`). Used by `nixosConfigurations`, `darwinConfigurations`, and local `nh switch`. No Go tests run.
- `pkgs.prism.override { runChecks = true; }` — same derivation with `doCheck = true`. Runs the Go suite inside the nix sandbox so the homeless-shelter signal is preserved. Built by the `nix-build-prism-checked` CI job. Not exposed as a flake output, so `nix flake check` does not pay for the test phase on every PR.

**Local pre-PR self-check (recommended).** Before pushing a prism-touching PR, run:

```bash
# From modules/programs/prism/prism/
go build ./...
go test ./...

# From the repo root
nix build .#prism
```

This catches build/test failures fast. The full homeless-shelter signal is then exercised by CI on the PR.

**Test-suite isolation (issue #1608).** The test suite under `modules/programs/prism/prism/internal/sidecar/` is fully isolated from host bus / DB / tmux state:

- Tests that construct a `sidecar.Sidecar` must use `sidecartest.NewIsolated(t, ...)` which redirects `$XDG_STATE_HOME` to a `t.TempDir()` and sets the `PRISM_TEST_MODE_RESTRICT_HOSTAPI` guard.
- Test session names use the `prism-test@` prefix, never `nixos-config@main` or any other slug that matches a live coordinator on the developer's host.
- Running `go test ./...` will not deliver any notification to a live coordinator, write to the real `prism.db`, create files under `$XDG_STATE_HOME/prism/run/`, or invoke `tmux` against the host server.

If you add a new test that exercises notification delivery, use `sidecartest.NewIsolated` — do not construct a `sidecar.Config` that touches the host environment. If you want to reproduce CI's checked build locally:

```bash
# From the repo root
nix build --impure --no-link \
  --expr '(builtins.getFlake (toString ./.)).packages.x86_64-linux.prism.override { runChecks = true; }'
```

The `go-tests` job catches race conditions and integration-test failures the nix sandbox masks (e.g. tests that `t.Skip` when bwrap is unavailable). The `nix-build-prism-checked` job catches the homeless-shelter failure class.

**Why this gate exists — the homeless-shelter failure class.** The Nix build runs the test suite inside a sandbox where `$HOME=/homeless-shelter`, an intentionally unwritable path. This catches tests that touch the user's actual home directory and pass in a normal dev shell but fail in the sandbox:

- `os.MkdirAll` on a path derived from `$HOME` or an unset `$XDG_STATE_HOME`
- `os.UserHomeDir()` followed by a write
- Opening a Unix socket under `~/.local/state/...`

This is not a hypothetical: PR #1455 (`TestDeliverToSession_PiPath_DeliverAsForwarded`) introduced exactly this failure. `go test ./...` passed, `prism review` passed, the PR merged — and main went red on the next `nh switch` because the test created a directory under `$HOME`. The fix was a one-liner, but the break surfaced only in the Nix sandbox.

**Scope — this gate applies only to prism-touching PRs.** PRs that touch only non-prism files (other modules, dotfiles, docs) do **not** trigger the `go-tests` or `nix-build-prism-checked` jobs. The relaxation introduced in #1441 stands for those paths.

### When `nix build` fails inside a sandbox

The local `nix build .#prism` is a *pre-PR* check, not the authoritative
gate — CI runs the homeless-shelter build (`nix-build-prism-checked`) on
every prism-touching PR and that is the build that must be green for merge.
A worker MAY push without a green local build provided the PR description
says so.

**If `nix build .#prism` fails inside a worker sandbox, escalate via
`prism escalate`. Do not attempt environment workarounds.**

Specifically, do NOT override any of `XDG_DATA_HOME`, `NIX_STORE_DIR`,
`NIX_DATA_DIR`, or `HOME` to try to "isolate" or "reset" nix between
retries. Pointing nix's local profile / trust DB / daemon-socket linkage at
an empty tempdir forces nix to bootstrap a fresh single-user store, and
inside sandbox-exec (no host nix-daemon access) the parallel evaluation
leaks file descriptors that the next retry inherits. On Darwin this
exhausts the system-wide `kern.maxfiles` cap; once hit, *every* process on
the host that calls `open()` fails (Karabiner, Chrome, Finder, the agent's
own harness), and recovery requires a reboot. This actually happened — see
issue #2180 for the post-incident writeup.

The pi extension's pre-tool-call deny list (`BLOCKED_BASH_PATTERNS` in
`modules/programs/prism/pi/extensions/prism.ts`) also blocks this command
shape as defence in depth; if you see that block fire, the correct response
is still `prism escalate`, not a different workaround.

### sandbox-exec testing convention

Any change to `internal/container/sandbox_exec.go::generateProfile`,
`Manager.PrepareSandboxExec`, or `Manager.PrepareSandboxExecHome` must be paired
with a Darwin-only integration test under `internal/integration/` that invokes
`/usr/bin/sandbox-exec` against a Nix-built test binary, plus a negative test
that mutates the profile to prove the positive is not a no-op. Substring
assertions on profile content are necessary but not sufficient. See
`modules/programs/prism/prism/docs/sandbox-exec-testing.md` for the full
convention and helpers (issue #1192).

### stdout-capture testing convention

Any test helper that redirects `os.Stdout` or `os.Stderr` through an
`os.Pipe` must drain the read end concurrently with the function under test —
otherwise a single write larger than the kernel pipe buffer (16 pages ≈ 64 KiB
on Linux) deadlocks the writer until `go test`'s timeout fires. The
`agent-context` JSON output (~69 KiB) is the current worst offender and the
reason this gap surfaced. See
`modules/programs/prism/prism/docs/stdout-capture-testing.md` for the full
convention and the canonical `captureStdout` helper (issue #1798).

### File naming and organisation

Names of files and directories should be in lowercase, with dashes between words — kebab case, not camel case.
For instance, it should be `all-packages.nix`, not `allPackages.nix` or `AllPackages.nix`.

### Formatting

All Nix files should be formatted using `nixfmt`:
```
nixfmt .
```

### Secrets Management

- Secrets are co-located with the modules that use them (e.g., `modules/qutebrowser/secrets/`).
- The public age key for encryption is located in the root `.sops.yaml` file. Do not ask for this key.
- When adding a new secret, create a new `.sops.yaml` file in the appropriate module directory.

### GitHub repository rules

Direct push to `main` is blocked by the repository ruleset. All changes must go through a pull request. Never attempt to push directly to main.

**Merge method:** The only allowed merge method is squash merge. Coordinators merging a PR must use:

```bash
gh pr merge <number> --squash
```

Never use `--merge` (creates a merge commit, rejected by the ruleset) or `--rebase` (creates individual commits rather than a squash, also rejected by the ruleset).

**Branch deletion:** Do not pass `--delete-branch` to `gh pr merge`. Branch deletion after merge is handled automatically by GitHub (`delete_branch_on_merge` is enabled at the repo level). Passing `--delete-branch` may cause an API error if the branch is already gone.

**Build agents:** If you are working on a feature branch, open the PR with `gh pr create` and do not attempt to merge it. The coordinator on `@main` handles merging.

### General Workflow Principles

- **Atomic Changes:** Group all related modifications (e.g., creating a new module, importing it, and removing the old package entry) into a single logical change and commit them together.
- **Git Tracking for Nix:** New files must be added to Git (and ideally committed) *before* Nix commands (like `nix build` or `nix flake check`) can recognize them.
- **Efficiency:** Build commands can be time-consuming. Use them judiciously, only after a complete set of related changes has been applied, and then await user feedback before further iterations. Do not use them as part of an iterative debugging process unless explicitly instructed.
- **Trusting User Feedback:** If the user confirms a fix, trust that feedback and move on, rather than attempting further "fixes" based on assumptions.
