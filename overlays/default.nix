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

      # direnv: disable the test phase on Darwin to work around a hang in
      # `direnv-test.zsh` introduced when libarchive was bumped 3.8.4 -> 3.8.6
      # on staging-25.11 (nixpkgs commit 32e655f). Direnv's source is
      # unchanged but transitive input changes force rebuilds, and the test
      # harness wedges on aarch64-darwin.
      # See: https://github.com/NixOS/nixpkgs/issues/507531
      # Remove this override once upstream lands a fix.
      direnv =
        if final.stdenv.hostPlatform.isDarwin then
          prev.direnv.overrideAttrs (_: {
            doCheck = false;
          })
        else
          prev.direnv;

      # qutebrowser: widen the built-in AMD+Wayland GBM workaround guard in
      # `misc/backendproblem.py::_fix_wayland_amd_gbm` from exact QtWebEngine
      # 6.11.0 to all 6.11.x. Upstream self-deactivated the workaround on
      # 6.11.1 assuming Qt fixed the underlying AMD GBM bug, but on our
      # amdgpu+Hyprland machine (navi) the bug persists on 6.11.1: page
      # elements render transparent/broken until `QTWEBENGINE_FORCE_USE_GBM=0`
      # is set. Manually replicating that env var restores rendering, so we
      # patch the version guard to `strip_patch() != VersionNumber(6, 11)` so
      # every 6.11.x re-enters the known-good 6.11.0 code path.
      #
      # Upstream references:
      #   https://github.com/qutebrowser/qutebrowser/issues/8841
      #   https://github.com/NixOS/nixpkgs/issues/531703
      #
      # REMOVAL CONDITION: delete this override (and its patch file) once
      # either
      #   (a) upstream qutebrowser widens the guard itself (e.g. matching
      #       all 6.11.x, or dropping the version check entirely), or
      #   (b) Qt lands a real fix for the AMD+Wayland GBM path and the
      #       workaround is no longer needed on our hardware — in which
      #       case the whole `_fix_wayland_amd_gbm` codepath becomes
      #       obsolete upstream and this patch with it.
      qutebrowser = addPatches prev.qutebrowser [
        ./qutebrowser-widen-amd-gbm-workaround.patch
      ];

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

      # linuxPackages_latest: build external kernel modules from the
      # nixpkgs-master derivations, against the kernel of the host package set
      # (not the master kernel). Issue #3221.
      #
      # v4l2loopback: the nixos-unstable derivation runs `modules_install`,
      # which writes kernel module index files such as `modules.builtin.bin`
      # into the package output and conflicts with the kernel modules
      # buildEnv. Master sets `DEPMOD=true` on install.
      # REMOVAL CONDITION (v4l2loopback): delete that attribute once the
      # `nixos-unstable` revision in flake.lock contains NixOS/nixpkgs#577221
      # (merge commit 827efc1c02d64911ec192041d8bad107722b472e).
      #
      # openrazer: the kernel modules aggregation (NixOS/nixpkgs#575192)
      # merges a `.ko.cer` file for each external module, and the
      # nixos-unstable openrazer driver does not produce them. Master has the
      # fix. The version is 3.12.4 on both revisions, so the driver stays in
      # step with the daemon.
      # REMOVAL CONDITION (openrazer): delete that attribute once the
      # `nixos-unstable` revision in flake.lock contains NixOS/nixpkgs#576293
      # (merge commit 965f876a780c1fad7f14b9f817e093e40ea5d5e5).
      linuxPackages_latest = prev.linuxPackages_latest.extend (
        kfinal: _kprev: {
          v4l2loopback =
            kfinal.callPackage "${inputs.nixpkgs-master}/pkgs/os-specific/linux/v4l2loopback"
              { };
          openrazer =
            kfinal.callPackage "${inputs.nixpkgs-master}/pkgs/os-specific/linux/openrazer/driver.nix"
              { };
        }
      );

      # pyopenjtalk-plus: Cython 3.3.0 changed the text of a TypeError, and
      # tests/test_frontend.py matches the old text, so the test phase fails
      # (issue #3223). This adds the upstream test-only patch from
      # NixOS/nixpkgs#576063. The library code does not change and the
      # tests still run. `pythonPackagesExtensions` covers every Python
      # package set.
      # See: https://github.com/NixOS/nixpkgs/issues/576442
      #
      # REMOVAL CONDITION: delete this override once the `nixos-unstable`
      # revision in flake.lock contains NixOS/nixpkgs#576063 (merge commit
      # 0b88cee4de185857a4637f75a176006b28e193b1). The patch then applies
      # twice and the build fails. That failure is the signal to remove
      # this override.
      pythonPackagesExtensions = prev.pythonPackagesExtensions ++ [
        (_pyFinal: pyPrev: {
          pyopenjtalk-plus = addPatches pyPrev.pyopenjtalk-plus [
            (final.fetchpatch {
              url = "https://github.com/tsukumijima/pyopenjtalk-plus/commit/90b644f9e9d397882e6eafe4dcd1843a4fde6a63.patch";
              includes = [ "tests/test_frontend.py" ];
              hash = "sha256-Jb7T/bRUHxqwBLwV6xYC/teaFHm5GmqJxA7sCX+oiNI=";
            })
          ];
        })
      ];

      _macronTypePkg =
        if final.stdenv.hostPlatform.isDarwin then final.callPackage ../pkgs/macron-type.nix { } else null;
      macron-type = if final.stdenv.hostPlatform.isDarwin then final._macronTypePkg.server else null;
      macron-send = if final.stdenv.hostPlatform.isDarwin then final._macronTypePkg.client else null;

    };
}
