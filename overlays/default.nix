{
  outputs,
  inputs,
}:
let
  addPatches =
    pkg: patches:
    pkg.overrideAttrs (oldAttrs: {
      patches = (oldAttrs.patches or [ ]) ++ patches;
    });
in
rec {
  modifications =
    final: prev:
    let
      masterPkgs = import inputs.nixpkgs-master {
        system = final.stdenv.hostPlatform.system;
        config.allowUnfree = true;
      };
      stablePkgs = import inputs.nixpkgs-stable {
        system = final.stdenv.hostPlatform.system;
        config.allowUnfree = true;
      };
    in
    {
      # use stable for firefox, unstable is currently failing to build
      firefox = stablePkgs.firefox;

      # use master for opencode, need the bleeding edge
      opencode = masterPkgs.opencode;

      # use master for claude-code, need the bleeding edge
      claude-code = masterPkgs.claude-code;

      # use master for discord, they block older versions
      discord = masterPkgs.discord;

      # use master for playwright-mcp, not in stable yet
      playwright-mcp = masterPkgs.playwright-mcp;

      # use master for beads, need the bleeding edge
      beads = masterPkgs.beads;

      vimPlugins = prev.vimPlugins // {
        obsidian-nvim = prev.vimUtils.buildVimPlugin {
          pname = "obsidian-nvim";
          version = "3.15.3";
          checkInputs = with prev.vimPlugins; [
            fzf-lua
            mini-nvim
            snacks-nvim
            telescope-nvim
          ];
          dependencies = with prev.vimPlugins; [
            plenary-nvim
          ];
          nvimSkipModules = [
            "minimal"
          ];
          src = prev.fetchFromGitHub {
            owner = "obsidian-nvim";
            repo = "obsidian.nvim";
            rev = "cc9f7b2588577a1961c563b8baa90f636e2d61b7";
            hash = "sha256-tGS1QLNcArFGGj2g2cmguHwzlEQBSRiCzj0FLxbm1FQ=";
          };
        };
      };
    };
}
