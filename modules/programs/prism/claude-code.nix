{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.prism.claude-code.enable = lib.mkEnableOption "enables claude-code" // {
      default = false;
    };
  };
  config = lib.mkIf config.nx.programs.prism.claude-code.enable (
    let
      # Define claude-code environment variables in one place
      claudeCodeEnvVars = {
        KUBECONFIG = "$HOME/.config/kube/agents-config";
      };
      # Build shell command prefix from env vars
      envPrefix = lib.concatStringsSep " " (
        lib.mapAttrsToList (name: value: "${name}=${value}") claudeCodeEnvVars
      );
    in
    {
      home-manager.users.${config.nx.username} =
        { lib, config, ... }:
        {
          programs.zsh.shellAliases = {
            # set environment variables for claude-code
            claude-code = "${envPrefix} claude-code";
          };
          # programs.neovim.extraLuaConfig =
          #   lib.mkAfter
          #     # lua
          #     ''
          #       -- open current project in new kitty window with claude-code
          #       vim.keymap.set(
          #         "n",
          #         "<leader>ca",
          #         ":!kitty -d $(pwd) env ${envPrefix} claude-code . &<CR><CR>",
          #         { silent = true, desc = "[C]laude code with [A]I agent" }
          #       )
          #     '';

          programs.claude-code = {
            enable = true;
            # Relocates claude-code's config dir AND ~/.claude.json to the XDG
            # path (verified effective in claude-code 2.1.161). The upstream
            # home-manager module renders settings.json into this dir and
            # auto-exports CLAUDE_CONFIG_DIR via home.sessionVariables
            # whenever it differs from the ~/.claude default — no hand-rolled
            # env var or settings-file emission needed here. The value is
            # delivered into prism sandboxes via nx.programs.prism.agent.envVars
            # (issue #2243, Step 3c of the #2132 staging-HOME elimination
            # design); keep this literal path in agreement with agent.envVars
            # (default.nix), the prism SBPL grant / bwrap mount, and the
            # migration destination below.
            #
            # Residual risk, accepted by design (#2132 §4 Step 3c): a claude
            # launch that does not source the home-manager session vars (e.g.
            # a bare login shell without hm integration, or a GUI process)
            # falls back to ~/.claude and forks its state from the migrated
            # copy. CLI-only usage on these machines makes this low —
            # documented, not engineered around.
            configDir = "${config.home.homeDirectory}/.config/claude";
            settings = {
              permissions = {
                defaultMode = "acceptEdits";
                allow = [
                  # Kubernetes tools
                  "Bash(flux:*)"
                  "Bash(helm:*)"
                  "Bash(kubectl:*)"
                  # nix commands
                  "Bash(nix build:*)"
                  "Bash(nixfmt:*)"
                ];
              };
            };
          };

          # One-time idempotent migration of pre-#2243 claude state
          # (~/.claude/{history.jsonl,projects,plugins,telemetry,backups} and
          # ~/.claude.json) to ~/.config/claude. Runs in the same activation
          # generation that delivers CLAUDE_CONFIG_DIR, so the flip is atomic
          # at switch time. Absent sources and already-migrated state are
          # no-ops; pre-existing destination entries are skipped with a
          # warning, never overwritten. Logic lives in claude-xdg-migrate.sh
          # (covered by claude_xdg_migration_script_test.go in the prism Go
          # tree). `run` honours home-manager's dry-run mode.
          home.activation.claudeXdgMigrate = lib.hm.dag.entryAfter [ "writeBoundary" ] ''
            run ${pkgs.bash}/bin/bash ${./claude-xdg-migrate.sh} \
              "$HOME/.claude" "$HOME/.claude.json" "$HOME/.config/claude"
          '';

          home.persistence."/persist" = {
            directories = [
              # ~/.claude stays persisted post-#2243: it is the migration
              # source on first activation after the flip, and live sessions
              # spawned pre-switch (plus no-session-vars fallback launches)
              # may still write stale state there.
              ".claude"
              ".config/claude"
              ".local/share/claude"
              ".local/state/claude"
            ];
            files = [
              # Kept post-#2243 for the same reason as ~/.claude above. On
              # impermanence hosts this entry is a symlink into /persist; the
              # migration moves the symlink (content stays in /persist, shared
              # by both paths), and the recreated link is then skipped by the
              # idempotent migration on later activations.
              ".claude.json"
            ];
          };
        };
    }
  );
}
