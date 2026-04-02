# Agents Project Guidance: nixos-config

This document provides guidance for AI agents on how to interact with this NixOS configuration repository.

## Application Configuration Lives in This Repo

Many applications — including opencode, prism, zsh, neovim, and others — are configured as Nix modules in this repository rather than having config files in `~/.config/` or other dotfile locations. **Before reading external config files or fetching docs to understand how an application is configured, search this repo first.**

For example:
- opencode configuration → `modules/programs/prism/opencode.nix`
- Custom opencode agents → `modules/programs/prism/opencode/agents/`
- opencode skills → `modules/programs/prism/opencode/skills/`
- Global opencode agent instructions (`~/.config/opencode/AGENTS.md`) → `modules/programs/prism/opencode.nix` (the `agentInstructions` string, rendered via `programs.opencode.rules`) — **never edit the file at `~/.config` directly, it is overwritten on every switch**

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

This is faster than a full nix build and should be the first check for any prism code change. Run the nix build as well if the change also touches `.nix` files.

## Platform-Aware Commands

This repo targets both NixOS and Darwin. Use the appropriate build and switch commands for your platform. Detect your platform with `uname -s` if unsure (`Linux` = NixOS, `Darwin` = macOS).

| | NixOS | Darwin |
|---|---|---|
| Build | `nix build .#nixosConfigurations.navi.config.system.build.toplevel` | `nix build .#darwinConfigurations.m4mac.config.system.build.toplevel` |
| Switch | `sudo nixos-rebuild switch --flake .` | `sudo darwin-rebuild switch --flake .` |

## Common Commands

- **Building/Testing Changes:** To validate configuration changes without applying them, use the platform-appropriate build command from the table above. Avoid `nh` for builds: it produces animated progress output that renders poorly in non-interactive environments.
- **Linting/Formatting:** Code is formatted with `nixfmt`.
    - `nixfmt .`
- **Flake Validation:** To check the flake for correctness across all defined systems, use:
    - `nix flake check --all-systems`
    - Note: this is slow and covers all systems. Do not run it routinely — reserve it for before major releases or flake input updates.
- **Updating Inputs:** To update all flake inputs, use:
    - `nix flake update`
- **Applying Configuration:** The user will typically handle applying the configuration manually. Do not attempt to apply changes unless explicitly asked. Use the platform-appropriate switch command from the table above.
- **Editing Secrets:** Secrets are encrypted with `sops`. To edit a secret, use the `sops` command.
    - `sops <path/to/secret.sops.yaml>`

## Code Structure & Conventions

### Adding New Packages/Applications

