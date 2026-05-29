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

      # openrazer userspace + kernel module: pinned to v3.12.3 to unblock
      # builds on kernel 7.0+, which changed the `hid_report_raw_event`
      # signature (5 args -> 6 args). openrazer 3.12.2 (still in our
      # nixpkgs input via the nixos-unstable channel) calls it with 5
      # args, so `razerkbd_driver.c` fails to compile against the latest
      # kernel. v3.12.3 carries the fix.
      #
      # Upstream fix: https://github.com/openrazer/openrazer/pull/2809
      # nixpkgs bump:  https://github.com/NixOS/nixpkgs/pull/523308
      #                (merged as nixpkgs commit 99643def)
      #
      # The userspace packages (pylib + daemon) are pulled wholesale from
      # `masterPkgs`. The kernel module is per-kernel and lives under
      # `linuxPackages_latest` (both `tui` and `navi` use
      # `pkgs.linuxPackages_latest`), so it gets `overrideAttrs`'d to swap
      # in the v3.12.3 source.
      #
      # REMOVAL CONDITION: delete this block once our nixos-unstable
      # channel pointer advances past nixpkgs commit 99643def
      # (`python3Packages.openrazer: 3.12.2 -> 3.12.3`).
      python3Packages = prev.python3Packages // {
        openrazer = masterPkgs.python3Packages.openrazer;
        openrazer-daemon = masterPkgs.python3Packages.openrazer-daemon;
      };
      # Use `.extend` (not attribute merging) on the kernel package set so
      # that NixOS's `boot.kernelPackages.apply = kp: kp.extend (...)` hook
      # preserves our override. A plain `prev.linuxPackages_latest // {...}`
      # loses the override because NixOS re-extends from the underlying
      # fixed-point.
      linuxPackages_latest = prev.linuxPackages_latest.extend (
        _kfinal: kprev: {
          openrazer = kprev.openrazer.overrideAttrs (_old: {
            version = "3.12.3-${kprev.kernel.version}";
            src = prev.fetchFromGitHub {
              owner = "openrazer";
              repo = "openrazer";
              tag = "v3.12.3";
              hash = "sha256-X1NPqbugBdxD5Nt9wIwQADV4CuydGLpgKhlNazVdrIY=";
            };
          });
        }
      );

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
      pi-coding-agent =
        let
          newSrc = prev.fetchFromGitHub {
            owner = "earendil-works";
            repo = "pi";
            tag = "v0.77.0";
            hash = "sha256-PJyhLWfqoPjHoYl4pKJVD3uMD5YjQB5YIk5mBZvGi8E=";
          };
        in
        prev.pi-coding-agent.overrideAttrs (old: {
          version = "0.77.0";
          src = newSrc;
          npmDeps = prev.fetchNpmDeps {
            inherit (old) npmWorkspace;
            src = newSrc;
            hash = "sha256-X0qMLqAi5pgrtTw5+DfSPsgIEngUnHwGxqYE6PL8NJU=";
          };
          # Upstream nixpkgs' postInstall hard-codes the old
          # @mariozechner/* workspace names; in v0.75.0 the monorepo
          # renamed to @earendil-works/*. Re-derive postInstall with the
          # new names.
          postInstall = ''
            local nm="$out/lib/node_modules/pi-monorepo/node_modules"

            for ws in @earendil-works/pi-ai:packages/ai \
                      @earendil-works/pi-agent-core:packages/agent \
                      @earendil-works/pi-tui:packages/tui; do
              IFS=: read -r pkg src <<< "$ws"
              rm "$nm/$pkg"
              cp -r "$src" "$nm/$pkg"
            done

            find "$nm" -type l -lname '*/packages/*' -delete
            find "$nm/.bin" -xtype l -delete
          '';
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
