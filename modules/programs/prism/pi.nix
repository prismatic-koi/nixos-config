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
  };

  config = lib.mkIf config.nx.programs.prism.pi.enable (
    let
      envPrefix = config.nx.programs.prism._internal.agentEnvPrefix;
      clipboardCmd = if pkgs.stdenv.isDarwin then "pbcopy" else "wl-copy";

      # System prompts sourced directly from the opencode agent files so there
      # is one authoritative copy — updates to those files flow through here
      # automatically. The YAML frontmatter is opencode-specific metadata and
      # is harmless when read as plain markdown by pi.
      workerSystemPrompt = builtins.readFile ./opencode/agents/worker.md;
      coordinatorSystemPrompt = builtins.readFile ./opencode/agents/coordinator.md;

      # AWS skill with clipboard command substituted at eval time.
      # Shared with opencode.nix — one file, no divergence.
      awsSkillFile = pkgs.replaceVars ./opencode/skills/aws/SKILL.md {
        inherit clipboardCmd;
      };

      # Merged skills directory — built as a single derivation so it can be
      # linked via one home.file entry. This avoids fragmented symlinks inside
      # the persisted ~/.pi/agent/skills/ directory that would dangle after
      # nix-collect-garbage removes the store paths they pointed to.
      skillsDir = pkgs.runCommand "pi-skills" { } ''
        mkdir -p $out/prism $out/aws $out/acceptance-criteria
        cp -r ${./opencode/skills/prism}/* $out/prism/
        ${lib.optionalString pkgs.stdenv.isLinux ''
          cp -r ${./opencode/skills/playwright-cli} $out/playwright-cli
        ''}
        cp ${awsSkillFile} $out/aws/SKILL.md
        cp -r ${./opencode/skills/acceptance-criteria}/* $out/acceptance-criteria/
      '';
      # Pi agent settings.json content, defined once here so a future container
      # role can reference the same value without duplication.
      # Follow-up container support will add containerWorkerSettingsJson /
      # containerCoordinatorSettingsJson options (analogous to the opencode.nix
      # container config blobs at lines 29-73) and reference piSettings there.
      piSettings = {
        steeringMode = "one-at-a-time";
        transport = "sse";
        theme = "dark";
        treeFilterMode = "default";
        quietStartup = true;
        enableInstallTelemetry = false;

        # Fork fallback: if leohenon/pi-anthropic-oauth becomes unmaintained,
        # fork to prismatic-koi/pi-anthropic-oauth, publish to npm (or vendor
        # into the nix store), and update this packages entry. The three most
        # likely break sites are CLIENT_ID, TOKEN_URL, and AUTHORIZE_URL in
        # the extension's src/auth.ts.
        packages = [ "npm:pi-anthropic-oauth@0.1.9" ];
      };

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
            toolSuccessBg = bg_green;
            toolErrorBg = bg_red;
            toolTitle = primary;
            toolOutput = "";
            # Markdown
            mdHeading = orange;
            mdLink = blue;
            mdLinkUrl = grey0;
            mdCode = aqua;
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
            thinkingMinimal = primary;
            thinkingLow = blue;
            thinkingMedium = aqua;
            thinkingHigh = purple;
            thinkingXhigh = red;
            # Bash Mode
            bashMode = orange;
          };
        };
    in
    {
      home-manager.users.${config.nx.username} = {
        home.packages = with pkgs; [
          pi-coding-agent
          fd
        ];

        programs.zsh.shellAliases = {
          pi = "PI_OFFLINE=1 ${envPrefix} pi";
        };

        home.file.".pi/agent/settings.json".text = builtins.toJSON piSettings;
        home.file.".pi/agent/system-prompt.md".text = workerSystemPrompt;
        home.file.".pi/agent/coordinator-system-prompt.md".text = coordinatorSystemPrompt;
        # Custom theme derived from config.theme, deployed so pi picks it up
        # from ~/.pi/agent/themes/ at runtime. The theme name matches
        # config.theme.name and all colour values come from the system palette.
        home.file.".pi/agent/themes/${config.theme.name}.json".text = piTheme;
        home.file.".pi/agent/skills".source = skillsDir;

        home.persistence."/persist" = {
          directories = [ ".pi" ];
        };
      };
    }
  );
}
