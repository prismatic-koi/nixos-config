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
          # NOTE: this is an option *default*, so a machine that defines
          # nx.programs.prism.agent.envVars at all replaces the whole attrset
          # rather than merging with it. No machine in this flake does, and the
          # NOTION_MCP_REPOS entry appended at the bottom relies on that.
          default = {
            KUBECONFIG = "$HOME/.config/kube/agents-config";
            AWS_CONFIG_FILE = "$HOME/.config/aws/readonly-config";
            AWS_SHARED_CREDENTIALS_FILE = "$HOME/.config/aws/credentials";
            # Relocates claude-code's config dir (and .claude.json) to the
            # XDG path — matches the host-wide home.sessionVariables value in
            # claude-code.nix (issue #2243, Step 3c of #2132). Both isolators
            # deliver the value as-is; the in-sandbox capability is the RW
            # SBPL (subpath ~/.config/claude) grant (sandbox-exec) / the
            # Dst==Src RW bind (bwrap).
            CLAUDE_CONFIG_DIR = "$HOME/.config/claude";
            # Suppresses chromium's nested seatbelt sandbox in playwright-cli
            # (issue #2261, regression introduced by #2257 — final step of the
            # staging-HOME removal #2132). With HOME now pointing at the real
            # host home, chromium's inner sandbox profile resolves
            # ~/Library/Keychains via getpwuid()->pw_dir (not
            # CFFIXED_USER_HOME) and is denied by the outer SBPL profile,
            # crashing CrBrowserMain with SIGTRAP on any https URL. Flipping
            # this to "false" makes playwright pass --no-sandbox to chromium,
            # leaving the outer prism SBPL profile as the sole security
            # boundary — which it already is by design. The alternative
            # (granting RO to ~/Library/Keychains in the SBPL) was rejected on
            # security grounds: it would leak login keychain, Safari
            # passwords, and iCloud tokens. Harmless no-op on Linux (chromium
            # uses a different sandboxing mechanism under bwrap).
            PLAYWRIGHT_MCP_SANDBOX = "false";
            GIT_EDITOR = "true";
          }
          //
            lib.optionalAttrs
              (config.nx.programs.prism.pi.notion.enable && config.nx.programs.prism.pi.notion.repos != [ ])
              {
                # Repo allowlist for the pi Notion MCP extension (issue #2448).
                #
                # This lives in agent.envVars rather than the zsh alias on
                # purpose. The alias only reaches interactive shells; envVars is
                # the channel that actually reaches prism-spawned agents (it is
                # serialised as agent_env_vars in profiles.json and applied by
                # all three isolators). ATLASSIAN_DEFAULT_CLOUD_ID goes through
                # the alias and therefore does NOT reach spawned agents — a
                # pre-existing gap this deliberately does not copy.
                #
                # Values are injected verbatim (internal/container/env.go — no
                # shell in the loop), so a "~/" or "$HOME/" entry arrives
                # unexpanded; notion/scope.ts::expandPath handles both forms.
                NOTION_MCP_REPOS = lib.concatStringsSep ":" config.nx.programs.prism.pi.notion.repos;
              }
          //
            # Eager-activation role lists for the deferred MCP tool families
            # (issue #2532). Each extension registers one activate_<family>
            # tool at session_start and does the expensive work only when that
            # tool is called. A role named here skips the call and activates at
            # its first before_agent_start instead.
            #
            # These live in agent.envVars, not the zsh alias, for the same
            # reason NOTION_MCP_REPOS does: only agent.envVars is serialised as
            # agent_env_vars in profiles.json and applied by all three
            # isolators, so only agent.envVars reaches prism-spawned agents —
            # and roles only exist for prism-spawned agents.
            #
            # A colon-separated list matches NOTION_MCP_REPOS. Values are
            # injected verbatim (internal/container/env.go — no shell in the
            # loop), so the extension trims each entry itself.
            #
            # An empty list emits no variable at all, which the extensions read
            # as "nobody is eager" — the cheap default.
            lib.optionalAttrs
              (
                config.nx.programs.prism.pi.atlassian.enable
                && config.nx.programs.prism.pi.atlassian.eagerRoles != [ ]
              )
              {
                ATLASSIAN_MCP_EAGER_ROLES = lib.concatStringsSep ":" config.nx.programs.prism.pi.atlassian.eagerRoles;
              }
          //
            lib.optionalAttrs
              (config.nx.programs.prism.pi.notion.enable && config.nx.programs.prism.pi.notion.eagerRoles != [ ])
              {
                NOTION_MCP_EAGER_ROLES = lib.concatStringsSep ":" config.nx.programs.prism.pi.notion.eagerRoles;
              }
          //
            # NOTE: GRAFANA_MCP_EAGER_ROLES is inert on its own. The grafana
            # extension self-gates on GRAFANA_MCP_CONFIG_PATH and
            # PI_GRAFANA_MCP_BIN, which internal/config/agent_env_roles.go
            # already strips for review roles (#2533), so a review agent that
            # somehow appeared in this list would still register nothing.
            lib.optionalAttrs
              (
                config.nx.programs.prism.pi.grafana.enable && config.nx.programs.prism.pi.grafana.eagerRoles != [ ]
              )
              {
                GRAFANA_MCP_EAGER_ROLES = lib.concatStringsSep ":" config.nx.programs.prism.pi.grafana.eagerRoles;
              }
          // lib.optionalAttrs config.nx.programs.prism.pi.grafana.enable (
            let
              secretName = "grafana_config_${config.nx.programs.prism.pi.grafana.config}";
              # Cross-platform sops-secret path resolution mirrors
              # container-tokens.nix: on Linux the system-level sops-nix
              # secret lives under /run/secrets/, while on Darwin home-manager
              # sops-nix places it under ~/.config/sops-nix/secrets/.
              secretPath =
                if pkgs.stdenv.hostPlatform.isLinux then
                  config.sops.secrets.${secretName}.path
                else
                  config.home-manager.users.${config.nx.username}.sops.secrets.${secretName}.path;
            in
            {
              # Absolute host path to the sops-decrypted Grafana config
              # bundle. The pi grafana extension reads this file at
              # session_start via config-loader.ts. Delivered via
              # agent.envVars (not sessionVariables) for the same reason
              # NOTION_MCP_REPOS is: only agent.envVars reaches
              # prism-spawned bwrap agents. The bwrap isolator additionally
              # binds the sops-resolved concrete file at THIS env-var path
              # (Src=EvalSymlinks(secretPath), Dst=secretPath — same shape
              # as the AWS/kube XDG binds in mounts.go) so
              # readFileSync(process.env.GRAFANA_MCP_CONFIG_PATH) inside
              # the sandbox resolves to the concrete file. See
              # internal/container/bwrap.go for the full Src≠Dst rationale.
              GRAFANA_MCP_CONFIG_PATH = secretPath;
              # Absolute Nix-store path to the mcp-grafana binary. Baked in
              # at eval time so the extension has no PATH dependency inside
              # the sandbox. bwrap already ro-binds /nix/store, so no extra
              # binds are needed for the binary itself.
              PI_GRAFANA_MCP_BIN = "${pkgs.mcp-grafana}/bin/mcp-grafana";
            }
          );
          description = "Environment variables to set for the AI agent (pi)";
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
              - "bwrap":        run the agent inside a bubblewrap sandbox (Linux-only, default on Linux).
              - "host":         run the agent directly in the tmux pane with no isolation (default on Darwin).
              - "sandbox-exec": run the agent inside a macOS sandbox-exec profile (Darwin-only).

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

        isolationOverrides = lib.mkOption {
          type = lib.types.attrsOf lib.types.str;
          default = { };
          example = {
            "~/documents/obsidian" = "host";
          };
          description = ''
            Per-path isolation mode overrides. Maps path strings (with optional
            "~/" prefix) to isolation mode strings ("bwrap", "sandbox-exec", or
            "host"). When a session path matches a key, the associated mode is
            used instead of the machine default (default_isolation_mode).

            Invalid mode values are silently ignored by prism — the machine
            default is used instead.

            Written to config.json as project_isolation_overrides. The compiled-in
            Go default already maps "~/documents/obsidian" → "host" for fresh
            Darwin installs; set this option explicitly to override or extend it.
          '';
        };
      };

      profile = {
        default = lib.mkOption {
          type = lib.types.enum (builtins.attrNames config.nx.programs.prism.profiles.data.profiles);
          default = "standard";
          description = ''
            The system-wide default profile written as `default` in profiles.json.
            This controls which profile is active when prism spawns a new session
            and affects all harnesses, not just pi.
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
    ./checkin.nix
    ./container-tokens.nix
    ./neovim
    ./pi.nix
    ./pi/grafana-secret.nix
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
        nx.programs.prism.tmux.enable = lib.mkDefault true;
        nx.programs.prism.sessioniser.enable = lib.mkDefault true;
        nx.programs.prism.contextSwitcher.enable = lib.mkDefault true;
        nx.programs.prism.claude-code.enable = lib.mkDefault true;
        nx.programs.prism.pi.enable = lib.mkDefault true;
        nx.programs.prism.tui.enable = lib.mkDefault true;

        # Auto-enable choose on Darwin when contextSwitcher is enabled
        nx.programs.choose.enable = lib.mkDefault (
          pkgs.stdenv.hostPlatform.isDarwin && config.nx.programs.prism.contextSwitcher.enable
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
        systemd.oomd.enableUserSlices = lib.mkIf pkgs.stdenv.hostPlatform.isLinux true;
      }
    ]
  );
}
