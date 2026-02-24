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

      # ncmpcpp and openscad fail to build against boost 1.89 because their old
      # build systems try to link boost_system as a shared library, but it has
      # been header-only since boost 1.69. Pin to boost187 until upstream is fixed.
      ncmpcpp = prev.ncmpcpp.override { boost = prev.boost187; };
      openscad = prev.openscad.override { boost = prev.boost187; };

      # calibre 8.16 fails to build due to a missing qmake in qtbase6-setup-hook.
      # Use stable until upstream is fixed.
      calibre = stablePkgs.calibre;

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
