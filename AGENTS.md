# Agents Project Guidance: nixos-config

<!-- doclint-ignore: refs/stash, AllPackages.nix, allPackages.nix, delete_branch_on_merge -->
<!--
  The identifiers above are intentionally unresolvable against the local
  source tree:

  - `refs/stash` is a git-internal reference path (in .git/refs/), not a
    file the repo tracks.
  - `AllPackages.nix` and `allPackages.nix` are deliberate counter-
    examples in the file-naming rule below — they are what the tree
    must NOT contain.
  - `delete_branch_on_merge` is a GitHub repo-level setting name managed
    via the API, not a symbol in any file in this repo.
-->

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

This is faster than a full nix build and must be the first check for any prism code change.

**Building, testing, and CI: load the `prism-testing` skill.** The homeless-shelter CI gate, the pipeline split, test-suite isolation, the pre-PR self-check, the `sandbox-exec` and stdout-capture testing conventions, and the Darwin FD-exhaustion defences all live there. Load it before you change prism Go source or its build.

### In-sandbox nix validation (flake CLI and trusted-settings)

This repo's `flake.nix` has a `nixConfig` block, so any flake-CLI nix command makes nix read `~/.local/share/nix/trusted-settings.json` in the **real** home. When that read is denied inside an older sandbox, use the non-flake fallback, which does not process flake `nixConfig` and so sidesteps the read entirely:

```bash
# From the repo root
nix-instantiate --eval --expr 'builtins.getFlake (toString ./.)'
```

The `prism-testing` skill carries the trusted-settings background and the read-only-allowance history.

### When `nix build` fails inside a sandbox

**If `nix build .#prism` fails inside a worker sandbox, escalate via `prism escalate`. Do not attempt environment workarounds.**

Specifically, do NOT override any of `XDG_DATA_HOME`, `NIX_STORE_DIR`, `NIX_DATA_DIR`, or `HOME` to try to "isolate" or "reset" nix between retries — doing so forces nix to bootstrap a fresh single-user store and adds real pressure to the host-wide FD pool. The `prism-testing` skill records why (the #2180 / #2198 FD-exhaustion incident). The pi extension's deny list (`BLOCKED_BASH_PATTERNS` in `modules/programs/prism/pi/extensions/prism.ts`) also blocks this command shape as defence in depth; the correct response to that block is still `prism escalate`, not a different workaround.

### Setting WIP aside — do not use git stash

**Worker-class prism sessions must never use `git stash`** (any subcommand — `stash -u`, `stash pop`, `stash apply`, `stash list`, …). In the bare+worktree layout the stash stack (`refs/stash` + its reflog) lives in the shared bare repo, so it is repo-wide, not per-worktree. Two sessions that stash concurrently race on a single LIFO stack (see the `prism-testing` skill for the #2202 incident). The pi extension's deny list (`BLOCKED_BASH_PATTERNS` in `modules/programs/prism/pi/extensions/prism.ts`) blocks `git stash` for worker-class agents as defence in depth; the coordinator — the single session on the main worktree — is exempt.

Sanctioned WIP-set-aside patterns — both are worktree-local:

- **Temp commit** (preferred — commit history is disposable on squash-merged branches):

  ```bash
  git add -A && git commit -m wip   # set WIP aside
  # ... do the other thing (e.g. rerun a test against HEAD~1 via a checkout) ...
  git reset --soft HEAD~1           # restore: changes return, staged
  ```

- **Patch file**:

  ```bash
  git diff > /tmp/wip.patch && git restore .   # set WIP aside
  # ... do the other thing ...
  git apply /tmp/wip.patch                     # restore
  ```

### Podman support for workers

A worker spawned with `prism spawn --containers` gets a filtered podman API socket inside its sandbox. The operational guide and security spec live in the `podman-proxy` skill and `modules/programs/prism/prism/docs/podman-proxy.md`.

### File naming and organisation

Names of files and directories must be in lowercase, with dashes between words — kebab case, not camel case.
For instance, it must be `all-packages.nix`, not `allPackages.nix` or `AllPackages.nix`.

### Formatting

All Nix files must be formatted using `nixfmt`:
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

**Branch deletion:** Do not pass `--delete-branch` to `gh pr merge`. Branch deletion after merge is handled automatically by GitHub (`delete_branch_on_merge` is enabled at the repo level). Passing `--delete-branch` can cause an API error if the branch is already gone.

**Build agents:** If you are working on a feature branch, your job ends at "PR opened and pushed". The coordinator on `@main` drives the merge via `prism merge <pr>` — do not attempt to merge the PR yourself, do not enqueue it in the merge queue, and do not wait for it to land before handing off. Once your PR is open and your self-review has passed, you are done.

**Required status checks:** The required status check on `main` is `pr-gate`. Your branch must be up to date with `main` before it can merge. If `gh pr merge` fails because the branch is behind, use `gh pr update-branch <number>` to sync it.

### General Workflow Principles

- **Atomic Changes:** Group all related modifications (e.g., creating a new module, importing it, and removing the old package entry) into a single logical change and commit them together.
- **Git Tracking for Nix:** New files must be added to Git (and ideally committed) *before* Nix commands (like `nix build` or `nix flake check`) can recognize them.
- **Efficiency:** Build commands can be time-consuming. Use them judiciously, only after a complete set of related changes has been applied, and then await user feedback before further iterations. Do not use them as part of an iterative debugging process unless explicitly instructed.
- **Trusting User Feedback:** If the user confirms a fix, trust that feedback and move on, rather than attempting further "fixes" based on assumptions.
