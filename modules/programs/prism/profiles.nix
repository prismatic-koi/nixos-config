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

  config = lib.mkMerge [
    {
      nx.programs.prism.profiles =
        let
          # ── Flat per-role profile schema (#1612) ────────────────────────────────
          #
          # A profile is a flat map from session role → per-role configuration
          # record. The 10 roles are exactly the 10 pi agent files in
          # modules/programs/prism/agents/.
          #
          # Each slot carries:
          #   - provider:         the routing provider
          #   - model:            the model identifier
          #   - thinking:         the reasoning level (rendered as pi "variant")
          #   - systemPromptPath: absolute path to the role's system prompt file

          # The canonical set of pi agent roles (mirrors the files in ./agents/).
          piRoles = [
            "coordinator"
            "worker"
            "ac"
            "retro"
            "investigate"
            "review-goal"
            "review-code"
            "review-security"
            "review-qa"
            "review-context"
          ];

          # Map a role name to its agent prompt file (sans .md extension).
          # Since filenames now match role names directly, this is an identity map.
          roleToAgentFile = lib.genAttrs piRoles (role: role);

          # Convenience: build a slot with a systemPromptPath derived from
          # the role's agent file. `thinking` defaults to "off" (the PI
          # harness zero value). `provider` defaults to "".
          slot =
            role:
            {
              provider ? "",
              model,
              thinking ? "off",
            }:
            {
              inherit provider model thinking;
              systemPromptPath = "$HOME/.config/prism/agents/${roleToAgentFile.${role}}.md";
            };

          # Build a profile entry by calling `fn role` for every pi role.
          # `fn` returns the per-role slot attrset.
          profileFromFn = fn: lib.genAttrs piRoles (role: fn role);

          # Convenience: build a profile where every role with a given set of
          # roles receives the same slot value. Roles not in the map fall back
          # to the provided default. Used to express "coordinator gets opus,
          # everything else gets sonnet".
          profileFromSlots =
            slotMap:
            lib.genAttrs piRoles (
              role:
              let
                s = slotMap.${role} or slotMap._default or null;
              in
              if s == null then
                throw "profiles.nix: no slot defined for role '${role}' and no _default"
              else
                s
                // {
                  systemPromptPath = "$HOME/.config/prism/agents/${roleToAgentFile.${role}}.md";
                }
            );

          # ── Profiles ────────────────────────────────────────────────────────────

          profiles = {
            anthropic = profileFromSlots {
              coordinator = slot "coordinator" {
                provider = "anthropic";
                model = "anthropic/claude-opus-4-7";
                thinking = "medium";
              };
              _default = slot "worker" {
                provider = "anthropic";
                model = "anthropic/claude-sonnet-4-6";
                thinking = "low";
              };
            };

            anthropic-opus = profileFromSlots {
              coordinator = slot "coordinator" {
                provider = "anthropic";
                model = "anthropic/claude-opus-4-7";
                thinking = "medium";
              };
              _default = slot "worker" {
                provider = "anthropic";
                model = "anthropic/claude-opus-4-7";
              };
            };

            gemini-hybrid = profileFromSlots {
              coordinator = slot "coordinator" {
                provider = "anthropic";
                model = "anthropic/claude-sonnet-4-6";
              };
              _default = slot "worker" {
                provider = "google";
                model = "google/gemini-3.1-pro-preview-customtools";
              };
            };

            github-copilot = profileFromSlots {
              coordinator = slot "coordinator" {
                provider = "github-copilot";
                model = "github-copilot/claude-sonnet-4.6";
                thinking = "medium";
              };
              _default = slot "worker" {
                provider = "github-copilot";
                model = "github-copilot/claude-sonnet-4.6";
              };
            };

            google = profileFromSlots {
              _default = slot "coordinator" {
                provider = "google";
                model = "google/gemini-3-flash";
              };
            };
          };

          quickProfiles = {
            pr = {
              model = "google/gemini-3.1-flash-lite";
              providerOrder = [
                "google"
                "google-vertex"
              ];
            };
          };

          # Translate a profile thinking value to the pi variant string.
          # The canonical zero value in profiles is "off" (the PI harness
          # convention), but pi expects "none" as its zero value.
          thinkingToVariant = thinking: if thinking == "off" then "none" else thinking;
        in
        {
          data = {
            inherit profiles quickProfiles;
          };

          json =
            let
              homeDir = config.home-manager.users.${config.nx.username}.home.homeDirectory;
              # Resolve a possible $HOME reference inside a string.
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
            builtins.toJSON (
              {
                default = config.nx.programs.prism.profile.default;
              }
              // {
                # Profiles are role-keyed. Each slot is emitted with its full
                # record (provider, model, thinking, systemPromptPath).
                profiles = lib.mapAttrs (
                  _name: profileEntry:
                  lib.mapAttrs (_role: roleSlot: {
                    provider = roleSlot.provider or "";
                    model = roleSlot.model;
                    thinking = roleSlot.thinking or "off";
                    systemPromptPath = expandHome (roleSlot.systemPromptPath or "");
                  }) profileEntry
                ) config.nx.programs.prism.profiles.data.profiles;
                # Agent environment variables to inject into host-mode agent processes.
                agent_env_vars = lib.mapAttrs (
                  _name: value: expandHome value
                ) config.nx.programs.prism.agent.envVars;
                # Quick command profiles.
                quick_profiles = config.nx.programs.prism.profiles.data.quickProfiles;
                # Per-container resource caps.
                container_resources = config.nx.programs.prism._internal.agentResources;
              }
            );

          # applyProfile patches `model` and `variant` onto each baseAgent that
          # the active profile defines a slot for. Agents not present in the
          # profile (e.g. `build`) are returned unchanged so they inherit the
          # top-level harness model.
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
                  variant = thinkingToVariant slotCfg.thinking;
                }
            ) baseAgents;
        };
    }

    # Write profiles.json to ~/.config/prism/ and persist that directory on
    # impermanence systems.
    (lib.mkIf config.nx.programs.prism.enable {
      home-manager.users.${config.nx.username} = {
        xdg.configFile."prism/profiles.json".text = config.nx.programs.prism.profiles.json;
        home.persistence."/persist" = {
          directories = [
            ".config/prism"
          ];
        };
      };
    })
  ];
}
