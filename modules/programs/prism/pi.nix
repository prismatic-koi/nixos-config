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
        mkdir -p $out/prism $out/prism-db $out/aws
        cp -r ${./opencode/skills/prism}/* $out/prism/
        cp -r ${./opencode/skills/prism-db}/* $out/prism-db/
        ${lib.optionalString pkgs.stdenv.isLinux ''
          cp -r ${./opencode/skills/playwright-cli} $out/playwright-cli
        ''}
        cp ${awsSkillFile} $out/aws/SKILL.md
      '';
    in
    {
      home-manager.users.${config.nx.username} = {
        home.packages = with pkgs; [
          pi-coding-agent
          fd
        ];

        programs.zsh.shellAliases = {
          pi = "${envPrefix} pi";
        };

        home.file.".pi/agent/system-prompt.md".text = workerSystemPrompt;
        home.file.".pi/agent/coordinator-system-prompt.md".text = coordinatorSystemPrompt;
        # Themes are bundled inside the nixpkgs pi-coding-agent package at
        # lib/node_modules/pi-monorepo/dist/modes/interactive/theme/ and are
        # resolved from there at runtime. ~/.pi/agent/themes/ is only for
        # custom user overrides — no module-managed theme files are needed.
        home.file.".pi/agent/skills".source = skillsDir;

        home.persistence."/persist" = {
          directories = [ ".pi" ];
        };
      };
    }
  );
}
