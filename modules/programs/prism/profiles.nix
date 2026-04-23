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
            "review-goal"
            "review-code"
            "review-security"
            "review-qa"
            "review-context"
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
              variant = "none";
            };
            secondary = {
              model = "anthropic/claude-sonnet-4-6";
              variant = "none";
            };
            lightweight = {
              model = "anthropic/claude-haiku-4-5";
              variant = "none";
            };
          };
          anthropic-opus = {
            primary = {
              model = "anthropic/claude-opus-4-7";
              variant = "none";
            };
            secondary = {
              model = "anthropic/claude-opus-4-7";
              variant = "none";
            };
            lightweight = {
              model = "anthropic/claude-haiku-4-5";
              variant = "none";
            };
          };
          gemini-hybrid = {
            primary = {
              model = "anthropic/claude-sonnet-4-6";
              variant = "none";
            };
            secondary = {
              model = "google/gemini-3.1-pro-preview-customtools";
              variant = "none";
            };
            lightweight = {
              model = "anthropic/claude-haiku-4-5";
              variant = "none";
            };
          };
          github-copilot = {
            primary = {
              model = "github-copilot/claude-sonnet-4.6";
              variant = "none";
            };
            secondary = {
              model = "github-copilot/claude-sonnet-4.6";
              variant = "none";
            };
            lightweight = {
              model = "github-copilot/claude-haiku-4.5";
              variant = "none";
            };
          };
          google = {
            primary = {
              model = "google/gemini-3-flash-preview";
              variant = "none";
            };
            secondary = {
              model = "google/gemini-3.1-flash-lite-preview";
              variant = "none";
            };
            lightweight = {
              model = "google/gemini-3.1-flash-lite-preview";
              variant = "none";
            };
          };
        };
        quickProfiles = {
          pr = {
            model = "google/gemini-3.1-flash-lite-preview";
            providerOrder = [
              "google"
              "google-vertex"
            ];
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
          # Per-agent review configs — each blob declares only its own agent.
          # The old container_review_config (PR-A) is retired; PR-B replaces it
          # with five agent-specific blobs.
          container_review_goal_config = config.nx.programs.prism.opencode.containerReviewGoalConfigJson;
          container_review_code_config = config.nx.programs.prism.opencode.containerReviewCodeConfigJson;
          container_review_security_config =
            config.nx.programs.prism.opencode.containerReviewSecurityConfigJson;
          container_review_qa_config = config.nx.programs.prism.opencode.containerReviewQaConfigJson;
          container_review_context_config =
            config.nx.programs.prism.opencode.containerReviewContextConfigJson;
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
          # Quick command profiles — lightweight model configs for prism quick subcommands.
          quick_profiles = config.nx.programs.prism.profiles.data.quickProfiles;
          # Per-container resource caps. These are read by the prism sidecar
          # and passed to container.Config so that podman run receives
          # --memory, --memory-swap, and --pids-limit for every agent container.
          # Values flow: nix option → _internal.agentResources → here → profiles.json
          # → prism sidecar → container.Config → buildRunArgs → podman run.
          container_resources = config.nx.programs.prism._internal.agentResources;
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
