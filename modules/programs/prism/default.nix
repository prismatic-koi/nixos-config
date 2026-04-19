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
            AWS_CONFIG_FILE = "$HOME/.config/aws/readonly-config";
            GIT_EDITOR = "true";
          };
          description = "Environment variables to set for the AI agent (opencode)";
        };

        resources = {
          memoryMax = lib.mkOption {
            type = lib.types.str;
            default = "8g";
            example = "4g";
            description = ''
              Maximum memory for each agent container, passed to podman run --memory.
              Set to an empty string to disable the limit (no flag emitted).
              Default of 8g allows up to ~3 concurrent workers on a 32 GB host
              while leaving headroom for the compositor, browser, and system services.
            '';
          };

          memorySwapMax = lib.mkOption {
            type = lib.types.str;
            default = "8g";
            example = "8g";
            description = ''
              Maximum combined memory+swap for each agent container, passed to
              podman run --memory-swap. Equal to memoryMax by default, which
              effectively disables swap for the container so a runaway process
              dies fast rather than thrashing. Set to an empty string to disable.
            '';
          };

          pidsLimit = lib.mkOption {
            type = lib.types.int;
            default = 4096;
            example = 2048;
            description = ''
              Maximum number of processes (PIDs) for each agent container, passed
              to podman run --pids-limit. Set to 0 to disable the limit.
            '';
          };
        };
      };

      worktreeExclude = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ "obsidian" ];
        example = [
          "nixos-config"
          "dotfiles"
        ];
        description = ''
          List of repository directory names to exclude from automatic bare+worktree
          conversion. Repos matching any name in this list will be opened directly
          as regular directories rather than being offered a worktree conversion.
        '';
      };

      projects = {
        locations = lib.mkOption {
          type = lib.types.listOf lib.types.str;
          default = [ "~/code" ];
          example = [
            "~/code"
            "~/work"
          ];
          description = ''
            Directories to scan for projects. Each immediate subdirectory becomes
            a selectable entry in the context switcher.
          '';
        };

        specific = lib.mkOption {
          type = lib.types.listOf lib.types.str;
          default = [ "~/documents/obsidian" ];
          example = [ "~/documents/obsidian" ];
          description = ''
            Specific directories to include directly in the context switcher
            (not scanned for subdirectories).
          '';
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
    ./container-image.nix
    ./container-tokens.nix
    ./neovim
    ./opencode.nix
    ./pi.nix
    ./profiles.nix
    ./tmux.nix
    ./sessioniser.nix
    ./context-switcher.nix
    ./claude-code.nix
    ./prism-tui.nix
  ];

  config = lib.mkIf config.nx.programs.prism.enable {
    # Enable all submodules by default (can be individually disabled)
    nx.programs.prism.neovim.enable = lib.mkDefault true;
    nx.programs.prism.opencode.enable = lib.mkDefault true;
    nx.programs.prism.tmux.enable = lib.mkDefault true;
    nx.programs.prism.sessioniser.enable = lib.mkDefault true;
    nx.programs.prism.contextSwitcher.enable = lib.mkDefault true;
    nx.programs.prism.claude-code.enable = lib.mkDefault true;
    nx.programs.prism.pi.enable = lib.mkDefault true;
    nx.programs.prism.tui.enable = lib.mkDefault true;

    # Auto-enable choose on Darwin when contextSwitcher is enabled
    nx.programs.choose.enable = lib.mkDefault (
      pkgs.stdenv.isDarwin && config.nx.programs.prism.contextSwitcher.enable
    );

    # Computed values that submodules can reference
    nx.programs.prism._internal = {
      agentEnvPrefix = lib.concatStringsSep " " (
        lib.mapAttrsToList (name: value: "${name}=${value}") config.nx.programs.prism.agent.envVars
      );
      agentResources = {
        memoryMax = config.nx.programs.prism.agent.resources.memoryMax;
        memorySwapMax = config.nx.programs.prism.agent.resources.memorySwapMax;
        pidsLimit = config.nx.programs.prism.agent.resources.pidsLimit;
      };
    };

    # systemd-oomd: enable monitoring on user slices so that oomd can
    # intervene when memory pressure on the user slice crosses 80%, acting
    # as a safety net if pressure escapes the per-container cgroup caps.
    # Guarded by isLinux — Darwin does not have systemd.
    systemd.oomd.enableUserSlices = lib.mkIf pkgs.stdenv.isLinux true;
  };
}
