{
  config,
  lib,
  pkgs,
  ...
}:
{
  options = {
    nx.programs.prism = {
      enable = lib.mkEnableOption "enables prism development environment" // {
        default = true;
      };

      # Shared configuration options
      agent = {
        envVars = lib.mkOption {
          type = lib.types.attrsOf lib.types.str;
          default = {
            KUBECONFIG = "$HOME/.config/kube/agents-config";
          };
          description = "Environment variables to set for the AI agent (opencode)";
        };
      };

      sessioniser = {
        windows = lib.mkOption {
          type = lib.types.listOf (
            lib.types.submodule {
              options = {
                index = lib.mkOption {
                  type = lib.types.int;
                  description = "Window index";
                };
                name = lib.mkOption {
                  type = lib.types.str;
                  description = "Window name";
                };
                command = lib.mkOption {
                  type = lib.types.nullOr lib.types.str;
                  default = null;
                  description = "Command to run in the window (null for default shell)";
                };
              };
            }
          );
          default = [
            {
              index = 0;
              name = "edit";
              command = null;
            } # nvim launched by sessioniser logic
            {
              index = 1;
              name = "agent";
              command = "agent";
            } # special marker, expanded with env vars
            {
              index = 2;
              name = "term";
              command = null;
            } # plain shell
          ];
          description = "Default windows to create in each project session";
        };
      };

      # Internal computed values for submodules to use
      _internal = lib.mkOption {
        type = lib.types.attrs;
        internal = true;
        description = "Internal computed values, do not set directly";
      };
    };
  };

  imports = [
    ./neovim
    ./opencode.nix
    ./tmux.nix
    ./sessioniser.nix
    ./context-switcher.nix
    ./scripts.nix
  ];

  config = lib.mkIf config.nx.programs.prism.enable {
    # Enable all submodules by default (can be individually disabled)
    nx.programs.prism.neovim.enable = lib.mkDefault true;
    nx.programs.prism.opencode.enable = lib.mkDefault true;
    nx.programs.prism.tmux.enable = lib.mkDefault true;
    nx.programs.prism.sessioniser.enable = lib.mkDefault true;
    nx.programs.prism.contextSwitcher.enable = lib.mkDefault true;
    nx.programs.prism.scripts.enable = lib.mkDefault true;

    # Auto-enable choose on Darwin when sessioniser is enabled
    nx.programs.choose.enable = lib.mkDefault (
      pkgs.stdenv.isDarwin && config.nx.programs.prism.sessioniser.enable
    );

    # Computed values that submodules can reference
    nx.programs.prism._internal = {
      agentEnvPrefix = lib.concatStringsSep " " (
        lib.mapAttrsToList (name: value: "${name}=${value}") config.nx.programs.prism.agent.envVars
      );
    };
  };
}
