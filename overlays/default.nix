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

      # bitwarden-cli: pinned to nixpkgs-stable as the most-vetted source.
      # NOTE: pinning alone is no longer sufficient to prevent the `bw unlock
      # --raw` bogus-session regression (bitwarden/clients#20703, issue #1894).
      # nixpkgs-stable rolled forward to bitwarden-cli-2025.11.0 which still
      # carries the bug. The root cause is a server-pushed feature flag
      # (`unlock-via-sdk`) that triggers the broken code path regardless of
      # client version. The durable fix is the `unlock-via-sdk = false` patch
      # in modules/programs/bitwarden.nix (home.activation hook) and the
      # defensive patch in modules/programs/qutebrowser/userscripts/bitwarden-prefetch.
      # The pin is kept for institutional memory and because nixpkgs-stable
      # remains our most-vetted package source.
      # See: https://github.com/prismatic-koi/nixos-config/issues/1894
      bitwarden-cli = stablePkgs.bitwarden-cli;

      # bitwarden-desktop: swap the source-built `electron_39` for the
      # prebuilt `electron_39-bin` so we don't compile EOL electron from
      # source. Electron 39 is past end-of-life and has been marked
      # insecure in nixpkgs; the matching `permittedInsecurePackages =
      # [ "electron-39.8.10" ];` entry lives in `flake.nix`'s base
      # config block (module-level `nixpkgs.config` is bypassed because
      # we pass a pre-built `pkgs` into `nixosSystem`).
      #
      # nixpkgs tracking issue (Electron 39 EOL, most dependents bumped):
      #   https://github.com/NixOS/nixpkgs/issues/521305
      # nixpkgs issue for bitwarden-desktop specifically:
      #   https://github.com/NixOS/nixpkgs/issues/526914
      # Upstream electron bump PR (open, not yet merged):
      #   https://github.com/bitwarden/clients/pull/20448
      #
      # REMOVAL CONDITION: delete this block (and the matching
      # `permittedInsecurePackages` entry in `flake.nix`) once
      # bitwarden/clients#20448 lands and nixpkgs bumps
      # `bitwarden-desktop` to a build on electron >= 40.
      bitwarden-desktop = prev.bitwarden-desktop.override {
        electron_39 = final.electron_39-bin;
      };
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
      # playwright-cli is cross-platform: on Linux it uses pkgs.chromium
      # (the user's daily-driver browser, closure already paid for); on
      # Darwin it uses pkgs.playwright-driver.browsers-chromium (the
      # playwright-pinned "Google Chrome for Testing" build, identity-
      # isolated from the user's Chrome.app).
      playwright-cli = final.callPackage ../pkgs/playwright-cli.nix { };

      prism = final.callPackage ../pkgs/prism.nix { };

      # battery-monitor: Linux-only Go daemon (UPower + sysfs +
      # session-bus notifications). See pkgs/battery-monitor.nix and
      # modules/services/battery-monitor/DESIGN.md.
      battery-monitor =
        if final.stdenv.isLinux then final.callPackage ../pkgs/battery-monitor.nix { } else null;

      _macronTypePkg =
        if final.stdenv.isDarwin then final.callPackage ../pkgs/macron-type.nix { } else null;
      macron-type = if final.stdenv.isDarwin then final._macronTypePkg.server else null;
      macron-send = if final.stdenv.isDarwin then final._macronTypePkg.client else null;

    };
}
