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
      jsonFormat = pkgs.formats.json { };
      # claude-code settings, rendered to the XDG config dir below
      # (~/.config/claude/settings.json — NOT the upstream home-manager
      # module's hardcoded ~/.claude/settings.json target; see the
      # CLAUDE_CONFIG_DIR note further down). The $schema key mirrors what
      # programs.claude-code.settings would have emitted.
      claudeSettings = {
        "$schema" = "https://json.schemastore.org/claude-code-settings.json";
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
    in
    {
      home-manager.users.${config.nx.username} =
        { lib, ... }:
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

          # CLAUDE_CONFIG_DIR relocates claude-code's config dir AND
          # ~/.claude.json to the XDG path (verified effective in claude-code
          # 2.1.161). Set host-wide here and delivered into prism sandboxes
          # via nx.programs.prism.agent.envVars (issue #2243, Step 3c of the
          # #2132 staging-HOME elimination design).
          #
          # Residual risk, accepted by design (#2132 §4 Step 3c): a claude
          # launch that does not source the home-manager session vars (e.g. a
          # bare login shell without hm integration, or a GUI process) falls
          # back to ~/.claude and forks its state from the migrated copy.
          # CLI-only usage on these machines makes this low — documented, not
          # engineered around.
          home.sessionVariables.CLAUDE_CONFIG_DIR = "$HOME/.config/claude";

          programs.claude-code = {
            # Installs the claude-code package only. Settings deliberately NOT
            # set via programs.claude-code.settings: the upstream home-manager
            # module hardcodes its settings.json target to ~/.claude/, which
            # CLAUDE_CONFIG_DIR has relocated away from. The settings file is
            # rendered at the XDG path via xdg.configFile below instead.
            enable = true;
          };
          xdg.configFile."claude/settings.json".source =
            jsonFormat.generate "claude-code-settings.json" claudeSettings;

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
