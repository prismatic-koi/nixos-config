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
    nx.programs.prism.profiles =
      let
        # ── Role-keyed profile schema (#1206) ──────────────────────────────────
        #
        # A profile is a map from session role → per-role configuration record.
        # Each slot carries:
        #   - provider:         the routing provider (used by future PI work)
        #   - model:            the model identifier emitted into opencode.json
        #   - thinking:         the reasoning level (rendered as opencode "variant")
        #   - systemPromptPath: optional path to a per-role system prompt (P2.AGENTRUN)
        #
        # Helpers below build a profile by stamping the same `slot` value across
        # every role in a list. This keeps migration concise while leaving room
        # for per-role divergence (e.g. a different reasoning level for review
        # agents) without further schema changes.

        # Role tier classification — preserved for legacy override semantics.
        # `--model` alongside a `--profile` overrides only the primary-tier
        # roles (mirrors pre-#1206 behaviour). It is also exported into
        # profiles.json under role_mapping for the Go-side override logic.
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

        # Stamp a slot value across the given list of role names.
        slotsFor = roles: slot: lib.genAttrs roles (_: slot);

        # Build a profile by combining tiered slot values. Each tier is a
        # `{primary, secondary, lightweight}` triple of slot values — the
        # canonical migration shape from the pre-#1206 schema. Tiers map onto
        # roleMapping so existing per-agent model assignments are preserved
        # bit-identically.
        profileFromTiers =
          tiers:
          (slotsFor roleMapping.primary tiers.primary)
          // (slotsFor roleMapping.secondary tiers.secondary)
          // (slotsFor roleMapping.lightweight tiers.lightweight);

        # Convenience: build a slot. `thinking` defaults to "none" to preserve
        # current opencode behaviour (renders as variant: "none"). `provider`
        # defaults to "" — populated explicitly by the migrated profiles below.
        # `systemPromptPath` is null until P2.AGENTRUN populates per-role prompts.
        slot =
          {
            provider ? "",
            model,
            thinking ? "none",
            systemPromptPath ? null,
          }:
          {
            inherit provider model thinking;
            systemPromptPath = if systemPromptPath == null then "" else toString systemPromptPath;
          };

        # ── Migrated profiles ──────────────────────────────────────────────────
        # Each existing profile is expanded into per-role slots via
        # profileFromTiers. The output is bit-identical (modulo whitespace /
        # key ordering) for opencode sessions because applyProfile (below)
        # consumes role-keyed slots directly and produces the same
        # {model, variant} merge per agent as the pre-#1206 implementation.
        profiles = {
          anthropic = profileFromTiers {
            primary = slot {
              provider = "anthropic";
              model = "anthropic/claude-sonnet-4-6";
            };
            secondary = slot {
              provider = "anthropic";
              model = "anthropic/claude-sonnet-4-6";
            };
            lightweight = slot {
              provider = "anthropic";
              model = "anthropic/claude-haiku-4-5";
            };
          };
          anthropic-opus = profileFromTiers {
            primary = slot {
              provider = "anthropic";
              model = "anthropic/claude-opus-4-7";
            };
            secondary = slot {
              provider = "anthropic";
              model = "anthropic/claude-opus-4-7";
            };
            lightweight = slot {
              provider = "anthropic";
              model = "anthropic/claude-haiku-4-5";
            };
          };
          gemini-hybrid = profileFromTiers {
            primary = slot {
              provider = "anthropic";
              model = "anthropic/claude-sonnet-4-6";
            };
            secondary = slot {
              provider = "google";
              model = "google/gemini-3.1-pro-preview-customtools";
            };
            lightweight = slot {
              provider = "anthropic";
              model = "anthropic/claude-haiku-4-5";
            };
          };
          github-copilot = profileFromTiers {
            primary = slot {
              provider = "github-copilot";
              model = "github-copilot/claude-sonnet-4.6";
            };
            secondary = slot {
              provider = "github-copilot";
              model = "github-copilot/claude-sonnet-4.6";
            };
            lightweight = slot {
              provider = "github-copilot";
              model = "github-copilot/claude-haiku-4.5";
            };
          };
          google = profileFromTiers {
            primary = slot {
              provider = "google";
              model = "google/gemini-3-flash-preview";
            };
            secondary = slot {
              provider = "google";
              model = "google/gemini-3.1-flash-lite-preview";
            };
            lightweight = slot {
              provider = "google";
              model = "google/gemini-3.1-flash-lite-preview";
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
      in
      {
        data = {
          inherit roleMapping profiles quickProfiles;
        };

        json =
          let
            homeDir = config.home-manager.users.${config.nx.username}.home.homeDirectory;
            # Resolve a possible $HOME reference inside a string. Used so that
            # systemPromptPath values pass through with $HOME expanded.
            expandHome =
              s:
              lib.strings.replaceStrings
                [
                  "$HOME"
                  "\${HOME}"
                ]
                [
                  homeDir
                  homeDir
                ]
                s;
          in
          builtins.toJSON {
            default = config.nx.programs.prism.opencode.provider;
            role_mapping = config.nx.programs.prism.profiles.data.roleMapping;
            # Profiles are role-keyed. Each slot is emitted with its full record
            # (provider, model, thinking, systemPromptPath). systemPromptPath is
            # always present (empty string when unset) to keep the JSON shape
            # uniform — Go reads it via the `omitempty` tag so empty values
            # round-trip cleanly.
            profiles = lib.mapAttrs (
              _name: profileEntry:
              lib.mapAttrs (_role: roleSlot: {
                provider = roleSlot.provider or "";
                model = roleSlot.model;
                thinking = roleSlot.thinking or "none";
                systemPromptPath = expandHome (roleSlot.systemPromptPath or "");
              }) profileEntry
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
              _name: value: expandHome value
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

        # applyProfile patches `model` and `variant` onto each baseAgent that
        # the active profile defines a slot for. Agents not present in the
        # profile (e.g. `build`) are returned unchanged so they inherit the
        # top-level opencode model.
        #
        # The mapping is direct under the role-keyed schema: `agentName` is the
        # slot key. `slot.thinking` is rendered as opencode's `variant` to
        # preserve bit-identical output with the pre-#1206 schema.
        applyProfile =
          profileName: baseAgents:
          let
            currentProfile = profiles.${profileName} or { };
          in
          lib.mapAttrs (
            name: cfg:
            let
              slotCfg = currentProfile.${name} or null;
            in
            if slotCfg == null then
              cfg
            else
              cfg
              // {
                model = slotCfg.model;
                variant = slotCfg.thinking;
              }
          ) baseAgents;
      };
  };
}
