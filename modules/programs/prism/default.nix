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

        isolation = {
          default = lib.mkOption {
            type = lib.types.enum [
              "bwrap"
              "host"
              "sandbox-exec"
            ];
            default = if pkgs.stdenv.hostPlatform.isLinux then "bwrap" else "host";
            example = "bwrap";
            description = ''
              Default isolation mode for new agent sessions. Valid values:
              - "bwrap":        run opencode inside a bubblewrap sandbox (Linux-only, default on Linux).
              - "host":         run opencode directly in the tmux pane with no isolation (default on Darwin).
              - "sandbox-exec": run opencode inside a macOS sandbox-exec profile (Darwin-only).

              This value is written to ~/.config/prism/config.json as default_isolation_mode.
              It can be overridden per-spawn via `prism spawn --isolation <mode>`.

              Setting "bwrap" on a Darwin host fails at eval time.
              Setting "sandbox-exec" on a Linux host fails at eval time.
            '';
          };
        };

        resources = {
          memoryMax = lib.mkOption {
            type = lib.types.str;
            default = "5g";
            example = "4g";
            description = ''
              Maximum memory for each agent session (passed to bwrap cgroup limits).
              Set to an empty string to disable the limit (no flag emitted).
              Default of 5g is chosen for a 32 GB host: observed steady-state per-session
              usage is 300–660 MB; Nix builds peak around 4.5 GB before releasing memory.
              5g provides headroom for peak Nix builds while making the cap functional —
              a runaway session will OOM-kill itself at 5 GB rather than dragging the host
              to a panic.
            '';
          };

          memorySwapMax = lib.mkOption {
            type = lib.types.str;
            default = "5g";
            example = "5g";
            description = ''
              Maximum combined memory+swap for each agent session.
              Equal to memoryMax by default, which effectively disables swap
              for the session so a runaway process dies fast rather than thrashing.
              Set to an empty string to disable.
            '';
          };

          pidsLimit = lib.mkOption {
            type = lib.types.int;
            default = 4096;
            example = 2048;
            description = ''
              Maximum number of processes (PIDs) for each agent session.
              Set to 0 to disable the limit.
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

      profile = {
        default = lib.mkOption {
          type = lib.types.enum (builtins.attrNames config.nx.programs.prism.profiles.data.profiles);
          default = "anthropic";
          description = ''
            The system-wide default profile written as `default` in profiles.json.
            This controls which profile is active when prism spawns a new session
            and affects all harnesses (opencode, pi, etc.), not just opencode.
            Changing this selects a different set of model assignments for all
            session roles (coordinator, worker, explore, etc.).
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
    (lib.mkRenamedOptionModule
      [
        "nx"
        "programs"
        "prism"
        "opencode"
        "provider"
      ]
      [
        "nx"
        "programs"
        "prism"
        "profile"
        "default"
      ]
    )
    ./container-tokens.nix
    ./neovim
    ./opencode.nix
    ./pi.nix
    ./profiles.nix
    ./secrets.nix
    ./tmux.nix
    ./sessioniser.nix
    ./context-switcher.nix
    ./claude-code.nix
    ./prism-tui.nix
  ];

  config = lib.mkIf config.nx.programs.prism.enable (
    lib.mkMerge [
      {
        # Assert that bwrap isolation is not requested on Darwin.
        # bubblewrap is Linux-only; setting default = "bwrap" on Darwin is
        # a configuration error that should fail at eval time.
        assertions = [
          {
            assertion =
              !(config.nx.programs.prism.agent.isolation.default == "bwrap" && !pkgs.stdenv.hostPlatform.isLinux);
            message = ''
              nx.programs.prism.agent.isolation.default = "bwrap" requires Linux.
              bubblewrap is not available on ${pkgs.stdenv.hostPlatform.system}.
              Use "host" instead, or only set "bwrap" on Linux hosts.
            '';
          }
          {
            assertion =
              !(
                config.nx.programs.prism.agent.isolation.default == "sandbox-exec"
                && !pkgs.stdenv.hostPlatform.isDarwin
              );
            message = ''
              nx.programs.prism.agent.isolation.default = "sandbox-exec" requires Darwin.
              sandbox-exec is not available on ${pkgs.stdenv.hostPlatform.system}.
              Use "bwrap" or "host" instead, or only set "sandbox-exec" on Darwin hosts.
            '';
          }
        ];
      }
      {
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
      }
    ]
  );
}
