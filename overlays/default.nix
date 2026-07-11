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

      # choose-gui: force the link step through `lld` on Darwin to work
      # around an `ld64` hardening regression that landed with the
      # nixpkgs staging-next merge on 2026-06-27 (flake bump
      # d407951 -> 0bb7ec5, PR #2380). The new hardening flag breaks
      # Objective-C / Cocoa-framework linking on aarch64-darwin; every
      # xcbuild-driven Cocoa build in the affected class fails at the
      # final `Ld` step. `choose-gui` builds via `xcbuild` and links
      # Cocoa, so it's in-class but did not get a per-package fix
      # upstream.
      #
      # nixpkgs shipped ~15 per-package workarounds with this exact
      # shape (add `llvmPackages.lld` to `nativeBuildInputs`, set
      # `NIX_CFLAGS_LINK = "-fuse-ld=lld"`), each carrying a
      # `# TODO: Clean up on \`staging\`.` comment. We mirror the shape
      # the maintainers used for `python3Packages.pyobjc-framework-Cocoa`
      # in https://github.com/NixOS/nixpkgs/pull/538151.
      #
      # REMOVAL CONDITION: delete this override once
      # https://github.com/NixOS/nixpkgs/pull/536365 ("ld64: disable
      # hardening again") lands on `staging` and a subsequent flake bump
      # picks up the reverted per-package workarounds. At that point the
      # underlying regression is gone and this block is dead weight.
      choose-gui =
        if final.stdenv.isDarwin then
          prev.choose-gui.overrideAttrs (old: {
            nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ [ final.llvmPackages.lld ];
            env = (old.env or { }) // {
              NIX_CFLAGS_LINK = ((old.env.NIX_CFLAGS_LINK or "") + " -fuse-ld=lld");
            };
          })
        else
          prev.choose-gui;

      # kitty: force the link step through `lld` on Darwin to work around
      # the same `ld64` hardening regression that bit `choose-gui`
      # (see the block above). `kitty`'s `glfw-cocoa.so` link step
      # trips the same `Trace/BPT trap: 5` in the cctools ld wrapper:
      #
      #   clang ... -framework Cocoa -framework IOKit ...
      #     -o build/kitty/glfw-cocoa.so
      #   .../cctools-.../bin/ld: line 292: 24087 Trace/BPT trap: 5
      #   clang: error: linker command failed with exit code 133
      #
      # Failing m4mac CI run:
      #   https://github.com/prismatic-koi/nixos-config/actions/runs/29171272201
      #
      # Pattern origin: unlike `choose-gui`, this is a direct mirror of
      # the actual upstream fix — nixpkgs PR
      # https://github.com/NixOS/nixpkgs/pull/539908
      # ("kitty: work around ld64 hardening issue") landed on `master`
      # on 2026-07-09 as commit
      # d2a7c286bdb34760a36d51b5deb37b5e736e5c60. Our flake follows
      # `nixos-unstable`, and the channel has NOT yet advanced past
      # that commit — the bump we picked up in #2380 stopped at
      # `0bb7ec5`, which predates d2a7c286. So we mirror the fix
      # locally until the channel catches up.
      #
      # REMOVAL CONDITION: delete this override once EITHER
      #   (a) our `nixos-unstable` flake-follow advances past nixpkgs
      #       commit d2a7c286bdb34760a36d51b5deb37b5e736e5c60 (the
      #       kitty fix's merge commit) — at which point upstream
      #       carries the same override and ours is a duplicate; OR
      #   (b) https://github.com/NixOS/nixpkgs/pull/536365 ("ld64:
      #       disable hardening again") lands and the whole
      #       workaround train unwinds — at which point the
      #       underlying regression is gone and every per-package
      #       fix in this class (including upstream's own) becomes
      #       dead weight.
      # Whichever comes first.
      kitty =
        if final.stdenv.isDarwin then
          prev.kitty.overrideAttrs (old: {
            nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ [ final.llvmPackages.lld ];
            env = (old.env or { }) // {
              NIX_CFLAGS_LINK = ((old.env.NIX_CFLAGS_LINK or "") + " -fuse-ld=lld");
            };
          })
        else
          prev.kitty;

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
