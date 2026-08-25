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
          #
          # The role system-prompt is no longer carried in the slot: it is
          # injected at runtime by the prism PI extension (before_agent_start)
          # from ~/.config/prism/agents/<role>.md (design #2031).

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

          # Convenience: build a slot. `thinking` defaults to "off" (the PI
          # harness zero value). `provider` defaults to "". The `role` arg is
          # retained for call-site readability but no longer affects the slot
          # (the role system-prompt is injected by the PI extension, not staged
          # via a per-role path — design #2031).
          slot =
            _role:
            {
              provider ? "",
              model,
              thinking ? "off",
            }:
            {
              inherit provider model thinking;
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
              if s == null then throw "profiles.nix: no slot defined for role '${role}' and no _default" else s
            );

          # ── Profiles ────────────────────────────────────────────────────────────

          # Tier-based profile naming (issue #2404). Tiers scale by task
          # complexity — pick with the `complexity-triage` skill. Coordinator
          # is never the cheapest slot in a tier; worker + all five review-*
          # roles scale together (uniform per tier); analytical roles
          # (`ac` / `retro` / `investigate`) get a sonnet floor.
          #
          # Helper: build a uniform slot for the worker + five review-* roles.
          reviewRoles = [
            "worker"
            "review-goal"
            "review-code"
            "review-security"
            "review-qa"
            "review-context"
          ];
          reviewSlots =
            s:
            lib.genAttrs reviewRoles (
              role: slot role (removeAttrs s [ "thinking" ] // { thinking = s.thinking or "off"; })
            );

          analyticalRoles = [
            "ac"
            "retro"
            "investigate"
          ];
          analyticalSlots =
            s:
            lib.genAttrs analyticalRoles (
              role: slot role (removeAttrs s [ "thinking" ] // { thinking = s.thinking or "off"; })
            );

          profiles = {
            light = profileFromSlots (
              {
                coordinator = slot "coordinator" {
                  provider = "anthropic";
                  model = "anthropic/claude-sonnet-5";
                  thinking = "low";
                };
              }
              // reviewSlots {
                provider = "anthropic";
                model = "anthropic/claude-sonnet-5";
                thinking = "off";
              }
              // analyticalSlots {
                provider = "anthropic";
                model = "anthropic/claude-sonnet-5";
                thinking = "low";
              }
            );

            standard = profileFromSlots (
              {
                coordinator = slot "coordinator" {
                  provider = "anthropic";
                  model = "anthropic/claude-opus-4-8";
                  thinking = "medium";
                };
              }
              // reviewSlots {
                provider = "anthropic";
                model = "anthropic/claude-sonnet-5";
                thinking = "low";
              }
              // analyticalSlots {
                provider = "anthropic";
                model = "anthropic/claude-sonnet-5";
                thinking = "low";
              }
            );

            heavy = profileFromSlots (
              {
                coordinator = slot "coordinator" {
                  provider = "anthropic";
                  model = "anthropic/claude-opus-5";
                  thinking = "medium";
                };
              }
              // reviewSlots {
                provider = "anthropic";
                model = "anthropic/claude-opus-4-8";
                thinking = "medium";
              }
              // analyticalSlots {
                provider = "anthropic";
                model = "anthropic/claude-opus-4-8";
                thinking = "low";
              }
            );

            max = profileFromSlots {
              _default = slot "worker" {
                provider = "anthropic";
                model = "anthropic/claude-opus-5";
                thinking = "xhigh";
              };
              coordinator = slot "coordinator" {
                provider = "anthropic";
                model = "anthropic/claude-opus-5";
                thinking = "xhigh";
              };
            };
          };

          quickProfiles = {
            pr = {
              # prism quick pr invokes `pi --print` (anthropic-oauth route).
              # See modules/programs/prism/prism/internal/quick/pr.go (#2118).
              model = "anthropic/claude-sonnet-5";
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
                # record (provider, model, thinking). The role system-prompt is
                # injected at runtime by the prism PI extension, not carried in
                # the slot (design #2031).
                profiles = lib.mapAttrs (
                  _name: profileEntry:
                  lib.mapAttrs (_role: roleSlot: {
                    provider = roleSlot.provider or "";
                    model = roleSlot.model;
                    thinking = roleSlot.thinking or "off";
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
