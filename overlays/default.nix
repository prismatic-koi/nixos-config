{
  outputs,
  inputs,
}:
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
      # bleeding edge
      pi-coding-agent = masterPkgs.pi-coding-agent;
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

      # packages not yet in nixpkgs; use local definitions
      # playwright-cli is cross-platform: on Linux it uses pkgs.chromium
      # (the user's daily-driver browser, closure already paid for); on
      # Darwin it uses pkgs.playwright-driver.browsers-chromium (the
      # playwright-pinned "Google Chrome for Testing" build, identity-
      # isolated from the user's Chrome.app).
      playwright-cli = final.callPackage ../pkgs/playwright-cli.nix { };

      # flate: offline Flux GitOps validator/renderer, not yet in nixpkgs
      # (issue #2990).
      flate = final.callPackage ../pkgs/flate.nix { };

      # twg: Atlassian Teamwork Graph CLI, prebuilt and unfree (issue #3195).
      twg = final.callPackage ../pkgs/twg.nix { };

      # claude-desktop: official Linux .deb, prebuilt and unfree (issue #3340).
      # Linux only; Darwin gets null so evaluation does not break.
      claude-desktop =
        if final.stdenv.hostPlatform.isLinux then
          final.callPackage ../pkgs/claude-desktop.nix { }
        else
          null;

      prism = final.callPackage ../pkgs/prism.nix { };

      # battery-monitor: Linux-only Go daemon (UPower + sysfs +
      # session-bus notifications). See pkgs/battery-monitor.nix and
      # modules/services/battery-monitor/DESIGN.md.
      battery-monitor =
        if final.stdenv.hostPlatform.isLinux then
          final.callPackage ../pkgs/battery-monitor.nix { }
        else
          null;

      # grafana-alloy: on Darwin only, drop the `netgo` build tag so the
      # binary links the cgo resolver instead of the pure-Go one. The
      # pure-Go resolver reads /etc/resolv.conf directly and ignores
      # macOS's per-interface "scoped" resolvers -- which is where
      # tailscale installs the `tailnet.internal` split-DNS route on
      # Darwin. With `netgo` in `tags`, the cgo resolver is compiled
      # OUT of the binary entirely, so `GODEBUG=netdns=cgo` (set on the
      # launchd daemon in modules/services/alloy/default.nix) has
      # nothing to switch to -- it's an inert flag without this
      # override. See issue #2694.
      #
      # `tags` is a Go build-flag list, not a vendoring input, so
      # removing `netgo` does not change `vendorHash`/`npmDepsHash` and
      # needs no re-hash. The Linux build is untouched: `prev` is
      # returned as-is there, so its `tags` still include `netgo`.
      #
      # Also on Darwin only: disable the test phase. The `checkPhase` link
      # step exhausts the macOS runner's disk when compiling the test suite.
      # The derivation hash changes from the `netgo` removal (no binary cache
      # hit), so every flake update rebuilds alloy and its full test suite
      # from source on the runner; the product binary builds fine but the
      # test binary link hits `errno=28` (ENOSPC, no space left on device).
      # See failed run: https://github.com/prismatic-koi/nixos-config/actions/runs/31683738928
      #
      # REMOVAL CONDITION: revisit if the `netgo` override is ever dropped.
      # Without the `netgo` change, `grafana-alloy` gets a binary cache hit
      # on Darwin, the test phase costs the runner nothing, and `doCheck = false`
      # becomes dead weight.
      grafana-alloy =
        if final.stdenv.hostPlatform.isDarwin then
          prev.grafana-alloy.overrideAttrs (oldAttrs: {
            tags = final.lib.remove "netgo" (oldAttrs.tags or [ ]);
            doCheck = false;
          })
        else
          prev.grafana-alloy;

      _macronTypePkg =
        if final.stdenv.hostPlatform.isDarwin then final.callPackage ../pkgs/macron-type.nix { } else null;
      macron-type = if final.stdenv.hostPlatform.isDarwin then final._macronTypePkg.server else null;
      macron-send = if final.stdenv.hostPlatform.isDarwin then final._macronTypePkg.client else null;

    };
}
