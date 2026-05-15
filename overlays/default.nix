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
      pi-coding-agent =
        let
          newSrc = prev.fetchFromGitHub {
            owner = "badlogic";
            repo = "pi-mono";
            tag = "v0.72.1";
            hash = "sha256-SqUxghc60P3HfmaFJGB/m23mvzw0cD7cDEUrNFOqo0Y=";
          };
        in
        prev.pi-coding-agent.overrideAttrs (old: {
          version = "0.72.1";
          src = newSrc;
          npmDeps = prev.fetchNpmDeps {
            inherit (old) npmWorkspace;
            src = newSrc;
            hash = "sha256-KUC1xQK6oJXtg962YeLOnO76uTdR10/VNa9iiCdT3VM=";
          };
        });

      # direnv: disable the test phase on Darwin to work around a hang in
      # `direnv-test.zsh` introduced when libarchive was bumped 3.8.4 -> 3.8.6
      # on staging-25.11 (nixpkgs commit 32e655f). Direnv's source is
      # unchanged but transitive input changes force rebuilds, and the test
      # harness wedges on aarch64-darwin.
      # See: https://github.com/NixOS/nixpkgs/issues/507531
      # Remove this override once upstream lands a fix.
      direnv =
        if final.stdenv.isDarwin then
          prev.direnv.overrideAttrs (_: {
            doCheck = false;
          })
        else
          prev.direnv;

      # packages not yet in nixpkgs; use local definitions
      # playwright-cli depends on chromium which is Linux-only
      playwright-cli =
        if final.stdenv.isLinux then final.callPackage ../pkgs/playwright-cli.nix { } else null;

      prism = final.callPackage ../pkgs/prism.nix { };

      _macronTypePkg =
        if final.stdenv.isDarwin then final.callPackage ../pkgs/macron-type.nix { } else null;
      macron-type = if final.stdenv.isDarwin then final._macronTypePkg.server else null;
      macron-send = if final.stdenv.isDarwin then final._macronTypePkg.client else null;

    };
}
