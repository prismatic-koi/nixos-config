# Agents Project Guidance: nixos-config

This document provides guidance for AI agents on how to interact with this NixOS configuration repository.

## Application Configuration Lives in This Repo

Many applications — including opencode, prism, zsh, neovim, and others — are configured as Nix modules in this repository rather than having config files in `~/.config/` or other dotfile locations. **Before reading external config files or fetching docs to understand how an application is configured, search this repo first.**

For example:
- opencode configuration → `modules/programs/prism/opencode.nix`
- Custom opencode agents → `modules/programs/prism/opencode/agents/`
- opencode skills → `modules/programs/prism/opencode/skills/`
- Global opencode agent instructions (`~/.config/opencode/AGENTS.md`) → `modules/programs/prism/opencode.nix` (the `agentInstructions` string, rendered via `programs.opencode.context`) — **never edit the file at `~/.config` directly, it is overwritten on every switch**

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
- **Opencode configuration**: `modules/programs/prism/opencode.nix`
- **Custom agents**: `modules/programs/prism/opencode/agents/`
- **Skills**: `modules/programs/prism/opencode/skills/`

When making changes to prism Go source, always build and test before committing:

```bash
# From modules/programs/prism/prism/
go build ./...
go test ./...
```

This is faster than a full nix build and should be the first check for any prism code change. Once the Go build and tests pass, verify the Nix derivation with:

```bash
nix build .#prism
```

Run `nix build .#prism` from the repo root whenever a change also touches `.nix` files or when confirming the final Nix build is correct.

### sandbox-exec testing convention

Any change to `internal/container/sandbox_exec.go::generateProfile`,
`Manager.PrepareSandboxExec`, or `Manager.PrepareSandboxExecHome` must be paired
with a Darwin-only integration test under `internal/integration/` that invokes
`/usr/bin/sandbox-exec` against a Nix-built test binary, plus a negative test
that mutates the profile to prove the positive is not a no-op. Substring
assertions on profile content are necessary but not sufficient. See
`modules/programs/prism/prism/docs/sandbox-exec-testing.md` for the full
convention and helpers (issue #1192).

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