1.  **Machine-Specific (Limited Use):** If a package is needed on only one machine, add it directly to that machine's `configuration.nix` (e.g., `machines/navi/configuration.nix`).
2.  **Global (Simple Package):** If a package should be available on all machines and requires no special configuration, add it to the main list in `modules/programs/default.nix`.
3.  **Global (With Configuration):** For applications that require configuration, files, or persisted state:
    - Create a new module file (e.g., `modules/programs/new-app.nix`).
    - In this file, define the necessary NixOS and/or Home Manager options.
    - Import the new module into `modules/programs/default.nix`.
    - Enable the module where needed (e.g., in a specific machine's configuration).

### Adding New Services

- Follow the same pattern as for applications with configuration, but place the new module within the `modules/services/` directory.

### File naming and organisation

Names of files and directories should be in lowercase, with dashes between words — kebab case, not camel case.
For instance, it should be `all-packages.nix`, not `allPackages.nix` or `AllPackages.nix`.

### Formatting

All Nix files should be formatted using `nixfmt`:
```
nixfmt .
```

### Syntax

- Set up [editorconfig](https://editorconfig.org) for your editor, such that [the settings](./.editorconfig) are automatically applied.

- Use `lowerCamelCase` for variable names, not `UpperCamelCase`.
  Note, this rule does not apply to package attribute names, which instead follow the rules in [package naming](./pkgs/README.md#package-naming).

- Functions should list their expected arguments as precisely as possible.
  That is, write

  ```nix
  {
    stdenv,
    fetchurl,
    perl,
  }:
  <...>
  ```

  instead of

  ```nix
  args: with args; <...>
  ```

  **Important exception: NixOS modules must use `...`**

  NixOS modules (files that define `options` and `config` sections) require `...` because the module system passes additional arguments automatically. For modules, use:

  ```nix
  {
    config,
    lib,
    pkgs,
    ...
  }:
  {
    options = { ... };
    config = { ... };
  }
  ```

  Only remove `...` from simple package definitions or pure configuration files that are not part of the NixOS module system.

  For functions that are truly generic in the number of arguments, but have some required arguments, you should write them using an `@`-pattern:

  ```nix
  {
    stdenv,
    doCoverageAnalysis ? false,
    ...
  }@args:

  stdenv.mkDerivation (args // { foo = if doCoverageAnalysis then "bla" else ""; })
  ```

  instead of

  ```nix
  args:

  args.stdenv.mkDerivation (
    args
    // {
      foo = if args ? doCoverageAnalysis && args.doCoverageAnalysis then "bla" else "";
    }
  )
  ```

- Unnecessary string conversions should be avoided.
  Do

  ```nix
  { rev = version; }
  ```

  instead of

  ```nix
  { rev = "${version}"; }
  ```

- Building lists conditionally _should_ be done with `lib.optional(s)` instead of using `if cond then [ ... ] else null` or `if cond then [ ... ] else [ ]`.

  ```nix
  { buildInputs = lib.optional stdenv.hostPlatform.isDarwin iconv; }
  ```

  instead of

  ```nix
  { buildInputs = if stdenv.hostPlatform.isDarwin then [ iconv ] else null; }
  ```

  As an exception, an explicit conditional expression with null can be used when fixing a important bug without triggering a mass rebuild.
  If this is done a follow up pull request _should_ be created to change the code to `lib.optional(s)`.

### Secrets Management

- Secrets are co-located with the modules that use them (e.g., `modules/qutebrowser/secrets/`).
- The public age key for encryption is located in the root `.sops.yaml` file. Do not ask for this key.
- When adding a new secret, create a new `.sops.yaml` file in the appropriate module directory.

## Workflows

### PR workflow (build agents on branches)

If the change touches prism Go source, run the Go build and tests first — see the [Prism](#prism) section for details.

After committing and before opening a PR, run the platform-appropriate build command (see [Platform-Aware Commands](#platform-aware-commands)) to verify the configuration builds. New files must be `git add`-ed first or nix will not see them.

If the build fails, fix it before opening the PR. A PR with a broken nix build should never be opened.

Do NOT run the switch command on branches — that is a main-only operation.

### Post-merge workflow (coordinator on main)

After merging a PR to main, always run the platform-appropriate build command (see [Platform-Aware Commands](#platform-aware-commands)) to verify the merged result builds.

If the change affects system state (packages, services, activation scripts, module options, overlays): run the platform-appropriate switch command to apply it.

If the change is limited to non-system files (opencode agents, opencode skills, Go source in prism, documentation): skip the switch — these do not affect the NixOS system.

### GitHub repository rules

Direct push to `main` is blocked by the repository ruleset. All changes must go through a pull request. Never attempt to push directly to main.

**Merge method:** The only allowed merge method is squash merge. Coordinators merging a PR must use:

```bash
gh pr merge <number> --squash
```

Never use `--merge` (creates a merge commit, rejected by the ruleset) or `--rebase` (creates individual commits rather than a squash, also rejected by the ruleset).

**Branch deletion:** Do not pass `--delete-branch` to `gh pr merge`. Branch deletion after merge is handled automatically by GitHub (`delete_branch_on_merge` is enabled at the repo level). Passing `--delete-branch` may cause an API error if the branch is already gone.

**Build agents:** If you are working on a feature branch, open the PR with `gh pr create` and do not attempt to merge it. The coordinator on `@main` handles merging.

### Temporary Testing Changes

The user may request changes for testing purposes that should not be committed. In these cases, modify the necessary files and run the platform-appropriate switch command to apply the changes, but do not stage or commit them.

### General Workflow Principles

- **Atomic Changes:** Group all related modifications (e.g., creating a new module, importing it, and removing the old package entry) into a single logical change and commit them together.
- **Git Tracking for Nix:** New files must be added to Git (and ideally committed) *before* Nix commands (like `nix build` or `nix flake check`) can recognize them.
- **Efficiency:** Build commands can be time-consuming. Use them judiciously, only after a complete set of related changes has been applied, and then await user feedback before further iterations. Do not use them as part of an iterative debugging process unless explicitly instructed.
- **Trusting User Feedback:** If the user confirms a fix, trust that feedback and move on, rather than attempting further "fixes" based on assumptions.

## Landing the Plane (Session Completion)

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
