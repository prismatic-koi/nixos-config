{
  config,
  lib,
  pkgs,
  ...
}:
{
  options = {
    nx.programs.prism.pi.enable = lib.mkEnableOption "enables pi coding agent" // {
      default = true;
    };
    nx.programs.prism.pi.atlassian.enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = ''
        Whether to enable the pi Atlassian MCP extension. When true, an entry
        pointing into the atlassianExtensionDir nix-store path is added to the
        extensions list in ~/.pi/agent/settings.json, and a
        home.file.".pi/agent/atlassian-extension" entry is emitted to GC-root
        that store path. Authentication is via /login-atlassian inside a pi
        session.

        This option controls only the pi-side extension and is independent of
        the Atlassian MCP server.
      '';
    };
    nx.programs.prism.pi.atlassian.defaultCloudId = lib.mkOption {
      type = lib.types.str;
      default = "";
      example = "08986a80-a6ed-4480-ae2d-4a439d50d71b";
      description = ''
        Default Atlassian cloud ID to use when the agent omits the cloudId
        parameter in any Atlassian MCP tool call. When set, the value is
        exposed to the pi Atlassian extension as the ATLASSIAN_DEFAULT_CLOUD_ID
        environment variable, and every tool call that omits cloudId has the
        default injected automatically before being forwarded upstream.

        The tool descriptions surfaced to the agent (via tools/list) are also
        updated to mark cloudId as optional and reference this default.

        Defaults to "" (empty string), which preserves the current behaviour
        where cloudId is required on every tool call.

        Single-site setups: set this to the UUID of your Atlassian site (find
        it via getAccessibleAtlassianResources or from your Atlassian admin).
        For example, thankyoupayroll.atlassian.net uses
        08986a80-a6ed-4480-ae2d-4a439d50d71b.

        Multi-site setups: leave this empty and pass cloudId explicitly on
        each tool call (current behaviour).
      '';
    };
  };

  config = lib.mkIf config.nx.programs.prism.pi.enable (
    let
      envPrefix = config.nx.programs.prism._internal.agentEnvPrefix;
      clipboardCmd = if pkgs.stdenv.isDarwin then "pbcopy" else "wl-copy";

      # AWS skill with clipboard command substituted at eval time.
      awsSkillFile = pkgs.replaceVars ./skills/aws/SKILL.md {
        inherit clipboardCmd;
      };

      # Merged skills directory — built as a single derivation so it can be
      # linked via one home.file entry. This avoids fragmented symlinks inside
      # the persisted ~/.pi/agent/skills/ directory that would dangle after
      # nix-collect-garbage removes the store paths they pointed to.
      skillsDir = pkgs.runCommand "pi-skills" { } ''
        mkdir -p $out/prism $out/aws $out/acceptance-criteria $out/retro $out/atlassian $out/grill-me $out/wip-branch
        cp -r ${./skills/prism}/* $out/prism/
        cp -r ${./skills/playwright-cli} $out/playwright-cli
        cp ${awsSkillFile} $out/aws/SKILL.md
        cp -r ${./skills/acceptance-criteria}/* $out/acceptance-criteria/
        cp -r ${./skills/retro}/* $out/retro/
        cp -r ${./skills/atlassian}/* $out/atlassian/
        cp -r ${./skills/grill-me}/* $out/grill-me/
        cp -r ${./skills/wip-branch}/* $out/wip-branch/
      '';

      # Vendored pi extensions directory — built as a single derivation so it
      # lands at a stable nix-store path and is correctly GC-rooted via
      # home.file. Mirrors the skillsDir pattern above.
      # See pi/extensions/anthropic-oauth/UPSTREAM.md for the port procedure.
      piExtensionsDir = pkgs.runCommand "pi-extensions" { } ''
        mkdir -p $out/anthropic-oauth
        cp -r ${./pi/extensions/anthropic-oauth}/* $out/anthropic-oauth/
      '';

      # Atlassian MCP extension for pi — connects to mcp.atlassian.com via
      # OAuth PKCE, enumerates tools/list at startup, and registers each tool
      # via pi.registerTool(). See pi/extensions/atlassian/UPSTREAM.md.
      atlassianExtensionDir = pkgs.runCommand "pi-atlassian-extension" { } ''
        mkdir -p $out
        cp -r ${./pi/extensions/atlassian}/* $out/
      '';

      # Prism extension for pi — a single derivation whose root contains
      # prism.ts. This path is wired into config.json via piExtensionDir so
      # prism can locate the extension at spawn time.
      prismExtensionDir = pkgs.runCommand "prism-pi-extension" { } ''
        mkdir -p $out
        cp ${./pi/extensions/prism.ts} $out/prism.ts
      '';

      # Pi agent settings.json content, defined once here so a future container
      # role can reference the same value without duplication.
      # Follow-up container support will add containerWorkerSettingsJson /
      # containerCoordinatorSettingsJson options (analogous to the container config
      # container config blobs at lines 29-73) and reference piSettings there.
      piSettings = {
        steeringMode = "one-at-a-time";
        transport = "sse";
        theme = config.theme.name;
        treeFilterMode = "default";
        quietStartup = true;
        enableInstallTelemetry = false;
        warnings = {
          anthropicExtraUsage = false;
        };

        # Vendored extensions registered in settings.json.
        #
        # NOTE: the prism PI extension is intentionally NOT listed here.
        # It is loaded via the `--extension` CLI flag emitted by prism's
        # Go side at spawn time:
        #   - internal/container/pi_invocation.go (bwrap / sandbox-exec)
        #   - internal/session/session.go::buildDirectAgentCmd (host mode, #2065)
        #
        # An earlier iteration of #2068 also registered prism.ts here as
        # defence-in-depth, on the assumption that pi's resource-loader
        # de-dupes extension paths by canonicalised realpath. That holds
        # in host mode but FAILS under bwrap: inside the sandbox, the
        # `--extension` flag uses the bind-mount path
        # (~/.config/prism/pi-extensions/prism.ts) while settings.json
        # carries the raw nix-store path, and bwrap bind mounts mask the
        # underlying realpath so canonicalisation returns two distinct
        # absolute paths. The extension then loaded twice, pi flagged
        # "--agent" as a conflicting flag registration, and the entire
        # prism↔pi integration surface broke on every bwrap session.
        # See the post-mortem for #2068 for the full diagnosis.
        #
        # The anthropic-oauth entry below replaces the previous
        # npm:pi-anthropic-oauth@0.1.9 package. All extensions are stored
        # in the nix store as plain TypeScript files and loaded by pi's
        # jiti-based extension loader at runtime.
        # To port a future upstream fix, see:
        #   modules/programs/prism/pi/extensions/anthropic-oauth/UPSTREAM.md
        extensions = [
          "${piExtensionsDir}/anthropic-oauth/index.ts"
        ]
        ++
          lib.optional config.nx.programs.prism.pi.atlassian.enable
            # Atlassian MCP extension: connects to mcp.atlassian.com via OAuth
            # PKCE, enumerates tools/list at startup, and registers each tool.
            # Use /login-atlassian in a pi session to authenticate.
            # See pi/extensions/atlassian/UPSTREAM.md for auth method rationale.
            "${atlassianExtensionDir}/index.ts";
      };

      colourLib = import ../../colour-scheme/lib.nix;

      piTheme =
        with config.theme;
        builtins.toJSON {
          "$schema" =
            "https://raw.githubusercontent.com/badlogic/pi-mono/main/packages/coding-agent/src/modes/interactive/theme/theme-schema.json";
          name = config.theme.name;
          colors = {
            # Core UI
            accent = primary;
            border = primary;
            borderAccent = secondary;
            borderMuted = grey0;
            success = green;
            error = red;
            warning = orange;
            muted = grey0;
            dim = grey1;
            text = "";
            thinkingText = grey0;
            # Backgrounds & Content
            selectedBg = bg_visual;
            userMessageBg = bg1;
            userMessageText = "";
            customMessageBg = bg1;
            customMessageText = "";
            customMessageLabel = primary;
            toolPendingBg = bg_dim;
            toolSuccessBg = colourLib.darken bg_green 35;
            toolErrorBg = colourLib.darken bg_red 35;
            toolTitle = primary;
            toolOutput = "";
            # Markdown
            mdHeading = orange;
            mdLink = blue;
            mdLinkUrl = grey0;
            mdCode = green;
            mdCodeBlock = "";
            mdCodeBlockBorder = grey0;
            mdQuote = grey0;
            mdQuoteBorder = grey0;
            mdHr = grey0;
            mdListBullet = aqua;
            # Tool Diffs
            toolDiffAdded = green;
            toolDiffRemoved = red;
            toolDiffContext = grey0;
            # Syntax Highlighting
            syntaxComment = grey0;
            syntaxKeyword = red;
            syntaxFunction = blue;
            syntaxVariable = orange;
            syntaxString = green;
            syntaxNumber = purple;
            syntaxType = aqua;
            syntaxOperator = primary;
            syntaxPunctuation = grey1;
            # Thinking Level Borders
            thinkingOff = grey0;
            thinkingMinimal = grey1;
            thinkingLow = aqua;
            thinkingMedium = green;
            thinkingHigh = orange;
            thinkingXhigh = red;
            # Bash Mode
            bashMode = orange;
          };
        };
    in
    {
      nx.programs.prism.piExtensionDir = "${prismExtensionDir}";

      home-manager.users.${config.nx.username} = {
        home.packages = with pkgs; [
          pi-coding-agent
          fd
          tsx # for testing pi extensions
          mitmproxy # for testing
          playwright-cli
        ];

        programs.zsh.shellAliases = {
          pi =
            "PI_OFFLINE=1 ${envPrefix}"
            + lib.optionalString (
              config.nx.programs.prism.pi.atlassian.enable
              && config.nx.programs.prism.pi.atlassian.defaultCloudId != ""
            ) " ATLASSIAN_DEFAULT_CLOUD_ID=${config.nx.programs.prism.pi.atlassian.defaultCloudId}"
            + " pi";
        };

        # Agent markdown files at ~/.config/prism/agents/ — consumed by
        # prism spawn validation (prismAgentRolePath) and review pre-flight
        # checks (CheckAgentAvailability). The directory is a single symlink
        # into the nix store so GC cannot fragment it.
        xdg.configFile."prism/agents".source = ./agents;
        # Skills at ~/.config/prism/skills/ — consumed by the prism spawn
        # skills manifest hash calculation (ComputeManifest).
        xdg.configFile."prism/skills".source = skillsDir;

        home.file.".pi/agent/settings.json".text = builtins.toJSON piSettings;
        # Note: ~/.pi/agent/system-prompt.md and coordinator-system-prompt.md
        # were removed in the #2064 cleanup. They were staged by the pre-#2031
        # mechanism (when prism wrote a per-session APPEND_SYSTEM.md staging
        # file), but pi 0.79 has no native loader for either filename — it
        # only auto-discovers SYSTEM.md / APPEND_SYSTEM.md per dist/core/
        # resource-loader.js. Confirmed inert from #2038 onwards (when the
        # APPEND_SYSTEM.md staging path was removed); role-prompt injection
        # now flows through the prism PI extension's pi.registerFlag("agent")
        # path (extension reads ~/.config/prism/agents/<role>.md at
        # before_agent_start).
        home.file.".pi/agent/AGENTS.md".text = builtins.readFile ./agents/global-instructions.md;
        # Custom theme derived from config.theme, deployed so pi picks it up
        # from ~/.pi/agent/themes/ at runtime. The theme name matches
        # config.theme.name and all colour values come from the system palette.
        home.file.".pi/agent/themes/${config.theme.name}.json".text = piTheme;
        home.file.".pi/agent/skills".source = skillsDir;
        # Vendored extensions directory — GC-rooted via this home.file entry.
        # The extension path referenced in settings.json points into this
        # nix-store path, so nix-store --gc will not remove it.
        home.file.".pi/agent/extensions".source = piExtensionsDir;
        # Prism extension — GC-rooted so the store path referenced in
        # config.json is not removed by nix-collect-garbage.
        home.file.".pi/agent/prism-extension".source = prismExtensionDir;
        # Atlassian MCP extension — GC-rooted so the nix-store path referenced
        # in settings.json is not removed by nix-collect-garbage.
        # Only emitted when nx.programs.prism.pi.atlassian.enable is true.
        home.file.".pi/agent/atlassian-extension" = lib.mkIf config.nx.programs.prism.pi.atlassian.enable {
          source = atlassianExtensionDir;
        };

        home.persistence."/persist" = {
          directories = [ ".pi" ];
        };
      };
    }
  );
}
