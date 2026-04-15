{
  config,
  lib,
  ...
}:
{
  options = {
    nx.programs.prism.profiles = {
      data = lib.mkOption {
        type = lib.types.attrs;
        description = "Single source of truth for profile data and role mappings.";
        default = { };
      };
      json = lib.mkOption {
        type = lib.types.str;
        description = "Rendered JSON of the profile data.";
        default = "";
      };
      applyProfile = lib.mkOption {
        type = lib.types.functionTo (lib.types.functionTo lib.types.attrs);
        description = "Helper function to apply profile to base agents.";
        default = profileName: baseAgents: baseAgents;
      };
    };
  };

  config = {
    nx.programs.prism.profiles = {
      data = {
        roleMapping = {
          primary = [
            "coordinator"
            "plan"
          ];
          secondary = [
            "worker"
            "review"
            "ac"
            "retro"
          ];
          lightweight = [
            "explore"
            "title"
            "summary"
            "compaction"
          ];
        };
        profiles = {
          anthropic = {
            primary = {
              model = "anthropic/claude-sonnet-4-6";
              variant = "medium";
            };
            secondary = {
              model = "anthropic/claude-sonnet-4-6";
            };
            lightweight = {
              model = "anthropic/claude-haiku-4-5";
            };
          };
          anthropic-opus = {
            primary = {
              model = "anthropic/claude-opus-4-6";
            };
            secondary = {
              model = "anthropic/claude-opus-4-6";
            };
            lightweight = {
              model = "anthropic/claude-haiku-4-5";
            };
          };
          gemini-hybrid = {
            primary = {
              model = "anthropic/claude-sonnet-4-6";
            };
            secondary = {
              model = "google/gemini-3.1-pro-preview-customtools";
              variant = "medium";
            };
            lightweight = {
              model = "anthropic/claude-haiku-4-5";
            };
          };
          github-copilot = {
            primary = {
              model = "github-copilot/claude-sonnet-4.6";
            };
            secondary = {
              model = "github-copilot/claude-sonnet-4.6";
            };
            lightweight = {
              model = "github-copilot/claude-haiku-4.5";
            };
          };
          google = {
            primary = {
              model = "google/gemini-3-flash-preview";
            };
            secondary = {
              model = "google/gemini-3.1-flash-lite-preview";
            };
            lightweight = {
              model = "google/gemini-3.1-flash-lite-preview";
            };
          };
        };
      };

      json =
        let
          homeDir = config.home-manager.users.${config.nx.username}.home.homeDirectory;
        in
        builtins.toJSON {
          default = config.nx.programs.prism.opencode.provider;
          role_mapping = config.nx.programs.prism.profiles.data.roleMapping;
          profiles = lib.mapAttrs (
            _name: profileEntry:
            lib.mapAttrs (
              _role: roleCfg:
              { model = roleCfg.model; } // (lib.optionalAttrs (roleCfg ? variant) { variant = roleCfg.variant; })
            ) profileEntry
          ) config.nx.programs.prism.profiles.data.profiles;
          # Container role configs — full opencode.json blobs injected as
          # OPENCODE_CONFIG_CONTENT (precedence level 6) so no project-level
          # opencode.jsonc can override agent identity or permissions.
          container_worker_config = config.nx.programs.prism.opencode.containerWorkerConfigJson;
          container_coordinator_config = config.nx.programs.prism.opencode.containerCoordinatorConfigJson;
          # Agent environment variables to inject into host-mode opencode processes.
          # Both $HOME and ${HOME} are expanded at Nix eval time so the JSON
          # always contains absolute paths regardless of which form is used.
          agent_env_vars = lib.mapAttrs (
            _name: value:
            lib.strings.replaceStrings
              [
                "$HOME"
                "\${HOME}"
              ]
              [
                homeDir
                homeDir
              ]
              value
          ) config.nx.programs.prism.agent.envVars;
        };

      applyProfile =
        profileName: baseAgents:
        let
          roleMapping = config.nx.programs.prism.profiles.data.roleMapping;
          profiles = config.nx.programs.prism.profiles.data.profiles;

          # Reverse mapping: agentName -> roleName
          agentToRole = lib.foldl' (
            acc: role: acc // (lib.genAttrs roleMapping.${role} (agentName: role))
          ) { } (builtins.attrNames roleMapping);

          currentProfile = profiles.${profileName} or { };
        in
        lib.mapAttrs (
          name: cfg:
          let
            role = agentToRole.${name} or null;
            profileConfig = if role != null then currentProfile.${role} or { } else { };
          in
          cfg // profileConfig
        ) baseAgents;
    };
  };
}
