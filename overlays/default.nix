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
      # packages where we use master by default for bleeding edge

      claude-code = masterPkgs.claude-code;
      discord = masterPkgs.discord;
      opencode = masterPkgs.opencode;
      playwright-mcp = masterPkgs.playwright-mcp;

      # packages not yet in nixpkgs; use local definitions
      playwright-cli = final.callPackage ../pkgs/playwright-cli.nix { };

      # package build fixes and other things

      # use stable for firefox, unstable is currently failing to build
      firefox = stablePkgs.firefox;

      # calibre 8.16 fails to build due to a missing qmake in qtbase6-setup-hook.
      # Use stable until upstream is fixed.
      calibre = stablePkgs.calibre;

      # libreoffice noto-fonts subset derivation uses a broken glob pattern after
      # noto-fonts filenames changed. Use master until nixpkgs#494721 lands in unstable.
      libreoffice = masterPkgs.libreoffice;

      vimPlugins = prev.vimPlugins // {
        obsidian-nvim = prev.vimUtils.buildVimPlugin {
          pname = "obsidian-nvim";
          version = "3.15.10";
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
            rev = "20432a5ca03d99a9d5ad51d362e19d9b832e46f0";
            hash = "sha256-zS6pX05kGFEsKlHef6xkfqIBCtNPbgbNKvzqj8ld5KM=";
          };
        };
      };
    };
}
