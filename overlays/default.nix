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
      opencodePkgs = import inputs.nixpkgs-opencode {
        system = final.stdenv.hostPlatform.system;
        config.allowUnfree = true;
      };
    in
    {
      # packages where we use master by default for bleeding edge

      claude-code = masterPkgs.claude-code;
      discord = masterPkgs.discord;
      # Pinned to nixpkgs-opencode (opencode 1.4.6). Upstream 1.4.11 regressed
      # the --prompt TUI flag (pre-fills instead of auto-submitting), breaking
      # prism's headless workflows. Unpin when either:
      #   (a) upstream opencode fixes the --prompt regression, or
      #   (b) we land the HTTP-submit path in the opencode harness adapter
      #       (decouples us from --prompt CLI semantics entirely).
      # See: https://github.com/prismatic-koi/nixos-config/issues/901
      opencode = opencodePkgs.opencode;
      pi-coding-agent = masterPkgs.pi-coding-agent;

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
