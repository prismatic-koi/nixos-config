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
      home-manager.users.${config.nx.username} = {
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
        home.persistence."/persist" = {
          directories = [
            ".claude"
            ".config/claude"
            ".local/share/claude"
            ".local/state/claude"
          ];
          files = [
            ".claude.json"
          ];
        };
      };
    }
  );
}
