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
      # packages pinned to stable to avoid broken builds in unstable
      bitwarden-desktop = stablePkgs.bitwarden-desktop;

      # packages where we use master by default for bleeding edge

      claude-code = masterPkgs.claude-code;
      discord = masterPkgs.discord;
      opencode = masterPkgs.opencode;

      # packages not yet in nixpkgs; use local definitions
      # playwright-cli depends on chromium which is Linux-only
      playwright-cli =
        if final.stdenv.isLinux then final.callPackage ../pkgs/playwright-cli.nix { } else null;

      # pinned to prevent random unstable changes which seem common these days
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
